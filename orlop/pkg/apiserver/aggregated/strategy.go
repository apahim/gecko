package aggregated

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/google/uuid"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"
	pkgschema "github.com/openshift-online/gecko/orlop/pkg/apiserver/schema"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	_ rest.RESTCreateStrategy = (*ResourceStrategy)(nil)
	_ rest.RESTUpdateStrategy = (*ResourceStrategy)(nil)
)

type ResourceStrategy struct {
	scheme     *runtime.Scheme
	processor  *pkgschema.Processor
	namespaced bool
	gvk        runtimeschema.GroupVersionKind
	logger     logr.Logger
}

func NewResourceStrategy(scheme *runtime.Scheme, processor *pkgschema.Processor, namespaced bool, gvk runtimeschema.GroupVersionKind, logger logr.Logger) *ResourceStrategy {
	return &ResourceStrategy{
		scheme:     scheme,
		processor:  processor,
		namespaced: namespaced,
		gvk:        gvk,
		logger:     logger,
	}
}

func (s *ResourceStrategy) ObjectKinds(obj runtime.Object) ([]runtimeschema.GroupVersionKind, bool, error) {
	return s.scheme.ObjectKinds(obj)
}
func (s *ResourceStrategy) Recognizes(gvk runtimeschema.GroupVersionKind) bool {
	return s.scheme.Recognizes(gvk)
}
func (s *ResourceStrategy) GenerateName(base string) string {
	return storage.GenerateName(base)
}
func (s *ResourceStrategy) NamespaceScoped() bool {
	return s.namespaced
}
func (s *ResourceStrategy) Canonicalize(obj runtime.Object)                 {}
func (s *ResourceStrategy) AllowCreateOnUpdate(_ context.Context) bool      { return false }
func (s *ResourceStrategy) AllowUnconditionalUpdate(_ context.Context) bool { return false }

func (s *ResourceStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	if s.processor != nil {
		if err := s.applyProcessing(ctx, obj); err != nil {
			s.logger.Error(err, "failed to apply schema processing during create")
			return
		}
	}

	clientObj, ok := obj.(client.Object)
	if !ok {
		s.logger.Error(fmt.Errorf("object does not implement client.Object"), "cannot prepare object for create")
		return
	}

	clientObj.SetUID(k8stypes.UID(uuid.New().String()))
	clientObj.SetCreationTimestamp(metav1.Now())
	clientObj.SetGeneration(1)

	// Set the created-by annotation from the authenticated user identity.
	if userInfo, ok := request.UserFrom(ctx); ok {
		if email := userInfo.GetName(); email != "" {
			annotations := clientObj.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations[constants.AnnotationCreatedBy] = email
			clientObj.SetAnnotations(annotations)
		}
	}

	if defaulter, ok := obj.(types.CustomDefaulter); ok {
		if err := defaulter.Default(ctx); err != nil {
			s.logger.Error(err, "custom defaulter failed during create")
		}
	}
}

func (s *ResourceStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	var allErrs field.ErrorList

	if s.processor != nil {
		objMap, err := toMap(obj)
		if err != nil {
			return field.ErrorList{field.InternalError(field.NewPath(""), err)}
		}
		allErrs = append(allErrs, s.processor.Process(ctx, objMap)...)
	}

	allErrs = append(allErrs, validateOwnerReferences(obj)...)

	if validator, ok := obj.(types.CustomValidator); ok {
		if err := validator.ValidateCreate(ctx); err != nil {
			allErrs = append(allErrs, field.InternalError(field.NewPath(""), err))
		}
	}

	return allErrs
}

