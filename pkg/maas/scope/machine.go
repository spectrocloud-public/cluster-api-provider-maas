/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scope

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	infrautil "github.com/spectrocloud/cluster-api-provider-maas/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MachineScopeParams defines the input parameters used to create a new Scope.
type MachineScopeParams struct {
	Client         client.Client
	Logger         logr.Logger
	Cluster        *clusterv1.Cluster
	ClusterScope   *ClusterScope
	Machine        *clusterv1.Machine
	MaasMachine    *infrav1beta1.MaasMachine
	ControllerName string

	Tracker *remote.ClusterCacheTracker
}

// MachineScope defines the basic context for an actuator to operate upon.
type MachineScope struct {
	logr.Logger
	client      client.Client
	patchHelper *patch.Helper

	Cluster      *clusterv1.Cluster
	ClusterScope *ClusterScope

	Machine     *clusterv1.Machine
	MaasMachine *infrav1beta1.MaasMachine

	controllerName string
	tracker        *remote.ClusterCacheTracker
}

// NewMachineScope creates a new Scope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewMachineScope(params MachineScopeParams) (*MachineScope, error) {

	helper, err := patch.NewHelper(params.MaasMachine, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}
	return &MachineScope{
		Logger:         params.Logger,
		Machine:        params.Machine,
		MaasMachine:    params.MaasMachine,
		Cluster:        params.Cluster,
		ClusterScope:   params.ClusterScope,
		patchHelper:    helper,
		client:         params.Client,
		tracker:        params.Tracker,
		controllerName: params.ControllerName,
	}, nil
}

// PatchObject persists the machine configuration and status.
func (m *MachineScope) PatchObject() error {

	applicableConditions := []clusterv1.ConditionType{
		infrav1beta1.MachineDeployedCondition,
	}

	if m.IsControlPlane() {
		applicableConditions = append(applicableConditions, infrav1beta1.DNSAttachedCondition)
	}
	// Always update the readyCondition by summarizing the state of other conditions.
	// A step counter is added to represent progress during the provisioning process (instead we are hiding it during the deletion process).
	conditions.SetSummary(m.MaasMachine,
		conditions.WithConditions(applicableConditions...),
		conditions.WithStepCounterIf(m.MaasMachine.ObjectMeta.DeletionTimestamp.IsZero()),
	)

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return m.patchHelper.Patch(
		context.TODO(),
		m.MaasMachine,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
			infrav1beta1.MachineDeployedCondition,
		}},
	)
}

// Close closes the current scope persisting the cluster configuration and status.
func (m *MachineScope) Close() error {
	return m.PatchObject()
}

// SetAddresses sets the MAAS Machine address status.
func (m *MachineScope) SetAddresses(addrs []clusterv1.MachineAddress) {
	m.MaasMachine.Status.Addresses = addrs
}

// SetReady sets the MaasMachine Ready Status
func (m *MachineScope) SetReady() {
	m.MaasMachine.Status.Ready = true
}

// IsReady gets MaasMachine Ready Status
func (m *MachineScope) IsReady() bool {
	return m.MaasMachine.Status.Ready
}

// SetNotReady sets the MaasMachine Ready Status to false
func (m *MachineScope) SetNotReady() {
	m.MaasMachine.Status.Ready = false
}

// SetFailureMessage sets the MaasMachine status failure message.
func (m *MachineScope) SetFailureMessage(v error) {
	m.MaasMachine.Status.FailureMessage = pointer.StringPtr(v.Error())
}

// SetFailureReason sets the MaasMachine status failure reason.
func (m *MachineScope) SetFailureReason(v capierrors.MachineStatusError) {
	m.MaasMachine.Status.FailureReason = &v
}

// IsControlPlane returns true if the machine is a control plane.
func (m *MachineScope) IsControlPlane() bool {
	return util.IsControlPlaneMachine(m.Machine)
}

