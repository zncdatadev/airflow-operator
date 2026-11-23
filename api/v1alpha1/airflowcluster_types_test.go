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
	"reflect"
	"testing"

	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestToGenericSpecMapsImage(t *testing.T) {
	t.Parallel()

	spec := AirflowClusterSpec{
		Image: &ImageSpec{
			Custom:          "registry.example.com/airflow:test",
			Repo:            "registry.example.com/products",
			ProductVersion:  "2.10.5",
			KubedoopVersion: "1.2.3",
			PullPolicy:      corev1.PullAlways,
			PullSecretName:  "private-registry",
		},
	}

	want := &commonsv1alpha1.ImageSpec{
		Custom:          "registry.example.com/airflow:test",
		Repo:            "registry.example.com/products",
		ProductVersion:  "2.10.5",
		KubedoopVersion: "1.2.3",
		PullPolicy:      corev1.PullAlways,
		PullSecretName:  "private-registry",
	}
	if got := spec.ToGenericSpec().Image; !reflect.DeepEqual(got, want) {
		t.Fatalf("ToGenericSpec() image = %#v, want %#v", got, want)
	}
}

func TestToGenericSpecReleasesLegacyDevelopmentVersionDefault(t *testing.T) {
	t.Parallel()

	spec := AirflowClusterSpec{
		Image: &ImageSpec{KubedoopVersion: legacyDefaultKubedoopVersion},
	}
	if got := spec.ToGenericSpec().Image.KubedoopVersion; got != "" {
		t.Fatalf("ToGenericSpec() kubedoopVersion = %q, want runtime default", got)
	}
}

func TestVectorAggregatorConfigMapName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cluster *AirflowCluster
		want    string
	}{
		{
			name:    "nil receiver",
			cluster: nil,
		},
		{
			name:    "nil cluster config",
			cluster: &AirflowCluster{},
		},
		{
			name: "configured aggregator",
			cluster: &AirflowCluster{
				Spec: AirflowClusterSpec{
					ClusterConfig: &ClusterConfigSpec{
						VectorAggregatorConfigMapName: "vector-aggregator-discovery",
					},
				},
			},
			want: "vector-aggregator-discovery",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.cluster.VectorAggregatorConfigMapName(); got != tt.want {
				t.Fatalf("VectorAggregatorConfigMapName() = %q, want %q", got, tt.want)
			}
		})
	}
}
