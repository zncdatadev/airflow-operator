package controller

import (
	"path"
	"regexp"
	"strings"

	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/constant"
)

const DefaultLogLevel = "INFO"

// reLeadingTabs matches a run of tabs at the start of the string or of a line.
var reLeadingTabs = regexp.MustCompile(`(^|\n)\t+`)

// indentTabs4Spaces converts leading tabs to four spaces each, matching the pre-framework
// util.IndentTab4Spaces the rendered scripts and config files were built with.
func indentTabs4Spaces(code string) string {
	return reLeadingTabs.ReplaceAllStringFunc(code, func(match string) string {
		if strings.HasPrefix(match, "\n") {
			return "\n" + strings.Repeat("    ", len(match)-1)
		}
		return strings.Repeat("    ", len(match))
	})
}

// renderWebserverConfig renders webserver_config.py: the FAB boilerplate plus the auth section
// when the cluster configures AuthenticationClasses.
func renderWebserverConfig(auth *Authentication) (string, error) {
	cfg := `
import os

from flask_appbuilder.const import (AUTH_DB, AUTH_LDAP, AUTH_OAUTH, AUTH_OID, AUTH_REMOTE_USER)


baseDir = os.path.abspath(os.path.dirname(__file__))

WTF_CSRF_ENABLED = False

`

	if auth != nil {
		authCfg, err := auth.GetConfig()
		if err != nil {
			return "", err
		}
		cfg += authCfg
	}
	return indentTabs4Spaces(cfg), nil
}

// renderLogging renders log_config.py: Airflow's DEFAULT_LOGGING_CONFIG deep-copied and extended
// with a JSON rotating file handler, at the levels the role group's logging config declares.
func renderLogging(roleName string, rgConfig *commonsv1alpha1.RoleGroupConfigSpec) string {
	fileLogLevel := DefaultLogLevel
	consoleLogLevel := DefaultLogLevel
	logFile := path.Join(constant.KubedoopLogDir, roleName, "airflow.log.json")

	rootLogLevel := DefaultLogLevel

	if rgConfig != nil && rgConfig.Logging != nil {
		logConfig, ok := rgConfig.Logging.Containers[roleName]
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

	logDir := path.Join(constant.KubedoopLogDir, roleName)
	cfg := `
import logging
import os
from copy import deepcopy
from logging.config import dictConfig

from airflow.config_templates.airflow_local_settings import DEFAULT_LOGGING_CONFIG

LOGDIR = '` + logDir + `'

os.makedirs(LOGDIR, exist_ok=True)

LOGGING_CONFIG = deepcopy(DEFAULT_LOGGING_CONFIG)

LOGGING_CONFIG.setdefault('loggers', {})
for logger_name, logger_config in LOGGING_CONFIG['loggers'].items():
	logger_config['level'] = logging.NOTSET
	# Do not change the setting of the airflow.task logger because
	# otherwise DAGs cannot be loaded anymore.
	if logger_name != 'airflow.task':
		logger_config['propagate'] == True

LOGGING_CONFIG.setdefault('formatters', {})
LOGGING_CONFIG['formatters']['json'] = {
	'()': 'airflow.utils.log.json_formatter.JSONFormatter',
	'json_fields': ['asctime', 'levelname', 'message', 'name']
}

LOGGING_CONFIG.setdefault('handlers', {})
LOGGING_CONFIG['handlers'].setdefault('console', {})
LOGGING_CONFIG['handlers']['console']['level'] = '` + consoleLogLevel + `'
LOGGING_CONFIG['handlers']['file'] = {
	'class': 'logging.handlers.RotatingFileHandler',
	'formatter': 'json',
	'level': '` + fileLogLevel + `',
	'filename': '` + logFile + `',
	'maxBytes': 10485760,
	'backupCount': 5
}

LOGGING_CONFIG['root'] = {
	'level': '` + rootLogLevel + `',
	'handlers': ['console', 'file'],
	'filters': ['mask_secrets']
}
`

	return indentTabs4Spaces(cfg)
}
