package controller

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/airflow-operator/internal/util/version"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/builder"
	"github.com/zncdatadev/operator-go/pkg/constant"
	"github.com/zncdatadev/operator-go/pkg/productlogging"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/sidecar"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AirflowRoleGroupHandler builds the resources for one Airflow role group (webserver, scheduler
// or celery executor).
//
// It embeds reconciler.BaseRoleGroupHandler to inherit the framework's canonical resource
// construction (labels, headless Service, builder-built StatefulSet, config volume, role-level
// PDB, sidecar injection). Role declarations supply Airflow's entrypoints, secret-backed env and
// logging producers; BuildResources adds the statsd exporter, product-owned Python config files,
// the legacy-compatible metrics Service and the narrow Gen 2 StatefulSet identity shim.
type AirflowRoleGroupHandler struct {
	*reconciler.BaseRoleGroupHandler[*airflowv1alpha1.AirflowCluster]
}

// legacyLogVolumeProvider preserves the always-present Gen 2 log emptyDir and main-container
// mount on role groups that do not run Vector. When Vector is active its provider owns the exact
// same volume and mount instead, so the pod never receives duplicate names.
type legacyLogVolumeProvider struct{}

func (legacyLogVolumeProvider) Volumes() []corev1.Volume {
	limit := resource.MustParse(LogVolumeSize)
	return []corev1.Volume{{
		Name: LogVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: &limit,
		}},
	}}
}

func (legacyLogVolumeProvider) VolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{{Name: LogVolumeName, MountPath: constant.KubedoopLogDir}}
}

var (
	_ reconciler.RoleGroupHandler[*airflowv1alpha1.AirflowCluster] = &AirflowRoleGroupHandler{}
	_ reconciler.RoleProvider[*airflowv1alpha1.AirflowCluster]     = &AirflowRoleGroupHandler{}
)

// LabelDomain is the product domain for identity (selector) labels:
// airflow.kubedoop.dev/{cluster,role,role-group}.
const LabelDomain = "airflow.kubedoop.dev"

const (
	AppConfigPath = constant.KubedoopRoot + "app/config"
	AirflowHome   = constant.KubedoopRoot + "airflow"

	LogVolumeName = "log"
	LogVolumeSize = "500Mi"

	WebserverPort   = int32(8080)
	MetricsPort     = int32(9102)
	MetricsPortName = "metrics"
	HTTPProtocol    = "http"

	metricsSidecarName = "metric"
	vectorSidecarName  = "vector"
	legacyAppName      = "airflowcluster"

	// executorCelery is the only executor the controller implements; admission rejects the
	// historical kubernetesExecutors field because it was never wired.
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

// ImageDefaults supplies the image fields that the CR leaves empty. KubedoopVersion is evaluated
// on every reconcile, so upgrading the operator also selects its co-released product image unless
// the user pinned an explicit version. The one exception is the historical "0.0.0-dev" schema
// default: it is treated as a migration sentinel because an explicit pin is indistinguishable from
// the value the old API server persisted for an omitted field.
func ImageDefaults() commonsv1alpha1.ImageSpec {
	return commonsv1alpha1.ImageSpec{
		Repo:            airflowv1alpha1.DefaultRepository,
		ProductVersion:  airflowv1alpha1.DefaultProductVersion,
		KubedoopVersion: version.BuildVersion,
		PullPolicy:      corev1.PullIfNotPresent,
	}
}

// NewAirflowRoleGroupHandler creates a stateless handler. Per-cluster and per-role inputs are
// declared by DeclareRoles and carried by RoleGroupBuildContext, never written onto this shared
// object.
func NewAirflowRoleGroupHandler(scheme *runtime.Scheme) *AirflowRoleGroupHandler {
	base := reconciler.NewBaseRoleGroupHandler[*airflowv1alpha1.AirflowCluster](scheme)
	base.LabelDomain = LabelDomain
	return &AirflowRoleGroupHandler{BaseRoleGroupHandler: base}
}

// BuildRolePodDisruptionBudget keeps the selector on the two recommended labels shared by both
// Gen 2 and Gen 3 pods. This prevents an enabled PDB from temporarily selecting zero pods while a
// legacy StatefulSet rolls onto templates carrying the new product-domain identity labels.
func (h *AirflowRoleGroupHandler) BuildRolePodDisruptionBudget(
	buildCtx *reconciler.RoleBuildContext,
) *policyv1.PodDisruptionBudget {
	pdb := h.BaseRoleGroupHandler.BuildRolePodDisruptionBudget(buildCtx)
	if pdb == nil {
		return nil
	}
	pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
		constant.LabelKubernetesInstance:  buildCtx.ClusterName,
		constant.LabelKubernetesComponent: buildCtx.RoleName,
	}}
	return pdb
}

