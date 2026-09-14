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
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	authv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/authentication/v1alpha1"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	operatorconfig "github.com/zncdatadev/operator-go/pkg/config"
	"github.com/zncdatadev/operator-go/pkg/constant"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/security"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testLDAPClassName = "airflow-ldap"
	testLDAPHostname  = "ldap.example.test"
	testLDAPSecret    = "airflow-ldap-bind"
	testOIDCHostname  = "keycloak.example.test"
	testOIDCSecret    = "airflow-oidc-client"
	testProfileScope  = "profile"
	legacyLDAPVolume  = "legacy-ldap"
)

func TestGetAuthProviderUsesRegisteredAuthenticationClass(t *testing.T) {
	g := NewWithT(t)
	testScheme := runtime.NewScheme()
	g.Expect(authv1alpha1.AddToScheme(testScheme)).To(Succeed())

	expected := &authv1alpha1.AuthenticationProvider{
		LDAP: &authv1alpha1.LDAPProvider{Hostname: testLDAPHostname},
	}
	authClass := &authv1alpha1.AuthenticationClass{
		ObjectMeta: metav1.ObjectMeta{Name: testLDAPClassName},
		Spec: authv1alpha1.AuthenticationClassSpec{
			AuthenticationProvider: expected,
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(authClass).Build()

	provider, err := GetAuthProvider(context.Background(), k8sClient, authClass.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(provider).To(Equal(expected))
}

func TestLDAPAuthenticationRendersOnlyLDAPAndProvisionsBindCredentials(t *testing.T) {
	g := NewWithT(t)
	testScheme := runtime.NewScheme()
	g.Expect(authv1alpha1.AddToScheme(testScheme)).To(Succeed())

	authClass := &authv1alpha1.AuthenticationClass{
		ObjectMeta: metav1.ObjectMeta{Name: testLDAPClassName},
		Spec: authv1alpha1.AuthenticationClassSpec{AuthenticationProvider: &authv1alpha1.AuthenticationProvider{
			LDAP: &authv1alpha1.LDAPProvider{
				Hostname:     testLDAPHostname,
				Port:         1389,
				SearchBase:   "ou=users,dc=example,dc=test",
				SearchFilter: "(objectClass=person)",
				BindCredentials: &commonsv1alpha1.Credentials{
					SecretClass: testLDAPSecret,
					Scope: &commonsv1alpha1.CredentialsScope{
						Pod:      true,
						Services: []string{"airflow-web"},
					},
				},
			},
		}},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(authClass).Build()

	auth, err := NewAuthentication(context.Background(), k8sClient, []airflowv1alpha1.AuthenticationSpec{{
		AuthenticationClass:  authClass.Name,
		SyncRolesAt:          syncRolesAtLogin,
		UserRegistration:     false,
		UserRegistrationRole: "Public",
	}})
	g.Expect(err).NotTo(HaveOccurred())

	config, err := auth.GetConfig()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(config).To(ContainSubstring("AUTH_TYPE = AUTH_LDAP"))
	g.Expect(config).NotTo(ContainSubstring("AUTH_OAUTH"))
	g.Expect(config).NotTo(ContainSubstring("OAUTH_PROVIDERS"))
	g.Expect(config).To(ContainSubstring(`AUTH_LDAP_SERVER = "ldap://ldap.example.test:1389"`))
	g.Expect(config).To(ContainSubstring(`AUTH_LDAP_EMAIL_FIELD = "mail"`))
	g.Expect(config).To(ContainSubstring("AUTH_ROLES_SYNC_AT_LOGIN = " + pythonTrue))
	g.Expect(config).To(ContainSubstring("AUTH_USER_REGISTRATION = False"))
	g.Expect(config).To(ContainSubstring(
		`with open("/kubedoop/secret/airflow-ldap-bind/user", 'r') as f:`,
	))
	g.Expect(config).To(ContainSubstring("except FileNotFoundError:"))
	g.Expect(config).To(ContainSubstring(
		`with open("/kubedoop/secret/airflow-ldap-bind/username", 'r') as f:`,
	))
	g.Expect(config).To(ContainSubstring(
		`with open("/kubedoop/secret/airflow-ldap-bind/password", 'r') as f:`,
	))
	assertPythonCompiles(t, g, config)

	providers := auth.VolumeProviders()
	g.Expect(providers).To(HaveLen(1))
	volumes := providers[0].Volumes()
	g.Expect(volumes).To(HaveLen(1))
	g.Expect(volumes[0].Name).To(Equal(ldapBindCredentialsVolumeName))
	g.Expect(volumes[0].Ephemeral).NotTo(BeNil())
	annotations := volumes[0].Ephemeral.VolumeClaimTemplate.Annotations
	g.Expect(annotations).To(HaveKeyWithValue(security.SecretClassAnnotation, testLDAPSecret))
	g.Expect(annotations).To(HaveKeyWithValue(security.SecretClassScopeAnnotation, "pod,service=airflow-web"))

	mounts := providers[0].VolumeMounts()
	g.Expect(mounts).To(ConsistOf(Equal(coreVolumeMount(
		ldapBindCredentialsVolumeName,
		constant.KubedoopSecretDir+testLDAPSecret,
	))))
}

func TestAuthenticationVolumeProvidersPreserveCompleteLegacyLDAPOverride(t *testing.T) {
	registration := security.CredentialsVolume(ldapBindCredentialsVolumeName, testLDAPSecret)
	provider := &ldapCredentialsVolumeProvider{
		provisioner: security.NewSecretProvisioner().
			WithMountBasePath(constant.KubedoopSecretDir).
			Register(registration),
		mountPath: path.Join(constant.KubedoopSecretDir, testLDAPSecret),
	}
	auth := &Authentication{volumeProviders: []reconciler.VolumeProvider{provider}}
	wantedPath := path.Join(constant.KubedoopSecretDir, testLDAPSecret)
	mainContainerName := "webservers"

	tests := []struct {
		name          string
		podOverride   *corev1.PodTemplateSpec
		wantProviders int
	}{
		{name: "no override", wantProviders: 1},
		{
			name: "complete legacy override",
			podOverride: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{{Name: legacyLDAPVolume}},
				Containers: []corev1.Container{{
					Name: mainContainerName,
					VolumeMounts: []corev1.VolumeMount{{
						Name: legacyLDAPVolume, MountPath: wantedPath,
					}},
				}},
			}},
			wantProviders: 0,
		},
		{
			name: "dangling legacy mount",
			podOverride: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: mainContainerName,
					VolumeMounts: []corev1.VolumeMount{{
						Name: legacyLDAPVolume, MountPath: wantedPath,
					}},
				}},
			}},
			wantProviders: 1,
		},
		{
			name: "same provider name",
			podOverride: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{{Name: ldapBindCredentialsVolumeName}},
				Containers: []corev1.Container{{
					Name: mainContainerName,
					VolumeMounts: []corev1.VolumeMount{{
						Name: ldapBindCredentialsVolumeName, MountPath: wantedPath,
					}},
				}},
			}},
			wantProviders: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buildCtx := &reconciler.RoleGroupBuildContext{
				ResourceName: "airflow-webservers-default",
				Declaration: reconciler.RoleDeclaration{
					MainContainerName: mainContainerName,
				},
				MergedConfig: &operatorconfig.MergedConfig{PodOverrides: tt.podOverride},
			}
			if got := len(authenticationVolumeProviders(buildCtx, auth)); got != tt.wantProviders {
				t.Fatalf("authenticationVolumeProviders() returned %d providers, want %d", got, tt.wantProviders)
			}
		})
	}
}

