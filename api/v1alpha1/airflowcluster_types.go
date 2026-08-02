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

package v1alpha1

import (
	authenticationv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/authentication/v1alpha1"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

const (
	DefaultRepository     = "quay.io/zncdatadev"
	DefaultProductVersion = "2.10.5"
	DefaultProductName    = "airflow"
)

type RoleName string

const (
	SchedulersRoleName          RoleName = "schedulers"
	WebserversRoleName          RoleName = "webservers"
	CeleryExecutorsRoleName     RoleName = "celeryexecutors"
	KubernetesExecutorsRoleName RoleName = "kubernetesexecutors"
)

type ImageSpec struct {
	// +kubebuilder:validation:Optional
	Custom string `json:"custom,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default=quay.io/zncdatadev
	Repo string `json:"repo,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default="0.0.0-dev"
	KubedoopVersion string `json:"kubedoopVersion,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default="2.10.5"
	ProductVersion string `json:"productVersion,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default:=IfNotPresent
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`

	// +kubebuilder:validation:Optional
	PullSecretName string `json:"pullSecretName,omitempty"`
}

type AuthenticationSpec struct {
	// +kubebuilder:validation:Optional
	AuthenticationClass string `json:"authenticationClass,omitempty"`

	// +kubebuilder:validation:Optional
	Oidc *authenticationv1alpha1.OidcSpec `json:"oidc,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Registration;Login
	SyncRolesAt string `json:"syncRolesAt,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default=true
	UserRegistration bool `json:"userRegistration,omitempty"`

	// +kubebuilder:validation:Optional
	UserRegistrationRole string `json:"userRegistrationRole,omitempty"`
}

type DagsGitSyncSpec struct {
	// +kubebuilder:validation:Optional
	Branch string `json:"branch,omitempty"`

	// +kubebuilder:validation:Optional
	CrdentialsSecret string `json:"crdentialsSecretName,omitempty"`

	// +kubebuilder:validation:Optional
	Depth *int8 `json:"depth,omitempty"`

	// +kubebuilder:validation:Optional
	GitFolder string `json:"gitFolder,omitempty"`

	// +kubebuilder:validation:Optional
	GitSyncConf map[string]string `json:"gitSyncConf,omitempty"`

	// +kubebuilder:validation:Required
	Repo string `json:"repo"`

	// The sync interval in seconds, default is 30 seconds
	// +kubebuilder:validation:Optional
	Wait *int16 `json:"wait,omitempty"`
}

type ClusterConfigSpec struct {
	// +kubebuilder:validation:Optional
	Authentication []AuthenticationSpec `json:"authentication,omitempty"`

	// airflow credentials secret name
	// The secret should contain the following keys:
	// 	- adminUser.username
	// 	- adminUser.firstusername
	// 	- adminUser.lastname
	// 	- adminUser.email
	// 	- adminUser.password
	// 	- connections.SecretKey	# Flask app secret key, eg: openssl rand -hex 30
	// 	- connections.sqlalchemyDatabaseUri	# SQLAlchemy database URI, currently only supports postgresql in product container image
	// 	- connections.celeryResultBackend	# Celery result backend, Only needed if using celery workers
	// 	- connections.celeryBrokerUrl	# Celery broker URL, Only needed if using celery workers
	// +kubebuilder:validation:Required
	Credentials string `json:"credentialsSecret"`

	// +kubebuilder:validation:Optional
	DagsGitSync []DagsGitSyncSpec `json:"dagsGitSync,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	// +kubebuilder:validation:Type=boolean
	ExposeConfig bool `json:"exposeConfig,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	// +kubebuilder:validation:Type=boolean
	LoadExamples bool `json:"loadExamples,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=cluster-internal;external-unstable;external-stable
	ListenerClass string `json:"listenerClass,omitempty"`

	// +kubebuilder:validation:Optional
	VectorAggregatorConfigMapName string `json:"vectorAggregatorConfigMapName,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:EmbeddedResource
	// +kubebuilder:validation:Type=object
	Volumes []k8sruntime.RawExtension `json:"volumes,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:EmbeddedResource
	// +kubebuilder:validation:Type=object
	VolumeMounts []k8sruntime.RawExtension `json:"volumeMounts,omitempty"`
}

type RoleGroupSpec struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=1
	Replicas                       *int32      `json:"replicas"`
	Config                         *ConfigSpec `json:"config,omitempty"`
	*commonsv1alpha1.OverridesSpec `json:",inline"`
}

