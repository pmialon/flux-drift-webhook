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

// Command flux-drift-webhook runs the validating admission webhook that
// prevents manual drift on FluxCD-managed resources. It bootstraps a
// controller-runtime manager from the fluxcd/pkg/runtime option structs.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	flag "github.com/spf13/pflag"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/leaderelection"
	"github.com/fluxcd/pkg/runtime/logger"
	"github.com/fluxcd/pkg/runtime/pprof"
	"github.com/fluxcd/pkg/runtime/probes"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	"github.com/pmialon/flux-drift-webhook/internal/controller"
	"github.com/pmialon/flux-drift-webhook/internal/metrics"
	webhookhandler "github.com/pmialon/flux-drift-webhook/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = admissionregistrationv1.AddToScheme(scheme)
}

func main() {
	var (
		webhookPort           int
		metricsAddr           string
		healthAddr            string
		certDir               string
		auditOnly             bool
		fluxNamespace         string
		webhookName           string
		vwcResyncInterval     time.Duration
		discoveryInterval     time.Duration
		namespaceLabel        string
		namespaceLabelValue   string
		namespaceFetchTimeout time.Duration
		systemControllerSAs   string
		kubeAPIQPS            float32
		kubeAPIBurst          int

		leaderElectionOptions leaderelection.Options
		loggerOptions         logger.Options
	)

	// Fail fast on malformed environment values: a typo silently falling back
	// to a default is worse than a crash at startup.
	resyncDefault, err := resyncIntervalDefault()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	flag.IntVar(&webhookPort, "webhook-port", config.DefaultWebhookPort, "Webhook server port")
	flag.StringVar(&metricsAddr, "metrics-bind-addr", ":8080", "Metrics endpoint address")
	flag.StringVar(&healthAddr, "health-probe-bind-addr", ":8081", "Health probe address")
	flag.StringVar(&certDir, "cert-dir", config.DefaultCertDir, "TLS certificates directory")
	flag.BoolVar(&auditOnly, "audit-only", false, "Audit-only mode (log without blocking)")
	flag.StringVar(&fluxNamespace, "flux-namespace", getEnv("FLUX_NAMESPACE", config.FluxNamespaceDefault), "Flux namespace")
	flag.StringVar(&webhookName, "webhook-name", getEnv("WEBHOOK_NAME", config.WebhookName), "ValidatingWebhookConfiguration name")
	flag.DurationVar(&vwcResyncInterval, "vwc-resync-interval", resyncDefault,
		"Interval between ValidatingWebhookConfiguration re-applies")
	flag.DurationVar(&discoveryInterval, "discovery-interval", resyncDefault,
		"Deprecated alias for --vwc-resync-interval")
	flag.StringVar(&namespaceLabel, "namespace-label", "", "Optional: namespace label key to filter webhook scope")
	flag.StringVar(&namespaceLabelValue, "namespace-label-value", "", "Optional: namespace label value to match (requires namespace-label)")
	flag.DurationVar(&namespaceFetchTimeout, "namespace-fetch-timeout", webhookhandler.DefaultNamespaceFetchTimeout, "Timeout for namespace label lookups")
	flag.StringVar(&systemControllerSAs, "system-controller-sas", getEnv("SYSTEM_CONTROLLER_SAS", ""),
		"Extra control-plane identities (CSV of namespace:name SA shorthands or full system: usernames) "+
			"allowed to create Flux-labelled derived resources and delete Flux-applied resources; "+
			"merged with built-in defaults")
	flag.Float32Var(&kubeAPIQPS, "kube-api-qps", 50.0, "The maximum queries-per-second of requests sent to the Kubernetes API.")
	flag.IntVar(&kubeAPIBurst, "kube-api-burst", 300, "The maximum burst queries-per-second of requests sent to the Kubernetes API.")

	leaderElectionOptions.BindFlags(flag.CommandLine)
	loggerOptions.BindFlags(flag.CommandLine)

	flag.Parse()

	log := logger.NewLogger(loggerOptions)
	logger.SetLogger(log)

	// The webhook rules are static since GVK discovery was replaced by a
	// wildcard rule plus CEL matchConditions, so the interval now only paces
	// re-applies. The old flag keeps working to avoid breaking deployments.
	if flag.CommandLine.Changed("discovery-interval") {
		log.Info("--discovery-interval is deprecated, use --vwc-resync-interval")
		vwcResyncInterval = discoveryInterval
	}

	if namespaceLabelValue != "" && namespaceLabel == "" {
		log.Error(nil, "namespace-label-value requires namespace-label to be set")
		os.Exit(1)
	}

	log.Info("starting flux-drift-webhook",
		"auditOnly", auditOnly,
		"fluxNamespace", fluxNamespace,
		"vwcResyncInterval", vwcResyncInterval,
	)

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = kubeAPIQPS
	restConfig.Burst = kubeAPIBurst

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		// Serve unstructured reads (owning Kustomization/HelmRelease lookups)
		// from the cache instead of hitting the API server on every request.
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: true}},
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			ExtraHandlers: pprof.GetHandlers(),
		},
		HealthProbeBindAddress: healthAddr,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: certDir,
		}),
		LeaderElection:                leaderElectionOptions.Enable,
		LeaderElectionID:              "flux-drift-webhook-leader",
		LeaderElectionNamespace:       fluxNamespace,
		LeaderElectionReleaseOnCancel: leaderElectionOptions.ReleaseOnCancel,
		LeaseDuration:                 &leaderElectionOptions.LeaseDuration,
		RenewDeadline:                 &leaderElectionOptions.RenewDeadline,
		RetryPeriod:                   &leaderElectionOptions.RetryPeriod,
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	m := metrics.NewMetrics()

	handler := &webhookhandler.DriftPreventionHandler{
		Log:                   log.WithName("webhook"),
		FluxNamespace:         fluxNamespace,
		AuditOnly:             auditOnly,
		Metrics:               m,
		NamespaceLabel:        namespaceLabel,
		NamespaceLabelValue:   namespaceLabelValue,
		NamespaceFetchTimeout: namespaceFetchTimeout,
		Client:                mgr.GetClient(),
		SystemControllerSAs:   mergeSystemControllerSAs(systemControllerSAs),
	}

	mgr.GetWebhookServer().Register(config.WebhookPath, &webhook.Admission{Handler: handler})

	// Local Kubernetes Events only (no external notification-controller webhook),
	// hence an empty webhook address. events.Recorder wraps the manager's
	// recorder and satisfies record.EventRecorder.
	eventRecorder, err := events.NewRecorder(mgr, log.WithName("event-recorder"), "", "flux-drift-webhook")
	if err != nil {
		log.Error(err, "unable to create event recorder")
		os.Exit(1)
	}

	if err := (&controller.WebhookConfigReconciler{
		Client:           mgr.GetClient(),
		Metrics:          m,
		EventRecorder:    eventRecorder,
		WebhookName:      webhookName,
		WebhookNamespace: fluxNamespace,
		WebhookService:   "flux-drift-webhook",
		WebhookPath:      config.WebhookPath,
		ResyncInterval:   vwcResyncInterval,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to setup webhook config controller")
		os.Exit(1)
	}

	probes.SetupChecks(mgr, log)

	// Gate readiness on the webhook server actually serving: probes.SetupChecks
	// only registers ping checks, so without this the pod reports Ready before
	// the TLS listener is bound and the Service could route requests into a
	// connection refused (failurePolicy Ignore would silently allow them).
	if err := mgr.AddReadyzCheck("webhook-server", mgr.GetWebhookServer().StartedChecker()); err != nil {
		log.Error(err, "unable to register webhook server readiness check")
		os.Exit(1)
	}

	// Gate readiness on the informer caches too. controller-runtime deliberately
	// starts the webhook server *before* the caches (see the WARNING in
	// manager/internal.go), so the TLS listener is up — and StartedChecker green —
	// while every cache-backed lookup still fails. Those lookups are fail-closed,
	// so without this the pod takes admission traffic during its cold start and
	// denies legitimate requests: namespace-terminating cascades, CREATEs whose
	// owner inventory cannot be read, and tenant reconcilers whose service account
	// cannot be resolved.
	synced := &cachesSynced{ch: make(chan struct{})}
	if err := mgr.Add(synced); err != nil {
		log.Error(err, "unable to register cache sync runnable")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("cache-sync", synced.Check); err != nil {
		log.Error(err, "unable to register cache sync readiness check")
		os.Exit(1)
	}

	warmInformerCaches(mgr, log)

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}

	log.Info("shutdown complete")
}

