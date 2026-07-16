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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterTarget describes one cluster/environment this repository is delivered to.
type ClusterTarget struct {
	// name is the target environment (e.g. "dev", "prod"). It selects which
	// taskapp-argocd apps/values-<name>.yaml file the operator patches, and
	// therefore also which destinationServer the generated Argo CD Application
	// uses (each values-<env>.yaml already carries the correct destinationServer
	// for every app rendered in that environment).
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// namespace is the destination namespace on the target cluster.
	// +kubebuilder:default=default
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ApplicationRepositorySpec defines the desired state of ApplicationRepository
type ApplicationRepositorySpec struct {
	// repoURL is the git URL of the application's own repo, containing its Helm chart.
	// +kubebuilder:validation:Required
	RepoURL string `json:"repoURL"`

	// targetRevision is the git branch or tag to track.
	// +kubebuilder:default=main
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`

	// chartPath is the path within repoURL where the Helm chart lives.
	// +kubebuilder:default=chart
	// +optional
	ChartPath string `json:"chartPath,omitempty"`

	// clusters lists every target cluster/environment this repository should
	// be delivered to. Sync is always automated (prune + selfHeal) for now.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Required
	Clusters []ClusterTarget `json:"clusters"`
}

// ClusterDeploymentStatus reports the git-commit state for one target cluster.
type ClusterDeploymentStatus struct {
	// name is the cluster/environment name from spec.clusters.
	Name string `json:"name"`

	// committed is true once this cluster's values-<env>.yaml entry matches spec.
	Committed bool `json:"committed"`

	// commitSHA is the taskapp-argocd commit that last updated this entry.
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`

	// lastAttempt is when the operator last tried to reconcile this cluster's entry.
	// +optional
	LastAttempt metav1.Time `json:"lastAttempt,omitzero"`

	// error is the last failure reason, set when committed is false
	// (e.g. retries exhausted after repeated conflicts committing to taskapp-argocd).
	// +optional
	Error string `json:"error,omitempty"`
}

// ApplicationRepositoryStatus defines the observed state of ApplicationRepository.
type ApplicationRepositoryStatus struct {
	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clusters reports, per spec.clusters[] entry, whether the corresponding
	// values-<env>.yaml entry in taskapp-argocd has been committed to match spec.
	// +optional
	Clusters []ClusterDeploymentStatus `json:"clusters,omitempty"`

	// conditions represent the current state of the ApplicationRepository resource.
	//
	// Condition types used:
	// - "SpecSynced": true once repositories.<name>.{repoURL,targetRevision,chartPath}
	//   in taskapp-argocd's apps/values.yaml matches spec.
	// - "Ready": true only when SpecSynced is true and every spec.clusters[] entry
	//   shows committed:true in status.clusters[].
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="RepoURL",type=string,JSONPath=`.spec.repoURL`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ApplicationRepository is the Schema for the applicationrepositories API
type ApplicationRepository struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ApplicationRepository
	// +required
	Spec ApplicationRepositorySpec `json:"spec"`

	// status defines the observed state of ApplicationRepository
	// +optional
	Status ApplicationRepositoryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ApplicationRepositoryList contains a list of ApplicationRepository
type ApplicationRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ApplicationRepository `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ApplicationRepository{}, &ApplicationRepositoryList{})
}
