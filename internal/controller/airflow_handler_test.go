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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("AirflowCluster reconciliation", func() {
	const (
		clusterName      = "airflow-test"
		noSecretCluster  = "airflow-nosecret"
		namespace        = "default"
		defaultGroupName = "default"
	)

	var r *reconciler.GenericReconciler[*airflowv1alpha1.AirflowCluster]

	newCluster := func() *airflowv1alpha1.AirflowCluster {
		one := int32(1)
		return &airflowv1alpha1.AirflowCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: airflowv1alpha1.AirflowClusterSpec{
				ClusterConfig: &airflowv1alpha1.ClusterConfigSpec{
					Credentials: "test-credentials",
				},
				Webservers: &airflowv1alpha1.WebserversSpec{
					RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{
						defaultGroupName: {Replicas: &one},
					},
				},
				Schedulers: &airflowv1alpha1.SchedulersSpec{
					RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{
						defaultGroupName: {Replicas: &one},
					},
				},
				CeleryExecutors: &airflowv1alpha1.CeleryExecutorsSpec{
					RoleGroups: map[string]airflowv1alpha1.RoleGroupSpec{
						defaultGroupName: {Replicas: &one},
					},
				},
			},
		}
	}

	newReconciler := func() *reconciler.GenericReconciler[*airflowv1alpha1.AirflowCluster] {
		rec, err := reconciler.NewGenericReconciler(
			&reconciler.GenericReconcilerConfig[*airflowv1alpha1.AirflowCluster]{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				Recorder:         record.NewFakeRecorder(1000),
				RoleGroupHandler: NewAirflowRoleGroupHandler(k8sClient.Scheme()),
				ProductConfig:    ComputeProductConfig,
				Prototype:        &airflowv1alpha1.AirflowCluster{},
				Dependencies: func(cr *airflowv1alpha1.AirflowCluster) []reconciler.Dependency {
					if cr.Spec.ClusterConfig == nil || cr.Spec.ClusterConfig.Credentials == "" {
						return nil
					}
					return []reconciler.Dependency{
						{Kind: reconciler.DependencySecret, Name: cr.Spec.ClusterConfig.Credentials},
					}
				},
			})
		Expect(err).NotTo(HaveOccurred())
		return rec
	}

	reconcileCluster := func(name string) {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	fetchSTS := func(name string) *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, sts)).To(Succeed())
		return sts
	}

	findContainerIn := func(sts *appsv1.StatefulSet, name string) *corev1.Container {
		for i := range sts.Spec.Template.Spec.Containers {
			if sts.Spec.Template.Spec.Containers[i].Name == name {
				return &sts.Spec.Template.Spec.Containers[i]
			}
		}
		return nil
	}

	findInitContainerIn := func(sts *appsv1.StatefulSet, name string) *corev1.Container {
		for i := range sts.Spec.Template.Spec.InitContainers {
			if sts.Spec.Template.Spec.InitContainers[i].Name == name {
				return &sts.Spec.Template.Spec.InitContainers[i]
			}
		}
		return nil
	}

	envNames := func(c *corev1.Container) []string {
		names := make([]string, 0, len(c.Env))
		for _, e := range c.Env {
			names = append(names, e.Name)
		}
		return names
	}

	BeforeEach(func() {
		r = newReconciler()
		// The credentials secret the dependency check and the env fan-out reference.
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "test-credentials", Namespace: namespace},
			StringData: map[string]string{
				"appSecretKey":                      "secret",
				"connections.sqlalchemyDatabaseUri": "postgresql://airflow@postgres:5432/airflow",
				"connections.celeryResultBackend":   "db+postgresql://airflow@postgres:5432/airflow",
				"connections.celeryBrokerUrl":       "redis://redis:6379/0",
			},
		}
		err := k8sClient.Create(ctx, secret)
		Expect(client.IgnoreAlreadyExists(err)).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &airflowv1alpha1.AirflowCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		})
	})

	It("builds the three role groups with product containers, scripts and env", func() {
		Expect(k8sClient.Create(ctx, newCluster())).To(Succeed())
		reconcileCluster(clusterName)

		// Webserver: role-named main container with the webserver script, a TCP readiness probe
		// on the http port (framework Ports[0] contract), the metrics sidecar, and the
		// credentials env fan-out.
		web := fetchSTS(clusterName + "-webservers-default")
		webMain := findContainerIn(web, "webservers")
		Expect(webMain).NotTo(BeNil())
		Expect(webMain.Command).To(Equal([]string{"/bin/bash", "-x", "-euo", "pipefail", "-c"}))
		Expect(strings.Join(webMain.Args, " ")).To(ContainSubstring("airflow webserver &"))
		Expect(webMain.ReadinessProbe).NotTo(BeNil())
		Expect(webMain.Env).To(ContainElement(HaveField("Name", "AIRFLOW__CORE__EXECUTOR")))
		Expect(envNames(webMain)).To(ContainElements(
			"AIRFLOW__DATABASE__SQL_ALCHEMY_CONN",
			"AIRFLOW__WEBSERVER__SECRET_KEY",
			"AIRFLOW__CELERY__BROKER_URL",
			"PYTHONPATH",
		))
		// The admin bootstrap env is scheduler-only.
		Expect(envNames(webMain)).NotTo(ContainElement("ADMIN_USERNAME"))
		// The metrics sidecar is a native sidecar (init container with RestartPolicy Always).
		metric := findInitContainerIn(web, "metric")
		Expect(metric).NotTo(BeNil())
		Expect(metric.RestartPolicy).NotTo(BeNil())
		Expect(*metric.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
		Expect(findContainerIn(web, "metric")).To(BeNil())
		Expect(findContainerIn(web, "vector")).To(BeNil())

		// Scheduler: db init + admin user creation in the startup script, admin env present.
		sched := fetchSTS(clusterName + "-schedulers-default")
		schedMain := findContainerIn(sched, "schedulers")
		Expect(schedMain).NotTo(BeNil())
		script := strings.Join(schedMain.Args, " ")
		Expect(script).To(ContainSubstring("airflow db init"))
		Expect(script).To(ContainSubstring("airflow users create"))
		Expect(envNames(schedMain)).To(ContainElement("ADMIN_USERNAME"))
		// Schedulers declare no ports: no generated probe.
		Expect(schedMain.ReadinessProbe).To(BeNil())

		// Celery executor.
		celery := fetchSTS(clusterName + "-celeryexecutors-default")
		celeryMain := findContainerIn(celery, "celeryexecutors")
		Expect(celeryMain).NotTo(BeNil())
		Expect(strings.Join(celeryMain.Args, " ")).To(ContainSubstring("airflow celery worker &"))

		// The shared log volume is mounted on the main container.
		Expect(webMain.VolumeMounts).To(ContainElement(HaveField("Name", "log")))
	})

	It("renders webserver_config.py and log_config.py into the role group ConfigMap", func() {
		Expect(k8sClient.Create(ctx, newCluster())).To(Succeed())
		reconcileCluster(clusterName)

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: clusterName + "-webservers-default",
		}, cm)).To(Succeed())

		Expect(cm.Data).To(HaveKey("webserver_config.py"))
		Expect(cm.Data).To(HaveKey("log_config.py"))
		// No AuthenticationClasses configured: the file carries the FAB boilerplate only (parity
		// with the pre-framework operator — FAB's own default is AUTH_DB).
		Expect(cm.Data["webserver_config.py"]).To(ContainSubstring("WTF_CSRF_ENABLED = False"))
		Expect(cm.Data["webserver_config.py"]).NotTo(ContainSubstring("AUTH_TYPE"))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring("LOGGING_CONFIG"))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring("/kubedoop/log/webservers/airflow.log.json"))

		// With an (empty) Authentication object, FAB falls back to AUTH_DB.
		authCfg, err := renderWebserverConfig(&Authentication{authenticators: map[AuthenticatorType][]Authenticator{}})
		Expect(err).NotTo(HaveOccurred())
		Expect(authCfg).To(ContainSubstring("AUTH_TYPE = 'AUTH_DB'"))
	})

	It("publishes the metrics Service under the role group name with Prometheus annotations", func() {
		Expect(k8sClient.Create(ctx, newCluster())).To(Succeed())
		reconcileCluster(clusterName)

		for _, role := range []string{"webservers", "schedulers", "celeryexecutors"} {
			name := clusterName + "-" + role + "-default"
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, svc)).To(Succeed())
			Expect(svc.Annotations).To(HaveKeyWithValue("prometheus.io/scrape", "true"))
			Expect(svc.Annotations).To(HaveKeyWithValue("prometheus.io/port", "9102"))
			Expect(svc.Annotations).To(HaveKeyWithValue("prometheus.io/path", "/metrics"))
			Expect(svc.Annotations).To(HaveKeyWithValue("prometheus.io/scheme", "http"))
			Expect(svc.Labels).To(HaveKeyWithValue(reconciler.LabelMetricsService, "true"))
			// Identity labels the framework stamps make the custom-named slot reclaimable.
			Expect(svc.Labels).To(HaveKey("app.kubernetes.io/instance"))
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(9102)))
		}

		// The headless Service exists under the framework's naming.
		headless := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: clusterName + "-webservers-default-headless",
		}, headless)).To(Succeed())
	})

	It("forces replicas to 0 when the cluster is stopped, keeping the resources", func() {
		cr := newCluster()
		cr.Spec.ClusterOperation = &commonsv1alpha1.ClusterOperationSpec{Stopped: true}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		reconcileCluster(clusterName)

		web := fetchSTS(clusterName + "-webservers-default")
		Expect(web.Spec.Replicas).NotTo(BeNil())
		Expect(*web.Spec.Replicas).To(Equal(int32(0)))
	})

	It("degrades the cluster when the credentials secret is missing", func() {
		cr := newCluster()
		cr.Name = noSecretCluster
		cr.Spec.ClusterConfig.Credentials = "absent-credentials"
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: namespace, Name: noSecretCluster,
		}})
		Expect(err).To(HaveOccurred())

		fetched := &airflowv1alpha1.AirflowCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: noSecretCluster}, fetched)).To(Succeed())
		degraded := apimeta.FindStatusCondition(fetched.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))

		_ = k8sClient.Delete(ctx, cr)
	})
})
