package role

import corev1 "k8s.io/api/core/v1"

var (
	ports = []corev1.ContainerPort{
		{
			Name:          "http",
			ContainerPort: 8080,
			Protocol:      corev1.ProtocolTCP,
		},
		{
			Name:          "metrics",
			ContainerPort: 9012, // statsd exporter port
			Protocol:      corev1.ProtocolTCP,
		},
	}
)
