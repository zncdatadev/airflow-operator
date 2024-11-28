package commons

type ExecutorType int32

const (
	LocalExecutor ExecutorType = iota
	CeleryExecutor
	KubernetesExecutor
)

func GetExecutorName(executor ExecutorType) string {
	switch executor {
	case LocalExecutor:
		return "LocalExecutor"
	case CeleryExecutor:
		return "CeleryExecutor"
	case KubernetesExecutor:
		return "KubernetesExecutor"
	default:
		return "UnknownExecutor"
	}
}
