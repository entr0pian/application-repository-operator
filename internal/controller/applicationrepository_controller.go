/*
Copyright 2026.

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
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/entr0pian/application-repository-operator/api/v1alpha1"
	"github.com/entr0pian/application-repository-operator/internal/githubapi"
	"github.com/entr0pian/application-repository-operator/internal/gitsync"
	"github.com/entr0pian/application-repository-operator/internal/yamlpatch"
)

const (
	applicationRepositoryFinalizer = "platform.taskapp.io/finalizer"

	// Paths are relative to the root of the taskapp-argocd repo that GitHub
	// is scoped to (see githubapi.Client).
	argoCDValuesPath       = "apps/values.yaml"
	argoCDValuesEnvPathFmt = "apps/values-%s.yaml"

	// requeueInterval re-checks taskapp-argocd on a fixed cadence, since git
	// state can drift (hand edits, partially-failed reconciles) with no
	// Kubernetes watch event to notify us.
	requeueInterval = 5 * time.Minute
)

// ApplicationRepositoryReconciler reconciles a ApplicationRepository object
type ApplicationRepositoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// GitHub is the write path into the taskapp-argocd repo. Scoped to a
	// single owner/repo/branch at construction (see githubapi.RESTClient).
	GitHub githubapi.Client
}

// +kubebuilder:rbac:groups=platform.taskapp.io,resources=applicationrepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.taskapp.io,resources=applicationrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.taskapp.io,resources=applicationrepositories/finalizers,verbs=update

// Reconcile drives one ApplicationRepository towards the desired state by
// patching structured keys into taskapp-argocd's Helm values files — it
// never generates or commits whole files. See gitsync.PatchYAML for the
// underlying fetch-mutate-write-retry mechanics.
func (r *ApplicationRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	appRepo := &platformv1alpha1.ApplicationRepository{}
	if err := r.Get(ctx, req.NamespacedName, appRepo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !appRepo.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.handleDeletion(ctx, appRepo)
	}

	if !controllerutil.ContainsFinalizer(appRepo, applicationRepositoryFinalizer) {
		controllerutil.AddFinalizer(appRepo, applicationRepositoryFinalizer)
		if err := r.Update(ctx, appRepo); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	statusBase := appRepo.DeepCopy()

	specErr := r.reconcileSpec(ctx, appRepo)
	r.setSpecSyncedCondition(appRepo, specErr)

	clustersErr := r.reconcileClusters(ctx, appRepo)
	r.setReadyCondition(appRepo)
	appRepo.Status.ObservedGeneration = appRepo.Generation

	if err := r.patchStatusIfChanged(ctx, statusBase, appRepo); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	if err := errors.Join(specErr, clustersErr); err != nil {
		log.Error(err, "reconcile failed, controller-runtime will retry with backoff")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// reconcileSpec patches this repository's shared, per-repo fields (the same
// for every cluster) into apps/values.yaml.
func (r *ApplicationRepositoryReconciler) reconcileSpec(ctx context.Context, appRepo *platformv1alpha1.ApplicationRepository) error {
	name := appRepo.Name
	spec := appRepo.Spec

	_, err := gitsync.PatchYAML(ctx, r.GitHub, argoCDValuesPath,
		fmt.Sprintf("chore(%s): sync application-repository spec", name),
		func(doc *yamlpatch.Document) bool {
			changedRepoURL := doc.SetString(spec.RepoURL, "repositories", name, "repoURL")
			changedRevision := doc.SetString(spec.TargetRevision, "repositories", name, "targetRevision")
			changedChartPath := doc.SetString(spec.ChartPath, "repositories", name, "chartPath")
			return changedRepoURL || changedRevision || changedChartPath
		},
	)
	if err != nil {
		return fmt.Errorf("sync %s: %w", argoCDValuesPath, err)
	}
	return nil
}

// reconcileClusters patches per-cluster fields into each target's
// apps/values-<env>.yaml. It walks the union of spec.Clusters (desired) and
// status.Clusters (previously known) so that a cluster removed from spec is
// disabled (enabled:false) in its values file rather than silently left
// stale — the same non-destructive behavior handleDeletion reuses by
// reconciling against an empty spec.Clusters.
func (r *ApplicationRepositoryReconciler) reconcileClusters(ctx context.Context, appRepo *platformv1alpha1.ApplicationRepository) error {
	name := appRepo.Name

	desired := make(map[string]platformv1alpha1.ClusterTarget, len(appRepo.Spec.Clusters))
	for _, c := range appRepo.Spec.Clusters {
		desired[c.Name] = c
	}

	previousStatus := appRepo.Status.Clusters

	var order []string
	seen := make(map[string]bool)
	for _, c := range appRepo.Spec.Clusters {
		if !seen[c.Name] {
			seen[c.Name] = true
			order = append(order, c.Name)
		}
	}
	for _, cs := range previousStatus {
		if !seen[cs.Name] {
			seen[cs.Name] = true
			order = append(order, cs.Name)
		}
	}

	var errs []error
	newStatus := make([]platformv1alpha1.ClusterDeploymentStatus, 0, len(order))
	for _, clusterName := range order {
		target, enabled := desired[clusterName]
		cs := platformv1alpha1.ClusterDeploymentStatus{
			Name:        clusterName,
			LastAttempt: metav1.Now(),
		}

		path := fmt.Sprintf(argoCDValuesEnvPathFmt, clusterName)
		result, err := gitsync.PatchYAML(ctx, r.GitHub, path,
			fmt.Sprintf("chore(%s): sync application-repository cluster entry (%s)", name, clusterName),
			func(doc *yamlpatch.Document) bool {
				changedEnabled := doc.SetBool(enabled, "repositories", name, "enabled")
				changedNamespace := false
				if enabled {
					changedNamespace = doc.SetString(target.Namespace, "repositories", name, "namespace")
				}
				return changedEnabled || changedNamespace
			},
		)
		if err != nil {
			cs.Committed = false
			cs.Error = err.Error()
			cs.CommitSHA = previousCommitSHA(previousStatus, clusterName)
			errs = append(errs, fmt.Errorf("sync %s: %w", path, err))
		} else {
			cs.Committed = true
			if result.Changed {
				cs.CommitSHA = result.CommitSHA
			} else {
				cs.CommitSHA = previousCommitSHA(previousStatus, clusterName)
			}
		}
		newStatus = append(newStatus, cs)
	}

	appRepo.Status.Clusters = newStatus
	return errors.Join(errs...)
}

// handleDeletion disables this repository (enabled:false) in every cluster
// it was ever committed to, then releases the finalizer. It deliberately
// reuses reconcileClusters against a copy with an empty spec.Clusters,
// rather than duplicating the patch logic, so deletion and cluster-removal
// go through the exact same non-destructive disable path.
func (r *ApplicationRepositoryReconciler) handleDeletion(ctx context.Context, appRepo *platformv1alpha1.ApplicationRepository) error {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(appRepo, applicationRepositoryFinalizer) {
		return nil
	}

	disabling := appRepo.DeepCopy()
	disabling.Spec.Clusters = nil
	if err := r.reconcileClusters(ctx, disabling); err != nil {
		log.Error(err, "failed to disable application-repository in target clusters during deletion")
		return err
	}

	controllerutil.RemoveFinalizer(appRepo, applicationRepositoryFinalizer)
	return r.Update(ctx, appRepo)
}

func (r *ApplicationRepositoryReconciler) setSpecSyncedCondition(appRepo *platformv1alpha1.ApplicationRepository, specErr error) {
	status := metav1.ConditionTrue
	reason := "Synced"
	message := fmt.Sprintf("%s matches spec", argoCDValuesPath)
	if specErr != nil {
		status = metav1.ConditionFalse
		reason = "SyncFailed"
		message = specErr.Error()
	}
	apimeta.SetStatusCondition(&appRepo.Status.Conditions, metav1.Condition{
		Type:               "SpecSynced",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: appRepo.Generation,
	})
}

func (r *ApplicationRepositoryReconciler) setReadyCondition(appRepo *platformv1alpha1.ApplicationRepository) {
	status := metav1.ConditionFalse
	reason := "SpecNotSynced"
	message := fmt.Sprintf("waiting for %s to sync", argoCDValuesPath)

	if apimeta.IsStatusConditionTrue(appRepo.Status.Conditions, "SpecSynced") {
		status = metav1.ConditionTrue
		reason = "Ready"
		message = "all target clusters committed"

		for _, target := range appRepo.Spec.Clusters {
			cs := findClusterStatus(appRepo.Status.Clusters, target.Name)
			if cs == nil || !cs.Committed {
				status = metav1.ConditionFalse
				reason = "ClusterSyncPending"
				message = fmt.Sprintf("cluster %s not yet committed", target.Name)
				break
			}
		}
	}

	apimeta.SetStatusCondition(&appRepo.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: appRepo.Generation,
	})
}

func (r *ApplicationRepositoryReconciler) patchStatusIfChanged(ctx context.Context, statusBase, appRepo *platformv1alpha1.ApplicationRepository) error {
	if equality.Semantic.DeepEqual(statusBase.Status, appRepo.Status) {
		return nil
	}
	patch := client.MergeFrom(statusBase)
	return client.IgnoreNotFound(r.Status().Patch(ctx, appRepo, patch))
}

func findClusterStatus(list []platformv1alpha1.ClusterDeploymentStatus, name string) *platformv1alpha1.ClusterDeploymentStatus {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func previousCommitSHA(list []platformv1alpha1.ClusterDeploymentStatus, name string) string {
	if cs := findClusterStatus(list, name); cs != nil {
		return cs.CommitSHA
	}
	return ""
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.ApplicationRepository{}).
		Named("applicationrepository").
		Complete(r)
}