type ConfigSpec struct {
	*commonsv1alpha1.RoleGroupConfigSpec `json:",inline"`
}

type CeleryExecutorsSpec struct {
	RoleGroups                     map[string]RoleGroupSpec        `json:"roleGroups,omitempty"`
	RoleConfig                     *commonsv1alpha1.RoleConfigSpec `json:"roleConfig,omitempty"`
	Config                         *ConfigSpec                     `json:"config,omitempty"`
	*commonsv1alpha1.OverridesSpec `json:",inline"`
}

type KubernetesExecutorsSpec struct {
	RoleConfig                           *commonsv1alpha1.RoleConfigSpec `json:"roleConfig,omitempty"`
	Config                               *ConfigSpec                     `json:"config,omitempty"`
	*commonsv1alpha1.OverridesSpec       `json:",inline"`
	*commonsv1alpha1.RoleGroupConfigSpec `json:",inline"`
}

type SchedulersSpec struct {
	RoleGroups                     map[string]RoleGroupSpec        `json:"roleGroups,omitempty"`
	RoleConfig                     *commonsv1alpha1.RoleConfigSpec `json:"roleConfig,omitempty"`
	Config                         *ConfigSpec                     `json:"config,omitempty"`
	*commonsv1alpha1.OverridesSpec `json:",inline"`
}

type WebserversSpec struct {
	RoleGroups                     map[string]RoleGroupSpec        `json:"roleGroups,omitempty"`
	RoleConfig                     *commonsv1alpha1.RoleConfigSpec `json:"roleConfig,omitempty"`
	Config                         *ConfigSpec                     `json:"config,omitempty"`
	*commonsv1alpha1.OverridesSpec `json:",inline"`
}

// AirflowClusterSpec defines the desired state of AirflowCluster.
type AirflowClusterSpec struct {
	// +kubebuilder:validation:Optional
	// +default:value={"repo": "quay.io/zncdatadev", "pullPolicy": "IfNotPresent"}
	Image *ImageSpec `json:"image,omitempty"`

	// +kubebuilder:validation:Optional
	ClusterOperation *commonsv1alpha1.ClusterOperationSpec `json:"clusterOperation,omitempty"`

	// +kubebuilder:validation:Optional
	ClusterConfig *ClusterConfigSpec `json:"clusterConfig,omitempty"`

	// +kubebuilder:validation:Optional
	CeleryExecutors *CeleryExecutorsSpec `json:"celeryExecutors,omitempty"`

	// +kubebuilder:validation:Optional
	KubernetesExecutors *KubernetesExecutorsSpec `json:"kubernetesExecutors,omitempty"`

	// +kubebuilder:validation:Optional
	Schedulers *SchedulersSpec `json:"schedulers,omitempty"`

	// +kubebuilder:validation:Optional
	Webservers *WebserversSpec `json:"webservers,omitempty"`
}

// AirflowClusterStatus defines the observed state of AirflowCluster.
type AirflowClusterStatus struct {
	commonsv1alpha1.GenericClusterStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// AirflowCluster is the Schema for the airflowclusters API.
type AirflowCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AirflowClusterSpec   `json:"spec,omitempty"`
	Status AirflowClusterStatus `json:"status,omitempty"`
}

// ClusterInterface implementation.
//
// GetSpec bridges the typed role fields into the generic Roles map the framework iterates.
// The role keys are the plural CR field names ("webservers", "schedulers", "celeryexecutors"),
// which keeps the role-group resource naming <cluster>-<role>-<group> identical to the
// pre-framework operator. "kubernetesexecutors" stays in the CRD but is not mapped: it was
// never implemented by the controller.
var _ common.ClusterInterface = &AirflowCluster{}

