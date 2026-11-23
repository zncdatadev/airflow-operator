# operator-go v0.13 Migration Guide

This guide covers an in-place upgrade from the pre-framework Airflow operator
to the implementation based on `operator-go` v0.13.0.

## Upgrade Order Is Mandatory

Helm 3 installs files under a chart's `crds/` directory when a release is first
installed, but it does not upgrade those CRDs during `helm upgrade`. The v0.13
controller must therefore not start until the new AirflowCluster CRD has been
validated and applied explicitly.

The new controller writes the generic status fields `conditions`,
`observedGeneration`, and `roleGroups`. The last field is also used to find and
remove resources for deleted role groups. An old CRD schema can prune these
fields. The new CRD also removes the API-server defaults for
`spec.image.kubedoopVersion` and `spec.image.productVersion`; image defaults are
now resolved by the running controller.

Use this order:

1. Back up the CRD and every AirflowCluster.
2. Validate the new CRD against the target API server.
3. Apply the new CRD and wait for it to become established.
4. Upgrade the operator controller and Helm release.
5. Verify the AirflowCluster status and every role StatefulSet rollout.

Do not reverse the first four steps. Do not use `--skip-crds` as a substitute
for the explicit CRD update.

## Upgrade from a Source Checkout

Confirm the active context before running any command. `KUBECONFIG` can be set
on each command when the target cluster does not use the default kubeconfig.

```bash
kubectl config current-context

backup_dir="airflow-operator-upgrade-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$backup_dir"
kubectl get crd airflowclusters.airflow.kubedoop.dev -o yaml \
  > "$backup_dir/airflowcluster-crd.yaml"
kubectl get airflowclusters.airflow.kubedoop.dev --all-namespaces -o yaml \
  > "$backup_dir/airflowclusters.yaml"

make helm-crd-upgrade-check
make helm-crd-upgrade
```

`helm-crd-upgrade-check` regenerates the canonical chart CRD, then runs a
strict API-server dry-run. It does not change cluster state.
`helm-crd-upgrade` repeats that gate, applies the same manifest with
`kubectl apply`, and waits for the CRD's `Established` condition. Neither target
upgrades the controller.

Only after both targets succeed, upgrade the release using the same values and
overrides used for the existing installation:

```bash
helm upgrade airflow-operator deploy/helm/airflow-operator \
  --namespace airflow-operator \
  --values /path/to/release-values.yaml \
  --wait

kubectl --namespace airflow-operator rollout status \
  deployment/airflow-operator-controller-manager --timeout=5m
kubectl get airflowclusters.airflow.kubedoop.dev --all-namespaces
```

Replace the release name, namespace, and values file with those used by the
existing release.

## Upgrade from a Packaged OCI Chart

Use `helm show crds` to extract the CRD from the exact target chart version.
Both `kubectl` commands below must succeed before `helm upgrade` is run.

```bash
set -o pipefail

chart=oci://quay.io/kubedoopcharts/airflow-operator
target_version=x.y.z

helm show crds "$chart" --version "$target_version" \
  | kubectl apply --dry-run=server --validate=strict -f -
helm show crds "$chart" --version "$target_version" \
  | kubectl apply --validate=strict -f -
kubectl wait --for=condition=Established --timeout=60s \
  customresourcedefinition/airflowclusters.airflow.kubedoop.dev

helm upgrade airflow-operator "$chart" \
  --namespace airflow-operator \
  --version "$target_version" \
  --values /path/to/release-values.yaml \
  --wait
```

The upgrade commands intentionally do not use `--force-conflicts`. Stop and
inspect any validation or field-ownership error instead of forcing a CRD
update.

## Audit Existing Configuration Overrides

Before upgrading, list ConfigMap filenames claimed anywhere in role-level or
role-group `configOverrides`:

```bash
kubectl get airflowclusters.airflow.kubedoop.dev --all-namespaces -o json \
  | jq -r '
      def risky_files:
        [.. | objects | .configOverrides? // empty | keys[] |
          select(. == "webserver_config.py" or
                 . == "log_config.py" or
                 . == "vector.yaml")] | unique[];
      .items[] | . as $cluster | risky_files |
      "\($cluster.metadata.namespace)/\($cluster.metadata.name)\t\(.)"
    '
```

