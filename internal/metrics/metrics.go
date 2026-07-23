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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metric label names.
const (
	labelOperation     = "operation"
	labelDecision      = "decision"
	labelKind          = "kind"
	labelPreviousOwner = "previous_owner"
	labelNewOwner      = "new_owner"
)

// Metrics holds the Prometheus collectors exposed by the webhook.
type Metrics struct {
	requestsTotal           *prometheus.CounterVec
	denialsTotal            *prometheus.CounterVec
	ownershipConflictsTotal *prometheus.CounterVec
	latencySeconds          *prometheus.HistogramVec
	discoveryErrors         prometheus.Counter
	configUpdatesTotal      *prometheus.CounterVec
}

// NewMetrics creates the webhook metrics and registers them with the
// controller-runtime Prometheus registry.
func NewMetrics() *Metrics {
	return NewMetricsWithRegistry(metrics.Registry)
}

// NewMetricsWithRegistry allows tests to use an isolated registry.
func NewMetricsWithRegistry(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "flux_drift_webhook_requests_total",
				Help: "Total number of admission requests processed",
			},
			[]string{labelOperation, labelDecision},
		),
		denialsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "flux_drift_webhook_denials_total",
				Help: "Total number of denied admission requests",
			},
			// Bounded labels only — namespace was removed to prevent
			// unbounded cardinality that could exhaust webhook memory.
			[]string{labelOperation, labelKind},
		),
		ownershipConflictsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "flux_drift_webhook_ownership_conflicts_total",
				Help: "Total Flux ownership conflicts: a resource's Flux owner " +
					"labels flipped between two reconcilers (dual/multiple ownership)",
			},
			// Labels are the conflicting Flux owners ("<namespace>/<name>") and the
			// resource kind — all bounded by the number of Flux objects, not by
			// resource count. The series stay at zero (and absent) in a healthy
			// cluster, so cardinality is self-limiting to the actual conflicts.
			[]string{labelKind, labelPreviousOwner, labelNewOwner},
		),
		latencySeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "flux_drift_webhook_latency_seconds",
				Help:    "Latency of admission request processing",
				Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
			},
			[]string{labelOperation},
		),
		discoveryErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "flux_drift_webhook_discovery_errors_total",
				Help: "Total number of GVK discovery errors",
			},
		),
		configUpdatesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "flux_drift_webhook_config_updates_total",
				Help: "Total number of ValidatingWebhookConfiguration updates",
			},
			[]string{"status"},
		),
	}

	reg.MustRegister(
		m.requestsTotal,
		m.denialsTotal,
		m.ownershipConflictsTotal,
		m.latencySeconds,
		m.discoveryErrors,
		m.configUpdatesTotal,
	)

	return m
}

// RecordRequest increments the processed-requests counter for the given
// operation and admission decision.
func (m *Metrics) RecordRequest(operation, decision string) {
	m.requestsTotal.WithLabelValues(operation, decision).Inc()
}

// RecordDenial increments the denied-requests counter for the given operation
// and resource kind.
func (m *Metrics) RecordDenial(operation, kind string) {
	m.denialsTotal.WithLabelValues(operation, kind).Inc()
}

// RecordOwnershipConflict increments the ownership-conflict counter for a
// resource kind whose Flux owner labels flipped from previousOwner to newOwner
// (both "<namespace>/<name>"). This surfaces dual/multiple ownership: two Flux
// reconcilers fighting over the same resource.
func (m *Metrics) RecordOwnershipConflict(kind, previousOwner, newOwner string) {
	m.ownershipConflictsTotal.WithLabelValues(kind, previousOwner, newOwner).Inc()
}

// RecordDiscoveryError increments the GVK discovery-error counter.
func (m *Metrics) RecordDiscoveryError() {
	m.discoveryErrors.Inc()
}

// RecordConfigUpdate increments the VWC update counter for the given status
// ("success" or "error").
func (m *Metrics) RecordConfigUpdate(status string) {
	m.configUpdatesTotal.WithLabelValues(status).Inc()
}

// Timer measures the latency of a single admission request.
type Timer struct {
	start     time.Time
	histogram *prometheus.HistogramVec
	operation string
}

// StartTimer returns a running Timer for the given operation.
func (m *Metrics) StartTimer(operation string) *Timer {
	return &Timer{
		start:     time.Now(),
		histogram: m.latencySeconds,
		operation: operation,
	}
}

// ObserveDuration records the elapsed time since the Timer was started.
func (t *Timer) ObserveDuration() {
	t.histogram.WithLabelValues(t.operation).Observe(time.Since(t.start).Seconds())
}
