#!/usr/bin/env python3
"""Validate Airflow's in-place framework upgrade and rollback evidence."""

from __future__ import annotations

import copy
import difflib
import json
import pathlib
import sys
from typing import Any


AIRFLOW_ROLES = ("webservers", "schedulers", "celeryexecutors")
AIRFLOW_PDB_ROLES = ("schedulers", "celeryexecutors")
AIRFLOW_STATEFULSETS = tuple(f"upgrade-{role}-default" for role in AIRFLOW_ROLES)
PHASES = ("before", "after", "rollback")
PHASE_ANNOTATION = "airflow.kubedoop.dev/upgrade-phase"
FRAMEWORK_SERVICE_ACCOUNT = "airflowcluster-upgrade"
ROLE_PDB_SLOT_LABEL = "pdb.kubedoop.dev/role"
LEGACY_LAST_APPLIED_ANNOTATION = "banzaicloud.com/last-applied"
EXPECTED_ADMIN_USERS = {
    "before": ("admin", "admin@example.com"),
    "after": ("admin-upgrade", "admin-upgrade@example.com"),
    "rollback": ("admin-rollback", "admin-rollback@example.com"),
}


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def resources(root: pathlib.Path, phase: str) -> dict[tuple[str, str], dict[str, Any]]:
    items = json.loads((root / f"{phase}-resources.json").read_text())["items"]
    return {(item["kind"], item["metadata"]["name"]): item for item in items}


def remove_phase_markers(value: Any) -> None:
    if isinstance(value, dict):
        value.pop(PHASE_ANNOTATION, None)
        for child in value.values():
            remove_phase_markers(child)
    elif isinstance(value, list):
        for child in value:
            remove_phase_markers(child)


def normalize(
    value: dict[str, Any], *, remove_owner_references: bool = False
) -> dict[str, Any]:
    value = copy.deepcopy(value)
    value.pop("status", None)
    metadata = value["metadata"]
    for key in (
        "uid",
        "resourceVersion",
        "generation",
        "creationTimestamp",
        "managedFields",
    ):
        metadata.pop(key, None)
    annotations = metadata.get("annotations", {})
    annotations.pop("kubectl.kubernetes.io/last-applied-configuration", None)
    annotations.pop(LEGACY_LAST_APPLIED_ANNOTATION, None)
    if remove_owner_references:
        metadata.pop("ownerReferences", None)
    else:
        for owner in metadata.get("ownerReferences", []):
            owner.pop("uid", None)
    if value["kind"] == "Service":
        for key in ("clusterIP", "clusterIPs", "healthCheckNodePort"):
            value["spec"].pop(key, None)
        for port in value["spec"].get("ports", []):
            port.pop("nodePort", None)
    if value["kind"] == "PersistentVolumeClaim":
        value["spec"].pop("volumeName", None)
    remove_phase_markers(value)
    return value


def phase_business(root: pathlib.Path, phase: str) -> dict[str, str]:
    return json.loads((root / f"{phase}-business.json").read_text())


def assert_phase_admin(root: pathlib.Path, phase: str) -> None:
    user = json.loads((root / f"{phase}-admin-user.json").read_text())
    expected_username, expected_email = EXPECTED_ADMIN_USERS[phase]
    require(
        user.get("username") == expected_username and user.get("email") == expected_email,
        f"{phase}: expected Airflow administrator {expected_username}/{expected_email}",
    )


def assert_ready(stateful_set: dict[str, Any]) -> None:
    name = stateful_set["metadata"]["name"]
    desired = stateful_set["spec"].get("replicas", 1)
    status = stateful_set.get("status", {})
    for field in ("readyReplicas", "currentReplicas", "updatedReplicas"):
        require(status.get(field, 0) == desired, f"{name}: {field} is not {desired}")
    require(
        status.get("observedGeneration", 0) >= stateful_set["metadata"]["generation"],
        f"{name}: controller has not observed the desired generation",
    )
    require(
        status.get("currentRevision") == status.get("updateRevision"),
        f"{name}: StatefulSet rollout is incomplete",
    )