Resolve every result before upgrading:

- `webserver_config.py` and `log_config.py` are product-owned. Gen 2 did not
  consume overrides for these keys; v0.13 rejects them instead of silently
  replacing user data. Remove the keys and express supported changes through
  the typed authentication or logging fields.
- When Vector is enabled, `vector.yaml` is framework-owned and an override is
  rejected as a collision. Configure the aggregator and typed logging fields
  instead. With Vector explicitly disabled, a custom `vector.yaml` can remain,
  but no framework collector consumes it.

## Remove Unsupported Kubernetes Executor Configuration

The CRD has historically accepted `spec.kubernetesExecutors`, but neither the
Gen 2 controller nor this v0.13 implementation has ever reconciled that field.
The product integration currently implements only CeleryExecutor workloads.
The v0.13 CRD now rejects `spec.kubernetesExecutors` instead of continuing to
accept configuration that has no effect.

Audit every existing AirflowCluster before applying the new CRD:

```bash
kubectl get airflowclusters.airflow.kubedoop.dev --all-namespaces -o json \
  | jq -r '
      .items[]
      | select(.spec | has("kubernetesExecutors"))
      | [.metadata.namespace, .metadata.name]
      | @tsv
    '
```

Remove the field from every reported cluster before the CRD upgrade. Its
presence is not evidence that KubernetesExecutor was active; inspect the
running Airflow configuration and migrate the workload through a separately
validated product change if KubernetesExecutor is required. The API-server
dry-run in `make helm-crd-upgrade-check` validates the CRD definition but does
not revalidate stored AirflowClusters, so it cannot replace this inventory.
After the stricter CRD is installed, a later update of a remaining invalid
cluster can fail admission even when it only intends to change another field.

## Audit Gen 2 Fixed Resources Missing from the Current Spec

Gen 2 did not record a role-group inventory in AirflowCluster status and did
not reliably prune fixed resources when a role group or role-level PDB was
removed. Those resources carry
`app.kubernetes.io/managed-by=airflow.kubedoop.dev`; the v0.13 orphan cleaner
discovers live resources carrying `managed-by=operator-go`. A group removed
before its first v0.13 reconcile can therefore leave an old scheduler or
worker running indefinitely unless the migration handles it explicitly.

Run this inventory before changing the CRD or controller. For StatefulSets,
ConfigMaps, and Services it reports only Gen 2 fixed names whose controller
owner UID is the matching AirflowCluster. Gen 2 role-level PDBs were originally
created without owner references, so the PDB branch instead requires the exact
old `<cluster>-<role>` name plus the four-label Gen 2 metadata and selector
fingerprint. It accepts either no controller owner or the current cluster UID:
the latter is produced when v0.13 adopts the PDB and a subsequent Gen 2
rollback restores the old labels while retaining the owner. Every reported
object is absent from the current role-group/PDB configuration:

