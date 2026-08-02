package controller

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/airflow-operator/internal/util/version"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/builder"
	"github.com/zncdatadev/operator-go/pkg/constant"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/sidecar"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AirflowRoleGroupHandler builds the resources for one Airflow role group (webserver, scheduler
// or celery executor).
//
// It embeds reconciler.BaseRoleGroupHandler to inherit the framework's canonical resource
// construction (labels, headless Service, builder-built StatefulSet, config volume, role-level
// PDB, sidecar injection), then customizes the returned StatefulSet and ConfigMap with Airflow
// specifics: the role's startup script, the credentials-secret env fan-out, the statsd-exporter
// metrics sidecar, the product's Python config files, and a metrics Service that keeps the
// pre-framework name and Prometheus annotations.
type AirflowRoleGroupHandler struct {
	reconciler.BaseRoleGroupHandler[*airflowv1alpha1.AirflowCluster]
}

var _ reconciler.RoleGroupHandler[*airflowv1alpha1.AirflowCluster] = &AirflowRoleGroupHandler{}

// LabelDomain is the product domain for identity (selector) labels:
// airflow.kubedoop.dev/{cluster,role,role-group}.
const LabelDomain = "airflow.kubedoop.dev"

const (
	AppConfigPath = constant.KubedoopRoot + "app/config"
	AirflowHome   = constant.KubedoopRoot + "airflow"

	LogVolumeName = "log"
	LogVolumeSize = "500Mi"

	MetricsPort     = int32(9102)
	MetricsPortName = "metrics"

	// executorCelery is the only executor the controller implements; the CRD still declares a
	// kubernetesExecutors role that was never wired.
	executorCelery = "CeleryExecutor"
)

const (
	EnvKeyAdminUserName  = "ADMIN_USERNAME"
	ENVKeyAdminFirstName = "ADMIN_FIRSTNAME"
	EnvKeyAdminLastName  = "ADMIN_LASTNAME"
	EnvKeyAdminEmail     = "ADMIN_EMAIL"
	EnvKeyAdminPassword  = "ADMIN_PASSWORD"
)

// bashEntrypoint is the entrypoint both the role containers and the metrics sidecar run under:
// xtrace for diagnosability, strict error handling, and the script passed as the -c argument.
var bashEntrypoint = []string{"/bin/bash", "-x", "-euo", "pipefail", "-c"}

// BashLibs is the signal-handling shell library sourced by every container entrypoint.
const BashLibs = `
prepare_signal_handlers()
{
    unset term_child_pid
    unset term_kill_needed
    trap 'handle_term_signal' TERM
}

handle_term_signal()
{
    if [ "${term_child_pid}" ]; then
        kill -TERM "${term_child_pid}" 2>/dev/null
    else
        term_kill_needed="yes"
    fi
}

wait_for_termination()
{
    set +e
    term_child_pid=$1
    if [[ -v term_kill_needed ]]; then
        kill -TERM "${term_child_pid}" 2>/dev/null
    fi
    wait ${term_child_pid} 2>/dev/null
    trap - TERM
    wait ${term_child_pid} 2>/dev/null
    set -e
}
`

