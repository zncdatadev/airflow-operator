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
	"maps"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/airflow-operator/internal/util/version"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/constant"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("AirflowCluster reconciliation", func() {
	const (
		clusterName         = "airflow-test"
		noSecretCluster     = "airflow-nosecret"
		namespace           = "default"
		defaultGroupName    = "default"
		webserverRole       = string(airflowv1alpha1.WebserversRoleName)
		schedulerRole       = string(airflowv1alpha1.SchedulersRoleName)
		celeryRole          = string(airflowv1alpha1.CeleryExecutorsRoleName)
		legacyOverrideValue = "kept"
		vectorAddressKey    = "ADDRESS"
		vectorAddress       = "vector-aggregator.default.svc:6123"
		legacyShell         = "/bin/sh"
		legacyCustomStart   = "exec /opt/airflow/custom-start"
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
		handler := NewAirflowRoleGroupHandler(k8sClient.Scheme())
		rec, err := reconciler.NewGenericReconciler(
			&reconciler.GenericReconcilerConfig[*airflowv1alpha1.AirflowCluster]{
				Client:           k8sClient,
				APIReader:        k8sClient,
				Scheme:           k8sClient.Scheme(),
				Recorder:         record.NewFakeRecorder(1000),
				RoleGroupHandler: handler,
				RoleProvider:     handler,
				RoleGroupResolver: reconciler.RoleGroupResolverFunc[*airflowv1alpha1.AirflowCluster](
					ComputeProductConfig,
				),
				ImageResolution: reconciler.ImageResolution{
					ProductName: airflowv1alpha1.DefaultProductName,
					Defaults:    ImageDefaults(),
				},
				Prototype: &airflowv1alpha1.AirflowCluster{},
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
		previousBuildVersion := version.BuildVersion
		version.BuildVersion = "0.0.0-test"
		DeferCleanup(func() { version.BuildVersion = previousBuildVersion })
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
		web := fetchSTS(clusterName + "-" + webserverRole + "-default")
		webMain := findContainerIn(web, webserverRole)
		Expect(webMain).NotTo(BeNil())
		Expect(webMain.Command[:5]).To(Equal([]string{"/bin/bash", "-x", "-euo", "pipefail", "-c"}))
		Expect(strings.Join(webMain.Command, " ")).To(ContainSubstring(`airflow webserver "$@" &`))
		Expect(webMain.Command[len(webMain.Command)-1]).To(Equal(webserverRole))
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
		Expect(metric.SecurityContext).NotTo(BeNil())
		Expect(metric.Ports).To(ContainElement(HaveField("Name", MetricsPortName)))
		Expect(findContainerIn(web, metricsSidecarName)).To(BeNil())
		Expect(findContainerIn(web, "vector")).To(BeNil())

		// Scheduler: db init + admin user creation in the startup script, admin env present.
		sched := fetchSTS(clusterName + "-schedulers-default")
		schedMain := findContainerIn(sched, "schedulers")
		Expect(schedMain).NotTo(BeNil())
		script := strings.Join(schedMain.Command, " ")
		Expect(script).To(ContainSubstring("airflow db init"))
		Expect(script).To(ContainSubstring("airflow users create"))
		Expect(script).To(ContainSubstring("airflow users list -o json"))
		Expect(envNames(schedMain)).To(ContainElement("ADMIN_USERNAME"))
		// Schedulers declare no ports: no generated probe.
		Expect(schedMain.ReadinessProbe).To(BeNil())

		// Celery executor.
		celery := fetchSTS(clusterName + "-celeryexecutors-default")
		celeryMain := findContainerIn(celery, "celeryexecutors")
		Expect(celeryMain).NotTo(BeNil())
		Expect(strings.Join(celeryMain.Command, " ")).To(ContainSubstring(`airflow celery worker "$@" &`))

		// Gen 2 always exposed the bounded log volume. Vector controls collection, not whether
		// product and user-defined containers can rely on that pod contract.
		Expect(webMain.VolumeMounts).To(ContainElement(And(
			HaveField("Name", LogVolumeName),
			HaveField("MountPath", constant.KubedoopLogDir),
		)))
		var logVolume *corev1.Volume
		for i := range web.Spec.Template.Spec.Volumes {
			if web.Spec.Template.Spec.Volumes[i].Name == LogVolumeName {
				logVolume = &web.Spec.Template.Spec.Volumes[i]
			}
		}
		Expect(logVolume).NotTo(BeNil())
		Expect(logVolume.EmptyDir).NotTo(BeNil())
		Expect(logVolume.EmptyDir.SizeLimit).NotTo(BeNil())
		Expect(logVolume.EmptyDir.SizeLimit.String()).To(Equal("500Mi"))
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
		Expect(cm.Data["log_config.py"]).To(ContainSubstring("'handlers': ['console', 'file']"))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring("RotatingFileHandler"))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring(
			"/kubedoop/log/webservers/airflow.py.json",
		))

		// With an (empty) Authentication object, FAB falls back to AUTH_DB.
		authCfg, err := renderWebserverConfig(&Authentication{authenticators: map[AuthenticatorType][]Authenticator{}})
		Expect(err).NotTo(HaveOccurred())
		Expect(authCfg).To(ContainSubstring("AUTH_TYPE = AUTH_DB"))
	})

	It("preserves configOverrides for files that are not product-owned", func() {
		const configOverrideCluster = "airflow-config-override"
		cr := newCluster()
		cr.Name = configOverrideCluster
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			ConfigOverrides: map[string]map[string]string{
				"airflow.cfg": {"core.executor": "LocalExecutor"},
			},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

		reconcileCluster(configOverrideCluster)

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: configOverrideCluster + "-webservers-default",
		}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey("airflow.cfg"))
		Expect(cm.Data["airflow.cfg"]).To(ContainSubstring("core.executor=LocalExecutor"))
		Expect(cm.Data).To(HaveKey("webserver_config.py"))
		Expect(cm.Data).To(HaveKey("log_config.py"))
	})

	DescribeTable("rejects configOverrides that collide with product-owned files",
		func(clusterSuffix, filename string) {
			cr := newCluster()
			cr.Name = "airflow-config-collision-" + clusterSuffix
			cr.Spec.Schedulers = nil
			cr.Spec.CeleryExecutors = nil
			webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
			webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
				ConfigOverrides: map[string]map[string]string{
					filename: {"user.key": "must-not-be-overwritten"},
				},
			}
			cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: namespace, Name: cr.Name,
			}})
			Expect(err).To(MatchError(ContainSubstring(
				"configOverrides for product-owned file \"" + filename + "\" are unsupported",
			)))
		},
		Entry("webserver config", "webserver", "webserver_config.py"),
		Entry("logging config", "logging", "log_config.py"),
	)

	It("publishes stable role-group Services with the web client and metrics ports", func() {
		Expect(k8sClient.Create(ctx, newCluster())).To(Succeed())
		reconcileCluster(clusterName)

		for _, role := range []string{webserverRole, schedulerRole, celeryRole} {
			name := clusterName + "-" + role + "-default"
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, svc)).To(Succeed())
			Expect(svc.Annotations).To(HaveKeyWithValue("prometheus.io/scrape", "true"))
			Expect(svc.Annotations).To(HaveKeyWithValue("prometheus.io/port", "9102"))
			Expect(svc.Annotations).To(HaveKeyWithValue("prometheus.io/path", "/metrics"))
			Expect(svc.Annotations).To(HaveKeyWithValue("prometheus.io/scheme", HTTPProtocol))
			Expect(svc.Labels).To(HaveKeyWithValue("prometheus.io/scrape", "true"))
			// Identity labels the framework stamps make the fixed ordinary slot reclaimable.
			Expect(svc.Labels).To(HaveKey("app.kubernetes.io/instance"))
			expectedPorts := []corev1.ServicePort{{
				Name: MetricsPortName, Port: MetricsPort,
				TargetPort: intstr.FromInt32(MetricsPort), Protocol: corev1.ProtocolTCP,
			}}
			if role == webserverRole {
				expectedPorts = append([]corev1.ServicePort{{
					Name: HTTPProtocol, Port: WebserverPort,
					TargetPort: intstr.FromString(HTTPProtocol), Protocol: corev1.ProtocolTCP,
				}}, expectedPorts...)
			}
			Expect(svc.Spec.Ports).To(Equal(expectedPorts))
		}

		// The headless Service exists under the framework's naming.
		headless := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: clusterName + "-webservers-default-headless",
		}, headless)).To(Succeed())
	})

	It("keeps the Gen 2 Vector default and canonical log mounts across pod overrides", func() {
		aggregator := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "vector-aggregator", Namespace: namespace},
			Data:       map[string]string{vectorAddressKey: vectorAddress},
		}
		Expect(k8sClient.Create(ctx, aggregator)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, aggregator) })

		cr := newCluster()
		cr.Spec.ClusterConfig.VectorAggregatorConfigMapName = aggregator.Name
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.Config = &airflowv1alpha1.ConfigSpec{RoleGroupConfigSpec: &commonsv1alpha1.RoleGroupConfigSpec{
			Logging: &commonsv1alpha1.LoggingSpec{
				Containers: map[string]commonsv1alpha1.LoggingConfigSpec{
					webserverRole: {
						Console: &commonsv1alpha1.LogLevelSpec{Level: "DEBUG"},
						File:    &commonsv1alpha1.LogLevelSpec{Level: "INFO"},
					},
				},
			},
		}}
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
				"spec": {
					"volumes": [{"name": "log", "emptyDir": {"medium": "Memory", "sizeLimit": "96Mi"}}],
					"containers": [{"name": "webservers", "volumeMounts": [{
						"name": "log", "mountPath": "/kubedoop/log/", "readOnly": true
					}]}]
				}
			}`)},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		alternateGroup := airflowv1alpha1.RoleGroupSpec{
			Replicas: webGroup.Replicas,
			OverridesSpec: &commonsv1alpha1.OverridesSpec{
				PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
						"spec": {"containers": [{"name": "webservers", "volumeMounts": [{
							"name": "log", "mountPath": "/custom/log", "readOnly": true
						}]}]}
					}`)},
			},
		}
		cr.Spec.Webservers.RoleGroups["alternate"] = alternateGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		reconcileCluster(clusterName)

		web := fetchSTS(clusterName + "-" + webserverRole + "-default")
		Expect(findInitContainerIn(web, "vector")).NotTo(BeNil())
		webMain := findContainerIn(web, webserverRole)
		var vectorMainLogMounts []corev1.VolumeMount
		for _, mount := range webMain.VolumeMounts {
			if mount.Name == LogVolumeName {
				vectorMainLogMounts = append(vectorMainLogMounts, mount)
			}
		}
		Expect(vectorMainLogMounts).To(Equal([]corev1.VolumeMount{{
			Name: LogVolumeName, MountPath: constant.KubedoopLogDir, ReadOnly: true,
		}}))
		var vectorLogVolumes []corev1.Volume
		for _, volume := range web.Spec.Template.Spec.Volumes {
			if volume.Name == LogVolumeName {
				vectorLogVolumes = append(vectorLogVolumes, volume)
			}
		}
		Expect(vectorLogVolumes).To(HaveLen(1))
		Expect(vectorLogVolumes[0].EmptyDir).NotTo(BeNil())
		Expect(vectorLogVolumes[0].EmptyDir.Medium).To(Equal(corev1.StorageMediumMemory))
		Expect(vectorLogVolumes[0].EmptyDir.SizeLimit.String()).To(Equal("96Mi"))

		alternate := fetchSTS(clusterName + "-" + webserverRole + "-alternate")
		Expect(findInitContainerIn(alternate, vectorSidecarName)).NotTo(BeNil())
		alternateMain := findContainerIn(alternate, webserverRole)
		Expect(alternateMain).NotTo(BeNil())
		var alternateLogMounts []corev1.VolumeMount
		for _, mount := range alternateMain.VolumeMounts {
			if mount.Name == LogVolumeName {
				alternateLogMounts = append(alternateLogMounts, mount)
			}
		}
		Expect(alternateLogMounts).To(HaveLen(2))
		Expect(alternateLogMounts).To(ContainElements(
			corev1.VolumeMount{Name: LogVolumeName, MountPath: "/custom/log", ReadOnly: true},
			corev1.VolumeMount{Name: LogVolumeName, MountPath: constant.KubedoopLogDir},
		))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: clusterName + "-webservers-default",
		}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey("vector.yaml"))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring("RotatingFileHandler"))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring(
			"/kubedoop/log/webservers/airflow.py.json",
		))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring(
			"LOGGING_CONFIG['handlers']['console']['level'] = 'DEBUG'",
		))
	})

	It("rejects redirecting the Vector log path to a different volume", func() {
		const invalidLogVolumeCluster = "airflow-invalid-vector-log"
		aggregator := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid-log-aggregator", Namespace: namespace},
			Data:       map[string]string{vectorAddressKey: vectorAddress},
		}
		Expect(k8sClient.Create(ctx, aggregator)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, aggregator) })

		cr := newCluster()
		cr.Name = invalidLogVolumeCluster
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		cr.Spec.ClusterConfig.VectorAggregatorConfigMapName = aggregator.Name
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
					"spec": {
						"volumes": [{"name": "custom-log", "emptyDir": {}}],
						"containers": [{"name": "webservers", "volumeMounts": [{
							"name": "custom-log", "mountPath": "/kubedoop/log/"
						}]}]
					}
				}`)},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: namespace,
			Name:      invalidLogVolumeCluster,
		}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("podOverrides validation failed"))
		Expect(err.Error()).To(ContainSubstring("volume mount"))
	})

	It("lets an explicit role-group setting disable the Vector compatibility default", func() {
		const vectorDisabledCluster = "airflow-vector-disabled"
		aggregator := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "vector-disabled-aggregator", Namespace: namespace},
			Data:       map[string]string{vectorAddressKey: vectorAddress},
		}
		Expect(k8sClient.Create(ctx, aggregator)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, aggregator) })

		cr := newCluster()
		cr.Name = vectorDisabledCluster
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		cr.Spec.ClusterConfig.VectorAggregatorConfigMapName = aggregator.Name
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		disabled := false
		webGroup.Config = &airflowv1alpha1.ConfigSpec{RoleGroupConfigSpec: &commonsv1alpha1.RoleGroupConfigSpec{
			Logging: &commonsv1alpha1.LoggingSpec{EnableVectorAgent: &disabled},
		}}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		reconcileCluster(vectorDisabledCluster)

		web := fetchSTS(vectorDisabledCluster + "-webservers-default")
		Expect(findInitContainerIn(web, "vector")).To(BeNil())
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: vectorDisabledCluster + "-webservers-default",
		}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKeyWithValue("vector.yaml", ""))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring("RotatingFileHandler"))
		Expect(cm.Data["log_config.py"]).To(ContainSubstring(
			"/kubedoop/log/webservers/airflow.py.json",
		))
	})

	It("moves a legacy metric container podOverride onto the native sidecar", func() {
		const metricOverrideCluster = "airflow-legacy-metric-override"
		cr := newCluster()
		cr.Name = metricOverrideCluster
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
				"spec": {"containers": [
					{"name": "metric", "env": [{"name": "LEGACY_METRIC_OVERRIDE", "value": "kept"}],
					 "resources": {"requests": {"cpu": "25m"}}},
					{"name": "user-sidecar", "image": "busybox:1.36"}
				]}
			}`)},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

		reconcileCluster(metricOverrideCluster)
		web := fetchSTS(metricOverrideCluster + "-webservers-default")
		Expect(findContainerIn(web, "metric")).To(BeNil())
		Expect(findContainerIn(web, "user-sidecar")).NotTo(BeNil())
		metric := findInitContainerIn(web, metricsSidecarName)
		Expect(metric).NotTo(BeNil())
		Expect(metric.Image).NotTo(BeEmpty())
		Expect(metric.RestartPolicy).NotTo(BeNil())
		Expect(*metric.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
		Expect(metric.Resources.Requests.Cpu().String()).To(Equal("25m"))
		Expect(metric.Env).To(ContainElement(corev1.EnvVar{
			Name: "LEGACY_METRIC_OVERRIDE", Value: legacyOverrideValue,
		}))
	})

	It("moves a legacy vector container podOverride onto the native sidecar", func() {
		const vectorOverrideCluster = "airflow-legacy-vector-override"
		aggregator := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "vector-override-aggregator", Namespace: namespace},
			Data:       map[string]string{vectorAddressKey: vectorAddress},
		}
		Expect(k8sClient.Create(ctx, aggregator)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, aggregator) })

		cr := newCluster()
		cr.Name = vectorOverrideCluster
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		cr.Spec.ClusterConfig.VectorAggregatorConfigMapName = aggregator.Name
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
				"spec": {"containers": [{
					"name": "vector",
					"env": [{"name": "LEGACY_VECTOR_OVERRIDE", "value": "kept"}],
					"resources": {"limits": {"memory": "128Mi"}}
				}]}
			}`)},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

		reconcileCluster(vectorOverrideCluster)
		web := fetchSTS(vectorOverrideCluster + "-webservers-default")
		Expect(findContainerIn(web, vectorSidecarName)).To(BeNil())
		vector := findInitContainerIn(web, vectorSidecarName)
		Expect(vector).NotTo(BeNil())
		Expect(vector.Image).NotTo(BeEmpty())
		Expect(vector.RestartPolicy).NotTo(BeNil())
		Expect(*vector.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
		Expect(vector.Resources.Limits.Memory().String()).To(Equal("128Mi"))
		Expect(vector.Env).To(ContainElement(corev1.EnvVar{
			Name: "LEGACY_VECTOR_OVERRIDE", Value: legacyOverrideValue,
		}))
		Expect(vector.VolumeMounts).To(ContainElement(HaveField("Name", "vector-config")))
	})

	It("updates an owned legacy StatefulSet in place without changing its immutable identity", func() {
		const legacyClusterName = "airflow-legacy-upgrade"
		cr := newCluster()
		cr.Name = legacyClusterName
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		cr.Spec.Webservers.RoleConfig = &commonsv1alpha1.RoleConfigSpec{
			PodDisruptionBudget: &commonsv1alpha1.PodDisruptionBudgetSpec{},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

		resourceName := legacyClusterName + "-webservers-default"
		legacySelector := map[string]string{
			"app.kubernetes.io/name":       legacyAppName,
			"app.kubernetes.io/instance":   legacyClusterName,
			"app.kubernetes.io/managed-by": LabelDomain,
			"app.kubernetes.io/component":  webserverRole,
			"app.kubernetes.io/role-group": "default",
		}
		legacy := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceName,
				Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(cr, airflowv1alpha1.GroupVersion.WithKind("AirflowCluster")),
				},
			},
			Spec: appsv1.StatefulSetSpec{
				ServiceName:         resourceName,
				PodManagementPolicy: appsv1.OrderedReadyPodManagement,
				Selector:            &metav1.LabelSelector{MatchLabels: legacySelector},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: legacySelector},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: webserverRole, Image: "busybox:1.36",
					}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		legacyUID := legacy.UID
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })
		legacyService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceName,
				Namespace: namespace,
				Labels:    maps.Clone(legacySelector),
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(cr, airflowv1alpha1.GroupVersion.WithKind("AirflowCluster")),
				},
			},
			Spec: corev1.ServiceSpec{
				Selector: maps.Clone(legacySelector),
				Ports: []corev1.ServicePort{
					{
						Name: HTTPProtocol, Port: WebserverPort,
						TargetPort: intstr.FromString(HTTPProtocol), Protocol: corev1.ProtocolTCP,
					},
					{
						Name: MetricsPortName, Port: MetricsPort,
						TargetPort: intstr.FromString(MetricsPortName), Protocol: corev1.ProtocolTCP,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, legacyService)).To(Succeed())
		Expect(legacyService.Spec.ClusterIP).NotTo(BeEmpty())
		legacyServiceUID := legacyService.UID
		legacyServiceClusterIP := legacyService.Spec.ClusterIP
		legacyServiceClusterIPs := slices.Clone(legacyService.Spec.ClusterIPs)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacyService) })
		legacyRoleSelector := map[string]string{
			constant.LabelKubernetesInstance:  legacyClusterName,
			constant.LabelKubernetesName:      legacyAppName,
			constant.LabelKubernetesManagedBy: LabelDomain,
			constant.LabelKubernetesComponent: webserverRole,
		}
		legacyPDB := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      legacyClusterName + "-webservers",
				Namespace: namespace,
				Labels:    maps.Clone(legacyRoleSelector),
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: maps.Clone(legacyRoleSelector)},
			},
		}
		Expect(k8sClient.Create(ctx, legacyPDB)).To(Succeed())
		Expect(legacyPDB.OwnerReferences).To(BeEmpty())
		legacyPDBUID := legacyPDB.UID
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacyPDB) })

		reconcileCluster(legacyClusterName)

		updated := fetchSTS(resourceName)
		Expect(updated.UID).To(Equal(legacyUID))
		Expect(updated.Spec.Selector.MatchLabels).To(Equal(legacySelector))
		Expect(updated.Spec.ServiceName).To(Equal(resourceName))
		Expect(updated.Spec.PodManagementPolicy).To(Equal(appsv1.OrderedReadyPodManagement))
		Expect(updated.Spec.Template.Spec.ServiceAccountName).To(BeEmpty())
		selector, err := metav1.LabelSelectorAsSelector(updated.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		Expect(selector.Matches(labels.Set(updated.Spec.Template.Labels))).To(BeTrue())
		Expect(updated.Spec.Template.Labels).To(HaveKeyWithValue(
			"airflow.kubedoop.dev/cluster", legacyClusterName,
		))
		Expect(updated.Spec.Template.Labels).To(HaveKeyWithValue(
			"airflow.kubedoop.dev/role", webserverRole,
		))

		headless := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: resourceName + "-headless",
		}, headless)).To(Succeed())
		Expect(headless.Spec.Selector).To(Equal(legacySelector))
		Expect(labels.SelectorFromSet(headless.Spec.Selector).Matches(
			labels.Set(updated.Spec.Template.Labels),
		)).To(BeTrue())

		metrics := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: resourceName,
		}, metrics)).To(Succeed())
		Expect(metrics.UID).To(Equal(legacyServiceUID))
		Expect(metrics.Spec.ClusterIP).To(Equal(legacyServiceClusterIP))
		Expect(metrics.Spec.ClusterIPs).To(Equal(legacyServiceClusterIPs))
		Expect(metrics.Spec.Selector).To(Equal(legacySelector))
		Expect(metrics.Spec.Ports).To(Equal([]corev1.ServicePort{
			{
				Name: HTTPProtocol, Port: WebserverPort,
				TargetPort: intstr.FromString(HTTPProtocol), Protocol: corev1.ProtocolTCP,
			},
			{
				Name: MetricsPortName, Port: MetricsPort,
				TargetPort: intstr.FromInt32(MetricsPort), Protocol: corev1.ProtocolTCP,
			},
		}))

		pdb := &policyv1.PodDisruptionBudget{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: legacyClusterName + "-webservers",
		}, pdb)).To(Succeed())
		Expect(pdb.UID).To(Equal(legacyPDBUID))
		Expect(metav1.IsControlledBy(pdb, cr)).To(BeTrue())
		Expect(pdb.Labels).To(HaveKeyWithValue(reconciler.LabelRolePodDisruptionBudget, webserverRole))
		Expect(pdb.Spec.Selector.MatchLabels).To(Equal(map[string]string{
			constant.LabelKubernetesInstance:  legacyClusterName,
			constant.LabelKubernetesComponent: webserverRole,
		}))

		// Compatibility must not permanently pin the old workload identity: an explicit later
		// podOverride is the migration escape hatch and remains authoritative.
		latest := &airflowv1alpha1.AirflowCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: legacyClusterName,
		}, latest)).To(Succeed())
		webGroup := latest.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
				"spec": {"serviceAccountName": "explicit-workload-identity"}
			}`)},
		}
		latest.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		reconcileCluster(legacyClusterName)

		updated = fetchSTS(resourceName)
		Expect(updated.UID).To(Equal(legacyUID))
		Expect(updated.Spec.Template.Spec.ServiceAccountName).To(Equal("explicit-workload-identity"))
	})

	It("keeps user overrides above role declarations and derived defaults", func() {
		const overrideClusterName = "airflow-user-overrides"
		cr := newCluster()
		cr.Name = overrideClusterName
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			CliOverrides: []string{"--user-argument"},
			EnvOverrides: map[string]string{
				"AIRFLOW__CORE__EXECUTOR":             "UserExecutor",
				"AIRFLOW__DATABASE__SQL_ALCHEMY_CONN": "postgresql://user-override",
			},
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
				"spec": {"containers": [{"name": "webservers", "command": ["/custom-entrypoint"]}]}
			}`)},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		reconcileCluster(overrideClusterName)

		web := fetchSTS(overrideClusterName + "-webservers-default")
		main := findContainerIn(web, webserverRole)
		Expect(main).NotTo(BeNil())
		Expect(main.Command).To(Equal([]string{"/custom-entrypoint"}))
		Expect(main.Args).To(Equal([]string{"--user-argument"}))

		effectiveEnv := func(name string) *corev1.EnvVar {
			var result *corev1.EnvVar
			for i := range main.Env {
				if main.Env[i].Name == name {
					result = &main.Env[i]
				}
			}
			return result
		}
		executor := effectiveEnv("AIRFLOW__CORE__EXECUTOR")
		Expect(executor).NotTo(BeNil())
		Expect(executor.Value).To(Equal("UserExecutor"))
		database := effectiveEnv("AIRFLOW__DATABASE__SQL_ALCHEMY_CONN")
		Expect(database).NotTo(BeNil())
		Expect(database.Value).To(Equal("postgresql://user-override"))
		Expect(database.ValueFrom).To(BeNil())
	})

	It("forwards cliOverrides to the Airflow subcommand", func() {
		const cliClusterName = "airflow-cli-overrides"
		cr := newCluster()
		cr.Name = cliClusterName
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			CliOverrides: []string{"--port", "9090"},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		reconcileCluster(cliClusterName)

		main := findContainerIn(fetchSTS(cliClusterName+"-webservers-default"), webserverRole)
		Expect(main).NotTo(BeNil())
		Expect(main.Command[len(main.Command)-1]).To(Equal(webserverRole))
		Expect(strings.Join(main.Command, " ")).To(ContainSubstring(`airflow webserver "$@" &`))
		Expect(main.Args).To(Equal([]string{"--port", "9090"}))
	})

	It("preserves pre-Gen3 full-command cliOverrides", func() {
		const legacyCLIClusterName = "airflow-legacy-cli-overrides"
		cr := newCluster()
		cr.Name = legacyCLIClusterName
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			CliOverrides: []string{legacyShell, "-c", legacyCustomStart},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		reconcileCluster(legacyCLIClusterName)

		main := findContainerIn(fetchSTS(legacyCLIClusterName+"-webservers-default"), webserverRole)
		Expect(main).NotTo(BeNil())
		Expect(main.Command).To(Equal([]string{legacyShell, "-c", legacyCustomStart}))
		Expect(main.Args).To(BeEmpty())
	})

	It("keeps args-only podOverrides above a legacy full-command cliOverride", func() {
		const combinedCLIClusterName = "airflow-combined-cli-overrides"
		cr := newCluster()
		cr.Name = combinedCLIClusterName
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			CliOverrides: []string{legacyShell, "-c", "exec /opt/airflow/legacy-start"},
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
				"spec": {"containers": [{"name": "webservers", "args": ["--from-pod-override"]}]}
			}`)},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		reconcileCluster(combinedCLIClusterName)

		main := findContainerIn(fetchSTS(combinedCLIClusterName+"-webservers-default"), webserverRole)
		Expect(main).NotTo(BeNil())
		Expect(main.Command).To(Equal(bashEntrypoint))
		Expect(main.Args).To(Equal([]string{"--from-pod-override"}))
	})

	It("preserves the pre-Gen3 args-only podOverrides startup-script semantics", func() {
		const legacyArgsClusterName = "airflow-legacy-pod-args"
		cr := newCluster()
		cr.Name = legacyArgsClusterName
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
				"spec": {"containers": [{"name": "webservers", "args": ["` + legacyCustomStart + `"]}]}
			}`)},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		reconcileCluster(legacyArgsClusterName)

		main := findContainerIn(fetchSTS(legacyArgsClusterName+"-webservers-default"), webserverRole)
		Expect(main).NotTo(BeNil())
		Expect(main.Command).To(Equal(bashEntrypoint))
		Expect(main.Args).To(Equal([]string{legacyCustomStart}))
	})

	It("keeps a user-supplied log volume and legacy sidecar above the compatibility default", func() {
		const logOverrideClusterName = "airflow-log-volume-override"
		cr := newCluster()
		cr.Name = logOverrideClusterName
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		webGroup := cr.Spec.Webservers.RoleGroups[defaultGroupName]
		webGroup.OverridesSpec = &commonsv1alpha1.OverridesSpec{
			PodOverrides: &k8sruntime.RawExtension{Raw: []byte(`{
				"spec": {
					"volumes": [{"name": "log", "emptyDir": {"medium": "Memory", "sizeLimit": "64Mi"}}],
					"containers": [
						{"name": "webservers", "volumeMounts": [{
							"name": "log", "mountPath": "/kubedoop/log/", "readOnly": true
						}]},
						{
						"name": "log-auditor",
						"image": "busybox:1.36",
						"command": ["sh", "-c", "tail -F /kubedoop/log/webservers/airflow.py.json"],
						"volumeMounts": [{"name": "log", "mountPath": "/kubedoop/log"}]
					}]
				}
			}`)},
		}
		cr.Spec.Webservers.RoleGroups[defaultGroupName] = webGroup
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		reconcileCluster(logOverrideClusterName)

		sts := fetchSTS(logOverrideClusterName + "-webservers-default")
		var logVolumes []corev1.Volume
		for _, volume := range sts.Spec.Template.Spec.Volumes {
			if volume.Name == LogVolumeName {
				logVolumes = append(logVolumes, volume)
			}
		}
		Expect(logVolumes).To(HaveLen(1))
		Expect(logVolumes[0].EmptyDir).NotTo(BeNil())
		Expect(logVolumes[0].EmptyDir.Medium).To(Equal(corev1.StorageMediumMemory))
		Expect(logVolumes[0].EmptyDir.SizeLimit.String()).To(Equal("64Mi"))
		main := findContainerIn(sts, webserverRole)
		Expect(main).NotTo(BeNil())
		var mainLogMounts []corev1.VolumeMount
		for _, mount := range main.VolumeMounts {
			if mount.Name == LogVolumeName {
				mainLogMounts = append(mainLogMounts, mount)
			}
		}
		Expect(mainLogMounts).To(Equal([]corev1.VolumeMount{{
			Name: LogVolumeName, MountPath: constant.KubedoopLogDir, ReadOnly: true,
		}}))
		auditor := findContainerIn(sts, "log-auditor")
		Expect(auditor).NotTo(BeNil())
		Expect(auditor.VolumeMounts).To(ContainElement(And(
			HaveField("Name", LogVolumeName),
			HaveField("MountPath", "/kubedoop/log"),
		)))
	})

	It("resolves image and pull settings independently for different clusters", func() {
		first := newCluster()
		first.Name = "airflow-image-first"
		first.Spec.Schedulers = nil
		first.Spec.CeleryExecutors = nil
		first.Spec.Image = &airflowv1alpha1.ImageSpec{
			Custom:         "registry.example/airflow:first",
			PullPolicy:     corev1.PullAlways,
			PullSecretName: "registry-credentials",
		}
		second := newCluster()
		second.Name = "airflow-image-second"
		second.Spec.Schedulers = nil
		second.Spec.CeleryExecutors = nil
		second.Spec.Image = &airflowv1alpha1.ImageSpec{
			Custom:     "registry.example/airflow:second",
			PullPolicy: corev1.PullNever,
		}

		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, first)
			_ = k8sClient.Delete(ctx, second)
		})
		reconcileCluster(first.Name)
		reconcileCluster(second.Name)

		firstSTS := fetchSTS(first.Name + "-webservers-default")
		secondSTS := fetchSTS(second.Name + "-webservers-default")
		firstMain := findContainerIn(firstSTS, webserverRole)
		secondMain := findContainerIn(secondSTS, webserverRole)
		Expect(firstMain.Image).To(Equal("registry.example/airflow:first"))
		Expect(firstMain.ImagePullPolicy).To(Equal(corev1.PullAlways))
		Expect(secondMain.Image).To(Equal("registry.example/airflow:second"))
		Expect(secondMain.ImagePullPolicy).To(Equal(corev1.PullNever))
		Expect(firstSTS.Spec.Template.Spec.ImagePullSecrets).To(ContainElement(
			corev1.LocalObjectReference{Name: "registry-credentials"},
		))
		Expect(secondSTS.Spec.Template.Spec.ImagePullSecrets).To(BeEmpty())
		Expect(findInitContainerIn(firstSTS, "metric").Image).To(Equal(firstMain.Image))
		Expect(findInitContainerIn(secondSTS, "metric").Image).To(Equal(secondMain.Image))
	})

	It("replaces the persisted pre-Gen3 development-version default with the operator build version", func() {
		const legacyImageClusterName = "airflow-legacy-image-default"
		cr := newCluster()
		cr.Name = legacyImageClusterName
		cr.Spec.Schedulers = nil
		cr.Spec.CeleryExecutors = nil
		cr.Spec.Image = &airflowv1alpha1.ImageSpec{
			Repo:            airflowv1alpha1.DefaultRepository,
			ProductVersion:  airflowv1alpha1.DefaultProductVersion,
			KubedoopVersion: "0.0.0-dev",
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		reconcileCluster(legacyImageClusterName)

		main := findContainerIn(fetchSTS(legacyImageClusterName+"-webservers-default"), webserverRole)
		Expect(main).NotTo(BeNil())
		Expect(main.Image).To(Equal(
			"quay.io/zncdatadev/airflow:2.10.5-kubedoop0.0.0-test",
		))
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
