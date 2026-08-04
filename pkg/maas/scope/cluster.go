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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	infrautil "github.com/spectrocloud/cluster-api-provider-maas/pkg/util"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

const (
	DnsSuffixLength = 6
)

// ClusterScopeParams defines the input parameters used to create a new Scope.
type ClusterScopeParams struct {
	Client              client.Client
	Logger              logr.Logger
	Cluster             *clusterv1.Cluster
	MaasCluster         *infrav1beta1.MaasCluster
	ControllerName      string
	Tracker             clustercache.ClusterCache
	ClusterEventChannel chan event.GenericEvent
}

// ClusterScope defines the basic context for an actuator to operate upon.
type ClusterScope struct {
	logr.Logger
	client      client.Client
	patchHelper *patch.Helper

	Cluster             *clusterv1.Cluster
	MaasCluster         *infrav1beta1.MaasCluster
	controllerName      string
	tracker             clustercache.ClusterCache
	clusterEventChannel chan event.GenericEvent
}

// NewClusterScope creates a new Scope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewClusterScope(params ClusterScopeParams) (*ClusterScope, error) {

	helper, err := patch.NewHelper(params.MaasCluster, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}
	return &ClusterScope{
		Logger:              params.Logger,
		client:              params.Client,
		Cluster:             params.Cluster,
		MaasCluster:         params.MaasCluster,
		patchHelper:         helper,
		controllerName:      params.ControllerName,
		tracker:             params.Tracker,
		clusterEventChannel: params.ClusterEventChannel,
	}, nil
}

// Client returns the Kubernetes client
func (s *ClusterScope) Client() client.Client {
	return s.client
}

// PatchObject persists the cluster configuration and status.
func (s *ClusterScope) PatchObject() error {
	// Always update the readyCondition by summarizing the state of other conditions.
	// A step counter is added to represent progress during the provisioning process (instead we are hiding it during the deletion process).
	conditions.SetSummary(s.MaasCluster,
		conditions.WithConditions(
			infrav1beta1.DNSReadyCondition,
			infrav1beta1.APIServerAvailableCondition,
		),
		conditions.WithStepCounterIf(s.MaasCluster.ObjectMeta.DeletionTimestamp.IsZero()),
	)

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return s.patchHelper.Patch(
		context.TODO(),
		s.MaasCluster,
		patch.WithOwnedConditions{Conditions: []string{
			string(clusterv1.ReadyCondition),
			string(infrav1beta1.DNSReadyCondition),
			string(infrav1beta1.APIServerAvailableCondition),
		}},
	)
}

// Close closes the current scope persisting the cluster configuration and status.
func (s *ClusterScope) Close() error {
	return s.PatchObject()
}

// APIServerPort returns the APIServerPort to use when creating the load balancer.
func (s *ClusterScope) APIServerPort() int {
	if infrautil.IsCustomEndpointPresent(s.MaasCluster.GetAnnotations()) && s.MaasCluster.Spec.ControlPlaneEndpoint.Port != 0 {
		return s.MaasCluster.Spec.ControlPlaneEndpoint.Port
	}

	if s.Cluster.Spec.ClusterNetwork.APIServerPort != 0 {
		return int(s.Cluster.Spec.ClusterNetwork.APIServerPort)
	}
	return 6443
}

// SetDNSName sets the Network systemID in spec.
func (s *ClusterScope) SetDNSName(dnsName string) {
	s.MaasCluster.Status.Network.DNSName = dnsName
}

// GetDNSName sets the Network systemID in spec.
// This can't do a lookup on Status.Network.DNSDomain name since it's derviced from here
func (s *ClusterScope) GetDNSName() string {
	if infrautil.IsCustomEndpointPresent(s.MaasCluster.GetAnnotations()) {
		s.SetDNSName(s.MaasCluster.Spec.ControlPlaneEndpoint.Host)
		return s.MaasCluster.Spec.ControlPlaneEndpoint.Host
	}

	if !s.Cluster.Spec.ControlPlaneEndpoint.IsZero() {
		return s.Cluster.Spec.ControlPlaneEndpoint.Host
	}

	if s.MaasCluster.Status.Network.DNSName != "" {
		return s.MaasCluster.Status.Network.DNSName
	}

	uid := uuid.New().String()
	dnsName := fmt.Sprintf("%s-%s.%s", s.Cluster.Name, uid[len(uid)-DnsSuffixLength:], s.MaasCluster.Spec.DNSDomain)

	s.SetDNSName(dnsName)
	return dnsName
}

// GetActiveMaasMachines all MaaS machines NOT being deleted
func (s *ClusterScope) GetClusterMaasMachines() ([]*infrav1beta1.MaasMachine, error) {

	machineList := &infrav1beta1.MaasMachineList{}
	labels := map[string]string{clusterv1.ClusterNameLabel: s.Cluster.Name}

	if err := s.client.List(
		context.TODO(),
		machineList,
		client.InNamespace(s.Cluster.Namespace),
		client.MatchingLabels(labels)); err != nil {
		return nil, errors.Wrap(err, "failed to list machines")
	}

	var machines []*infrav1beta1.MaasMachine
	for i := range machineList.Items {
		m := &machineList.Items[i]
		machines = append(machines, m)
		// TODO need active?
		//if m.DeletionTimestamp.IsZero() {
		//}
	}

	return machines, nil
}

