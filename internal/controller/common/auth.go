package commons

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	authv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/authentication/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/builder"
	"github.com/zncdatadev/operator-go/pkg/client"
	"github.com/zncdatadev/operator-go/pkg/config"
	"github.com/zncdatadev/operator-go/pkg/config/properties"
	"github.com/zncdatadev/operator-go/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
)

type AuthenticatorType string

const (
	AuthenticatorTypeLDAP AuthenticatorType = "ldap"
	AuthenticatorTypeOIDC AuthenticatorType = "oidc"
)

var (
	AirflowSupportAuthTypes = []AuthenticatorType{AuthenticatorTypeLDAP, AuthenticatorTypeOIDC}
)

const (
	DefaultLDAPFieldEmail     = "email"
	DefaultLDAPFieldGivenName = "givenName"
	DefaultLDAPFieldGroup     = "memberOf"
	DefaultLDAPFieldSurname   = "sn"
	DefaultLDAPFieldUid       = "uid"
)

const (
	EnvKeyOidcClientId     = "OIDC_CLIENT_ID"
	EnvKeyOidcClientSecret = "OIDC_CLIENT_SECRET"
)

var authLogger = ctrl.Log.WithName("authenticator")

type Authenticator interface {
	GetEnvVars() []corev1.EnvVar
	GetVolumes() []corev1.Volume
	GetVolumeMounts() []corev1.VolumeMount
	GetConfig() *properties.Properties
	GetCommands() []string
}

func GetAuthProvider(ctx context.Context, client *client.Client, authclass string) (*authv1alpha1.AuthenticationProvider, error) {
	obj := &authv1alpha1.AuthenticationClass{}
	if err := client.Get(ctx, ctrlclient.ObjectKey{Name: authclass}, obj); err != nil {
		if ctrlclient.IgnoreNotFound(err) != nil {
			return nil, err
		}
		authLogger.Info("AuthenticationClass not found", "name", authclass)
	}
	return obj.Spec.AuthenticationProvider, nil
}

type Authentication struct {
	authenticators       map[AuthenticatorType][]Authenticator
	syncRolesAt          *string
	userRegistration     *bool
	userRegistrationRole *string
}

func containsAuthType(authTypes []AuthenticatorType, authType AuthenticatorType) bool {
	for _, at := range authTypes {
		if at == authType {
			return true
		}
	}
	return false
}

func NewAuthentication(
	ctx context.Context,
	client *client.Client,
	auths []airflowv1alpha1.AuthenticationSpec,
) (*Authentication, error) {
	authenticators := make(map[AuthenticatorType][]Authenticator)
	var syncRolesAt *string
	var userRegistration *bool
	var userRegistrationRole *string
	for _, auth := range auths {
		provider, err := GetAuthProvider(ctx, client, auth.AuthenticationClass)
		if err != nil {
			return nil, err
		}

		if provider.OIDC != nil && containsAuthType(AirflowSupportAuthTypes, AuthenticatorTypeOIDC) {
			oidcAuth := &oidcAuthenticator{config: auth.Oidc, provider: provider.OIDC}
			authenticators[AuthenticatorTypeOIDC] = append(authenticators[AuthenticatorTypeOIDC], oidcAuth)
		} else if provider.LDAP != nil && containsAuthType(AirflowSupportAuthTypes, AuthenticatorTypeLDAP) {
			ldapAuth := &ldapAuthenticator{provider: provider.LDAP}
			authenticators[AuthenticatorTypeLDAP] = append(authenticators[AuthenticatorTypeLDAP], ldapAuth)
		} else {
			return nil, fmt.Errorf("unsupported authentication provider: %s", auth.AuthenticationClass)
		}

		if syncRolesAt == nil {
			syncRolesAt = &auth.SyncRolesAt
		} else if *syncRolesAt != auth.SyncRolesAt {
			return nil, fmt.Errorf("syncRolesAt must be the same for all authentication providers")
		}
		if userRegistration == nil {
			userRegistration = &auth.UserRegistration
		} else if *userRegistration != auth.UserRegistration {
			return nil, fmt.Errorf("userRegistration must be the same for all authentication providers")
		}
		if userRegistrationRole == nil {
			userRegistrationRole = &auth.UserRegistrationRole
		} else if *userRegistrationRole != auth.UserRegistrationRole {
			return nil, fmt.Errorf("userRegistrationRole must be the same for all authentication providers")
		}
	}

	return &Authentication{
		authenticators:       authenticators,
		syncRolesAt:          syncRolesAt,
		userRegistration:     userRegistration,
		userRegistrationRole: userRegistrationRole,
	}, nil
}

