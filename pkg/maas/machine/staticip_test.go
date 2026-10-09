package machine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	pkgerrors "github.com/pkg/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	mockclientset "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/client/mock"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/maas-client-go/maasclient"
)

// testSubnet is a maasclient.Subnet carrying the ID the static-IP checks key on.
// (machine_test.go already has a cidr-only fakeSubnet with a hardcoded ID.)
type testSubnet struct {
	id   int
	cidr string
}

func (f testSubnet) ID() int               { return f.id }
func (f testSubnet) Name() string          { return "subnet" }
func (f testSubnet) Space() string         { return "space" }
func (f testSubnet) VLAN() maasclient.VLAN { return nil }
func (f testSubnet) CIDR() string          { return f.cidr }

// testSubnets serves canned subnet data so the static-IP checks can be exercised
// without a MAAS server.
type testSubnets struct {
	subnets     []maasclient.Subnet
	listErr     error
	allocErr    error
	reservedErr error
	allocs      map[int][]maasclient.SubnetIPAddress
	reserved    map[int][]maasclient.SubnetIPRange
}

func (f testSubnets) List(context.Context) ([]maasclient.Subnet, error) {
	return f.subnets, f.listErr
}

func (f testSubnets) GetIDByCIDR(_ context.Context, cidr string) (int, error) {
	for _, sn := range f.subnets {
		if sn.CIDR() == cidr {
			return sn.ID(), nil
		}
	}
	return 0, errors.New("subnet not found")
}

func (f testSubnets) GetIPAddresses(_ context.Context, subnetID int) ([]maasclient.SubnetIPAddress, error) {
	if f.allocErr != nil {
		return nil, f.allocErr
	}
	return f.allocs[subnetID], nil
}

func (f testSubnets) IsIPInUse(_ context.Context, subnetID int, ip string) (bool, error) {
	for _, a := range f.allocs[subnetID] {
		if a.IP == ip {
			return true, nil
		}
	}
	return false, nil
}

func (f testSubnets) GetReservedIPRanges(_ context.Context, subnetID int) ([]maasclient.SubnetIPRange, error) {
	if f.reservedErr != nil {
		return nil, f.reservedErr
	}
	return f.reserved[subnetID], nil
}

func (f testSubnets) GetUnreservedIPRanges(context.Context, int) ([]maasclient.SubnetIPRange, error) {
	return nil, nil
}

func newStaticIPService(t *testing.T, subnets maasclient.Subnets) *Service {
	t.Helper()
	ctrl := gomock.NewController(t)
	clientSet := mockclientset.NewMockClientSetInterface(ctrl)
	clientSet.EXPECT().Subnets().Return(subnets).AnyTimes()
	return &Service{
		scope:      &scope.MachineScope{Logger: klogr.New()},
		maasClient: clientSet,
	}
}

const testSubnetCIDR = "10.0.0.0/24"

