package aggregated

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"

	testv1 "github.com/openshift-online/gecko/orlop/apis/private/test/v1"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"
	pkgschema "github.com/openshift-online/gecko/orlop/pkg/apiserver/schema"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"sigs.k8s.io/yaml"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := testv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add test scheme: %v", err)
	}
	return scheme
}

var testGVK = schema.GroupVersionKind{
	Group:   testv1.GroupVersion.Group,
	Version: testv1.GroupVersion.Version,
	Kind:    "Object",
}

func newTestStrategy(t *testing.T, namespaced bool) *ResourceStrategy {
	t.Helper()
	return NewResourceStrategy(newTestScheme(t), nil, namespaced, testGVK, logr.Discard())
}

func TestPrepareForCreate(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	obj := &testv1.Object{
		TypeMeta: metav1.TypeMeta{
			APIVersion: testv1.GroupVersion.String(),
			Kind:       "Object",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
		Spec: testv1.ObjectSpec{
			PublicField: "value",
		},
	}

	strategy.PrepareForCreate(ctx, obj)

	if obj.UID == "" {
		t.Error("expected UID to be set after PrepareForCreate")
	}
	if obj.CreationTimestamp.IsZero() {
		t.Error("expected CreationTimestamp to be set after PrepareForCreate")
	}
	if obj.Generation != 1 {
		t.Errorf("expected Generation to be 1, got %d", obj.Generation)
	}
}

func TestPrepareForUpdate_SpecChanged(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-obj",
			Namespace:  "default",
			UID:        "old-uid",
			Generation: 3,
			CreationTimestamp: metav1.Time{
				Time: metav1.Now().Add(-1 * 60 * 1e9),
			},
		},
		Spec: testv1.ObjectSpec{
			PublicField: "original",
		},
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
		Spec: testv1.ObjectSpec{
			PublicField: "changed",
		},
	}

	strategy.PrepareForUpdate(ctx, newObj, oldObj)

	if newObj.Generation != 4 {
		t.Errorf("expected Generation to be 4 (incremented), got %d", newObj.Generation)
	}
}

func TestPrepareForUpdate_SpecUnchanged(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-obj",
			Namespace:  "default",
			UID:        "old-uid",
			Generation: 5,
			CreationTimestamp: metav1.Time{
				Time: metav1.Now().Add(-1 * 60 * 1e9),
			},
		},
		Spec: testv1.ObjectSpec{
			PublicField: "same-value",
		},
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
		Spec: testv1.ObjectSpec{
			PublicField: "same-value",
		},
	}

	strategy.PrepareForUpdate(ctx, newObj, oldObj)

	if newObj.Generation != 5 {
		t.Errorf("expected Generation to remain 5, got %d", newObj.Generation)
	}
}

func TestPrepareForUpdate_PreservesMetadata(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	creationTime := metav1.Now()
	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-obj",
			Namespace:         "default",
			UID:               "preserved-uid",
			Generation:        2,
			CreationTimestamp: creationTime,
		},
		Spec: testv1.ObjectSpec{
			PublicField: "value",
		},
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
			// UID and CreationTimestamp intentionally not set;
			// PrepareForUpdate should copy them from old.
		},
		Spec: testv1.ObjectSpec{
			PublicField: "value",
		},
	}

	strategy.PrepareForUpdate(ctx, newObj, oldObj)

	if newObj.UID != "preserved-uid" {
		t.Errorf("expected UID %q, got %q", "preserved-uid", newObj.UID)
	}
	if !newObj.CreationTimestamp.Equal(&creationTime) {
		t.Errorf("expected CreationTimestamp %v, got %v", creationTime, newObj.CreationTimestamp)
	}
}

func TestNamespaceScoped(t *testing.T) {
	tests := []struct {
		name       string
		namespaced bool
	}{
		{"returns true when namespaced", true},
		{"returns false when cluster-scoped", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			strategy := newTestStrategy(t, tc.namespaced)
			if got := strategy.NamespaceScoped(); got != tc.namespaced {
				t.Errorf("NamespaceScoped() = %v, want %v", got, tc.namespaced)
			}
		})
	}
}

