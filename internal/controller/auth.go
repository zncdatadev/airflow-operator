package controller

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"text/template"

	authv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/authentication/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/constant"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/security"
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

const (
	DefaultLDAPFieldEmail     = "mail"
	DefaultLDAPFieldGivenName = "givenName"
	DefaultLDAPFieldGroup     = "memberOf"
	DefaultLDAPFieldSurname   = "sn"
	DefaultLDAPFieldUid       = "uid"
)

const (
	EnvKeyOidcClientId     = "OIDC_CLIENT_ID"
	EnvKeyOidcClientSecret = "OIDC_CLIENT_SECRET"

	ldapBindCredentialsVolumeName     = "ldap-bind-credentials"
	ldapBindCredentialsUserFile       = "user"
	ldapBindCredentialsLegacyUserFile = "username"
	ldapBindCredentialsPassFile       = "password"
	syncRolesAtRegistration           = "Registration"
	syncRolesAtLogin                  = "Login"
	pythonTrue                        = "True"
	pythonFalse                       = "False"
	oidcPrincipalClaim                = "preferred_username"
	oidcRequiredScope                 = "openid"
	oidcEmailScope                    = "email"
)

var authLogger = ctrl.Log.WithName("authenticator")

// Authenticator renders one AuthenticationClass provider into Airflow terms: env vars for the
// webserver container and template values for webserver_config.py. Authentication owns any
// provider-level resources such as the LDAP SecretClass volume.
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
	volumeProviders      []reconciler.VolumeProvider
	syncRolesAt          *string
	userRegistration     *bool
	userRegistrationRole *string
}

// ldapCredentialsVolumeProvider keeps the Gen 2 configuration path
// /kubedoop/secret/<SecretClass> while using a stable, Kubernetes-safe volume name. The old
// implementation exposed that path to webserver_config.py but failed to attach its declared
// volume; preserving the path avoids breaking users who supplied the missing mount themselves.
type ldapCredentialsVolumeProvider struct {
	provisioner *security.SecretProvisioner
	mountPath   string
}

func (p *ldapCredentialsVolumeProvider) Volumes() []corev1.Volume {
	return p.provisioner.Volumes()
}

func (p *ldapCredentialsVolumeProvider) VolumeMounts() []corev1.VolumeMount {
	mounts := p.provisioner.VolumeMounts()
	for i := range mounts {
		if mounts[i].Name == ldapBindCredentialsVolumeName {
			mounts[i].MountPath = p.mountPath
		}
	}
	return mounts
}

var _ reconciler.VolumeProvider = &ldapCredentialsVolumeProvider{}

func NewAuthentication(
	ctx context.Context,
	c ctrlclient.Client,
	auths []airflowv1alpha1.AuthenticationSpec,
) (*Authentication, error) {
	result := &Authentication{authenticators: make(map[AuthenticatorType][]Authenticator)}
	if len(auths) == 0 {
		return result, nil
	}
	if len(auths) > 1 {
		return nil, fmt.Errorf("airflow supports exactly one authentication provider; multiple OIDC or LDAP entries cannot be combined")
	}

	auth := auths[0]
	switch auth.SyncRolesAt {
	case "", syncRolesAtRegistration, syncRolesAtLogin:
	default:
		return nil, fmt.Errorf("syncRolesAt must be Registration or Login, got %q", auth.SyncRolesAt)
	}
	if auth.AuthenticationClass == "" {
		return nil, fmt.Errorf("authenticationClass is required")
	}
	provider, err := GetAuthProvider(ctx, c, auth.AuthenticationClass)
	if err != nil {
		return nil, err
	}
	authType, authenticator, volumeProvider, err := newAuthenticator(auth, provider)
	if err != nil {
		return nil, err
	}
	result.authenticators[authType] = []Authenticator{authenticator}
	if volumeProvider != nil {
		result.volumeProviders = []reconciler.VolumeProvider{volumeProvider}
	}

	syncRolesAt := auth.SyncRolesAt
	userRegistration := auth.UserRegistration
	userRegistrationRole := auth.UserRegistrationRole
	result.syncRolesAt = &syncRolesAt
	result.userRegistration = &userRegistration
	result.userRegistrationRole = &userRegistrationRole
	return result, nil
}

