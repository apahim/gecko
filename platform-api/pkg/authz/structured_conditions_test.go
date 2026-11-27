package authz

import (
	"testing"

	"github.com/cedar-policy/cedar-go"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestStructuredBindingIsolationAndReload(t *testing.T) {
	stores, authorizer := newTestAuthorizer(t)
	role := &privatev1.PlatformRole{Spec: privatev1.PlatformRoleSpec{Permissions: []string{"cluster.get", "nodepool.get", "role.get", "rolebinding.get"}}}
	role.Name = "mixed"
	role.SetGroupVersionKind(privatev1.GroupVersion.WithKind("PlatformRole"))
	if err := stores.PlatformRoles.Create(t.Context(), role); err != nil {
		t.Fatal(err)
	}
	binding := &privatev1.RoleBinding{Spec: privatev1.RoleBindingSpec{Subject: "alice@example.com", RoleRef: privatev1.RoleRef{Kind: "PlatformRole", Name: "mixed", APIGroup: privatev1.GroupVersion.Group}, Conditions: []privatev1.RoleBindingRequirement{{Key: "region", Operator: "In", Values: []string{"us-central1", "europe-*"}}, {Key: "name", Operator: "NotIn", Values: []string{"*-internal"}}}}}
	binding.Name = "regional"
	binding.Namespace = "project-a"
	binding.SetGroupVersionKind(privatev1.GroupVersion.WithKind("RoleBinding"))
	if err := stores.RoleBindings.Create(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	reload := func() {
		t.Helper()
		if err := authorizer.Reload(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	reload()
	check := func(plural, name, region, zone, namespace string, action Action, want bool) {
		t.Helper()
		gcp := map[string]interface{}{}
		if region != "" {
			gcp["region"] = region
		}
		if zone != "" {
			gcp["zone"] = zone
		}
		object := &unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": name}, "spec": map[string]interface{}{"platform": map[string]interface{}{"gcp": gcp}}}}
		context, err := CedarContextForObject(object, plural, "GET")
		if err != nil {
			t.Fatal(err)
		}
		got, err := authorizer.AuthorizeWithContext(t.Context(), "alice@example.com", action, namespace, context)
		if err != nil || got != want {
			t.Fatalf("%s/%s/%s: got %v %v, want %v", plural, name, region, got, err, want)
		}
	}
	check("clusters", "demo", "us-central1", "", "project-a", GetCluster, true)
	check("clusters", "demo", "europe-west2", "", "project-a", GetCluster, true)
	check("clusters", "demo-internal", "us-central1", "", "project-a", GetCluster, false)
	check("clusters", "demo", "us-west4", "", "project-a", GetCluster, false)
	check("nodepools", "workers", "", "us-central1-a", "project-a", GetNodepool, true)
	check("nodepools", "workers", "", "us-west4-ai2b", "project-a", GetNodepool, false)
	check("roles", "demo", "us-central1", "", "project-a", GetRole, false)
	check("rolebindings", "demo", "us-central1", "", "project-a", GetRoleBinding, false)
	check("clusters", "demo", "us-central1", "", "project-b", GetCluster, false)
	check("clusters", "demo", "us-central1", "", "project-a", DeleteCluster, false)
	check("clusters", "", "us-central1", "", "project-a", GetCluster, false)
	// A separate grant is additive, never narrowed by the regional grant.
	independent := binding.DeepCopy()
	independent.Name = "name-only"
	independent.ResourceVersion = ""
	independent.UID = ""
	independent.Spec.Conditions = []privatev1.RoleBindingRequirement{{Key: "name", Operator: "In", Values: []string{"demo"}}}
	if err := stores.RoleBindings.Create(t.Context(), independent); err != nil {
		t.Fatal(err)
	}
	reload()
	check("roles", "demo", "", "", "project-a", GetRole, true)
	check("rolebindings", "demo", "", "", "project-a", GetRoleBinding, true)
	check("clusters", "demo", "us-west4", "", "project-a", GetCluster, true)
	if err := stores.RoleBindings.Delete(t.Context(), "project-a", "name-only"); err != nil {
		t.Fatal(err)
	}
	// A corrupted binding cannot bypass the remaining valid grant or prevent reload.
	corrupt := binding.DeepCopy()
	corrupt.Name = "corrupt"
	corrupt.ResourceVersion = ""
	corrupt.Spec.Conditions = []privatev1.RoleBindingRequirement{{Key: "invalid", Operator: "In", Values: []string{"x"}}}
	if err := stores.RoleBindings.Create(t.Context(), corrupt); err != nil {
		t.Fatal(err)
	}
	reload()
	check("roles", "demo", "", "", "project-a", GetRole, false)
	check("clusters", "demo", "us-central1", "", "project-a", GetCluster, true)
	if err := stores.RoleBindings.Delete(t.Context(), "project-a", "regional"); err != nil {
		t.Fatal(err)
	}
	reload()
	check("clusters", "demo", "us-central1", "", "project-a", GetCluster, false)
}

func TestInvalidStructuredBindingCannotAdmitList(t *testing.T) {
	stores, authorizer := newTestAuthorizer(t)
	seedClusterListBinding(t, stores, "project-b", nil)
	allowedBefore, err := authorizer.HasNamespacePermission(t.Context(), "alice@example.com", ListClusters, "project-b")
	if err != nil || !allowedBefore {
		t.Fatalf("valid namespace permission missing: %v %v", allowedBefore, err)
	}
	other, err := authorizer.HasNamespacePermission(t.Context(), "alice@example.com", ListClusters, "project-a")
	if err != nil || other {
		t.Fatalf("permission crossed namespace: %v %v", other, err)
	}
	object, err := stores.RoleBindings.Get(t.Context(), "project-b", "alice-viewer")
	if err != nil {
		t.Fatal(err)
	}
	binding := object.(*privatev1.RoleBinding)
	binding.Spec.Conditions = []privatev1.RoleBindingRequirement{{Key: "invalid", Operator: "In", Values: []string{"x"}}}
	if err := stores.RoleBindings.Update(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	allowed, err := authorizer.HasNamespacePermission(t.Context(), "alice@example.com", ListClusters, "project-b")
	if err != nil || allowed {
		t.Fatalf("invalid structured coarse admission: %v %v", allowed, err)
	}
}

func TestStructuredBindingEvaluationErrorIsolation(t *testing.T) {
	stores, authorizer := newTestAuthorizer(t)
	seedClusterGetBinding(t, stores, "project-a", nil)
	binding := &privatev1.RoleBinding{Spec: privatev1.RoleBindingSpec{Subject: "alice@example.com", RoleRef: privatev1.RoleRef{Kind: "PlatformRole", Name: "cluster-viewer", APIGroup: privatev1.GroupVersion.Group}, Conditions: []privatev1.RoleBindingRequirement{{Key: "region", Operator: "In", Values: []string{"us-*"}}}}}
	binding.Name = "pattern"
	binding.Namespace = "project-a"
	binding.SetGroupVersionKind(privatev1.GroupVersion.WithKind("RoleBinding"))
	if err := stores.RoleBindings.Create(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Inject an impossible normalized-attribute type to exercise evaluator failure,
	// not schema validation. One erroneous binding must not cancel a valid grant.
	wrongType := cedar.NewRecord(cedar.RecordMap{"resourceRegion": cedar.Long(1)})
	allowed, err := authorizer.AuthorizeWithContext(t.Context(), "alice@example.com", GetCluster, "project-a", wrongType)
	if err != nil || !allowed {
		t.Fatalf("independent grant canceled by evaluation error: %v %v", allowed, err)
	}
	if err := stores.RoleBindings.Delete(t.Context(), "project-a", "alice-viewer"); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	allowed, err = authorizer.AuthorizeWithContext(t.Context(), "alice@example.com", GetCluster, "project-a", wrongType)
	if err == nil || allowed {
		t.Fatalf("evaluation error did not fail closed: %v %v", allowed, err)
	}
}

// TestLegacyRawConditionIgnored documents the intentionally permissive cutover:
// old raw restrictions have no effect; namespace, role actions and structured
// requirements still gate access.
func TestLegacyRawConditionIgnored(t *testing.T) {
	for _, raw := range []string{"false", "true", "invalid Cedar"} {
		t.Run(raw, func(t *testing.T) {
			stores, authorizer := newTestAuthorizer(t)
			seedClusterBinding(t, stores, "project-a", []string{"cluster.get", "cluster.list"}, nil)
			object, err := stores.RoleBindings.Get(t.Context(), "project-a", "alice-viewer")
			if err != nil {
				t.Fatal(err)
			}
			document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
			if err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(document, raw, "spec", "condition"); err != nil {
				t.Fatal(err)
			}
			var binding privatev1.RoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(document, &binding); err != nil {
				t.Fatal(err)
			}
			if err := stores.RoleBindings.Update(t.Context(), &binding); err != nil {
				t.Fatal(err)
			}
			if err := authorizer.Reload(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct {
				email, namespace string
				action           Action
				want             bool
			}{
				{"alice@example.com", "project-a", GetCluster, true},
				{"alice@example.com", "project-b", GetCluster, false},
				{"alice@example.com", "project-a", DeleteCluster, false},
				{"bob@example.com", "project-a", GetCluster, false},
			} {
				got, err := authorizer.Authorize(t.Context(), check.email, check.action, check.namespace)
				if err != nil || got != check.want {
					t.Fatalf("legacy grant got=%v err=%v want=%v", got, err, check.want)
				}
			}
			allowed, err := authorizer.HasNamespacePermission(t.Context(), "alice@example.com", ListClusters, "project-a")
			if err != nil || !allowed {
				t.Fatalf("legacy list admission: %v %v", allowed, err)
			}
			// Even raw "true" cannot override a structured restriction.
			if err := unstructured.SetNestedField(document, binding.ResourceVersion, "metadata", "resourceVersion"); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedSlice(document, []interface{}{map[string]interface{}{"key": "name", "operator": "In", "values": []interface{}{"demo"}}}, "spec", "conditions"); err != nil {
				t.Fatal(err)
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(document, &binding); err != nil {
				t.Fatal(err)
			}
			if err := stores.RoleBindings.Update(t.Context(), &binding); err != nil {
				t.Fatal(err)
			}
			if err := authorizer.Reload(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"demo", "other"} {
				requestContext, err := cedarContext(name, "clusters", "GET", nil)
				if err != nil {
					t.Fatal(err)
				}
				got, err := authorizer.AuthorizeWithContext(t.Context(), "alice@example.com", GetCluster, "project-a", requestContext)
				if err != nil || got != (name == "demo") {
					t.Fatalf("structured grant for %s got=%v err=%v", name, got, err)
				}
			}
		})
	}
}

func TestEmptyStoredRequirementsGrantNothing(t *testing.T) {
	for _, key := range []string{"name", "region"} {
		for _, op := range []string{"In", "NotIn"} {
			t.Run(key+"/"+op, func(t *testing.T) {
				stores, a := newTestAuthorizer(t)
				seedClusterBinding(t, stores, "project-a", []string{"cluster.get", "cluster.list"}, nil)
				original, err := stores.RoleBindings.Get(t.Context(), "project-a", "alice-viewer")
				if err != nil {
					t.Fatal(err)
				}
				bad := original.(*privatev1.RoleBinding).DeepCopy()
				bad.Name = "invalid-empty"
				bad.ResourceVersion = ""
				bad.UID = ""
				bad.Spec.Conditions = []privatev1.RoleBindingRequirement{{Key: privatev1.RoleBindingConditionKey(key), Operator: privatev1.RoleBindingConditionOperator(op), Values: []string{}}}
				if err := stores.RoleBindings.Create(t.Context(), bad); err != nil {
					t.Fatal(err)
				}
				if err := a.Reload(t.Context()); err != nil {
					t.Fatal(err)
				}
				allowed, err := a.Authorize(t.Context(), "alice@example.com", GetCluster, "project-a")
				if err != nil || !allowed {
					t.Fatalf("valid independent binding lost: %v %v", allowed, err)
				}
				if err := stores.RoleBindings.Delete(t.Context(), "project-a", "alice-viewer"); err != nil {
					t.Fatal(err)
				}
				if err := a.Reload(t.Context()); err != nil {
					t.Fatal(err)
				}
				allowed, err = a.Authorize(t.Context(), "alice@example.com", GetCluster, "project-a")
				if err != nil || allowed {
					t.Fatalf("empty values granted access: %v %v", allowed, err)
				}
				allowed, err = a.HasNamespacePermission(t.Context(), "alice@example.com", ListClusters, "project-a")
				if err != nil || allowed {
					t.Fatalf("empty values admitted list: %v %v", allowed, err)
				}
			})
		}
	}
}