// GetControlPlaneMaasMachines returns all MaasMachine objects associated with control plane machines in the cluster
func (s *ClusterScope) GetControlPlaneMaasMachines() ([]*infrav1beta1.MaasMachine, error) {
	machines, err := s.GetClusterMaasMachines()
	if err != nil {
		return nil, err
	}

	var cpMachines []*infrav1beta1.MaasMachine
	for _, machine := range machines {
		// Check for control plane label
		if _, ok := machine.ObjectMeta.Labels[clusterv1.MachineControlPlaneLabel]; ok {
			cpMachines = append(cpMachines, machine)
		}
	}

	return cpMachines, nil
}

// SetStatus sets the MaasCluster status
func (s *ClusterScope) SetStatus(status infrav1beta1.MaasClusterStatus) {
	s.MaasCluster.Status = status
}

// GetMaasClientIdentity returns the MAAS client identity
func (s *ClusterScope) GetMaasClientIdentity() ClientIdentity {
	// Try to get MAAS credentials from a secret
	// The secret is expected to be in the same namespace as the MaasCluster
	// and named "maas-credentials" by default
	// Secret containing MAAS endpoint/token created by Palette bootstrapper
	// Default name switched from "maas-credentials" to "capmaas-manager-bootstrap-credentials"
	secretName := "capmaas-manager-bootstrap-credentials"

	// Get the secret
	secret := &corev1.Secret{}
	key := types.NamespacedName{
		Namespace: s.MaasCluster.Namespace,
		Name:      secretName,
	}

	// Try to get the secret
	err := s.client.Get(context.Background(), key, secret)
	if err != nil {
		// If the secret doesn't exist, fall back to environment variables or default values
		s.Info("Failed to get MAAS bootstrap credentials secret, using fallback values", "error", err)
		return ClientIdentity{
			URL:   getEnvOrDefault("MAAS_API_URL", "http://localhost:5240/MAAS"),
			Token: getEnvOrDefault("MAAS_API_TOKEN", "dummy-token"),
		}
	}

	// Get the credentials from the secret
	url := string(secret.Data["MAAS_ENDPOINT"])
	token := string(secret.Data["MAAS_API_KEY"])

	// Validate the credentials
	if url == "" || token == "" {
		s.Info("Invalid MAAS credentials in secret, using fallback values")
		return ClientIdentity{
			URL:   getEnvOrDefault("MAAS_API_URL", "http://localhost:5240/MAAS"),
			Token: getEnvOrDefault("MAAS_API_TOKEN", "dummy-token"),
		}
	}

	return ClientIdentity{
		URL:   url,
		Token: token,
	}
}

// getEnvOrDefault returns the value of the environment variable or the default value if not set
func getEnvOrDefault(key, defaultValue string) string {
	value := os.Getenv(key)
	if value != "" {
		return value
	}
	return defaultValue
}

// ClientIdentity contains MAAS client identity information
type ClientIdentity struct {
	URL   string
	Token string
}

const (
	maasPreferredSubnetConfigmap = "maas-preferred-subnet"
	preferredSubnetKey           = "preferredSubnets"
)

func (s *ClusterScope) GetPreferredSubnets() ([]string, error) {
	maasPreferredSubnet := &corev1.ConfigMap{}
	err := s.client.Get(context.Background(), types.NamespacedName{
		Namespace: s.Cluster.GetNamespace(),
		Name:      maasPreferredSubnetConfigmap,
	}, maasPreferredSubnet)
	switch {
	case err != nil && !apierrors.IsNotFound(err):
		return nil, err
	case err != nil && apierrors.IsNotFound(err):
		return nil, nil
	}

	subnetsString := maasPreferredSubnet.Data[preferredSubnetKey]
	var result []string
	for _, subnet := range strings.Split(subnetsString, ",") {
		result = append(result, strings.TrimSpace(subnet))
	}
	return result, nil
}

var (
	// apiServerTriggers is used to prevent multiple goroutines for a single
	// Cluster that poll to see if the target API server is online.
	apiServerTriggers   = map[types.UID]struct{}{}
	apiServerTriggersMu sync.Mutex
)

