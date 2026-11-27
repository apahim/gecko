package versionsync

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/yaml"
)

func testTemplate(name, uid string, verified bool) privatev1.WIFConfigTemplate {
	return privatev1.WIFConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec:       privatev1.WIFConfigTemplateSpec{Verified: &verified},
	}
}

// validTemplate returns a boundary whose contract passes current executable
// validation, loading the reviewed chart baseline. Tests that exercise
// pinning must use it: retained-but-invalid boundaries are deliberately
// unpinnable, and an empty contract fails validation.
func validTemplate(t *testing.T, name, uid string, verified bool) privatev1.WIFConfigTemplate {
	t.Helper()
	data, err := os.ReadFile("../../helm/charts/platform-api-server/files/wifconfigtemplate-project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	template := privatev1.WIFConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec:       privatev1.WIFConfigTemplateSpec{Verified: &verified},
	}
	if err := yaml.UnmarshalStrict(data, &template.Spec); err != nil {
		t.Fatal(err)
	}
	template.Spec.Verified = &verified
	return template
}

func TestSelectTemplateBoundary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		version     string
		templates   []privatev1.WIFConfigTemplate
		want        string
		wantNil     bool
		description string
	}{
		{
			name:        "exact boundary",
			version:     "5.0.0",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "uid-a", true)},
			want:        "5.0.0",
			description: "an exact numeric match is selected",
		},
		{
			name:        "greatest floor below release",
			version:     "5.0.7",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "uid-a", true), testTemplate("5.0.3", "uid-b", true)},
			want:        "5.0.3",
			description: "the greatest boundary at or below the release is selected",
		},
		{
			name:        "numeric ordering not lexical",
			version:     "5.0.12",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.6", "uid-a", true), testTemplate("5.0.10", "uid-b", true)},
			want:        "5.0.10",
			description: "5.0.10 is a higher floor than 5.0.6 despite lexical order",
		},
		{
			name:        "boundary above release excluded",
			version:     "5.0.2",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.3", "uid-a", true)},
			wantNil:     true,
			description: "a boundary above the release never applies",
		},
		{
			name:        "no cross-minor fallback",
			version:     "5.1.0",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.9", "uid-a", true)},
			wantNil:     true,
			description: "selection never falls back to another minor",
		},
		{
			name:        "no cross-major fallback",
			version:     "6.0.0",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.9", "uid-a", true)},
			wantNil:     true,
			description: "selection never falls back to another major",
		},
		{
			name:        "prerelease inherits numeric base",
			version:     "5.0.6-rc.1",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "uid-a", true), testTemplate("5.0.6", "uid-b", true)},
			want:        "5.0.6",
			description: "a prerelease inherits its numeric base's contract",
		},
		{
			name:        "nightly inherits numeric base",
			version:     "5.0.0-0.nightly-2026-10-04-051631",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "uid-a", true)},
			want:        "5.0.0",
			description: "a nightly prerelease inherits its numeric base's contract",
		},
		{
			name:        "unverified highest match included",
			version:     "5.0.7",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "uid-a", true), testTemplate("5.0.3", "uid-b", false)},
			want:        "5.0.3",
			description: "the unverified highest match is selected, never falling back to a lower verified boundary",
		},
		{
			name:        "malformed template names skipped",
			version:     "5.0.1",
			templates:   []privatev1.WIFConfigTemplate{testTemplate("five", "uid-a", true), testTemplate("5.0.x", "uid-b", true), testTemplate("05.0.0", "uid-c", true), testTemplate("5.0.0", "uid-d", true)},
			want:        "5.0.0",
			description: "non-canonical boundary names do not participate in selection",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := selectTemplateBoundary(tc.version, tc.templates)
			if tc.wantNil {
				assert.Nil(t, selected, tc.description)
				return
			}
			require.NotNil(t, selected, tc.description)
			assert.Equal(t, tc.want, selected.Name, tc.description)
		})
	}
}

