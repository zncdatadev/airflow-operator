#!/usr/bin/env bash
# Destructive acceptance test confined to an empty, explicitly selected kind cluster.
set -euo pipefail

: "${KUBECONFIG:?Set KUBECONFIG to the disposable kind cluster}"
: "${CHAINSAW_CLUSTER:?Set the disposable kind cluster name}"
: "${UPGRADE_BASELINE_DIR:?Provide a checkout of the pre-framework operator}"
: "${UPGRADE_BASELINE_IMAGE:?Build the pre-framework image first}"
: "${IMG:?Build the framework image first}"
: "${UPGRADE_FRESH_CLUSTER:?Run this script through make framework-upgrade-e2e}"
[[ "$UPGRADE_FRESH_CLUSTER" == "1" ]] || { echo 'UPGRADE_FRESH_CLUSTER must be 1' >&2; exit 1; }

root=$(cd "$(dirname "$0")/.." && pwd)
product_version=${PRODUCT_VERSION:-2.10.5}
kind_bin=${KIND:-kind}
container_tool=${CONTAINER_TOOL:-docker}
framework_ref=$(git -C "$root" rev-parse HEAD)
[[ -z "$(git -C "$root" status --porcelain --untracked-files=all)" ]] || {
    echo 'The framework checkout must remain clean so the evidence identifies exact source.' >&2
    exit 1
}
python3 - <<'PY'
if not __debug__:
    raise RuntimeError("PYTHONOPTIMIZE must be disabled for upgrade acceptance")
PY
evidence_parent=${UPGRADE_EVIDENCE_DIR:-$root/upgrade-evidence}
mkdir -p "$evidence_parent"
evidence_parent=$(cd "$evidence_parent" && pwd)
evidence=$(mktemp -d "$evidence_parent/run.XXXXXX")
echo "Migration evidence: $evidence"

test "$(kubectl config current-context)" = "kind-$CHAINSAW_CLUSTER"
{
	printf 'baseline_ref=%s\n' "$(git -C "$UPGRADE_BASELINE_DIR" rev-parse HEAD)"
	printf 'framework_ref=%s\n' "$framework_ref"
	printf 'product_version=%s\n' "$product_version"
	printf 'kind_cluster=%s\n' "$CHAINSAW_CLUSTER"
	kubectl version -o yaml
	"$kind_bin" version
} > "$evidence/run-metadata.txt"
crd=$(kubectl get crd airflowclusters.airflow.kubedoop.dev --ignore-not-found -o name)
clusters=
if [[ -n "$crd" ]]; then
    clusters=$(kubectl get airflowclusters.airflow.kubedoop.dev -A -o name)
fi
if [[ -n "$clusters" ]]; then
    echo 'Use an empty kind cluster: existing AirflowClusters would be affected.' >&2
    exit 1
fi

namespace=airflow-framework-upgrade
if kubectl get namespace "$namespace" >/dev/null 2>&1; then
    echo "Namespace $namespace already exists; inspect the previous run before retrying." >&2
    exit 1
fi

scratch=$(mktemp -d)
guard_pid=
guard_stop="$scratch/stop-sts-guard"
guard_failure="$scratch/sts-guard-failed"
airflow_statefulsets=(
    upgrade-webservers-default
    upgrade-schedulers-default
    upgrade-celeryexecutors-default
)

k() { kubectl -n "$namespace" "$@"; }

finish() {
    local result=$?
    touch "$guard_stop"
    if [[ -n "$guard_pid" ]]; then
        wait "$guard_pid" 2>/dev/null || true
    fi
    if (( result != 0 )); then
        k get airflowcluster,sts,cm,svc,sa,pdb,deploy,pod,pvc -o yaml \
            > "$evidence/failure-resources.yaml" 2>&1 || true
        k get events --sort-by=.lastTimestamp > "$evidence/failure-events.txt" 2>&1 || true
        kubectl -n airflow-operator-system logs deployment/airflow-operator-controller-manager \
            --tail=300 > "$evidence/failure-operator.log" 2>&1 || true
    fi
    rm -rf "$scratch"
}
trap finish EXIT

