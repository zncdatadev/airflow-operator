package commons

import (
	"fmt"
	"path"
	"strconv"

	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/builder"
	"github.com/zncdatadev/operator-go/pkg/client"
	"github.com/zncdatadev/operator-go/pkg/constants"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/util"
	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
)

var (
	AppPath = path.Join(constants.KubedoopRoot, "airflow")

	KubernetesExecutorPodTemplateFileName = "airflow_executor_pod_template.yaml"
	KubernetesExecutorPodTemplatePath     = path.Join(AppPath, "template")
)

const (
	EnvKeyAdminUserName  = "ADMIN_USERNAME"
	ENVKeyAdminFirstName = "ADMIN_FIRSTNAME"
	EnvKeyAdminLastName  = "ADMIN_LASTNAME"
	EnvKeyAdminEmail     = "ADMIN_EMAIL"
	EnvKeyAdminPassword  = "ADMIN_PASSWORD"
)

const (
	LogVolumeMountName    = "log"
	ConfigVolumeMountName = "config"
)

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

func NewStatefulSetReconciler(
	client *client.Client,
	roleGroupInfo reconciler.RoleGroupInfo,
	clusterConfig *airflowv1alpha1.ClusterConfigSpec,
	ports []corev1.ContainerPort,
	image *util.Image,
	replicas *int32,
	stopped bool,
	overrides *commonsv1alpha1.OverridesSpec,
	roleGroupConfig *commonsv1alpha1.RoleGroupConfigSpec,
	executor ExecutorType,
	auth *Authentication,
	options ...builder.Option,
) (*reconciler.StatefulSet, error) {

	b := NewStatefulSetBuilder(
		client,
		roleGroupInfo.GetFullName(),
		clusterConfig,
		replicas,
		image,
		ports,
		overrides,
		roleGroupConfig,
		executor,
		auth,
		options...,
	)

	return reconciler.NewStatefulSet(
		client,
		b,
		stopped,
	), nil
}

var _ builder.StatefulSetBuilder = &StatefulSetBuilder{}

// StatefulSetBuilder is an implementation of StatefulSetBuilder
type StatefulSetBuilder struct {
	builder.StatefulSet
	ClusterConfig *airflowv1alpha1.ClusterConfigSpec
	Executor      ExecutorType
	Auth          *Authentication
}

// NewStatefulSetBuilder returns a new StatefulSetBuilder
func NewStatefulSetBuilder(
	client *client.Client,
	name string,
	clusterConfig *airflowv1alpha1.ClusterConfigSpec,
	replicas *int32,
	image *util.Image,
	ports []corev1.ContainerPort,
	overrides *commonsv1alpha1.OverridesSpec,
	roleGroupConfig *commonsv1alpha1.RoleGroupConfigSpec,
	executor ExecutorType,
	auth *Authentication,
	options ...builder.Option,
) *StatefulSetBuilder {
	return &StatefulSetBuilder{
		StatefulSet: *builder.NewStatefulSetBuilder(
			client,
			name,
			replicas,
			image,
			overrides,
			roleGroupConfig,
			options...,
		),
		ClusterConfig: clusterConfig,
		Executor:      executor,
	}
}

func (b *StatefulSetBuilder) Build(ctx context.Context) (ctrlclient.Object, error) {
	cb, err := b.getMainContainer()
	if err != nil {
		return nil, err
	}
	mc := b.getMetricContainer()
	b.AddContainer(cb.Build())
	b.AddContainer(mc.Build())
	b.AddVolume(
		&corev1.Volume{
			Name: ConfigVolumeMountName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					DefaultMode:          &[]int32{420}[0],
					LocalObjectReference: corev1.LocalObjectReference{Name: b.Name},
				},
			},
		},
	)

	obj, err := b.GetObject()
	if err != nil {
		return nil, err
	}

	if b.ClusterConfig != nil && b.ClusterConfig.VectorAggregatorConfigMapName != "" {
		builder.NewVectorDecorator(
			obj,
			b.GetImage(),
			LogVolumeMountName,
			"vector",
			b.Name,
		)
	}

	return obj, nil
}