```bash
cluster_snapshot=$(mktemp)
trap 'rm -f "$cluster_snapshot"' EXIT

kubectl get airflowclusters.airflow.kubedoop.dev --all-namespaces -o json \
  > "$cluster_snapshot"
kubectl get \
    statefulsets.apps,configmaps,services,poddisruptionbudgets.policy \
    --all-namespaces \
    -l app.kubernetes.io/managed-by=airflow.kubedoop.dev \
    -o json \
  | jq -r --slurpfile clusters "$cluster_snapshot" '
      def role_spec($cluster; $role):
        if $role == "webservers" then $cluster.spec.webservers
        elif $role == "schedulers" then $cluster.spec.schedulers
        elif $role == "celeryexecutors" then $cluster.spec.celeryExecutors
        else null
        end;
      def pdb_enabled($role_spec):
        ($role_spec.roleConfig.podDisruptionBudget // null) as $pdb
        | ($pdb != null and
           (if $pdb.enabled == null then true else $pdb.enabled end));

      . as $resources
      | $clusters[0].items[] as $cluster
      | $resources.items[] as $resource
      | select(
          $resource.metadata.namespace == $cluster.metadata.namespace and
          $resource.metadata.labels["app.kubernetes.io/instance"] ==
            $cluster.metadata.name
        )
      | ($resource.metadata.labels["app.kubernetes.io/component"] // "") as $role
      | ($resource.metadata.labels["app.kubernetes.io/role-group"] // "") as $group
      | role_spec($cluster; $role) as $role_spec
      | {
          "app.kubernetes.io/instance": $cluster.metadata.name,
          "app.kubernetes.io/name": "airflowcluster",
          "app.kubernetes.io/managed-by": "airflow.kubedoop.dev",
          "app.kubernetes.io/component": $role
        } as $legacy_pdb_labels
      | select(
          if $resource.kind == "PodDisruptionBudget" then
            $group == "" and
            $resource.metadata.name == ($cluster.metadata.name + "-" + $role) and
            all($resource.metadata.ownerReferences[]?;
              .controller != true or .uid == $cluster.metadata.uid) and
            ($legacy_pdb_labels | to_entries |
              all(.[]; $resource.metadata.labels[.key] == .value)) and
            $resource.spec.selector.matchLabels == $legacy_pdb_labels and
            (($resource.spec.selector.matchExpressions // []) | length) == 0
          else
            $group != "" and
            $resource.metadata.name ==
              ($cluster.metadata.name + "-" + $role + "-" + $group) and
            any($resource.metadata.ownerReferences[]?;
              .controller == true and .uid == $cluster.metadata.uid)
          end
        )
      | select(
          if $resource.kind == "PodDisruptionBudget" then
            (pdb_enabled($role_spec) | not)
          else
            (($role_spec != null and
              (($role_spec.roleGroups // {}) | has($group))) | not)
          end
        )
      | [
          $resource.kind,
          $resource.metadata.namespace,
          $resource.metadata.name,
          $role,
          (if $group == "" then "-" else $group end)
        ]
      | @tsv
    '
```

Do not upgrade while this command prints a row. Choose one of these paths:

1. For a two-phase upgrade, restore every reported role group and re-enable
   every reported role PDB in the CR. Upgrade with that unchanged resource set
   and wait for one successful v0.13 reconcile, which adopts/relabels the
   resources and records the groups in status. Only then remove the group or
   disable the PDB and wait for v0.13 cleanup to finish.
2. During a maintenance window, scale each reported StatefulSet to zero. For
   StatefulSets, ConfigMaps, and Services, verify the controller owner UID
   again; for a PDB, verify its exact old metadata/selector fingerprint and
   confirm that its controller is absent or the same cluster UID. Manually
   delete only the reported object before upgrading.

The controller performs the same narrow owner/name/label inventory during
reconciliation and rejects unsafe one-step migrations with a two-phase or
manual-cleanup instruction. It only reports these resources; it never adopts
or deletes an object, and a PDB controlled by another owner is ignored.

## Audit Legacy Role-Group Resource Names

Gen 2 used the literal `<cluster>-<role>-<group>` name for each role-group
StatefulSet, ConfigMap, and Service. `operator-go` v0.13 reserves room for the
new `-headless` Service suffix, so it hashes a base name longer than 54
characters. A live Gen 2 resource whose literal base name is 55 through 63
characters cannot be adopted into the corresponding v0.13 fixed resource
slot. The v0.13 controller rejects that cluster before applying role resources
instead of starting a second workload under the hashed name.

Run this audit while the Gen 2 controller is still installed. It reports only
long fixed resources that are controller-owned by the AirflowCluster which
declares the role group:

