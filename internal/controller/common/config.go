package commons

import (
	"context"
	"path"

	"github.com/zncdatadev/operator-go/pkg/builder"
	"github.com/zncdatadev/operator-go/pkg/client"
	"github.com/zncdatadev/operator-go/pkg/constants"
	"github.com/zncdatadev/operator-go/pkg/productlogging"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/util"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
)

var _ builder.ConfigBuilder = &ConfigMapBuilder{}

func NewConfigReconciler(
	client *client.Client,
	clusterConfig *airflowv1alpha1.ClusterConfigSpec,
	roleGroupConfig *airflowv1alpha1.ConfigSpec,
	roleGroupInfo reconciler.RoleGroupInfo,
	auth *Authentication,
	options ...builder.Option,
) *reconciler.SimpleResourceReconciler[builder.ConfigBuilder] {

	supersetConfigSecretBuilder := NewConfigMapBuilder(
		client,
		roleGroupInfo.GetFullName(),
		clusterConfig,
		roleGroupConfig,
		auth,
		options...,
	)
	return reconciler.NewSimpleResourceReconciler[builder.ConfigBuilder](
		client,
		supersetConfigSecretBuilder,
	)
}

// ConfigMapBuilder is an implementation of ConfigMapBuilder
type ConfigMapBuilder struct {
	builder.ConfigMapBuilder

	ClusterConfig   *airflowv1alpha1.ClusterConfigSpec
	RoleGroupConfig *airflowv1alpha1.ConfigSpec
	Auth            *Authentication
}

// NewConfigMapBuilder returns a new ConfigMapBuilder
func NewConfigMapBuilder(
	client *client.Client,
	name string,
	clusterConfig *airflowv1alpha1.ClusterConfigSpec,
	roleGroupConfig *airflowv1alpha1.ConfigSpec,
	auth *Authentication,
	options ...builder.Option,
) *ConfigMapBuilder {
	return &ConfigMapBuilder{
		ConfigMapBuilder: *builder.NewConfigMapBuilder(
			client,
			name,
			options...,
		),
		ClusterConfig:   clusterConfig,
		RoleGroupConfig: roleGroupConfig,
	}
}

func (b *ConfigMapBuilder) Build(ctx context.Context) (ctrlclient.Object, error) {

	airflowConfig, err := b.getAirflowConfig()
	if err != nil {
		return nil, err
	}

	loggingConfig, err := b.getLogging()
	if err != nil {
		return nil, err
	}

	vectorConfig, err := b.getVector(ctx)
	if err != nil {
		return nil, err
	}

	b.AddItem("webserver_config.py", airflowConfig)
	b.AddItem("log_config.py", loggingConfig)
	b.AddItem("vector.yaml", vectorConfig)

	return b.GetObject(), nil
}

func (b *ConfigMapBuilder) getLogging() (string, error) {
	fileLogLevel := "INFO"
	consoleLogLevel := "INFO"
	logFile := path.Join(constants.KubedoopLogDir, b.RoleName, "airflow.log.json")

	rootLogLevel := "INFO"

	if b.RoleGroupConfig.Logging != nil {
		logConfig, ok := b.RoleGroupConfig.Logging.Containers[b.RoleName]
		if ok {
			if logConfig.File != nil {
				fileLogLevel = logConfig.File.Level
			}

			if logConfig.Console != nil {
				consoleLogLevel = logConfig.Console.Level
			}

			if rootLogger, ok := logConfig.Loggers["root"]; ok {
				rootLogLevel = rootLogger.Level
			}

		}
	}

	logDir := path.Join(constants.KubedoopLogDir, b.RoleName)
	cfg := `
import logging
import os
from copy import deepcopy
from logging.config import dictConfig

from airflow.config_templates.airflow_local_settings import DEFAULT_LOGGING_CONFIG

LOGDIR = ` + logDir + `

os.makedirs(LOGDIR, exist_ok=True)

LOGGING_CONFIG = deepcopy(DEFAULT_LOGGING_CONFIG)

LOGGING_CONFIG.setdefault'loggers', {})
for logger_name, logger_config in LOGGING_CONFIG['loggers'].items():
	logger_config['level'] = logging.NOTSET
	# Do not change the setting of the airflow.task logger because
	# otherwise DAGs cannot be loaded anymore.
	if logger_name != 'airflow.task':
		logger_config['propagate'] == True

LOGGING_CONFIG.setdefault'formatters', {})
LOGGING_CONFIG['formatters']['json'] = {
	'()': 'airflow.utils.log.json_formatter.JSONFormatter',
	'json_fields': ['asctime', 'levelname', 'message', 'name']
}

LOGGING_CONFIG.setdefault'handlers', {})
LOGGING_CONFIG['handlers'].setdefault('console', {})
LOGGING_CONFIG['handlers']['console']['level'] = ` + consoleLogLevel + `
LOGGING_CONFIG['handlers']['file'] = {
	'class': 'logging.handlers.RotatingFileHandler',
	'formatter': 'json',
	'level': ` + fileLogLevel + `,
	'filename': ` + logFile + `,
	'maxBytes': 1048576,
	'backupCount': 5
}

LOGGING_CONFIG['root'] = {
	'level': ` + rootLogLevel + `,
	'handlers': ['console', 'file']
	'filters': ['mask_secrets']
}
`

	return util.IndentTab4Spaces(cfg), nil
}

func (b *ConfigMapBuilder) getAirflowConfig() (string, error) {

	authCfg, err := b.Auth.GetConfig()
	if err != nil {
		return "", err
	}

	cfg := `
import os

from flask_appbuilder.const import (AUTH_DB, AUTH_LDAP, AUTH_OAUTH, AUTH_OID, AUTH_REMOTE_USER)


baseDir = os.path.abspath(os.path.dirname(__file__))

WTF_CSRF_ENABLED = False

` + authCfg + `
`

	return util.IndentTab4Spaces(cfg), nil
}

func (b *ConfigMapBuilder) getVector(ctx context.Context) (string, error) {
	if b.ClusterConfig != nil && b.ClusterConfig.VectorAggregatorConfigMapName != "" {
		s, err := productlogging.MakeVectorYaml(
			ctx,
			b.Client.Client,
			b.Client.GetOwnerNamespace(),
			b.ClusterName,
			b.RoleName,
			b.RoleGroupName,
			b.ClusterConfig.VectorAggregatorConfigMapName,
		)
		if err != nil {
			return "", err
		}
		return s, nil
	}

	return "", nil
}
