package controller

import (
	"fmt"
	"maps"
	"slices"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
)

type airflowServiceSlot struct {
	role  string
	group string
	kind  string
}

func (s airflowServiceSlot) description() string {
	return fmt.Sprintf("role %q group %q %s Service slot", s.role, s.group, s.kind)
}

// validateRoleGroupServiceSlots rejects Service identities claimed by two role groups.
//
// Airflow fills both of the framework's per-group Service slots: the ordinary slot contains the
// legacy metrics Service and the suffixed slot is the StatefulSet's headless governing Service.
// A group named "<x>-headless" can therefore claim the same Service as group "<x>". The framework
// validates slots one group at a time, after reconciliation has already entered the role loop, so
// it cannot detect that sibling collision. Reject it from the complete spec before any API lookup
// or role resource apply instead.
//
// TODO(operator-go): promote — https://github.com/zncdatadev/operator-go/issues/644
func validateRoleGroupServiceSlots(cr *airflowv1alpha1.AirflowCluster) error {
	claims := map[string]airflowServiceSlot{}
	spec := cr.GetSpec()
	for _, roleName := range slices.Sorted(maps.Keys(spec.Roles)) {
		role := spec.Roles[roleName]
		for _, groupName := range slices.Sorted(maps.Keys(role.RoleGroups)) {
			resourceName := reconciler.RoleGroupResourceName(cr.Name, roleName, groupName)
			for _, slot := range []struct {
				name string
				kind string
			}{
				{name: resourceName, kind: "metrics"},
				{name: resourceName + "-headless", kind: "headless"},
			} {
				claim := airflowServiceSlot{role: roleName, group: groupName, kind: slot.kind}
				if previous, exists := claims[slot.name]; exists {
					return fmt.Errorf(
						"service resource name collision in namespace %q: %s and %s both resolve to %q; rename one role group so every metrics and headless Service has a distinct canonical name before upgrading or reconciling",
						cr.Namespace,
						previous.description(),
						claim.description(),
						slot.name,
					)
				}
				claims[slot.name] = claim
			}
		}
	}
	return nil
}