// DeclareRoles describes all supported Airflow roles once per reconciliation. Declarations are
// applied before user overrides, which keeps envOverrides and podOverrides authoritative.
func (h *AirflowRoleGroupHandler) DeclareRoles(
	ctx context.Context,
	k8sClient client.Client,
	cr *airflowv1alpha1.AirflowCluster,
) (reconciler.RoleCatalog, error) {
	clusterConfig := cr.Spec.ClusterConfig
	if clusterConfig == nil {
		return nil, fmt.Errorf("clusterConfig is required")
	}
	if clusterConfig.Credentials == "" {
		return nil, fmt.Errorf("credentials secret name in cluster config is empty")
	}
	if err := validateRoleGroupServiceSlots(cr); err != nil {
		return nil, err
	}
	if err := validateLegacyResourceInventory(ctx, k8sClient, cr); err != nil {
		return nil, err
	}
	if err := validateLegacyRoleGroupResourceNames(ctx, k8sClient, cr); err != nil {
		return nil, err
	}

	var auth *Authentication
	if len(clusterConfig.Authentication) > 0 {
		var err error
		auth, err = NewAuthentication(ctx, k8sClient, clusterConfig.Authentication)
		if err != nil {
			return nil, fmt.Errorf("failed to create authentication: %w", err)
		}
	}

	// Gen 2 enabled Vector whenever an aggregator ConfigMap was configured. Preserve that API
	// contract as a product default, while still allowing a role or role group to explicitly set
	// logging.enableVectorAgent=false at the higher-precedence user layer.
	var configDefaults *commonsv1alpha1.RoleGroupConfigSpec
	if clusterConfig.VectorAggregatorConfigMapName != "" {
		enabled := true
		configDefaults = &commonsv1alpha1.RoleGroupConfigSpec{
			Logging: &commonsv1alpha1.LoggingSpec{EnableVectorAgent: &enabled},
		}
	}

	catalog := make(reconciler.RoleCatalog, 3)
	for _, role := range []airflowv1alpha1.RoleName{
		airflowv1alpha1.WebserversRoleName,
		airflowv1alpha1.SchedulersRoleName,
		airflowv1alpha1.CeleryExecutorsRoleName,
	} {
		roleName := string(role)
		script, err := mainContainerScript(roleName)
		if err != nil {
			return nil, err
		}
		env, err := h.credentialsEnv(roleName, clusterConfig, auth)
		if err != nil {
			return nil, err
		}

		var ports []corev1.ContainerPort
		if role == airflowv1alpha1.WebserversRoleName {
			ports = []corev1.ContainerPort{{
				Name: HTTPProtocol, ContainerPort: WebserverPort, Protocol: corev1.ProtocolTCP,
			}}
		}

		command := append([]string{}, bashEntrypoint...)
		// bash consumes the first word after the -c script as $0. Keep a stable sentinel there so
		// operator-go's cliOverrides, which are appended as container args, arrive in the script as
		// $@ and can be forwarded to the selected Airflow subcommand.
		command = append(command, script, roleName)
		catalog[roleName] = reconciler.RoleDeclaration{
			MainContainerName: roleName,
			ContainerPorts:    ports,
			Command:           command,
			Env:               env,
			LogProducers: []productlogging.ContainerLogging{{
				Container:   roleName,
				LogFileName: "airflow.py.json",
			}},
			LogVolumeSize:  LogVolumeSize,
			ConfigDefaults: configDefaults,
			Optional:       true,
		}
	}

	return catalog, nil
}

// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=authentication.kubedoop.dev,resources=authenticationclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;delete
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

	// Resolve authentication (webserver_config.py + OIDC env). Only the webserver receives the
	// provider section and credentials: Airflow's scheduler-side users CLI also imports
	// webserver_config.py, so rendering LDAP file reads into an unmounted scheduler config would
	// crash its bootstrap before the scheduler starts.
	var auth *Authentication
	if len(clusterConfig.Authentication) > 0 {
		var err error
		auth, err = NewAuthentication(ctx, k8sClient, clusterConfig.Authentication)
		if err != nil {
			return nil, fmt.Errorf("failed to create authentication: %w", err)
		}
	}

	// Register the statsd-exporter metrics sidecar before base.BuildResources, which runs
	// SidecarManager.InjectAll.
	h.registerMetricsSidecar(buildCtx)
	managedSidecarOverrides, err := captureManagedSidecarOverrides(buildCtx)
	if err != nil {
		return nil, err
	}
	if roleName == string(airflowv1alpha1.WebserversRoleName) && auth != nil {
		buildCtx.VolumeProviders = append(
			buildCtx.VolumeProviders,
			authenticationVolumeProviders(buildCtx, auth)...,
		)
	}
	// Gen 2 always supplied this bounded volume before applying podOverrides. Register it even when
	// Vector is active: the framework provider de-duplicates the same name later, while having the
	// volume in the base template keeps legacy mount-only overrides valid during builder validation.
	buildCtx.VolumeProviders = append(buildCtx.VolumeProviders, legacyLogVolumeProvider{})

	// Let the framework build the skeleton: canonical labels, headless Service, StatefulSet
	// (config volume + injected sidecars), and the ConfigMap shell. No client Service is built by
	// the base handler because Airflow's historical metrics and web client reconcilers claimed the
	// same fixed name. The combined, stable Service is built below in that legacy resource slot.
	res, err := h.BaseRoleGroupHandler.BuildResources(ctx, k8sClient, cr, buildCtx)
	if err != nil {
		return nil, fmt.Errorf("base build failed: %w", err)
	}
	if err := applyManagedSidecarOverrides(&res.StatefulSet.Spec.Template.Spec, managedSidecarOverrides); err != nil {
		return nil, err
	}
	if err := ensureCanonicalLogMount(buildCtx, &res.StatefulSet.Spec.Template.Spec); err != nil {
		return nil, err
	}
	if err := preserveLegacyCliOverride(buildCtx, res.StatefulSet); err != nil {
		return nil, err
	}
	legacySelector, err := preserveLegacyStatefulSetIdentity(ctx, k8sClient, cr, buildCtx, res.StatefulSet)
	if err != nil {
		return nil, err
	}
	serviceSelector := h.SelectorLabels(buildCtx)
	if legacySelector != nil {
		serviceSelector = legacySelector
		// The old metrics Service remains the StatefulSet governing Service after upgrade. Keeping
		// both Service selectors on the frozen legacy identity avoids an endpoint gap before the
		// first pod rollout has added the new product-domain labels.
		res.HeadlessService.Spec.Selector = maps.Clone(legacySelector)
	}

	if err := h.customizeConfigMap(res.ConfigMap, buildCtx, authenticationForRole(roleName, auth)); err != nil {
		return nil, err
	}

	// Airflow's historical metrics Service already occupies the framework's ordinary Service
	// slot: its name is exactly the role-group resource name and it is ClusterIP (not the
	// framework's suffixed, headless MetricsService slot). Construct that fixed slot directly so
	// upgrades keep the name, ClusterIP and scheduler/celery readiness behaviour.
	serviceBuilder := builder.NewServiceBuilder(buildCtx.ResourceName, buildCtx.ClusterNamespace).
		WithLabels(res.StatefulSet.Labels).
		WithAnnotations(prometheusAnnotations()).
		WithSelector(serviceSelector)
	if roleName == string(airflowv1alpha1.WebserversRoleName) {
		serviceBuilder = serviceBuilder.AddPort(
			HTTPProtocol,
			WebserverPort,
			intstr.FromString(HTTPProtocol),
			corev1.ProtocolTCP,
		)
	}
	res.Service = serviceBuilder.
		AddPort(MetricsPortName, MetricsPort, intstr.FromInt(int(MetricsPort)), corev1.ProtocolTCP).
		Build()
	res.Service.Labels["prometheus.io/scrape"] = "true"

	return res, nil
}