func TestValidateStaticIPAvailable(t *testing.T) {
	tests := []struct {
		name string
		// subnets under test
		subnets testSubnets
		ip      string
		// wantErr is a substring the error must carry; "" means no error expected
		wantErr string
	}{
		{
			name:    "free address in a known subnet composes",
			subnets: testSubnets{subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}}},
			ip:      "10.0.0.5",
		},
		{
			name:    "address outside every MAAS subnet is refused",
			subnets: testSubnets{subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}}},
			ip:      "192.168.5.5",
			wantErr: "not within any subnet known to MAAS",
		},
		{
			name: "address already allocated to another owner names the conflict",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
				allocs: map[int][]maasclient.SubnetIPAddress{
					7: {{IP: "10.0.0.5", AllocType: 4, User: "someone-else"}},
				},
			},
			ip:      "10.0.0.5",
			wantErr: "already allocated in MAAS",
		},
		{
			name: "allocated address with no recorded owner still reports a conflict",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
				allocs: map[int][]maasclient.SubnetIPAddress{
					7: {{IP: "10.0.0.5", AllocType: 0}},
				},
			},
			ip:      "10.0.0.5",
			wantErr: "an unknown owner",
		},
		{
			name:    "invalid address is refused",
			subnets: testSubnets{subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}}},
			ip:      "not-an-ip",
			wantErr: "not a valid IP address",
		},
		{
			name:    "subnet lookup failure is fail-closed",
			subnets: testSubnets{listErr: errors.New("maas unreachable")},
			ip:      "10.0.0.5",
			wantErr: "failed to list subnets",
		},
		{
			name: "address lookup failure is fail-closed",
			subnets: testSubnets{
				subnets:  []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
				allocErr: errors.New("403 forbidden"),
			},
			ip:      "10.0.0.5",
			wantErr: "could not read the allocated addresses",
		},
		{
			// MAAS keeps reserved ranges out of dynamic allocation; on a managed
			// subnet that is where static addresses belong, so it is not a conflict.
			name: "address inside a reserved range is not treated as a conflict",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
				reserved: map[int][]maasclient.SubnetIPRange{
					7: {{Start: "10.0.0.1", End: "10.0.0.20"}},
				},
			},
			ip: "10.0.0.5",
		},
		{
			name: "reserved-range lookup failure does not block composition",
			subnets: testSubnets{
				subnets:     []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
				reservedErr: errors.New("boom"),
			},
			ip: "10.0.0.5",
		},
		{
			name:    "ipv6 address in a known subnet composes",
			subnets: testSubnets{subnets: []maasclient.Subnet{testSubnet{id: 9, cidr: "fd00::/64"}}},
			ip:      "fd00::5",
		},
		{
			// MAAS may report an equivalent address in a different textual form; a string
			// compare would miss the conflict and let the compose through.
			name: "ipv6 conflict is found through a different textual form",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 9, cidr: "fd00::/64"}},
				allocs: map[int][]maasclient.SubnetIPAddress{
					9: {{IP: "fd00:0:0:0:0:0:0:5", AllocType: 4, User: "someone-else"}},
				},
			},
			ip:      "fd00::5",
			wantErr: "already allocated in MAAS",
		},
		{
			name: "ipv4 conflict is found through a v4-in-v6 form",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
				allocs: map[int][]maasclient.SubnetIPAddress{
					7: {{IP: "::ffff:10.0.0.5", AllocType: 4, User: "someone-else"}},
				},
			},
			ip:      "10.0.0.5",
			wantErr: "already allocated in MAAS",
		},
		{
			name: "an unparseable allocation entry is fail-closed",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
				allocs: map[int][]maasclient.SubnetIPAddress{
					7: {{IP: "not-an-ip"}},
				},
			},
			ip:      "10.0.0.5",
			wantErr: "cannot be parsed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newStaticIPService(t, tt.subnets)

			err := svc.validateStaticIPAvailable(context.Background(), tt.ip)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

// TestValidateStaticIPAvailableConflictMessage pins the parts of the message an
// operator needs in order to act: the address, the subnet, and who holds it.
func TestValidateStaticIPAvailableConflictMessage(t *testing.T) {
	svc := newStaticIPService(t, testSubnets{
		subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
		allocs: map[int][]maasclient.SubnetIPAddress{
			7: {{IP: "10.0.0.5", AllocType: 4, User: "someone-else"}},
		},
	})

	err := svc.validateStaticIPAvailable(context.Background(), "10.0.0.5")
	if err == nil {
		t.Fatal("expected a conflict error, got nil")
	}
	for _, want := range []string{"10.0.0.5", "subnet ID: 7", "someone-else", "alloc type: 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err.Error(), want)
		}
	}
}