func TestAllowCreateOnUpdate(t *testing.T) {
	strategy := newTestStrategy(t, true)
	if strategy.AllowCreateOnUpdate(t.Context()) {
		t.Error("expected AllowCreateOnUpdate() to return false")
	}
}

func TestAllowUnconditionalUpdate(t *testing.T) {
	strategy := newTestStrategy(t, true)
	if strategy.AllowUnconditionalUpdate(t.Context()) {
		t.Error("expected AllowUnconditionalUpdate() to return false")
	}
}

func TestPrepareForUpdate_PreservesDeletionTimestamp(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	deletionTime := metav1.Now()
	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-obj",
			Namespace:         "default",
			UID:               "old-uid",
			Generation:        2,
			DeletionTimestamp: &deletionTime,
			Finalizers:        []string{"test-finalizer"},
			CreationTimestamp: metav1.Time{Time: metav1.Now().Add(-1 * 60 * 1e9)},
		},
		Spec: testv1.ObjectSpec{
			PublicField: "value",
		},
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
			// DeletionTimestamp intentionally not set by client.
		},
		Spec: testv1.ObjectSpec{
			PublicField: "value",
		},
	}

	strategy.PrepareForUpdate(ctx, newObj, oldObj)

	if newObj.DeletionTimestamp == nil {
		t.Fatal("expected DeletionTimestamp to be preserved, got nil")
	}
	if !newObj.DeletionTimestamp.Equal(&deletionTime) {
		t.Errorf("expected DeletionTimestamp %v, got %v", deletionTime, newObj.DeletionTimestamp)
	}
}

func TestPrepareForUpdate_NoDeletionTimestamp(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-obj",
			Namespace:         "default",
			UID:               "old-uid",
			Generation:        2,
			CreationTimestamp: metav1.Time{Time: metav1.Now().Add(-1 * 60 * 1e9)},
		},
		Spec: testv1.ObjectSpec{
			PublicField: "value",
		},
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
		Spec: testv1.ObjectSpec{
			PublicField: "value",
		},
	}

	strategy.PrepareForUpdate(ctx, newObj, oldObj)

	if newObj.DeletionTimestamp != nil {
		t.Errorf("expected DeletionTimestamp to remain nil, got %v", newObj.DeletionTimestamp)
	}
}

// defaulterObject wraps testv1.Object to implement types.CustomDefaulter.
type defaulterObject struct {
	testv1.Object
	defaultCalled bool
	defaultErr    error
}

func (d *defaulterObject) Default(_ context.Context) error {
	d.defaultCalled = true
	return d.defaultErr
}

func (d *defaulterObject) DeepCopyObject() runtime.Object {
	cp := *d
	cp.Object = *d.DeepCopy()
	return &cp
}

func TestPrepareForCreate_CallsCustomDefaulter(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	obj := &defaulterObject{
		Object: testv1.Object{
			TypeMeta: metav1.TypeMeta{
				APIVersion: testv1.GroupVersion.String(),
				Kind:       "Object",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-obj",
				Namespace: "default",
			},
		},
	}

	strategy.PrepareForCreate(ctx, obj)

	if !obj.defaultCalled {
		t.Error("expected CustomDefaulter.Default to be called during PrepareForCreate")
	}
}

func TestPrepareForUpdate_CallsCustomDefaulter(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-obj",
			Namespace:         "default",
			UID:               "old-uid",
			Generation:        1,
			CreationTimestamp: metav1.Time{Time: metav1.Now().Add(-1 * 60 * 1e9)},
		},
		Spec: testv1.ObjectSpec{PublicField: "value"},
	}

	newObj := &defaulterObject{
		Object: testv1.Object{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-obj",
				Namespace: "default",
			},
			Spec: testv1.ObjectSpec{PublicField: "value"},
		},
	}

	strategy.PrepareForUpdate(ctx, newObj, oldObj)

	if !newObj.defaultCalled {
		t.Error("expected CustomDefaulter.Default to be called during PrepareForUpdate")
	}
}