func (a *Authentication) GetEnvVars() []corev1.EnvVar {
	envVars := make([]corev1.EnvVar, 0)
	for _, typedAuthenticator := range a.authenticators {
		for _, authenticator := range typedAuthenticator {
			envVars = append(envVars, authenticator.GetEnvVars()...)
		}
	}
	return envVars
}

func (a *Authentication) GetVolumes() []corev1.Volume {
	var volumes []corev1.Volume
	for _, typedAuthenticator := range a.authenticators {
		for _, authenticator := range typedAuthenticator {
			volumes = append(volumes, authenticator.GetVolumes()...)
		}
	}
	return volumes
}

func (a *Authentication) GetVolumeMounts() []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	for _, typedAuthenticator := range a.authenticators {
		for _, authenticator := range typedAuthenticator {
			mounts = append(mounts, authenticator.GetVolumeMounts()...)
		}
	}
	return mounts
}

func (a *Authentication) getAuthDBConfig() string {
	return "AUTH_TYPE = 'AUTH_DB'"
}

func (a *Authentication) getOidcConfig() (string, error) {
	data := make(map[string]interface{})
	for authType, typedAuthenticator := range a.authenticators {
		providerData := make(map[string]string)
		if authType == AuthenticatorTypeOIDC {
			for _, authenticator := range typedAuthenticator {
				cfg := authenticator.GetConfig()
				for _, k := range cfg.Keys() {
					v, _ := cfg.Get(k)
					providerData[k] = v
				}
			}
			data["providers"] = providerData
		}
	}
	data["auth_type"] = "AUTH_OAUTH"
	data["auth_roles_sync_at_login"] = a.syncRolesAt
	data["user_registration"] = a.userRegistration
	data["user_registration_role"] = a.userRegistrationRole

	tpl := `
AUTH_TYPE = '{{ .auth_type }}'

{{- if .auth_roles_sync_at_login }}
AUTH_ROLES_SYNC_AT_LOGIN = '{{ .auth_roles_sync_at_login }}'
{{- end }}
{{- if .user_registration }}
AUTH_USER_REGISTRATION = {{ .user_registration }}
{{- end }}
{{- if .user_registration_role }}
AUTH_USER_REGISTRATION_ROLE = '{{ .user_registration_role }}'
{{- end }}

OAUTH_PROVIDERS = [
{{- range .providers }}
					{
						'name': 'keycloak',
						'token_key': 'access_token',
						'remote_app': {
							'client_id': {{ .client_id }},
							'client_secret': {{ .client_secret }},
							'client_kwargs': {
								'scope': {{ .scopes }},
							},
							'api_base_url': {{ .api_base_url }},
							'server_metadata_url': {{ .server_metadata_url }},
						}
					},
{{ end }}
				]
`

	t := config.TemplateParser{Template: tpl, Value: data}
	return t.Parse()
}

