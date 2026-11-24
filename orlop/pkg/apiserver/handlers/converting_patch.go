package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"reflect"

	jsonpatch "github.com/evanphx/json-patch/v5"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"

	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/util/json"
)

// applyPublicPatch operates only on the public projection, never on private
// stored fields. Historical object/merge-patch callers retain their semantics;
// strategic-merge media type remains a legacy merge-patch alias, not SSA.
func applyPublicPatch(contentType string, original, patch []byte) ([]byte, bool, int, error) {
	mediaType := ""
	if contentType != "" {
		var err error
		mediaType, _, err = mime.ParseMediaType(contentType)
		if err != nil {
			// Before JSONPatch support every header was treated as merge patch.
			// Retain that fallback, including malformed legacy headers.
			mediaType = ""
		}
	}
	switch mediaType {
	case constants.ContentTypeJSONPatch:
		trimmed := bytes.TrimSpace(patch)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return nil, true, http.StatusBadRequest, fmt.Errorf("invalid JSON patch: expected operation array")
		}
		operations, err := jsonpatch.DecodePatch(patch)
		if err != nil {
			return nil, true, http.StatusBadRequest, fmt.Errorf("invalid JSON patch")
		}
		options := jsonpatch.NewApplyOptions()
		options.SupportNegativeIndices = false // RFC 6902 does not allow -1.
		// Bound amplification from repeated copies of the resource into itself.
		options.AccumulatedCopySizeLimit = 3 << 20
		patched, err := operations.ApplyWithOptions(original, options)
		if errors.Is(err, jsonpatch.ErrTestFailed) {
			// Do not return dependency error text: it can contain input paths or
			// compared values. This fixed marker is distinct from decode errors.
			return nil, true, http.StatusConflict, fmt.Errorf("JSON patch test failed")
		}
		if err != nil {
			return nil, true, http.StatusBadRequest, fmt.Errorf("JSON patch operation failed")
		}
		return patched, true, http.StatusOK, nil
	default:
		var patchMap map[string]interface{}
		if err := kjson.Unmarshal(patch, &patchMap); err != nil {
			return nil, false, http.StatusBadRequest, fmt.Errorf("invalid merge patch JSON")
		}
		stripStatus(patchMap)
		patchBytes, err := json.Marshal(patchMap)
		if err != nil {
			return nil, false, http.StatusBadRequest, fmt.Errorf("invalid merge patch JSON")
		}
		patched, err := jsonMergePatch(original, patchBytes)
		if err != nil {
			return nil, false, http.StatusBadRequest, fmt.Errorf("merge patch failed")
		}
		return patched, false, http.StatusOK, nil
	}
}

// validatePublicPatchIdentity rejects attempts to remove or overwrite immutable
// metadata, including through whole-document or whole-metadata operations. In
// particular, an operation must not clear RV and thereby disable store CAS.
func validatePublicPatchIdentity(next, previous map[string]interface{}) error {
	for _, field := range []string{"apiVersion", "kind"} {
		if !reflect.DeepEqual(next[field], previous[field]) {
			return fmt.Errorf("JSON patch cannot change server-managed identity")
		}
	}
	nextMeta, _ := next["metadata"].(map[string]interface{})
	previousMeta, _ := previous["metadata"].(map[string]interface{})
	for _, field := range []string{"name", "namespace", "uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "ownerReferences", "finalizers", "managedFields"} {
		if !reflect.DeepEqual(nextMeta[field], previousMeta[field]) {
			return fmt.Errorf("JSON patch cannot change server-managed metadata")
		}
	}
	return nil
}

func (h *ConvertingResourceHandler) preservePublicPatchStatus(next, previous runtime.Object) (runtime.Object, error) {
	nextMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(next)
	if err != nil {
		return nil, err
	}
	previousMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(previous)
	if err != nil {
		return nil, err
	}
	stripStatus(nextMap)
	if status, ok := previousMap["status"]; ok {
		nextMap["status"] = runtime.DeepCopyJSONValue(status)
	}
	// Decode into a fresh object so that fields absent from the saved status
	// cannot survive from the converted input via additive JSON map merging.
	result, err := h.privateScheme.New(h.gvk)
	if err != nil {
		return nil, err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(nextMap, result); err != nil {
		return nil, err
	}
	return result, nil
}
