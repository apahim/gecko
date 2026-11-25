package v1

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
type Object struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +orlop:public
	Spec ObjectSpec `json:"spec,omitempty"`
	// +orlop:public
	Status ObjectStatus `json:"status,omitempty"`
}

// ValidateCreate rejects spec.publicField values prefixed with invalid-create
// so handler tests can exercise create-path custom validation.
func (o *Object) ValidateCreate(_ context.Context) error {
	if strings.HasPrefix(o.Spec.PublicField, "invalid-create") {
		return fmt.Errorf("spec.publicField is rejected by create validation")
	}
	return nil
}

// ValidateUpdate rejects spec.publicField values prefixed with invalid-update
// so handler tests can exercise update-path custom validation.
func (o *Object) ValidateUpdate(_ context.Context, oldObj runtime.Object) error {
	old, ok := oldObj.(*Object)
	if !ok {
		return fmt.Errorf("expected an Object old object")
	}
	if strings.HasPrefix(old.Spec.PublicField, "invalid-update") || strings.HasPrefix(o.Spec.PublicField, "invalid-update") {
		return fmt.Errorf("spec.publicField is rejected by update validation")
	}
	return nil
}

// ValidateDelete permits deletion; it completes the CustomValidator interface.
func (o *Object) ValidateDelete(_ context.Context) error {
	return nil
}

// +kubebuilder:object:root=true
type ObjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	// +orlop:public
	Items []Object `json:"items"`
}

type ObjectSpec struct {
	// +orlop:public
	PublicField   string `json:"publicField"`
	InternalField string `json:"internalField"`
	// +orlop:public
	Nested ObjectNested `json:"nested"`
	// +orlop:public
	// +kubebuilder:default="default-value"
	DefaultField string `json:"defaultField,omitempty"`
}

type ObjectNested struct {
	// +orlop:public
	PublicField   string `json:"publicField"`
	InternalField string `json:"internalField"`
}

type ObjectStatus struct {
	// +orlop:public
	Conditions []string `json:"conditions,omitempty"`
}

func init() { register(&Object{}, &ObjectList{}) }