func TestAuthenticationConfigurationIsRenderedOnlyForWebservers(t *testing.T) {
	g := NewWithT(t)
	auth := &Authentication{authenticators: map[AuthenticatorType][]Authenticator{
		AuthenticatorTypeLDAP: {
			&ldapAuthenticator{
				provider: &authv1alpha1.LDAPProvider{Hostname: testLDAPHostname},
				urlHost:  testLDAPHostname,
			},
		},
	}}

	for _, roleName := range []string{
		string(airflowv1alpha1.SchedulersRoleName),
		string(airflowv1alpha1.CeleryExecutorsRoleName),
	} {
		config, err := renderWebserverConfig(authenticationForRole(roleName, auth))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(config).NotTo(ContainSubstring("AUTH_TYPE = AUTH_LDAP"), roleName)
		g.Expect(config).NotTo(ContainSubstring("with open("), roleName)
	}

	config, err := renderWebserverConfig(authenticationForRole(string(airflowv1alpha1.WebserversRoleName), auth))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(config).To(ContainSubstring("AUTH_TYPE = AUTH_LDAP"))
}

func TestOIDCAuthenticationRendersValidProviderWithoutMutatingAuthenticationClass(t *testing.T) {
	g := NewWithT(t)
	testScheme := runtime.NewScheme()
	g.Expect(authv1alpha1.AddToScheme(testScheme)).To(Succeed())

	providerScopes := make([]string, 1, 4)
	providerScopes[0] = oidcRequiredScope
	authClass := &authv1alpha1.AuthenticationClass{
		ObjectMeta: metav1.ObjectMeta{Name: "airflow-oidc"},
		Spec: authv1alpha1.AuthenticationClassSpec{AuthenticationProvider: &authv1alpha1.AuthenticationProvider{
			OIDC: &authv1alpha1.OIDCProvider{
				Hostname:       testOIDCHostname,
				Port:           8080,
				RootPath:       "/realms/kubedoop/",
				Scopes:         providerScopes,
				ProviderHint:   "keycloak",
				PrincipalClaim: oidcPrincipalClaim,
			},
		}},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(authClass).Build()

	auth, err := NewAuthentication(context.Background(), k8sClient, []airflowv1alpha1.AuthenticationSpec{{
		AuthenticationClass: authClass.Name,
		Oidc: &authv1alpha1.OidcSpec{
			ClientCredentialsSecret: testOIDCSecret,
			ExtraScopes:             []string{oidcEmailScope, testProfileScope},
		},
		UserRegistration:     true,
		UserRegistrationRole: "Viewer",
	}})
	g.Expect(err).NotTo(HaveOccurred())

	config, err := auth.GetConfig()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(config).To(ContainSubstring("AUTH_TYPE = AUTH_OAUTH"))
	g.Expect(config).NotTo(ContainSubstring("AUTH_LDAP"))
	g.Expect(config).To(ContainSubstring(`'name': "keycloak"`))
	g.Expect(config).To(ContainSubstring(`'scope': "openid email profile"`))
	g.Expect(config).To(ContainSubstring(`'api_base_url': "http://keycloak.example.test:8080/realms/kubedoop/protocol/"`))
	g.Expect(config).To(ContainSubstring(`'server_metadata_url': "http://keycloak.example.test:8080/realms/kubedoop/.well-known/openid-configuration"`))
	g.Expect(strings.Count(config, "'client_id': os.environ.get('OIDC_CLIENT_ID')")).To(Equal(1))
	g.Expect(providerScopes).To(Equal([]string{"openid"}))
	assertPythonCompiles(t, g, config)

	envs := auth.GetEnvVars()
	g.Expect(envs).To(HaveLen(2))
	g.Expect(envs[0].Name).To(Equal(EnvKeyOidcClientId))
	g.Expect(envs[0].ValueFrom.SecretKeyRef.Name).To(Equal(testOIDCSecret))
	g.Expect(envs[0].ValueFrom.SecretKeyRef.Key).To(Equal("CLIENT_ID"))
	g.Expect(envs[1].Name).To(Equal(EnvKeyOidcClientSecret))
	g.Expect(envs[1].ValueFrom.SecretKeyRef.Key).To(Equal("CLIENT_SECRET"))
	g.Expect(auth.VolumeProviders()).To(BeEmpty())
}

func TestOIDCAuthenticationRequiresProductSpecificConfiguration(t *testing.T) {
	g := NewWithT(t)
	testScheme := runtime.NewScheme()
	g.Expect(authv1alpha1.AddToScheme(testScheme)).To(Succeed())
	authClass := oidcAuthenticationClass("airflow-oidc")
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(authClass).Build()

	_, err := NewAuthentication(context.Background(), k8sClient, []airflowv1alpha1.AuthenticationSpec{{
		AuthenticationClass: authClass.Name,
	}})
	g.Expect(err).To(MatchError(ContainSubstring("requires oidc configuration")))
}

func TestOIDCScopesAlwaysIncludeOpenIDAndRemainUnique(t *testing.T) {
	tests := []struct {
		name           string
		providerScopes []string
		extraScopes    []string
		want           string
	}{
		{name: "empty", want: oidcRequiredScope},
		{name: "extra only", extraScopes: []string{oidcEmailScope}, want: "openid email"},
		{
			name:           "duplicates and blanks",
			providerScopes: []string{testProfileScope, oidcRequiredScope, ""},
			extraScopes:    []string{oidcEmailScope, testProfileScope, oidcRequiredScope},
			want:           "openid profile email",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authenticator := &oidcAuthenticator{
				provider: &authv1alpha1.OIDCProvider{Scopes: tt.providerScopes},
				config:   &authv1alpha1.OidcSpec{ExtraScopes: tt.extraScopes},
			}
			if got := authenticator.GetConfig()["scopes"]; got != tt.want {
				t.Fatalf("scopes = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProviderURLHostValidation(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		port     int
		want     string
		wantErr  string
	}{
		{name: "dns", hostname: testOIDCHostname, port: 8080, want: "keycloak.example.test:8080"},
		{name: "ipv6", hostname: "2001:db8::1", port: 8443, want: "[2001:db8::1]:8443"},
		{name: "bracketed ipv6", hostname: "[2001:db8::1]", want: "[2001:db8::1]"},
		{name: "scheme", hostname: "https://keycloak.example.test", wantErr: "host name or IP address"},
		{name: "embedded port", hostname: "keycloak.example.test:8443", wantErr: "embedded port"},
		{name: "out of range port", hostname: testOIDCHostname, port: 65536, wantErr: "between 0 and 65535"},
		{name: "interior whitespace", hostname: "keycloak .example.test", wantErr: "host name or IP address"},
		{name: "bracketed dns", hostname: "[keycloak.example.test]", wantErr: "non-IPv6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := providerURLHost(tt.hostname, tt.port)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("providerURLHost(%q, %d) error = %v, want substring %q", tt.hostname, tt.port, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("providerURLHost(%q, %d): %v", tt.hostname, tt.port, err)
			}
			if got != tt.want {
				t.Fatalf("providerURLHost(%q, %d) = %q, want %q", tt.hostname, tt.port, got, tt.want)
			}
		})
	}
}

func TestAuthenticationRejectsMixedLDAPAndOIDC(t *testing.T) {
	g := NewWithT(t)
	testScheme := runtime.NewScheme()
	g.Expect(authv1alpha1.AddToScheme(testScheme)).To(Succeed())
	ldapClass := &authv1alpha1.AuthenticationClass{
		ObjectMeta: metav1.ObjectMeta{Name: testLDAPClassName},
		Spec: authv1alpha1.AuthenticationClassSpec{AuthenticationProvider: &authv1alpha1.AuthenticationProvider{
			LDAP: &authv1alpha1.LDAPProvider{Hostname: testLDAPHostname},
		}},
	}
	oidcClass := oidcAuthenticationClass("airflow-oidc")
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(ldapClass, oidcClass).Build()

	_, err := NewAuthentication(context.Background(), k8sClient, []airflowv1alpha1.AuthenticationSpec{
		{AuthenticationClass: ldapClass.Name},
		{
			AuthenticationClass: oidcClass.Name,
			Oidc: &authv1alpha1.OidcSpec{
				ClientCredentialsSecret: testOIDCSecret,
			},
		},
	})
	g.Expect(err).To(MatchError(ContainSubstring("multiple OIDC or LDAP entries cannot be combined")))
}

func TestAuthenticationRejectsTLSRatherThanSilentlyUsingCleartext(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := authv1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("register authentication scheme: %v", err)
	}

	tests := []struct {
		name      string
		authClass *authv1alpha1.AuthenticationClass
		spec      airflowv1alpha1.AuthenticationSpec
	}{
		{
			name: "oidc",
			authClass: func() *authv1alpha1.AuthenticationClass {
				result := oidcAuthenticationClass("tls-oidc")
				result.Spec.AuthenticationProvider.OIDC.TLS = &authv1alpha1.OIDCTls{}
				return result
			}(),
			spec: airflowv1alpha1.AuthenticationSpec{
				AuthenticationClass: "tls-oidc",
				Oidc: &authv1alpha1.OidcSpec{
					ClientCredentialsSecret: testOIDCSecret,
				},
			},
		},
		{
			name: "ldap",
			authClass: &authv1alpha1.AuthenticationClass{
				ObjectMeta: metav1.ObjectMeta{Name: "tls-ldap"},
				Spec: authv1alpha1.AuthenticationClassSpec{
					AuthenticationProvider: &authv1alpha1.AuthenticationProvider{
						LDAP: &authv1alpha1.LDAPProvider{
							Hostname: testLDAPHostname,
							TLS:      &authv1alpha1.LDAPTLS{},
						},
					},
				},
			},
			spec: airflowv1alpha1.AuthenticationSpec{AuthenticationClass: "tls-ldap"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(tt.authClass).Build()
			_, err := NewAuthentication(context.Background(), k8sClient, []airflowv1alpha1.AuthenticationSpec{tt.spec})
			g.Expect(err).To(MatchError(And(ContainSubstring("TLS"), ContainSubstring("not supported"))))
		})
	}
}

func TestAuthenticationRejectsAmbiguousAuthenticationClass(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := authv1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("register authentication scheme: %v", err)
	}

	tests := []struct {
		name      string
		providers *authv1alpha1.AuthenticationProvider
		spec      airflowv1alpha1.AuthenticationSpec
	}{
		{
			name: "oidc and static",
			providers: &authv1alpha1.AuthenticationProvider{
				OIDC:   oidcAuthenticationClass("unused").Spec.AuthenticationProvider.OIDC,
				Static: &authv1alpha1.StaticProvider{},
			},
			spec: airflowv1alpha1.AuthenticationSpec{
				Oidc: &authv1alpha1.OidcSpec{ClientCredentialsSecret: testOIDCSecret},
			},
		},
		{
			name: "ldap and kerberos",
			providers: &authv1alpha1.AuthenticationProvider{
				LDAP:     &authv1alpha1.LDAPProvider{Hostname: testLDAPHostname},
				Kerberos: &authv1alpha1.KerberosProvider{},
			},
		},
		{
			name: "ldap and oidc",
			providers: &authv1alpha1.AuthenticationProvider{
				LDAP: &authv1alpha1.LDAPProvider{Hostname: testLDAPHostname},
				OIDC: oidcAuthenticationClass("unused").Spec.AuthenticationProvider.OIDC,
			},
			spec: airflowv1alpha1.AuthenticationSpec{
				Oidc: &authv1alpha1.OidcSpec{ClientCredentialsSecret: testOIDCSecret},
			},
		},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			className := fmt.Sprintf("ambiguous-%d", index)
			authClass := &authv1alpha1.AuthenticationClass{
				ObjectMeta: metav1.ObjectMeta{Name: className},
				Spec: authv1alpha1.AuthenticationClassSpec{
					AuthenticationProvider: tt.providers,
				},
			}
			k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(authClass).Build()
			spec := tt.spec
			spec.AuthenticationClass = className
			_, err := NewAuthentication(context.Background(), k8sClient, []airflowv1alpha1.AuthenticationSpec{spec})
			g.Expect(err).To(MatchError(ContainSubstring("exactly one authentication provider, found 2")))
		})
	}
}