func (a *Authentication) getLdapConfig() (string, error) {
	data := make(map[string]interface{})
	exist := false
	for authType, typedAuthenticator := range a.authenticators {
		if authType == AuthenticatorTypeLDAP {
			if exist {
				authLogger.Info("Multiple LDAP authenticators found, using the first one")
				continue
			}
			for _, authenticator := range typedAuthenticator {
				cfg := authenticator.GetConfig()
				for _, k := range cfg.Keys() {
					v, _ := cfg.Get(k)
					data[k] = v
				}
			}
			exist = true
		}
	}
	data["auth_type"] = "AUTH_LDAP"
	data["auth_roles_sync_at_login"] = a.syncRolesAt
	data["user_registration"] = a.userRegistration
	data["user_registration_role"] = a.userRegistrationRole

	tpl := `
AUTH_TYPE = '{{ .auth_type }}'

{{- if .auth_roles_sync_at_login }}
AUTH_ROLES_SYNC_AT_LOGIN = '{{ .auth_roles_sync_at_login }}'
{{- end }}
{{- if .user_registration }}
AUTH_USER_REGISTRATION = {{ .user_registration }}
{{- end }}
{{- if .user_registration_role }}
AUTH_USER_REGISTRATION_ROLE = '{{ .user_registration_role }}'
{{- end }}

AUTH_LDAP_SERVER = '{{ .auth_ldap_server }}'
AUTH_LDAP_SEARCH = '{{ .auth_ldap_search }}'
AUTH_LDAP_SEARCH_FILTER = '{{ .auth_ldap_search_filter }}'
AUTH_LDAP_UID_FIELD = '{{ .auth_ldap_uid_field }}'
AUTH_LDAP_GROUP_FIELD = '{{ .auth_ldap_group_field }}'
AUTH_LDAP_FIRSTNAME_FIELD = '{{ .auth_ldap_firstname_field }}'
AUTH_LDAP_LASTNAME_FIELD = '{{ .auth_ldap_lastname_field }}'
AUTH_LDAP_EMAIL_FIELD = '{{ .auth_ldap_email_field }}'

{{- if .auth_ldap_bind_user_file }}
with open('{{ .auth_ldap_bind_user_file }}', 'r') as f:
	AUTH_LDAP_BIND_USER = f.read().strip()
{{- end }}

{{- if .auth_ldap_bind_password_file }}
with open('{{ .auth_ldap_bind_password_file }}', 'r') as f:
	AUTH_LDAP_BIND_PASSWORD = f.read().strip()
{{- end }}
`

	t := config.TemplateParser{Template: tpl, Value: data}
	return t.Parse()
}

func (a *Authentication) GetConfig() (string, error) {
	if len(a.authenticators) == 0 {
		return a.getAuthDBConfig(), nil
	}

	configs := make([]string, 0)

	ldapConfig, err := a.getLdapConfig()
	if err != nil {
		authLogger.Error(err, "Failed to get LDAP config")
		return "", err
	}
	configs = append(configs, ldapConfig)

	oidcConfig, err := a.getOidcConfig()
	if err != nil {
		authLogger.Error(err, "Failed to get OIDC config")
		return "", err
	}
	configs = append(configs, oidcConfig)
	return strings.Join(configs, "\n"), nil
}

type ldapAuthenticator struct {
	provider *authv1alpha1.LDAPProvider
}

func (a *ldapAuthenticator) GetEnvVars() []corev1.EnvVar {
	return nil
}

func (a *ldapAuthenticator) GetVolumes() []corev1.Volume {
	if a.provider.BindCredentials == nil {
		return nil
	}
	secretClass := a.provider.BindCredentials.SecretClass

	svcScope := make([]string, 0)
	podScope := false
	nodeScope := false
	if a.provider.BindCredentials.Scope != nil {
		if a.provider.BindCredentials.Scope.Pod {
			podScope = true
		}
		if a.provider.BindCredentials.Scope.Node {
			nodeScope = true
		}
		if a.provider.BindCredentials.Scope.Services != nil {
			for _, s := range a.provider.BindCredentials.Scope.Services {
				svcScope = append(svcScope, string(constants.ServiceScope)+"="+s)
			}
		}
	}

	b := builder.NewSecretOperatorVolume(a.getVolumeName(), secretClass)
	b.SetScope(podScope, nodeScope, strings.Join(svcScope, ","), "")
	return []corev1.Volume{*b.Builde()}
}

func (a *ldapAuthenticator) getVolumeName() string {
	return fmt.Sprintf("ldap-%s", a.provider.BindCredentials.SecretClass)
}

func (a *ldapAuthenticator) GetVolumeMounts() []corev1.VolumeMount {
	if a.provider.BindCredentials == nil {
		return nil
	}
	return []corev1.VolumeMount{
		{
			Name:      a.getVolumeName(),
			MountPath: path.Join(constants.KubedoopSecretDir, a.provider.BindCredentials.SecretClass),
		},
	}
}

