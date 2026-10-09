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
	subnets       []maasclient.Subnet
	listErr       error
	allocErr      error
	unreservedErr error
	allocs        map[int][]maasclient.SubnetIPAddress
	unreserved    map[int][]maasclient.SubnetIPRange
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
	// Production code no longer reads this endpoint - it cannot distinguish a
	// user-reserved range from the DHCP pool or the gateway. The typed ipranges
	// endpoint is used instead; see validateStaticIPIsAllocatableSpace.
	return nil, nil
}

func (f testSubnets) GetUnreservedIPRanges(_ context.Context, subnetID int) ([]maasclient.SubnetIPRange, error) {
	if f.unreservedErr != nil {
		return nil, f.unreservedErr
	}
	return f.unreserved[subnetID], nil
}

// testIPRange / testIPRanges stand in for the typed ipranges endpoint, which is what
// distinguishes an operator-reserved range from the DHCP pool.
type testIPRange struct {
	typ, start, end, comment string
}

func (r testIPRange) ID() int                   { return 1 }
func (r testIPRange) Type() string              { return r.typ }
func (r testIPRange) StartIP() string           { return r.start }
func (r testIPRange) EndIP() string             { return r.end }
func (r testIPRange) Comment() string           { return r.comment }
func (r testIPRange) Subnet() maasclient.Subnet { return nil }

type testIPRanges struct {
	ranges map[int][]maasclient.IPRange
	err    error
}

func (f testIPRanges) List(context.Context) ([]maasclient.IPRange, error) { return nil, f.err }

func (f testIPRanges) ListBySubnet(_ context.Context, subnetID int) ([]maasclient.IPRange, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ranges[subnetID], nil
}

func (f testIPRanges) IsIPInRange(context.Context, int, string) (bool, error) { return false, f.err }

func newStaticIPService(t *testing.T, subnets maasclient.Subnets, ranges ...maasclient.IPRanges) *Service {
	t.Helper()
	ctrl := gomock.NewController(t)
	clientSet := mockclientset.NewMockClientSetInterface(ctrl)
	clientSet.EXPECT().Subnets().Return(subnets).AnyTimes()
	var ipr maasclient.IPRanges = testIPRanges{}
	if len(ranges) > 0 {
		ipr = ranges[0]
	}
	clientSet.EXPECT().IPRanges().Return(ipr).AnyTimes()
	return &Service{
		scope:      &scope.MachineScope{Logger: klogr.New()},
		maasClient: clientSet,
	}
}

const testSubnetCIDR = "10.0.0.0/24"

// freeSubnet is a subnet whose whole usable span MAAS reports as unreserved.
func freeSubnet() testSubnets {
	return testSubnets{
		subnets:    []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
		unreserved: map[int][]maasclient.SubnetIPRange{7: {{Start: "10.0.0.2", End: "10.0.0.254"}}},
	}
}