func TestAuthenticationRejectsMissingOrUnsupportedProvider(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := authv1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("register authentication scheme: %v", err)
	}

	tests := []struct {
		name      string
		providers *authv1alpha1.AuthenticationProvider
		want      string
	}{
		{name: "nil", want: "has no authentication provider"},
		{name: "empty", providers: &authv1alpha1.AuthenticationProvider{}, want: "found 0"},
		{name: "static", providers: &authv1alpha1.AuthenticationProvider{Static: &authv1alpha1.StaticProvider{}}, want: "unsupported authentication provider"},
		{name: "tls", providers: &authv1alpha1.AuthenticationProvider{TLS: &authv1alpha1.TLSProvider{}}, want: "unsupported authentication provider"},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			className := fmt.Sprintf("provider-%d", index)
			authClass := &authv1alpha1.AuthenticationClass{
				ObjectMeta: metav1.ObjectMeta{Name: className},
				Spec: authv1alpha1.AuthenticationClassSpec{
					AuthenticationProvider: tt.providers,
				},
			}
			k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(authClass).Build()
			_, err := NewAuthentication(context.Background(), k8sClient, []airflowv1alpha1.AuthenticationSpec{{
				AuthenticationClass: className,
			}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewAuthentication error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func oidcAuthenticationClass(name string) *authv1alpha1.AuthenticationClass {
	return &authv1alpha1.AuthenticationClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: authv1alpha1.AuthenticationClassSpec{AuthenticationProvider: &authv1alpha1.AuthenticationProvider{
			OIDC: &authv1alpha1.OIDCProvider{
				Hostname:       testOIDCHostname,
				ProviderHint:   "keycloak",
				PrincipalClaim: oidcPrincipalClaim,
			},
		}},
	}
}

func coreVolumeMount(name, mountPath string) corev1.VolumeMount {
	return corev1.VolumeMount{Name: name, MountPath: mountPath, ReadOnly: true}
}

func assertPythonCompiles(t *testing.T, g *WithT, source string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable; exact renderer assertions still ran")
	}
	cmd := exec.Command(python, "-c", "import sys; compile(sys.stdin.read(), 'webserver_config.py', 'exec')")
	cmd.Stdin = strings.NewReader(source)
	output, err := cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), string(output))
}