```bash
kubectl get airflowclusters.airflow.kubedoop.dev --all-namespaces -o json \
  | jq -r '
      .items[] as $cluster
      | [
          {name: "webservers", spec: $cluster.spec.webservers},
          {name: "schedulers", spec: $cluster.spec.schedulers},
          {name: "celeryexecutors", spec: $cluster.spec.celeryExecutors}
        ][]
      | select(.spec != null) as $role
      | ($role.spec.roleGroups // {} | keys[]) as $group
      | ($cluster.metadata.name + "-" + $role.name + "-" + $group) as $resource
      | select(($resource | length) > 54)
      | [
          $cluster.metadata.namespace,
          $cluster.metadata.name,
          $cluster.metadata.uid,
          $role.name,
          $group,
          $resource,
          ($resource | length | tostring)
        ]
      | @tsv
    ' \
  | while IFS=$'\t' read -r namespace cluster uid role group resource length; do
      for kind in statefulset configmap service; do
        object=$(kubectl --namespace "$namespace" get "$kind" "$resource" \
          -o json 2>/dev/null) || continue
        if jq -e --arg uid "$uid" '
            any(.metadata.ownerReferences[]?;
              .controller == true and .uid == $uid)
          ' <<<"$object" >/dev/null; then
          printf '%s\t%s\t%s/%s\t%s\t%s\t%s chars\n' \
            "$namespace" "$cluster" "$role" "$group" \
            "$kind" "$resource" "$length"
        fi
      done
    done
```

Do not upgrade while this command prints any row. Kubernetes objects cannot be
renamed, so resolve each affected role group under the old controller during a
maintenance window:

1. Back up the AirflowCluster and record the UID, replica count, and rendered
   configuration of every reported resource.
2. Scale the affected old role group to zero and wait until its StatefulSet has
   no running pod. This prevents the old and replacement groups from executing
   concurrently.
3. Add an equivalent role group with a shorter key such that the complete
   `<cluster>-<role>-<group>` name is at most 54 characters, then restore the
   intended replica count and wait for the replacement StatefulSet to become
   ready.
4. Remove the old role-group key. Gen 2 does not reliably prune resources for a
   removed role group, so verify their controller owner UID and manually delete
   only the reported zero-replica StatefulSet, ConfigMap, and Service.
5. Run the audit again. Upgrade only after it produces no output.

If the cluster and role prefix is already too long for any practical group key,
there is no supported in-place rename. Create and validate a replacement
AirflowCluster with a shorter `metadata.name`, cut over during maintenance, and
retire the old cluster only after its external PostgreSQL database, broker, and
credentials have been verified. Do not edit owner references or pre-create a
hashed resource name to bypass the guard.

## Audit Role-Group Service Slot Collisions

Every Airflow role group has both a fixed-name metrics Service and a governing
Service with the `-headless` suffix. For short names, sibling groups named `x`
and `x-headless` would make those two logical slots resolve to the same
Kubernetes Service. They cannot safely coexist: the later role group would
replace the first group's selector and ports while Kubernetes preserves the
Service's immutable headless identity. The controller rejects the complete
cluster configuration before looking up or applying any role-group resource.

Run this audit before upgrading. The 45-character limit below is 54 characters
for the canonical base name minus the 9-character `-headless` suffix; longer
sibling names are independently hashed and do not have this literal collision.

```bash
kubectl get airflowclusters.airflow.kubedoop.dev --all-namespaces -o json \
  | jq -r '
      .items[] as $cluster
      | [
          {name: "webservers", spec: $cluster.spec.webservers},
          {name: "schedulers", spec: $cluster.spec.schedulers},
          {name: "celeryexecutors", spec: $cluster.spec.celeryExecutors}
        ][]
      | select(.spec != null) as $role
      | ($role.spec.roleGroups // {} | keys) as $groups
      | $groups[] as $group
      | ($cluster.metadata.name + "-" + $role.name + "-" + $group) as $base
      | select(($base | length) <= 45)
      | select(($groups | index($group + "-headless")) != null)
      | [
          $cluster.metadata.namespace,
          $cluster.metadata.name,
          $role.name,
          $group,
          ($group + "-headless"),
          ($base + "-headless")
        ]
      | @tsv
    '
```

Each output row identifies the namespace, cluster, role, two colliding group
keys, and the Service name they both claim. Rename either group under the Gen 2
controller using the same scale-to-zero maintenance procedure above, and run
both audits again before upgrading. For a fresh v0.13 cluster, choose distinct
group keys before creating the AirflowCluster.

## Compatibility Details

### Image defaults and the development sentinel

The old CRD persisted `spec.image.kubedoopVersion: 0.0.0-dev` when the field was
omitted. During migration, the controller treats that exact value as the old
default sentinel and resolves the Kubedoop image version from its runtime build
version. This prevents an upgraded release from remaining pinned to a
development image tag.