// Role returns the machine role from the labels.
func (m *MachineScope) Role() string {
	if util.IsControlPlaneMachine(m.Machine) {
		return "control-plane"
	}
	return "node"
}

// GetInstanceID returns the MaasMachine instance id by parsing Spec.ProviderID.
func (m *MachineScope) GetInstanceID() *string {
	parsed, err := infrautil.NewProviderID(m.GetProviderID())
	if err != nil {
		m.Error(err, "failed to parse providerID", "providerID", m.GetProviderID())
		return nil
	}
	return pointer.StringPtr(parsed.ID())
}

// GetProviderID returns the MaasMachine providerID from the spec.
func (m *MachineScope) GetProviderID() string {
	if m.MaasMachine.Spec.ProviderID != nil {
		return *m.MaasMachine.Spec.ProviderID
	}
	return ""
}

// SetProviderID sets the MaasMachine providerID in spec.
func (m *MachineScope) SetProviderID(systemID, availabilityZone string) {
	providerID := fmt.Sprintf("maas:///%s/%s", availabilityZone, systemID)
	m.MaasMachine.Spec.ProviderID = pointer.StringPtr(providerID)
}

// SetFailureDomain sets the MaasMachine systemID in spec.
func (m *MachineScope) SetFailureDomain(availabilityZone string) {
	m.MaasMachine.Spec.FailureDomain = pointer.StringPtr(availabilityZone)
}

// SetInstanceID sets the MaasMachine systemID in spec.
func (m *MachineScope) SetSystemID(systemID string) {
	m.MaasMachine.Spec.SystemID = pointer.StringPtr(systemID)
}

func (m *MachineScope) GetSystemID() string {
	return *m.MaasMachine.Spec.SystemID
}

// GetMachineState returns the MaasMachine instance state from the status.
func (m *MachineScope) GetMachineState() *infrav1beta1.MachineState {
	return m.MaasMachine.Status.MachineState
}

// SetMachineState sets the MaasMachine status instance state.
func (m *MachineScope) SetMachineState(v infrav1beta1.MachineState) {
	m.MaasMachine.Status.MachineState = &v
}
func (m *MachineScope) SetPowered(powered bool) {
	m.MaasMachine.Status.MachinePowered = powered
}

// GetMachineHostname retrns the hostname
func (m *MachineScope) GetMachineHostname() string {
	if m.MaasMachine.Status.Hostname != nil {
		return *m.MaasMachine.Status.Hostname
	}
	return ""
}

// SetMachineHostname sets the hostname
func (m *MachineScope) SetMachineHostname(hostname string) {
	m.MaasMachine.Status.Hostname = &hostname
}

func (m *MachineScope) MachineIsRunning() bool {
	state := m.GetMachineState()
	return state != nil && infrav1beta1.MachineRunningStates.Has(string(*state))
}

func (m *MachineScope) MachineIsOperational() bool {
	state := m.GetMachineState()
	return state != nil && infrav1beta1.MachineOperationalStates.Has(string(*state))
}

func (m *MachineScope) MachineIsInKnownState() bool {
	state := m.GetMachineState()
	return state != nil && infrav1beta1.MachineKnownStates.Has(string(*state))
}

// GetRawBootstrapData returns the bootstrap data from the secret in the Machine's bootstrap.dataSecretName.
func (m *MachineScope) GetRawBootstrapData() ([]byte, error) {
	if m.Machine.Spec.Bootstrap.DataSecretName == nil {
		return nil, errors.New("error retrieving bootstrap data: linked Machine's bootstrap.dataSecretName is nil")
	}

	namespace := m.Machine.Namespace

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: *m.Machine.Spec.Bootstrap.DataSecretName}
	if err := m.client.Get(context.TODO(), key, secret); err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve bootstrap data secret for MaasMachine %s/%s", namespace, m.Machine.Name)
	}

	value, ok := secret.Data["value"]
	if !ok {
		return nil, errors.New("error retrieving bootstrap data: secret value key is missing")
	}

	return value, nil
}

