package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// writeError writes an error response with a Status object.
func writeError(w http.ResponseWriter, code int, message string) {
	status := metav1.Status{
		TypeMeta: metav1.TypeMeta{
			APIVersion: constants.APIVersionV1,
			Kind:       constants.KindStatus,
		},
		Status:  metav1.StatusFailure,
		Message: message,
		Code:    int32(code),
	}

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(status)
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
