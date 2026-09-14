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
	"testing"

	. "github.com/onsi/gomega"
	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/config"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/sidecar"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

const testManagedVectorImage = "managed-vector:latest"

func TestLegacyManagedSidecarOverridesMoveToNativeSidecars(t *testing.T) {
	const managedValue = "kept"
	for _, name := range []string{metricsSidecarName, vectorSidecarName} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			manager := sidecar.NewSidecarManager()
			manager.Register(sidecar.NewStaticContainerProvider(corev1.Container{
				Name:            name,
				Image:           "managed:latest",
				Command:         []string{"/managed-entrypoint"},
				Args:            []string{"--managed"},
				RestartPolicy:   sidecar.SidecarRestartPolicy(),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env:             []corev1.EnvVar{{Name: "MANAGED", Value: managedValue}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "managed-config", MountPath: "/managed/config"},
				},
			}), &sidecar.SidecarConfig{Enabled: true})

			extra := corev1.Container{Name: "user-sidecar", Image: "user:latest"}
			legacy := corev1.Container{
				Name: name,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")},
				},
				Env: []corev1.EnvVar{{Name: "LEGACY_OVERRIDE", Value: name}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "user-data", MountPath: "/user/data"},
				},
			}
			buildCtx := &reconciler.RoleGroupBuildContext{
				SidecarManager: manager,
				MergedConfig: &config.MergedConfig{PodOverrides: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{legacy, extra}},
				}},
			}

			captured, err := captureManagedSidecarOverrides(buildCtx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(captured).To(HaveLen(1))
			g.Expect(buildCtx.MergedConfig.PodOverrides.Spec.Containers).To(Equal([]corev1.Container{extra}))

			podSpec := corev1.PodSpec{}
			g.Expect(manager.InjectAll(&podSpec)).To(Succeed())
			g.Expect(applyManagedSidecarOverrides(&podSpec, captured)).To(Succeed())
			g.Expect(podSpec.Containers).To(BeEmpty())
			g.Expect(podSpec.InitContainers).To(HaveLen(1))

			native := &podSpec.InitContainers[0]
			g.Expect(native.Name).To(Equal(name))
			g.Expect(native.Image).To(Equal("managed:latest"))
			g.Expect(native.Command).To(Equal([]string{"/managed-entrypoint"}))
			g.Expect(native.Args).To(Equal([]string{"--managed"}))
			g.Expect(native.RestartPolicy).NotTo(BeNil())
			g.Expect(*native.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
			g.Expect(native.Resources.Requests.Cpu().String()).To(Equal("25m"))
			g.Expect(native.Env).To(ContainElements(
				corev1.EnvVar{Name: "MANAGED", Value: managedValue},
				corev1.EnvVar{Name: "LEGACY_OVERRIDE", Value: name},
			))
			g.Expect(native.VolumeMounts).To(ContainElements(
				corev1.VolumeMount{Name: "managed-config", MountPath: "/managed/config"},
				corev1.VolumeMount{Name: "user-data", MountPath: "/user/data"},
			))
		})
	}
}

