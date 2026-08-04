package controllers

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	infrautil "github.com/spectrocloud/cluster-api-provider-maas/pkg/util"
	"github.com/spectrocloud/maas-client-go/maasclient"
)

// mockDNSServicer implements dnsServicer for unit tests without a live MAAS endpoint.
type mockDNSServicer struct {
	dnsResource       maasclient.DNSResource
	getDNSResourceErr error
	updateCalled      bool
	updateIPs         []string
	isDriftResult     bool
	isDriftCalled     bool
}

func (m *mockDNSServicer) GetDNSResource() (maasclient.DNSResource, error) {
	return m.dnsResource, m.getDNSResourceErr
}

func (m *mockDNSServicer) UpdateDNSAttachmentsWithResource(_ maasclient.DNSResource, ips []string) (bool, error) {
	m.updateCalled = true
	m.updateIPs = ips
	return true, nil
}

func (m *mockDNSServicer) IsDriftDetected(_ maasclient.DNSResource, _ []string) bool {
	m.isDriftCalled = true
	return m.isDriftResult
}

func machineStatePtr(s infrav1beta1.MachineState) *infrav1beta1.MachineState { return &s }

func makeCPMachine(name, ns, clusterName string, powered bool, state infrav1beta1.MachineState, ip string) *infrav1beta1.MaasMachine {
	m := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
		},
		Status: infrav1beta1.MaasMachineStatus{
			MachinePowered: powered,
			MachineState:   machineStatePtr(state),
		},
	}
	if ip != "" {
		m.Status.Addresses = []clusterv1.MachineAddress{
			{Type: clusterv1.MachineExternalIP, Address: ip},
		}
	}
	return m
}

func newTestClusterScope(t *testing.T, maasCluster *infrav1beta1.MaasCluster, machines ...client.Object) *scope.ClusterScope {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = infrav1beta1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = k8sscheme.AddToScheme(scheme) // needed for ConfigMap (GetPreferredSubnets)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machines...).Build()

	cs, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:  fakeClient,
		Logger:  klogr.New(),
		Cluster: &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "test-ns"}},
		MaasCluster: maasCluster,
	})
	if err != nil {
		t.Fatalf("NewClusterScope: %v", err)
	}
	return cs
}

func defaultMaasCluster() *infrav1beta1.MaasCluster {
	return &infrav1beta1.MaasCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "test-ns"},
		Spec:       infrav1beta1.MaasClusterSpec{DNSDomain: "maas.test"},
		Status: infrav1beta1.MaasClusterStatus{
			Network: infrav1beta1.Network{DNSName: "test-cluster-abc.maas.test"},
		},
	}
}

// ---------------------------------------------------------------------------
// IsRunning
// ---------------------------------------------------------------------------

func TestIsRunning(t *testing.T) {
	tests := []struct {
		name     string
		powered  bool
		state    infrav1beta1.MachineState
		expected bool
	}{
		{"deployed and powered on", true, infrav1beta1.MachineStateDeployed, true},
		{"deployed but powered off (the bug case)", false, infrav1beta1.MachineStateDeployed, false},
		{"deploying and powered on", true, infrav1beta1.MachineStateDeploying, true},
		{"deploying but powered off", false, infrav1beta1.MachineStateDeploying, false},
		{"allocated (not in RunningStates)", true, infrav1beta1.MachineStateAllocated, false},
		{"ready (not in RunningStates)", true, infrav1beta1.MachineStateReady, false},
		{"releasing (not in RunningStates)", true, infrav1beta1.MachineStateReleasing, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			m := &infrav1beta1.MaasMachine{
				Status: infrav1beta1.MaasMachineStatus{
					MachinePowered: tc.powered,
					MachineState:   machineStatePtr(tc.state),
				},
			}
			g.Expect(IsRunning(m)).To(Equal(tc.expected))
		})
	}
}

func TestIsRunning_NilState(t *testing.T) {
	g := NewGomegaWithT(t)
	m := &infrav1beta1.MaasMachine{
		Status: infrav1beta1.MaasMachineStatus{MachinePowered: true, MachineState: nil},
	}
	g.Expect(IsRunning(m)).To(BeFalse())
}