wait_for() {
    local description=$1
    shift
    local deadline=$((SECONDS + 600))
    until "$@"; do
        if (( SECONDS >= deadline )); then
            echo "Timed out: $description" >&2
            k get sts,pod,pvc,svc,deploy -o wide || true
            k get events --sort-by=.lastTimestamp || true
            return 1
        fi
        sleep 5
    done
}

ready_statefulset() {
    k get sts "$1" -o json | python3 -c '
import json, sys
s = json.load(sys.stdin)
desired = s["spec"].get("replicas", 1)
status = s.get("status", {})
assert status.get("readyReplicas", 0) == desired
assert status.get("currentReplicas", 0) == desired
assert status.get("updatedReplicas", 0) == desired
assert status.get("observedGeneration", 0) >= s["metadata"]["generation"]
assert status.get("currentRevision") == status.get("updateRevision")
' 2>/dev/null
}

ready_airflow() {
    local name
    for name in "${airflow_statefulsets[@]}"; do
        ready_statefulset "$name" || return 1
    done
}

render_operator() {
    local source=$1 image=$2 output=$3
    local render
    render=$(mktemp -d "$scratch/render.XXXXXX")
    cp -R "$source/config" "$render/config"
    (cd "$render/config/manager" && "$root/bin/kustomize" edit set image "controller=$image")
    "$root/bin/kustomize" build "$render/config/default" > "$output"
}

deploy_operator() {
	local manifest=$1 phase=$2
	kubectl apply -f "$manifest"
	kubectl -n airflow-operator-system rollout status \
		deployment/airflow-operator-controller-manager --timeout=300s
	kubectl -n airflow-operator-system get deployment,pod -o json \
		> "$evidence/$phase-controller.json"
}

quiesce_operator() {
    kubectl -n airflow-operator-system scale \
        deployment/airflow-operator-controller-manager --replicas=0
    wait_for 'outgoing controller Pod deletion' operator_quiesced
}

operator_quiesced() {
    kubectl -n airflow-operator-system get pod \
        -l control-plane=controller-manager -o json | python3 -c '
import json, sys
assert not json.load(sys.stdin)["items"]
' 2>/dev/null
}

generation_from_snapshot() {
    local phase=$1 name=$2
    python3 - "$evidence/$phase-resources.json" "$name" <<'PY'
import json, sys
items = json.load(open(sys.argv[1]))["items"]
print(next(x["metadata"]["generation"] for x in items if x["kind"] == "StatefulSet" and x["metadata"]["name"] == sys.argv[2]))
PY
}

generation_greater_than() {
    local name=$1 old_generation=$2 current
    current=$(k get sts "$name" -o jsonpath='{.metadata.generation}')
    (( current > old_generation ))
}

phase_applied() {
    local phase=$1 name
    for name in "${airflow_statefulsets[@]}"; do
        k get sts "$name" -o json | python3 -c '
import json, sys
phase = sys.argv[1]
sts = json.load(sys.stdin)
template = sts["spec"]["template"]
assert template.get("metadata", {}).get("annotations", {}).get(
    "airflow.kubedoop.dev/upgrade-phase"
) == phase
status = sts.get("status", {})
assert status.get("currentRevision") == status.get("updateRevision")
' "$phase" 2>/dev/null || return 1
        k get pod "$name-0" -o json | python3 -c '
import json, sys
phase = sys.argv[1]
pod = json.load(sys.stdin)
assert pod.get("metadata", {}).get("annotations", {}).get(
    "airflow.kubedoop.dev/upgrade-phase"
) == phase
' "$phase" 2>/dev/null || return 1
    done
}

wait_for_reconciliation() {
	local previous_phase=$1 current_phase=$2 name old_generation
    for name in "${airflow_statefulsets[@]}"; do
        old_generation=$(generation_from_snapshot "$previous_phase" "$name")
        wait_for "$current_phase reconciled $name" generation_greater_than "$name" "$old_generation"
	done
	wait_for "$current_phase Airflow roles ready" ready_airflow
	wait_for "$current_phase pod template and pods observed" phase_applied "$current_phase"
}