func TestExplicitNativeSidecarOverrideWinsOverLegacyShape(t *testing.T) {
	const enabledValue = "true"
	g := NewWithT(t)
	manager := sidecar.NewSidecarManager()
	manager.Register(sidecar.NewStaticContainerProvider(corev1.Container{
		Name:          vectorSidecarName,
		Image:         testManagedVectorImage,
		Command:       []string{"/managed-vector"},
		RestartPolicy: sidecar.SidecarRestartPolicy(),
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health"},
		}},
	}), &sidecar.SidecarConfig{Enabled: true})

	legacy := corev1.Container{
		Name: vectorSidecarName,
		Env:  []corev1.EnvVar{{Name: "LEGACY_INHERITED", Value: enabledValue}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")},
		},
	}
	explicit := corev1.Container{
		Name:    vectorSidecarName,
		Command: []string{"/explicit-vector"},
		Env:     []corev1.EnvVar{{Name: "EXPLICIT_NATIVE", Value: enabledValue}},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"/check-vector"}},
		}},
	}
	extra := corev1.Container{Name: "user-sidecar", Image: "user:latest"}
	userInit := corev1.Container{Name: "user-init", Image: "init:latest"}
	buildCtx := &reconciler.RoleGroupBuildContext{
		SidecarManager: manager,
		MergedConfig: &config.MergedConfig{PodOverrides: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers:     []corev1.Container{legacy, extra},
				InitContainers: []corev1.Container{explicit, userInit},
			},
		}},
	}

	captured, err := captureManagedSidecarOverrides(buildCtx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(captured).To(HaveLen(1))
	g.Expect(captured[0].Command).To(Equal(explicit.Command))
	g.Expect(captured[0].Env).To(ContainElements(legacy.Env[0], explicit.Env[0]))
	g.Expect(captured[0].Resources.Requests.Cpu().String()).To(Equal("25m"))
	g.Expect(captured[0].Resources.Limits.Memory().String()).To(Equal("128Mi"))
	g.Expect(buildCtx.MergedConfig.PodOverrides.Spec.Containers).To(Equal([]corev1.Container{extra}))
	g.Expect(buildCtx.MergedConfig.PodOverrides.Spec.InitContainers).To(Equal([]corev1.Container{userInit}))

	podSpec := corev1.PodSpec{}
	g.Expect(manager.InjectAll(&podSpec)).To(Succeed())
	g.Expect(applyManagedSidecarOverrides(&podSpec, captured)).To(Succeed())
	native := &podSpec.InitContainers[0]
	g.Expect(native.Image).To(Equal(testManagedVectorImage))
	g.Expect(native.Command).To(Equal([]string{"/explicit-vector"}))
	g.Expect(native.RestartPolicy).NotTo(BeNil())
	g.Expect(*native.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
	g.Expect(native.Env).To(ContainElements(legacy.Env[0], explicit.Env[0]))
	g.Expect(native.Resources.Requests.Cpu().String()).To(Equal("25m"))
	g.Expect(native.Resources.Limits.Memory().String()).To(Equal("128Mi"))
	g.Expect(native.LivenessProbe.Exec).NotTo(BeNil())
	g.Expect(native.LivenessProbe.HTTPGet).To(BeNil())
}

func TestManagedSidecarOverrideUsesHighestPrecedenceLayer(t *testing.T) {
	tests := []struct {
		name      string
		role      string
		roleGroup string
	}{
		{
			name: "role group legacy shape beats role native shape",
			role: `{"spec":{"initContainers":[{"name":"vector","command":["/role"],
				"env":[{"name":"SHARED","value":"role"},{"name":"ROLE_ONLY","value":"kept"}],
				"resources":{"requests":{"cpu":"25m"}},
				"lifecycle":{"preStop":{"exec":{"command":["/role-stop"]}}}}]}}`,
			roleGroup: `{"spec":{"containers":[{"name":"vector","command":["/group"],
				"env":[{"name":"SHARED","value":"group"},{"name":"GROUP_ONLY","value":"kept"}],
				"lifecycle":{"preStop":{"httpGet":{"path":"/stop","port":8080}}}}]}}`,
		},
		{
			name: "role group native shape beats role legacy shape",
			role: `{"spec":{"containers":[{"name":"vector","command":["/role"],
				"env":[{"name":"SHARED","value":"role"},{"name":"ROLE_ONLY","value":"kept"}],
				"resources":{"requests":{"cpu":"25m"}},
				"lifecycle":{"preStop":{"exec":{"command":["/role-stop"]}}}}]}}`,
			roleGroup: `{"spec":{"initContainers":[{"name":"vector","command":["/group"],
				"env":[{"name":"SHARED","value":"group"},{"name":"GROUP_ONLY","value":"kept"}],
				"lifecycle":{"preStop":{"httpGet":{"path":"/stop","port":8080}}}}]}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			manager := sidecar.NewSidecarManager()
			manager.Register(sidecar.NewStaticContainerProvider(corev1.Container{
				Name: vectorSidecarName, Image: testManagedVectorImage,
			}), &sidecar.SidecarConfig{Enabled: true})

			role := &commonsv1alpha1.RoleSpec{
				PodOverrides: &k8sruntime.RawExtension{Raw: []byte(tt.role)},
			}
			roleGroup := commonsv1alpha1.RoleGroupSpec{
				PodOverrides: &k8sruntime.RawExtension{Raw: []byte(tt.roleGroup)},
			}
			buildCtx := &reconciler.RoleGroupBuildContext{
				RoleSpec:       role,
				RoleGroupSpec:  roleGroup,
				SidecarManager: manager,
				MergedConfig: config.NewConfigMerger().Merge(
					role.GetOverrides(),
					roleGroup.GetOverrides(),
				),
			}

			captured, err := captureManagedSidecarOverrides(buildCtx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(captured).To(HaveLen(1))
			g.Expect(captured[0].Command).To(Equal([]string{"/group"}))
			g.Expect(captured[0].Env).To(ContainElements(
				corev1.EnvVar{Name: "SHARED", Value: "group"},
				corev1.EnvVar{Name: "ROLE_ONLY", Value: "kept"},
				corev1.EnvVar{Name: "GROUP_ONLY", Value: "kept"},
			))
			g.Expect(captured[0].Env).NotTo(ContainElement(corev1.EnvVar{Name: "SHARED", Value: "role"}))
			g.Expect(captured[0].Resources.Requests.Cpu().String()).To(Equal("25m"))
			g.Expect(captured[0].Lifecycle).NotTo(BeNil())
			g.Expect(captured[0].Lifecycle.PreStop).NotTo(BeNil())
			g.Expect(captured[0].Lifecycle.PreStop.Exec).To(BeNil())
			g.Expect(captured[0].Lifecycle.PreStop.HTTPGet).NotTo(BeNil())
			g.Expect(buildCtx.MergedConfig.PodOverrides.Spec.Containers).To(BeEmpty())
			g.Expect(buildCtx.MergedConfig.PodOverrides.Spec.InitContainers).To(BeEmpty())
		})
	}
}

func TestDisabledManagedSidecarIsNotMigrated(t *testing.T) {
	g := NewWithT(t)
	manager := sidecar.NewSidecarManager()
	manager.Register(sidecar.NewStaticContainerProvider(corev1.Container{
		Name: vectorSidecarName, Image: testManagedVectorImage,
	}), &sidecar.SidecarConfig{Enabled: false})
	legacy := corev1.Container{Name: vectorSidecarName, Image: "user-vector:latest"}
	podOverrides := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{legacy}},
	}
	buildCtx := &reconciler.RoleGroupBuildContext{
		SidecarManager: manager,
		MergedConfig:   &config.MergedConfig{PodOverrides: podOverrides},
	}

	captured, err := captureManagedSidecarOverrides(buildCtx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(captured).To(BeEmpty())
	g.Expect(buildCtx.MergedConfig.PodOverrides).To(BeIdenticalTo(podOverrides))
	g.Expect(buildCtx.MergedConfig.PodOverrides.Spec.Containers).To(Equal([]corev1.Container{legacy}))
}