def service_identity(service: dict[str, Any]) -> tuple[Any, ...]:
    spec = service["spec"]
    return (
        service["metadata"]["uid"],
        spec.get("clusterIP"),
        tuple(spec.get("clusterIPs", [])),
    )


def service_target_port(service: dict[str, Any], name: str) -> str | int:
    matches = [port for port in service["spec"]["ports"] if port["name"] == name]
    require(
        len(matches) == 1,
        f"{service['metadata']['name']}: expected one Service port named {name}",
    )
    return matches[0]["targetPort"]


def legacy_web_service_variants(cluster_name: str) -> tuple[dict[str, Any], ...]:
    client_labels = {
        "app.kubernetes.io/instance": cluster_name,
        "app.kubernetes.io/managed-by": "kubedoop.dev",
    }
    role_group_labels = {
        "app.kubernetes.io/component": "webservers",
        "app.kubernetes.io/instance": cluster_name,
        "app.kubernetes.io/managed-by": "airflow.kubedoop.dev",
        "app.kubernetes.io/name": "airflowcluster",
        "app.kubernetes.io/role-group": "default",
    }
    metrics_annotations = {
        "prometheus.io/path": "/metrics",
        "prometheus.io/port": "9102",
        "prometheus.io/scheme": "http",
        "prometheus.io/scrape": "true",
    }
    metrics_port = {
        "name": "metrics",
        "port": 9102,
        "protocol": "TCP",
        "targetPort": "metrics",
    }
    return (
        {
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
        },
        {
            "labels": {**role_group_labels, "prometheus.io/scrape": "true"},
            "annotations": metrics_annotations,
            "selector": role_group_labels,
            "ports": [metrics_port],
        },
    )


def legacy_web_service_projection(service: dict[str, Any]) -> dict[str, Any]:
    annotations = copy.deepcopy(service["metadata"].get("annotations", {}))
    annotations.pop(LEGACY_LAST_APPLIED_ANNOTATION, None)
    annotations.pop("kubectl.kubernetes.io/last-applied-configuration", None)
    return {
        "labels": service["metadata"].get("labels", {}),
        "annotations": annotations,
        "selector": service["spec"].get("selector", {}),
        "ports": service["spec"].get("ports", []),
    }


def assert_legacy_web_service_variant(
    service: dict[str, Any], *, phase: str, cluster_name: str
) -> None:
    require(
        legacy_web_service_projection(service)
        in legacy_web_service_variants(cluster_name),
        f"{phase}: legacy webserver Service is neither known Gen 2 variant",
    )


def canonicalize_legacy_web_service(
    service: dict[str, Any], cluster_name: str
) -> dict[str, Any]:
    service = copy.deepcopy(service)
    canonical = legacy_web_service_variants(cluster_name)[1]
    service["metadata"]["labels"] = copy.deepcopy(canonical["labels"])
    service["metadata"]["annotations"] = copy.deepcopy(canonical["annotations"])
    service["spec"]["selector"] = copy.deepcopy(canonical["selector"])
    service["spec"]["ports"] = copy.deepcopy(canonical["ports"])
    return service


def pod(snapshot: dict[tuple[str, str], dict[str, Any]], name: str) -> dict[str, Any]:
    return snapshot[("Pod", name)]


def container_names(pod_spec: dict[str, Any], field: str) -> set[str]:
    return {container["name"] for container in pod_spec.get(field, [])}


def named_container(
    pod_spec: dict[str, Any], field: str, name: str
) -> dict[str, Any]:
    matches = [
        container
        for container in pod_spec.get(field, [])
        if container["name"] == name
    ]
    require(len(matches) == 1, f"expected one {field} entry named {name}")
    return matches[0]


