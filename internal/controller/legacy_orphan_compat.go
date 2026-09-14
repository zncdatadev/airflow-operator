/*
Copyright 2024 ZNCDataDev.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/constant"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const legacyPodDisruptionBudgetKind = "PodDisruptionBudget"

type legacyRoleGroupCoordinate struct {
	role  string
	group string
}

type legacyMigrationBlocker struct {
	kind   string
	name   string
	reason string
}

// validateLegacyResourceInventory prevents a Gen 2 fixed resource from becoming invisible to
// operator-go v0.13's orphan cleaner. Gen 2 did not persist a role-group ledger in status and
// stamped managed-by=airflow.kubedoop.dev, while the v0.13 live inventory selects
// managed-by=operator-go. A role/group removed before its first Gen 3 reconcile would otherwise
// leave its old StatefulSet running indefinitely.
//
// The fingerprint is deliberately narrow: namespace and recommended labels are constrained by
// the List, then every role-group candidate must have the current CR's controller UID and the
// exact Gen 2 fixed-slot name. Gen 2 role PDBs are the exception: v0.12.6 created them without an
// owner reference, so they are recognized by their exact name and old four-label metadata and
// selector fingerprint. A PDB controlled by this CR is also recognized because an earlier v0.13
// adoption followed by a v0.12 rollback preserves that owner while restoring the old labels.
// Foreign or product-specific resources are not migration blockers.
func validateLegacyResourceInventory(
	ctx context.Context,
	k8sClient client.Client,
	cr *airflowv1alpha1.AirflowCluster,
) error {
	if cr == nil || cr.UID == "" {
		// Reconciled API objects always have a UID. Without one there is no safe ownership
		// identity to compare, so this helper has no candidate it can claim.
		return nil
	}
	if k8sClient == nil {
		return fmt.Errorf("check legacy resource inventory: Kubernetes client is required")
	}

	spec := cr.GetSpec()
	desiredGroups := make(map[legacyRoleGroupCoordinate]struct{})
	desiredRolePDBs := make(map[string]struct{})
	for roleName, roleSpec := range spec.Roles {
		for groupName := range roleSpec.RoleGroups {
			desiredGroups[legacyRoleGroupCoordinate{role: roleName, group: groupName}] = struct{}{}
		}
		if roleSpec.RoleConfig != nil &&
			roleSpec.RoleConfig.PodDisruptionBudget != nil &&
			roleSpec.RoleConfig.PodDisruptionBudget.IsEnabled() {
			desiredRolePDBs[roleName] = struct{}{}
		}
	}

	listOptions := []client.ListOption{
		client.InNamespace(cr.Namespace),
		client.MatchingLabels{
			constant.LabelKubernetesInstance:  cr.Name,
			constant.LabelKubernetesManagedBy: LabelDomain,
		},
	}
	blockers := make([]legacyMigrationBlocker, 0)
	inspectRoleGroup := func(kind string, obj client.Object) {
		if !metav1.IsControlledBy(obj, cr) {
			return
		}
		labels := obj.GetLabels()
		coordinate := legacyRoleGroupCoordinate{
			role:  labels[constant.LabelKubernetesComponent],
			group: labels[constant.LabelKubernetesRoleGroup],
		}
		if coordinate.role == "" || coordinate.group == "" ||
			obj.GetName() != fmt.Sprintf("%s-%s-%s", cr.Name, coordinate.role, coordinate.group) {
			return
		}
		if _, expected := desiredGroups[coordinate]; expected {
			return
		}
		blockers = append(blockers, legacyMigrationBlocker{
			kind: kind,
			name: obj.GetName(),
			reason: fmt.Sprintf(
				"role %q group %q is absent from the current spec",
				coordinate.role,
				coordinate.group,
			),
		})
	}

	statefulSets := &appsv1.StatefulSetList{}
	if err := k8sClient.List(ctx, statefulSets, listOptions...); err != nil {
		return fmt.Errorf("list legacy StatefulSets: %w", err)
	}
	for i := range statefulSets.Items {
		inspectRoleGroup(legacyStatefulSetKind, &statefulSets.Items[i])
	}

	configMaps := &corev1.ConfigMapList{}
	if err := k8sClient.List(ctx, configMaps, listOptions...); err != nil {
		return fmt.Errorf("list legacy ConfigMaps: %w", err)
	}
	for i := range configMaps.Items {
		inspectRoleGroup(legacyConfigMapKind, &configMaps.Items[i])
	}

	services := &corev1.ServiceList{}
	if err := k8sClient.List(ctx, services, listOptions...); err != nil {
		return fmt.Errorf("list legacy Services: %w", err)
	}
	for i := range services.Items {
		inspectRoleGroup(legacyServiceKind, &services.Items[i])
	}

	podDisruptionBudgets := &policyv1.PodDisruptionBudgetList{}
	if err := k8sClient.List(ctx, podDisruptionBudgets, listOptions...); err != nil {
		return fmt.Errorf("list legacy PodDisruptionBudgets: %w", err)
	}
	for i := range podDisruptionBudgets.Items {
		pdb := &podDisruptionBudgets.Items[i]
		labels := pdb.GetLabels()
		roleName := labels[constant.LabelKubernetesComponent]
		if !isLegacyRolePodDisruptionBudget(pdb, cr, roleName) {
			continue
		}
		if _, expected := desiredRolePDBs[roleName]; expected {
			continue
		}
		blockers = append(blockers, legacyMigrationBlocker{
			kind: legacyPodDisruptionBudgetKind,
			name: pdb.Name,
			reason: fmt.Sprintf(
				"role %q is absent or its roleConfig.podDisruptionBudget is not enabled in the current spec",
				roleName,
			),
		})
	}

	if len(blockers) == 0 {
		return nil
	}
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].kind != blockers[j].kind {
			return blockers[i].kind < blockers[j].kind
		}
		return blockers[i].name < blockers[j].name
	})
	descriptions := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		descriptions = append(descriptions, fmt.Sprintf(
			"%s %s/%s (%s)",
			blocker.kind,
			cr.Namespace,
			blocker.name,
			blocker.reason,
		))
	}

	return fmt.Errorf(
		"cannot safely migrate Gen 2 fixed resources that are no longer declared: %s; "+
			"operator-go v0.13 cannot discover resources that still carry managed-by=%s for orphan cleanup. "+
			"Use a two-phase upgrade: first upgrade with the same role groups and PDB settings so v0.13 can adopt them, "+
			"then remove the role/group or disable its PDB; or manually clean up each verified Gen 2 fixed resource before upgrading",
		strings.Join(descriptions, "; "),
		LabelDomain,
	)
}

func isLegacyRolePodDisruptionBudget(
	pdb *policyv1.PodDisruptionBudget,
	cr *airflowv1alpha1.AirflowCluster,
	roleName string,
) bool {
	if pdb == nil || cr == nil || roleName == "" ||
		pdb.Name != fmt.Sprintf("%s-%s", cr.Name, roleName) {
		return false
	}
	if controller := metav1.GetControllerOf(pdb); controller != nil && controller.UID != cr.UID {
		return false
	}

	legacyLabels := map[string]string{
		constant.LabelKubernetesInstance:  cr.Name,
		constant.LabelKubernetesName:      legacyAppName,
		constant.LabelKubernetesManagedBy: LabelDomain,
		constant.LabelKubernetesComponent: roleName,
	}
	for key, value := range legacyLabels {
		if pdb.Labels[key] != value {
			return false
		}
	}
	return pdb.Spec.Selector != nil &&
		len(pdb.Spec.Selector.MatchExpressions) == 0 &&
		maps.Equal(pdb.Spec.Selector.MatchLabels, legacyLabels)
}