// cachesSynced reports whether the manager's informer caches have completed
// their initial sync. controller-runtime starts the "Others" runnable group only
// after runnables.Caches.Start has waited for that sync, so Start being called is
// the signal.
//
// NeedLeaderElection must report false: a bare Runnable falls through to the
// leader-election group, which with --enable-leader-election (the deploy
// overlays set it) would leave every non-leader replica permanently unready.
type cachesSynced struct {
	ch chan struct{}
}

func (c *cachesSynced) Start(ctx context.Context) error {
	close(c.ch)
	<-ctx.Done()
	return nil
}

func (c *cachesSynced) NeedLeaderElection() bool { return false }

// Check is the readyz checker backed by the signal above.
func (c *cachesSynced) Check(_ *http.Request) error {
	select {
	case <-c.ch:
		return nil
	default:
		return errors.New("informer caches have not completed their initial sync")
	}
}

// warmInformerCaches registers the informers the admission handler reads from
// before the manager starts. Informers registered at this point are covered by
// the manager's initial cache sync, so by the time readiness flips the lookups
// are served from a warm cache. Left to itself the cache creates each informer
// lazily, on the first request that needs it, which pays the list+watch inline.
//
// Registration is cheap and non-blocking here: cache.GetInformer only waits for
// a sync once the cache is running, which it is not before mgr.Start.
//
// A type that cannot be resolved — CRD absent, e.g. a cluster running
// kustomize-controller but not helm-controller — is skipped with a warning
// rather than being fatal. The handler already tolerates an unreadable owner,
// and refusing to start would make the webhook unusable on such clusters.
func warmInformerCaches(mgr ctrl.Manager, log logr.Logger) {
	for _, obj := range webhookhandler.CachedObjectTypes() {
		name := fmt.Sprintf("%T", obj)
		if gvk, err := apiutil.GVKForObject(obj, mgr.GetScheme()); err == nil {
			name = gvk.String()
		}
		if _, err := mgr.GetCache().GetInformer(context.Background(), obj); err != nil {
			log.Info("skipping informer pre-warm; this type is not cached, so the checks "+
				"that read it stay fail-closed until its first use syncs it",
				"type", name, "error", err.Error())
			continue
		}
		log.V(1).Info("informer registered for initial cache sync", "type", name)
	}
}

// resyncIntervalDefault resolves the ValidatingWebhookConfiguration resync
// default from the environment. VWC_RESYNC_INTERVAL wins; DISCOVERY_INTERVAL is
// the pre-1.0 name, still honoured so an upgrade does not silently reset the
// cadence of a deployment that set it.
func resyncIntervalDefault() (time.Duration, error) {
	legacy, err := getEnvDuration("DISCOVERY_INTERVAL", config.DefaultVWCResyncInterval)
	if err != nil {
		return 0, err
	}
	return getEnvDuration("VWC_RESYNC_INTERVAL", legacy)
}

// mergeSystemControllerSAs returns the built-in default control-plane service
// accounts unioned with any operator-supplied "namespace:name" entries (CSV).
// Defaults are always included so configuring extras never drops them.
func mergeSystemControllerSAs(csv string) []string {
	result := append([]string{}, config.DefaultSystemControllerServiceAccounts()...)
	seen := make(map[string]bool, len(result))
	for _, e := range result {
		seen[e] = true
	}
	for _, e := range strings.Split(csv, ",") {
		e = strings.TrimSpace(e)
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		result = append(result, e)
	}
	return result
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q for %s: %w", v, key, err)
	}
	return d, nil
}
