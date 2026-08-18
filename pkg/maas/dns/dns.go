package dns

import (
	"context"
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

// GetMachineIPForInterface returns a machine IP that belongs to the provided
// interface name.
func (s *Service) GetMachineIPForInterface(systemID, interfaceName string) (string, error) {
	if systemID == "" {
		return "", errors.New("systemID is required")
	}
	if interfaceName == "" {
		return "", errors.New("interfaceName is required")
	}

	ctx := context.TODO()
	interfaces, err := s.maasClient.NetworkInterfaces().Get(ctx, systemID)
	if err != nil {
		return "", errors.Wrapf(err, "failed to list interfaces for machine %q", systemID)
	}

	for _, iface := range interfaces {
		if iface == nil || iface.Name() != interfaceName {
			continue
		}

		for _, link := range iface.Links() {
			if link == nil || link.IPAddress() == nil {
				continue
			}
			if ip := link.IPAddress().String(); ip != "" && ip != "<nil>" {
				return ip, nil
			}
		}

		return "", errors.Errorf("no IP found on interface %q for machine %q", interfaceName, systemID)
	}

	return "", errors.Errorf("interface %q not found for machine %q", interfaceName, systemID)
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
