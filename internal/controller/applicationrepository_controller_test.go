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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/entr0pian/application-repository-operator/api/v1alpha1"
	"github.com/entr0pian/application-repository-operator/internal/githubapi"
)

// fakeGitHubClient is an in-memory githubapi.Client so controller tests
// exercise the real gitsync/yamlpatch write path without hitting GitHub.
type fakeGitHubClient struct {
	files map[string][]byte
	rev   map[string]int
}

var _ githubapi.Client = (*fakeGitHubClient)(nil)

func newFakeGitHubClient() *fakeGitHubClient {
	return &fakeGitHubClient{files: map[string][]byte{}, rev: map[string]int{}}
}

func (f *fakeGitHubClient) GetFile(_ context.Context, path string) ([]byte, string, error) {
	content, ok := f.files[path]
	if !ok {
		return nil, "", githubapi.ErrNotFound
	}
	return content, fmt.Sprintf("sha-%d", f.rev[path]), nil
}

func (f *fakeGitHubClient) UpdateFile(_ context.Context, path string, content []byte, sha, _ string) (string, error) {
	if _, exists := f.files[path]; exists && sha != fmt.Sprintf("sha-%d", f.rev[path]) {
		return "", githubapi.ErrConflict
	}
	f.rev[path]++
	f.files[path] = content
	return fmt.Sprintf("commit-%d", f.rev[path]), nil
}

var _ = Describe("ApplicationRepository Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		applicationrepository := &platformv1alpha1.ApplicationRepository{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ApplicationRepository")
			err := k8sClient.Get(ctx, typeNamespacedName, applicationrepository)
			if err != nil && errors.IsNotFound(err) {
				resource := &platformv1alpha1.ApplicationRepository{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: platformv1alpha1.ApplicationRepositorySpec{
						RepoURL: "https://github.com/entr0pian/example.git",
						Clusters: []platformv1alpha1.ClusterTarget{
							{Name: "dev"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &platformv1alpha1.ApplicationRepository{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ApplicationRepository")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ApplicationRepositoryReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				GitHub: newFakeGitHubClient(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})

	Context("imageTag handling", func() {
		ctx := context.Background()

		// cleanup deletes resource and drives the reconciler through
		// handleDeletion so the finalizer is actually removed and the object
		// is gone before the next spec reuses a name, instead of leaving a
		// terminating zombie that would fail a later Create.
		cleanup := func(reconciler *ApplicationRepositoryReconciler, name types.NamespacedName) {
			resource := &platformv1alpha1.ApplicationRepository{}
			Expect(k8sClient.Get(ctx, name, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
		}

		It("never writes an imageTag key when the field is left unset", func() {
			name := types.NamespacedName{Name: "imagetag-unset", Namespace: "default"}
			resource := &platformv1alpha1.ApplicationRepository{
				ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
				Spec: platformv1alpha1.ApplicationRepositorySpec{
					RepoURL:  "https://github.com/entr0pian/example.git",
					Clusters: []platformv1alpha1.ClusterTarget{{Name: "dev"}},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			gh := newFakeGitHubClient()
			reconciler := &ApplicationRepositoryReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), GitHub: gh}
			defer cleanup(reconciler, name)

			// First reconcile only adds the finalizer and returns early; the
			// second one is the reconcile that actually patches taskapp-argocd.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())

			Expect(gh.files["apps/values-dev.yaml"]).NotTo(BeNil())
			Expect(string(gh.files["apps/values-dev.yaml"])).NotTo(ContainSubstring("imageTag"))
		})

		It("writes and then removes imageTag as the field is set and then cleared", func() {
			name := types.NamespacedName{Name: "imagetag-set-then-cleared", Namespace: "default"}
			resource := &platformv1alpha1.ApplicationRepository{
				ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
				Spec: platformv1alpha1.ApplicationRepositorySpec{
					RepoURL:  "https://github.com/entr0pian/example.git",
					Clusters: []platformv1alpha1.ClusterTarget{{Name: "dev", ImageTag: "abc123"}},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			gh := newFakeGitHubClient()
			reconciler := &ApplicationRepositoryReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), GitHub: gh}
			defer cleanup(reconciler, name)

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(gh.files["apps/values-dev.yaml"])).To(ContainSubstring("imageTag: abc123"))

			Expect(k8sClient.Get(ctx, name, resource)).To(Succeed())
			resource.Spec.Clusters = []platformv1alpha1.ClusterTarget{{Name: "dev"}}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(gh.files["apps/values-dev.yaml"])).NotTo(ContainSubstring("imageTag"))
		})
	})
})
