package v1_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pkgschema "github.com/openshift-online/gecko/orlop/pkg/apiserver/schema"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
	publicv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/yaml"
)

func TestChannelReleaseStreamSchema(t *testing.T) {
	for _, tc := range []struct {
		name        string
		streams     []string
		wantInvalid bool
	}{
		{name: "existing production channel"},
		{name: "explicit CI stream", streams: []string{"4.22.0-0.nightly"}},
		{name: "multiple explicit CI streams", streams: []string{"4.22.0-0.nightly", "4.23.0-0.nightly"}},
		{name: "empty stream name", streams: []string{""}, wantInvalid: true},
		{name: "duplicate streams", streams: []string{"4.22.0-0.nightly", "4.22.0-0.nightly"}, wantInvalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := channelForSchemaTest()
			channel.Spec.ReleaseStreams = tc.streams
			errs := channelSchemaProcessor(t, privatev1.ChannelSchemaYAML).Process(context.Background(), channelAsMap(t, channel))
			if tc.wantInvalid {
				if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "releaseStreams") {
					t.Fatalf("expected releaseStreams validation error, got %v", errs)
				}
			} else if len(errs) != 0 {
				t.Fatalf("expected valid Channel, got %v", errs)
			}
		})
	}
}

func TestChannelStatusPublicConversion(t *testing.T) {
	for _, status := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown} {
		t.Run(string(status), func(t *testing.T) {
			channel := channelForSchemaTest()
			channel.Spec.ReleaseStreams = []string{"4.22.0-0.nightly"}
			channel.Status.Conditions = []metav1.Condition{{
				Type:               privatev1.ChannelDefaultVersionAvailable,
				Status:             status,
				ObservedGeneration: 3,
				LastTransitionTime: metav1.NewTime(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)),
				Reason:             "CatalogEvaluated",
				Message:            "Availability of the pinned install default in the synchronized catalog.",
			}}
			var public publicv1.Channel
			if err := publicv1.Convert_Channel_PrivateToPublic(&channel, &public, nil); err != nil {
				t.Fatal(err)
			}
			object := channelAsMap(t, public)
			if _, exposed := object["spec"].(map[string]any)["releaseStreams"]; exposed {
				t.Fatal("public Channel exposes private stream configuration")
			}
			for _, schema := range []struct {
				name, value string
				object      map[string]any
			}{
				{"private", privatev1.ChannelSchemaYAML, channelAsMap(t, channel)},
				{"public", publicv1.ChannelSchemaYAML, object},
			} {
				if errs := channelSchemaProcessor(t, schema.value).Process(context.Background(), schema.object); len(errs) != 0 {
					t.Fatalf("%s Channel status failed validation: %v", schema.name, errs)
				}
			}
			var roundTrip privatev1.Channel
			if err := publicv1.Convert_Channel_PublicToPrivate(&public, &roundTrip, nil); err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(channel.Status, roundTrip.Status) {
				t.Fatalf("status changed during public conversion: got %#v, want %#v", roundTrip.Status, channel.Status)
			}
		})
	}
}

func channelForSchemaTest() privatev1.Channel {
	return privatev1.Channel{
		TypeMeta:   metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "Channel"},
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Generation: 3},
		Spec:       privatev1.ChannelSpec{InstallDefaultVersion: "4.22.14", FleetMinorVersion: "4.22"},
	}
}

func channelAsMap(t *testing.T, channel any) map[string]any {
	t.Helper()
	data, err := json.Marshal(channel)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func channelSchemaProcessor(t *testing.T, schemaYAML string) *pkgschema.Processor {
	t.Helper()
	var propsV1 apiextv1.JSONSchemaProps
	if err := yaml.Unmarshal([]byte(schemaYAML), &propsV1); err != nil {
		t.Fatal(err)
	}
	var props apiext.JSONSchemaProps
	if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&propsV1, &props, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&props)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := pkgschema.NewProcessor(structural, &props)
	if err != nil {
		t.Fatal(err)
	}
	return processor
}
