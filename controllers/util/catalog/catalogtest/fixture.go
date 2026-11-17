// Package catalogtest supplies reviewed catalog fixtures for controller tests.
// It is not a runtime permission source and must not be used by controllers.
package catalogtest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilversion "k8s.io/apimachinery/pkg/util/version"

	"sigs.k8s.io/yaml"
)

// Ready attaches valid catalog readiness to an existing release fixture and
// returns the corresponding template for insertion into its API reader/store.
func Ready(t testing.TB, v *privatev1.Version) *privatev1.WIFConfigTemplate {
	t.Helper()
	w, err := ready(v)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// MustReady supports pure fixture constructors that do not take testing.TB.
// A malformed fixture fails the test immediately; this is never runtime fallback.
func MustReady(v *privatev1.Version) *privatev1.WIFConfigTemplate {
	w, err := ready(v)
	if err != nil {
		panic(err)
	}
	return w
}

func ready(v *privatev1.Version) (*privatev1.WIFConfigTemplate, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("locate catalog fixture")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../helm/charts/platform-api-server/files/wifconfigtemplate-project.yaml"))
	if err != nil {
		return nil, err
	}
	parsed, err := utilversion.ParseSemantic(v.Name)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%d.%d.0", parsed.Major(), parsed.Minor())
	w := &privatev1.WIFConfigTemplate{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("test-template-" + name)}}
	if err := yaml.UnmarshalStrict(data, &w.Spec); err != nil {
		return nil, err
	}
	w.Spec.Verified = new(true)
	ref, err := privatev1.WIFConfigReference(w)
	if err != nil {
		return nil, err
	}
	if v.Generation == 0 {
		v.Generation = 1
	}
	v.Spec.WIFConfigTemplateRef = ref
	v.Status.Conditions = []metav1.Condition{{Type: privatev1.VersionAvailable, Status: metav1.ConditionTrue, ObservedGeneration: v.Generation, Reason: "Available", LastTransitionTime: metav1.Now()}}
	return w, nil
}

// Apply records an assertion only inside a test fixture, never against cloud/API.
func Apply(c *privatev1.Cluster, ref *privatev1.WIFConfigTemplateReference) {
	if c.Spec.Platform.GCP == nil {
		c.Spec.Platform.GCP = &privatev1.GCPClusterPlatform{}
	}
	if c.Spec.Platform.GCP.IAM == nil {
		if c.Spec.InfraID == "" {
			c.Spec.InfraID = "fixture-1"
		}
		if c.Spec.IssuerURL == "" {
			c.Spec.IssuerURL = "https://issuer.example/" + c.Spec.InfraID
		}
		if c.Spec.Platform.Type == "" {
			c.Spec.Platform.Type = "GCP"
		}
		g := c.Spec.Platform.GCP
		if g.ProjectID == "" {
			g.ProjectID = "test-project"
		}
		if g.WorkloadIdentity.ProjectNumber == "" {
			g.WorkloadIdentity.ProjectNumber = "123456789"
		}
		g.WorkloadIdentity.PoolID = c.Spec.InfraID + "-wi-pool"
		g.WorkloadIdentity.ProviderID = c.Spec.InfraID + "-k8s-provider"
		email := func(component string) string {
			return c.Spec.InfraID + "-" + component + "@" + g.ProjectID + ".iam.gserviceaccount.com"
		}
		if g.WorkloadIdentity.ServiceAccountsRef == nil {
			g.WorkloadIdentity.ServiceAccountsRef = &privatev1.ServiceAccountsRef{NodePoolEmail: email("nodepool-mgmt"), ControlPlaneEmail: email("ctrlplane-op"), CloudControllerEmail: email("cloud-controller"), StorageEmail: email("gcp-pd-csi"), ImageRegistryEmail: email("image-registry"), NetworkEmail: email("cloud-network")}
		}
		g.IAM = &privatev1.ClusterIAMSpec{WorkerServiceAccount: g.WorkloadIdentity.ProjectNumber + "-compute@developer.gserviceaccount.com"}
	}
	for _, old := range c.Spec.Platform.GCP.IAM.TemplateApplications {
		if old == *ref {
			return
		}
	}
	c.Spec.Platform.GCP.IAM.TemplateApplications = append(c.Spec.Platform.GCP.IAM.TemplateApplications, *ref)
}

// CommittedCluster marks an existing reconciliation fixture, not a first install.
// Dedicated dispatch tests must start without these receipts to prove the gate.
func CommittedCluster(c *privatev1.Cluster) {
	vr := c.Status.VersionResolution
	if vr == nil {
		return
	}
	if vr.ChannelGroup == "" {
		vr.ChannelGroup = "stable"
	}
	v := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: vr.ReleaseVersion}, Spec: privatev1.VersionSpec{ReleaseImage: vr.ReleaseImage, ChannelGroups: []string{vr.ChannelGroup}}}
	MustReady(v)
	vr.WIFConfigTemplateRef = v.Spec.WIFConfigTemplateRef.DeepCopy()
	Apply(c, v.Spec.WIFConfigTemplateRef)
	c.Status.IAMDispatch = &privatev1.IAMDispatchState{Dispatched: &privatev1.IAMDispatchReceipt{ObjectUID: string(c.UID), ClusterUID: string(c.UID), ReleaseVersion: vr.ReleaseVersion, ReleaseImage: vr.ReleaseImage, ChannelGroup: vr.ChannelGroup, TemplateRef: *vr.WIFConfigTemplateRef}}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: privatev1.IAMPermissionsReady, Status: metav1.ConditionTrue, Reason: "IAMContractCovered", Message: "The installation target has exact applied IAM contract coverage", ObservedGeneration: c.Generation})
}

func CommittedPool(p *privatev1.NodePool, c *privatev1.Cluster) {
	vr := p.Status.VersionResolution
	if vr == nil {
		return
	}
	if vr.ChannelGroup == "" {
		vr.ChannelGroup = "candidate"
	}
	v := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: vr.ReleaseVersion}, Spec: privatev1.VersionSpec{ReleaseImage: vr.ReleaseImage, ChannelGroups: []string{vr.ChannelGroup}}}
	MustReady(v)
	vr.WIFConfigTemplateRef = v.Spec.WIFConfigTemplateRef.DeepCopy()
	Apply(c, v.Spec.WIFConfigTemplateRef)
	p.Status.IAMDispatch = &privatev1.IAMDispatchState{Dispatched: &privatev1.IAMDispatchReceipt{ObjectUID: string(p.UID), ClusterUID: string(c.UID), ReleaseVersion: vr.ReleaseVersion, ReleaseImage: vr.ReleaseImage, ChannelGroup: vr.ChannelGroup, TemplateRef: *vr.WIFConfigTemplateRef}}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: privatev1.IAMPermissionsReady, Status: metav1.ConditionTrue, Reason: "IAMContractCovered", Message: "The installation target has exact applied IAM contract coverage", ObservedGeneration: p.Generation})
}