func (b *StatefulSetBuilder) getMainContainerArgs() (string, error) {

	var mainCommand string

	switch airflowv1alpha1.RoleName(b.RoleName) {
	case airflowv1alpha1.WebserversRoleName:
		mainCommand = "airflow webserver &"
	case airflowv1alpha1.SchedulersRoleName:
		mainCommand = `
airflow db init
airflow db upgrade
set +x	# disable xtrace
airflow users create \
	--username $` + EnvKeyAdminUserName + `
	--firstname $` + ENVKeyAdminFirstName + `
	--lastname $` + EnvKeyAdminLastName + `
	--email $` + EnvKeyAdminEmail + `
	--password $` + EnvKeyAdminPassword + `
	--role "Admin"
set -x 	# enable xtrace

airflow scheduler &
		`
	case airflowv1alpha1.WorkersRoleName:
		mainCommand = "airflow celery worker &"

	default:
		return "", fmt.Errorf("unsupported role %s", b.RoleName)
	}

	args := `
cp -RL ` + constants.KubedoopConfigDirMount + ` ` + AppPath + `

` + BashLibs + `

prepare_signal_handlers
rm -rf ` + builder.VectorShutdownFile + `

` + mainCommand + `
wait_for_termination $!
mkdir -p ` + builder.VectorWatcherDir + ` && touch ` + builder.VectorShutdownFile + `
`

	return util.IndentTab4Spaces(args), nil
}

