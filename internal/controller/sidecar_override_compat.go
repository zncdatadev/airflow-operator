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
	"encoding/json"
	"fmt"

	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/sidecar"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// captureManagedSidecarOverrides bridges pod overrides written before operator-go v0.13 moved
// managed sidecars from PodSpec.Containers to native sidecars in PodSpec.InitContainers. The
// framework applies podOverrides before injecting its sidecars, so leaving a legacy patch in the
// regular-container list creates a second, usually image-less container; putting the patch in the
// new list does not help by itself because the provider subsequently replaces that entry.
//
// Only sidecars which are actually registered and enabled are migrated. A disabled Vector entry
// remains an ordinary user-owned container/init container. If both shapes address the same
// managed sidecar at one layer, they are normalized into one logical patch with the explicit
// v0.13 initContainers shape winning only conflicting fields. Logical role and role-group patches
// are then strategic-merged in normal precedence order, so a role-group conflict wins while
// unrelated role-level fields remain inherited. Other override containers are preserved on the
// copied PodTemplateSpec.
//
// TODO(operator-go): promote — https://github.com/zncdatadev/operator-go/issues/643
func captureManagedSidecarOverrides(
	buildCtx *reconciler.RoleGroupBuildContext,
) ([]corev1.Container, error) {
	if buildCtx == nil || buildCtx.MergedConfig == nil ||
		buildCtx.MergedConfig.PodOverrides == nil || buildCtx.SidecarManager == nil {
		return nil, nil
	}

	overrides := buildCtx.MergedConfig.PodOverrides.DeepCopy()
	var captured []corev1.Container
	changed := false
	for _, name := range []string{metricsSidecarName, vectorSidecarName} {
		if !managedSidecarEnabled(buildCtx.SidecarManager, name) {
			continue
		}

		containers, legacy := removeContainerOverride(overrides.Spec.Containers, name)
		initContainers, explicit := removeContainerOverride(overrides.Spec.InitContainers, name)
		if legacy == nil && explicit == nil {
			continue
		}
		logical, foundLayer, err := layeredManagedSidecarOverride(buildCtx, name)
		if err != nil {
			return nil, fmt.Errorf("merge podOverride layers for managed sidecar %q: %w", name, err)
		}
		if !foundLayer {
			logical, _, err = managedSidecarOverrideFromPodSpec(&overrides.Spec, name)
			if err != nil {
				return nil, fmt.Errorf("merge podOverride shapes for managed sidecar %q: %w", name, err)
			}
		}
		if logical == nil {
			return nil, fmt.Errorf("managed sidecar %q was present in merged podOverrides but no logical patch could be reconstructed", name)
		}

		overrides.Spec.Containers = containers
		overrides.Spec.InitContainers = initContainers
		changed = true
		captured = append(captured, *logical)
	}

	if changed {
		buildCtx.MergedConfig.PodOverrides = overrides
	}
	return captured, nil
}

// layeredManagedSidecarOverride reconstructs the user's logical sidecar patch before the role and
// role-group PodTemplates lose their precedence boundary in MergedConfig. Airflow contributes no
// product-default podOverride; callers that provide only a synthetic MergedConfig use the fallback
// in captureManagedSidecarOverrides.
func layeredManagedSidecarOverride(
	buildCtx *reconciler.RoleGroupBuildContext,
	name string,
) (*corev1.Container, bool, error) {
	var logical *corev1.Container
	found := false
	if buildCtx.RoleSpec != nil && buildCtx.RoleSpec.PodOverrides != nil {
		layer, present, err := managedSidecarOverrideFromRaw(buildCtx.RoleSpec.PodOverrides.Raw, name)
		if err != nil {
			return nil, false, fmt.Errorf("decode role podOverrides: %w", err)
		}
		if present {
			logical = layer
			found = true
		}
	}
	if raw := buildCtx.RoleGroupSpec.PodOverrides; raw != nil {
		layer, present, err := managedSidecarOverrideFromRaw(raw.Raw, name)
		if err != nil {
			return nil, false, fmt.Errorf("decode role-group podOverrides: %w", err)
		}
		if present {
			logical, err = mergeManagedSidecarContainers(logical, layer)
			if err != nil {
				return nil, false, err
			}
			found = true
		}
	}
	return logical, found, nil
}

func managedSidecarOverrideFromRaw(
	raw []byte,
	name string,
) (*corev1.Container, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var override corev1.PodTemplateSpec
	if err := json.Unmarshal(raw, &override); err != nil {
		return nil, false, err
	}
	return managedSidecarOverrideFromPodSpec(&override.Spec, name)
}

func managedSidecarOverrideFromPodSpec(
	podSpec *corev1.PodSpec,
	name string,
) (*corev1.Container, bool, error) {
	_, legacy := removeContainerOverride(podSpec.Containers, name)
	_, explicit := removeContainerOverride(podSpec.InitContainers, name)
	if legacy == nil && explicit == nil {
		return nil, false, nil
	}
	logical, err := mergeManagedSidecarContainers(legacy, explicit)
	if err != nil {
		return nil, false, err
	}
	return logical, true, nil
}

func managedSidecarEnabled(manager *sidecar.SidecarManager, name string) bool {
	if _, registered := manager.GetProvider(name); !registered {
		return false
	}
	config, configured := manager.GetConfig(name)
	return !configured || config == nil || config.Enabled
}