func TestIPInRange(t *testing.T) {
	tests := []struct {
		name           string
		start, end, ip string
		want           bool
	}{
		{name: "inside", start: "10.0.0.1", end: "10.0.0.20", ip: "10.0.0.5", want: true},
		{name: "on the lower bound", start: "10.0.0.1", end: "10.0.0.20", ip: "10.0.0.1", want: true},
		{name: "on the upper bound", start: "10.0.0.1", end: "10.0.0.20", ip: "10.0.0.20", want: true},
		{name: "below", start: "10.0.0.1", end: "10.0.0.20", ip: "10.0.0.0", want: false},
		{name: "above", start: "10.0.0.1", end: "10.0.0.20", ip: "10.0.0.21", want: false},
		{name: "ipv6 is out of scope and reports false", start: "fd00::1", end: "fd00::ff", ip: "fd00::5", want: false},
		{name: "malformed range", start: "nonsense", end: "10.0.0.20", ip: "10.0.0.5", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ipInRange(tt.start, tt.end, net.ParseIP(tt.ip)); got != tt.want {
				t.Errorf("ipInRange(%q, %q, %q) = %v, want %v", tt.start, tt.end, tt.ip, got, tt.want)
			}
		})
	}
}

// TestErrMachineCommissioningSurvivesWrapping guards the contract both the
// DeployMachine defer and the controller's requeue branch depend on: the sentinel has to
// stay detectable through the pkg/errors wrapping applied on the way out. If it stops
// matching, the defer releases a machine that is merely commissioning and the reconcile
// retries against a different one.
func TestErrMachineCommissioningSurvivesWrapping(t *testing.T) {
	inner := fmt.Errorf("%w: static IP configuration will be retried once commissioning completes", ErrMachineCommissioning)

	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "unwrapped", err: inner},
		{name: "wrapped as the standard path does", err: pkgerrors.Wrapf(inner, "failed to configure static IP")},
		{name: "wrapped as the LXD path does", err: pkgerrors.Wrap(inner, "failed to configure static IP before deploy")},
		{name: "wrapped twice", err: pkgerrors.Wrap(pkgerrors.Wrap(inner, "outer"), "outermost")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, ErrMachineCommissioning) {
				t.Fatalf("errors.Is lost the sentinel through %s: %v", tt.name, tt.err)
			}
		})
	}

	// An unrelated failure must not be mistaken for the commissioning wait, or the defer
	// would stop releasing machines that genuinely failed to deploy.
	t.Run("an unrelated error does not match", func(t *testing.T) {
		if errors.Is(pkgerrors.Wrap(errors.New("boom"), "failed to configure static IP"), ErrMachineCommissioning) {
			t.Fatal("an unrelated error matched ErrMachineCommissioning")
		}
	})
}

// TestSetMachineStaticIPCommissioningReturnsSentinel pins the actual return of
// setMachineStaticIP, not just the wrapping contract. An earlier revision of this work
// returned a bare fmt.Errorf here, which matched no sentinel: the controller then
// reported a deployment failure and the deferred cleanup in DeployMachine released the
// commissioning machine. Guard the typed return so that cannot regress silently.
func TestSetMachineStaticIPCommissioningReturnsSentinel(t *testing.T) {
	ctrl := gomock.NewController(t)
	clientSet := mockclientset.NewMockClientSetInterface(ctrl)
	machines := mockclientset.NewMockMachines(ctrl)
	machine := mockclientset.NewMockMachine(ctrl)

	clientSet.EXPECT().Machines().Return(machines)
	machines.EXPECT().Machine("abc123").Return(machine)
	machine.EXPECT().Get(gomock.Any()).Return(machine, nil)
	machine.EXPECT().State().Return("Commissioning").AnyTimes()

	svc := &Service{
		scope:      &scope.MachineScope{Logger: klogr.New()},
		maasClient: clientSet,
	}

	err := svc.setMachineStaticIP("abc123", &infrav1beta1.StaticIPConfig{IP: "10.0.0.5"})
	if err == nil {
		t.Fatal("expected an error while the machine is commissioning, got nil")
	}
	if !errors.Is(err, ErrMachineCommissioning) {
		t.Fatalf("expected ErrMachineCommissioning, got %v", err)
	}
}