def env_value(container: dict[str, Any], name: str) -> str | None:
    return next(
        (entry.get("value") for entry in container.get("env", []) if entry["name"] == name),
        None,
    )


def assert_managed_sidecar_migration(
    phase_specs: dict[str, tuple[dict[str, Any], dict[str, Any]]],
    *,
    stateful_set_name: str,
) -> None:
    """Verify the real Gen 2 sidecar layout in templates and live pods."""

    require(
        set(phase_specs) == set(PHASES),
        f"{stateful_set_name}: sidecar evidence does not contain all phases",
    )
    native_sidecars = {"metric", "vector"}
    for phase in PHASES:
        template_spec, pod_spec = phase_specs[phase]
        marker_field = "initContainers" if phase == "after" else "containers"
        for view, observed in (("template", template_spec), ("live pod", pod_spec)):
            regular = container_names(observed, "containers")
            native = container_names(observed, "initContainers")
            if phase == "after":
                require(
                    not native_sidecars.intersection(regular)
                    and native_sidecars.issubset(native),
                    f"{stateful_set_name}/{phase}: framework {view} does not contain "
                    "both managed native sidecars",
                )
            else:
                require(
                    "metric" in regular
                    and "vector" not in regular
                    and not native_sidecars.intersection(native),
                    f"{stateful_set_name}/{phase}: {view} does not match the real "
                    "Gen 2 metric-only sidecar layout",
                )
            require(
                env_value(
                    named_container(observed, marker_field, "metric"),
                    "UPGRADE_OVERRIDE_MARKER",
                )
                == "metric",
                f"{stateful_set_name}/{phase}: metric override missing from {view}",
            )


def assert_vector_runtime(
    pod_resource: dict[str, Any], *, stateful_set_name: str
) -> None:
    """Verify that the framework-native Vector restartable init sidecar is healthy."""

    matches = [
        status
        for status in pod_resource.get("status", {}).get("initContainerStatuses", [])
        if status.get("name") == "vector"
    ]
    require(
        len(matches) == 1,
        f"{stateful_set_name}/after: expected one Vector init-container status",
    )
    status = matches[0]
    require(
        status.get("ready") is True,
        f"{stateful_set_name}/after: Vector sidecar is not ready",
    )
    require(
        bool(status.get("state", {}).get("running")),
        f"{stateful_set_name}/after: Vector sidecar is not running",
    )
    require(
        status.get("restartCount") == 0,
        f"{stateful_set_name}/after: Vector sidecar restarted",
    )


def assert_rollback_status_cleanup(
    root: pathlib.Path, rollback_cluster: dict[str, Any]
) -> None:
    """Prove that stale v0.13 status was observed and cleared after Gen 2 rollback."""

    pre_cleanup = json.loads(
        (root / "rollback-pre-status-cleanup-cluster.json").read_text()
    )
    status = pre_cleanup.get("status", {})
    observed_generation = status.get("observedGeneration")
    current_generation = pre_cleanup["metadata"]["generation"]
    require(
        isinstance(observed_generation, int)
        and observed_generation < current_generation,
        "rollback: expected stale v0.13 status before explicit cleanup",
    )
    require(
        not rollback_cluster.get("status"),
        "rollback: stale v0.13 status remained after explicit cleanup",
    )


def controller_image(root: pathlib.Path, phase: str) -> tuple[str, str]:
    items = json.loads((root / f"{phase}-controller.json").read_text())["items"]
    deployment = next(item for item in items if item["kind"] == "Deployment")
    image = deployment["spec"]["template"]["spec"]["containers"][0]["image"]
    ready_pods = []
    for item in items:
        if item["kind"] != "Pod":
            continue
        if item["spec"]["containers"][0]["image"] != image:
            continue
        conditions = {
            condition["type"]: condition["status"]
            for condition in item.get("status", {}).get("conditions", [])
        }
        if conditions.get("Ready") == "True":
            ready_pods.append(item)
    require(len(ready_pods) == 1, f"{phase}: expected one ready controller pod for {image}")
    statuses = ready_pods[0].get("status", {}).get("containerStatuses", [])
    require(statuses and statuses[0].get("imageID"), f"{phase}: controller imageID missing")
    return image, statuses[0]["imageID"]


