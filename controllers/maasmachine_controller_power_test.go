package controllers

import (
	"testing"

	. "github.com/onsi/gomega"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
)

// TestIsConfirmedPoweredOff is the regression gate for PCP-7384. Only a positively-confirmed
// "off" may authorise a power-on: MAAS translates op-power_on into `ipmipower --cycle --on-if-off`
// (Launchpad #1730089), so acting on an unconfirmed reading hard power-cycles a running host.
func TestIsConfirmedPoweredOff(t *testing.T) {
	tests := []struct {
		name          string
		powerState    string
		expectPowerOn bool
	}{
		{
			name:          "confirmed off authorises power-on",
			powerState:    infrav1beta1.MachinePowerStateOff,
			expectPowerOn: true,
		},
		{
			name:          "confirmed on does not authorise power-on",
			powerState:    infrav1beta1.MachinePowerStateOn,
			expectPowerOn: false,
		},
		{
			name:          "failed BMC query must never authorise power-on",
			powerState:    infrav1beta1.MachinePowerStateError,
			expectPowerOn: false,
		},
		{
			name:          "unknown must never authorise power-on",
			powerState:    infrav1beta1.MachinePowerStateUnknown,
			expectPowerOn: false,
		},
		{
			name:          "empty reading must never authorise power-on",
			powerState:    "",
			expectPowerOn: false,
		},
		{
			name:          "unrecognised value must never authorise power-on",
			powerState:    "Off",
			expectPowerOn: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(isConfirmedPoweredOff(tt.powerState)).To(Equal(tt.expectPowerOn))
		})
	}
}
