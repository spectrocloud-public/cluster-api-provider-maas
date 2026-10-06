package v1beta1

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getCreated reads an object back after Create.
//
// testEnv's client reads through the manager's cache, which is populated by a watch, so a Get
// immediately after a Create can miss. Polling rather than a bare Get is what makes these
// tests deterministic instead of dependent on how fast the watch happens to deliver.
func getCreated(t *testing.T, ctx context.Context, key client.ObjectKey, obj client.Object) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if err := testEnv.Get(ctx, key, obj); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
}

// These tests run against a real apiserver (envtest) with the generated CRDs installed, so
// they check what a unit test cannot: that the schema actually accepts the virsh fields, that
// kubebuilder defaults apply, and that nothing is silently dropped on the round trip.
//
// Silent field loss is the failure mode worth guarding here. A field present in the Go struct
// but missing from the CRD is accepted by the client, pruned by the apiserver, and read back
// as absent -- so composition would fall back to bare metal with no error anywhere.

func TestMaasClusterVirshRoundTrips(t *testing.T) {
	ctx := context.Background()

	cluster := &MaasCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "MaasCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "virsh-roundtrip", Namespace: "default"},
		Spec: MaasClusterSpec{
			DNSDomain: "maas.sc",
			Virsh: &VirshConfig{
				Enabled:      ptr.To(true),
				ResourcePool: "pool-a",
				Zone:         "zone-a",
				HostTags:     []string{"k8s-worker", "kvm"},
				StoragePool:  "fast",
			},
		},
	}
	if err := testEnv.Create(ctx, cluster); err != nil {
		t.Fatalf("create with spec.virsh rejected by the apiserver: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Delete(ctx, cluster) })

	got := &MaasCluster{}
	getCreated(t, ctx, client.ObjectKeyFromObject(cluster), got)

	v := got.Spec.Virsh
	if v == nil {
		t.Fatal("spec.virsh was pruned by the apiserver -- the CRD is missing the field")
	}
	if v.Enabled == nil || !*v.Enabled {
		t.Errorf("spec.virsh.enabled = %v, want true", v.Enabled)
	}
	if v.ResourcePool != "pool-a" {
		t.Errorf("spec.virsh.resourcePool = %q, want %q", v.ResourcePool, "pool-a")
	}
	if v.Zone != "zone-a" {
		t.Errorf("spec.virsh.zone = %q, want %q", v.Zone, "zone-a")
	}
	if v.StoragePool != "fast" {
		t.Errorf("spec.virsh.storagePool = %q, want %q", v.StoragePool, "fast")
	}
	if len(v.HostTags) != 2 || v.HostTags[0] != "k8s-worker" || v.HostTags[1] != "kvm" {
		t.Errorf("spec.virsh.hostTags = %v, want [k8s-worker kvm]", v.HostTags)
	}
}

// TestMaasClusterVirshDefaultsToDisabled: virsh composition is opt-in. A cluster that
// merely mentions virsh without enabling it must not start composing VMs.
func TestMaasClusterVirshDefaultsToDisabled(t *testing.T) {
	ctx := context.Background()

	cluster := &MaasCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "MaasCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "virsh-default", Namespace: "default"},
		Spec: MaasClusterSpec{
			DNSDomain: "maas.sc",
			Virsh:     &VirshConfig{ResourcePool: "pool-a"},
		},
	}
	if err := testEnv.Create(ctx, cluster); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Delete(ctx, cluster) })

	got := &MaasCluster{}
	getCreated(t, ctx, client.ObjectKeyFromObject(cluster), got)
	if got.Spec.Virsh == nil || got.Spec.Virsh.Enabled == nil {
		t.Fatal("expected enabled to be defaulted by the CRD, got nil")
	}
	if *got.Spec.Virsh.Enabled {
		t.Error("spec.virsh.enabled defaulted to true; virsh composition must be opt-in")
	}
}

