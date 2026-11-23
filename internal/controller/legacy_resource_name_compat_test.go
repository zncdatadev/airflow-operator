package controller

import (
	"context"
	"strings"
	"testing"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLegacyRoleGroupResourceNamePreflight(t *testing.T) {
	t.Parallel()

	const (
		namespace = "default"
		roleName  = string(airflowv1alpha1.WebserversRoleName)
		groupName = "default"
	)

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register core scheme: %v", err)
	}
	if err := policyv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register policy scheme: %v", err)
	}
	if err := airflowv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register Airflow scheme: %v", err)
	}

	newCluster := func(name string, uid types.UID) *airflowv1alpha1.AirflowCluster {
		return &airflowv1alpha1.AirflowCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: uid},
			Spec: airflowv1alpha1.AirflowClusterSpec{
				ClusterConfig: &airflowv1alpha1.ClusterConfigSpec{Credentials: fixtureCredentialsSecret},
				Webservers: &airflowv1alpha1.WebserversSpec{
					RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{groupName: {}},
				},
			},
		}
	}

	declare := func(t *testing.T, cr *airflowv1alpha1.AirflowCluster, objects ...client.Object) error {
		t.Helper()
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
		handler := NewAirflowRoleGroupHandler(scheme)
		_, err := handler.DeclareRoles(context.Background(), k8sClient, cr)
		return err
	}

	legacyName := func(cr *airflowv1alpha1.AirflowCluster) string {
		return cr.Name + "-" + roleName + "-" + groupName
	}

	controlledObject := func(
		kind string,
		name string,
		owner *airflowv1alpha1.AirflowCluster,
	) client.Object {
		meta := metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(owner, airflowv1alpha1.GroupVersion.WithKind("AirflowCluster")),
			},
		}
		switch kind {
		case legacyStatefulSetKind:
			return &appsv1.StatefulSet{ObjectMeta: meta}
		case legacyConfigMapKind:
			return &corev1.ConfigMap{ObjectMeta: meta}
		case legacyServiceKind:
			return &corev1.Service{ObjectMeta: meta}
		default:
			t.Fatalf("unsupported test kind %q", kind)
			return nil
		}
	}

	t.Run("54-character legacy name remains canonical", func(t *testing.T) {
		cr := newCluster(strings.Repeat("a", 35), "at-boundary")
		rawName := legacyName(cr)
		if len(rawName) != 54 {
			t.Fatalf("test setup: legacy name length = %d, want 54", len(rawName))
		}
		if canonical := reconciler.RoleGroupResourceName(cr.Name, roleName, groupName); canonical != rawName {
			t.Fatalf("54-character name unexpectedly changed: got %q, want %q", canonical, rawName)
		}

		legacy := controlledObject(legacyStatefulSetKind, rawName, cr)
		if err := declare(t, cr, legacy); err != nil {
			t.Fatalf("DeclareRoles rejected the 54-character compatibility boundary: %v", err)
		}
	})

	for _, kind := range []string{legacyStatefulSetKind, legacyConfigMapKind, legacyServiceKind} {
		t.Run("55-character owned legacy "+kind+" is rejected", func(t *testing.T) {
			cr := newCluster(strings.Repeat("b", 36), "needs-migration")
			rawName := legacyName(cr)
			if len(rawName) != 55 {
				t.Fatalf("test setup: legacy name length = %d, want 55", len(rawName))
			}
			canonical := reconciler.RoleGroupResourceName(cr.Name, roleName, groupName)
			if canonical == rawName {
				t.Fatalf("test setup: 55-character legacy name was not bounded")
			}

			err := declare(t, cr, controlledObject(kind, rawName, cr))
			if err == nil {
				t.Fatal("DeclareRoles accepted an owned legacy resource under the non-canonical name")
			}
			for _, want := range []string{kind, rawName, canonical, "second workload"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}

	t.Run("fresh 55-character name uses the bounded Gen3 slot", func(t *testing.T) {
		cr := newCluster(strings.Repeat("c", 36), "fresh")
		rawName := legacyName(cr)
		if canonical := reconciler.RoleGroupResourceName(cr.Name, roleName, groupName); canonical == rawName {
			t.Fatalf("test setup: 55-character legacy name was not bounded")
		}
		if err := declare(t, cr); err != nil {
			t.Fatalf("DeclareRoles rejected a fresh long-name cluster: %v", err)
		}
	})

	t.Run("non-owned 55-character object does not block the cluster", func(t *testing.T) {
		cr := newCluster(strings.Repeat("d", 36), "current-owner")
		other := newCluster(cr.Name, "different-owner")
		foreign := controlledObject(legacyStatefulSetKind, legacyName(cr), other)
		if err := declare(t, cr, foreign); err != nil {
			t.Fatalf("DeclareRoles rejected an object controlled by another owner: %v", err)
		}
	})
}
