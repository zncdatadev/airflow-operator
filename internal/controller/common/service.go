package commons

import (
	"fmt"
	"maps"

	"github.com/zncdatadev/operator-go/pkg/builder"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	corev1 "k8s.io/api/core/v1"
)

// Create Service Reconciler
func GetServiceReconciler(roleReconciler reconciler.RoleReconciler, rgInfo reconciler.RoleGroupInfo, ports []corev1.ContainerPort) *reconciler.Service {
	metricsPort := 0
	for _, port := range ports {
		if port.Name == "metrics" {
			metricsPort = int(port.ContainerPort)
			break
		}
	}
	if metricsPort > 0 {
		annotations := rgInfo.GetAnnotations()
		maps.Copy(annotations, getPrometheusAnnotations(int32(metricsPort)))

		svcReconciler := reconciler.NewServiceReconciler(
			roleReconciler.GetClient(),
			rgInfo.GetFullName(),
			[]corev1.ContainerPort{
				{
					Name:          "metrics",
					ContainerPort: int32(metricsPort),
				},
			},
			func(o *builder.ServiceBuilderOptions) {
				o.Annotations = annotations
			},
		)
		return svcReconciler

	}
	return nil
}

// Common annotations for Prometheus scraping
func getPrometheusAnnotations(port int32) map[string]string {
	return map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   fmt.Sprintf("%d", port),
		"prometheus.io/path":   "/metrics",
		"prometheus.io/scheme": "http",
	}
}
