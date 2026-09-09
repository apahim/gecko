package handlers

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SetStorageGVK configures the GVK used for storage. When this differs from
// the handler's serving GVK, objects are automatically converted between
// versions on read and write using a JSON round-trip through the scheme.
func (h *ResourceHandler) SetStorageGVK(gvk runtimeschema.GroupVersionKind) {
	h.storageGVK = gvk
}

func (h *ResourceHandler) needsConversion() bool {
	return !h.storageGVK.Empty() && h.storageGVK != h.gvk
}

// convertToServingVersion converts an object from the storage version to the
// handler's serving version. Returns the object unchanged when no conversion
// is needed.
func (h *ResourceHandler) convertToServingVersion(obj client.Object) (client.Object, error) {
	if !h.needsConversion() {
		return obj, nil
	}
	return h.convertObject(obj, h.gvk)
}

// convertToStorageVersion converts an object from the handler's serving
// version to the storage version. Returns the object unchanged when no
// conversion is needed.
func (h *ResourceHandler) convertToStorageVersion(obj client.Object) (client.Object, error) {
	if !h.needsConversion() {
		return obj, nil
	}
	return h.convertObject(obj, h.storageGVK)
}

// convertListToServingVersion converts all items in a list from the storage
// version to the handler's serving version. It returns a new list of the
// target version type with converted items.
func (h *ResourceHandler) convertListToServingVersion(list client.ObjectList) (client.ObjectList, error) {
	if !h.needsConversion() {
		return list, nil
	}

	targetListGVK := h.gvk.GroupVersion().WithKind(h.gvk.Kind + "List")

	data, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal list for version conversion: %w", err)
	}

	targetListObj, err := h.scheme.New(targetListGVK)
	if err != nil {
		return nil, fmt.Errorf("target list version %s not registered in scheme: %w", targetListGVK, err)
	}

	if err := json.Unmarshal(data, targetListObj); err != nil {
		return nil, fmt.Errorf("failed to unmarshal into target list version: %w", err)
	}

	targetList, ok := targetListObj.(client.ObjectList)
	if !ok {
		return nil, fmt.Errorf("converted list does not implement client.ObjectList")
	}

	targetList.GetObjectKind().SetGroupVersionKind(targetListGVK)

	// Fix GVK on individual items
	items, err := meta.ExtractList(targetList)
	if err == nil {
		for _, item := range items {
			if obj, ok := item.(client.Object); ok {
				obj.GetObjectKind().SetGroupVersionKind(h.gvk)
			}
		}
	}

	return targetList, nil
}

func (h *ResourceHandler) convertObject(obj client.Object, targetGVK runtimeschema.GroupVersionKind) (client.Object, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal object for version conversion: %w", err)
	}

	target, err := h.scheme.New(targetGVK)
	if err != nil {
		return nil, fmt.Errorf("target version %s not registered in scheme: %w", targetGVK, err)
	}

	if err := json.Unmarshal(data, target); err != nil {
		return nil, fmt.Errorf("failed to unmarshal into target version: %w", err)
	}

	target.GetObjectKind().SetGroupVersionKind(targetGVK)
	clientObj, ok := target.(client.Object)
	if !ok {
		return nil, fmt.Errorf("converted object does not implement client.Object")
	}

	return clientObj, nil
}

// effectiveStorageGVK returns the GVK that should be set on objects before
// writing to the store. Returns storageGVK when conversion is active,
// otherwise the handler's serving GVK.
func (h *ResourceHandler) effectiveStorageGVK() runtimeschema.GroupVersionKind {
	if h.needsConversion() {
		return h.storageGVK
	}
	return h.gvk
}

// storageVersionSchemeNew creates a new object of the storage version type
// (or the serving version when no conversion is active).
func (h *ResourceHandler) storageVersionSchemeNew() (runtime.Object, error) {
	return h.scheme.New(h.effectiveStorageGVK())
}