func TestValidateStaticIPAvailable(t *testing.T) {
	tests := []struct {
		name string
		// subnets under test
		subnets testSubnets
		// typed ipranges (dynamic vs reserved) for the same subnet
		ranges testIPRanges
		ip     string
		// wantErr is a substring the error must carry; "" means no error expected
		wantErr string
	}{
		{
			name:    "address in MAAS's freely allocatable space composes",
			subnets: freeSubnet(),
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
			// An operator-reserved range is the intended home for a static address, so
			// being outside free space is acceptable when a typed "reserved" range covers it.
			name: "address in an operator-reserved range composes",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
			},
			ranges: testIPRanges{ranges: map[int][]maasclient.IPRange{
				7: {testIPRange{typ: "reserved", start: "10.0.0.1", end: "10.0.0.20", comment: "CP static pool"}},
			}},
			ip: "10.0.0.5",
		},
		{
			// The DHCP pool: MAAS allocates it itself, so a static assignment there is refused.
			name: "address in the dynamic DHCP range is refused",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
			},
			ranges: testIPRanges{ranges: map[int][]maasclient.IPRange{
				7: {testIPRange{typ: "dynamic", start: "10.0.0.100", end: "10.0.0.200"}},
			}},
			ip:      "10.0.0.150",
			wantErr: "DHCP (dynamic) range",
		},
		{
			// Gateway / DNS / assigned-elsewhere: held back, with no typed range naming it.
			name: "address MAAS holds back for its own use is refused",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
			},
			ip:      "10.0.0.1",
			wantErr: "reserved for MAAS's own use",
		},
		{
			name: "unreserved-range lookup failure is fail-closed",
			subnets: testSubnets{
				subnets:       []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
				unreservedErr: errors.New("boom"),
			},
			ip:      "10.0.0.5",
			wantErr: "could not read the unreserved IP ranges",
		},
		{
			name: "iprange lookup failure is fail-closed",
			subnets: testSubnets{
				subnets: []maasclient.Subnet{testSubnet{id: 7, cidr: testSubnetCIDR}},
			},
			ranges:  testIPRanges{err: errors.New("boom")},
			ip:      "10.0.0.5",
			wantErr: "could not read the IP ranges",
		},
		{
			name: "ipv6 address in free space composes",
			subnets: testSubnets{
				subnets:    []maasclient.Subnet{testSubnet{id: 9, cidr: "fd00::/64"}},
				unreserved: map[int][]maasclient.SubnetIPRange{9: {{Start: "fd00::1", End: "fd00::ffff"}}},
			},
			ip: "fd00::5",
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
			svc := newStaticIPService(t, tt.subnets, tt.ranges)

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
		{name: "ipv6 inside the range", start: "fd00::1", end: "fd00::ff", ip: "fd00::5", want: true},
		{name: "ipv6 on the upper bound", start: "fd00::1", end: "fd00::ff", ip: "fd00::ff", want: true},
		{name: "ipv6 above the range", start: "fd00::1", end: "fd00::ff", ip: "fd00::100", want: false},
		{name: "mixed families do not compare", start: "10.0.0.1", end: "10.0.0.20", ip: "fd00::5", want: false},
		{name: "v4-mapped form still matches a v4 range", start: "10.0.0.1", end: "10.0.0.20", ip: "::ffff:10.0.0.5", want: true},
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

// TestFindSubnetIDForIPPrefersMostSpecific covers nested subnets. The engineering MAAS
// defines 10.11.160.0/24 inside 10.11.160.0/23 (and 10.10.173.0/24 inside 10.10.128.0/18),
// and MAAS records an address against the narrower one. Returning whichever subnet happened
// to come first in the list would read a different subnet's allocations and could miss a
// conflict; MAAS's list order is neither sorted nor documented. PCP-6208.
func TestFindSubnetIDForIPPrefersMostSpecific(t *testing.T) {
	tests := []struct {
		name    string
		subnets []maasclient.Subnet
		ip      string
		want    int
	}{
		{
			name: "narrower subnet listed first",
			subnets: []maasclient.Subnet{
				testSubnet{id: 6, cidr: "10.11.160.0/24"},
				testSubnet{id: 213, cidr: "10.11.160.0/23"},
			},
			ip:   "10.11.160.15",
			want: 6,
		},
		{
			// The ordering that would break a first-match implementation.
			name: "wider subnet listed first",
			subnets: []maasclient.Subnet{
				testSubnet{id: 213, cidr: "10.11.160.0/23"},
				testSubnet{id: 6, cidr: "10.11.160.0/24"},
			},
			ip:   "10.11.160.15",
			want: 6,
		},
		{
			name: "deeply nested, wider first",
			subnets: []maasclient.Subnet{
				testSubnet{id: 4, cidr: "10.10.128.0/18"},
				testSubnet{id: 59, cidr: "10.10.173.0/24"},
			},
			ip:   "10.10.173.50",
			want: 59,
		},
		{
			name: "address only the wider subnet covers",
			subnets: []maasclient.Subnet{
				testSubnet{id: 59, cidr: "10.10.173.0/24"},
				testSubnet{id: 4, cidr: "10.10.128.0/18"},
			},
			ip:   "10.10.161.120",
			want: 4,
		},
		{
			name: "unparseable CIDRs are skipped, not fatal",
			subnets: []maasclient.Subnet{
				testSubnet{id: 1, cidr: "not-a-cidr"},
				testSubnet{id: 6, cidr: "10.11.160.0/24"},
			},
			ip:   "10.11.160.15",
			want: 6,
		},
		{
			name:    "ipv6 nesting prefers the longer prefix",
			subnets: []maasclient.Subnet{testSubnet{id: 20, cidr: "fd00::/48"}, testSubnet{id: 21, cidr: "fd00::/64"}},
			ip:      "fd00::5",
			want:    21,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newStaticIPService(t, testSubnets{subnets: tt.subnets}, testIPRanges{})

			got, err := svc.findSubnetIDForIP(context.Background(), net.ParseIP(tt.ip))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("findSubnetIDForIP(%s) = subnet %d, want %d", tt.ip, got, tt.want)
			}
		})
	}
}