patch_rollout_phase() {
    local phase=$1 username=$2 email=$3 patch_file
    patch_file="$scratch/$phase-rollout-patch.json"
    k patch secret credentials --type merge -p "{\"stringData\":{\"adminUser.username\":\"$username\",\"adminUser.email\":\"$email\"}}"
    python3 - "$phase" "$patch_file" <<'PY'
import json, pathlib, sys

phase = sys.argv[1]
annotation = {"airflow.kubedoop.dev/upgrade-phase": phase}
patch = {
    "spec": {
        role: {
            "roleGroups": {
                "default": {
                    "podOverrides": {"metadata": {"annotations": annotation}}
                }
            }
        }
        for role in ("webservers", "schedulers", "celeryExecutors")
    }
}
pathlib.Path(sys.argv[2]).write_text(json.dumps(patch) + "\n")
PY
    k patch airflowcluster upgrade --type=merge --patch-file "$patch_file"
}

web_healthy() {
    k exec upgrade-webservers-default-0 -c webservers -- python -c '
import json, urllib.request
data = json.load(urllib.request.urlopen("http://127.0.0.1:8080/health", timeout=10))
assert data["metadatabase"]["status"] == "healthy"
print(json.dumps(data, sort_keys=True))
' > "$scratch/web-health" 2>&1
}

database_healthy() {
    k exec upgrade-schedulers-default-0 -c schedulers -- airflow db check \
        > "$scratch/database-health" 2>&1
}

celery_healthy() {
    timeout 45s kubectl -n "$namespace" exec upgrade-schedulers-default-0 -c schedulers -- \
        celery --app airflow.providers.celery.executors.celery_executor.app \
        inspect ping --timeout 10 \
        > "$scratch/celery-health" 2>&1 &&
        python3 - "$scratch/celery-health" <<'PY'
import pathlib, sys

output = pathlib.Path(sys.argv[1]).read_text()
assert "celery@upgrade-celeryexecutors-default-0: OK" in output
assert "pong" in output
assert "1 node online." in output
PY
}

admin_user_matches() {
    local username=$1 email=$2
    k exec upgrade-schedulers-default-0 -c schedulers -- \
        airflow users list -o json | python3 -c '
import json, sys
username, email = sys.argv[1:]
users = json.load(sys.stdin)
matches = [
    user for user in users
    if user.get("username") == username and user.get("email") == email
]
assert len(matches) == 1
print(json.dumps(matches[0], indent=2, sort_keys=True))
' "$username" "$email"
}

metrics_services_healthy() {
    local service
    for service in "${airflow_statefulsets[@]}"; do
        k get endpointslice -l "kubernetes.io/service-name=$service" -o json | python3 -c '
import json, sys
name = sys.argv[1]
items = json.load(sys.stdin)["items"]
addresses = [
    address
    for item in items
    for endpoint in item.get("endpoints", [])
    for address in endpoint.get("addresses", [])
]
ports = [port for item in items for port in (item.get("ports") or [])]
ports_by_name = {port.get("name"): port.get("port") for port in ports}
assert len(addresses) == 1
assert ports_by_name.get("metrics") == 9102
if name == "upgrade-webservers-default":
    assert ports_by_name.get("http") == 8080
else:
    assert "http" not in ports_by_name
' "$service" 2>/dev/null || return 1
        k exec upgrade-webservers-default-0 -c webservers -- python -c '
import sys, urllib.request
body = urllib.request.urlopen("http://" + sys.argv[1] + ":9102/metrics", timeout=10).read()
assert body
' "$service" >/dev/null 2>&1 || return 1
        if k get service "$service-headless" >/dev/null 2>&1; then
            k get endpointslice -l "kubernetes.io/service-name=$service-headless" -o json | python3 -c '
import json, sys
items = json.load(sys.stdin)["items"]
addresses = [
    address
    for item in items
    for endpoint in item.get("endpoints", [])
    for address in endpoint.get("addresses", [])
]
assert len(addresses) == 1
' 2>/dev/null || return 1
        fi
    done

    # Exercise the user-facing Web Service from another role, rather than accepting only the
    # webserver Pod's loopback health endpoint.
    k exec upgrade-schedulers-default-0 -c schedulers -- python -c '
import json, urllib.request
data = json.load(urllib.request.urlopen(
    "http://upgrade-webservers-default:8080/health", timeout=10
))
assert data["metadatabase"]["status"] == "healthy"
' >/dev/null 2>&1
}