func authenticationForRole(roleName string, auth *Authentication) *Authentication {
	if roleName != string(airflowv1alpha1.WebserversRoleName) {
		return nil
	}
	return auth
}

func prometheusAnnotations() map[string]string {
	return map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   strconv.Itoa(int(MetricsPort)),
		"prometheus.io/path":   "/metrics",
		"prometheus.io/scheme": HTTPProtocol,
	}
}

// ensureCanonicalLogMount preserves Gen 2 strategic-merge behaviour when a podOverride mounts the
// log volume at an additional path. The v0.13 sidecar manager de-duplicates provider mounts by
// volume name, so without this bridge an alternate-path "log" mount can suppress the canonical
// mount even though Airflow's generated log_config.py still writes below KubedoopLogDir. A same-path
// override using the canonical volume keeps highest precedence. With Vector enabled, redirecting
// that path to another volume cannot be made coherent with the collector and fails explicitly.
func ensureCanonicalLogMount(
	buildCtx *reconciler.RoleGroupBuildContext,
	podSpec *corev1.PodSpec,
) error {
	mainName := buildCtx.Declaration.MainContainerName
	if mainName == "" {
		mainName = buildCtx.ResourceName
	}

	for i := range podSpec.Containers {
		container := &podSpec.Containers[i]
		if container.Name != mainName {
			continue
		}
		var canonicalMounts []corev1.VolumeMount
		for _, mount := range container.VolumeMounts {
			if mount.MountPath == constant.KubedoopLogDir {
				canonicalMounts = append(canonicalMounts, mount)
			}
		}
		vectorEnabled := buildCtx.SidecarManager != nil &&
			managedSidecarEnabled(buildCtx.SidecarManager, vectorSidecarName)
		if vectorEnabled && (len(canonicalMounts) != 1 || canonicalMounts[0].Name != LogVolumeName) {
			mountNames := make([]string, 0, len(canonicalMounts))
			for _, mount := range canonicalMounts {
				mountNames = append(mountNames, mount.Name)
			}
			return fmt.Errorf(
				"podOverrides for role %q group %q must leave exactly one %s mount backed by volume %q when Vector is enabled; found volume names %v",
				buildCtx.RoleName,
				buildCtx.RoleGroupName,
				constant.KubedoopLogDir,
				LogVolumeName,
				mountNames,
			)
		}
		if len(canonicalMounts) > 0 {
			return nil
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      LogVolumeName,
			MountPath: constant.KubedoopLogDir,
		})
		return nil
	}

	return fmt.Errorf("main container %q not found while ensuring the canonical log mount", mainName)
}