// TestMaasClusterWithoutVirshIsUnaffected: every existing cluster keeps working untouched.
func TestMaasClusterWithoutVirshIsUnaffected(t *testing.T) {
	ctx := context.Background()

	cluster := &MaasCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "MaasCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "no-virsh", Namespace: "default"},
		Spec:       MaasClusterSpec{DNSDomain: "maas.sc"},
	}
	if err := testEnv.Create(ctx, cluster); err != nil {
		t.Fatalf("create without virsh: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Delete(ctx, cluster) })

	got := &MaasCluster{}
	getCreated(t, ctx, client.ObjectKeyFromObject(cluster), got)
	if got.Spec.Virsh != nil {
		t.Errorf("spec.virsh should stay nil when unset, got %+v", got.Spec.Virsh)
	}
}

func TestMaasMachineVirshRoundTrips(t *testing.T) {
	ctx := context.Background()

	m := &MaasMachine{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "MaasMachine"},
		ObjectMeta: metav1.ObjectMeta{Name: "virsh-machine", Namespace: "default"},
		Spec: MaasMachineSpec{
			MinCPU:        ptr.To(4),
			MinMemoryInMB: ptr.To(8192),
			Image:         "ubuntu/noble",
			Virsh: &MachineVirshConfig{
				Enabled:     ptr.To(true),
				StoragePool: "nvme",
				HostTags:    []string{"gpu"},
			},
		},
	}
	if err := testEnv.Create(ctx, m); err != nil {
		t.Fatalf("create with spec.virsh rejected: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Delete(ctx, m) })

	got := &MaasMachine{}
	getCreated(t, ctx, client.ObjectKeyFromObject(m), got)
	if got.Spec.Virsh == nil {
		t.Fatal("spec.virsh was pruned -- the MaasMachine CRD is missing the field")
	}
	if got.Spec.Virsh.Enabled == nil || !*got.Spec.Virsh.Enabled {
		t.Error("spec.virsh.enabled did not round trip")
	}
	if got.Spec.Virsh.StoragePool != "nvme" {
		t.Errorf("spec.virsh.storagePool = %q, want nvme", got.Spec.Virsh.StoragePool)
	}
	if len(got.Spec.Virsh.HostTags) != 1 || got.Spec.Virsh.HostTags[0] != "gpu" {
		t.Errorf("spec.virsh.hostTags = %v, want [gpu]", got.Spec.Virsh.HostTags)
	}
}

// TestMaasMachineTemplateCarriesVirsh matters because MachineDeployments render machines from
// the template. If virsh is absent from the template schema it is pruned there, and every
// scaled-up worker silently lands on bare metal instead of a hypervisor.
func TestMaasMachineTemplateCarriesVirsh(t *testing.T) {
	ctx := context.Background()

	tpl := &MaasMachineTemplate{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "MaasMachineTemplate"},
		ObjectMeta: metav1.ObjectMeta{Name: "virsh-template", Namespace: "default"},
		Spec: MaasMachineTemplateSpec{
			Template: MaasMachineTemplateResource{
				Spec: MaasMachineSpec{
					MinCPU:        ptr.To(4),
					MinMemoryInMB: ptr.To(8192),
					Image:         "ubuntu/noble",
					Virsh:         &MachineVirshConfig{Enabled: ptr.To(true), StoragePool: "fast"},
				},
			},
		},
	}
	if err := testEnv.Create(ctx, tpl); err != nil {
		t.Fatalf("create template with virsh rejected: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Delete(ctx, tpl) })

	got := &MaasMachineTemplate{}
	getCreated(t, ctx, client.ObjectKeyFromObject(tpl), got)
	v := got.Spec.Template.Spec.Virsh
	if v == nil {
		t.Fatal("template.spec.virsh was pruned -- every templated worker would land on metal")
	}
	if v.Enabled == nil || !*v.Enabled || v.StoragePool != "fast" {
		t.Errorf("template.spec.virsh did not round trip: %+v", v)
	}
}