func newAuthenticator(
	auth airflowv1alpha1.AuthenticationSpec,
	provider *authv1alpha1.AuthenticationProvider,
) (AuthenticatorType, Authenticator, reconciler.VolumeProvider, error) {
	if provider == nil {
		return "", nil, nil, fmt.Errorf("AuthenticationClass %s has no authentication provider", auth.AuthenticationClass)
	}
	if count := authenticationProviderCount(provider); count != 1 {
		return "", nil, nil, fmt.Errorf(
			"authenticationClass %s must configure exactly one authentication provider, found %d",
			auth.AuthenticationClass,
			count,
		)
	}
	switch {
	case provider.OIDC != nil:
		authenticator, err := newOIDCAuthenticator(auth, provider.OIDC)
		return AuthenticatorTypeOIDC, authenticator, nil, err
	case provider.LDAP != nil:
		authenticator, volumeProvider, err := newLDAPAuthenticator(auth, provider.LDAP)
		return AuthenticatorTypeLDAP, authenticator, volumeProvider, err
	default:
		return "", nil, nil, fmt.Errorf("unsupported authentication provider: %s", auth.AuthenticationClass)
	}
}

func authenticationProviderCount(provider *authv1alpha1.AuthenticationProvider) int {
	count := 0
	for _, configured := range []bool{
		provider.OIDC != nil,
		provider.TLS != nil,
		provider.Static != nil,
		provider.LDAP != nil,
		provider.Kerberos != nil,
	} {
		if configured {
			count++
		}
	}
	return count
}

func newOIDCAuthenticator(
	auth airflowv1alpha1.AuthenticationSpec,
	provider *authv1alpha1.OIDCProvider,
) (Authenticator, error) {
	urlHost, err := providerURLHost(provider.Hostname, provider.Port)
	if err != nil {
		return nil, fmt.Errorf("oidc AuthenticationClass %s provider endpoint: %w", auth.AuthenticationClass, err)
	}
	if provider.TLS != nil {
		return nil, fmt.Errorf(
			"oidc TLS in AuthenticationClass %s is not supported by airflow-operator",
			auth.AuthenticationClass,
		)
	}
	if provider.PrincipalClaim != oidcPrincipalClaim {
		return nil, fmt.Errorf(
			"oidc AuthenticationClass %s must use principalClaim %q; airflow's Keycloak security manager does not honor other claims",
			auth.AuthenticationClass,
			oidcPrincipalClaim,
		)
	}
	if auth.Oidc == nil {
		return nil, fmt.Errorf(
			"authentication entry for OIDC AuthenticationClass %s requires oidc configuration",
			auth.AuthenticationClass,
		)
	}
	if auth.Oidc.ClientCredentialsSecret == "" {
		return nil, fmt.Errorf(
			"authentication entry for OIDC AuthenticationClass %s requires oidc.clientCredentialsSecret",
			auth.AuthenticationClass,
		)
	}
	return &oidcAuthenticator{config: auth.Oidc, provider: provider, urlHost: urlHost}, nil
}

func newLDAPAuthenticator(
	auth airflowv1alpha1.AuthenticationSpec,
	provider *authv1alpha1.LDAPProvider,
) (Authenticator, reconciler.VolumeProvider, error) {
	urlHost, err := providerURLHost(provider.Hostname, provider.Port)
	if err != nil {
		return nil, nil, fmt.Errorf("ldap AuthenticationClass %s provider endpoint: %w", auth.AuthenticationClass, err)
	}
	if provider.TLS != nil {
		return nil, nil, fmt.Errorf(
			"ldap TLS in AuthenticationClass %s is not supported by airflow-operator",
			auth.AuthenticationClass,
		)
	}
	if auth.Oidc != nil {
		return nil, nil, fmt.Errorf(
			"authentication entry for LDAP AuthenticationClass %s must not contain oidc configuration",
			auth.AuthenticationClass,
		)
	}

	ldapAuth := &ldapAuthenticator{provider: provider, urlHost: urlHost}
	credentials := provider.BindCredentials
	if credentials == nil {
		return ldapAuth, nil, nil
	}
	if credentials.SecretClass == "" {
		return nil, nil, fmt.Errorf(
			"LDAP AuthenticationClass %s bindCredentials.secretClass is required",
			auth.AuthenticationClass,
		)
	}
	registration := security.CredentialsVolume(ldapBindCredentialsVolumeName, credentials.SecretClass)
	if scope := security.ScopeString(credentials.Scope); scope != "" {
		registration = registration.WithScope(scope)
	}
	provisioner := security.NewSecretProvisioner().
		WithMountBasePath(constant.KubedoopSecretDir).
		Register(registration)
	ldapAuth.bindCredentialsMountPath = path.Join(constant.KubedoopSecretDir, credentials.SecretClass)
	return ldapAuth, &ldapCredentialsVolumeProvider{
		provisioner: provisioner,
		mountPath:   ldapAuth.bindCredentialsMountPath,
	}, nil
}

