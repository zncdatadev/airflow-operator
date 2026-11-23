package controller

import (
	"path"
	"regexp"
	"strings"

	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
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

// renderLogging renders Airflow's product-owned log_config.py. A non-empty target enables the
// rolling JSON appender. The role-group handler always supplies one to preserve Airflow's Gen 2
// on-disk logging contract; Vector independently controls whether those files are collected.
func renderLogging(logConfig *commonsv1alpha1.LoggingConfigSpec, logFileTarget string) string {
	fileLogLevel := DefaultLogLevel
	consoleLogLevel := DefaultLogLevel
	rootLogLevel := DefaultLogLevel

	if logConfig != nil {
		if logConfig.File != nil && logConfig.File.Level != "" {
			fileLogLevel = logConfig.File.Level
		}
		if logConfig.Console != nil && logConfig.Console.Level != "" {
			consoleLogLevel = logConfig.Console.Level
		}
		if rootLogger, ok := logConfig.Loggers["root"]; ok && rootLogger != nil && rootLogger.Level != "" {
			rootLogLevel = rootLogger.Level
		}
	}

	cfg := `
import logging
from copy import deepcopy
from logging.config import dictConfig

from airflow.config_templates.airflow_local_settings import DEFAULT_LOGGING_CONFIG

LOGGING_CONFIG = deepcopy(DEFAULT_LOGGING_CONFIG)

LOGGING_CONFIG.setdefault('loggers', {})
for logger_name, logger_config in LOGGING_CONFIG['loggers'].items():
	logger_config['level'] = logging.NOTSET
	# Do not change the setting of the airflow.task logger because
	# otherwise DAGs cannot be loaded anymore.
	if logger_name != 'airflow.task':
		logger_config['propagate'] = True

LOGGING_CONFIG.setdefault('handlers', {})
LOGGING_CONFIG['handlers'].setdefault('console', {})
LOGGING_CONFIG['handlers']['console']['level'] = '` + consoleLogLevel + `'
`

	rootHandlers := "'console'"
	if logFileTarget != "" {
		cfg += `
LOGDIR = '` + path.Dir(logFileTarget) + `'

os.makedirs(LOGDIR, exist_ok=True)

LOGGING_CONFIG.setdefault('formatters', {})
LOGGING_CONFIG['formatters']['json'] = {
	'()': 'airflow.utils.log.json_formatter.JSONFormatter',
	'json_fields': ['asctime', 'levelname', 'message', 'name']
}

LOGGING_CONFIG['handlers']['file'] = {
	'class': 'logging.handlers.RotatingFileHandler',
	'formatter': 'json',
	'level': '` + fileLogLevel + `',
	'filename': '` + logFileTarget + `',
	'maxBytes': 10485760,
	'backupCount': 5
}
`
		// os is needed only on the file-appender path. Keeping the import conditional makes the
		// console-only result accurately describe that it has no filesystem side effects.
		cfg = strings.Replace(cfg, "import logging\n", "import logging\nimport os\n", 1)
		rootHandlers = "'console', 'file'"
	}

	cfg += `
LOGGING_CONFIG['root'] = {
	'level': '` + rootLogLevel + `',
	'handlers': [` + rootHandlers + `],
	'filters': ['mask_secrets']
}
`

	return indentTabs4Spaces(cfg)
}