func (s *ClusterScope) ReconcileMaasClusterWhenAPIServerIsOnline() {
	if ptr.Deref(s.Cluster.Status.Initialization.ControlPlaneInitialized, false) {
		s.Info("skipping reconcile when API server is online",
			"reason", "ControlPlaneInitialized")
		return
	} else if !s.Cluster.DeletionTimestamp.IsZero() {
		s.Info("skipping reconcile when API server is online",
			"reason", "controlPlaneDeleting")
		return
	}

	apiServerTriggersMu.Lock()
	defer apiServerTriggersMu.Unlock()
	if _, ok := apiServerTriggers[s.Cluster.UID]; ok {
		s.Info("skipping reconcile when API server is online",
			"reason", "alreadyPolling")
		return
	}
	apiServerTriggers[s.Cluster.UID] = struct{}{}
	go func() {
		// Block until the target API server is online.

		s.Info("start polling API server for online check")
		_ = wait.PollImmediateInfinite(time.Second*1, func() (bool, error) { return s.IsAPIServerOnline() })
		s.Info("stop polling API server for online check")
		s.Info("triggering GenericEvent", "reason", "api-server-online")
		s.clusterEventChannel <- event.GenericEvent{
			Object: s.MaasCluster,
		}

		apiServerTriggersMu.Lock()
		delete(apiServerTriggers, s.Cluster.UID)
		apiServerTriggersMu.Unlock()

		//// Once the control plane has been marked as initialized it is safe to
		//// remove the key from the map that prevents multiple goroutines from
		//// polling the API server to see if it is online.
		//s.Info("start polling for control plane initialized")
		//wait.PollImmediateInfinite(time.Second*1, func() (bool, error) { return r.isControlPlaneInitialized(ctx), nil }) // nolint:errcheck
		//s.Info("stop polling for control plane initialized")
	}()
}

func (s *ClusterScope) IsAPIServerOnline() (bool, error) {

	ctx := context.TODO()

	cluster := &clusterv1.Cluster{}
	if err := s.client.Get(ctx, util.ObjectKey(s.Cluster), cluster); err != nil {
		return false, err
	} else if !cluster.DeletionTimestamp.IsZero() {
		s.Info("Cluster is deleting; abort APIServerOnline check", "cluster", cluster.Name)
		return false, errors.New("Cluster is deleting; abort IsAPIServerOnline")
	}

	remoteClient, err := s.tracker.GetClient(ctx, util.ObjectKey(s.Cluster))
	if err != nil {
		s.V(2).Info("Waiting for online server to come online")
		return false, nil
	}

	err = remoteClient.List(ctx, new(corev1.NodeList))

	return err == nil, nil
}

// IsCustomEndpoint returns true if the cluster has a custom endpoint
func (s *ClusterScope) IsCustomEndpoint() bool {
	if infrautil.IsCustomEndpointPresent(s.MaasCluster.GetAnnotations()) {
		s.GetDNSName()
		s.V(0).Info("custom dns is provided skipping dns reconcile", "dns", s.GetDNSName())
		return true
	}
	return false
}

// IsLXDHostEnabled returns true if LXD host registration is enabled for the infrastructure cluster.
// Prefer the explicit flag on LXDConfig.
func (s *ClusterScope) IsLXDHostEnabled() bool {
	// New preferred switch
	if s.MaasCluster.Spec.LXDConfig != nil && s.MaasCluster.Spec.LXDConfig.Enabled != nil {
		return *s.MaasCluster.Spec.LXDConfig.Enabled
	}

	return false
}

// GetLXDConfig returns the LXD configuration
func (s *ClusterScope) GetLXDConfig() *infrav1beta1.LXDConfig {
	return s.MaasCluster.Spec.LXDConfig
}

// GetWorkloadClusterClient returns a client for the workload cluster
func (s *ClusterScope) GetWorkloadClusterClient(ctx context.Context) (client.Client, error) {
	return s.tracker.GetClient(ctx, util.ObjectKey(s.Cluster))
}

// EnsureClusterReadinessLabel sets the readiness label on the target cluster when API server is available
func (s *ClusterScope) EnsureClusterReadinessLabel() error {
	ctx := context.TODO()

	// Get remote client for target cluster
	remoteClient, err := s.tracker.GetClient(ctx, util.ObjectKey(s.Cluster))
	if err != nil {
		// Remote cluster not accessible yet
		s.V(2).Info("Cannot get remote client to set readiness label")
		return nil // Not an error, just not ready yet
	}

	// Check if label is already set
	namespace := &corev1.Namespace{}
	namespaceName := types.NamespacedName{Name: "kube-system"}

	if err := remoteClient.Get(ctx, namespaceName, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			s.V(2).Info("kube-system namespace not found in target cluster")
			return nil
		}
		return err
	}

	if labelValue, exists := namespace.Labels[infrautil.APIServerReadinessLabel]; exists && labelValue == "true" {
		s.V(2).Info("API server readiness label already set on kube-system namespace")
		return nil
	}

	// Set the readiness label immediately since API server is available
	if namespace.Labels == nil {
		namespace.Labels = make(map[string]string)
	}
	namespace.Labels[infrautil.APIServerReadinessLabel] = "true"

	if err := remoteClient.Update(ctx, namespace); err != nil {
		return fmt.Errorf("failed to set API server readiness label on kube-system namespace: %w", err)
	}

	s.Info("Successfully set API server readiness label", "label", infrautil.APIServerReadinessLabel)
	return nil
}