legacy_metrics_topology_observed() {
    local service service_file endpoints_file
    for service in "${airflow_statefulsets[@]}"; do
        service_file="$scratch/$service-service.json"
        endpoints_file="$scratch/$service-endpointslices.json"
        k get service "$service" -o json > "$service_file" || return 1
        k get endpointslice -l "kubernetes.io/service-name=$service" -o json \
            > "$endpoints_file" || return 1
        python3 - "$service" "$service_file" "$endpoints_file" <<'PY' 2>/dev/null || return 1
import json, sys

name = sys.argv[1]
service = json.load(open(sys.argv[2]))
slices = json.load(open(sys.argv[3]))["items"]
annotations = service["metadata"].get("annotations", {}).copy()
annotations.pop("banzaicloud.com/last-applied", None)
annotations.pop("kubectl.kubernetes.io/last-applied-configuration", None)
projection = {
    "labels": service["metadata"].get("labels", {}),
    "annotations": annotations,
    "selector": service["spec"].get("selector", {}),
    "ports": service["spec"].get("ports", []),
}
addresses = {
    address
    for item in slices
    for endpoint in item.get("endpoints", [])
    for address in endpoint.get("addresses", [])
}
ports = [port for item in slices for port in (item.get("ports") or [])]

metrics = [port for port in projection["ports"] if port["name"] == "metrics"]
assert len(metrics) == 1
assert metrics[0]["targetPort"] == "metrics"
if name == "upgrade-webservers-default":
    client_labels = {
        "app.kubernetes.io/instance": "upgrade",
        "app.kubernetes.io/managed-by": "kubedoop.dev",
    }
    role_group_labels = {
        "app.kubernetes.io/component": "webservers",
        "app.kubernetes.io/instance": "upgrade",
        "app.kubernetes.io/managed-by": "airflow.kubedoop.dev",
        "app.kubernetes.io/name": "airflowcluster",
        "app.kubernetes.io/role-group": "default",
    }
    metrics_port = {
        "name": "metrics",
        "port": 9102,
        "protocol": "TCP",
        "targetPort": "metrics",
    }
    client = {
        "labels": client_labels,
        "annotations": {},
        "selector": client_labels,
        "ports": [
            {
                "name": "http",
                "port": 8080,
                "protocol": "TCP",
                "targetPort": "http",
            },
            metrics_port,
        ],
    }
    role_metrics = {
        "labels": {**role_group_labels, "prometheus.io/scrape": "true"},
        "annotations": {
            "prometheus.io/path": "/metrics",
            "prometheus.io/port": "9102",
            "prometheus.io/scheme": "http",
            "prometheus.io/scrape": "true",
        },
        "selector": role_group_labels,
        "ports": [metrics_port],
    }
    assert projection in (client, role_metrics)
    # EndpointSlice reconciliation is asynchronous with the Service's one-second A/B
    # mutation loop, so the independent snapshot can legitimately show either selector.
    assert len(addresses) in (0, 1)
else:
    assert len(addresses) == 1
assert not ports
PY
        k get pod "$service-0" -o json | python3 -c '
import json, sys
pod = json.load(sys.stdin)
port_names = {
    port.get("name")
    for container in pod["spec"]["containers"]
    for port in container.get("ports", [])
}
assert "metrics" not in port_names
' 2>/dev/null || return 1
    done
}

