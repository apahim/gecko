package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UpdateStatus handles PUT requests to update only the status subresource.
func (h *ResourceHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, constants.URLParamNamespace)
	name := chi.URLParam(r, constants.URLParamName)
	h.logger.V(1).Info("Update status request", "kind", h.gvk.Kind, "namespace", namespace, "name", name)

	// Parse request body
	var updateMap map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updateMap); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	// Get existing object
	existing, err := h.store.Get(r.Context(), namespace, name)
	if err != nil {
		if errors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get object: %v", err))
		}
		return
	}

	if !validateParentOwnership(r.Context(), existing) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// Check resource version
	updateRV, _ := getResourceVersionFromMap(updateMap)
	existingAccessor, err := meta.Accessor(existing)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to access metadata: %v", err))
		return
	}
	existingRV := existingAccessor.GetResourceVersion()

	if updateRV != "" && updateRV != existingRV {
		writeError(w, http.StatusConflict, fmt.Sprintf("resource version mismatch: expected %s, got %s", existingRV, updateRV))
		return
	}

	// Convert existing object to map to preserve spec
	existingJSON, err := json.Marshal(existing)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to marshal existing object: %v", err))
		return
	}
	var existingMap map[string]interface{}
	if err := json.Unmarshal(existingJSON, &existingMap); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to unmarshal existing object: %v", err))
		return
	}

	// Replace only the status field
	if status, ok := updateMap["status"]; ok {
		existingMap["status"] = status
	}

	// Convert back to typed object in storage version
	updatedJSON, err := json.Marshal(existingMap)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to marshal updated object: %v", err))
		return
	}
	obj, err := h.storageVersionSchemeNew()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create object: %v", err))
		return
	}
	if err := json.Unmarshal(updatedJSON, obj); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to unmarshal object: %v", err))
		return
	}
	clientObj := obj.(client.Object)

	// Set GVK to storage version
	clientObj.GetObjectKind().SetGroupVersionKind(h.effectiveStorageGVK())

	// Update in storage
	if err := h.store.Update(r.Context(), clientObj); err != nil {
		h.logger.Error(err, "Update status failed", "kind", h.gvk.Kind, "namespace", namespace, "name", name)
		if errors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else if errors.IsConflict(err) {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update status: %v", err))
		}
		return
	}

	// Convert to serving version for response
	responseObj, err := h.convertToServingVersion(clientObj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to convert response version: %v", err))
		return
	}

	h.logger.Info("Updated status", "kind", h.gvk.Kind, "namespace", namespace, "name", name)

	// Return updated object
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(responseObj)
}

// getResourceVersionFromMap extracts resource version from metadata map.
func getResourceVersionFromMap(objMap map[string]interface{}) (string, error) {
	metadata, ok := objMap["metadata"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("metadata not found or invalid")
	}

	rv, ok := metadata["resourceVersion"].(string)
	if !ok {
		return "", nil
	}

	return rv, nil
}