// NewAirflowRoleGroupHandler creates a handler with the framework-level options that are constant
// across reconciliations. Per-CR options (images, pull policy) are set in BuildResources.
func NewAirflowRoleGroupHandler(scheme *runtime.Scheme) *AirflowRoleGroupHandler {
	h := &AirflowRoleGroupHandler{}
	h.Scheme = scheme
	h.ImagePullPolicy = corev1.PullIfNotPresent
	h.LabelDomain = LabelDomain
	// Only the webserver declares container ports (http first: it is the readiness probe target,
	// per the framework's Ports[0] contract). Schedulers and celery executors declare none, so
	// they get no generated probe and no client Service — exactly the pre-framework shape.
	h.RoleContainerPorts = map[string][]corev1.ContainerPort{
		string(airflowv1alpha1.WebserversRoleName): {
			{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
			{Name: MetricsPortName, ContainerPort: MetricsPort, Protocol: corev1.ProtocolTCP},
		},
		string(airflowv1alpha1.SchedulersRoleName):      {},
		string(airflowv1alpha1.CeleryExecutorsRoleName): {},
	}
	// The main container keeps the role name, as before the migration (podOverrides and the
	// metrics sidecar address it by that name).
	for _, role := range []airflowv1alpha1.RoleName{
		airflowv1alpha1.WebserversRoleName,
		airflowv1alpha1.SchedulersRoleName,
		airflowv1alpha1.CeleryExecutorsRoleName,
	} {
		h.SetRoleMainContainerName(string(role), string(role))
	}
	return h
}

// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=authentication.kubedoop.dev,resources=authenticationclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps;services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// BuildResources builds all Kubernetes resources for one Airflow role group.
func (h *AirflowRoleGroupHandler) BuildResources(
	ctx context.Context,
	k8sClient client.Client,
	cr *airflowv1alpha1.AirflowCluster,
	buildCtx *reconciler.RoleGroupBuildContext,
) (*reconciler.RoleGroupResources, error) {
	roleName := buildCtx.RoleName
	if roleName != string(airflowv1alpha1.WebserversRoleName) &&
		roleName != string(airflowv1alpha1.SchedulersRoleName) &&
		roleName != string(airflowv1alpha1.CeleryExecutorsRoleName) {
		return nil, fmt.Errorf("unsupported role: %s", roleName)
	}

	clusterConfig := cr.Spec.ClusterConfig
	if clusterConfig == nil {
		return nil, fmt.Errorf("clusterConfig is required")
	}
	if clusterConfig.Credentials == "" {
		return nil, fmt.Errorf("credentials secret name in cluster config is empty")
	}

	// Resolve authentication (webserver_config.py + OIDC env). Only the webserver consumes the
	// env vars, but every role's config file carries the rendered section, as before.
	var auth *Authentication
	if len(clusterConfig.Authentication) > 0 {
		var err error
		auth, err = NewAuthentication(ctx, k8sClient, clusterConfig.Authentication)
		if err != nil {
			return nil, fmt.Errorf("failed to create authentication: %w", err)
		}
	}

	image := h.resolveImage(cr)
	if cr.Spec.Image != nil && cr.Spec.Image.PullPolicy != "" {
		h.ImagePullPolicy = cr.Spec.Image.PullPolicy
	}

	// Per-CR base inputs.
	h.SetRoleImage(roleName, image)

	// Register the statsd-exporter metrics sidecar before base.BuildResources, which runs
	// SidecarManager.InjectAll.
	h.registerMetricsSidecar(buildCtx, image)

	if clusterConfig.VectorAggregatorConfigMapName != "" {
		log.Log.Info("clusterConfig.vectorAggregatorConfigMapName is set, but Vector log " +
			"aggregation is not wired in this operator version yet; the setting is ignored")
	}

	// Let the framework build the skeleton: canonical labels, headless Service, StatefulSet
	// (config volume + injected sidecars), and the ConfigMap shell. No client Service is built:
	// no role declares service ports (parity with the pre-framework operator, whose metrics
	// Service overwrote the webserver's client Service under the same name).
	res, err := h.BaseRoleGroupHandler.BuildResources(ctx, k8sClient, cr, buildCtx)
	if err != nil {
		return nil, fmt.Errorf("base build failed: %w", err)
	}

	if err := h.customizeStatefulSet(res.StatefulSet, buildCtx, clusterConfig, auth); err != nil {
		return nil, err
	}

	if err := h.customizeConfigMap(res.ConfigMap, buildCtx, auth); err != nil {
		return nil, err
	}

	// The metrics Service keeps the pre-framework naming: it is published under the role group
	// resource name itself (not "<name>-metrics"), with the Prometheus annotations the e2e suite
	// asserts. The framework identifies the slot by the labels it stamps at apply time, so the
	// custom name stays fully managed, including reclaim when metrics are turned off.
	res.MetricsService = builder.NewMetricsServiceBuilder(
		buildCtx.ResourceName,
		buildCtx.ClusterNamespace,
		MetricsPort,
		res.StatefulSet.Labels,
	).
		WithName(buildCtx.ResourceName).
		WithPath("/metrics").
		WithTargetPortName(MetricsPortName).
		WithSelector(h.SelectorLabels(buildCtx)).
		Build()

	return res, nil
}

// resolveImage constructs the container image string from the CR spec, mirroring the
// pre-framework util.Image resolution: <repo>/<product>:<productVersion>-kubedoop<kubedoopVersion>,
// with the kubedoop suffix defaulting to this operator's build version.
func (h *AirflowRoleGroupHandler) resolveImage(cr *airflowv1alpha1.AirflowCluster) string {
	repo := airflowv1alpha1.DefaultRepository
	productVersion := airflowv1alpha1.DefaultProductVersion
	kubedoopVersion := version.BuildVersion

	if cr.Spec.Image != nil {
		img := cr.Spec.Image
		if img.Custom != "" {
			return img.Custom
		}
		if img.Repo != "" {
			repo = img.Repo
		}
		if img.ProductVersion != "" {
			productVersion = img.ProductVersion
		}
		if img.KubedoopVersion != "" {
			kubedoopVersion = img.KubedoopVersion
		}
	}

	return fmt.Sprintf("%s/%s:%s-kubedoop%s",
		repo, airflowv1alpha1.DefaultProductName, productVersion, kubedoopVersion)
}

// registerMetricsSidecar registers the statsd-exporter container on the SidecarManager so
// base.BuildResources injects it. It runs the product image's bundled exporter binary.
//
// The framework injects managed containers into InitContainers; RestartPolicy: Always makes it
// a native sidecar (stable since Kubernetes 1.33), which is the framework's idiom for a
// long-running companion — the pre-framework operator ran it as a regular container, which is
// the only rendered-placement change for this pod.
func (h *AirflowRoleGroupHandler) registerMetricsSidecar(buildCtx *reconciler.RoleGroupBuildContext, image string) {
	args := BashLibs + `

prepare_signal_handlers

` + path.Join(constant.KubedoopRoot, "bin", "statsd-exporter") + `&
wait_for_termination $!`

	container := &corev1.Container{
		Name:            "metric",
		Image:           image,
		ImagePullPolicy: h.ImagePullPolicy,
		Command:         bashEntrypoint,
		Args:            []string{indentTabs4Spaces(args)},
		RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways),
	}

	buildCtx.SidecarManager.Register(
		sidecar.NewStaticContainerProvider(*container),
		&sidecar.SidecarConfig{Enabled: true},
	)
}

