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
	"context"

	"github.com/zncdatadev/operator-go/pkg/client"
	"github.com/zncdatadev/operator-go/pkg/reconciler"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	airflowv1alpha1 "github.com/zncdatadev/airflow-operator/api/v1alpha1"
)

var logger = ctrl.Log.WithName("controller")

// AirflowClustersReconciler reconciles a AirflowClusters object
type AirflowClustersReconciler struct {
	ctrlclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airflow.kubedoop.dev,resources=airflowclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=authentication.kubedoop.dev,resources=authenticationclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

func (r *AirflowClustersReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	logger.Info("Reconciling AirflowCluster")

	instance := &airflowv1alpha1.AirflowClusters{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if ctrlclient.IgnoreNotFound(err) == nil {
			logger.V(1).Info("AirflowCluster resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	resourceClient := &client.Client{
		Client:         r.Client,
		OwnerReference: instance,
	}

	clusterInfo := reconciler.ClusterInfo{
		GVK: &metav1.GroupVersionKind{
			Group:   airflowv1alpha1.GroupVersion.Group,
			Version: airflowv1alpha1.GroupVersion.Version,
			Kind:    "AirflowClusters",
		},
		ClusterName: instance.Name,
	}

	reconciler := NewClusterReconciler(resourceClient, clusterInfo, &instance.Spec)

	if err := reconciler.RegisterResource(ctx); err != nil {
		return ctrl.Result{}, err
	}

	return reconciler.Run(ctx)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AirflowClustersReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airflowv1alpha1.AirflowClusters{}).
		Named("airflowclusters").
		Complete(r)
}
