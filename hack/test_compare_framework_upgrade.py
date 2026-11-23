#!/usr/bin/env python3
"""Unit tests for the framework-upgrade evidence comparator."""

from __future__ import annotations

import copy
import importlib.util
import json
import pathlib
import tempfile
import unittest
from typing import Any


SCRIPT = pathlib.Path(__file__).with_name("compare-framework-upgrade.py")
SPEC = importlib.util.spec_from_file_location("compare_framework_upgrade", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot import comparator from {SCRIPT}")
COMPARATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(COMPARATOR)


class AdminEvidenceTest(unittest.TestCase):
    def test_phase_admin_contract(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            for phase, (username, email) in COMPARATOR.EXPECTED_ADMIN_USERS.items():
                (root / f"{phase}-admin-user.json").write_text(
                    '{"username":"' + username + '","email":"' + email + '"}'
                )
                COMPARATOR.assert_phase_admin(root, phase)

            (root / "after-admin-user.json").write_text(
                '{"username":"admin","email":"admin@example.com"}'
            )
            with self.assertRaisesRegex(AssertionError, "after: expected"):
                COMPARATOR.assert_phase_admin(root, "after")


class ManagedSidecarMigrationTest(unittest.TestCase):
    def container(self, name: str, *, marker: bool = False) -> dict[str, Any]:
        container: dict[str, Any] = {"name": name}
        if marker:
            container["env"] = [
                {"name": "UPGRADE_OVERRIDE_MARKER", "value": "metric"}
            ]
        return container

    def valid_phase_specs(
        self,
    ) -> dict[str, tuple[dict[str, Any], dict[str, Any]]]:
        result: dict[str, tuple[dict[str, Any], dict[str, Any]]] = {}
        for phase in COMPARATOR.PHASES:
            field = "initContainers" if phase == "after" else "containers"
            companions = [self.container("metric", marker=True)]
            if phase == "after":
                companions.append(self.container("vector"))
            spec = {"containers": [], "initContainers": []}
            spec[field] = companions
            result[phase] = (copy.deepcopy(spec), copy.deepcopy(spec))
        return result

    def test_valid_real_gen2_framework_rollback_layout(self) -> None:
        COMPARATOR.assert_managed_sidecar_migration(
            self.valid_phase_specs(),
            stateful_set_name="upgrade-webservers-default",
        )

    def test_rejects_wrong_template_or_live_pod_layout(self) -> None:
        cases = []

        baseline_vector = self.valid_phase_specs()
        baseline_vector["before"][0]["containers"].append(self.container("vector"))
        cases.append(("baseline-vector", baseline_vector, "metric-only"))

        missing_live_vector = self.valid_phase_specs()
        missing_live_vector["after"][1]["initContainers"] = [
            self.container("metric", marker=True)
        ]
        cases.append(("missing-live-vector", missing_live_vector, "native sidecars"))

        for name, phase_specs, pattern in cases:
            with self.subTest(name=name):
                with self.assertRaisesRegex(AssertionError, pattern):
                    COMPARATOR.assert_managed_sidecar_migration(
                        phase_specs,
                        stateful_set_name="upgrade-webservers-default",
                    )

    def test_vector_runtime_status_contract(self) -> None:
        pod = {
            "status": {
                "initContainerStatuses": [
                    {
                        "name": "vector",
                        "ready": True,
                        "restartCount": 0,
                        "state": {"running": {"startedAt": "2026-09-15T00:00:00Z"}},
                    }
                ]
            }
        }
        COMPARATOR.assert_vector_runtime(
            pod,
            stateful_set_name="upgrade-webservers-default",
        )

        cases = []
        missing = copy.deepcopy(pod)
        missing["status"]["initContainerStatuses"] = []
        cases.append(("missing", missing, "one Vector"))
        unready = copy.deepcopy(pod)
        unready["status"]["initContainerStatuses"][0]["ready"] = False
        cases.append(("unready", unready, "not ready"))
        terminated = copy.deepcopy(pod)
        terminated["status"]["initContainerStatuses"][0]["state"] = {
            "terminated": {"exitCode": 1}
        }
        cases.append(("terminated", terminated, "not running"))
        restarted = copy.deepcopy(pod)
        restarted["status"]["initContainerStatuses"][0]["restartCount"] = 1
        cases.append(("restarted", restarted, "restarted"))

        for name, observed, pattern in cases:
            with self.subTest(name=name):
                with self.assertRaisesRegex(AssertionError, pattern):
                    COMPARATOR.assert_vector_runtime(
                        observed,
                        stateful_set_name="upgrade-webservers-default",
                    )


class LegacyWebServiceTest(unittest.TestCase):
    def service(self, variant: int) -> dict[str, Any]:
        projection = copy.deepcopy(
            COMPARATOR.legacy_web_service_variants("upgrade")[variant]
        )
        projection["annotations"][COMPARATOR.LEGACY_LAST_APPLIED_ANNOTATION] = (
            "legacy-bookkeeping"
        )
        projection["annotations"][
            "kubectl.kubernetes.io/last-applied-configuration"
        ] = "kubectl-bookkeeping"
        return {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "name": "upgrade-webservers-default",
                "uid": "stable-service-uid",
                "labels": projection["labels"],
                "annotations": projection["annotations"],
            },
            "spec": {
                "clusterIP": "10.96.0.42",
                "clusterIPs": ["10.96.0.42"],
                "selector": projection["selector"],
                "ports": projection["ports"],
            },
        }

    def test_accepts_both_strict_gen2_variants_and_canonicalizes_them(self) -> None:
        variants = [self.service(0), self.service(1)]
        for service in variants:
            COMPARATOR.assert_legacy_web_service_variant(
                service,
                phase="before",
                cluster_name="upgrade",
            )

        normalized = [COMPARATOR.normalize(service) for service in variants]
        for service in normalized:
            self.assertNotIn(
                COMPARATOR.LEGACY_LAST_APPLIED_ANNOTATION,
                service["metadata"].get("annotations", {}),
            )
        canonical = [
            COMPARATOR.canonicalize_legacy_web_service(service, "upgrade")
            for service in normalized
        ]
        self.assertEqual(canonical[0], canonical[1])

    def test_rejects_unknown_gen2_variant(self) -> None:
        service = self.service(1)
        service["spec"]["ports"][0]["targetPort"] = 9102
        with self.assertRaisesRegex(AssertionError, "neither known Gen 2 variant"):
            COMPARATOR.assert_legacy_web_service_variant(
                service,
                phase="rollback",
                cluster_name="upgrade",
            )


class RollbackStatusCleanupTest(unittest.TestCase):
    def test_requires_stale_pre_cleanup_and_empty_final_status(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            pre_cleanup = {
                "metadata": {"generation": 8},
                "status": {"observedGeneration": 7, "conditions": [{"type": "Available"}]},
            }
            (root / "rollback-pre-status-cleanup-cluster.json").write_text(
                json.dumps(pre_cleanup)
            )
            COMPARATOR.assert_rollback_status_cleanup(root, {"status": {}})

            current = copy.deepcopy(pre_cleanup)
            current["status"]["observedGeneration"] = 8
            (root / "rollback-pre-status-cleanup-cluster.json").write_text(
                json.dumps(current)
            )
            with self.assertRaisesRegex(AssertionError, "expected stale"):
                COMPARATOR.assert_rollback_status_cleanup(root, {"status": {}})

            (root / "rollback-pre-status-cleanup-cluster.json").write_text(
                json.dumps(pre_cleanup)
            )
            with self.assertRaisesRegex(AssertionError, "remained"):
                COMPARATOR.assert_rollback_status_cleanup(
                    root,
                    {"status": {"observedGeneration": 7}},
                )


class RolePDBMigrationTest(unittest.TestCase):
    cluster_name = "upgrade"
    cluster_uid = "airflow-cluster-uid"

    def legacy_selector(self, role: str) -> dict[str, Any]:
        return {
            "matchLabels": {
                "app.kubernetes.io/component": role,
                "app.kubernetes.io/instance": self.cluster_name,
                "app.kubernetes.io/managed-by": "airflow.kubedoop.dev",
                "app.kubernetes.io/name": "airflowcluster",
            }
        }

    def framework_selector(self, role: str) -> dict[str, Any]:
        return {
            "matchLabels": {
                "app.kubernetes.io/component": role,
                "app.kubernetes.io/instance": self.cluster_name,
            }
        }

    def owner_reference(self, uid: str | None = None) -> dict[str, Any]:
        return {
            "apiVersion": "airflow.kubedoop.dev/v1alpha1",
            "kind": "AirflowCluster",
            "name": self.cluster_name,
            "uid": uid or self.cluster_uid,
            "controller": True,
            "blockOwnerDeletion": True,
        }

    def role_pdb(
        self,
        role: str,
        phase: str,
        *,
        uid: str | None = None,
    ) -> dict[str, Any]:
        labels = {
            "app.kubernetes.io/component": role,
            "app.kubernetes.io/instance": self.cluster_name,
            "app.kubernetes.io/managed-by": "airflow.kubedoop.dev",
            "app.kubernetes.io/name": "airflowcluster",
        }
        metadata: dict[str, Any] = {
            "name": f"{self.cluster_name}-{role}",
            "uid": uid or f"{role}-pdb-uid",
            "labels": labels,
        }
        selector = self.legacy_selector(role)
        if phase == "after":
            metadata["labels"][COMPARATOR.ROLE_PDB_SLOT_LABEL] = role
            selector = self.framework_selector(role)
        if phase != "before":
            metadata["ownerReferences"] = [self.owner_reference()]
        return {
            "apiVersion": "policy/v1",
            "kind": "PodDisruptionBudget",
            "metadata": metadata,
            "spec": {"maxUnavailable": 1, "selector": selector},
        }

    def valid_snapshots(
        self,
    ) -> dict[str, dict[tuple[str, str], dict[str, Any]]]:
        snapshots: dict[str, dict[tuple[str, str], dict[str, Any]]] = {
            phase: {} for phase in COMPARATOR.PHASES
        }
        for role in COMPARATOR.AIRFLOW_PDB_ROLES:
            key = ("PodDisruptionBudget", f"{self.cluster_name}-{role}")
            for phase in COMPARATOR.PHASES:
                snapshots[phase][key] = self.role_pdb(role, phase)
        return snapshots

    def assert_contract_fails(
        self,
        snapshots: dict[str, dict[tuple[str, str], dict[str, Any]]],
        pattern: str,
    ) -> None:
        with self.assertRaisesRegex(AssertionError, pattern):
            COMPARATOR.assert_role_pdb_migration(
                snapshots,
                cluster_name=self.cluster_name,
                cluster_uid=self.cluster_uid,
            )

    def test_valid_adoption_and_rollback_contract(self) -> None:
        snapshots = self.valid_snapshots()
        verified = COMPARATOR.assert_role_pdb_migration(
            snapshots,
            cluster_name=self.cluster_name,
            cluster_uid=self.cluster_uid,
        )
        expected = {
            ("PodDisruptionBudget", f"{self.cluster_name}-{role}")
            for role in COMPARATOR.AIRFLOW_PDB_ROLES
        }
        self.assertEqual(verified, expected)

        with tempfile.TemporaryDirectory() as directory:
            normalized = COMPARATOR.write_normalized_evidence(
                pathlib.Path(directory), snapshots, verified
            )
        self.assertEqual(normalized["rollback"], normalized["before"])

        ordinary_before = {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": "ordinary"},
        }
        ordinary_rollback = copy.deepcopy(ordinary_before)
        ordinary_rollback["metadata"]["ownerReferences"] = [self.owner_reference()]
        self.assertNotEqual(
            COMPARATOR.normalize(ordinary_before),
            COMPARATOR.normalize(ordinary_rollback),
        )

    def test_owner_contract_errors(self) -> None:
        cases = []

        before_owned = self.valid_snapshots()
        before_owned["before"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ]["ownerReferences"] = [self.owner_reference()]
        cases.append(("before-owned", before_owned))

        after_ownerless = self.valid_snapshots()
        after_ownerless["after"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ].pop("ownerReferences")
        cases.append(("after-ownerless", after_ownerless))

        after_foreign = self.valid_snapshots()
        after_foreign["after"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ]["ownerReferences"] = [self.owner_reference("foreign-uid")]
        cases.append(("after-foreign", after_foreign))

        before_non_controller = self.valid_snapshots()
        before_non_controller["before"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ]["ownerReferences"] = [{"uid": "other", "controller": False}]
        cases.append(("before-non-controller", before_non_controller))

        after_extra_non_controller = self.valid_snapshots()
        after_extra_non_controller["after"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ]["ownerReferences"].append({"uid": "other", "controller": False})
        cases.append(("after-extra-non-controller", after_extra_non_controller))

        rollback_ownerless = self.valid_snapshots()
        rollback_ownerless["rollback"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ].pop("ownerReferences")
        cases.append(("rollback-ownerless", rollback_ownerless))

        for name, snapshots in cases:
            with self.subTest(name=name):
                self.assert_contract_fails(snapshots, "owner")

    def test_slot_label_contract_errors(self) -> None:
        cases = []

        after_missing = self.valid_snapshots()
        after_missing["after"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ]["labels"].pop(COMPARATOR.ROLE_PDB_SLOT_LABEL)
        cases.append(("after-missing", after_missing))

        after_wrong = self.valid_snapshots()
        after_wrong["after"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ]["labels"][COMPARATOR.ROLE_PDB_SLOT_LABEL] = "webservers"
        cases.append(("after-wrong", after_wrong))

        rollback_retained = self.valid_snapshots()
        rollback_retained["rollback"][("PodDisruptionBudget", "upgrade-schedulers")][
            "metadata"
        ]["labels"][COMPARATOR.ROLE_PDB_SLOT_LABEL] = "webservers"
        cases.append(("rollback-retained", rollback_retained))

        for name, snapshots in cases:
            with self.subTest(name=name):
                self.assert_contract_fails(snapshots, "role-slot label")

    def test_selector_contract_errors(self) -> None:
        cases = []

        after_legacy = self.valid_snapshots()
        after_legacy["after"][("PodDisruptionBudget", "upgrade-schedulers")][
            "spec"
        ]["selector"] = self.legacy_selector("schedulers")
        cases.append(("after-legacy", after_legacy))

        after_wrong_role = self.valid_snapshots()
        after_wrong_role["after"][("PodDisruptionBudget", "upgrade-schedulers")][
            "spec"
        ]["selector"] = self.framework_selector("webservers")
        cases.append(("after-wrong-role", after_wrong_role))

        rollback_framework = self.valid_snapshots()
        rollback_framework["rollback"][("PodDisruptionBudget", "upgrade-schedulers")][
            "spec"
        ]["selector"] = self.framework_selector("schedulers")
        cases.append(("rollback-framework", rollback_framework))

        for name, snapshots in cases:
            with self.subTest(name=name):
                self.assert_contract_fails(snapshots, "selector")

    def test_disabled_webserver_pdb_must_remain_absent(self) -> None:
        snapshots = self.valid_snapshots()
        key = ("PodDisruptionBudget", "upgrade-webservers")
        for phase in COMPARATOR.PHASES:
            snapshots[phase][key] = self.role_pdb("webservers", phase)
        self.assert_contract_fails(snapshots, "disabled PDB")


if __name__ == "__main__":
    unittest.main()