func (b *StatefulSetBuilder) setMainContainerEnv() ([]corev1.EnvVar, error) {
	credentialsName := b.ClusterConfig.CrdentialsSecret
	if credentialsName == "" {
		return nil, fmt.Errorf("credentials secret name in cluster config is empty")
	}

	DagFloder := path.Join(constants.KubedoopRoot, "airflow", "dags")

	if len(b.ClusterConfig.DagsGitSync) > 0 {
		dag := b.ClusterConfig.DagsGitSync[0]
		if dag.GitFolder != "" {
			DagFloder = path.Join(DagFloder, dag.GitFolder)
		}
	}

	var envs = []corev1.EnvVar{
		{
			Name:  "PYTHONPATH",
			Value: DagFloder,
		},
		{
			Name:  "AIRFLOW__CORE__DAGS_FOLDER",
			Value: DagFloder,
		},
		{
			Name:  "AIRFLOW__LOGGING__LOGGING_CONFIG_CLASS",
			Value: "log_config.LOGGING_CONFIG",
		},
		{
			Name:  "AIRFLOW__METRICS__STATSD_ON",
			Value: "True",
		},
		{
			Name:  "AIRFLOW__METRICS__STATSD_HOST",
			Value: "0.0.0.0",
		},
		{
			Name:  "AIRFLOW__METRICS__STATSD_PORT",
			Value: "8125",
		},
		{
			Name:  "AIRFLOW__API__AUTH_BACKEND",
			Value: "airflow.api.auth.backend.basic_auth",
		},

		{
			Name: "AIRFLOW__WEBSERVER__SECRET_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key: "connections.secretKey",
					LocalObjectReference: corev1.LocalObjectReference{
						Name: credentialsName,
					},
				},
			},
		},
		{
			Name: "AIRFLOW__CORE__SQL_ALCHEMY_CONN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key: "connections.sqlalchemyDatabaseUri",
					LocalObjectReference: corev1.LocalObjectReference{
						Name: credentialsName,
					},
				},
			},
		},

		{
			Name:  "AIRFLOW__CORE__LOAD_EXAMPLES",
			Value: strconv.FormatBool(b.ClusterConfig.LoadExamples),
		},
		{
			Name:  "AIRFLOW__WEBSERVER__EXPOSE_CONFIG",
			Value: strconv.FormatBool(b.ClusterConfig.ExposeConfig),
		},
		{
			Name:  "AIRFLOW__CORE__EXECUTOR",
			Value: string(b.Executor),
		},
	}
	if b.Executor == CeleryExecutor {
		envs = append(envs,
			corev1.EnvVar{
				Name:  "AIRFLOW__CELERY__RESULT_BACKEND",
				Value: "connections.celeryResultBackend",
			},
			corev1.EnvVar{
				Name:  "AIRFLOW__CELERY__BROKER_URL",
				Value: "connections.celeryBrokerUrl",
			},
		)
	}
	// TODO: add support for KubernetesExecutor to pod template
	// if b.Executor == KubernetesExecutor {
	// 	envs = append(envs,
	// 		corev1.EnvVar{
	// 			Name:  "AIRFLOW__KUBERNETES_EXECUTOR__POD_TEMPLATE_FILE",
	// 			Value: path.Join(KubernetesExecutorPodTemplatePath, KubernetesExecutorPodTemplateFileName),
	// 		},
	// 		corev1.EnvVar{
	// 			Name:  "AIRFLOW__KUBERNETES_EXECUTOR__NAMESPACE",
	// 			Value: b.GetObjectMeta().Namespace,
	// 		},
	// 		corev1.EnvVar{
	// 			Name:  "AIRFLOW__CORE__EXECUTOR",
	// 			Value: string(LocalExecutor),
	// 		},
	// 	)
	// }

	if b.RoleGroupName == string(airflowv1alpha1.SchedulersRoleName) {
		envKeyMapping := [][]string{
			{EnvKeyAdminUserName, "adminUser.username"},
			{ENVKeyAdminFirstName, "adminUser.firstusername"},
			{EnvKeyAdminLastName, "adminUser.lastname"},
			{EnvKeyAdminEmail, "adminUser.email"},
			{EnvKeyAdminPassword, "adminUser.password"},
		}

		for _, mapping := range envKeyMapping {
			envs = append(envs, corev1.EnvVar{
				Name: mapping[0],
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						Key: mapping[1],
						LocalObjectReference: corev1.LocalObjectReference{
							Name: credentialsName,
						},
					},
				},
			})
		}
	}

	if b.RoleGroupName == string(airflowv1alpha1.WebserversRoleName) && b.Auth != nil {
		envs = append(envs, b.Auth.GetEnvVars()...)
	}

	return envs, nil
}

func (b *StatefulSetBuilder) getMainContainerVolumeMount() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{
			Name:      ConfigVolumeMountName,
			MountPath: constants.KubedoopConfigDirMount,
		},
	}
}

func (b *StatefulSetBuilder) getMainContainer() (builder.ContainerBuilder, error) {
	container := builder.NewContainer(b.RoleName, b.Image)
	container.SetCommand([]string{"/bin/bash", "-x", "-euo", "pipefail", "-c"})
	args, err := b.getMainContainerArgs()
	if err != nil {
		return nil, err
	}
	container.SetArgs([]string{args})
	envs, err := b.setMainContainerEnv()
	if err != nil {
		return nil, err
	}
	container.AddEnvVars(envs)

	container.AddVolumeMounts(b.getMainContainerVolumeMount())

	return container, nil
}

func (b *StatefulSetBuilder) getMetricContainer() builder.ContainerBuilder {
	container := builder.NewContainer(b.RoleName, b.Image)
	container.SetCommand([]string{"/bin/bash", "-x", "-euo", "pipefail", "-c"})
	args := `

` + BashLibs + `

prepare_signal_handlers

` + path.Join(constants.KubedoopRoot, "bin", "statsd_exporter") + `& 
wait_for_termination $!
`

	container.SetArgs([]string{util.IndentTab4Spaces(args)})
	return container
}
