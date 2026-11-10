// Package replicationguard prevents public writes to replicated resources in
// configured follower regions.
package replicationguard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Config describes this API server's replication mode.
//
//nolint:kubeapilinter // Middleware configuration is not a serialized API type.
type Config struct {
	Enabled      bool
	Region       string
	LeaderRegion string
	Replicate    string
	Resources    []types.ResourceInfo
}

//nolint:kubeapilinter // Internal route lookup key, not a serialized API type.
type resourceRoute struct {
	group        string
	version      string
	plural       string
	parentPlural string
	namespaced   bool
}

//nolint:kubeapilinter // Internal request path, not a serialized API type.
type parsedRoute struct {
	resourceRoute
	named         bool
	trailingSlash bool
}

//nolint:kubeapilinter // Internal method lookup value, not a serialized API type.
type writeOperation struct {
	verb         string
	requiresName bool
}

var writeOperations = map[string]writeOperation{
	http.MethodPost:   {verb: "create"},
	http.MethodPut:    {verb: "update", requiresName: true},
	http.MethodPatch:  {verb: "patch", requiresName: true},
	http.MethodDelete: {verb: "delete", requiresName: true},
}

var (
	rejectedWrites = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      "replication",
			Name:           "readonly_write_rejections_total",
			Help:           "Public API writes rejected because this region is a replication follower.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"region", "resource_kind", "verb"},
	)
	registerMetrics sync.Once
)

// New validates the configured mode and returns a public API middleware.
func New(config Config) (func(http.Handler) http.Handler, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	var replicated map[resourceRoute]string
	if config.Enabled {
		var err error
		replicated, err = parseResources(config.Replicate, config.Resources)
		if err != nil {
			return nil, fmt.Errorf("replicate: %w", err)
		}
		// Register the rejection counter only when the guard is enabled; a
		// disabled deployment would otherwise export an always-zero series. Only
		// the private Kubernetes API port serves this registry at /metrics (the
		// public router has none), so follower rejection counts are scrapeable
		// only on the private port.
		registerMetrics.Do(func() {
			legacyregistry.MustRegister(rejectedWrites)
		})
	}

	return func(next http.Handler) http.Handler {
		if !config.Enabled || config.Region == config.LeaderRegion {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resourceKind, verb, replicatedWrite := replicatedMutation(r, replicated)
			if !replicatedWrite {
				next.ServeHTTP(w, r)
				return
			}

			rejectedWrites.WithLabelValues(config.Region, resourceKind, verb).Inc()
			writeReadOnlyStatus(w, config.Region, config.LeaderRegion, resourceKind)
		})
	}, nil
}

// writeReadOnlyStatus rejects a replicated write in a follower region with a
// Kubernetes metav1.Status, matching the shape the public API's resource
// handlers return so k8s-aware clients parse the error and surface its message.
//
// The status is 405 (MethodNotAllowed), not 409 (Conflict): 409 signals a
// retryable resource-version conflict and clients commonly retry it, whereas a
// follower region is permanently read-only. The Allow header and a region-aware
// message make clear that writes belong on the leader region, since 405 alone
// would read as "this resource never supports writes".
func writeReadOnlyStatus(w http.ResponseWriter, region, leaderRegion, resourceKind string) {
	status := metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReasonMethodNotAllowed,
		Code:     http.StatusMethodNotAllowed,
		Message: fmt.Sprintf(
			"region %q is a replication follower and does not accept writes to %s resources; send writes to the leader region %q",
			region, resourceKind, leaderRegion),
	}
	w.Header().Set("Allow", http.MethodGet)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(status)
}

func (config Config) validate() error {
	if !config.Enabled {
		return nil
	}
	if config.Region == "" || config.LeaderRegion == "" ||
		config.Region != strings.TrimSpace(config.Region) ||
		config.LeaderRegion != strings.TrimSpace(config.LeaderRegion) {
		return fmt.Errorf("enabled replication write guard requires nonempty, unpadded region and leader region")
	}
	return nil
}