func (a *AirflowCluster) GetSpec() *commonsv1alpha1.GenericClusterSpec {
	return a.Spec.ToGenericSpec()
}

// GetStatus returns a pointer into the CR's embedded generic status; the framework writes
// conditions and the role-group ledger through it, so product status fields would survive a
// reconcile cycle alongside it.
func (a *AirflowCluster) GetStatus() *commonsv1alpha1.GenericClusterStatus {
	return &a.Status.GenericClusterStatus
}

// ToGenericSpec adapts AirflowClusterSpec to GenericClusterSpec.
func (s *AirflowClusterSpec) ToGenericSpec() *commonsv1alpha1.GenericClusterSpec {
	result := &commonsv1alpha1.GenericClusterSpec{
		ClusterOperation: s.ClusterOperation,
	}

	if s.Image != nil {
		result.Image = &commonsv1alpha1.ImageSpec{
			Custom:          s.Image.Custom,
			Repo:            s.Image.Repo,
			ProductVersion:  s.Image.ProductVersion,
			KubedoopVersion: s.Image.KubedoopVersion,
		}
	}

	roles := make(map[string]commonsv1alpha1.RoleSpec)
	if s.Webservers != nil {
		roles[string(WebserversRoleName)] = adaptRoleSpec(s.Webservers.RoleConfig, s.Webservers.Config, s.Webservers.OverridesSpec, s.Webservers.RoleGroups)
	}
	if s.Schedulers != nil {
		roles[string(SchedulersRoleName)] = adaptRoleSpec(s.Schedulers.RoleConfig, s.Schedulers.Config, s.Schedulers.OverridesSpec, s.Schedulers.RoleGroups)
	}
	if s.CeleryExecutors != nil {
		roles[string(CeleryExecutorsRoleName)] = adaptRoleSpec(s.CeleryExecutors.RoleConfig, s.CeleryExecutors.Config, s.CeleryExecutors.OverridesSpec, s.CeleryExecutors.RoleGroups)
	}
	result.Roles = roles

	return result
}

// adaptRoleSpec converts a product role spec into the generic RoleSpec, flattening the embedded
// overrides the way the CRD surfaces them (directly on the role, not nested).
func adaptRoleSpec(
	roleConfig *commonsv1alpha1.RoleConfigSpec,
	config *ConfigSpec,
	overrides *commonsv1alpha1.OverridesSpec,
	roleGroups map[string]RoleGroupSpec,
) commonsv1alpha1.RoleSpec {
	roleSpec := commonsv1alpha1.RoleSpec{
		RoleConfig: roleConfig,
	}

	if config != nil && config.RoleGroupConfigSpec != nil {
		roleSpec.Config = config.RoleGroupConfigSpec
	}

	if overrides != nil {
		roleSpec.ConfigOverrides = overrides.ConfigOverrides
		roleSpec.EnvOverrides = overrides.EnvOverrides
		roleSpec.CliOverrides = overrides.CliOverrides
		roleSpec.PodOverrides = overrides.PodOverrides
	}

	groups := make(map[string]commonsv1alpha1.RoleGroupSpec, len(roleGroups))
	for name, rg := range roleGroups {
		adapted := commonsv1alpha1.RoleGroupSpec{
			Replicas: rg.Replicas,
		}
		if rg.Config != nil && rg.Config.RoleGroupConfigSpec != nil {
			adapted.Config = rg.Config.RoleGroupConfigSpec
		}
		if rg.OverridesSpec != nil {
			adapted.ConfigOverrides = rg.ConfigOverrides
			adapted.EnvOverrides = rg.EnvOverrides
			adapted.CliOverrides = rg.CliOverrides
			adapted.PodOverrides = rg.PodOverrides
		}
		groups[name] = adapted
	}
	roleSpec.RoleGroups = groups

	return roleSpec
}

// +kubebuilder:object:root=true

// AirflowClusterList contains a list of AirflowCluster.
type AirflowClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AirflowCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AirflowCluster{}, &AirflowClusterList{})
}
