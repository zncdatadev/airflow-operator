package controller

import (
	"context"
	"strings"
	"testing"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestRoleGroupServiceSlotPreflight(t *testing.T) {
	t.Parallel()

	const namespace = "default"

	newCluster := func(name string) *airflowv1alpha1.AirflowCluster {
		return &airflowv1alpha1.AirflowCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: airflowv1alpha1.AirflowClusterSpec{
				ClusterConfig: &airflowv1alpha1.ClusterConfigSpec{Credentials: fixtureCredentialsSecret},
			},
		}
	}
	roleGroups := func(names ...string) map[string]airflowv1alpha1.RoleGroupSpec {
		groups := make(map[string]airflowv1alpha1.RoleGroupSpec, len(names))
		for _, name := range names {
			groups[name] = airflowv1alpha1.RoleGroupSpec{}
		}
		return groups
	}

	t.Run("x and x-headless are rejected before API lookup", func(t *testing.T) {
		cr := newCluster("airflow")
		cr.Spec.Webservers = &airflowv1alpha1.WebserversSpec{RoleGroups: roleGroups("x", "x-headless")}

		// A nil client makes any attempted legacy/auth API lookup panic. The collision must be
		// rejected first, using only the complete CR spec.
		handler := NewAirflowRoleGroupHandler(runtime.NewScheme())
		_, err := handler.DeclareRoles(context.Background(), nil, cr)
		if err == nil {
			t.Fatal("DeclareRoles accepted two role groups that claim the same Service")
		}

		collisionName := reconciler.RoleGroupResourceName(
			cr.Name,
			string(airflowv1alpha1.WebserversRoleName),
			"x",
		) + "-headless"
		for _, want := range []string{
			namespace,
			collisionName,
			`group "x" headless`,
			`group "x-headless" metrics`,
			"rename one role group",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})

	t.Run("same suffix pattern across roles does not collide", func(t *testing.T) {
		cr := newCluster("airflow")
		cr.Spec.Webservers = &airflowv1alpha1.WebserversSpec{RoleGroups: roleGroups("x")}
		cr.Spec.Schedulers = &airflowv1alpha1.SchedulersSpec{RoleGroups: roleGroups("x-headless")}

		if err := validateRoleGroupServiceSlots(cr); err != nil {
			t.Fatalf("cross-role names include the role and must remain distinct: %v", err)
		}
	})

	t.Run("ordinary group names do not collide", func(t *testing.T) {
		cr := newCluster("airflow")
		cr.Spec.Webservers = &airflowv1alpha1.WebserversSpec{RoleGroups: roleGroups("blue", "green")}

		if err := validateRoleGroupServiceSlots(cr); err != nil {
			t.Fatalf("non-colliding groups were rejected: %v", err)
		}
	})

	t.Run("canonical hash removes a literal suffix collision", func(t *testing.T) {
		cr := newCluster(strings.Repeat("a", 41))
		cr.Spec.Webservers = &airflowv1alpha1.WebserversSpec{RoleGroups: roleGroups("x", "x-headless")}
		roleName := string(airflowv1alpha1.WebserversRoleName)

		headlessOfX := reconciler.RoleGroupResourceName(cr.Name, roleName, "x") + "-headless"
		metricsOfXHeadless := reconciler.RoleGroupResourceName(cr.Name, roleName, "x-headless")
		if len(reconciler.RoleGroupResourceName(cr.Name, roleName, "x")) != 54 {
			t.Fatal("test setup: group x must be exactly at the canonical-name boundary")
		}
		if headlessOfX == metricsOfXHeadless {
			t.Fatalf("test setup: canonical hashing did not separate %q", headlessOfX)
		}

		if err := validateRoleGroupServiceSlots(cr); err != nil {
			t.Fatalf("canonical hashed names are distinct and must not be rejected: %v", err)
		}
	})
}