// preserveLegacyCliOverride bridges the pre-Gen3 meaning of cliOverrides. operator-go <= v0.12
// replaced the entire container command, so existing Airflow CRs commonly start the slice with an
// executable. v0.13 correctly models flag-shaped values as arguments. Keep both forms during the
// migration: a leading flag is forwarded to the Airflow subcommand through $@, while a leading
// executable remains a full command replacement. Gen 2 also stored its startup script in Args,
// so an args-only main-container podOverride remains a bash-script replacement; new subcommand
// flags belong in cliOverrides. An explicit podOverrides command still wins both forms.
func preserveLegacyCliOverride(
	buildCtx *reconciler.RoleGroupBuildContext,
	sts *appsv1.StatefulSet,
) error {
	if buildCtx.MergedConfig == nil {
		return nil
	}

	mainName := buildCtx.Declaration.MainContainerName
	if mainName == "" {
		mainName = buildCtx.ResourceName
	}
	if podOverrides := buildCtx.MergedConfig.PodOverrides; podOverrides != nil {
		for i := range podOverrides.Spec.Containers {
			container := &podOverrides.Spec.Containers[i]
			if container.Name != mainName {
				continue
			}
			if container.Command != nil {
				return nil
			}
			if container.Args != nil {
				for j := range sts.Spec.Template.Spec.Containers {
					main := &sts.Spec.Template.Spec.Containers[j]
					if main.Name == mainName {
						main.Command = slices.Clone(bashEntrypoint)
						main.Args = slices.Clone(container.Args)
						return nil
					}
				}
				return fmt.Errorf("main container %q not found while applying podOverrides args", mainName)
			}
		}
	}

	if len(buildCtx.MergedConfig.CliArgs) == 0 ||
		strings.HasPrefix(buildCtx.MergedConfig.CliArgs[0], "-") {
		return nil
	}
	for i := range sts.Spec.Template.Spec.Containers {
		container := &sts.Spec.Template.Spec.Containers[i]
		if container.Name != mainName {
			continue
		}
		// A different command came from podOverrides, which is the highest-precedence user layer.
		if !slices.Equal(container.Command, buildCtx.Declaration.Command) {
			return nil
		}
		container.Command = slices.Clone(buildCtx.MergedConfig.CliArgs)
		container.Args = nil
		return nil
	}

	return fmt.Errorf("main container %q not found while applying cliOverrides", mainName)
}

// registerMetricsSidecar registers the statsd-exporter container on the SidecarManager so
// base.BuildResources injects it. It runs the product image's bundled exporter binary.
//
// The framework injects managed containers into InitContainers; RestartPolicy: Always makes it
// a native sidecar (stable since Kubernetes 1.33), which is the framework's idiom for a
// long-running companion — the pre-framework operator ran it as a regular container, which is
// the only rendered-placement change for this pod.
func (h *AirflowRoleGroupHandler) registerMetricsSidecar(buildCtx *reconciler.RoleGroupBuildContext) {
	args := BashLibs + `

prepare_signal_handlers

` + path.Join(constant.KubedoopRoot, "bin", "statsd-exporter") + `&
wait_for_termination $!`

	container := &corev1.Container{
		Name:            metricsSidecarName,
		Image:           buildCtx.ResolvedImage.Reference,
		ImagePullPolicy: buildCtx.ResolvedImage.PullPolicy,
		Command:         bashEntrypoint,
		Args:            []string{indentTabs4Spaces(args)},
		Ports: []corev1.ContainerPort{{
			Name: MetricsPortName, ContainerPort: MetricsPort, Protocol: corev1.ProtocolTCP,
		}},
		SecurityContext: sidecar.DefaultSecurityContext(),
		RestartPolicy:   sidecar.SidecarRestartPolicy(),
	}

	buildCtx.SidecarManager.Register(
		sidecar.NewStaticContainerProvider(*container),
		&sidecar.SidecarConfig{Enabled: true},
	)
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
	// Gen 2 always exposed this key through the role-group ConfigMap, using an empty file when
	// Vector was not configured. Keep it for existing podOverrides/custom sidecars that mount or
	// probe the path; its presence does not activate v0.13's collector pipeline.
	if _, exists := cm.Data["vector.yaml"]; !exists {
		cm.Data["vector.yaml"] = ""
	}
	for _, productOwnedFile := range []string{"webserver_config.py", "log_config.py"} {
		if _, exists := cm.Data[productOwnedFile]; exists {
			return fmt.Errorf(
				"configOverrides for product-owned file %q are unsupported; use the typed Airflow or logging fields instead",
				productOwnedFile,
			)
		}
	}

	webserverConfig, err := renderWebserverConfig(auth)
	if err != nil {
		return fmt.Errorf("failed to render webserver_config.py: %w", err)
	}
	cm.Data["webserver_config.py"] = webserverConfig
	if len(buildCtx.Declaration.LogProducers) != 1 {
		return fmt.Errorf("role %q must declare exactly one log producer", buildCtx.RoleName)
	}
	decl := buildCtx.Declaration.LogProducers[0]
	logFileTarget := buildCtx.LogFileTarget(decl)
	if logFileTarget == "" {
		// Gen 2 always wrote a bounded rolling file, whether or not a Vector collector was active.
		// The v0.13 accessor intentionally returns an empty target without Vector, so Airflow keeps
		// this product-specific compatibility policy locally while still using the framework path
		// convention and parser-compatible .py.json suffix.
		logFileTarget = path.Join(productlogging.LogDirFor(decl), decl.LogFileName)
	}
	cm.Data["log_config.py"] = renderLogging(
		buildCtx.ContainerLogging(decl.Container),
		logFileTarget,
	)
	return nil
}

