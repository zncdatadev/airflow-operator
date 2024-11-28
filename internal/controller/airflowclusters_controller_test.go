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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
)

var _ = Describe("AirflowCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		airflowcluster := &airflowv1alpha1.AirflowCluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind AirflowCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, airflowcluster)
			if err != nil && errors.IsNotFound(err) {
				resource := &airflowv1alpha1.AirflowCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: airflowv1alpha1.AirflowClusterSpec{
						ClusterConfig: &airflowv1alpha1.ClusterConfigSpec{
							Credentials: "test-credentials",
						},
						Webservers: &airflowv1alpha1.WebserversSpec{
							RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{
								"default": {
									Replicas: ptr.To[int32](1),
								},
							},
						},
						CeleryExecutors: &airflowv1alpha1.CeleryExecutorsSpec{
							RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{
								"default": {
									Replicas: ptr.To[int32](1),
								},
							},
						},
						Schedulers: &airflowv1alpha1.SchedulersSpec{
							RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{
								"default": {
									Replicas: ptr.To[int32](1),
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &airflowv1alpha1.AirflowCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance AirflowCluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &AirflowClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
