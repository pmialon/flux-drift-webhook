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
	"testing"
	"time"
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