// findNodeByHostname attempts to find a node by trying different hostname variations
// It tries both the exact hostname and hostname with FQDN stripped to handle
// different node naming conventions across different Kubernetes setups
func (m *MachineScope) findNodeByHostname(ctx context.Context, remoteClient client.Client, hostname string) (*corev1.Node, error) {
	if hostname == "" {
		return nil, errors.New("hostname cannot be empty")
	}

	node := &corev1.Node{}

	// First try with the exact hostname (lowercased)
	nodeName := strings.ToLower(hostname)
	err := remoteClient.Get(ctx, client.ObjectKey{Name: nodeName}, node)
	if err == nil {
		return node, nil
	}

	// If that fails and the hostname contains a dot (potential FQDN), try with just the hostname part
	if strings.Contains(hostname, ".") {
		shortHostname := strings.ToLower(strings.Split(hostname, ".")[0])
		if shortHostname != nodeName { // Avoid duplicate attempt
			err = remoteClient.Get(ctx, client.ObjectKey{Name: shortHostname}, node)
			if err == nil {
				return node, nil
			}
		}
	}

	// If hostname doesn't contain a dot, try common FQDN patterns by listing nodes
	// and matching against hostname prefixes
	nodeList := &corev1.NodeList{}
	if listErr := remoteClient.List(ctx, nodeList); listErr == nil {
		targetPrefix := strings.ToLower(hostname)
		for _, n := range nodeList.Items {
			nodeHostname := strings.ToLower(n.Name)
			// Check if node name starts with our hostname (e.g., "node1" matches "node1.example.com")
			if strings.HasPrefix(nodeHostname, targetPrefix+".") {
				return &n, nil
			}
		}
	}

	// Return the original error if all attempts failed
	return nil, err
}

// SetNodeProviderID patches the node with the ID
func (m *MachineScope) SetNodeProviderID() error {
	ctx := context.TODO()
	remoteClient, err := m.tracker.GetClient(ctx, util.ObjectKey(m.Cluster))
	if err != nil {
		return err
	}

	node, err := m.findNodeByHostname(ctx, remoteClient, m.GetMachineHostname())
	if err != nil {
		return errors.Wrapf(err, "failed to find node for hostname %s", m.GetMachineHostname())
	}

	providerID := m.GetProviderID()
	if node.Spec.ProviderID == providerID {
		return nil
	}

	patchHelper, err := patch.NewHelper(node, remoteClient)
	if err != nil {
		return err
	}

	node.Spec.ProviderID = providerID

	return patchHelper.Patch(ctx, node)
}

// GetDynamicLXD returns whether this machine should be created as an LXD VM (driven by higher-level policy).
func (m *MachineScope) GetDynamicLXD() bool {
	if m.MaasMachine.Spec.LXD != nil && m.MaasMachine.Spec.LXD.Enabled != nil {
		return *m.MaasMachine.Spec.LXD.Enabled
	}
	return false
}

// GetStaticIP returns the static IP to assign to the machine, if configured
func (m *MachineScope) GetStaticIP() string {
	if m.MaasMachine.Spec.StaticIP == nil {
		m.Info("StaticIP config is nil")
		return ""
	}

	m.Info("StaticIP config found", "ip", m.MaasMachine.Spec.StaticIP.IP)
	return m.MaasMachine.Spec.StaticIP.IP
}

// GetStaticIPConfig returns the full static IP configuration if configured
func (m *MachineScope) GetStaticIPConfig() *infrav1beta1.StaticIPConfig {
	if m.MaasMachine.Spec.StaticIP == nil {
		m.Info("StaticIPConfig: StaticIP config is nil")
		return nil
	}

	m.Info("StaticIPConfig: returning config", "ip", m.MaasMachine.Spec.StaticIP.IP)
	return m.MaasMachine.Spec.StaticIP
}
