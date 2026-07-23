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

package main

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/pmialon/flux-drift-webhook/internal/config"
)

func TestGetEnv(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		envValue     string
		defaultValue string
		want         string
	}{
		{"returns default when unset", "TEST_GETENV_UNSET", "", "default", "default"},
		{"returns env value when set", "TEST_GETENV_SET", "custom", "default", "custom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.envValue)
			if got := getEnv(tt.key, tt.defaultValue); got != tt.want {
				t.Errorf("getEnv(%q, %q) = %q, want %q", tt.key, tt.defaultValue, got, tt.want)
			}
		})
	}
}

func TestGetEnvDuration(t *testing.T) {
	tests := []struct {
		name         string
		envValue     string
		defaultValue time.Duration
		want         time.Duration
		wantErr      bool
	}{
		{"returns default when unset", "", 5 * time.Minute, 5 * time.Minute, false},
		{"parses valid duration", "10s", 5 * time.Minute, 10 * time.Second, false},
		{"errors on invalid duration", "not-a-duration", 5 * time.Minute, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := "TEST_DURATION_VAR"
			t.Setenv(key, tt.envValue)
			got, err := getEnvDuration(key, tt.defaultValue)
			if (err != nil) != tt.wantErr {
				t.Fatalf("getEnvDuration() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("getEnvDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeSystemControllerSAs(t *testing.T) {
	defaults := len(mergeSystemControllerSAs(""))
	if defaults == 0 {
		t.Fatal("expected built-in default system-controller service accounts")
	}

	tests := []struct {
		name string
		csv  string
		want int
	}{
		{"empty keeps defaults only", "", defaults},
		{"adds one extra entry", "tenant-ns:custom-controller", defaults + 1},
		{"deduplicates against defaults", "kube-system:endpoint-controller", defaults},
		{"trims and ignores blanks", " , tenant-ns:a , ", defaults + 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(mergeSystemControllerSAs(tt.csv)); got != tt.want {
				t.Errorf("mergeSystemControllerSAs(%q) returned %d entries, want %d", tt.csv, got, tt.want)
			}
		})
	}
}

func TestCachesSyncedCheck(t *testing.T) {
	c := &cachesSynced{ch: make(chan struct{})}

	if err := c.Check(nil); err == nil {
		t.Fatal("Check() returned nil before the caches synced, want an error so readyz stays red")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	// Start closes the channel immediately; poll rather than sleep so the test
	// does not depend on goroutine scheduling.
	deadline := time.After(5 * time.Second)
	for {
		if err := c.Check(nil); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Check() never returned nil after Start()")
		default:
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start() returned %v, want nil on context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after its context was cancelled")
	}
}

// TestCachesSyncedNeedLeaderElection guards a trap: controller-runtime's
// runnables.Add sends a Runnable that does not implement LeaderElectionRunnable
// to the leader-election group. The deploy overlays enable leader election, so
// were this to report true, every non-leader replica would block on the readyz
// check forever and the Deployment would never roll out.
func TestCachesSyncedNeedLeaderElection(t *testing.T) {
	c := &cachesSynced{ch: make(chan struct{})}
	if c.NeedLeaderElection() {
		t.Error("NeedLeaderElection() = true, want false so the runnable starts on every replica")
	}
	var _ manager.LeaderElectionRunnable = c
	var _ manager.Runnable = c
}

// TestResyncIntervalDefault pins the environment precedence for the resync
// interval. DISCOVERY_INTERVAL is the pre-1.0 name kept as an alias, so a
// deployment that set it must not silently fall back to the built-in default
// on upgrade.
func TestResyncIntervalDefault(t *testing.T) {
	tests := []struct {
		name    string
		vwcEnv  string
		legacy  string
		want    time.Duration
		wantErr bool
	}{
		{name: "neither set uses the built-in default", want: config.DefaultVWCResyncInterval},
		{name: "legacy alone is honoured", legacy: "7m", want: 7 * time.Minute},
		{name: "new name alone", vwcEnv: "4m", want: 4 * time.Minute},
		{name: "new name wins over the alias", vwcEnv: "4m", legacy: "7m", want: 4 * time.Minute},
		{name: "invalid new name errors", vwcEnv: "nope", wantErr: true},
		{name: "invalid alias errors", legacy: "nope", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("VWC_RESYNC_INTERVAL", tt.vwcEnv)
			t.Setenv("DISCOVERY_INTERVAL", tt.legacy)

			got, err := resyncIntervalDefault()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resyncIntervalDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}
