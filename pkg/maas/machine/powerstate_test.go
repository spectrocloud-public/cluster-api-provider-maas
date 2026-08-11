package machine

import (
	"net"
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/gomega"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	mockclientset "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/client/mock"
)

// TestFromSDKTypeToMachinePowerState pins the SDK-boundary contract for PCP-7384: the raw MAAS
// power_state reading must survive conversion, so the reconciler can tell a positively-confirmed
// off from a failed BMC query. Powered keeps its original "on"-only semantics - existing consumers
// (Status.MachinePowered, IsRunning, control-plane DNS) must be unaffected by this change.
func TestFromSDKTypeToMachinePowerState(t *testing.T) {
	tests := []struct {
		name             string
		powerState       string
		expectPowered    bool
		expectPowerState string
	}{
		{
			name:             "confirmed on",
			powerState:       infrav1beta1.MachinePowerStateOn,
			expectPowered:    true,
			expectPowerState: infrav1beta1.MachinePowerStateOn,
		},
		{
			name:             "confirmed off",
			powerState:       infrav1beta1.MachinePowerStateOff,
			expectPowered:    false,
			expectPowerState: infrav1beta1.MachinePowerStateOff,
		},
		{
			name:             "failed BMC query surfaces as error, not off",
			powerState:       infrav1beta1.MachinePowerStateError,
			expectPowered:    false,
			expectPowerState: infrav1beta1.MachinePowerStateError,
		},
		{
			name:             "unknown power state is preserved verbatim",
			powerState:       infrav1beta1.MachinePowerStateUnknown,
			expectPowered:    false,
			expectPowerState: infrav1beta1.MachinePowerStateUnknown,
		},
		{
			name:             "empty power state is preserved verbatim",
			powerState:       "",
			expectPowered:    false,
			expectPowerState: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctrl := gomock.NewController(t)
			mockMachine := mockclientset.NewMockMachine(ctrl)
			mockZone := mockclientset.NewMockZone(ctrl)

			mockMachine.EXPECT().SystemID().Return("abc123")
			mockMachine.EXPECT().Hostname().Return("abc.hostname")
			mockMachine.EXPECT().State().Return("Deployed")
			mockMachine.EXPECT().PowerState().Return(tt.powerState)
			mockMachine.EXPECT().Zone().Return(mockZone)
			mockZone.EXPECT().Name().Return("zone1")
			mockMachine.EXPECT().FQDN().AnyTimes().Return("")
			mockMachine.EXPECT().IPAddresses().Return([]net.IP{})

			machine := fromSDKTypeToMachine(mockMachine)

			g.Expect(machine.Powered).To(Equal(tt.expectPowered))
			g.Expect(machine.PowerState).To(Equal(tt.expectPowerState))
			g.Expect(machine.State).To(BeEquivalentTo("Deployed"))
		})
	}
}
