package v1_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/conversion"
	pkgschema "github.com/openshift-online/gecko/orlop/pkg/apiserver/schema"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
	publicv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/yaml"
)

func TestChannelWithoutStatus(t *testing.T) {
	object := channelAsMap(t, channelForSchemaTest())
	delete(object, "status")
	for _, schema := range []struct{ name, value string }{
		{"private", privatev1.ChannelSchemaYAML},
		{"public", publicv1.ChannelSchemaYAML},
	} {
		t.Run(schema.name, func(t *testing.T) {
			if errs := channelSchemaProcessor(t, schema.value).Process(context.Background(), object); len(errs) != 0 {
				t.Fatalf("Channel without status failed validation: %v", errs)
			}
		})
	}
}

func TestChannelDefaultAvailabilityIsPrivate(t *testing.T) {
	privateScheme, publicScheme := runtime.NewScheme(), runtime.NewScheme()
	if err := privatev1.AddToScheme(privateScheme); err != nil {
		t.Fatal(err)
	}
	if err := publicv1.AddToScheme(publicScheme); err != nil {
		t.Fatal(err)
	}
	converter := conversion.NewConverter(publicScheme, privateScheme, "")
	for _, status := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown} {
		t.Run(string(status), func(t *testing.T) {
			channel := channelForSchemaTest()
			channel.Status.Conditions = []metav1.Condition{{
				Type:               privatev1.ChannelDefaultVersionAvailable,
				Status:             status,
				ObservedGeneration: 3,
				LastTransitionTime: metav1.NewTime(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)),
				Reason:             "CatalogEvaluated",
				Message:            "Availability of the pinned install default in the synchronized catalog.",
			}}
			// The API's runtime converter applies the condition allowlist after
			// field filtering. Generated type conversion alone does not do this.
			converted, err := converter.PrivateToPublic(&channel)
			if err != nil {
				t.Fatal(err)
			}
			public := converted.(*publicv1.Channel)
			if len(public.Status.Conditions) != 0 {
				t.Fatalf("public Channel exposes private conditions: %#v", public.Status.Conditions)
			}
			if len(channel.Status.Conditions) != 1 {
				t.Fatal("public conversion removed the private condition from the source")
			}
			object := channelAsMap(t, public)
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
			roundTrip, err := converter.PublicToPrivate(public, &channel)
			if err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(channel.Status, roundTrip.(*privatev1.Channel).Status) {
				t.Fatal("public round trip did not preserve the existing private condition")
			}
			// A public client cannot inject or overwrite the private condition.
			public.Status.Conditions = []metav1.Condition{{
				Type:   privatev1.ChannelDefaultVersionAvailable,
				Status: metav1.ConditionFalse,
				Reason: "ClientInjected",
			}}
			forged := public.DeepCopy()
			updated, err := converter.PublicToPrivate(public, &channel)
			if err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(channel.Status, updated.(*privatev1.Channel).Status) {
				t.Fatal("public input overwrote the existing private condition")
			}
			created, err := converter.PublicToPrivate(forged, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(created.(*privatev1.Channel).Status.Conditions) != 0 {
				t.Fatal("public input injected a private condition")
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