func (s *ResourceStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

func (s *ResourceStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newObj, ok := obj.(client.Object)
	if !ok {
		s.logger.Error(fmt.Errorf("new object does not implement client.Object"), "cannot prepare object for update")
		return
	}
	oldObj, ok := old.(client.Object)
	if !ok {
		s.logger.Error(fmt.Errorf("old object does not implement client.Object"), "cannot prepare object for update")
		return
	}

	newObj.SetCreationTimestamp(oldObj.GetCreationTimestamp())
	newObj.SetUID(oldObj.GetUID())

	// Preserve the created-by annotation — it is immutable after creation.
	if createdBy := oldObj.GetAnnotations()[constants.AnnotationCreatedBy]; createdBy != "" {
		annotations := newObj.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[constants.AnnotationCreatedBy] = createdBy
		newObj.SetAnnotations(annotations)
	}

	// Preserve deletionTimestamp — it cannot be changed via Update.
	if deletionTimestamp := oldObj.GetDeletionTimestamp(); deletionTimestamp != nil {
		newObj.SetDeletionTimestamp(deletionTimestamp)
	}

	if s.processor != nil {
		if err := s.applyProcessing(ctx, obj); err != nil {
			s.logger.Error(err, "failed to apply schema processing during update")
			return
		}
	}

	if defaulter, ok := obj.(types.CustomDefaulter); ok {
		if err := defaulter.Default(ctx); err != nil {
			s.logger.Error(err, "custom defaulter failed during update")
		}
	}

	if specChanged(old, obj) {
		newObj.SetGeneration(oldObj.GetGeneration() + 1)
	} else {
		newObj.SetGeneration(oldObj.GetGeneration())
	}
}

func (s *ResourceStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	var allErrs field.ErrorList

	if s.processor != nil {
		objMap, err := toMap(obj)
		if err != nil {
			return field.ErrorList{field.InternalError(field.NewPath(""), err)}
		}
		allErrs = append(allErrs, s.processor.Process(ctx, objMap)...)
	}

	allErrs = append(allErrs, validateOwnerReferences(obj)...)

	if validator, ok := obj.(types.CustomValidator); ok {
		if err := validator.ValidateUpdate(ctx, old); err != nil {
			allErrs = append(allErrs, field.InternalError(field.NewPath(""), err))
		}
	}

	return allErrs
}

func (s *ResourceStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

func (s *ResourceStrategy) applyProcessing(ctx context.Context, obj runtime.Object) error {
	objMap, err := toMap(obj)
	if err != nil {
		return fmt.Errorf("converting object to map: %w", err)
	}
	// Process mutates objMap in place (pruning unknown fields, applying defaults).
	// Validation errors are handled separately by Validate/ValidateUpdate.
	s.processor.Process(ctx, objMap)
	data, err := json.Marshal(objMap)
	if err != nil {
		return fmt.Errorf("marshaling processed object: %w", err)
	}
	return json.Unmarshal(data, obj)
}

func toMap(obj runtime.Object) (map[string]interface{}, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	return m, err
}

// validateOwnerReferences validates that owner references have required fields
// and that no cross-namespace owner references are present.
func validateOwnerReferences(obj runtime.Object) field.ErrorList {
	clientObj, ok := obj.(client.Object)
	if !ok {
		return nil
	}
	ownerRefs := clientObj.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		return nil
	}

	var allErrs field.ErrorList
	for i, ref := range ownerRefs {
		fldPath := field.NewPath("metadata", "ownerReferences").Index(i)
		if ref.Name == "" {
			allErrs = append(allErrs, field.Required(fldPath.Child("name"), "owner reference name is required"))
		}
		if ref.Kind == "" {
			allErrs = append(allErrs, field.Required(fldPath.Child("kind"), "owner reference kind is required"))
		}
		if ref.APIVersion == "" {
			allErrs = append(allErrs, field.Required(fldPath.Child("apiVersion"), "owner reference apiVersion is required"))
		}
	}

	// Cross-namespace check: the standard metav1.OwnerReference struct has no
	// Namespace field, but raw JSON may carry one via unknown-field preservation.
	// Convert to map and inspect, consistent with the standalone handler's
	// validateOwnerReferencesFromMap.
	allErrs = append(allErrs, validateOwnerReferencesNamespace(clientObj)...)

	return allErrs
}

// validateOwnerReferencesNamespace rejects ownerReferences that contain a
// namespace field pointing to a different namespace than the object.
func validateOwnerReferencesNamespace(obj client.Object) field.ErrorList {
	namespace := obj.GetNamespace()
	if namespace == "" {
		return nil
	}

	objMap, err := toMap(obj)
	if err != nil {
		return nil
	}
	metadata, ok := objMap["metadata"].(map[string]interface{})
	if !ok {
		return nil
	}
	ownerRefsRaw, ok := metadata["ownerReferences"]
	if !ok {
		return nil
	}
	ownerRefs, ok := ownerRefsRaw.([]interface{})
	if !ok {
		return nil
	}

	var allErrs field.ErrorList
	for i, refRaw := range ownerRefs {
		ref, ok := refRaw.(map[string]interface{})
		if !ok {
			continue
		}
		ownerNS, ok := ref["namespace"]
		if !ok {
			continue
		}
		nsStr, _ := ownerNS.(string)
		if nsStr != "" && nsStr != namespace {
			fldPath := field.NewPath("metadata", "ownerReferences").Index(i).Child("namespace")
			allErrs = append(allErrs, field.Invalid(fldPath, nsStr,
				fmt.Sprintf("cross-namespace owner references are not allowed, owner is in namespace %q but object is in namespace %q", nsStr, namespace)))
		}
	}
	return allErrs
}

// specChanged checks if the spec field has changed between two objects.
// All gecko resource types must serialize their spec under the JSON key "spec".
func specChanged(old, new runtime.Object) bool {
	oldData, err := json.Marshal(old)
	if err != nil {
		return true
	}
	newData, err := json.Marshal(new)
	if err != nil {
		return true
	}
	var oldRaw, newRaw map[string]json.RawMessage
	if err := json.Unmarshal(oldData, &oldRaw); err != nil {
		return true
	}
	if err := json.Unmarshal(newData, &newRaw); err != nil {
		return true
	}
	return !bytes.Equal(oldRaw["spec"], newRaw["spec"])
}
