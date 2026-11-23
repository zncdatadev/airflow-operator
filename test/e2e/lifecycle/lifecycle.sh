#!/usr/bin/env bash
set -euo pipefail

: "${NAMESPACE:?Chainsaw must supply an isolated namespace}"

state=$(mktemp -d)
trap 'rm -rf "$state"' EXIT

k() {
    kubectl -n "$NAMESPACE" "$@"
}

wait_for() {
    local description=$1
    shift
    local deadline=$((SECONDS + 300))
    until "$@"; do
        if (( SECONDS >= deadline )); then
            echo "Timed out: $description" >&2
            return 1
        fi
        sleep 3
    done
}

replicas_are() {
    local name=$1 expected=$2
    k get sts "$name" -o json | python3 -c '
import json, sys
sts = json.load(sys.stdin)
expected = int(sys.argv[1])
status = sts.get("status", {})
assert sts["spec"]["replicas"] == expected
assert status.get("readyReplicas", 0) == expected
assert status.get("replicas", 0) == expected
assert status.get("observedGeneration", 0) >= sts["metadata"]["generation"]
' "$expected" 2>/dev/null
}

resource_exists() {
    k get "$1" >/dev/null 2>&1
}

role_pdb_covers() {
    local expected_pods=$1
    # The role PDB intentionally uses the two labels shared by Gen 2 and Gen 3 pods, so it
    # continues protecting the workload throughout an in-place framework upgrade.
    k get pdb -l app.kubernetes.io/instance=lifecycle,app.kubernetes.io/component=webservers -o json | python3 -c '
import json, sys
items = json.load(sys.stdin)["items"]
assert [item["metadata"]["name"] for item in items] == ["lifecycle-webservers"]
pdb = items[0]
expected = int(sys.argv[1])
selector = pdb["spec"]["selector"]["matchLabels"]
assert pdb["spec"]["maxUnavailable"] == 1
assert selector == {
    "app.kubernetes.io/instance": "lifecycle",
    "app.kubernetes.io/component": "webservers",
}
status = pdb.get("status", {})
assert status.get("expectedPods", 0) == expected
assert status.get("currentHealthy", 0) == expected
assert status.get("disruptionsAllowed", 0) == 1
' "$expected_pods" 2>/dev/null
}

snapshot_identity() {
    k get sts,cm,svc,pdb -l airflow.kubedoop.dev/cluster=lifecycle -o json |
        python3 -c '
import json, sys
items = json.load(sys.stdin)["items"]
print(json.dumps(sorted((item["kind"], item["metadata"]["name"], item["metadata"]["uid"]) for item in items)))
'
}

for name in \
    lifecycle-webservers-primary \
    lifecycle-webservers-secondary \
    lifecycle-schedulers-default \
    lifecycle-celeryexecutors-default; do
    wait_for "$name ready" replicas_are "$name" 1
done

wait_for 'one webserver PDB covers both groups' role_pdb_covers 2
if k get pdb lifecycle-webservers-primary >/dev/null 2>&1 ||
    k get pdb lifecycle-webservers-secondary >/dev/null 2>&1; then
    echo 'role-group PDB found; expected exactly one role-level PDB' >&2
    exit 1
fi

echo 'Stop retains resources while scaling every role group to zero'
snapshot_identity > "$state/before-stop.json"
k patch airflowcluster lifecycle --type=merge \
    -p '{"spec":{"clusterOperation":{"stopped":true}}}'
for name in \
    lifecycle-webservers-primary \
    lifecycle-webservers-secondary \
    lifecycle-schedulers-default \
    lifecycle-celeryexecutors-default; do
    wait_for "$name stopped" replicas_are "$name" 0
done
snapshot_identity > "$state/after-stop.json"
cmp "$state/before-stop.json" "$state/after-stop.json"

echo 'Resume restores all requested replicas without replacing resources'
k patch airflowcluster lifecycle --type=merge \
    -p '{"spec":{"clusterOperation":{"stopped":false}}}'
for name in \
    lifecycle-webservers-primary \
    lifecycle-webservers-secondary \
    lifecycle-schedulers-default \
    lifecycle-celeryexecutors-default; do
    wait_for "$name resumed" replicas_are "$name" 1
done
snapshot_identity > "$state/after-resume.json"
cmp "$state/before-stop.json" "$state/after-resume.json"

echo 'Reconciliation pause freezes a replica change for the whole observation window'
k patch airflowcluster lifecycle --type=merge \
    -p '{"spec":{"clusterOperation":{"reconciliationPaused":true}}}'
# Let an already-running reconciliation finish before changing the desired state under test.
sleep 10
k patch airflowcluster lifecycle --type=merge \
    -p '{"spec":{"webservers":{"roleGroups":{"secondary":{"replicas":0}}}}}'
for _ in {1..10}; do
    replicas_are lifecycle-webservers-secondary 1
    sleep 2
done

echo 'Unpausing applies the pending replica change'
k patch airflowcluster lifecycle --type=merge \
    -p '{"spec":{"clusterOperation":{"reconciliationPaused":false}}}'
wait_for 'pending replica change applied' replicas_are lifecycle-webservers-secondary 0
k patch airflowcluster lifecycle --type=merge \
    -p '{"spec":{"webservers":{"roleGroups":{"secondary":{"replicas":1}}}}}'
wait_for 'secondary restored' replicas_are lifecycle-webservers-secondary 1

echo 'Adding a role group creates its full resource set and expands the role PDB'
k patch airflowcluster lifecycle --type=json -p '[
  {"op":"add","path":"/spec/webservers/roleGroups/transient","value":{"replicas":1}}
]'
wait_for 'transient StatefulSet ready' replicas_are lifecycle-webservers-transient 1
for resource in \
    cm/lifecycle-webservers-transient \
    svc/lifecycle-webservers-transient \
    svc/lifecycle-webservers-transient-headless; do
    wait_for "$resource created" resource_exists "$resource"
done
wait_for 'role PDB expanded to transient group' role_pdb_covers 3

echo 'Removing a role group reclaims generated resources but leaves the role PDB and siblings'
k patch airflowcluster lifecycle --type=json \
    -p '[{"op":"remove","path":"/spec/webservers/roleGroups/transient"}]'
for resource in \
    sts/lifecycle-webservers-transient \
    cm/lifecycle-webservers-transient \
    svc/lifecycle-webservers-transient \
    svc/lifecycle-webservers-transient-headless; do
    k wait --for=delete "$resource" --timeout=300s
done
wait_for 'role PDB contracts after group cleanup' role_pdb_covers 2
replicas_are lifecycle-webservers-primary 1
replicas_are lifecycle-webservers-secondary 1

echo 'Gen3 lifecycle acceptance passed'
