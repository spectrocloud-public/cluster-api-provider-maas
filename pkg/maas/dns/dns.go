package dns

import (
	"context"
	"slices"
	"sort"

	"github.com/pkg/errors"
	infrainfrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/maas-client-go/maasclient"
	"k8s.io/apimachinery/pkg/util/sets"
)

type Service struct {
	scope      *scope.ClusterScope
	maasClient maasclient.ClientSetInterface
}

var ErrNotFound = errors.New("resource not found")

// DNS service returns a new helper for managing a MaaS "DNS" (DNS client loadbalancing)
func NewService(clusterScope *scope.ClusterScope) *Service {
	return &Service{
		scope:      clusterScope,
		maasClient: scope.NewMaasClient(clusterScope),
	}
}

// ReconcileDNS reconciles the load balancers for the given cluster.
func (s *Service) ReconcileDNS() error {
	s.scope.V(2).Info("Reconciling DNS")
	ctx := context.TODO()

	dnsResource, err := s.GetDNSResource()
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	dnsName := s.scope.GetDNSName()

	if dnsResource == nil {
		if _, err = s.maasClient.DNSResources().
			Builder().
			WithFQDN(s.scope.GetDNSName()).
			WithAddressTTL("10").
			WithIPAddresses(nil).
			Create(ctx); err != nil {
			return errors.Wrapf(err, "Unable to create DNS Resources")
		}
	}

	s.scope.SetDNSName(dnsName)

	return nil
}

// UpdateAttachments reconciles the load balancers for the given cluster.
func (s *Service) UpdateDNSAttachments(IPs []string) error {
	s.scope.V(2).Info("Updating DNS Attachments")
	ctx := context.TODO()
	// get ID of loadbalancer
	dnsResource, err := s.GetDNSResource()
	if err != nil {
		return err
	}

	if _, err = dnsResource.Modifier().SetIPAddresses(IPs).Modify(ctx); err != nil {
		return errors.Wrap(err, "Unable to update IPs")
	}

	return nil
}

// TODO do at some point
//func MachineIsRunning(m *infrainfrav1beta1.MaasMachine) bool {
//	if !m.Status.MachinePowered {
//		return false
//	}
//
//	//allMachinePodConditions := []clusterv1.ConditionType{
//	//	controlplanev1.MachineAPIServerPodHealthyCondition,
//	//	controlplanev1.MachineControllerManagerPodHealthyCondition,
//	//	controlplanev1.MachineSchedulerPodHealthyCondition,
//	//}
//	//if controlPlane.IsEtcdManaged() {
//	//	allMachinePodConditions = append(allMachinePodConditions, controlplanev1.MachineEtcdPodHealthyCondition)
//	//}
//
//}

// InstanceIsRegisteredWithAPIServerELB returns true if the instance is already registered with the APIServer ELB.
func (s *Service) MachineIsRegisteredWithAPIServerDNS(i *infrainfrav1beta1.Machine) (bool, error) {
	ips, err := s.GetAPIServerDNSRecords()
	if err != nil {
		return false, err
	}

	for _, mAddress := range i.Addresses {
		if ips.Has(mAddress.Address) {
			return true, nil
		}
	}

	return false, nil
}

func (s *Service) GetAPIServerDNSRecords() (sets.String, error) {
	dnsResource, err := s.GetDNSResource()
	if err != nil {
		return nil, err
	}

	ips := sets.NewString()
	for _, address := range dnsResource.IPAddresses() {
		if address.IP().String() != "" {
			ips.Insert(address.IP().String())
		}
	}

	return ips, nil
}

// GetMachineIPForInterfaceTag returns a machine IP from the interface that
// carries the provided MAAS tag. Returns an explicit error if multiple
// interfaces share the same tag, to prevent ambiguous DNS entries.
func (s *Service) GetMachineIPForInterfaceTag(systemID, interfaceTag string) (string, error) {
	if systemID == "" {
		return "", errors.New("systemID is required")
	}
	if interfaceTag == "" {
		return "", errors.New("interfaceTag is required")
	}

	ctx := context.TODO()
	interfaces, err := s.maasClient.NetworkInterfaces().Get(ctx, systemID)
	if err != nil {
		return "", errors.Wrapf(err, "failed to list interfaces for machine %q", systemID)
	}

	var matched []maasclient.NetworkInterface
	for _, iface := range interfaces {
		if iface != nil && slices.Contains(iface.Tags(), interfaceTag) {
			matched = append(matched, iface)
		}
	}

	if len(matched) == 0 {
		return "", errors.Errorf("no interface with tag %q found for machine %q", interfaceTag, systemID)
	}

	if len(matched) > 1 {
		names := make([]string, len(matched))
		for i, iface := range matched {
			names[i] = iface.Name()
		}
		return "", errors.Errorf("tag %q is assigned to multiple interfaces %v on machine %q; each tag must be unique per machine", interfaceTag, names, systemID)
	}

	iface := matched[0]

	// Build the list of candidate interfaces to check for IPs.
	// When a physical NIC is bridged, MAAS moves its links to the bridge child,
	// leaving the NIC with empty Links(). Follow Children() to handle that case.
	candidates := []maasclient.NetworkInterface{iface}
	for _, childName := range iface.Children() {
		for _, candidate := range interfaces {
			if candidate != nil && candidate.Name() == childName {
				candidates = append(candidates, candidate)
				break
			}
		}
	}

	for _, candidate := range candidates {
		var ipv4s []string
		for _, link := range candidate.Links() {
			if link == nil {
				continue
			}
			if ip := link.IPAddress(); ip != nil && ip.To4() != nil {
				ipv4s = append(ipv4s, ip.String())
			}
		}
		if len(ipv4s) > 0 {
			sort.Strings(ipv4s)
			return ipv4s[0], nil
		}
	}

	return "", errors.Errorf("no IPv4 address found on interface %q or its children (tag %q) for machine %q", iface.Name(), interfaceTag, systemID)
}

func (s *Service) GetDNSResource() (maasclient.DNSResource, error) {
	dnsName := s.scope.GetDNSName()
	if dnsName == "" {
		return nil, errors.New("No DNS on the cluster set!")
	}

	d, err := s.maasClient.DNSResources().
		List(context.Background(),
			maasclient.ParamsBuilder().Set(maasclient.FQDNKey, dnsName))
	if err != nil {
		return nil, errors.Wrapf(err, "error retrieving dns resources %q", dnsName)
	} else if len(d) > 1 {
		return nil, errors.Errorf("expected 1 DNS Resource for %q, got %d", dnsName, len(d))
	} else if len(d) == 0 {
		return nil, ErrNotFound
	}

	return d[0], nil
}