vector_sidecars_healthy() {
    local workload role
    for workload in "${airflow_statefulsets[@]}"; do
        role=${workload#upgrade-}
        role=${role%-default}
        k get pod "$workload-0" -o json | python3 -c '
import json, sys
pod = json.load(sys.stdin)
matches = [
    status
    for status in pod.get("status", {}).get("initContainerStatuses", [])
    if status.get("name") == "vector"
]
assert len(matches) == 1
status = matches[0]
assert status.get("ready") is True
assert status.get("state", {}).get("running")
assert status.get("restartCount") == 0
' 2>/dev/null || return 1
        k exec "$workload-0" -c "$role" -- python -c '
import urllib.request
body = urllib.request.urlopen("http://127.0.0.1:9598/metrics", timeout=10).read()
assert body
' >/dev/null 2>&1 || return 1
    done
}

snapshot_vector_metrics() {
    local workload role
    for workload in "${airflow_statefulsets[@]}"; do
        role=${workload#upgrade-}
        role=${role%-default}
        k exec "$workload-0" -c "$role" -- python -c '
import hashlib, json, sys, urllib.request
body = urllib.request.urlopen("http://127.0.0.1:9598/metrics", timeout=10).read()
print(json.dumps({
    "bytes": len(body),
    "sha256": hashlib.sha256(body).hexdigest(),
    "workload": sys.argv[1],
}, sort_keys=True))
' "$workload"
    done
}

write_business_markers() {
    k exec upgrade-schedulers-default-0 -c schedulers -- \
        airflow variables set framework_upgrade_marker airflow-framework-upgrade
    k exec upgrade-schedulers-default-0 -c schedulers -- python -c '
import os, redis
client = redis.Redis.from_url(os.environ["AIRFLOW__CELERY__BROKER_URL"])
client.set("framework_upgrade_marker", "airflow-framework-upgrade")
assert client.get("framework_upgrade_marker") == b"airflow-framework-upgrade"
'
}

snapshot_business() {
    local phase=$1 airflow_value redis_value username email
    case "$phase" in
        before)
            username=admin
            email=admin@example.com
            ;;
        after)
            username=admin-upgrade
            email=admin-upgrade@example.com
            ;;
        rollback)
            username=admin-rollback
            email=admin-rollback@example.com
            ;;
        *)
            echo "Unknown snapshot phase: $phase" >&2
            return 1
            ;;
    esac
    wait_for "$phase webserver health" web_healthy
    wait_for "$phase database health" database_healthy
    wait_for "$phase Celery worker through Redis" celery_healthy
    if [[ "$phase" == "after" ]]; then
        wait_for "$phase repaired metrics Services and endpoints" metrics_services_healthy
        wait_for "$phase Vector sidecars running and serving metrics" vector_sidecars_healthy
        snapshot_vector_metrics > "$evidence/$phase-vector-metrics.jsonl"
    else
        wait_for "$phase legacy unresolved metrics topology" legacy_metrics_topology_observed
    fi
    wait_for "$phase expected Airflow administrator" \
        admin_user_matches "$username" "$email" >/dev/null 2>&1
    admin_user_matches "$username" "$email" > "$evidence/$phase-admin-user.json"
    airflow_value=$(k exec upgrade-schedulers-default-0 -c schedulers -- \
        airflow variables get framework_upgrade_marker | tail -n 1)
    redis_value=$(k exec upgrade-schedulers-default-0 -c schedulers -- python -c '
import os, redis
value = redis.Redis.from_url(os.environ["AIRFLOW__CELERY__BROKER_URL"]).get("framework_upgrade_marker")
assert value is not None
print(value.decode())
' | tail -n 1)
    python3 - "$airflow_value" "$redis_value" "$evidence/$phase-business.json" <<'PY'
import json, pathlib, sys
pathlib.Path(sys.argv[3]).write_text(json.dumps({
    "airflow_variable": sys.argv[1],
    "redis_marker": sys.argv[2],
}, indent=2, sort_keys=True) + "\n")
PY
    cp "$scratch/web-health" "$evidence/$phase-web-health.json"
    cp "$scratch/database-health" "$evidence/$phase-database-health.txt"
    cp "$scratch/celery-health" "$evidence/$phase-celery-status.txt"
}