def assert_role_pdb_migration(
    snapshots: dict[str, dict[tuple[str, str], dict[str, Any]]],
    *,
    cluster_name: str,
    cluster_uid: str,
) -> set[tuple[str, str]]:
    """Verify the intentional Gen 2 -> v0.13 -> Gen 2 role-PDB transition."""

    verified: set[tuple[str, str]] = set()
    for role in AIRFLOW_PDB_ROLES:
        name = f"{cluster_name}-{role}"
        key = ("PodDisruptionBudget", name)
        require(
            all(key in snapshots[phase] for phase in PHASES),
            f"{name}: role PDB missing from one or more phases",
        )

        before = snapshots["before"][key]
        after = snapshots["after"][key]
        rollback = snapshots["rollback"][key]
        pdb_uid = before["metadata"]["uid"]
        require(
            all(snapshots[phase][key]["metadata"]["uid"] == pdb_uid for phase in PHASES),
            f"{name}: role PDB was replaced",
        )
        for phase in PHASES:
            require(
                snapshots[phase][key]["spec"].get("maxUnavailable") == 1,
                f"{name}/{phase}: maxUnavailable is not 1",
            )

        before_owners = before["metadata"].get("ownerReferences", [])
        require(
            not before_owners,
            f"{name}/before: Gen 2 role PDB unexpectedly has an owner reference",
        )
        for phase, resource in (("after", after), ("rollback", rollback)):
            owners = resource["metadata"].get("ownerReferences", [])
            require(
                len(owners) == 1
                and owners[0].get("controller") is True
                and owners[0].get("apiVersion") == "airflow.kubedoop.dev/v1alpha1"
                and owners[0].get("kind") == "AirflowCluster"
                and owners[0].get("name") == cluster_name
                and owners[0].get("uid") == cluster_uid,
                f"{name}/{phase}: expected one controller owner for AirflowCluster "
                f"{cluster_name} with UID {cluster_uid}",
            )

        legacy_labels = {
            "app.kubernetes.io/component": role,
            "app.kubernetes.io/instance": cluster_name,
            "app.kubernetes.io/managed-by": "airflow.kubedoop.dev",
            "app.kubernetes.io/name": "airflowcluster",
        }
        legacy_selector = {"matchLabels": legacy_labels}
        framework_selector = {
            "matchLabels": {
                "app.kubernetes.io/component": role,
                "app.kubernetes.io/instance": cluster_name,
            }
        }
        require(
            before["spec"]["selector"] == legacy_selector,
            f"{name}/before: baseline PDB selector is not the exact Gen 2 fingerprint",
        )
        require(
            after["spec"]["selector"] == framework_selector,
            f"{name}/after: framework PDB selector is not Gen 2/Gen 3 compatible",
        )
        require(
            rollback["spec"]["selector"] == legacy_selector,
            f"{name}/rollback: legacy PDB selector was not restored",
        )
        require(
            ROLE_PDB_SLOT_LABEL not in before["metadata"].get("labels", {}),
            f"{name}/before: Gen 2 role PDB unexpectedly has the v0.13 role-slot label",
        )
        require(
            after["metadata"].get("labels", {}).get(ROLE_PDB_SLOT_LABEL) == role,
            f"{name}/after: v0.13 role-slot label is missing or incorrect",
        )
        require(
            ROLE_PDB_SLOT_LABEL not in rollback["metadata"].get("labels", {}),
            f"{name}/rollback: v0.13 role-slot label was not removed",
        )
        require(
            before["metadata"].get("labels", {}) == legacy_labels,
            f"{name}/before: baseline PDB metadata labels are not the exact Gen 2 fingerprint",
        )
        require(
            rollback["metadata"].get("labels", {}) == legacy_labels,
            f"{name}/rollback: legacy PDB metadata labels were not restored",
        )
        verified.add(key)

    webserver_key = ("PodDisruptionBudget", f"{cluster_name}-webservers")
    require(
        all(webserver_key not in snapshots[phase] for phase in PHASES),
        f"{cluster_name}-webservers: disabled PDB unexpectedly exists",
    )
    return verified


