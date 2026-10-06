package virsh

import (
	"context"
	"errors"
	"testing"

	"github.com/spectrocloud/maas-client-go/maasclient"
)

// --- test doubles ---------------------------------------------------------------------------

type recordingComposer struct {
	gotParams maasclient.Params
	returnErr error
	machine   maasclient.Machine
	calls     int
}

func (c *recordingComposer) Compose(ctx context.Context, p maasclient.Params) (maasclient.Machine, error) {
	c.calls++
	c.gotParams = p
	if c.returnErr != nil {
		return nil, c.returnErr
	}
	return c.machine, nil
}

type composableHost struct {
	*fakeHost
	composer *recordingComposer
}

func (h *composableHost) Composer() maasclient.VMComposer { return h.composer }

func newComposableHost(name, zone, pool string, composer *recordingComposer) *composableHost {
	return &composableHost{fakeHost: virshHost(name, zone, pool, 64, 262144), composer: composer}
}

// --- tests ----------------------------------------------------------------------------------

func TestComposeSendsTheVMShape(t *testing.T) {
	c := &recordingComposer{machine: nil}
	host := newComposableHost("kvm1", "zone-a", "pool-a", c)

	_, err := Compose(context.Background(), host, ComposeRequest{
		Hostname: "vm-c1-01-abcde", Cores: 4, MemoryMB: 8192, DiskGB: 100,
	})
	if err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("composer called %d times, want 1", c.calls)
	}

	got := c.gotParams.Values()
	for k, want := range map[string]string{
		"hostname": "vm-c1-01-abcde",
		"cores":    "4",
		"memory":   "8192",
		"storage":  "100",
	} {
		if got.Get(k) != want {
			t.Errorf("param %q = %q, want %q", k, got.Get(k), want)
		}
	}
}

// TestComposeUsesTheHostsZoneNotTheRequests pins a correctness property that is easy to get
// wrong and silent when wrong: a VM cannot be in a different zone from its hypervisor, so the
// zone must come from the selected host. Sending a desired zone instead would let MAAS record
// a failure domain that is a lie, corrupting every rack-spreading decision downstream.
func TestComposeUsesTheHostsZoneNotTheRequests(t *testing.T) {
	c := &recordingComposer{}
	host := newComposableHost("kvm1", "zone-a", "pool-a", c)

	if _, err := Compose(context.Background(), host, ComposeRequest{
		Hostname: "vm-x", Cores: 2, MemoryMB: 4096,
	}); err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	// fakeZone/fakePool report ID 0; the point is that the keys are sent from the host at all.
	if _, ok := c.gotParams.Values()["zone"]; !ok {
		t.Error("compose params must carry the selected host's zone")
	}
	if _, ok := c.gotParams.Values()["pool"]; !ok {
		t.Error("compose params must carry the selected host's resource pool")
	}
}

func TestComposeStoragePoolQualifiesTheDisk(t *testing.T) {
	c := &recordingComposer{}
	host := newComposableHost("kvm1", "zone-a", "pool-a", c)

	if _, err := Compose(context.Background(), host, ComposeRequest{
		Hostname: "vm-x", Cores: 2, MemoryMB: 4096, DiskGB: 60, StoragePool: "fast",
	}); err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	if got := c.gotParams.Values().Get("storage"); got != "fast:60" {
		t.Fatalf("storage = %q, want %q", got, "fast:60")
	}
}

// TestComposeOmitsZeroDisk: sending "storage=0" would ask MAAS for a zero-sized disk rather
// than for the host default.
func TestComposeOmitsZeroDisk(t *testing.T) {
	c := &recordingComposer{}
	host := newComposableHost("kvm1", "zone-a", "pool-a", c)

	if _, err := Compose(context.Background(), host, ComposeRequest{
		Hostname: "vm-x", Cores: 2, MemoryMB: 4096, DiskGB: 0,
	}); err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	if _, present := c.gotParams.Values()["storage"]; present {
		t.Fatal("storage must be omitted entirely when no disk size is requested")
	}
}

// TestComposeSurfacesHostnameTakenAsTypedError is the VM-leak guard.
//
// Composition is not idempotent at the MAAS API: a retried compose creates a SECOND VM. The
// controller composes before it records the system ID, so any interruption in that window
// produces this error with a real VM already on the hypervisor. If it is not distinguishable,
// the caller retries, MAAS refuses, and the orphan is never adopted or deleted -- it just holds
// capacity until a human finds it.
func TestComposeSurfacesHostnameTakenAsTypedError(t *testing.T) {
	c := &recordingComposer{returnErr: errors.New("Failed to compose machine: hostname already exists")}
	host := newComposableHost("kvm1", "zone-a", "pool-a", c)

	_, err := Compose(context.Background(), host, ComposeRequest{Hostname: "vm-x", Cores: 2, MemoryMB: 4096})

	var taken *ErrHostnameTaken
	if !errors.As(err, &taken) {
		t.Fatalf("expected *ErrHostnameTaken so the caller can adopt the orphan, got %T: %v", err, err)
	}
	if taken.Hostname != "vm-x" {
		t.Fatalf("ErrHostnameTaken.Hostname = %q, want %q", taken.Hostname, "vm-x")
	}
}

// TestComposeDoesNotMisreadUnrelatedErrors is the other half. Misclassifying a generic failure
// as a duplicate would make the caller adopt some unrelated machine.
func TestComposeDoesNotMisreadUnrelatedErrors(t *testing.T) {
	for _, msg := range []string{
		"invalid hostname supplied", // mentions hostname, not a duplicate
		"resource already exists",   // mentions already exists, not a hostname
		"500 internal server error",
	} {
		c := &recordingComposer{returnErr: errors.New(msg)}
		host := newComposableHost("kvm1", "zone-a", "pool-a", c)
		_, err := Compose(context.Background(), host, ComposeRequest{Hostname: "vm-x", Cores: 2, MemoryMB: 4096})

		var taken *ErrHostnameTaken
		if errors.As(err, &taken) {
			t.Fatalf("error %q must NOT be classified as a duplicate hostname", msg)
		}
		if err == nil {
			t.Fatalf("error %q should still surface as an error", msg)
		}
	}
}

func TestComposeRejectsMissingInputs(t *testing.T) {
	c := &recordingComposer{}
	host := newComposableHost("kvm1", "zone-a", "pool-a", c)

	if _, err := Compose(context.Background(), nil, ComposeRequest{Hostname: "vm-x"}); err == nil {
		t.Error("compose with no host must fail")
	}
	if _, err := Compose(context.Background(), host, ComposeRequest{Hostname: ""}); err == nil {
		t.Error("compose with no hostname must fail")
	}
	if c.calls != 0 {
		t.Fatalf("invalid requests must not reach MAAS, but composer was called %d times", c.calls)
	}
}
