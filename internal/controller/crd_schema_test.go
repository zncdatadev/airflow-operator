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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("AirflowCluster CRD admission", func() {
	const (
		namespace            = "default"
		invalidRoleGroupName = "Bad_Name"
	)

	newCluster := func(name string) *airflowv1alpha1.AirflowCluster {
		return &airflowv1alpha1.AirflowCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		}
	}

	It("rejects an unsupported image pull policy", func() {
		cr := newCluster("invalid-pull-policy")
		cr.Spec.Image = &airflowv1alpha1.ImageSpec{PullPolicy: corev1.PullPolicy("Sometimes")}

		err := k8sClient.Create(ctx, cr)
		Expect(err).To(MatchError(ContainSubstring("spec.image.pullPolicy")))
	})

	It("rejects a negative role-group replica count", func() {
		cr := newCluster("negative-replicas")
		negative := int32(-1)
		cr.Spec.Webservers = &airflowv1alpha1.WebserversSpec{
			RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{
				"default": {Replicas: &negative},
			},
		}

		err := k8sClient.Create(ctx, cr)
		Expect(err).To(MatchError(ContainSubstring("spec.webservers.roleGroups.default.replicas")))
	})

	It("rejects an empty authentication class reference", func() {
		cr := newCluster("empty-authentication-class")
		cr.Spec.ClusterConfig = &airflowv1alpha1.ClusterConfigSpec{
			Credentials:    fixtureCredentialsSecret,
			Authentication: []airflowv1alpha1.AuthenticationSpec{{}},
		}

		err := k8sClient.Create(ctx, cr)
		Expect(err).To(MatchError(ContainSubstring(
			"spec.clusterConfig.authentication[0].authenticationClass",
		)))
	})

	It("rejects multiple authentication entries", func() {
		cr := newCluster("multiple-authentication-entries")
		cr.Spec.ClusterConfig = &airflowv1alpha1.ClusterConfigSpec{
			Credentials: fixtureCredentialsSecret,
			Authentication: []airflowv1alpha1.AuthenticationSpec{
				{AuthenticationClass: "first"},
				{AuthenticationClass: "second"},
			},
		}

		err := k8sClient.Create(ctx, cr)
		Expect(err).To(MatchError(And(
			ContainSubstring("spec.clusterConfig.authentication"),
			ContainSubstring("must have at most 1 item"),
		)))
	})

	It("rejects the unimplemented Kubernetes executor field", func() {
		cr := newCluster("unsupported-kubernetes-executor")
		cr.Spec.KubernetesExecutors = &airflowv1alpha1.KubernetesExecutorsSpec{}

		err := k8sClient.Create(ctx, cr)
		Expect(err).To(MatchError(ContainSubstring(
			"spec.kubernetesExecutors is unsupported because this operator only implements CeleryExecutor; remove the field",
		)))
	})

	DescribeTable("rejects non-RFC1123 role-group names on every role",
		func(name string, configure func(*airflowv1alpha1.AirflowCluster)) {
			cr := newCluster(name)
			configure(cr)

			err := k8sClient.Create(ctx, cr)
			Expect(err).To(MatchError(ContainSubstring(
				"each role group name must be a lowercase RFC 1123 label",
			)))
		},
		Entry("webservers", "invalid-web-group", func(cr *airflowv1alpha1.AirflowCluster) {
			cr.Spec.Webservers = &airflowv1alpha1.WebserversSpec{
				RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{invalidRoleGroupName: {}},
			}
		}),
		Entry("schedulers", "invalid-scheduler-group", func(cr *airflowv1alpha1.AirflowCluster) {
			cr.Spec.Schedulers = &airflowv1alpha1.SchedulersSpec{
				RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{invalidRoleGroupName: {}},
			}
		}),
		Entry("celery executors", "invalid-celery-group", func(cr *airflowv1alpha1.AirflowCluster) {
			cr.Spec.CeleryExecutors = &airflowv1alpha1.CeleryExecutorsSpec{
				RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{invalidRoleGroupName: {}},
			}
		}),
	)

	It("rejects a role-group name longer than 63 characters", func() {
		cr := newCluster("overlong-role-group")
		cr.Spec.Webservers = &airflowv1alpha1.WebserversSpec{
			RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{
				strings.Repeat("a", 64): {},
			},
		}

		err := k8sClient.Create(ctx, cr)
		Expect(err).To(MatchError(ContainSubstring(
			"each role group name must be a lowercase RFC 1123 label",
		)))
	})

	It("rejects more than 256 role groups", func() {
		cr := newCluster("too-many-role-groups")
		groups := make(map[string]airflowv1alpha1.RoleGroupSpec, 257)
		for index := range 257 {
			groups[fmt.Sprintf("group-%03d", index)] = airflowv1alpha1.RoleGroupSpec{}
		}
		cr.Spec.Webservers = &airflowv1alpha1.WebserversSpec{RoleGroups: groups}

		err := k8sClient.Create(ctx, cr)
		Expect(err).To(MatchError(And(
			ContainSubstring("spec.webservers.roleGroups"),
			ContainSubstring("must have at most 256 items"),
		)))
	})
})