An existing, non-empty `productVersion` remains an explicit pin. A newly
omitted product version uses the controller's runtime default. If
`0.0.0-dev` was intentionally used rather than defaulted, use an explicit
`spec.image.custom` image before upgrading because the API cannot distinguish
the two historical cases.

### `cliOverrides`

Both the old and v0.13 forms are accepted during migration:

- A first item beginning with `-` is treated as arguments for the role's
  standard Airflow command. For example, `--port`, `9090` is forwarded to
  `airflow webserver`.
- A non-flag first item is treated as the pre-v0.13 full-command replacement.
- An explicit main-container `command` in `podOverrides` remains authoritative.
- An args-only main-container `podOverrides` retains its Gen 2 meaning: the
  items replace the bash startup script. Use `cliOverrides` rather than
  `podOverrides.args` for new Airflow subcommand arguments.

Prefer the argument form for new configuration. Keep the full-command form
only while migrating existing custom entrypoints.

### StatefulSets, Services, and PDBs

For an owned StatefulSet matching the legacy Airflow identity, the migration
preserves its UID, selector, `serviceName`, and `podManagementPolicy`. New
headless Services are added without replacing the historical fixed-name
role-group Services. The webserver Service becomes the stable union of its
HTTP client port and the metrics port; scheduler and Celery Services remain
metrics-only.

The live pod template's `serviceAccountName` is also preserved for those
legacy StatefulSets. An empty value continues to mean the namespace's default
ServiceAccount, retaining any image pull secrets, workload identity, or IRSA
configuration attached there. Fresh v0.13 clusters use the dedicated
`airflowcluster-<cluster-name>` ServiceAccount instead.

The fixed Gen 2 baseline also created each metrics Service with the named
`targetPort: metrics` but did not declare that port on a workload container.
Its EndpointSlice consequently has no resolved port, so the Service is not
usable. Scheduler and Celery EndpointSlices retain one address. The webserver
has an additional Gen 2 defect: its client and metrics reconcilers continuously
replace the mutable fields of the same Service, so an independent EndpointSlice
sample can have zero or one address. During the v0.13 phase the same Service UID
and ClusterIP are preserved, the Service is made stable, its HTTP 8080 client
port is retained, the metrics target becomes numeric port 9102, and the native
`metric` sidecar declares that port. A controller rollback restores the
original unresolved target and webserver two-shape loop. Upgrade acceptance
strictly fingerprints either known Gen 2
webserver Service shape and canonicalizes only those oscillating mutable fields
for rollback comparison; an unknown shape still fails.

To move an in-place upgraded workload to another identity later, set a
non-empty `podOverrides.spec.serviceAccountName`; that explicit value takes
precedence over the preserved live value. Kubernetes' typed pod template does
not distinguish an omitted string from an explicitly empty string here, so an
empty override cannot act as a reset. Name `default` explicitly when that is
the intended target.

The role PodDisruptionBudget keeps its existing name and is updated in place.
Its selector uses the `app.kubernetes.io/instance` and
`app.kubernetes.io/component` labels shared by old and new pods, so it remains
effective while pods roll. Before upgrading, make sure no unrelated workload
in the same namespace uses the same instance/component pair.

Gen 2 created each role PDB without an owner reference. The v0.13 adoption adds
the AirflowCluster controller owner and the `pdb.kubedoop.dev/role` slot label.
If the controller is then rolled back, Gen 2 restores its old four-label
metadata and selector but does not remove the adopted controller owner; it does
remove the v0.13 slot label when it restores the old label map. The retained
owner is expected rollback state and ties the PDB lifecycle to the same
AirflowCluster. Do not remove it merely to make a byte-for-byte comparison with
the original object.

The Gen 2 webserver Service loop returns from reconciliation before the
webserver PDB resource slot, so an enabled webserver PDB is not reliably
created by the old controller. The fixed upgrade acceptance therefore enables
and adopts the scheduler and Celery PDBs only, and explicitly proves that the
disabled webserver PDB is absent in every phase. For a real cluster that already
has, or plans to enable, a webserver PDB, record it separately: v0.13 can create
or adopt it, but a later Gen 2 rollback cannot be relied on to restore its old
metadata. Delete or restore that object manually as part of the rollback plan.