// customizeStatefulSet sets the main container's entrypoint and env, and adds the shared log
// volume the framework does not own (no logging producers are declared; log_config.py is
// product-rendered).
func (h *AirflowRoleGroupHandler) customizeStatefulSet(
	sts *appsv1.StatefulSet,
	buildCtx *reconciler.RoleGroupBuildContext,
	clusterConfig *airflowv1alpha1.ClusterConfigSpec,
	auth *Authentication,
) error {
	roleName := buildCtx.RoleName
	podSpec := &sts.Spec.Template.Spec

	mainContainer := findContainer(podSpec, roleName)
	if mainContainer == nil {
		return fmt.Errorf("main container %q not found in built StatefulSet", roleName)
	}

	script, err := mainContainerScript(roleName)
	if err != nil {
		return err
	}
	mainContainer.Command = bashEntrypoint
	mainContainer.Args = []string{script}

	envs, err := h.credentialsEnv(roleName, clusterConfig, auth)
	if err != nil {
		return err
	}
	mainContainer.Env = append(mainContainer.Env, envs...)

	mainContainer.VolumeMounts = append(mainContainer.VolumeMounts, corev1.VolumeMount{
		Name:      LogVolumeName,
		MountPath: strings.TrimSuffix(constant.KubedoopLogDir, "/"),
	})

	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: LogVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: ptr.To(resource.MustParse(LogVolumeSize)),
			},
		},
	})

	return nil
}

// credentialsEnv builds the env vars sourced from the user-provided credentials secret (valueFrom
// cannot flow through the config merge pipeline, which carries plain strings only), plus the
// webserver's OIDC client credentials when authentication is configured.
func (h *AirflowRoleGroupHandler) credentialsEnv(
	roleName string,
	clusterConfig *airflowv1alpha1.ClusterConfigSpec,
	auth *Authentication,
) ([]corev1.EnvVar, error) {
	credentialsName := clusterConfig.Credentials
	if credentialsName == "" {
		return nil, fmt.Errorf("credentials secret name in cluster config is empty")
	}

	secretEnv := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key:                  key,
					LocalObjectReference: corev1.LocalObjectReference{Name: credentialsName},
				},
			},
		}
	}

	envs := []corev1.EnvVar{
		secretEnv("AIRFLOW__WEBSERVER__SECRET_KEY", "appSecretKey"),
		secretEnv("AIRFLOW__DATABASE__SQL_ALCHEMY_CONN", "connections.sqlalchemyDatabaseUri"),
		// The executor is always CeleryExecutor in this operator version.
		secretEnv("AIRFLOW__CELERY__RESULT_BACKEND", "connections.celeryResultBackend"),
		secretEnv("AIRFLOW__CELERY__BROKER_URL", "connections.celeryBrokerUrl"),
	}

	if roleName == string(airflowv1alpha1.SchedulersRoleName) {
		envKeyMapping := [][]string{
			{EnvKeyAdminUserName, "adminUser.username"},
			{ENVKeyAdminFirstName, "adminUser.firstname"},
			{EnvKeyAdminLastName, "adminUser.lastname"},
			{EnvKeyAdminEmail, "adminUser.email"},
			{EnvKeyAdminPassword, "adminUser.password"},
		}
		for _, mapping := range envKeyMapping {
			envs = append(envs, secretEnv(mapping[0], mapping[1]))
		}
	}

	// The pre-framework code compared the role GROUP name against the role name here, so the OIDC
	// env never reached a webserver in any group not literally named "webservers". Fixed to match
	// the documented intent.
	if roleName == string(airflowv1alpha1.WebserversRoleName) && auth != nil {
		envs = append(envs, auth.GetEnvVars()...)
	}

	return envs, nil
}