// ---------------------------------------------------------------------------
// IsControlPlaneMachine
// ---------------------------------------------------------------------------

func TestIsControlPlaneMachine(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{clusterv1.MachineControlPlaneLabel: ""}},
	}
	worker := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"other": "label"}},
	}

	g.Expect(IsControlPlaneMachine(cp)).To(BeTrue())
	g.Expect(IsControlPlaneMachine(worker)).To(BeFalse())
}

// ---------------------------------------------------------------------------
// getExternalMachineIP
// ---------------------------------------------------------------------------

func TestGetExternalMachineIP(t *testing.T) {
	log := klogr.New()

	tests := []struct {
		name             string
		preferredSubnets []string
		addresses        []clusterv1.MachineAddress
		expectedIP       string
	}{
		{
			name:             "no subnet filter, has ExternalIP",
			preferredSubnets: nil,
			addresses: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineExternalIP, Address: "10.0.0.1"},
			},
			expectedIP: "10.0.0.1",
		},
		{
			name:             "no subnet filter, no ExternalIP (only InternalIP)",
			preferredSubnets: nil,
			addresses: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineInternalIP, Address: "10.0.0.1"},
			},
			expectedIP: "",
		},
		{
			name:             "subnet filter matches IP",
			preferredSubnets: []string{"10.0.0.0/24"},
			addresses: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineExternalIP, Address: "10.0.0.5"},
			},
			expectedIP: "10.0.0.5",
		},
		{
			name:             "subnet filter does not match IP",
			preferredSubnets: []string{"192.168.1.0/24"},
			addresses: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineExternalIP, Address: "10.0.0.5"},
			},
			expectedIP: "",
		},
		{
			name:             "no addresses",
			preferredSubnets: nil,
			addresses:        nil,
			expectedIP:       "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			m := &infrav1beta1.MaasMachine{Status: infrav1beta1.MaasMachineStatus{Addresses: tc.addresses}}
			g.Expect(getExternalMachineIP(log, tc.preferredSubnets, m)).To(Equal(tc.expectedIP))
		})
	}
}

// ---------------------------------------------------------------------------
// reconcileDNSAttachments
// ---------------------------------------------------------------------------

