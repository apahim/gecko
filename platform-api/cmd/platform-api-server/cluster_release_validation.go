package main

import (
	"context"
	"fmt"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/conversion"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
)

//nolint:kubeapilinter // Runtime storage dependencies are not serialized API fields.
type clusterReleaseValidator struct {
	versions storage.ResourceStore
	scheme   *runtime.Scheme
}

func newClusterReleaseValidator(factory apiserver.StorageFactory, scheme *runtime.Scheme) (*clusterReleaseValidator, error) {
	gvk := privatev1.GroupVersion.WithKind("Version")
	store, err := factory(apiserver.GroupKindResourceType(gvk.GroupKind()), scheme, gvk)
	if err != nil {
		return nil, fmt.Errorf("create Version validation store: %w", err)
	}
	if store == nil {
		return nil, fmt.Errorf("create Version validation store: nil store")
	}
	return &clusterReleaseValidator{versions: store, scheme: scheme}, nil
}

// Validate rejects release requests the Version catalog definitively does not
// offer. A minor stream (for example "4.23") is accepted when any concrete
// version in that stream is published in the channel group.
func (v *clusterReleaseValidator) Validate(ctx context.Context, version, channelGroup string) error {
	if privatev1.IsMinorRelease(version) {
		return v.validateStream(ctx, version, channelGroup)
	}
	release, err := v.release(ctx, version)
	if err != nil {
		return err
	}
	return validateReleaseChannelGroups(version, channelGroup, release.Spec.ChannelGroups)
}

func (v *clusterReleaseValidator) validateStream(ctx context.Context, stream, channelGroup string) error {
	obj, err := v.versions.List(ctx, storage.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Version catalog for release validation: %w", err)
	}
	list, ok := obj.(*privatev1.VersionList)
	if !ok {
		return fmt.Errorf("the stored Version list has an unexpected type")
	}
	for i := range list.Items {
		release := &list.Items[i]
		if privatev1.ReleaseMatches(stream, release.Name) && containsReleaseChannelGroup(release.Spec.ChannelGroups, channelGroup) {
			return nil
		}
	}
	return fmt.Errorf("no version in stream %s is published in channel group %s", stream, channelGroup)
}

func (v *clusterReleaseValidator) release(ctx context.Context, version string) (*privatev1.Version, error) {
	obj, err := v.versions.Get(ctx, "", version)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("version %s is not present in the Version catalog", version)
	}
	if err != nil {
		return nil, fmt.Errorf("read version %s for release validation: %w", version, err)
	}
	typed, err := conversion.TypedOldObject(v.scheme, privatev1.GroupVersion.WithKind("Version"), obj)
	if err != nil {
		return nil, fmt.Errorf("decode version %s for release validation: %w", version, err)
	}
	release, ok := typed.(*privatev1.Version)
	if !ok {
		return nil, fmt.Errorf("the stored Version has an unexpected type")
	}
	return release, nil
}

func containsReleaseChannelGroup(groups []string, channelGroup string) bool {
	for _, candidate := range groups {
		if candidate == channelGroup {
			return true
		}
	}
	return false
}

func validateReleaseChannelGroups(version, channelGroup string, groups []string) error {
	if containsReleaseChannelGroup(groups, channelGroup) {
		return nil
	}
	return fmt.Errorf("version %s is not published in channel group %s", version, channelGroup)
}