// validatorObject wraps testv1.Object to implement types.CustomValidator.
type validatorObject struct {
	testv1.Object
	validateCreateErr error
	validateUpdateErr error
}

func (v *validatorObject) ValidateCreate(_ context.Context) error {
	return v.validateCreateErr
}

func (v *validatorObject) ValidateUpdate(_ context.Context, _ runtime.Object) error {
	return v.validateUpdateErr
}

func (v *validatorObject) ValidateDelete(_ context.Context) error {
	return nil
}

func (v *validatorObject) DeepCopyObject() runtime.Object {
	cp := *v
	cp.Object = *v.DeepCopy()
	return &cp
}

func TestValidate_CallsCustomValidator(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	obj := &validatorObject{
		Object: testv1.Object{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-obj",
				Namespace: "default",
			},
		},
	}

	// No error case - should return no errors.
	errs := strategy.Validate(ctx, obj)
	if len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}

	// Error case - should return error in ErrorList.
	obj.validateCreateErr = fmt.Errorf("custom validation failed")
	errs = strategy.Validate(ctx, obj)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if errs[0].Detail != "custom validation failed" {
		t.Errorf("expected error detail %q, got %q", "custom validation failed", errs[0].Detail)
	}
}

func TestValidateUpdate_CallsCustomValidator(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
	}

	obj := &validatorObject{
		Object: testv1.Object{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-obj",
				Namespace: "default",
			},
		},
	}

	// No error case.
	errs := strategy.ValidateUpdate(ctx, obj, oldObj)
	if len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}

	// Error case.
	obj.validateUpdateErr = fmt.Errorf("update validation failed")
	errs = strategy.ValidateUpdate(ctx, obj, oldObj)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if errs[0].Detail != "update validation failed" {
		t.Errorf("expected error detail %q, got %q", "update validation failed", errs[0].Detail)
	}
}

func TestGenerateName(t *testing.T) {
	strategy := newTestStrategy(t, true)

	name := strategy.GenerateName("foo-")
	if len(name) < len("foo-")+5 {
		t.Errorf("generated name too short: %q", name)
	}
	if name[:4] != "foo-" {
		t.Errorf("expected prefix %q, got %q", "foo-", name[:4])
	}

	// Two calls should produce different names.
	name2 := strategy.GenerateName("foo-")
	if name == name2 {
		t.Errorf("expected distinct names, got %q twice", name)
	}
}

func TestPrepareForCreate_SetsCreatedByFromUserInfo(t *testing.T) {
	strategy := newTestStrategy(t, true)

	obj := &testv1.Object{
		TypeMeta: metav1.TypeMeta{
			APIVersion: testv1.GroupVersion.String(),
			Kind:       "Object",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
	}

	ctx := request.WithUser(context.Background(), &user.DefaultInfo{
		Name: "admin@example.com",
	})

	strategy.PrepareForCreate(ctx, obj)

	got := obj.GetAnnotations()[constants.AnnotationCreatedBy]
	if got != "admin@example.com" {
		t.Errorf("created-by annotation = %q, want %q", got, "admin@example.com")
	}
}

func TestPrepareForCreate_NoUserInfo_NoCreatedByAnnotation(t *testing.T) {
	strategy := newTestStrategy(t, true)

	obj := &testv1.Object{
		TypeMeta: metav1.TypeMeta{
			APIVersion: testv1.GroupVersion.String(),
			Kind:       "Object",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
	}

	strategy.PrepareForCreate(context.Background(), obj)

	annotations := obj.GetAnnotations()
	if annotations != nil {
		if _, ok := annotations[constants.AnnotationCreatedBy]; ok {
			t.Error("expected no created-by annotation when no user info in context")
		}
	}
}

func TestPrepareForUpdate_PreservesCreatedByAnnotation(t *testing.T) {
	strategy := newTestStrategy(t, true)

	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-obj",
			Namespace:         "default",
			UID:               "old-uid",
			Generation:        1,
			CreationTimestamp: metav1.Time{Time: metav1.Now().Add(-1 * 60 * 1e9)},
			Annotations: map[string]string{
				constants.AnnotationCreatedBy: "original@example.com",
			},
		},
		Spec: testv1.ObjectSpec{PublicField: "value"},
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
			// No annotations set — PrepareForUpdate must copy created-by from old.
		},
		Spec: testv1.ObjectSpec{PublicField: "value"},
	}

	strategy.PrepareForUpdate(context.Background(), newObj, oldObj)

	got := newObj.GetAnnotations()[constants.AnnotationCreatedBy]
	if got != "original@example.com" {
		t.Errorf("created-by annotation = %q, want %q", got, "original@example.com")
	}
}

