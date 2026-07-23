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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewMetrics(t *testing.T) {
	m := NewMetrics()

	if m.requestsTotal == nil {
		t.Error("requestsTotal should not be nil")
	}
	if m.denialsTotal == nil {
		t.Error("denialsTotal should not be nil")
	}
	if m.ownershipConflictsTotal == nil {
		t.Error("ownershipConflictsTotal should not be nil")
	}
	if m.latencySeconds == nil {
		t.Error("latencySeconds should not be nil")
	}
}

func TestRecordRequest(t *testing.T) {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_requests_total",
			Help: "Test counter",
		},
		[]string{"operation", "decision"},
	)
	reg.MustRegister(counter)

	m := &Metrics{requestsTotal: counter}

	m.RecordRequest("UPDATE", "allowed_flux_controller")
	m.RecordRequest("UPDATE", "allowed_flux_controller")
	m.RecordRequest("DELETE", "denied_delete_flux_managed")

	expected := float64(2)
	actual := testutil.ToFloat64(counter.WithLabelValues("UPDATE", "allowed_flux_controller"))
	if actual != expected {
		t.Errorf("expected %v, got %v", expected, actual)
	}
}

func TestRecordOwnershipConflict(t *testing.T) {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_ownership_conflicts_total",
			Help: "Test counter",
		},
		[]string{"kind", "previous_owner", "new_owner"},
	)
	reg.MustRegister(counter)

	m := &Metrics{ownershipConflictsTotal: counter}

	m.RecordOwnershipConflict("RoleBinding", "flux-system/qdcore-apps", "flux-system/argo-rbac")
	m.RecordOwnershipConflict("RoleBinding", "flux-system/qdcore-apps", "flux-system/argo-rbac")
	m.RecordOwnershipConflict("Role", "flux-system/qdcore-apps", "flux-system/argo-rbac")

	if got := testutil.ToFloat64(
		counter.WithLabelValues("RoleBinding", "flux-system/qdcore-apps", "flux-system/argo-rbac"),
	); got != 2 {
		t.Errorf("expected 2 RoleBinding conflicts, got %v", got)
	}
}

func TestTimerObserveDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	histogram := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "test_latency_seconds",
			Help:    "Test histogram",
			Buckets: []float64{0.001, 0.01, 0.1, 1},
		},
		[]string{"operation"},
	)
	reg.MustRegister(histogram)

	m := &Metrics{latencySeconds: histogram}

	timer := m.StartTimer("UPDATE")
	timer.ObserveDuration()

	count := testutil.CollectAndCount(histogram)
	if count == 0 {
		t.Error("expected histogram to have observations")
	}
}