Record StatefulSet, Service, and PDB UIDs before the upgrade and verify they do
not change. A delete-and-recreate plan is not an in-place migration.

### Logging

Every role retains the Gen 2 `log` emptyDir (bounded at 500 MiB), its
`/kubedoop/log/` main-container mount, the rolling JSON file handler, and
console logging whether or not Vector is enabled. This also keeps an existing
user sidecar that mounts the product-owned `log` volume valid. Vector controls
collection only.

The generated Airflow JSON log target changes from `airflow.log.json` to
`airflow.py.json`, matching the v0.13 logging framework's parser suffix. Update
any external collector, alert, or dashboard that refers directly to the old
filename.

### Managed sidecar overrides

operator-go v0.13 represents long-running `metric` and `vector` companions as
native sidecars in `initContainers`. Existing Gen 2 `metric` overrides address
that name under `podOverrides.spec.containers`; deployments which worked
around Gen 2's missing Vector container may also have supplied a complete
custom `vector` entry there. The compatibility bridge moves either patch onto
the corresponding managed native sidecar, preserving the provider image and
defaults for fields the user did not override while applying user resources,
environment, security context, and mounts. It does not create a duplicate
regular container.

For new configuration, address these managed sidecars under
`podOverrides.spec.initContainers`. If the same name is present in both old and
new locations, the explicit `initContainers` patch wins. A disabled Vector
entry is not claimed by this bridge and remains user-owned.

Gen 2 intended to inject Vector whenever an aggregator ConfigMap name was
present, but the fixed migration baseline assembled the StatefulSet before
adding that container, so the live workload contains only the regular
`metric` sidecar. The v0.13 upgrade therefore adds Vector as a native sidecar;
rollback removes it again. The v0.13 implementation also honors an explicit
higher-precedence `logging.enableVectorAgent: false` and disables the
collector. It keeps the historical `vector.yaml` ConfigMap key as an empty
file so an existing custom mount or probe does not lose the path.

Upgrade acceptance does not infer Vector health from PodSpec placement. In the
v0.13 phase it also requires exactly one live `vector` init-container status per
role, `ready: true`, a running state, zero restarts, and a non-empty response
from Vector's `127.0.0.1:9598/metrics` endpoint. It records the response size and
SHA-256 digest for each workload.

### Known health-reporting limitation

An in-place upgraded legacy StatefulSet must retain
`app.kubernetes.io/managed-by=airflow.kubedoop.dev` in its immutable selector.
The v0.13 failing-pod scan selects `managed-by=operator-go`, so the per-pod
failure part of the `Degraded` signal can miss CrashLoopBackOff or image-pull
failures for those retained legacy StatefulSets. StatefulSet rollout and
availability conditions still operate, and newly created v0.13 clusters do not
have this selector mismatch.