// newObjectSchemaProcessor builds a real (non-nil) schema.Processor from
// testv1.Object's generated OpenAPI schema, mirroring how ResourceStrategy
// is actually constructed in production (apiserver.go's createProcessor).
// Existing specChanged tests all pass a nil processor, which skips
// applyProcessing entirely and never exercises pruning/defaulting.
func newObjectSchemaProcessor(t *testing.T) *pkgschema.Processor {
	t.Helper()

	var propsV1 apiextv1.JSONSchemaProps
	if err := yaml.Unmarshal([]byte(testv1.ObjectSchemaYAML), &propsV1); err != nil {
		t.Fatalf("failed to unmarshal schema YAML: %v", err)
	}

	var props apiext.JSONSchemaProps
	if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&propsV1, &props, nil); err != nil {
		t.Fatalf("failed to convert schema: %v", err)
	}

	structural, err := extschema.NewStructural(&props)
	if err != nil {
		t.Fatalf("failed to create structural schema: %v", err)
	}

	processor, err := pkgschema.NewProcessor(structural, &props)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	return processor
}

// TestPrepareForUpdate_SpecUnchanged_TypeMismatch reproduces the
// representation mismatch that the postgres (and spanner) storage backends
// hit in production: Get() returns an *unstructured.Unstructured (a
// map[string]interface{}, which encoding/json always marshals with
// alphabetically-sorted keys), while the incoming update is a typed struct
// (marshaled in Go struct-declaration order). ObjectSpec's declared order is
// publicField, internalField, nested, defaultField — not alphabetical — so a
// raw byte comparison of old vs. new would differ even for identical
// content. specChanged must compare semantically, not byte-for-byte, so this
// must not bump generation.
func TestPrepareForUpdate_SpecUnchanged_TypeMismatch(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	oldObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": testv1.GroupVersion.String(),
			"kind":       "Object",
			"metadata": map[string]interface{}{
				"name":       "test-obj",
				"namespace":  "default",
				"uid":        "old-uid",
				"generation": int64(3),
			},
			"spec": map[string]interface{}{
				"publicField":   "value",
				"internalField": "internal",
				"nested": map[string]interface{}{
					"publicField":   "n-value",
					"internalField": "n-internal",
				},
				"defaultField": "default-value",
			},
		},
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
		Spec: testv1.ObjectSpec{
			PublicField:   "value",
			InternalField: "internal",
			Nested: testv1.ObjectNested{
				PublicField:   "n-value",
				InternalField: "n-internal",
			},
			DefaultField: "default-value",
		},
	}

	strategy.PrepareForUpdate(ctx, newObj, oldObj)

	if newObj.Generation != 3 {
		t.Errorf("expected Generation to remain 3 (spec semantically unchanged despite unstructured-vs-typed representation mismatch), got %d", newObj.Generation)
	}
}