func TestReconcileDNSAttachments(t *testing.T) {
	r := &MaasClusterReconciler{Log: klogr.New()}

	const (
		ns          = "test-ns"
		clusterName = "test-cluster"
		cpIP        = "10.11.135.36"
	)

	runningCP := makeCPMachine("cp-1", ns, clusterName, true, infrav1beta1.MachineStateDeployed, cpIP)
	poweredOffCP := makeCPMachine("cp-1", ns, clusterName, false, infrav1beta1.MachineStateDeployed, cpIP)

	deletedCP := func() *infrav1beta1.MaasMachine {
		m := makeCPMachine("cp-deleted", ns, clusterName, true, infrav1beta1.MachineStateDeployed, cpIP)
		m.Finalizers = []string{"test-finalizer"} // required for DeletionTimestamp to be set
		ts := metav1.NewTime(time.Now())
		m.DeletionTimestamp = &ts
		return m
	}()

	runningCPNoIP := makeCPMachine("cp-no-ip", ns, clusterName, true, infrav1beta1.MachineStateDeployed, "") // powered on but no ExternalIP

	tests := []struct {
		name             string
		maasCluster      *infrav1beta1.MaasCluster
		machines         []client.Object
		mockSvc          *mockDNSServicer
		wantUpdateCalled bool
		wantHashSet      bool
		wantIsDrift      bool
		wantErr          bool
	}{
		{
			name:             "CP powered off — existingCPCount>0, DNS preserved",
			maasCluster:      defaultMaasCluster(),
			machines:         []client.Object{poweredOffCP},
			mockSvc:          &mockDNSServicer{},
			wantUpdateCalled: false,
			wantHashSet:      false,
		},
		{
			// Fix for #2: with no CP objects, DNS must be cleared (e.g. rolling replacement gap).
			// Before this fix, the guard returned nil unconditionally when IPs were empty.
			name:             "no CP machines — existingCPCount=0, DNS cleared",
			maasCluster:      defaultMaasCluster(),
			machines:         []client.Object{},
			mockSvc:          &mockDNSServicer{},
			wantUpdateCalled: true,
			wantHashSet:      true,
		},
		{
			// Fix for #2: CP has DeletionTimestamp → excluded from existingCPCount →
			// DNS should be cleared, not preserved.
			name:             "CP with DeletionTimestamp — existingCPCount=0, DNS cleared",
			maasCluster:      defaultMaasCluster(),
			machines:         []client.Object{deletedCP},
			mockSvc:          &mockDNSServicer{},
			wantUpdateCalled: true,
			wantHashSet:      true,
		},
		{
			// Fix for #5: CP is running (existingCPCount=1, runningCPCount=1) but has no
			// ExternalIP (same code path as preferred-subnet mismatch) → DNS preserved.
			name:             "CP running but no ExternalIP — existingCPCount>0, DNS preserved",
			maasCluster:      defaultMaasCluster(),
			machines:         []client.Object{runningCPNoIP},
			mockSvc:          &mockDNSServicer{},
			wantUpdateCalled: false,
			wantHashSet:      false,
		},
		{
			name:             "CP running, no prior annotation — DNS updated and hash cached",
			maasCluster:      defaultMaasCluster(),
			machines:         []client.Object{runningCP},
			mockSvc:          &mockDNSServicer{},
			wantUpdateCalled: true,
			wantHashSet:      true,
		},
		{
			name: "CP running, hash matches, MAAS in sync — skips update",
			maasCluster: func() *infrav1beta1.MaasCluster {
				mc := defaultMaasCluster()
				mc.Annotations = map[string]string{
					lastAppliedAnn: infrautil.StableHashStringSlice([]string{cpIP}),
				}
				return mc
			}(),
			machines:         []client.Object{runningCP},
			mockSvc:          &mockDNSServicer{isDriftResult: false},
			wantUpdateCalled: false,
			wantIsDrift:      true,
		},
		{
			name: "CP running, hash matches, MAAS drifted (empty) — forces re-sync",
			maasCluster: func() *infrav1beta1.MaasCluster {
				mc := defaultMaasCluster()
				mc.Annotations = map[string]string{
					lastAppliedAnn: infrautil.StableHashStringSlice([]string{cpIP}),
				}
				return mc
			}(),
			machines:         []client.Object{runningCP},
			mockSvc:          &mockDNSServicer{isDriftResult: true},
			wantUpdateCalled: true,
			wantIsDrift:      true,
			wantHashSet:      true,
		},
		{
			// Fix for #6: GetDNSResource error must propagate to the caller.
			name:        "GetDNSResource error is propagated",
			maasCluster: defaultMaasCluster(),
			machines:    []client.Object{runningCP},
			mockSvc:     &mockDNSServicer{getDNSResourceErr: errors.New("MAAS unreachable")},
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cs := newTestClusterScope(t, tc.maasCluster, tc.machines...)

			err := r.reconcileDNSAttachments(cs, tc.mockSvc)

			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(tc.mockSvc.updateCalled).To(Equal(tc.wantUpdateCalled), "updateCalled mismatch")
			g.Expect(tc.mockSvc.isDriftCalled).To(Equal(tc.wantIsDrift), "isDriftCalled mismatch")
			if tc.wantHashSet {
				g.Expect(cs.MaasCluster.Annotations).To(HaveKey(lastAppliedAnn))
				g.Expect(cs.MaasCluster.Annotations[lastAppliedAnn]).NotTo(BeEmpty())
			} else if !tc.wantUpdateCalled && !tc.wantIsDrift {
				// Hash must not be set to the empty-string sentinel
				emptyHash := infrautil.StableHashStringSlice(nil)
				if cs.MaasCluster.Annotations != nil {
					g.Expect(cs.MaasCluster.Annotations[lastAppliedAnn]).NotTo(Equal(emptyHash),
						"should never cache hash of empty IP set")
				}
			}
		})
	}
}