def write_normalized_evidence(
    root: pathlib.Path,
    snapshots: dict[str, dict[tuple[str, str], dict[str, Any]]],
    owner_reference_ignored_keys: set[tuple[str, str]],
    *,
    legacy_web_service_key: tuple[str, str] | None = None,
    cluster_name: str = "upgrade",
) -> dict[str, list[dict[str, Any]]]:
    normalized: dict[str, list[dict[str, Any]]] = {}
    excluded_kinds = {"Pod"}
    for phase, items in snapshots.items():
        normalized[phase] = []
        for key in sorted(items):
            if key[0] in excluded_kinds:
                continue
            item = normalize(
                items[key],
                remove_owner_references=key in owner_reference_ignored_keys,
            )
            if phase != "after" and key == legacy_web_service_key:
                item = canonicalize_legacy_web_service(item, cluster_name)
            normalized[phase].append(item)
        (root / f"{phase}-normalized.json").write_text(
            json.dumps(normalized[phase], indent=2, sort_keys=True) + "\n"
        )
    for phase in ("after", "rollback"):
        diff = difflib.unified_diff(
            (root / "before-normalized.json").read_text().splitlines(True),
            (root / f"{phase}-normalized.json").read_text().splitlines(True),
            fromfile="before",
            tofile=phase,
        )
        (root / f"{phase}.diff").write_text("".join(diff))
    return normalized


