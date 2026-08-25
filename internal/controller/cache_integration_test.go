//go:build integration

/*
Copyright 2026 Qube Research & Technologies

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
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	webhookhandler "github.com/pmialon/flux-drift-webhook/internal/webhook"
)

// TestIntegration_CacheOptions_NamespaceMetadataOnly proves the production
// cache wiring against a real apiserver: a cache built from
// webhookhandler.CacheOptions serves namespace reads as PartialObjectMetadata
// (the metadata-only informer path the handler uses) with managedFields
// stripped by the default transform. The unit tests cover the transform
// functions; only a real apiserver exercises the informer list+watch pipeline
// they hang off.
func TestIntegration_CacheOptions_NamespaceMetadataOnly(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The namespace exists before the cache starts, so the initial list must
	// deliver it through the transform pipeline.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "cache-it-ns",
		Labels: map[string]string{"env": "cache-it"},
	}}
	g.Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	// Namespace deletion wedges in envtest (no namespace controller runs), so
	// no cleanup: the envtest environment is torn down with the process.

	// The live object carries managedFields (written by this test's client).
	var live corev1.Namespace
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cache-it-ns"}, &live)).To(Succeed())
	g.Expect(live.ManagedFields).NotTo(BeEmpty(), "precondition: the apiserver records managedFields")

	opts := webhookhandler.CacheOptions()
	opts.Scheme = k8sClient.Scheme()
	c, err := cache.New(testEnv.Config, opts)
	g.Expect(err).NotTo(HaveOccurred())

	go func() { _ = c.Start(ctx) }()
	g.Expect(c.WaitForCacheSync(ctx)).To(BeTrue())

	got := webhookhandler.NamespaceMetadata()
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: "cache-it-ns"}, got)
	}, 10*time.Second).Should(Succeed())

	// The read the handler performs: labels served, managedFields stripped.
	g.Expect(got.Labels).To(HaveKeyWithValue("env", "cache-it"))
	g.Expect(got.ManagedFields).To(BeEmpty(),
		"cached namespace metadata kept managedFields — the default transform is not wired")
}
