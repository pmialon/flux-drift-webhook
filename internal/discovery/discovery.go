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

package discovery

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/pmialon/flux-drift-webhook/internal/config"
)

// Discoverer enumerates the cluster's API GroupVersions, excluding groups that
// would cause a reconcile loop with the webhook's own configuration.
type Discoverer struct {
	client discovery.DiscoveryInterface
	log    logr.Logger
}

// NewDiscoverer returns a Discoverer backed by the given discovery client.
func NewDiscoverer(client discovery.DiscoveryInterface, log logr.Logger) *Discoverer {
	return &Discoverer{
		client: client,
		log:    log.WithName("discovery"),
	}
}

// minMatchConditionsMinor is the first Kubernetes minor version where webhook
// matchConditions are beta and on by default (1.28); they are alpha and gated
// off in 1.27, and GA from 1.30. On an older server the field is silently
// pruned from the ValidatingWebhookConfiguration, which would leave wildcard
// rules with no exclusion at all — hence the check before enabling them.
const minMatchConditionsMinor = 28

// SupportsMatchConditions reports whether the API server is recent enough to
// honour ValidatingWebhook matchConditions. It returns the reported version so
// callers can log what they saw when falling back.
func (d *Discoverer) SupportsMatchConditions() (bool, string, error) {
	info, err := d.client.ServerVersion()
	if err != nil {
		return false, "", fmt.Errorf("querying server version: %w", err)
	}

	reported := info.GitVersion
	if reported == "" {
		reported = info.Major + "." + info.Minor
	}

	major, err := leadingInt(info.Major)
	if err != nil {
		return false, reported, fmt.Errorf("parsing server major version %q: %w", info.Major, err)
	}
	// Distributions decorate the minor version ("28+", "30-gke.5"), so only the
	// leading digits are meaningful.
	minor, err := leadingInt(info.Minor)
	if err != nil {
		return false, reported, fmt.Errorf("parsing server minor version %q: %w", info.Minor, err)
	}

	if major > 1 {
		return true, reported, nil
	}
	return major == 1 && minor >= minMatchConditionsMinor, reported, nil
}

// leadingInt parses the leading run of digits in s, ignoring any suffix.
func leadingInt(s string) (int, error) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("no leading digits in %q", s)
	}
	return strconv.Atoi(s[:end])
}

// DiscoverGroupVersions returns the distinct, non-excluded API GroupVersions
// currently served by the cluster.
func (d *Discoverer) DiscoverGroupVersions(ctx context.Context) ([]schema.GroupVersion, error) {
	apiResourceLists, err := d.fetchAPIResources(ctx)
	if err != nil {
		return nil, err
	}

	result := d.filterGroupVersions(apiResourceLists)
	d.log.V(1).Info("discovered GroupVersions", "count", len(result))
	return result, nil
}

// fetchAPIResources wraps ServerGroupsAndResources with context cancellation
// support (the underlying call does not accept a context).
func (d *Discoverer) fetchAPIResources(ctx context.Context) ([]*metav1.APIResourceList, error) {
	type discoveryResult struct {
		lists []*metav1.APIResourceList
		err   error
	}
	ch := make(chan discoveryResult, 1)
	go func() {
		_, lists, err := d.client.ServerGroupsAndResources()
		ch <- discoveryResult{lists: lists, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("discovery cancelled: %w", ctx.Err())
	case res := <-ch:
		if res.err != nil && res.lists == nil {
			return nil, fmt.Errorf("discovery failed completely: %w", res.err)
		}
		if res.err != nil {
			d.log.Info("partial discovery error, continuing with available groups", "error", res.err)
		}
		return res.lists, nil
	}
}

func (d *Discoverer) filterGroupVersions(lists []*metav1.APIResourceList) []schema.GroupVersion {
	seen := make(map[schema.GroupVersion]bool)
	result := make([]schema.GroupVersion, 0, len(lists))

	for _, apiResourceList := range lists {
		if apiResourceList == nil {
			continue
		}

		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			d.log.V(1).Info("failed to parse GroupVersion",
				"groupVersion", apiResourceList.GroupVersion, "error", err)
			continue
		}

		if slices.Contains(config.ExcludedGroups(), gv.Group) || seen[gv] {
			continue
		}

		seen[gv] = true
		result = append(result, gv)
	}

	return result
}
