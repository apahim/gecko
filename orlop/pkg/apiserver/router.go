package apiserver

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/conversion"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/handlers"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/middleware"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"

	"k8s.io/apimachinery/pkg/runtime"
)

// notImplementedHandler returns an http.HandlerFunc that responds with
// 501 Not Implemented for verbs not listed in +orlop:public-verbs.
func notImplementedHandler(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprintf(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","message":"verb %q is not supported for this resource","reason":"MethodNotAllowed","code":501}`, verb)
	}
}

// registerVerbsForResource registers HTTP handlers for each standard verb on
// path prefix p (e.g., "/clusters"). Verbs not permitted by info.VerbAllowed
// are wired to notImplementedHandler so callers receive 501 rather than 404.
func registerVerbsForResource(r chi.Router, p string, h *handlers.ConvertingResourceHandler, info types.ResourceInfo) {
	// Collection endpoints
	if info.VerbAllowed("create") {
		r.Post("/"+p, h.Create)
	} else {
		r.Post("/"+p, notImplementedHandler("create"))
	}
	if info.VerbAllowed("list") {
		r.Get("/"+p, h.List)
	} else {
		r.Get("/"+p, notImplementedHandler("list"))
	}
	// Item endpoints
	if info.VerbAllowed("get") {
		r.Get("/"+p+"/{name}", h.Get)
	} else {
		r.Get("/"+p+"/{name}", notImplementedHandler("get"))
	}
	if info.VerbAllowed("update") {
		r.Put("/"+p+"/{name}", h.Update)
	} else {
		r.Put("/"+p+"/{name}", notImplementedHandler("update"))
	}
	if info.VerbAllowed("patch") {
		r.Patch("/"+p+"/{name}", h.Patch)
	} else {
		r.Patch("/"+p+"/{name}", notImplementedHandler("patch"))
	}
	if info.VerbAllowed("delete") {
		r.Delete("/"+p+"/{name}", h.Delete)
	} else {
		r.Delete("/"+p+"/{name}", notImplementedHandler("delete"))
	}
}

// registerVerbsForNestedResource registers HTTP handlers for a nested
// (parent/child) route prefix (e.g., "/"). Verb restrictions from info apply.
func registerVerbsForNestedResource(r chi.Router, h *handlers.ConvertingResourceHandler, info types.ResourceInfo) {
	if info.VerbAllowed("create") {
		r.Post("/", h.Create)
	} else {
		r.Post("/", notImplementedHandler("create"))
	}
	if info.VerbAllowed("list") {
		r.Get("/", h.List)
	} else {
		r.Get("/", notImplementedHandler("list"))
	}
	if info.VerbAllowed("get") {
		r.Get("/{name}", h.Get)
	} else {
		r.Get("/{name}", notImplementedHandler("get"))
	}
	if info.VerbAllowed("update") {
		r.Put("/{name}", h.Update)
	} else {
		r.Put("/{name}", notImplementedHandler("update"))
	}
	if info.VerbAllowed("patch") {
		r.Patch("/{name}", h.Patch)
	} else {
		r.Patch("/{name}", notImplementedHandler("patch"))
	}
	if info.VerbAllowed("delete") {
		r.Delete("/{name}", h.Delete)
	} else {
		r.Delete("/{name}", notImplementedHandler("delete"))
	}
}

// healthChecker is a function that tests storage backend connectivity.
// It returns nil if the backend is healthy, or an error describing the problem.
type healthChecker func() error

func registerHealthEndpoints(r chi.Router, check healthChecker) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		if check != nil {
			if err := check(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte("storage check failed: " + err.Error()))
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
	r.Get("/healthz", handler)
	r.Get("/readyz", handler)
}