The generic migration contract and this health-discovery limitation are tracked
upstream in [operator-go#645](https://github.com/zncdatadev/operator-go/issues/645).

Monitor the StatefulSet status and pod conditions directly during an in-place
upgrade; do not rely only on the AirflowCluster `Degraded` condition.

### Authentication is not covered by migration acceptance

The v0.13 refactor validates and renders exactly one LDAP or OIDC provider.
LDAP-only configuration is no longer followed by an empty OIDC block, OIDC is
rendered as a real provider list, FAB constants and Python booleans retain their
Python types, and every generated auth section is syntax-compiled in unit
tests. LDAP bind credentials use the v0.13 SecretProvisioner and the documented
`user` and `password` files; `username` remains a read-only fallback for Gen 2
SecretClasses during migration. The volume is attached only to webserver pods.

Gen 2 omitted the LDAP credentials volume even though it rendered the file
paths. If a cluster worked around that bug with a complete `podOverrides`
volume and webserver mount at `/kubedoop/secret/<SecretClass>`, the refactored
operator keeps that mount instead of adding a conflicting SecretProvisioner
mount. A mount without its matching volume remains invalid and is rejected by
the framework. After upgrading the SecretClass to emit `user` and `password`,
remove the manual volume and mount so the operator can own credential
provisioning.

The framework upgrade acceptance test still does not exercise a real LDAP or
OIDC login. This release therefore rejects configurations it cannot safely
represent instead of silently generating a broken webserver configuration:

- more than one authentication entry, including mixed LDAP and OIDC;
- OIDC without `oidc.clientCredentialsSecret`;
- an OIDC `principalClaim` other than `preferred_username`; and
- LDAP or OIDC AuthenticationClasses with TLS enabled.

Audit authentication-enabled clusters for those conditions before upgrading.
For supported cleartext providers, validate an actual login and role mapping
in a staging cluster; renderer and volume tests are not end-to-end identity
provider evidence. Do not use the generic upgrade acceptance result alone as
evidence that production authentication works.

## Migration Acceptance

The repository includes a destructive acceptance test for a dedicated, empty
Kind cluster:

```bash
git status --short
PRODUCT_VERSION=2.10.5 make framework-upgrade-e2e
make cleanup-framework-upgrade-e2e
```

`framework-upgrade-e2e` deliberately refuses a dirty checkout. The current
implementation, generated CRD/RBAC/chart artifacts, and upgrade scripts must be
committed at one clean HEAD so the evidence can be tied to an exact revision.
It also refuses an existing cluster name.

The test runs the fixed pre-framework baseline, upgrades to the current
controller, and rolls back while retaining the expanded CRD. It enables and
checks the PDB for the scheduler and Celery roles, the two slots the fixed Gen 2
controller actually reaches. For each PDB it requires an
ownerless Gen 2 baseline, the same UID plus the AirflowCluster controller owner
and v0.13 role-slot label after adoption, and the retained controller owner but
restored Gen 2 labels and selector after rollback. Only after those assertions
does normalized rollback parity ignore `ownerReferences` for those two PDBs;
owner changes on every other resource remain strict. It also requires the
disabled webserver PDB to remain absent. The test checks
StatefulSet and Service identity, pod revisions, PostgreSQL PVC identity,
Airflow and Redis state markers, the Web UI through the framework Service,
metrics endpoints, the real Gen 2
metric-only sidecar baseline, native sidecar placement and runtime health, and
preservation of a legacy `metric` container override. The legacy object's
`banzaicloud.com/last-applied` value is excluded only after all strict resource
contracts are checked because it is Gen 2 apply-engine bookkeeping that embeds
transient rollout content.

At each controller hand-off the test first scales the outgoing manager to zero
and waits for its Pod to disappear. Only then does it patch the rollout marker
and credentials and start the incoming controller. This prevents the outgoing
reconciler from consuming the next phase and makes the recorded StatefulSet
generation attributable to the intended implementation. The test queries
`airflow users list -o json` in every phase and records the exact expected
administrator, so a healthy scheduler alone cannot hide a missed or duplicate
user-creation transition. The rollback credentials must name a new user: the
Gen 2 bootstrap unconditionally runs `airflow users create`, so reusing an
existing username can crash the scheduler instead of proving rollback. Phase-
specific health and topology checks complete before the resource snapshot is
written.
Evidence is written under `target/upgrade-evidence/`.

Keep the expanded CRD during a controller rollback. The old controller ignores
the additional status schema, while rolling back the CRD first can prune status
used by the v0.13 controller during the transition. Gen 2 does not update the
generic status, however: once the rollback patch advances `.metadata.generation`,
the old v0.13 `Available` view remains visible with an older
`.status.observedGeneration`. After the legacy workloads are fully ready, save
that stale status as evidence and clear `.status` through the status subresource.
Do not treat a pre-cleanup Ready condition as current Gen 2 health. The bundled
acceptance test performs and verifies this cleanup before final parity.

## Framework Follow-ups

The migration identifies two framework follow-up candidates:

- StatefulSet immutable identity, pod-template labels, and dependent Service
  selectors are kept coherent by a local migration shim;
- failing-pod discovery through owned StatefulSets is not yet implemented, so
  legacy pods without `managed-by=operator-go` can be missed by the framework
  health scan.

Both belong upstream. Removing the local identity shim requires dedicated
upgrade evidence, not only fresh-cluster E2E coverage; closing the health gap
requires an operator-go change or a separate local compatibility path.
