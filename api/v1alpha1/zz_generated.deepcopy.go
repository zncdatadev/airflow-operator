//go:build !ignore_autogenerated

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

// Code generated by controller-gen. DO NOT EDIT.

package v1alpha1

import (
	authenticationv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/authentication/v1alpha1"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AirflowCluster) DeepCopyInto(out *AirflowCluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AirflowCluster.
func (in *AirflowCluster) DeepCopy() *AirflowCluster {
	if in == nil {
		return nil
	}
	out := new(AirflowCluster)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *AirflowCluster) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AirflowClusterList) DeepCopyInto(out *AirflowClusterList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]AirflowCluster, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AirflowClusterList.
func (in *AirflowClusterList) DeepCopy() *AirflowClusterList {
	if in == nil {
		return nil
	}
	out := new(AirflowClusterList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *AirflowClusterList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AirflowClusterSpec) DeepCopyInto(out *AirflowClusterSpec) {
	*out = *in
	if in.Image != nil {
		in, out := &in.Image, &out.Image
		*out = new(ImageSpec)
		**out = **in
	}
	if in.ClusterOperation != nil {
		in, out := &in.ClusterOperation, &out.ClusterOperation
		*out = new(commonsv1alpha1.ClusterOperationSpec)
		**out = **in
	}
	if in.ClusterConfig != nil {
		in, out := &in.ClusterConfig, &out.ClusterConfig
		*out = new(ClusterConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.CeleryExecutors != nil {
		in, out := &in.CeleryExecutors, &out.CeleryExecutors
		*out = new(CeleryExecutorsSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.KubernetesExecutors != nil {
		in, out := &in.KubernetesExecutors, &out.KubernetesExecutors
		*out = new(KubernetesExecutorsSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Schedulers != nil {
		in, out := &in.Schedulers, &out.Schedulers
		*out = new(SchedulersSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Webservers != nil {
		in, out := &in.Webservers, &out.Webservers
		*out = new(WebserversSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AirflowClusterSpec.
func (in *AirflowClusterSpec) DeepCopy() *AirflowClusterSpec {
	if in == nil {
		return nil
	}
	out := new(AirflowClusterSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AirflowClusterStatus) DeepCopyInto(out *AirflowClusterStatus) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AirflowClusterStatus.
func (in *AirflowClusterStatus) DeepCopy() *AirflowClusterStatus {
	if in == nil {
		return nil
	}
	out := new(AirflowClusterStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AuthenticationSpec) DeepCopyInto(out *AuthenticationSpec) {
	*out = *in
	if in.Oidc != nil {
		in, out := &in.Oidc, &out.Oidc
		*out = new(authenticationv1alpha1.OidcSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AuthenticationSpec.
func (in *AuthenticationSpec) DeepCopy() *AuthenticationSpec {
	if in == nil {
		return nil
	}
	out := new(AuthenticationSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *CeleryExecutorsSpec) DeepCopyInto(out *CeleryExecutorsSpec) {
	*out = *in
	if in.RoleGroups != nil {
		in, out := &in.RoleGroups, &out.RoleGroups
		*out = make(map[string]RoleGroupSpec, len(*in))
		for key, val := range *in {
			(*out)[key] = *val.DeepCopy()
		}
	}
	if in.RoleConfig != nil {
		in, out := &in.RoleConfig, &out.RoleConfig
		*out = new(commonsv1alpha1.RoleConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Config != nil {
		in, out := &in.Config, &out.Config
		*out = new(ConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.OverridesSpec != nil {
		in, out := &in.OverridesSpec, &out.OverridesSpec
		*out = new(commonsv1alpha1.OverridesSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new CeleryExecutorsSpec.
func (in *CeleryExecutorsSpec) DeepCopy() *CeleryExecutorsSpec {
	if in == nil {
		return nil
	}
	out := new(CeleryExecutorsSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ClusterConfigSpec) DeepCopyInto(out *ClusterConfigSpec) {
	*out = *in
	if in.Authentication != nil {
		in, out := &in.Authentication, &out.Authentication
		*out = make([]AuthenticationSpec, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.DagsGitSync != nil {
		in, out := &in.DagsGitSync, &out.DagsGitSync
		*out = make([]DagsGitSyncSpec, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.Volumes != nil {
		in, out := &in.Volumes, &out.Volumes
		*out = make([]runtime.RawExtension, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.VolumeMounts != nil {
		in, out := &in.VolumeMounts, &out.VolumeMounts
		*out = make([]runtime.RawExtension, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new ClusterConfigSpec.
func (in *ClusterConfigSpec) DeepCopy() *ClusterConfigSpec {
	if in == nil {
		return nil
	}
	out := new(ClusterConfigSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ConfigSpec) DeepCopyInto(out *ConfigSpec) {
	*out = *in
	if in.RoleGroupConfigSpec != nil {
		in, out := &in.RoleGroupConfigSpec, &out.RoleGroupConfigSpec
		*out = new(commonsv1alpha1.RoleGroupConfigSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new ConfigSpec.
func (in *ConfigSpec) DeepCopy() *ConfigSpec {
	if in == nil {
		return nil
	}
	out := new(ConfigSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *DagsGitSyncSpec) DeepCopyInto(out *DagsGitSyncSpec) {
	*out = *in
	if in.Depth != nil {
		in, out := &in.Depth, &out.Depth
		*out = new(int8)
		**out = **in
	}
	if in.GitSyncConf != nil {
		in, out := &in.GitSyncConf, &out.GitSyncConf
		*out = make(map[string]string, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
	if in.Wait != nil {
		in, out := &in.Wait, &out.Wait
		*out = new(int16)
		**out = **in
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new DagsGitSyncSpec.
func (in *DagsGitSyncSpec) DeepCopy() *DagsGitSyncSpec {
	if in == nil {
		return nil
	}
	out := new(DagsGitSyncSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ImageSpec) DeepCopyInto(out *ImageSpec) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new ImageSpec.
func (in *ImageSpec) DeepCopy() *ImageSpec {
	if in == nil {
		return nil
	}
	out := new(ImageSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *KubernetesExecutorsSpec) DeepCopyInto(out *KubernetesExecutorsSpec) {
	*out = *in
	if in.RoleConfig != nil {
		in, out := &in.RoleConfig, &out.RoleConfig
		*out = new(commonsv1alpha1.RoleConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Config != nil {
		in, out := &in.Config, &out.Config
		*out = new(ConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.OverridesSpec != nil {
		in, out := &in.OverridesSpec, &out.OverridesSpec
		*out = new(commonsv1alpha1.OverridesSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.RoleGroupConfigSpec != nil {
		in, out := &in.RoleGroupConfigSpec, &out.RoleGroupConfigSpec
		*out = new(commonsv1alpha1.RoleGroupConfigSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new KubernetesExecutorsSpec.
func (in *KubernetesExecutorsSpec) DeepCopy() *KubernetesExecutorsSpec {
	if in == nil {
		return nil
	}
	out := new(KubernetesExecutorsSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *RoleGroupSpec) DeepCopyInto(out *RoleGroupSpec) {
	*out = *in
	if in.Replicas != nil {
		in, out := &in.Replicas, &out.Replicas
		*out = new(int32)
		**out = **in
	}
	if in.Config != nil {
		in, out := &in.Config, &out.Config
		*out = new(ConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.OverridesSpec != nil {
		in, out := &in.OverridesSpec, &out.OverridesSpec
		*out = new(commonsv1alpha1.OverridesSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new RoleGroupSpec.
func (in *RoleGroupSpec) DeepCopy() *RoleGroupSpec {
	if in == nil {
		return nil
	}
	out := new(RoleGroupSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SchedulersSpec) DeepCopyInto(out *SchedulersSpec) {
	*out = *in
	if in.RoleGroups != nil {
		in, out := &in.RoleGroups, &out.RoleGroups
		*out = make(map[string]RoleGroupSpec, len(*in))
		for key, val := range *in {
			(*out)[key] = *val.DeepCopy()
		}
	}
	if in.RoleConfig != nil {
		in, out := &in.RoleConfig, &out.RoleConfig
		*out = new(commonsv1alpha1.RoleConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Config != nil {
		in, out := &in.Config, &out.Config
		*out = new(ConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.OverridesSpec != nil {
		in, out := &in.OverridesSpec, &out.OverridesSpec
		*out = new(commonsv1alpha1.OverridesSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SchedulersSpec.
func (in *SchedulersSpec) DeepCopy() *SchedulersSpec {
	if in == nil {
		return nil
	}
	out := new(SchedulersSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *WebserversSpec) DeepCopyInto(out *WebserversSpec) {
	*out = *in
	if in.RoleGroups != nil {
		in, out := &in.RoleGroups, &out.RoleGroups
		*out = make(map[string]RoleGroupSpec, len(*in))
		for key, val := range *in {
			(*out)[key] = *val.DeepCopy()
		}
	}
	if in.RoleConfig != nil {
		in, out := &in.RoleConfig, &out.RoleConfig
		*out = new(commonsv1alpha1.RoleConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Config != nil {
		in, out := &in.Config, &out.Config
		*out = new(ConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.OverridesSpec != nil {
		in, out := &in.OverridesSpec, &out.OverridesSpec
		*out = new(commonsv1alpha1.OverridesSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new WebserversSpec.
func (in *WebserversSpec) DeepCopy() *WebserversSpec {
	if in == nil {
		return nil
	}
	out := new(WebserversSpec)
	in.DeepCopyInto(out)
	return out
}