// providerURLHost validates the AuthenticationClass hostname as a host-only value and combines
// it with its separately typed port. IPv6 literals are bracketed for URL use. Rejecting embedded
// schemes, paths and ports avoids generating a syntactically valid but unintended auth endpoint.
func providerURLHost(hostname string, port int) (string, error) {
	if hostname == "" {
		return "", fmt.Errorf("hostname is required")
	}
	if strings.TrimSpace(hostname) != hostname || strings.ContainsAny(hostname, "/?#@\\ \t\r\n") {
		return "", fmt.Errorf("hostname %q must contain only a host name or IP address", hostname)
	}
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("port %d must be between 0 and 65535", port)
	}

	rawHost := hostname
	bracketed := false
	if strings.HasPrefix(rawHost, "[") || strings.HasSuffix(rawHost, "]") {
		if !strings.HasPrefix(rawHost, "[") || !strings.HasSuffix(rawHost, "]") {
			return "", fmt.Errorf("hostname %q has mismatched IPv6 brackets", hostname)
		}
		bracketed = true
		rawHost = strings.TrimSuffix(strings.TrimPrefix(rawHost, "["), "]")
	}
	isIPv6 := strings.Contains(rawHost, ":")
	if bracketed && (!isIPv6 || net.ParseIP(rawHost) == nil) {
		return "", fmt.Errorf("hostname %q brackets a non-IPv6 value", hostname)
	}
	if isIPv6 && net.ParseIP(rawHost) == nil {
		return "", fmt.Errorf("hostname %q contains an embedded port or invalid IPv6 address", hostname)
	}
	if port != 0 {
		return net.JoinHostPort(rawHost, strconv.Itoa(port)), nil
	}
	if isIPv6 {
		return "[" + rawHost + "]", nil
	}
	return rawHost, nil
}

// VolumeProviders returns the secret volumes required by the configured authentication mode.
// Only LDAP bind credentials currently need a volume; callers attach it only to webserver pods.
func (a *Authentication) VolumeProviders() []reconciler.VolumeProvider {
	return append([]reconciler.VolumeProvider(nil), a.volumeProviders...)
}

// authenticationVolumeProviders keeps a complete Gen 2 LDAP workaround usable during an
// in-place upgrade. Gen 2 rendered the bind-credential file paths but did not attach the
// SecretClass volume, so some users supplied both a volume and a same-path mount through
// podOverrides. Injecting the v0.13 SecretProvisioner beside that complete workaround would make
// the framework reject the displaced mount. A dangling mount is deliberately not accepted: the
// framework provider remains present so its validation reports the malformed override.
func authenticationVolumeProviders(
	buildCtx *reconciler.RoleGroupBuildContext,
	auth *Authentication,
) []reconciler.VolumeProvider {
	if auth == nil {
		return nil
	}

	providers := auth.VolumeProviders()
	if buildCtx == nil || buildCtx.MergedConfig == nil || buildCtx.MergedConfig.PodOverrides == nil {
		return providers
	}

	podOverride := buildCtx.MergedConfig.PodOverrides
	mainContainerName := buildCtx.Declaration.MainContainerName
	if mainContainerName == "" {
		mainContainerName = buildCtx.ResourceName
	}

	var mainContainer *corev1.Container
	for i := range podOverride.Spec.Containers {
		if podOverride.Spec.Containers[i].Name == mainContainerName {
			mainContainer = &podOverride.Spec.Containers[i]
			break
		}
	}
	if mainContainer == nil {
		return providers
	}

	volumeNames := make(map[string]struct{}, len(podOverride.Spec.Volumes))
	for _, volume := range podOverride.Spec.Volumes {
		volumeNames[volume.Name] = struct{}{}
	}

	filtered := make([]reconciler.VolumeProvider, 0, len(providers))
	for _, provider := range providers {
		completeLegacyOverride := false
		for _, desiredMount := range provider.VolumeMounts() {
			for _, overrideMount := range mainContainer.VolumeMounts {
				if overrideMount.MountPath != desiredMount.MountPath || overrideMount.Name == desiredMount.Name {
					continue
				}
				if _, exists := volumeNames[overrideMount.Name]; exists {
					completeLegacyOverride = true
					break
				}
			}
			if completeLegacyOverride {
				break
			}
		}
		if !completeLegacyOverride {
			filtered = append(filtered, provider)
		}
	}

	return filtered
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
	return "AUTH_TYPE = AUTH_DB"
}