def main(root: pathlib.Path) -> None:
    require(__debug__, "PYTHONOPTIMIZE must be disabled for upgrade acceptance")
    snapshots = {phase: resources(root, phase) for phase in PHASES}
    before = snapshots["before"]
    after = snapshots["after"]
    rollback = snapshots["rollback"]

    stable_objects = (
        ("AirflowCluster", "upgrade"),
        ("StatefulSet", "airflow-postgres"),
        ("Deployment", "redis"),
    )
    for key in stable_objects:
        expected_uid = before[key]["metadata"]["uid"]
        for phase in PHASES:
            require(
                snapshots[phase][key]["metadata"]["uid"] == expected_uid,
                f"{phase}: {key} was replaced",
            )

    pvc_key = ("PersistentVolumeClaim", "data-airflow-postgres-0")
    pvc_identity = (
        before[pvc_key]["metadata"]["uid"],
        before[pvc_key]["spec"]["volumeName"],
    )
    for phase in PHASES:
        current = snapshots[phase][pvc_key]
        require(
            (current["metadata"]["uid"], current["spec"]["volumeName"]) == pvc_identity,
            f"{phase}: PostgreSQL PVC/PV identity changed",
        )

    for name in AIRFLOW_STATEFULSETS:
        key = ("StatefulSet", name)
        old = before[key]
        old_identity = (
            old["metadata"]["uid"],
            old["spec"]["selector"],
            old["spec"]["serviceName"],
            old["spec"].get("podManagementPolicy", "OrderedReady"),
        )
        pod_uids: list[str] = []
        for phase in PHASES:
            current = snapshots[phase][key]
            assert_ready(current)
            identity = (
                current["metadata"]["uid"],
                current["spec"]["selector"],
                current["spec"]["serviceName"],
                current["spec"].get("podManagementPolicy", "OrderedReady"),
            )
            require(identity == old_identity, f"{name}/{phase}: StatefulSet identity changed")
            require(
                current["spec"]["template"].get("metadata", {}).get("annotations", {}).get(
                    PHASE_ANNOTATION
                )
                == phase,
                f"{name}/{phase}: phase annotation was not rolled out",
            )
            phase_pod = pod(snapshots[phase], name + "-0")
            require(
                phase_pod.get("metadata", {}).get("annotations", {}).get(PHASE_ANNOTATION)
                == phase,
                f"{name}/{phase}: live pod is from another phase",
            )
            pod_uids.append(phase_pod["metadata"]["uid"])

        require(len(set(pod_uids)) == len(PHASES), f"{name}: a phase reused the previous pod")
        require(
            after[key]["metadata"]["generation"] > old["metadata"]["generation"],
            f"{name}: framework generation did not advance",
        )
        require(
            rollback[key]["metadata"]["generation"] > after[key]["metadata"]["generation"],
            f"{name}: rollback generation did not advance",
        )

        before_spec = old["spec"]["template"]["spec"]
        after_spec = after[key]["spec"]["template"]["spec"]
        rollback_spec = rollback[key]["spec"]["template"]["spec"]
        baseline_service_account = before_spec.get("serviceAccountName", "")
        require(
            baseline_service_account in ("", "default"),
            f"{name}: baseline ServiceAccount is unexpected",
        )
        require(
            after_spec.get("serviceAccountName", "") == baseline_service_account,
            f"{name}: framework rollout changed the legacy workload ServiceAccount",
        )
        require(
            rollback_spec.get("serviceAccountName", "") == baseline_service_account,
            f"{name}: rollback changed the legacy workload ServiceAccount",
        )
        phase_specs = {
            "before": (before_spec, pod(before, name + "-0")["spec"]),
            "after": (after_spec, pod(after, name + "-0")["spec"]),
            "rollback": (rollback_spec, pod(rollback, name + "-0")["spec"]),
        }
        assert_managed_sidecar_migration(
            phase_specs,
            stateful_set_name=name,
        )
        assert_vector_runtime(
            pod(after, name + "-0"),
            stateful_set_name=name,
        )

        for phase in PHASES:
            phase_spec = phase_specs[phase][0]
            log_volumes = [
                volume for volume in phase_spec.get("volumes", []) if volume["name"] == "log"
            ]
            require(len(log_volumes) == 1, f"{name}/{phase}: expected one log volume")
            require(
                log_volumes[0].get("emptyDir", {}).get("sizeLimit") == "384Mi",
                f"{name}/{phase}: user log-volume override was not preserved",
            )

        service_key = ("Service", name)
        expected_service_identity = service_identity(before[service_key])
        if name == "upgrade-webservers-default":
            assert_legacy_web_service_variant(
                before[service_key], phase="before", cluster_name="upgrade"
            )
            assert_legacy_web_service_variant(
                rollback[service_key], phase="rollback", cluster_name="upgrade"
            )
        require(
            service_target_port(before[service_key], "metrics") == "metrics",
            f"{name}: baseline metrics Service does not have the Gen 2 named targetPort",
        )
        require(
            service_target_port(after[service_key], "metrics") == 9102,
            f"{name}: framework metrics Service did not repair the unresolved targetPort",
        )
        require(
            service_target_port(rollback[service_key], "metrics") == "metrics",
            f"{name}: rollback did not restore the Gen 2 named metrics targetPort",
        )
        after_port_names = {port["name"] for port in after[service_key]["spec"]["ports"]}
        if name == "upgrade-webservers-default":
            require(
                after_port_names == {"http", "metrics"},
                f"{name}: framework Service must expose exactly http and metrics",
            )
            require(
                service_target_port(after[service_key], "http") == "http",
                f"{name}: framework Service did not retain the named web targetPort",
            )
            http_ports = [
                port["port"]
                for port in after[service_key]["spec"]["ports"]
                if port["name"] == "http"
            ]
            require(
                http_ports == [8080],
                f"{name}: framework Service did not expose Airflow HTTP port 8080",
            )
        else:
            require(
                after_port_names == {"metrics"},
                f"{name}: non-web framework Service exposed an unexpected client port",
            )
        config_map_key = ("ConfigMap", name)
        expected_config_map_uid = before[config_map_key]["metadata"]["uid"]
        for phase in PHASES:
            require(
                service_identity(snapshots[phase][service_key])
                == expected_service_identity,
                f"{name}/{phase}: ordinary Service UID or ClusterIP changed",
            )
            require(
                snapshots[phase][config_map_key]["metadata"]["uid"]
                == expected_config_map_uid,
                f"{name}/{phase}: role-group ConfigMap was replaced",
            )

        require(
            "airflow.log.json" in before[config_map_key]["data"]["log_config.py"],
            f"{name}: baseline Airflow log path is unexpected",
        )
        require(
            "airflow.py.json" in after[config_map_key]["data"]["log_config.py"],
            f"{name}: framework Airflow log path was not applied",
        )
        require(
            "airflow.log.json" in rollback[config_map_key]["data"]["log_config.py"],
            f"{name}: rollback did not restore the legacy Airflow log path",
        )

        headless_key = ("Service", name + "-headless")
        require(
            after[headless_key]["spec"]["clusterIP"] == "None",
            f"{name}: framework headless Service is not headless",
        )
        require(headless_key not in rollback, f"{name}: rollback retained headless Service")

    cluster_uid = before[("AirflowCluster", "upgrade")]["metadata"]["uid"]
    assert_rollback_status_cleanup(
        root,
        rollback[("AirflowCluster", "upgrade")],
    )
    verified_role_pdbs = assert_role_pdb_migration(
        snapshots,
        cluster_name="upgrade",
        cluster_uid=cluster_uid,
    )

    framework_sa = ("ServiceAccount", FRAMEWORK_SERVICE_ACCOUNT)
    require(framework_sa in after, "framework ServiceAccount missing after upgrade")
    require(framework_sa not in rollback, "framework ServiceAccount remained after rollback")

    business = [phase_business(root, phase) for phase in PHASES]
    require(business[0] == business[1] == business[2], "business markers changed")
    require(
        business[0]["airflow_variable"] == "airflow-framework-upgrade",
        "Airflow Variable marker changed",
    )
    require(
        business[0]["redis_marker"] == "airflow-framework-upgrade",
        "Redis marker changed",
    )
    for phase in PHASES:
        assert_phase_admin(root, phase)

    before_controller = controller_image(root, "before")
    after_controller = controller_image(root, "after")
    rollback_controller = controller_image(root, "rollback")
    require(
        before_controller[0] != after_controller[0],
        "framework controller image tag equals the baseline tag",
    )
    require(
        rollback_controller[0] == before_controller[0],
        "rollback did not restore the baseline controller image",
    )
    require(
        after_controller[1] != before_controller[1],
        "framework controller pod did not run a different imageID",
    )

    # Gen 2 created role PDBs without owner references. v0.13 adoption adds the
    # AirflowCluster controller owner, and the old controller does not remove it on rollback.
    # assert_role_pdb_migration verifies that exact transition before these two PDBs alone
    # receive the owner-reference normalization needed for baseline parity.
    normalized = write_normalized_evidence(
        root,
        snapshots,
        verified_role_pdbs,
        legacy_web_service_key=("Service", "upgrade-webservers-default"),
        cluster_name="upgrade",
    )
    require(
        normalized["rollback"] == normalized["before"],
        "rollback resources differ from baseline after normalization; inspect rollback.diff",
    )

    print(
        "Airflow migration contracts passed: completed pod revisions, StatefulSet/Service/PDB "
        "identities, PostgreSQL PVC, Redis, business markers, and phase administrators survived; "
        "normalized rollback matches baseline."
    )


if __name__ == "__main__":
    main(pathlib.Path(sys.argv[1]))
