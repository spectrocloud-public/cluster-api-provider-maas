package controllers

import (
	"errors"
	"testing"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

type fakeMachineIPResolver struct {
	ip           string
	err          error
	gotSystemID  string
	gotInterface string
}

func (f *fakeMachineIPResolver) GetMachineIPForInterface(systemID, interfaceName string) (string, error) {
	f.gotSystemID = systemID
	f.gotInterface = interfaceName
	return f.ip, f.err
}

func TestSelectMachineIPForDNSFallsBackToExternalIP(t *testing.T) {
	m := &infrav1beta1.MaasMachine{
		Status: infrav1beta1.MaasMachineStatus{
			Addresses: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineInternalIP, Address: "10.0.0.10"},
				{Type: clusterv1.MachineExternalIP, Address: "192.168.0.10"},
				{Type: clusterv1.MachineExternalIP, Address: "192.168.0.11"},
			},
		},
	}

	got, err := selectMachineIPForDNS(m, "", nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != "192.168.0.10" {
		t.Fatalf("expected first external IP, got %q", got)
	}
}

func TestSelectMachineIPForDNSUsesPreferredInterface(t *testing.T) {
	systemID := "abc123"
	m := &infrav1beta1.MaasMachine{
		Spec: infrav1beta1.MaasMachineSpec{
			SystemID: &systemID,
		},
	}
	resolver := &fakeMachineIPResolver{ip: "10.20.30.40"}

	got, err := selectMachineIPForDNS(m, "eno2", resolver)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != "10.20.30.40" {
		t.Fatalf("expected resolver IP, got %q", got)
	}
	if resolver.gotSystemID != systemID || resolver.gotInterface != "eno2" {
		t.Fatalf("resolver called with unexpected args: systemID=%q interface=%q", resolver.gotSystemID, resolver.gotInterface)
	}
}

func TestSelectMachineIPForDNSErrorsWithoutSystemIDWhenInterfaceConfigured(t *testing.T) {
	m := &infrav1beta1.MaasMachine{}

	_, err := selectMachineIPForDNS(m, "eno2", &fakeMachineIPResolver{})
	if err == nil {
		t.Fatal("expected error when systemID is missing")
	}
}

func TestSelectMachineIPForDNSPropagatesResolverError(t *testing.T) {
	systemID := "abc123"
	m := &infrav1beta1.MaasMachine{
		Spec: infrav1beta1.MaasMachineSpec{
			SystemID: &systemID,
		},
	}
	expectedErr := errors.New("resolver failed")
	resolver := &fakeMachineIPResolver{err: expectedErr}

	_, err := selectMachineIPForDNS(m, "eno2", resolver)
	if err == nil {
		t.Fatal("expected resolver error")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected wrapped resolver error, got %v", err)
	}
}
