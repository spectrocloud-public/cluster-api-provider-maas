package controllers

import (
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pkg/errors"

	maasmachine "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/machine"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/virsh"
)

func TestIsWaitingForVirshHost(t *testing.T) {
	g := NewWithT(t)

	// Wrapped as DeployMachine and deployMachine wrap it.
	noHost := errors.Wrapf(
		errors.Wrap(&virsh.NoEligibleHostError{Considered: 2, Reason: "full"}, "virsh: compose failed prior to deploy"),
		"failed to deploy MaasMachine instance")

	g.Expect(isWaitingForVirshHost(noHost)).To(BeTrue())
	g.Expect(isWaitingForVirshHost(maasmachine.ErrVMComposing)).To(BeFalse())
	g.Expect(isWaitingForVirshHost(errors.New("compose failed"))).To(BeFalse())
}
