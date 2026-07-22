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
	"fmt"
	"os"
	"testing"

	"github.com/fluxcd/pkg/runtime/testenv"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// testEnv is the shared envtest-backed environment (real apiserver + etcd).
	testEnv *testenv.Environment
	// k8sClient is a cacheless client used by every integration test, so reads
	// observe writes immediately without cache-sync delay.
	k8sClient client.Client
)

// TestMain boots a single envtest environment for the package's integration
// tests. testenv.New starts the apiserver synchronously (and installs its own
// logger). The manager goroutine is started (so Stop can cancel it cleanly),
// but the tests drive Reconcile() directly with the cacheless k8sClient rather
// than the controller loop — keeping the suite deterministic (no cache sync,
// no leader election, no watch timing).
func TestMain(m *testing.M) {
	testEnv = testenv.New(testenv.WithScheme(clientgoscheme.Scheme))

	var err error
	k8sClient, err = client.New(testEnv.Config, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		panic(fmt.Sprintf("failed to create cacheless k8s client: %v", err))
	}

	go func() {
		if err := testEnv.Start(context.Background()); err != nil {
			panic(fmt.Sprintf("failed to start the test environment: %v", err))
		}
	}()
	<-testEnv.Elected()

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		panic(fmt.Sprintf("failed to stop the test environment: %v", err))
	}
	os.Exit(code)
}