// TestDeployMachineKeepsCommissioningAllocation covers the deferred cleanup in
// DeployMachine. That defer fires on any non-nil return and normally releases the machine
// and clears Spec.ProviderID / Spec.SystemID so the next reconcile picks a different one.
// A machine that is merely still commissioning must be exempt: otherwise the
// "retry once commissioning completes" wait releases the machine mid-commissioning and
// retries against a fresh allocation, churning machines instead of waiting. PCP-6208.
func TestDeployMachineKeepsCommissioningAllocation(t *testing.T) {
	ctrl := gomock.NewController(t)
	clientSet := mockclientset.NewMockClientSetInterface(ctrl)
	machines := mockclientset.NewMockMachines(ctrl)
	machine := mockclientset.NewMockMachine(ctrl)
	modifier := mockclientset.NewMockMachineModifier(ctrl)

	// Reuse path: a providerID is already set, so DeployMachine skips allocation and
	// takes the branch that registers the deferred cleanup.
	providerID := "maas:///zone1/abc123"
	systemID := "abc123"
	maasMachine := &infrav1beta1.MaasMachine{
		Spec: infrav1beta1.MaasMachineSpec{
			ProviderID: &providerID,
			SystemID:   &systemID,
			StaticIP:   &infrav1beta1.StaticIPConfig{IP: "10.0.0.5"},
		},
	}

	svc := &Service{
		scope: &scope.MachineScope{
			Logger:      klogr.New(),
			Cluster:     &clusterv1.Cluster{ObjectMeta: v1.ObjectMeta{Name: "a"}},
			MaasMachine: maasMachine,
			// A control-plane machine, which is what the static-IP path applies to.
			Machine: &clusterv1.Machine{
				ObjectMeta: v1.ObjectMeta{
					Name:   "cp-0",
					Labels: map[string]string{clusterv1.MachineControlPlaneLabel: ""},
				},
			},
			ClusterScope: &scope.ClusterScope{MaasCluster: &infrav1beta1.MaasCluster{}},
		},
		maasClient: clientSet,
	}

	clientSet.EXPECT().Machines().Return(machines).AnyTimes()
	machines.EXPECT().Machine(systemID).Return(machine).AnyTimes()
	machine.EXPECT().Get(gomock.Any()).Return(machine, nil).AnyTimes()
	machine.EXPECT().SystemID().Return(systemID).AnyTimes()
	machine.EXPECT().Modifier().Return(modifier).AnyTimes()
	modifier.EXPECT().SetSwapSize(0).Return(modifier).AnyTimes()
	modifier.EXPECT().Update(gomock.Any()).Return(machine, nil).AnyTimes()
	// Still commissioning, so setMachineStaticIP returns the sentinel.
	machine.EXPECT().State().Return("Commissioning").AnyTimes()

	// The point of the test: Releaser() must never be reached on this path. It is left
	// unstubbed, so a release attempt fails the test through gomock.

	_, err := svc.DeployMachine("")
	if err == nil {
		t.Fatal("expected an error while the machine is commissioning, got nil")
	}
	if !errors.Is(err, ErrMachineCommissioning) {
		t.Fatalf("expected ErrMachineCommissioning, got %v", err)
	}

	// The allocation must survive, or the next reconcile would not retry this machine.
	if maasMachine.Spec.ProviderID == nil || *maasMachine.Spec.ProviderID != providerID {
		t.Errorf("Spec.ProviderID was cleared or changed: %v", maasMachine.Spec.ProviderID)
	}
	if maasMachine.Spec.SystemID == nil || *maasMachine.Spec.SystemID != systemID {
		t.Errorf("Spec.SystemID was cleared or changed: %v", maasMachine.Spec.SystemID)
	}
}
