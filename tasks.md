# Airflow Operator Refactoring Tasks
# Based on hdfs-operator PR #255: Upgrade kubebuilder scaffold to 4.10.1

## Overview
This document tracks the refactoring of airflow-operator to align with hdfs-operator PR #255 standards.
The refactoring focuses on upgrading kubebuilder scaffold, improving Makefile structure, updating configurations, and standardizing GitHub workflows.

---

## MODULE-001: PROJECT File Update
**Reference**: PROJECT file in PR #255
**Target**: /home/runner/work/airflow-operator/airflow-operator/PROJECT
**Changes**:
- Add `cliVersion: 4.10.1` field after the comment block
**Checkpoint**: PROJECT file contains cliVersion field

---

## MODULE-002: cmd/main.go Refactoring
**Reference**: cmd/main.go in PR #255  
**Target**: /home/runner/work/airflow-operator/airflow-operator/cmd/main.go
**Changes**:
1. Reorder imports: Move webhook and metrics/filters imports to after other controller-runtime imports
2. Add certificate path variables: metricsCertPath, metricsCertName, metricsCertKey, webhookCertPath, webhookCertName, webhookCertKey
3. Reorder variable declarations (move tlsOpts after showVersion)
4. Add flag parameters for certificate paths
5. Update webhook server initialization with certificate options
6. Update metrics server initialization with certificate options
7. Fix version string from "zookeeper-operator" to "airflow-operator" (ALREADY DONE)
8. Update controller-runtime package reference comments
9. Add blank line before +kubebuilder:scaffold:builder
**Checkpoint**: main.go follows PR #255 structure with certificate support

---

## MODULE-003: Makefile Comprehensive Refactoring
**Reference**: Makefile in PR #255
**Target**: /home/runner/work/airflow-operator/airflow-operator/Makefile
**Changes**:
1. Remove ENVTEST_K8S_VERSION from top (ALREADY PARTIALLY DONE)
2. Remove OCI_REGISTRY from top
3. Remove BUILD_TIMESTAMP and BUILD_COMMIT from top
4. Add blank line after all: build
5. Add blank line after help target
6. Update manifests target: Change controller-gen flags
7. Update generate target: Add quotes around CONTROLLER_GEN
8. Update test target: Use setup-envtest, add quotes
9. Add KIND_CLUSTER variable
10. Add setup-test-e2e target
11. Update test-e2e target
12. Add cleanup-test-e2e target
13. Add lint-config target
14. Move BUILD variables to ##@ Build section
15. Add quotes around CONTAINER_TOOL
16. Update docker-buildx target
17. Simplify build-installer target
18. Remove chart and chart-publish targets (move to helm section)
19. Update install/uninstall targets with conditional output
20. Update deploy/undeploy targets with quotes
21. Update LOCALBIN creation with quotes
22. Add KIND tool variable
23. Update tool versions (KUSTOMIZE_VERSION, CONTROLLER_TOOLS_VERSION, GOLANGCI_LINT_VERSION)
24. Add dynamic ENVTEST_VERSION and ENVTEST_K8S_VERSION  
25. Add setup-envtest target
26. Update go-install-tool function with symlink logic
27. Add gomodver function
28. Add ##@ Helm Charts section
29. Add helm-crd-sync, helm-chart-package, helm-chart-publish targets
30. Add ##@ Chainsaw E2E section
31. Update chainsaw variables and targets
32. Add setup-chainsaw-cluster target
33. Rename chainsaw-setup to setup-chainsaw-e2e
34. Rename chainsaw-test to chainsaw-e2e
35. Add cleanup-chainsaw-e2e and cleanup-chainsaw-cluster targets
**Checkpoint**: Makefile structure matches PR #255

---

## MODULE-004: .gitignore Updates
**Reference**: .gitignore in PR #255
**Target**: /home/runner/work/airflow-operator/airflow-operator/.gitignore
**Changes**:
1. Replace `**/__debug_*` section with organized sections:
   - `# Ignore kubeconfig files` with `.kubeconfig*`
   - `# Ignore docker digest files` with `docker-digests.json`
2. Add `*-local.*` pattern after `*-local.yaml`
3. Remove duplicate docker-digests.json entry
**Checkpoint**: .gitignore follows PR #255 pattern

---