func (a *ldapAuthenticator) GetConfig() *properties.Properties {

	server := url.URL{Scheme: "ldap", Host: a.provider.Hostname}
	if a.provider.Port != 0 {
		server.Host += ":" + strconv.Itoa(a.provider.Port)
	}

	ldapFieldUid := DefaultLDAPFieldUid
	ldapFieldSurname := DefaultLDAPFieldSurname
	ldapFieldGivenName := DefaultLDAPFieldGivenName
	ldapFieldEmail := DefaultLDAPFieldEmail
	ldapFieldGroup := DefaultLDAPFieldGroup

	if a.provider.LDAPFieldNames != nil {
		ldapFieldUid = a.provider.LDAPFieldNames.Uid
		ldapFieldSurname = a.provider.LDAPFieldNames.Surname
		ldapFieldGivenName = a.provider.LDAPFieldNames.GivenName
		ldapFieldEmail = a.provider.LDAPFieldNames.Email
		ldapFieldGroup = a.provider.LDAPFieldNames.Group
	}

	cfg := properties.NewProperties()
	cfg.Add("auth_ldap_server", server.String())
	cfg.Add("auth_ldap_search", a.provider.SearchBase)
	cfg.Add("auth_ldap_search_filter", a.provider.SearchFilter)
	cfg.Add("auth_ldap_uid_field", ldapFieldUid)
	cfg.Add("auth_ldap_group_field", ldapFieldGroup)
	cfg.Add("auth_ldap_firstname_field", ldapFieldGivenName)
	cfg.Add("auth_ldap_lastname_field", ldapFieldSurname)
	cfg.Add("auth_ldap_email_field", ldapFieldEmail)

	mountPath := path.Join(constants.KubedoopSecretDir, a.provider.BindCredentials.SecretClass)

	if a.provider.BindCredentials != nil {
		cfg.Add("auth_ldap_bind_user_file", path.Join(mountPath, "username"))
		cfg.Add("auth_ldap_bind_password_file", path.Join(mountPath, "password"))
	}

	return cfg
}

func (a *ldapAuthenticator) GetCommands() []string {
	return nil
}

type oidcAuthenticator struct {
	config   *authv1alpha1.OidcSpec
	provider *authv1alpha1.OIDCProvider
}

func (a *oidcAuthenticator) GetEnvVars() []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{
			Name: EnvKeyOidcClientId,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key: "CLIENT_ID",
					LocalObjectReference: corev1.LocalObjectReference{
						Name: a.config.ClientCredentialsSecret,
					},
				},
			},
		},
		{
			Name: EnvKeyOidcClientSecret,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key: "CLIENT_SECRET",
					LocalObjectReference: corev1.LocalObjectReference{
						Name: a.config.ClientCredentialsSecret,
					},
				},
			},
		},
	}
	return envVars
}

func (a *oidcAuthenticator) GetVolumes() []corev1.Volume {
	return nil
}

func (a *oidcAuthenticator) GetVolumeMounts() []corev1.VolumeMount {
	return nil
}

func (a *oidcAuthenticator) GetConfig() *properties.Properties {

	scopes := a.provider.Scopes
	scopes = append(scopes, a.config.ExtraScopes...)

	issuer := url.URL{
		Scheme: "http",
		Host:   a.provider.Hostname,
		Path:   a.provider.RootPath,
	}

	if a.provider.Port != 0 {
		issuer.Host = fmt.Sprintf("%s:%d", a.provider.Hostname, a.provider.Port)
	}

	// TODO: Add Tls support

	cfg := properties.NewProperties()
	cfg.Add("client_id", fmt.Sprintf("os.environ.get('%s')", EnvKeyOidcClientId))
	cfg.Add("client_secret", fmt.Sprintf("os.environ.get('%s')", EnvKeyOidcClientSecret))
	cfg.Add("scopes", strings.Join(scopes, " "))
	cfg.Add("api_base_url", fmt.Sprintf("%s/protocol/", issuer.String()))
	cfg.Add("server_metadata_url", fmt.Sprintf("%s/.well-known/openid-configuration", issuer.String()))
	cfg.Add("provider_hint", a.provider.ProviderHint)

	return cfg
}

func (a *oidcAuthenticator) GetCommands() []string {
	return nil
}