snapshot() {
    local phase=$1
    local resources_file="$evidence/$phase-resources.json"
    snapshot_business "$phase"
    k get airflowcluster,sts,cm,svc,sa,pdb,deploy,pod,pvc -o json > "$resources_file"
    k get airflowcluster upgrade -o yaml > "$evidence/$phase-cluster.yaml"
    python3 - "$resources_file" "$evidence/$phase-statefulset-identities.json" <<'PY'
import json, pathlib, sys
items = json.load(open(sys.argv[1]))["items"]
pods = {
    x["metadata"]["name"]: x["metadata"]["uid"]
    for x in items if x["kind"] == "Pod"
}
result = {}
for item in items:
    if item["kind"] != "StatefulSet" or not item["metadata"]["name"].startswith("upgrade-"):
        continue
    spec = item["spec"]
    result[item["metadata"]["name"]] = {
        "uid": item["metadata"]["uid"],
        "generation": item["metadata"]["generation"],
        "selector": spec["selector"],
        "serviceName": spec["serviceName"],
        "podManagementPolicy": spec.get("podManagementPolicy", "OrderedReady"),
        "serviceAccountName": spec["template"]["spec"].get("serviceAccountName", ""),
        "containers": [c["name"] for c in spec["template"]["spec"].get("containers", [])],
        "initContainers": [c["name"] for c in spec["template"]["spec"].get("initContainers", [])],
        "phase": spec["template"].get("metadata", {}).get("annotations", {}).get(
            "airflow.kubedoop.dev/upgrade-phase"
        ),
        "replicas": spec.get("replicas", 1),
        "status": item.get("status", {}),
        "podUID": pods.get(item["metadata"]["name"] + "-0"),
    }
pathlib.Path(sys.argv[2]).write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
PY
    k get pvc data-airflow-postgres-0 -o json > "$evidence/$phase-postgres-pvc.json"
    k get events --sort-by=.lastTimestamp > "$evidence/$phase-events.txt"
    kubectl -n airflow-operator-system logs deployment/airflow-operator-controller-manager \
        --tail=500 > "$evidence/$phase-operator.log"
}

legacy_workload_restored() {
    local name
    for name in "${airflow_statefulsets[@]}"; do
        k get sts "$name" -o json | python3 -c '
import json
import sys

sts = json.load(sys.stdin)
pod_spec = sts["spec"]["template"]["spec"]
assert pod_spec.get("serviceAccountName", "") in ("", "default")
containers = [c["name"] for c in pod_spec.get("containers", [])]
init_containers = [c["name"] for c in pod_spec.get("initContainers", [])]
assert "metric" in containers
assert "vector" not in containers
assert not {"metric", "vector"}.intersection(init_containers)
' 2>/dev/null || return 1
        k get pod "$name-0" -o json | python3 -c '
import json
import sys

pod = json.load(sys.stdin)
assert pod["spec"].get("serviceAccountName", "default") == "default"
' 2>/dev/null || return 1
    done
}