// templateData collects the shared FAB settings. Nil pointers render as their zero value, which
// makes the `{{- if }}` guards in the templates below skip the unset lines.
func (a *Authentication) templateData() map[string]interface{} {
	data := make(map[string]interface{})
	if a.syncRolesAt != nil {
		switch *a.syncRolesAt {
		case syncRolesAtLogin:
			data["auth_roles_sync_at_login"] = pythonTrue
		case syncRolesAtRegistration:
			data["auth_roles_sync_at_login"] = pythonFalse
		}
	}
	if a.userRegistration != nil {
		data["has_user_registration"] = true
		if *a.userRegistration {
			data["user_registration"] = pythonTrue
		} else {
			data["user_registration"] = pythonFalse
		}
	}
	if a.userRegistrationRole != nil {
		data["user_registration_role"] = *a.userRegistrationRole
	}
	return data
}

func (a *Authentication) getOidcConfig() (string, error) {
	data := a.templateData()
	providers := make([]map[string]string, 0, len(a.authenticators[AuthenticatorTypeOIDC]))
	for _, authenticator := range a.authenticators[AuthenticatorTypeOIDC] {
		providerData := make(map[string]string)
		for k, v := range authenticator.GetConfig() {
			providerData[k] = v
		}
		providers = append(providers, providerData)
	}
	data["providers"] = providers
	data["auth_type"] = "AUTH_OAUTH"

	tpl := `
AUTH_TYPE = {{ .auth_type }}

{{- if .auth_roles_sync_at_login }}
AUTH_ROLES_SYNC_AT_LOGIN = {{ .auth_roles_sync_at_login }}
{{- end }}
{{- if .has_user_registration }}
AUTH_USER_REGISTRATION = {{ .user_registration }}
{{- end }}
{{- if .user_registration_role }}
AUTH_USER_REGISTRATION_ROLE = {{ printf "%q" .user_registration_role }}
{{- end }}

OAUTH_PROVIDERS = [
{{- range .providers }}
					{
						'name': {{ printf "%q" .provider_hint }},
						'token_key': 'access_token',
						'remote_app': {
							'client_id': {{ .client_id }},
							'client_secret': {{ .client_secret }},
							'client_kwargs': {
								'scope': {{ printf "%q" .scopes }},
							},
							'api_base_url': {{ printf "%q" .api_base_url }},
							'server_metadata_url': {{ printf "%q" .server_metadata_url }},
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
AUTH_TYPE = {{ .auth_type }}

{{- if .auth_roles_sync_at_login }}
AUTH_ROLES_SYNC_AT_LOGIN = {{ .auth_roles_sync_at_login }}
{{- end }}
{{- if .has_user_registration }}
AUTH_USER_REGISTRATION = {{ .user_registration }}
{{- end }}
{{- if .user_registration_role }}
AUTH_USER_REGISTRATION_ROLE = {{ printf "%q" .user_registration_role }}
{{- end }}

AUTH_LDAP_SERVER = {{ printf "%q" .auth_ldap_server }}
AUTH_LDAP_SEARCH = {{ printf "%q" .auth_ldap_search }}
AUTH_LDAP_SEARCH_FILTER = {{ printf "%q" .auth_ldap_search_filter }}
AUTH_LDAP_UID_FIELD = {{ printf "%q" .auth_ldap_uid_field }}
AUTH_LDAP_GROUP_FIELD = {{ printf "%q" .auth_ldap_group_field }}
AUTH_LDAP_FIRSTNAME_FIELD = {{ printf "%q" .auth_ldap_firstname_field }}
AUTH_LDAP_LASTNAME_FIELD = {{ printf "%q" .auth_ldap_lastname_field }}
AUTH_LDAP_EMAIL_FIELD = {{ printf "%q" .auth_ldap_email_field }}

{{- if .auth_ldap_bind_user_file }}
try:
	with open({{ printf "%q" .auth_ldap_bind_user_file }}, 'r') as f:
		AUTH_LDAP_BIND_USER = f.read().strip()
except FileNotFoundError:
	with open({{ printf "%q" .auth_ldap_bind_legacy_user_file }}, 'r') as f:
		AUTH_LDAP_BIND_USER = f.read().strip()
{{- end }}

{{- if .auth_ldap_bind_password_file }}
with open({{ printf "%q" .auth_ldap_bind_password_file }}, 'r') as f:
	AUTH_LDAP_BIND_PASSWORD = f.read().strip()
{{- end }}
`

	return renderAuthTemplate(tpl, data)
}

