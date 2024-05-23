/*
Copyright 2024.

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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ctrl "sigs.k8s.io/controller-runtime"

	scanv1 "github.com/jsulthan/clusterscan-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterScanReconciler reconciles a ClusterScan object
type ClusterScanReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=scan.example.com,resources=clusterscans,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=scan.example.com,resources=clusterscans/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=scan.example.com,resources=clusterscans/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ClusterScan object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *ClusterScanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the latest version of the ClusterScan instance
	clusterScan := &scanv1.ClusterScan{}
	err := r.Get(ctx, req.NamespacedName, clusterScan)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("ClusterScan resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get ClusterScan")
		return ctrl.Result{}, err
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(clusterScan, "clusterscan.finalizers.example.com") {
		controllerutil.AddFinalizer(clusterScan, "clusterscan.finalizers.example.com")

		// Fetch the latest version of the resource to get the correct ResourceVersion
		latestClusterScan := &scanv1.ClusterScan{}
		err := r.Get(ctx, req.NamespacedName, latestClusterScan)
		if err != nil {
			log.Error(err, "Failed to get latest ClusterScan for finalizer update")
			return ctrl.Result{}, err
		}

		latestClusterScan.Finalizers = append(latestClusterScan.Finalizers, "clusterscan.finalizers.example.com")
		err = r.Update(ctx, latestClusterScan)
		if err != nil {
			log.Error(err, "Failed to update ClusterScan with finalizer")
			return ctrl.Result{}, err
		}
	}

	// Check if the resource is marked for deletion
	if clusterScan.DeletionTimestamp != nil {
		// Handle finalizer logic
		if controllerutil.ContainsFinalizer(clusterScan, "clusterscan.finalizers.example.com") {
			// Perform cleanup logic (e.g., delete associated Jobs or CronJobs)
			if err := r.cleanupClusterScan(ctx, clusterScan); err != nil {
				return ctrl.Result{}, err
			}

			// Fetch the latest version of the resource to get the correct ResourceVersion
			latestClusterScan := &scanv1.ClusterScan{}
			err := r.Get(ctx, req.NamespacedName, latestClusterScan)
			if err != nil {
				log.Error(err, "Failed to get latest ClusterScan for finalizer removal")
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(latestClusterScan, "clusterscan.finalizers.example.com")
			err = r.Update(ctx, latestClusterScan)
			if err != nil {
				log.Error(err, "Failed to remove finalizer from ClusterScan")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Define the kube-bench job template
	jobTemplate := batchv1.JobTemplateSpec{
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": clusterScan.Name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "kube-bench",
							Image:   "aquasec/kube-bench:latest",
							Command: []string{"kube-bench", "--json"},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	// Handle one-off jobs
	if clusterScan.Spec.Type == "one-off" {
		foundJob := &batchv1.Job{}
		err = r.Get(ctx, types.NamespacedName{Name: clusterScan.Name, Namespace: clusterScan.Namespace}, foundJob)
		if err != nil && errors.IsNotFound(err) {
			// Define a new job
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterScan.Name,
					Namespace: clusterScan.Namespace,
				},
				Spec: jobTemplate.Spec,
			}
			controllerutil.SetControllerReference(clusterScan, job, r.Scheme)
			err = r.Create(ctx, job)
			if err != nil {
				log.Error(err, "Failed to create new Job", "Job.Namespace", job.Namespace, "Job.Name", job.Name)
				return ctrl.Result{}, err
			}
			// Job created successfully - requeue after a minute to check its status
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		} else if err != nil {
			log.Error(err, "Failed to get Job")
			return ctrl.Result{}, err
		}
		// Handle recurring jobs
	} else if clusterScan.Spec.Type == "recurring" {
		foundCronJob := &batchv1.CronJob{}
		err = r.Get(ctx, types.NamespacedName{Name: clusterScan.Name, Namespace: clusterScan.Namespace}, foundCronJob)
		if err != nil && errors.IsNotFound(err) {
			// Define a new cronjob
			cronJob := &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterScan.Name,
					Namespace: clusterScan.Namespace,
				},
				Spec: batchv1.CronJobSpec{
					Schedule:    clusterScan.Spec.Schedule,
					JobTemplate: jobTemplate,
				},
			}
			controllerutil.SetControllerReference(clusterScan, cronJob, r.Scheme)
			err = r.Create(ctx, cronJob)
			if err != nil {
				log.Error(err, "Failed to create new CronJob", "CronJob.Namespace", cronJob.Namespace, "CronJob.Name", cronJob.Name)
				return ctrl.Result{}, err
			}
			// CronJob created successfully - don't requeue
			return ctrl.Result{}, nil
		} else if err != nil {
			log.Error(err, "Failed to get CronJob")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// cleanupClusterScan performs cleanup tasks for ClusterScan resources
func (r *ClusterScanReconciler) cleanupClusterScan(ctx context.Context, clusterScan *scanv1.ClusterScan) error {
	log := log.FromContext(ctx)

	// Delete associated Jobs
	if err := r.deleteAssociatedJobs(ctx, clusterScan); err != nil {
		log.Error(err, "Failed to delete associated Jobs")
		return err
	}

	// Delete associated CronJobs
	if err := r.deleteAssociatedCronJobs(ctx, clusterScan); err != nil {
		log.Error(err, "Failed to delete associated CronJobs")
		return err
	}

	return nil
}

// deleteAssociatedJobs deletes Jobs associated with the ClusterScan
func (r *ClusterScanReconciler) deleteAssociatedJobs(ctx context.Context, clusterScan *scanv1.ClusterScan) error {
	jobs := &batchv1.JobList{}
	opts := []client.ListOption{
		client.InNamespace(clusterScan.Namespace),
		client.MatchingLabels{"app": clusterScan.Name},
	}
	if err := r.List(ctx, jobs, opts...); err != nil {
		return err
	}

	for _, job := range jobs.Items {
		if err := r.Delete(ctx, &job); err != nil {
			return err
		}
	}
	return nil
}

// deleteAssociatedCronJobs deletes CronJobs associated with the ClusterScan
func (r *ClusterScanReconciler) deleteAssociatedCronJobs(ctx context.Context, clusterScan *scanv1.ClusterScan) error {
	cronJobs := &batchv1.CronJobList{}
	opts := []client.ListOption{
		client.InNamespace(clusterScan.Namespace),
		client.MatchingLabels{"app": clusterScan.Name},
	}
	if err := r.List(ctx, cronJobs, opts...); err != nil {
		return err
	}

	for _, cronJob := range cronJobs.Items {
		if err := r.Delete(ctx, &cronJob); err != nil {
			return err
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterScanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&scanv1.ClusterScan{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Complete(r)
}