// parseResources resolves the same plural.group identifiers used by the
// replication controller against the public API's registered resources.
func parseResources(csv string, available []types.ResourceInfo) (map[resourceRoute]string, error) {
	registered := make(map[string]types.ResourceInfo, len(available))
	for _, resource := range available {
		id := resource.Plural + "." + resource.GVK.Group
		registered[id] = resource
	}

	selected := make(map[resourceRoute]string)
	for _, part := range strings.Split(csv, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		resource, ok := registered[id]
		if !ok {
			return nil, fmt.Errorf("unsupported public replicated resource %q", id)
		}
		route := resourceRoute{
			group: resource.GVK.Group, version: resource.GVK.Version,
			plural: resource.Plural, namespaced: resource.Namespaced,
		}
		// Direct route key: /apis/<g>/<v>[/namespaces/<ns>]/<plural>.
		selected[route] = resource.GVK.Kind
		if resource.Namespaced && resource.ParentResource != nil {
			// Nested route key: /apis/<g>/<v>/namespaces/<ns>/<parent>/<id>/<plural>.
			// orlop serves a nested child on both the direct and the nested parent
			// route, so both keys are load-bearing; keep both assignments.
			route.parentPlural = resource.ParentResource.Plural
			selected[route] = resource.GVK.Kind
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("at least one replicated resource is required")
	}
	return selected, nil
}

// replicatedMutation matches mutating public routes for configured resources.
func replicatedMutation(r *http.Request, replicated map[resourceRoute]string) (resourceKind, verb string, matched bool) {
	if r == nil || r.URL == nil {
		return "", "", false
	}
	operation, ok := writeOperations[r.Method]
	if !ok {
		return "", "", false
	}
	// Chi routes on RawPath when present. Inspect the same path so encoded
	// separators in names or namespaces cannot bypass the write guard.
	requestPath := r.URL.Path
	if r.URL.RawPath != "" {
		requestPath = r.URL.RawPath
	}
	parsed, ok := parseRoute(requestPath)
	if !ok || parsed.named != operation.requiresName {
		return "", "", false
	}
	if parsed.trailingSlash && (parsed.parentPlural == "" || parsed.named || r.Method != http.MethodPost) {
		return "", "", false
	}
	resourceKind, ok = replicated[parsed.resourceRoute]
	if !ok {
		return "", "", false
	}
	return resourceKind, operation.verb, true
}

// parseRoute decomposes a public API request path into the resource coordinates
// the guard matches against. It mirrors how chi dispatches these routes instead
// of imposing its own idea of a well-formed path: chi matches on raw path
// segments without canonicalization, so parseRoute splits on "/" and never
// cleans or rewrites the path. A "." / ".." or empty segment that chi still
// routes to a handler — "/namespaces/../roles", "/namespaces//roles", a "."
// resource name, and the like — is therefore parsed like any other request, so
// those writes cannot reach a handler unseen by the guard.
//
// Only the segments chi itself treats as fixed gate a match: the "apis" prefix,
// the "namespaces" marker, and the resource plural (which the caller verifies
// against the replicated set). The segments chi captures as parameters —
// namespace, parent ID, and resource name — are taken verbatim. The one
// structural requirement is that a named route carry a non-empty name, because
// chi returns 404 for a trailing-slash item path such as "/roles/" rather than
// routing it.
//
// This matches exactly the route shapes orlop registers today. The public API
// has no subresources (GCP-1062); if a replicated resource ever gained one, a
// path like "/<plural>/<name>/status" would parse here as a nested route with
// plural "status" and pass through unfenced. Since --replicate is operator
// configurable, keep this parser in sync with the routes orlop registers (see
// orlop/pkg/apiserver/router.go), and prefer a router-level test asserting every
// registered mutating route for a replicated resource is rejected.
func parseRoute(rawPath string) (parsedRoute, bool) {
	// A trailing slash is meaningful only for nested collection routes, which chi
	// serves with one. Record and strip it here; replicatedMutation decides where
	// it is allowed.
	parsed := parsedRoute{trailingSlash: strings.HasSuffix(rawPath, "/")}
	if parsed.trailingSlash {
		rawPath = strings.TrimSuffix(rawPath, "/")
	}
	if !strings.HasPrefix(rawPath, "/") {
		return parsedRoute{}, false
	}

	parts := strings.Split(strings.TrimPrefix(rawPath, "/"), "/")
	if len(parts) < 4 || parts[0] != "apis" {
		return parsedRoute{}, false
	}

	parsed.resourceRoute = resourceRoute{group: parts[1], version: parts[2]}
	switch {
	case (len(parts) == 6 || len(parts) == 7) && parts[3] == "namespaces":
		parsed.namespaced = true
		parsed.plural = parts[5]
		parsed.named = len(parts) == 7
	case (len(parts) == 8 || len(parts) == 9) && parts[3] == "namespaces":
		parsed.namespaced = true
		parsed.parentPlural = parts[5]
		parsed.plural = parts[7]
		parsed.named = len(parts) == 9
	case len(parts) == 4 || len(parts) == 5:
		parsed.plural = parts[3]
		parsed.named = len(parts) == 5
	default:
		return parsedRoute{}, false
	}
	if parsed.plural == "" || parsed.named && parts[len(parts)-1] == "" {
		return parsedRoute{}, false
	}
	return parsed, true
}