func removeContainerOverride(
	containers []corev1.Container,
	name string,
) ([]corev1.Container, *corev1.Container) {
	kept := make([]corev1.Container, 0, len(containers))
	var found *corev1.Container
	for i := range containers {
		if containers[i].Name != name {
			kept = append(kept, containers[i])
			continue
		}
		if found == nil {
			found = containers[i].DeepCopy()
		}
	}
	return kept, found
}

func mergeManagedSidecarContainers(
	base, override *corev1.Container,
) (*corev1.Container, error) {
	switch {
	case base == nil && override == nil:
		return nil, nil
	case base == nil:
		return override.DeepCopy(), nil
	case override == nil:
		return base.DeepCopy(), nil
	}
	if base.Name != override.Name {
		return nil, fmt.Errorf("cannot merge sidecar patches with different names %q and %q", base.Name, override.Name)
	}

	base = base.DeepCopy()
	override = override.DeepCopy()
	clearManagedSidecarOverrideUnions(base, override)
	baseJSON, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("marshal base sidecar %q: %w", base.Name, err)
	}
	overrideJSON, err := json.Marshal(override)
	if err != nil {
		return nil, fmt.Errorf("marshal sidecar override %q: %w", override.Name, err)
	}
	mergedJSON, err := strategicpatch.StrategicMergePatch(baseJSON, overrideJSON, corev1.Container{})
	if err != nil {
		return nil, fmt.Errorf("strategic-merge sidecar override %q: %w", override.Name, err)
	}

	var merged corev1.Container
	if err := json.Unmarshal(mergedJSON, &merged); err != nil {
		return nil, fmt.Errorf("decode merged sidecar override %q: %w", override.Name, err)
	}
	if merged.Name != override.Name {
		return nil, fmt.Errorf(
			"sidecar override for %q changed its name to %q",
			override.Name,
			merged.Name,
		)
	}
	return &merged, nil
}

// applyManagedSidecarOverrides replays the captured patch after SidecarManager has produced the
// complete native sidecar. Strategic merge keeps provider-owned image, command, mounts and probes
// when the user did not mention them, while retaining Kubernetes merge keys for env, ports and
// volume mounts.
func applyManagedSidecarOverrides(
	podSpec *corev1.PodSpec,
	overrides []corev1.Container,
) error {
	for i := range overrides {
		override := &overrides[i]
		idx := sidecar.FindInitContainerIndex(podSpec, override.Name)
		if idx < 0 {
			return fmt.Errorf(
				"managed sidecar %q was enabled but was not injected before applying podOverrides",
				override.Name,
			)
		}

		merged, err := mergeManagedSidecarContainers(&podSpec.InitContainers[idx], override)
		if err != nil {
			return fmt.Errorf("apply podOverride for managed sidecar %q: %w", override.Name, err)
		}
		podSpec.InitContainers[idx] = *merged
	}
	return nil
}

// clearManagedSidecarOverrideUnions mirrors the Kubernetes one-of handling used by the
// operator-go pod override builder. Without it, replacing Vector's HTTP liveness probe with an
// exec probe (or a literal env value with valueFrom) would strategic-merge both mutually exclusive
// variants into an invalid container.
func clearManagedSidecarOverrideUnions(base, override *corev1.Container) {
	for i := range base.Env {
		for j := range override.Env {
			if base.Env[i].Name != override.Env[j].Name {
				continue
			}
			if override.Env[j].ValueFrom != nil {
				base.Env[i].Value = ""
			}
			if override.Env[j].Value != "" {
				base.Env[i].ValueFrom = nil
			}
		}
	}
	clearManagedSidecarProbeUnion(base.LivenessProbe, override.LivenessProbe)
	clearManagedSidecarProbeUnion(base.ReadinessProbe, override.ReadinessProbe)
	clearManagedSidecarProbeUnion(base.StartupProbe, override.StartupProbe)
	if base.Lifecycle != nil && override.Lifecycle != nil {
		clearManagedSidecarLifecycleUnion(base.Lifecycle.PostStart, override.Lifecycle.PostStart)
		clearManagedSidecarLifecycleUnion(base.Lifecycle.PreStop, override.Lifecycle.PreStop)
	}
}

func clearManagedSidecarProbeUnion(base, override *corev1.Probe) {
	if base == nil || override == nil {
		return
	}
	baseKind := probeHandlerKind(base.ProbeHandler)
	overrideKind := probeHandlerKind(override.ProbeHandler)
	if baseKind != "" && overrideKind != "" && baseKind != overrideKind {
		base.ProbeHandler = corev1.ProbeHandler{}
	}
}

func clearManagedSidecarLifecycleUnion(base, override *corev1.LifecycleHandler) {
	if base == nil || override == nil {
		return
	}
	baseKind := lifecycleHandlerKind(base)
	overrideKind := lifecycleHandlerKind(override)
	if baseKind != "" && overrideKind != "" && baseKind != overrideKind {
		*base = corev1.LifecycleHandler{}
	}
}

func lifecycleHandlerKind(handler *corev1.LifecycleHandler) string {
	switch {
	case handler.Exec != nil:
		return "exec"
	case handler.HTTPGet != nil:
		return "httpGet"
	case handler.TCPSocket != nil:
		return "tcpSocket"
	case handler.Sleep != nil:
		return "sleep"
	default:
		return ""
	}
}

func probeHandlerKind(handler corev1.ProbeHandler) string {
	switch {
	case handler.Exec != nil:
		return "exec"
	case handler.HTTPGet != nil:
		return "httpGet"
	case handler.TCPSocket != nil:
		return "tcpSocket"
	case handler.GRPC != nil:
		return "grpc"
	default:
		return ""
	}
}