// preserveLegacyStatefulSetIdentity lets a Gen 2 Airflow StatefulSet roll forward in place. The
// selector, serviceName and podManagementPolicy are immutable; operator-go preserves their live
// values during apply, but the desired pod template still has to match the frozen legacy selector
// or Kubernetes rejects the update. Preserve the live workload ServiceAccount too: Gen 2 used the
// namespace default unless podOverrides selected another one, and silently switching identities
// can drop registry credentials or cloud workload-identity annotations. The narrow fingerprint
// and owner check keep this shim from adopting an unrelated object that shares the derived name.
//
// A legacy selector necessarily keeps app.kubernetes.io/managed-by=airflow.kubedoop.dev on the
// replacement pods. operator-go v0.13's failing-pod scan selects managed-by=operator-go, so those
// upgraded pods are not included in that one health signal; StatefulSet availability/progress
// conditions remain active. Removing this limitation requires an explicit destructive StatefulSet
// replacement, not an in-place patch of its immutable selector.
func preserveLegacyStatefulSetIdentity(
	ctx context.Context,
	k8sClient client.Client,
	cr *airflowv1alpha1.AirflowCluster,
	buildCtx *reconciler.RoleGroupBuildContext,
	desired *appsv1.StatefulSet,
) (map[string]string, error) {
	live := &appsv1.StatefulSet{}
	err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: desired.Namespace,
		Name:      desired.Name,
	}, live)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read existing StatefulSet %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	if !metav1.IsControlledBy(live, cr) || live.Spec.Selector == nil {
		return nil, nil
	}

	legacyLabels := live.Spec.Selector.MatchLabels
	if legacyLabels["app.kubernetes.io/name"] != legacyAppName ||
		legacyLabels["app.kubernetes.io/managed-by"] != LabelDomain ||
		legacyLabels["app.kubernetes.io/instance"] != cr.Name ||
		legacyLabels["app.kubernetes.io/component"] != buildCtx.RoleName ||
		legacyLabels["app.kubernetes.io/role-group"] != buildCtx.RoleGroupName {
		return nil, nil
	}

	desired.Spec.Selector = live.Spec.Selector.DeepCopy()
	desired.Spec.ServiceName = live.Spec.ServiceName
	desired.Spec.PodManagementPolicy = live.Spec.PodManagementPolicy
	// Preserve the Gen 2 workload identity by default, but do not turn compatibility into a
	// permanent lock: a later explicit non-empty podOverride is the user's migration escape hatch
	// and remains the highest-precedence layer.
	if buildCtx.MergedConfig == nil || buildCtx.MergedConfig.PodOverrides == nil ||
		buildCtx.MergedConfig.PodOverrides.Spec.ServiceAccountName == "" {
		desired.Spec.Template.Spec.ServiceAccountName = live.Spec.Template.Spec.ServiceAccountName
	}
	if desired.Spec.Template.Labels == nil {
		desired.Spec.Template.Labels = map[string]string{}
	}
	for key, value := range live.Spec.Selector.MatchLabels {
		desired.Spec.Template.Labels[key] = value
	}

	selector, err := metav1.LabelSelectorAsSelector(live.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("parse legacy StatefulSet selector for %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	if !selector.Matches(labels.Set(desired.Spec.Template.Labels)) {
		return nil, fmt.Errorf(
			"legacy StatefulSet selector for %s/%s cannot match the migrated pod template",
			desired.Namespace,
			desired.Name,
		)
	}

	return maps.Clone(live.Spec.Selector.MatchLabels), nil
}

