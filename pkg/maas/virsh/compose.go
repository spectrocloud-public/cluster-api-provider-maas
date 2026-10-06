package virsh

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/spectrocloud/maas-client-go/maasclient"
)

// ComposeRequest describes the VM to compose on a selected virsh host.
type ComposeRequest struct {
	// Hostname must be stable across reconciles -- see ErrHostnameTaken.
	Hostname string
	Cores    int
	MemoryMB int
	DiskGB   int
	// StoragePool is the libvirt storage pool for the disk; empty uses the host default.
	StoragePool string
}

// ErrHostnameTaken signals that MAAS already has a machine with the requested hostname.
//
// Compose is not idempotent -- a retry creates a second VM -- and the controller composes
// before it can record the system ID. Any interruption in that window produces this error with
// a real VM already on the hypervisor, so callers must adopt it rather than retry. Treating it
// as a generic failure leaks the VM: nothing references it, so nothing deletes it.
type ErrHostnameTaken struct {
	Hostname string
	Cause    error
}

func (e *ErrHostnameTaken) Error() string {
	return fmt.Sprintf("a machine named %q already exists in MAAS: %v", e.Hostname, e.Cause)
}

func (e *ErrHostnameTaken) Unwrap() error { return e.Cause }

// isHostnameTaken recognises MAAS's duplicate-hostname rejection, which arrives as a 400 with
// a validation message rather than a typed error. Both words must match: misreading an
// unrelated failure as a duplicate would make the caller adopt some other machine.
func isHostnameTaken(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "hostname") && strings.Contains(s, "already exists")
}

// Compose creates a VM on the given virsh host.
//
// Zone and pool come from the selected host, not the request: a VM cannot be in a different
// zone from the hypervisor running it, and a VM whose recorded failure domain is wrong
// corrupts every rack-spreading decision downstream.
func Compose(ctx context.Context, host maasclient.VMHost, req ComposeRequest) (maasclient.Machine, error) {
	if host == nil {
		return nil, errors.New("virsh: no host selected")
	}
	if req.Hostname == "" {
		return nil, errors.New("virsh: compose requires a stable hostname")
	}

	params := maasclient.ParamsBuilder().
		Set("hostname", req.Hostname).
		Set("cores", fmt.Sprintf("%d", req.Cores)).
		Set("memory", fmt.Sprintf("%d", req.MemoryMB))

	// MAAS expects "<size in GB>" or "<pool>:<size>". Omit it entirely when no size was asked
	// for, so that a zero does not become a request for a 0 GB disk.
	if req.DiskGB > 0 {
		if req.StoragePool != "" {
			params.Set("storage", fmt.Sprintf("%s:%d", req.StoragePool, req.DiskGB))
		} else {
			params.Set("storage", fmt.Sprintf("%d", req.DiskGB))
		}
	}

	if z := host.Zone(); z != nil {
		params.Set("zone", fmt.Sprintf("%d", z.ID()))
	}
	if p := host.ResourcePool(); p != nil {
		params.Set("pool", fmt.Sprintf("%d", p.ID()))
	}

	m, err := host.Composer().Compose(ctx, params)
	if err != nil {
		if isHostnameTaken(err) {
			return nil, &ErrHostnameTaken{Hostname: req.Hostname, Cause: err}
		}
		return nil, errors.Wrapf(err, "virsh: compose on host %q failed", host.Name())
	}
	return m, nil
}

// FindByHostname locates a machine by hostname, for adopting a VM that an earlier compose
// created but never recorded. Returns nil, nil when there is no such machine.
func FindByHostname(ctx context.Context, client maasclient.ClientSetInterface, hostname string) (maasclient.Machine, error) {
	machines, err := client.Machines().List(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "virsh: unable to list machines to adopt by hostname")
	}
	for _, m := range machines {
		if strings.EqualFold(m.Hostname(), hostname) {
			return m, nil
		}
	}
	return nil, nil
}
