package main

import (
	"context"
	"fmt"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/conversion"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/runtime"
)

func newWIFConfigTemplateLookup(factory apiserver.StorageFactory, scheme *runtime.Scheme) (func(context.Context, string) (*privatev1.WIFConfigTemplate, error), error) {
	gvk := privatev1.GroupVersion.WithKind("WIFConfigTemplate")
	store, err := factory(apiserver.GroupKindResourceType(gvk.GroupKind()), scheme, gvk)
	if err != nil {
		return nil, fmt.Errorf("create IAM catalog validation store: %w", err)
	}
	if store == nil {
		return nil, fmt.Errorf("create IAM catalog validation store: nil store")
	}
	return func(ctx context.Context, name string) (*privatev1.WIFConfigTemplate, error) {
		obj, err := store.Get(ctx, "", name)
		if err != nil {
			return nil, err
		}
		typed, err := conversion.TypedOldObject(scheme, gvk, obj)
		if err != nil {
			return nil, err
		}
		template, ok := typed.(*privatev1.WIFConfigTemplate)
		if !ok {
			return nil, fmt.Errorf("unexpected IAM catalog object type")
		}
		return template, nil
	}, nil
}