// mainContainerScript renders the role's bash entrypoint: workspace preparation, signal-handler
// plumbing, the role command backgrounded, and a wait that forwards SIGTERM.
func mainContainerScript(roleName string) (string, error) {
	var mainCommand string

	switch airflowv1alpha1.RoleName(roleName) {
	case airflowv1alpha1.WebserversRoleName:
		mainCommand = `airflow webserver "$@" &`
	case airflowv1alpha1.SchedulersRoleName:
		mainCommand = `
airflow db init
airflow db upgrade
set +x	# disable xtrace
if ! airflow users create \
	--username $` + EnvKeyAdminUserName + ` \
	--firstname $` + ENVKeyAdminFirstName + ` \
	--lastname $` + EnvKeyAdminLastName + ` \
	--email $` + EnvKeyAdminEmail + ` \
	--password $` + EnvKeyAdminPassword + ` \
	--role "Admin"; then
	# A scheduler Pod restart must not become a permanent CrashLoop merely because bootstrap
	# already succeeded. Only tolerate create failure when the requested user is actually present;
	# database/CLI failures still make the container fail under pipefail.
	airflow users list -o json | python -c '
import json
import sys

username = sys.argv[1]
users = json.load(sys.stdin)
if not any(user.get("username") == username for user in users):
    raise SystemExit("admin user creation failed and the user does not exist")
' "$` + EnvKeyAdminUserName + `"
fi

set -x 	# enable xtrace

airflow scheduler "$@" &
`
	case airflowv1alpha1.CeleryExecutorsRoleName:
		mainCommand = `airflow celery worker "$@" &`
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
// envOverrides in the CRD always win. Secret-backed values are declared in DeclareRoles because
// the map-shaped contribution cannot carry valueFrom.
func ComputeProductConfig(
	_ context.Context,
	_ client.Client,
	cr *airflowv1alpha1.AirflowCluster,
	_ *reconciler.RoleGroupBuildContext,
) (*reconciler.Contribution, error) {
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

	return &reconciler.Contribution{
		EnvVars: map[string]string{
			"PYTHONPATH":                             strings.Join([]string{AppConfigPath, dagsFolder}, ":"),
			"AIRFLOW__CORE__DAGS_FOLDER":             dagsFolder,
			"AIRFLOW__LOGGING__LOGGING_CONFIG_CLASS": "log_config.LOGGING_CONFIG",
			"AIRFLOW__METRICS__STATSD_ON":            pythonTrue,
			"AIRFLOW__METRICS__STATSD_HOST":          "0.0.0.0",
			"AIRFLOW__METRICS__STATSD_PORT":          "8125",
			"AIRFLOW__API__AUTH_BACKENDS":            "airflow.api.auth.backend.basic_auth",
			"AIRFLOW__CORE__LOAD_EXAMPLES":           strconv.FormatBool(loadExamples),
			"AIRFLOW__WEBSERVER__EXPOSE_CONFIG":      strconv.FormatBool(exposeConfig),
			"AIRFLOW__CORE__EXECUTOR":                executorCelery,
		},
	}, nil
}
