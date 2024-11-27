package role

import (
	"context"

	commonsv1alpha1 "github.com/zncdatadev/operator-go/pkg/apis/commons/v1alpha1"
	"github.com/zncdatadev/operator-go/pkg/builder"
	"github.com/zncdatadev/operator-go/pkg/client"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/util"
	corev1 "k8s.io/api/core/v1"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	common "github.com/zncdatadev/airflow-operator/internal/controller/common"
)

var _ reconciler.RoleReconciler = &SchedulersReconciler{}

type SchedulersReconciler struct {
	reconciler.BaseRoleReconciler[*airflowv1alpha1.SchedulersSpec]
	ClusterConfig *airflowv1alpha1.ClusterConfigSpec
	Image         *util.Image
}

func NewSchedulersReconciler(
	client *client.Client,
	clusterStopped bool,
	clusterConfig *airflowv1alpha1.ClusterConfigSpec,
	roleInfo reconciler.RoleInfo,
	image *util.Image,
	spec *airflowv1alpha1.SchedulersSpec,
) *SchedulersReconciler {
	return &SchedulersReconciler{
		BaseRoleReconciler: *reconciler.NewBaseRoleReconciler(client, clusterStopped, roleInfo, spec),
		ClusterConfig:      clusterConfig,
		Image:              image,
	}
}

func (r *SchedulersReconciler) RegisterResources(ctx context.Context) error {
	for name, roleGroup := range r.Spec.RoleGroups {
		mergedRoleGroupConfig, err := util.MergeObject(r.Spec.Config, roleGroup.Config)
		if err != nil {
			return err
		}

		mergedOverrides, err := util.MergeObject(r.Spec.OverridesSpec, roleGroup.OverridesSpec)
		if err != nil {
			return err
		}

		info := reconciler.RoleGroupInfo{
			RoleInfo:      r.RoleInfo,
			RoleGroupName: name,
		}

		reconcilers, err := r.RegisterResourceWithRoleGroup(ctx, info, roleGroup.Replicas, mergedRoleGroupConfig, mergedOverrides)

		if err != nil {
			return err
		}

		for _, reconciler := range reconcilers {
			r.AddResource(reconciler)
		}
	}
	return nil
}

func (r *SchedulersReconciler) RegisterResourceWithRoleGroup(
	ctx context.Context,
	info reconciler.RoleGroupInfo,
	replicas *int32,
	config *airflowv1alpha1.ConfigSpec,
	overrides *commonsv1alpha1.OverridesSpec,
) ([]reconciler.Reconciler, error) {

	var auth *common.Authentication
	var err error
	executorType := common.CeleryExecutor

	var commonsRoleGroupConfig *commonsv1alpha1.RoleGroupConfigSpec
	if config != nil {
		commonsRoleGroupConfig = config.RoleGroupConfigSpec
	}

	if len(r.ClusterConfig.Authentication) > 0 {
		auth, err = common.NewAuthentication(ctx, r.Client, r.ClusterConfig.Authentication)
		if err != nil {
			return nil, err
		}
	}

	options := func(o *builder.Options) {
		o.ClusterName = info.GetClusterName()
		o.RoleName = info.GetRoleName()
		o.RoleGroupName = info.GetGroupName()

		o.Labels = info.GetLabels()
		o.Annotations = info.GetAnnotations()
	}

	configmapReconciler := common.NewConfigReconciler(
		r.Client,
		r.ClusterConfig,
		config,
		info,
		auth,
		options,
	)

	deploymentReconciler, err := common.NewStatefulSetReconciler(
		r.Client,
		info,
		r.ClusterConfig,
		make([]corev1.ContainerPort, 0),
		r.Image,
		replicas,
		r.ClusterStopped(),
		overrides,
		commonsRoleGroupConfig,
		executorType,
		auth,
		options,
	)
	if err != nil {
		return nil, err
	}

	return []reconciler.Reconciler{configmapReconciler, deploymentReconciler}, nil
}