start_statefulset_guard() {
    local baseline="$evidence/before-resources.json"
    : > "$evidence/sts-continuity.jsonl"
    (
        while [[ ! -e "$guard_stop" ]]; do
            if ! k get sts "${airflow_statefulsets[@]}" -o json | \
                python3 -c '
import datetime, json, pathlib, sys
expected_items = json.load(open(sys.argv[1]))["items"]
expected = {
    x["metadata"]["name"]: {
        "uid": x["metadata"]["uid"],
        "selector": x["spec"]["selector"],
        "serviceName": x["spec"]["serviceName"],
        "podManagementPolicy": x["spec"].get("podManagementPolicy", "OrderedReady"),
    }
    for x in expected_items
    if x["kind"] == "StatefulSet" and x["metadata"]["name"].startswith("upgrade-")
}
current = json.load(sys.stdin)["items"]
sample = {"timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(), "statefulSets": {}}
assert len(current) == len(expected) == 3
for item in current:
    name = item["metadata"]["name"]
    identity = expected[name]
    assert item["metadata"]["uid"] == identity["uid"], (name, "UID changed")
    assert item["spec"]["selector"] == identity["selector"], (name, "selector changed")
    assert item["spec"]["serviceName"] == identity["serviceName"], (name, "serviceName changed")
    assert item["spec"].get("podManagementPolicy", "OrderedReady") == identity["podManagementPolicy"], (name, "podManagementPolicy changed")
    assert item["spec"].get("replicas", 1) == 1, (name, "replicas changed")
    sample["statefulSets"][name] = {
        "uid": item["metadata"]["uid"],
        "generation": item["metadata"]["generation"],
        "readyReplicas": item.get("status", {}).get("readyReplicas", 0),
    }
with pathlib.Path(sys.argv[2]).open("a") as stream:
    stream.write(json.dumps(sample, sort_keys=True) + "\n")
' "$baseline" "$evidence/sts-continuity.jsonl" \
                2>> "$evidence/sts-continuity-errors.txt"; then
                touch "$guard_failure"
                exit 1
            fi
            sleep 2
        done
    ) &
    guard_pid=$!
}

stop_statefulset_guard() {
    touch "$guard_stop"
    local guard_result=0
    wait "$guard_pid" || guard_result=$?
    guard_pid=
    if (( guard_result != 0 )) || [[ -e "$guard_failure" ]]; then
        echo 'Airflow StatefulSet continuity guard detected deletion, replacement, or scaling.' >&2
        return 1
    fi
    if (( $(wc -l < "$evidence/sts-continuity.jsonl") < 3 )); then
        echo 'StatefulSet continuity guard recorded too few samples.' >&2
        return 1
    fi
}

render_operator "$UPGRADE_BASELINE_DIR" "$UPGRADE_BASELINE_IMAGE" "$evidence/baseline-operator.yaml"
render_operator "$root" "$IMG" "$evidence/framework-operator.yaml"
{
    printf 'baseline_operator_image=%s\n' "$UPGRADE_BASELINE_IMAGE"
    printf 'baseline_operator_image_id=%s\n' "$("$container_tool" image inspect --format '{{.Id}}' "$UPGRADE_BASELINE_IMAGE")"
    printf 'framework_operator_image=%s\n' "$IMG"
    printf 'framework_operator_image_id=%s\n' "$("$container_tool" image inspect --format '{{.Id}}' "$IMG")"
    sha256sum \
        "$root/hack/test-framework-upgrade.sh" \
        "$root/hack/compare-framework-upgrade.py" \
        "$evidence/baseline-operator.yaml" \
        "$evidence/framework-operator.yaml"
} >> "$evidence/run-metadata.txt"
"$kind_bin" load docker-image --name "$CHAINSAW_CLUSTER" "$UPGRADE_BASELINE_IMAGE" "$IMG"
deploy_operator "$evidence/baseline-operator.yaml" before

kubectl create namespace "$namespace"
cp "$root/test/e2e/default/postgres.yaml" "$evidence/postgres.yaml"
cp "$root/test/e2e/default/redis.yaml" "$evidence/redis.yaml"
cp "$root/test/e2e/default/credentials.yaml" "$evidence/credentials.yaml"
k apply -f "$evidence/postgres.yaml"
k apply -f "$evidence/redis.yaml"
k apply -f "$evidence/credentials.yaml"
k create configmap vector-aggregator-discovery --from-literal=ADDRESS=vector-aggregator:6000
wait_for 'PostgreSQL ready' ready_statefulset airflow-postgres
kubectl -n "$namespace" rollout status deployment/redis --timeout=300s

cat > "$evidence/input-cluster.yaml" <<YAML
apiVersion: airflow.kubedoop.dev/v1alpha1
kind: AirflowCluster
metadata:
  name: upgrade
spec:
  image:
    productVersion: "$product_version"
  clusterConfig:
    credentialsSecret: credentials
    vectorAggregatorConfigMapName: vector-aggregator-discovery
  webservers:
    roleGroups:
      default:
        replicas: 1
        podOverrides: &legacy_sidecar_overrides
          metadata:
            annotations:
              airflow.kubedoop.dev/upgrade-phase: before
          spec:
            volumes:
              - name: log
                emptyDir:
                  sizeLimit: 384Mi
            containers:
              - name: metric
                env:
                  - name: UPGRADE_OVERRIDE_MARKER
                    value: metric
  schedulers:
    roleConfig:
      podDisruptionBudget:
        enabled: true
        maxUnavailable: 1
    roleGroups:
      default:
        replicas: 1
        podOverrides: *legacy_sidecar_overrides
  celeryExecutors:
    roleConfig:
      podDisruptionBudget:
        enabled: true
        maxUnavailable: 1
    roleGroups:
      default:
        replicas: 1
        podOverrides: *legacy_sidecar_overrides
YAML
sha256sum \
    "$evidence/postgres.yaml" \
    "$evidence/redis.yaml" \
    "$evidence/credentials.yaml" \
    "$evidence/input-cluster.yaml" \
    >> "$evidence/run-metadata.txt"
k apply -f "$evidence/input-cluster.yaml"
wait_for 'baseline Airflow roles ready' ready_airflow
wait_for 'baseline legacy metrics topology observed' legacy_metrics_topology_observed
wait_for 'write persistent business markers' write_business_markers
snapshot before
start_statefulset_guard

echo 'Upgrade controller in place while sampling StatefulSet identity and replica intent.'
quiesce_operator
patch_rollout_phase after admin-upgrade admin-upgrade@example.com
deploy_operator "$evidence/framework-operator.yaml" after
wait_for_reconciliation before after
snapshot after

echo 'Rollback controller in place while keeping the expanded CRD.'
quiesce_operator
patch_rollout_phase rollback admin-rollback admin-rollback@example.com
python3 - "$evidence/baseline-operator.yaml" "$scratch/rollback-operator.yaml" <<'PY'
import pathlib, sys
documents = pathlib.Path(sys.argv[1]).read_text().split("\n---\n")
pathlib.Path(sys.argv[2]).write_text("\n---\n".join(
    document
    for document in documents
    if "\nkind: CustomResourceDefinition\n" not in "\n" + document
))
PY
deploy_operator "$scratch/rollback-operator.yaml" rollback
wait_for_reconciliation after rollback
wait_for 'legacy pod template and live pods are restored' legacy_workload_restored
k get airflowcluster upgrade -o json > "$evidence/rollback-pre-status-cleanup-cluster.json"
python3 - "$evidence/rollback-pre-status-cleanup-cluster.json" \
    "$scratch/rollback-status-cleanup.json" <<'PY'
import json, pathlib, sys

cluster = json.load(open(sys.argv[1]))
observed = cluster.get("status", {}).get("observedGeneration")
assert isinstance(observed, int)
assert observed < cluster["metadata"]["generation"]
cluster["status"] = {}
pathlib.Path(sys.argv[2]).write_text(json.dumps(cluster) + "\n")
PY
kubectl replace \
    --raw "/apis/airflow.kubedoop.dev/v1alpha1/namespaces/$namespace/airflowclusters/upgrade/status" \
    -f "$scratch/rollback-status-cleanup.json" \
    > "$evidence/rollback-status-cleanup-response.json"
k get airflowcluster upgrade -o json | python3 -c '
import json, sys
assert not json.load(sys.stdin).get("status")
'
k get airflowcluster,sts,cm,svc,sa,pdb,deploy,pod,pvc -o json \
    > "$evidence/rollback-pre-cleanup-resources.json"
# The legacy reconciler has no slots for these framework-only resources. Remove them after the
# workload has rolled back; the dedicated ServiceAccount was deliberately unused by the in-place
# framework phase so the live Gen 2 workload identity remained stable. Airflow StatefulSets remain.
k delete svc \
    upgrade-webservers-default-headless \
    upgrade-schedulers-default-headless \
    upgrade-celeryexecutors-default-headless \
    --ignore-not-found
k delete serviceaccount airflowcluster-upgrade --ignore-not-found
snapshot rollback
stop_statefulset_guard

python3 "$root/hack/compare-framework-upgrade.py" "$evidence"
[[ "$(git -C "$root" rev-parse HEAD)" == "$framework_ref" ]]
[[ -z "$(git -C "$root" status --porcelain --untracked-files=all)" ]] || {
    echo 'Framework source changed during the acceptance run; discard this evidence.' >&2
    exit 1
}
kubectl delete namespace "$namespace" --wait=true --timeout=300s
deploy_operator "$evidence/framework-operator.yaml" restored
printf '%s\n' \
    "PASS: Airflow $product_version baseline -> framework -> baseline; sampled StatefulSet identities, completed pod revisions, Vector runtime, status cleanup, PostgreSQL data, Redis, and Airflow variables preserved" \
    | tee "$evidence/result.txt"