// TestPrepareForUpdate_SymmetricProcessing_DefaultingDoesNotCauseChurn uses a
// real (non-nil) schema processor, unlike the other PrepareForUpdate tests in
// this file. old and new both omit defaultField, relying on the schema's
// default. applyProcessing is only invoked on new inside PrepareForUpdate; if
// old isn't processed the same way before comparison, new ends up with
// defaultField populated while old does not, making an otherwise-unchanged
// apply look like a permanent spec change on every single reapply.
func TestPrepareForUpdate_SymmetricProcessing_DefaultingDoesNotCauseChurn(t *testing.T) {
	processor := newObjectSchemaProcessor(t)
	strategy := NewResourceStrategy(newTestScheme(t), processor, true, testGVK, logr.Discard())
	ctx := context.Background()

	spec := testv1.ObjectSpec{
		PublicField:   "value",
		InternalField: "internal",
		Nested: testv1.ObjectNested{
			PublicField:   "n-value",
			InternalField: "n-internal",
		},
		// DefaultField intentionally omitted on both sides.
	}

	oldObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-obj",
			Namespace:  "default",
			Generation: 7,
		},
		Spec: spec,
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
		Spec: spec,
	}

	strategy.PrepareForUpdate(ctx, newObj, oldObj)

	if newObj.Spec.DefaultField != "default-value" {
		t.Fatalf("expected DefaultField to be defaulted to %q, got %q", "default-value", newObj.Spec.DefaultField)
	}
	if newObj.Generation != 7 {
		t.Errorf("expected Generation to remain 7 (defaulting applied symmetrically to old and new), got %d", newObj.Generation)
	}
}

// typedOldValidatorObject mirrors the real private API validators, which
// type-assert the old object to their own concrete type before using it.
type typedOldValidatorObject struct {
	testv1.Object
	gotOld runtime.Object
}

func (v *typedOldValidatorObject) ValidateCreate(_ context.Context) error { return nil }

func (v *typedOldValidatorObject) ValidateUpdate(_ context.Context, oldObj runtime.Object) error {
	v.gotOld = oldObj
	if _, ok := oldObj.(*testv1.Object); !ok {
		return fmt.Errorf("expected old object to be *testv1.Object, got %T", oldObj)
	}
	return nil
}

func (v *typedOldValidatorObject) ValidateDelete(_ context.Context) error { return nil }

func (v *typedOldValidatorObject) DeepCopyObject() runtime.Object {
	cp := *v
	cp.Object = *v.DeepCopy()
	return &cp
}

// The storage layer always decodes rows into *unstructured.Unstructured, so the
// old object handed to a CustomValidator must be converted back to the typed
// object before the validator runs. Without that conversion every validator
// that asserts its concrete type rejects every update.
func TestValidateUpdate_ConvertsUnstructuredOldObjectToTyped(t *testing.T) {
	strategy := newTestStrategy(t, true)
	ctx := context.Background()

	oldObj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testv1.GroupVersion.String(),
		"kind":       "Object",
		"metadata": map[string]interface{}{
			"name":      "test-obj",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"publicField": "old-value",
		},
	}}

	obj := &typedOldValidatorObject{
		Object: testv1.Object{
			ObjectMeta: metav1.ObjectMeta{Name: "test-obj", Namespace: "default"},
			Spec:       testv1.ObjectSpec{PublicField: "new-value"},
		},
	}

	errs := strategy.ValidateUpdate(ctx, obj, oldObj)
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors, got %v", errs)
	}

	typedOld, ok := obj.gotOld.(*testv1.Object)
	if !ok {
		t.Fatalf("validator received old object of type %T, want *testv1.Object", obj.gotOld)
	}
	if typedOld.Spec.PublicField != "old-value" {
		t.Errorf("converted old object lost spec data: PublicField = %q, want %q",
			typedOld.Spec.PublicField, "old-value")
	}
	if typedOld.Name != "test-obj" {
		t.Errorf("converted old object lost metadata: Name = %q, want %q", typedOld.Name, "test-obj")
	}
}