## MODULE-005: GitHub Workflows Updates
**Reference**: .github/workflows/*.yml in PR #255

### MODULE-005A: chart-lint-test.yml
**Target**: /home/runner/work/airflow-operator/airflow-operator/.github/workflows/chart-lint-test.yml
**Changes**:
1. Add blank line after go-version-file
2. Fix typo: "manifest" -> "manifests" in error message
3. Add needs: check-crds-sync to linter-artifacthub
4. Add needs: check-crds-sync to lint-test  
5. Update helm action version
6. Update chart-testing-action version
7. Add helm-chart-package step before e2e
8. Update helm install command to use packaged chart
9. Update namespace naming
**Checkpoint**: chart-lint-test.yml matches PR #255

### MODULE-005B: publish.yml
**Target**: /home/runner/work/airflow-operator/airflow-operator/.github/workflows/publish.yml
**Changes**:
1. Rename chart-publish to helm-chart-publish
2. Update checkout action version
**Checkpoint**: publish.yml matches PR #255

### MODULE-005C: release.yml
**Target**: /home/runner/work/airflow-operator/airflow-operator/.github/workflows/release.yml
**Changes**:
1. Rename chainsaw-test job to chainsaw-e2e
2. Update KUBECONFIG to CHAINSAW_KUBECONFIG
3. Rename make targets: kind-create -> setup-chainsaw-cluster, chainsaw-setup -> setup-chainsaw-e2e, chainsaw-test -> chainsaw-e2e
4. Add check-crds-sync job
5. Add needs dependencies
6. Add helm-chart-package step
7. Update helm install commands
8. Update job dependencies
9. Rename chart-publish to helm-chart-publish
**Checkpoint**: release.yml matches PR #255

### MODULE-005D: test.yml
**Target**: /home/runner/work/airflow-operator/airflow-operator/.github/workflows/test.yml
**Changes**:
1. Rename chainsaw-test to chainsaw-e2e
2. Remove max-parallel comment
3. Update KUBECONFIG to CHAINSAW_KUBECONFIG
4. Rename make targets
**Checkpoint**: test.yml matches PR #255

---

## MODULE-006: Config Directory Refactoring

### MODULE-006A: CRD Updates
**Target**: config/crd/bases/airflow.kubedoop.dev_airflowclusters.yaml
**Changes**:
1. Update controller-gen version annotation from v0.17.1 to v0.19.0
**Checkpoint**: CRD has correct controller-gen version

### MODULE-006B: CRD Kustomization
**Target**: config/crd/kustomization.yaml
**Changes**:
1. Remove commented patch paths, keep only scaffold markers
2. Comment out configurations section
**Checkpoint**: CRD kustomization matches PR #255

### MODULE-006C: Remove CRD Patches
**Targets**: 
- config/crd/patches/cainjection_in_airflowclusters.yaml
- config/crd/patches/webhook_in_airflowclusters.yaml
**Changes**:
1. Delete these files if they exist
**Checkpoint**: Patch files removed

---

## MODULE-007: Config/Default Updates

### MODULE-007A: Add cert_metrics_manager_patch.yaml
**Target**: config/default/cert_metrics_manager_patch.yaml (NEW FILE)
**Changes**:
1. Create new file with metrics cert configuration
**Checkpoint**: File created with correct content

### MODULE-007B: Update kustomization.yaml
**Target**: config/default/kustomization.yaml  
**Changes**:
1. Add METRICS-WITH-CERTS patch section
2. Update webhook patch comments
3. Expand replacements section with metrics cert config
4. Update CRD CA injection scaffolding
**Checkpoint**: kustomization matches PR #255

### MODULE-007C: Update metrics_service.yaml
**Target**: config/default/metrics_service.yaml
**Changes**:
1. Fix app.kubernetes.io/name label
2. Add app.kubernetes.io/name to selector
**Checkpoint**: Service labels corrected

---

## MODULE-008: Config/Manager Updates
**Target**: config/manager/manager.yaml
**Changes**:
1. Simplify namespace labels
2. Update deployment labels
3. Update selector matchLabels
4. Update pod labels  
5. Update securityContext comments
6. Uncomment seccompProfile
7. Update args formatting
8. Add empty ports array
9. Add readOnlyRootFilesystem
10. Add empty volumeMounts and volumes arrays
**Checkpoint**: manager.yaml matches PR #255

---

## MODULE-009: Config/Network-Policy Updates
**Target**: config/network-policy/allow-metrics-traffic.yaml
**Changes**:
1. Fix grammar in comment
2. Update app.kubernetes.io/name label
3. Add app.kubernetes.io/name to podSelector matchLabels
**Checkpoint**: Network policy updated

---

## MODULE-010: Config/Prometheus Updates
**Target**: config/prometheus/monitor.yaml
**Changes**:
1. Remove blank line at top
2. Simplify labels
3. Update TLS comments
4. Add app.kubernetes.io/name to selector
**Checkpoint**: ServiceMonitor updated

---

## MODULE-011: Config/RBAC Updates

### MODULE-011A: Add admin role
**Target**: config/rbac/airflowcluster_admin_role.yaml (NEW FILE)
**Changes**:
1. Create new admin role file
**Checkpoint**: Admin role created

### MODULE-011B: Update editor role
**Target**: config/rbac/airflowcluster_editor_role.yaml
**Changes**:
1. Update comments
2. Simplify labels
**Checkpoint**: Editor role updated

### MODULE-011C: Update viewer role  
**Target**: config/rbac/airflowcluster_viewer_role.yaml
**Changes**:
1. Update comments
2. Simplify labels
**Checkpoint**: Viewer role updated

### MODULE-011D: Update kustomization
**Target**: config/rbac/kustomization.yaml
**Changes**:
1. Update comments
2. Add admin role resource
**Checkpoint**: RBAC kustomization updated

### MODULE-011E: Simplify role labels
**Targets**:
- config/rbac/leader_election_role.yaml
- config/rbac/leader_election_role_binding.yaml
- config/rbac/role_binding.yaml
- config/rbac/service_account.yaml
**Changes**:
1. Simplify labels to only app.kubernetes.io/name and app.kubernetes.io/managed-by
**Checkpoint**: All RBAC files have simplified labels

---

## MODULE-012: Test Directory Updates

### MODULE-012A: Move chainsaw config
**Source**: .chainsaw.yaml
**Target**: test/e2e/.chainsaw.yaml
**Changes**:
1. Move file if it exists at root
**Checkpoint**: Chainsaw config in correct location

### MODULE-012B: Remove kind-config.yaml
**Target**: test/e2e/kind-config.yaml
**Changes**:
1. Delete file if it exists
**Checkpoint**: File removed

---

## Final Verification Checklist
- [ ] All MODULE tasks completed
- [ ] No compilation errors
- [ ] All paths updated correctly
- [ ] No missing files
- [ ] Labels consistent across config files
- [ ] Makefile targets working
- [ ] GitHub workflows updated
- [ ] Test structure correct
