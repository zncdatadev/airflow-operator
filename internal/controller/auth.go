package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"text/template"

	authv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/authentication/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/constant"
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

var AirflowSupportAuthTypes = []AuthenticatorType{AuthenticatorTypeLDAP, AuthenticatorTypeOIDC}

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

// Authenticator renders one AuthenticationClass provider into Airflow terms: env vars for the
// webserver container and template values for webserver_config.py. The pre-framework code also
// built an LDAP SecretClass volume here that was never attached to any pod; that dead path is
// gone, so the interface covers only what reaches a rendered resource.
type Authenticator interface {
	GetEnvVars() []corev1.EnvVar
	GetConfig() map[string]string
}

func GetAuthProvider(ctx context.Context, c ctrlclient.Client, authclass string) (*authv1alpha1.AuthenticationProvider, error) {
	obj := &authv1alpha1.AuthenticationClass{}
	if err := c.Get(ctx, ctrlclient.ObjectKey{Name: authclass}, obj); err != nil {
		return nil, fmt.Errorf("failed to get AuthenticationClass %s: %w", authclass, err)
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
	c ctrlclient.Client,
	auths []airflowv1alpha1.AuthenticationSpec,
) (*Authentication, error) {
	authenticators := make(map[AuthenticatorType][]Authenticator)
	var syncRolesAt *string
	var userRegistration *bool
	var userRegistrationRole *string
	for _, auth := range auths {
		provider, err := GetAuthProvider(ctx, c, auth.AuthenticationClass)
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

func (a *Authentication) getTotalAuthenticatorCount() int {
	total := 0
	for _, typedAuthenticator := range a.authenticators {
		total += len(typedAuthenticator)
	}
	return total
}

func (a *Authentication) GetEnvVars() []corev1.EnvVar {
	envVars := make([]corev1.EnvVar, 0, a.getTotalAuthenticatorCount()*3)
	for _, typedAuthenticator := range a.authenticators {
		for _, authenticator := range typedAuthenticator {
			envVars = append(envVars, authenticator.GetEnvVars()...)
		}
	}
	return envVars
}

func (a *Authentication) getAuthDBConfig() string {
	return "AUTH_TYPE = 'AUTH_DB'"
}

// templateData collects the shared FAB settings. Nil pointers render as their zero value, which
// makes the `{{- if }}` guards in the templates below skip the unset lines.
func (a *Authentication) templateData() map[string]interface{} {
	data := make(map[string]interface{})
	if a.syncRolesAt != nil {
		data["auth_roles_sync_at_login"] = *a.syncRolesAt
	}
	if a.userRegistration != nil {
		data["user_registration"] = *a.userRegistration
	}
	if a.userRegistrationRole != nil {
		data["user_registration_role"] = *a.userRegistrationRole
	}
	return data
}

func (a *Authentication) getOidcConfig() (string, error) {
	data := a.templateData()
	for authType, typedAuthenticator := range a.authenticators {
		providerData := make(map[string]string)
		if authType == AuthenticatorTypeOIDC {
			for _, authenticator := range typedAuthenticator {
				for k, v := range authenticator.GetConfig() {
					providerData[k] = v
				}
			}
			data["providers"] = providerData
		}
	}
	data["auth_type"] = "AUTH_OAUTH"

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

	return renderAuthTemplate(tpl, data)
}

func (a *Authentication) getLdapConfig() (string, error) {
	data := a.templateData()
	exist := false
	for authType, typedAuthenticator := range a.authenticators {
		if authType == AuthenticatorTypeLDAP {
			if exist {
				authLogger.Info("Multiple LDAP authenticators found, using the first one")
				continue
			}
			for _, authenticator := range typedAuthenticator {
				for k, v := range authenticator.GetConfig() {
					data[k] = v
				}
			}
			exist = true
		}
	}
	data["auth_type"] = "AUTH_LDAP"

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

	return renderAuthTemplate(tpl, data)
}

// GetConfig renders the FAB section appended to webserver_config.py. With no authenticators it
// is AUTH_DB; otherwise the LDAP and OIDC blocks are rendered in that order (an empty block still
// renders its skeleton, matching the pre-framework behavior the product's e2e was built on).
func (a *Authentication) GetConfig() (string, error) {
	if len(a.authenticators) == 0 {
		return a.getAuthDBConfig(), nil
	}

	configs := make([]string, 0, 2)

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

func renderAuthTemplate(tpl string, data map[string]interface{}) (string, error) {
	t, err := template.New("auth").Parse(tpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type ldapAuthenticator struct {
	provider *authv1alpha1.LDAPProvider
}

func (a *ldapAuthenticator) GetEnvVars() []corev1.EnvVar {
	return nil
}

func (a *ldapAuthenticator) GetConfig() map[string]string {
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

	cfg := map[string]string{
		"auth_ldap_server":          server.String(),
		"auth_ldap_search":          a.provider.SearchBase,
		"auth_ldap_search_filter":   a.provider.SearchFilter,
		"auth_ldap_uid_field":       ldapFieldUid,
		"auth_ldap_group_field":     ldapFieldGroup,
		"auth_ldap_firstname_field": ldapFieldGivenName,
		"auth_ldap_lastname_field":  ldapFieldSurname,
		"auth_ldap_email_field":     ldapFieldEmail,
	}

	if a.provider.BindCredentials != nil {
		mountPath := path.Join(constant.KubedoopSecretDir, a.provider.BindCredentials.SecretClass)
		cfg["auth_ldap_bind_user_file"] = path.Join(mountPath, "username")
		cfg["auth_ldap_bind_password_file"] = path.Join(mountPath, "password")
	}

	return cfg
}

type oidcAuthenticator struct {
	config   *authv1alpha1.OidcSpec
	provider *authv1alpha1.OIDCProvider
}

func (a *oidcAuthenticator) GetEnvVars() []corev1.EnvVar {
	return []corev1.EnvVar{
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
}

func (a *oidcAuthenticator) GetConfig() map[string]string {
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

	return map[string]string{
		"client_id":           fmt.Sprintf("os.environ.get('%s')", EnvKeyOidcClientId),
		"client_secret":       fmt.Sprintf("os.environ.get('%s')", EnvKeyOidcClientSecret),
		"scopes":              strings.Join(scopes, " "),
		"api_base_url":        fmt.Sprintf("%s/protocol/", issuer.String()),
		"server_metadata_url": fmt.Sprintf("%s/.well-known/openid-configuration", issuer.String()),
		"provider_hint":       a.provider.ProviderHint,
	}
}