// GetConfig renders the one FAB authentication mode appended to webserver_config.py. FAB has one
// global AUTH_TYPE, so mixing LDAP and OIDC is rejected by NewAuthentication rather than emitting
// two blocks where the last assignment silently wins.
func (a *Authentication) GetConfig() (string, error) {
	if len(a.authenticators) == 0 {
		return a.getAuthDBConfig(), nil
	}

	hasLDAP := len(a.authenticators[AuthenticatorTypeLDAP]) > 0
	hasOIDC := len(a.authenticators[AuthenticatorTypeOIDC]) > 0
	switch {
	case hasLDAP && !hasOIDC:
		return a.getLdapConfig()
	case hasOIDC && !hasLDAP:
		return a.getOidcConfig()
	default:
		return "", fmt.Errorf("authentication must configure exactly one of LDAP or OIDC")
	}
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
	provider                 *authv1alpha1.LDAPProvider
	urlHost                  string
	bindCredentialsMountPath string
}

func (a *ldapAuthenticator) GetEnvVars() []corev1.EnvVar {
	return nil
}

func (a *ldapAuthenticator) GetConfig() map[string]string {
	server := url.URL{Scheme: "ldap", Host: a.urlHost}

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

	if a.bindCredentialsMountPath != "" {
		cfg["auth_ldap_bind_user_file"] = path.Join(a.bindCredentialsMountPath, ldapBindCredentialsUserFile)
		cfg["auth_ldap_bind_legacy_user_file"] = path.Join(
			a.bindCredentialsMountPath,
			ldapBindCredentialsLegacyUserFile,
		)
		cfg["auth_ldap_bind_password_file"] = path.Join(a.bindCredentialsMountPath, ldapBindCredentialsPassFile)
	}

	return cfg
}

type oidcAuthenticator struct {
	config   *authv1alpha1.OidcSpec
	provider *authv1alpha1.OIDCProvider
	urlHost  string
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
	// Do not append into the provider's backing array: AuthenticationClass objects come from the
	// shared client cache and must stay immutable across role-group reconciles.
	scopes := []string{oidcRequiredScope}
	seenScopes := map[string]struct{}{oidcRequiredScope: {}}
	for _, scope := range append(append([]string(nil), a.provider.Scopes...), a.config.ExtraScopes...) {
		if scope == "" {
			continue
		}
		if _, exists := seenScopes[scope]; exists {
			continue
		}
		seenScopes[scope] = struct{}{}
		scopes = append(scopes, scope)
	}

	issuer := url.URL{
		Scheme: HTTPProtocol,
		Host:   a.urlHost,
		Path:   a.provider.RootPath,
	}
	issuerURL := strings.TrimRight(issuer.String(), "/")

	return map[string]string{
		"client_id":           fmt.Sprintf("os.environ.get('%s')", EnvKeyOidcClientId),
		"client_secret":       fmt.Sprintf("os.environ.get('%s')", EnvKeyOidcClientSecret),
		"scopes":              strings.Join(scopes, " "),
		"api_base_url":        fmt.Sprintf("%s/protocol/", issuerURL),
		"server_metadata_url": fmt.Sprintf("%s/.well-known/openid-configuration", issuerURL),
		"provider_hint":       a.provider.ProviderHint,
	}
}