// setupConvertingRouter configures the HTTP router with converting handlers for public API.
// publicRegistry defines the public API types and schemas.
// privateRegistry provides the shared storage backend.
func setupConvertingRouter(publicRegistry *ResourceRegistry, privateRegistry *ResourceRegistry, converter *conversion.Converter, privateScheme *runtime.Scheme, corsOrigins []string, customMiddleware []func(http.Handler) http.Handler, check healthChecker) (chi.Router, error) {
	r := chi.NewRouter()

	// Add CORS middleware
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: corsOrigins,
	}))

	for _, mw := range customMiddleware {
		r.Use(mw)
	}

	registerHealthEndpoints(r, check)

	// Create discovery handler using public registry (for public API types)
	// Public API does not advertise status subresource (GCP-1062)
	advertiseStatus := false
	discoveryHandler := handlers.NewDiscoveryHandler(publicRegistry, &handlers.DiscoveryOptions{
		AdvertiseStatus: &advertiseStatus,
	})

	// Discovery endpoints (must be registered BEFORE resource routes to avoid shadowing)
	r.Get("/apis", discoveryHandler.APIGroupList)
	r.Get("/openapi/v2", discoveryHandler.OpenAPIV2)
	r.Get("/openapi/v3", discoveryHandler.OpenAPIV3)

	// Group resources by GroupVersion (from public registry)
	gvResources := make(map[string][]ResourceInfo)
	for _, res := range publicRegistry.GetResources() {
		gv := fmt.Sprintf("%s/%s", res.GVK.Group, res.GVK.Version)
		gvResources[gv] = append(gvResources[gv], res)
	}

	// Setup routes for each GroupVersion
	// Errors from handler construction are captured here and returned
	// after all routes are processed, so a misconfigured resource is
	// never silently advertised in discovery with no routes.
	var routeErr error
	for gv, resources := range gvResources {
		group := resources[0].GVK.Group
		version := resources[0].GVK.Version
		apiPath := "/apis/" + gv

		r.Route(apiPath, func(r chi.Router) {
			// Discovery endpoint for this specific group/version (before namespaced routes)
			r.Get("/", func(w http.ResponseWriter, req *http.Request) {
				discoveryHandler.APIResourceList(w, req, group, version)
			})

			// Cluster-scoped resources get CRUD directly under the GV path
			for _, res := range resources {
				if res.Namespaced {
					continue
				}
				handlerInterface, err := createConvertingHandlerWithSharedStore(publicRegistry, privateRegistry, converter, privateScheme, res)
				if err != nil {
					routeErr = fmt.Errorf("resource %s/%s %s: %w", res.GVK.Group, res.GVK.Version, res.Plural, err)
					return
				}
				handler := handlerInterface.(*handlers.ConvertingResourceHandler)
				registerVerbsForResource(r, res.Plural, handler, res)
				// Status updates not allowed on public API (GCP-1062)
			}

			// Namespaced resources: LIST across all namespaces + CRUD under /namespaces/{namespace}
			type convertingEntry struct {
				res     ResourceInfo
				plural  string
				handler *handlers.ConvertingResourceHandler
			}
			var namespacedHandlers []convertingEntry
			for _, res := range resources {
				if !res.Namespaced {
					continue
				}
				handlerInterface, err := createConvertingHandlerWithSharedStore(publicRegistry, privateRegistry, converter, privateScheme, res)
				if err != nil {
					routeErr = fmt.Errorf("resource %s/%s %s: %w", res.GVK.Group, res.GVK.Version, res.Plural, err)
					return
				}
				handler := handlerInterface.(*handlers.ConvertingResourceHandler)
				// Cross-namespace list: honor verb restriction
				if res.VerbAllowed("list") {
					r.Get("/"+res.Plural, handler.List)
				} else {
					r.Get("/"+res.Plural, notImplementedHandler("list"))
				}
				namespacedHandlers = append(namespacedHandlers, convertingEntry{res, res.Plural, handler})
			}
			if len(namespacedHandlers) > 0 {
				r.Route("/namespaces/{namespace}", func(r chi.Router) {
					for _, nh := range namespacedHandlers {
						registerVerbsForResource(r, nh.plural, nh.handler, nh.res)
						// Status updates not allowed on public API (GCP-1062)

						if nh.res.ParentResource != nil {
							parentPlural := nh.res.ParentResource.Plural
							idField := nh.res.ParentResource.IDField
							childPlural := nh.plural
							handler := nh.handler
							res := nh.res
							r.Route("/"+parentPlural+"/{parentID}/"+childPlural, func(r chi.Router) {
								r.Use(parentFilterMiddleware(idField, "parentID"))
								registerVerbsForNestedResource(r, handler, res)
								// Status updates not allowed on public API (GCP-1062)
							})
						}
					}
				})
			}
		})

		if routeErr != nil {
			return nil, fmt.Errorf("setting up converting routes: %w", routeErr)
		}

		// Per-group discovery endpoint
		r.Get("/apis/"+group, func(w http.ResponseWriter, req *http.Request) {
			discoveryHandler.APIGroup(w, req, group)
		})

		// OpenAPI v3 per-group-version endpoint
		r.Get("/openapi/v3/apis/"+gv, func(w http.ResponseWriter, req *http.Request) {
			discoveryHandler.OpenAPIV3GroupVersion(w, req, group, version)
		})
	}

	return r, nil
}
