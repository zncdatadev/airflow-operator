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
	"maps"
	"strings"
	"testing"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/constant"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLegacyResourceInventoryPreflight(t *testing.T) {
	t.Parallel()

	const (
		namespace     = "default"
		clusterName   = "airflow"
		webserverRole = string(airflowv1alpha1.WebserversRoleName)
		defaultGroup  = "default"
		removedGroup  = "removed"
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

	newCluster := func(uid types.UID) *airflowv1alpha1.AirflowCluster {
		return &airflowv1alpha1.AirflowCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace, UID: uid},
			Spec: airflowv1alpha1.AirflowClusterSpec{
				ClusterConfig: &airflowv1alpha1.ClusterConfigSpec{Credentials: fixtureCredentialsSecret},
				Webservers: &airflowv1alpha1.WebserversSpec{
					RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{defaultGroup: {}},
				},
			},
		}
	}

	legacyMeta := func(
		name, roleName, groupName string,
		owner *airflowv1alpha1.AirflowCluster,
		controller bool,
	) metav1.ObjectMeta {
		labels := map[string]string{
			constant.LabelKubernetesInstance:  clusterName,
			constant.LabelKubernetesName:      legacyAppName,
			constant.LabelKubernetesManagedBy: LabelDomain,
			constant.LabelKubernetesComponent: roleName,
		}
		if groupName != "" {
			labels[constant.LabelKubernetesRoleGroup] = groupName
		}
		ownerReference := *metav1.NewControllerRef(
			owner,
			airflowv1alpha1.GroupVersion.WithKind("AirflowCluster"),
		)
		ownerReference.Controller = ptr.To(controller)
		return metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerReference},
		}
	}

	roleGroupObject := func(
		kind, groupName string,
		owner *airflowv1alpha1.AirflowCluster,
		controller bool,
	) client.Object {
		name := strings.Join([]string{clusterName, webserverRole, groupName}, "-")
		meta := legacyMeta(name, webserverRole, groupName, owner, controller)
		switch kind {
		case legacyStatefulSetKind:
			return &appsv1.StatefulSet{ObjectMeta: meta}
		case legacyConfigMapKind:
			return &corev1.ConfigMap{ObjectMeta: meta}
		case legacyServiceKind:
			return &corev1.Service{ObjectMeta: meta}
		default:
			t.Fatalf("unsupported role-group resource kind %q", kind)
			return nil
		}
	}

	rolePDB := func(roleName string) *policyv1.PodDisruptionBudget {
		labels := map[string]string{
			constant.LabelKubernetesInstance:  clusterName,
			constant.LabelKubernetesName:      legacyAppName,
			constant.LabelKubernetesManagedBy: LabelDomain,
			constant.LabelKubernetesComponent: roleName,
		}
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      strings.Join([]string{clusterName, roleName}, "-"),
				Namespace: namespace,
				Labels:    maps.Clone(labels),
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: maps.Clone(labels)},
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

	t.Run("owned expected role-group slots are accepted", func(t *testing.T) {
		cr := newCluster("current")
		objects := []client.Object{
			roleGroupObject(legacyStatefulSetKind, defaultGroup, cr, true),
			roleGroupObject(legacyConfigMapKind, defaultGroup, cr, true),
			roleGroupObject(legacyServiceKind, defaultGroup, cr, true),
		}
		if err := declare(t, cr, objects...); err != nil {
			t.Fatalf("DeclareRoles rejected expected Gen 2 fixed resources: %v", err)
		}
	})

	for _, kind := range []string{legacyStatefulSetKind, legacyConfigMapKind, legacyServiceKind} {
		t.Run("owned stale "+kind+" is rejected", func(t *testing.T) {
			cr := newCluster("current")
			stale := roleGroupObject(kind, removedGroup, cr, true)

			err := declare(t, cr, stale)
			if err == nil {
				t.Fatalf("DeclareRoles accepted stale Gen 2 %s", kind)
			}
			for _, want := range []string{
				kind,
				stale.GetName(),
				`group "removed" is absent`,
				"two-phase upgrade",
				"manually clean up",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}

	t.Run("foreign controller owner is ignored", func(t *testing.T) {
		cr := newCluster("current")
		foreignOwner := newCluster("foreign")
		foreign := roleGroupObject(legacyStatefulSetKind, removedGroup, foreignOwner, true)

		if err := declare(t, cr, foreign); err != nil {
			t.Fatalf("DeclareRoles rejected a resource controlled by another UID: %v", err)
		}
	})

	t.Run("non-controller owner reference is ignored", func(t *testing.T) {
		cr := newCluster("current")
		nonController := roleGroupObject(legacyStatefulSetKind, removedGroup, cr, false)

		if err := declare(t, cr, nonController); err != nil {
			t.Fatalf("DeclareRoles rejected a non-controller owner reference: %v", err)
		}
	})

	t.Run("enabled expected role PDB is accepted", func(t *testing.T) {
		cr := newCluster("current")
		cr.Spec.Webservers.RoleConfig = &commonsv1alpha1.RoleConfigSpec{
			PodDisruptionBudget: &commonsv1alpha1.PodDisruptionBudgetSpec{Enabled: ptr.To(true)},
		}
		if err := declare(t, cr, rolePDB(webserverRole)); err != nil {
			t.Fatalf("DeclareRoles rejected the enabled Gen 2 role PDB: %v", err)
		}
	})

	t.Run("PDB disabled in the same upgrade is rejected", func(t *testing.T) {
		cr := newCluster("current")
		cr.Spec.Webservers.RoleConfig = &commonsv1alpha1.RoleConfigSpec{
			PodDisruptionBudget: &commonsv1alpha1.PodDisruptionBudgetSpec{Enabled: ptr.To(false)},
		}
		pdb := rolePDB(webserverRole)

		err := declare(t, cr, pdb)
		if err == nil {
			t.Fatal("DeclareRoles accepted a legacy PDB disabled in the upgrade spec")
		}
		for _, want := range []string{
			legacyPodDisruptionBudgetKind,
			pdb.Name,
			"is absent or its roleConfig.podDisruptionBudget is not enabled",
			"two-phase upgrade",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})

	t.Run("PDB for a removed role is rejected", func(t *testing.T) {
		cr := newCluster("current")
		cr.Spec.Webservers = nil
		pdb := rolePDB(webserverRole)

		err := declare(t, cr, pdb)
		if err == nil {
			t.Fatal("DeclareRoles accepted a Gen 2 PDB for a removed role")
		}
		for _, want := range []string{legacyPodDisruptionBudgetKind, pdb.Name, `role "webservers" is absent`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})

	t.Run("same-owner rolled-back PDB for a removed role is rejected", func(t *testing.T) {
		cr := newCluster("current")
		cr.Spec.Webservers = nil
		pdb := rolePDB(webserverRole)
		pdb.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(
				cr,
				airflowv1alpha1.GroupVersion.WithKind("AirflowCluster"),
			),
		}

		err := declare(t, cr, pdb)
		if err == nil {
			t.Fatal("DeclareRoles accepted a same-owner rolled-back PDB for a removed role")
		}
		for _, want := range []string{legacyPodDisruptionBudgetKind, pdb.Name, `role "webservers" is absent`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})

	t.Run("foreign-controlled PDB is ignored", func(t *testing.T) {
		cr := newCluster("current")
		foreignOwner := newCluster("foreign")
		pdb := rolePDB(webserverRole)
		pdb.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(
				foreignOwner,
				airflowv1alpha1.GroupVersion.WithKind("AirflowCluster"),
			),
		}

		if err := declare(t, cr, pdb); err != nil {
			t.Fatalf("DeclareRoles rejected a PDB controlled by another UID: %v", err)
		}
	})

	t.Run("ownerless PDB with a non-Gen2 selector is ignored", func(t *testing.T) {
		cr := newCluster("current")
		pdb := rolePDB(webserverRole)
		pdb.Spec.Selector.MatchLabels[constant.LabelKubernetesManagedBy] = "someone-else"

		if err := declare(t, cr, pdb); err != nil {
			t.Fatalf("DeclareRoles rejected an ownerless PDB without the exact Gen 2 fingerprint: %v", err)
		}
	})
}
