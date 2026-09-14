package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	legacyStatefulSetKind = "StatefulSet"
	legacyConfigMapKind   = "ConfigMap"
	legacyServiceKind     = "Service"
)

// validateLegacyRoleGroupResourceNames prevents a pre-framework role group from silently turning
// into a second workload when operator-go's bounded resource-name function changes its name.
//
// Gen 2 used the literal "<cluster>-<role>-<group>" for its fixed resources. That name can be a
// valid StatefulSet and Service name up to 63 characters, while operator-go v0.13 bounds the base
// name to 54 characters so that its new "-headless" Service suffix still fits. A live Gen 2 base
// name in the 55..63 range therefore cannot occupy the v0.13 fixed slots. Continuing would create
// the hashed v0.13 StatefulSet beside the still-owned legacy one.
func validateLegacyRoleGroupResourceNames(
	ctx context.Context,
	k8sClient client.Client,
	cr *airflowv1alpha1.AirflowCluster,
) error {
	spec := cr.GetSpec()
	for _, roleName := range slices.Sorted(maps.Keys(spec.Roles)) {
		role := spec.Roles[roleName]
		for _, groupName := range slices.Sorted(maps.Keys(role.RoleGroups)) {
			legacyName := fmt.Sprintf("%s-%s-%s", cr.Name, roleName, groupName)
			canonicalName := reconciler.RoleGroupResourceName(cr.Name, roleName, groupName)
			if legacyName == canonicalName {
				continue
			}

			key := types.NamespacedName{Namespace: cr.Namespace, Name: legacyName}
			for _, candidate := range []client.Object{
				&appsv1.StatefulSet{},
				&corev1.ConfigMap{},
				&corev1.Service{},
			} {
				if err := k8sClient.Get(ctx, key, candidate); err != nil {
					if apierrors.IsNotFound(err) {
						continue
					}
					return fmt.Errorf("check legacy role-group resource %s/%s: %w", key.Namespace, key.Name, err)
				}
				if !metav1.IsControlledBy(candidate, cr) {
					continue
				}

				return fmt.Errorf(
					"cannot migrate legacy %s %s/%s in place: operator-go v0.13 maps role %q group %q to the fixed resource name %q; continuing would create a second workload. Follow the operator-go v0.13 migration guide to rename or recreate this role group before upgrading",
					resourceKind(candidate),
					key.Namespace,
					key.Name,
					roleName,
					groupName,
					canonicalName,
				)
			}
		}
	}
	return nil
}

func resourceKind(obj client.Object) string {
	switch obj.(type) {
	case *appsv1.StatefulSet:
		return legacyStatefulSetKind
	case *corev1.ConfigMap:
		return legacyConfigMapKind
	case *corev1.Service:
		return legacyServiceKind
	default:
		return fmt.Sprintf("%T", obj)
	}
}