func TestSelectTemplateBoundaryRejectsMalformedVersionNames(t *testing.T) {
	templates := []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "uid-a", true)}
	for _, version := range []string{"v5.0.0", "5.0", "5.0.0.0", "5.x.0", "05.0.0", "5.0.00", "five"} {
		t.Run(version, func(t *testing.T) {
			assert.Nil(t, selectTemplateBoundary(version, templates), "non-canonical version name must not select a boundary")
		})
	}
}

// Non-canonical boundary names never apply to any Version; they are surfaced
// once per pass so a typo'd name is visible to operators.
func TestNonCanonicalTemplateNames(t *testing.T) {
	templates := []privatev1.WIFConfigTemplate{
		testTemplate("5.0.0", "uid-a", true),
		testTemplate("5.0.x", "uid-b", true),
		testTemplate("5.0.3", "uid-c", true),
		testTemplate("v5.0.4", "uid-d", true),
		testTemplate("5.0", "uid-e", true),
	}
	assert.Equal(t, []string{"5.0.x", "v5.0.4", "5.0"}, nonCanonicalTemplateNames(templates))
	assert.Nil(t, nonCanonicalTemplateNames(nil))
}

// Selection must be deterministic regardless of catalog input order.
func TestSelectTemplateBoundaryShuffledInput(t *testing.T) {
	templates := []privatev1.WIFConfigTemplate{
		testTemplate("5.0.3", "uid-c", true),
		testTemplate("5.0.10", "uid-d", true),
		testTemplate("5.0.0", "uid-a", true),
		testTemplate("5.0.6", "uid-b", true),
	}
	rotations := [][]privatev1.WIFConfigTemplate{
		{templates[3], templates[0], templates[2], templates[1]},
		{templates[2], templates[1], templates[3], templates[0]},
		{templates[1], templates[3], templates[0], templates[2]},
	}
	for i, shuffled := range rotations {
		selected := selectTemplateBoundary("5.0.12", shuffled)
		require.NotNil(t, selected, "rotation %d", i)
		assert.Equal(t, "5.0.10", selected.Name, "rotation %d", i)
	}
}

func TestResolvePin(t *testing.T) {
	t.Run("builds reference from live catalog state", func(t *testing.T) {
		template := validTemplate(t, "5.0.0", "template-uid", true)
		pin, err := resolvePin("5.0.6", []privatev1.WIFConfigTemplate{template})
		require.NoError(t, err)
		require.NotNil(t, pin)
		digest, err := privatev1.WIFConfigDigest(template.Spec)
		require.NoError(t, err)
		assert.Equal(t, &privatev1.WIFConfigTemplateReference{Name: "5.0.0", UID: "template-uid", Digest: digest}, pin)
	})
	t.Run("no applicable boundary", func(t *testing.T) {
		pin, err := resolvePin("4.23.1", []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "uid-a", true)})
		require.NoError(t, err)
		assert.Nil(t, pin)
	})
	t.Run("malformed version name", func(t *testing.T) {
		pin, err := resolvePin("not-a-version", []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "uid-a", true)})
		require.NoError(t, err)
		assert.Nil(t, pin)
	})
	t.Run("unpinnable highest boundary yields no pin and no fallback", func(t *testing.T) {
		// A retained boundary failing current contract validation: selection
		// still prefers it as the highest applicable boundary, but it can
		// never be admitted as a pin.
		invalid := testTemplate("5.0.3", "uid-high", true)
		valid := validTemplate(t, "5.0.0", "uid-low", true)
		templates := []privatev1.WIFConfigTemplate{invalid, valid}

		selected := selectTemplateBoundary("5.0.6", templates)
		require.NotNil(t, selected)
		assert.Equal(t, "5.0.3", selected.Name, "selection must not fall back below an invalid highest boundary")

		pin, err := resolvePin("5.0.6", templates)
		require.NoError(t, err)
		assert.Nil(t, pin, "an unpinnable boundary must not produce a pin")
	})
}
