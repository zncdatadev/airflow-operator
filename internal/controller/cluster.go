package controller

import (
	"context"

	"github.com/zncdatadev/operator-go/pkg/client"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	"github.com/zncdatadev/operator-go/pkg/util"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
	"github.com/zncdatadev/airflow-operator/internal/controller/role"
	airflowversion "github.com/zncdatadev/airflow-operator/internal/util/version"
)

var _ reconciler.Reconciler = &ClusterReconciler{}

type ClusterReconciler struct {
	reconciler.BaseCluster[*airflowv1alpha1.AirflowClusterSpec]
	ClusterConfig *airflowv1alpha1.ClusterConfigSpec
}

func NewClusterReconciler(
	client *client.Client,
	clusterInfo reconciler.ClusterInfo,
	spec *airflowv1alpha1.AirflowClusterSpec,
) *ClusterReconciler {
	return &ClusterReconciler{
		BaseCluster: *reconciler.NewBaseCluster(
			client,
			clusterInfo,
			spec.ClusterOperation,
			spec,
		),
		ClusterConfig: spec.ClusterConfig,
	}
}

func (r *ClusterReconciler) GetImage() *util.Image {
	image := util.NewImage(
		airflowv1alpha1.DefaultProductName,
		airflowversion.BuildVersion,
		airflowv1alpha1.DefaultProductVersion,
		func(options *util.ImageOptions) {
			options.Custom = r.Spec.Image.Custom
			options.Repo = r.Spec.Image.Repo
			options.PullPolicy = r.Spec.Image.PullPolicy
		},
	)

	if r.Spec.Image.KubedoopVersion != "" {
		image.KubedoopVersion = r.Spec.Image.KubedoopVersion
	}

	return image
}

func (r *ClusterReconciler) RegisterResource(ctx context.Context) error {

	celery := role.NewCeleryExecutorsReconciler(
		r.Client,
		r.IsStopped(),
		r.ClusterConfig,
		reconciler.RoleInfo{
			ClusterInfo: r.ClusterInfo,
			RoleName:    string(airflowv1alpha1.CeleryExecutorsRoleName),
		},
		r.GetImage(),
		r.Spec.CeleryExecutors,
	)
	if err := celery.RegisterResources(ctx); err != nil {
		return err
	}

	r.AddResource(celery)

	schedulers := role.NewSchedulersReconciler(
		r.Client,
		r.IsStopped(),
		r.ClusterConfig,
		reconciler.RoleInfo{
			ClusterInfo: r.ClusterInfo,
			RoleName:    string(airflowv1alpha1.SchedulersRoleName),
		},
		r.GetImage(),
		r.Spec.Schedulers,
	)
	if err := schedulers.RegisterResources(ctx); err != nil {
		return err
	}

	r.AddResource(schedulers)

	webservers := role.NewWebserversReconciler(
		r.Client,
		r.IsStopped(),
		r.ClusterConfig,
		reconciler.RoleInfo{
			ClusterInfo: r.ClusterInfo,
			RoleName:    string(airflowv1alpha1.WebserversRoleName),
		},
		r.GetImage(),
		r.Spec.Webservers,
	)
	if err := webservers.RegisterResources(ctx); err != nil {
		return err
	}

	r.AddResource(webservers)

	return nil
}