// customizeConfigMap renders the product's Python config files into the framework-built ConfigMap.
func (h *AirflowRoleGroupHandler) customizeConfigMap(
	cm *corev1.ConfigMap,
	buildCtx *reconciler.RoleGroupBuildContext,
	auth *Authentication,
) error {
	webserverConfig, err := renderWebserverConfig(auth)
	if err != nil {
		return fmt.Errorf("failed to render webserver_config.py: %w", err)
	}
	cm.Data["webserver_config.py"] = webserverConfig
	cm.Data["log_config.py"] = renderLogging(buildCtx.RoleName, buildCtx.RoleGroupSpec.Config)
	return nil
}

// mainContainerScript renders the role's bash entrypoint: workspace preparation, signal-handler
// plumbing, the role command backgrounded, and a wait that forwards SIGTERM.
func mainContainerScript(roleName string) (string, error) {
	var mainCommand string

	switch airflowv1alpha1.RoleName(roleName) {
	case airflowv1alpha1.WebserversRoleName:
		mainCommand = "airflow webserver &"
	case airflowv1alpha1.SchedulersRoleName:
		mainCommand = `
airflow db init
airflow db upgrade
set +x	# disable xtrace
airflow users create \
	--username $` + EnvKeyAdminUserName + ` \
	--firstname $` + ENVKeyAdminFirstName + ` \
	--lastname $` + EnvKeyAdminLastName + ` \
	--email $` + EnvKeyAdminEmail + ` \
	--password $` + EnvKeyAdminPassword + ` \
	--role "Admin"

set -x 	# enable xtrace

airflow scheduler &
`
	case airflowv1alpha1.CeleryExecutorsRoleName:
		mainCommand = "airflow celery worker &"
	default:
		return "", fmt.Errorf("unsupported role %s", roleName)
	}

	args := `
mkdir -p ` + AppConfigPath + `
mkdir -p ` + AirflowHome + `
cp -RL ` + constant.KubedoopConfigDirMount + `*.py ` + AppConfigPath + `
cp -RL ` + constant.KubedoopConfigDirMount + `*.py ` + AirflowHome + `

` + BashLibs + `

prepare_signal_handlers

` + mainCommand + `
wait_for_termination $!
`
	return indentTabs4Spaces(args), nil
}

// ComputeProductConfig contributes Airflow's static environment as the lowest merge layer, so
// envOverrides in the CRD always win. Plain values only: anything sourced from a Secret is
// appended in BuildResources (the merge pipeline carries no valueFrom).
func ComputeProductConfig(cr *airflowv1alpha1.AirflowCluster, roleName, roleGroupName string) *commonsv1alpha1.OverridesSpec {
	dagsFolder := path.Join(AirflowHome, "dags")
	if cr.Spec.ClusterConfig != nil && len(cr.Spec.ClusterConfig.DagsGitSync) > 0 {
		if gitFolder := cr.Spec.ClusterConfig.DagsGitSync[0].GitFolder; gitFolder != "" {
			dagsFolder = path.Join(dagsFolder, gitFolder)
		}
	}

	loadExamples := false
	exposeConfig := false
	if cr.Spec.ClusterConfig != nil {
		loadExamples = cr.Spec.ClusterConfig.LoadExamples
		exposeConfig = cr.Spec.ClusterConfig.ExposeConfig
	}

	return &commonsv1alpha1.OverridesSpec{
		EnvOverrides: map[string]string{
			"PYTHONPATH":                             strings.Join([]string{AppConfigPath, dagsFolder}, ":"),
			"AIRFLOW__CORE__DAGS_FOLDER":             dagsFolder,
			"AIRFLOW__LOGGING__LOGGING_CONFIG_CLASS": "log_config.LOGGING_CONFIG",
			"AIRFLOW__METRICS__STATSD_ON":            "True",
			"AIRFLOW__METRICS__STATSD_HOST":          "0.0.0.0",
			"AIRFLOW__METRICS__STATSD_PORT":          "8125",
			"AIRFLOW__API__AUTH_BACKENDS":            "airflow.api.auth.backend.basic_auth",
			"AIRFLOW__CORE__LOAD_EXAMPLES":           strconv.FormatBool(loadExamples),
			"AIRFLOW__WEBSERVER__EXPOSE_CONFIG":      strconv.FormatBool(exposeConfig),
			"AIRFLOW__CORE__EXECUTOR":                executorCelery,
		},
	}
}

func findContainer(podSpec *corev1.PodSpec, name string) *corev1.Container {
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == name {
			return &podSpec.Containers[i]
		}
	}
	return nil
}
