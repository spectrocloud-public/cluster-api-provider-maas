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

package controllers

import (
	"context"
	"fmt"
	"net"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/dns"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/lxd"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	infrautil "github.com/spectrocloud/cluster-api-provider-maas/pkg/util"
	"github.com/spectrocloud/maas-client-go/maasclient"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

// dnsServicer is the subset of dns.Service used by reconcileDNSAttachments.
// Extracted as an interface to allow unit testing without a live MAAS endpoint.
type dnsServicer interface {
	GetDNSResource() (maasclient.DNSResource, error)
	UpdateDNSAttachmentsWithResource(maasclient.DNSResource, []string) (bool, error)
	IsDriftDetected(maasclient.DNSResource, []string) bool
}

const lastAppliedAnn = "infrastructure.cluster.x-k8s.io/last-applied-dns-hash"

// MaasClusterReconciler reconciles a MaasCluster object
type MaasClusterReconciler struct {
	client.Client
	Log                 logr.Logger
	Scheme              *runtime.Scheme
	Recorder            record.EventRecorder
	GenericEventChannel chan event.GenericEvent
	Tracker             *remote.ClusterCacheTracker
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=maasclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=maasclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;patch;update

// Reconcile reads that state of the cluster for a MaasCluster object and makes changes based on the state read
// and what is in the MaasCluster.Spec
func (r *MaasClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	log := r.Log.WithValues("maascluster", req.Name)

	// Fetch the MaasCluster instance
	maasCluster := &infrav1beta1.MaasCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, maasCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, maasCluster.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("Waiting for Cluster Controller to set OwnerRef on MaasCluster")
		return ctrl.Result{}, nil
	}

	// Create the scope.
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:              r.Client,
		Logger:              log,
		Cluster:             cluster,
		MaasCluster:         maasCluster,
		ClusterEventChannel: r.GenericEventChannel,
		ControllerName:      "maascluster",
		Tracker:             r.Tracker,
	})
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to create scope: %+v", err)
	}

	// Always close the scope when exiting this function so we can persist any MAAS Cluster changes.
	defer func() {
		if err := clusterScope.Close(); err != nil && rerr == nil {
			rerr = err
		}
	}()

	// Support FailureDomains
	// In cloud providers this would likely look up which failure domains are supported and set the status appropriately.
	// so kCP will distribute the CPs across multiple failure domains
	failureDomains := []clusterv1.FailureDomain{}
	for _, az := range maasCluster.Spec.FailureDomains {
		failureDomains = append(failureDomains, clusterv1.FailureDomain{
			Name:         az,
			ControlPlane: ptr.To(true),
		})
	}
	maasCluster.Status.FailureDomains = failureDomains

	// Handle deleted clusters
	if !maasCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, clusterScope)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, clusterScope)
}

func (r *MaasClusterReconciler) reconcileDelete(ctx context.Context, clusterScope *scope.ClusterScope) (ctrl.Result, error) {
	clusterScope.Info("Reconciling MaasCluster delete")

	maasCluster := clusterScope.MaasCluster

	maasMachines, err := infrautil.GetMAASMachinesInCluster(ctx, r.Client, clusterScope.Cluster.Namespace, clusterScope.Cluster.Name)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err,
			"unable to list MAASMachines part of MAASCluster %s/%s", clusterScope.Cluster.Namespace, clusterScope.Cluster.Name)
	}

	if len(maasMachines) > 0 {
		r.Log.Info("Waiting for MAASMachines to be deleted", "count", len(maasMachines))
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Cluster is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(maasCluster, infrav1beta1.ClusterFinalizer)

	// TODO(saamalik) implement the recorder stuff (look at aws)

	return reconcile.Result{}, nil
}

func (r *MaasClusterReconciler) reconcileDNSAttachments(clusterScope *scope.ClusterScope, dnssvc dnsServicer) error {

	if clusterScope.IsCustomEndpoint() {
		return nil
	}

	machines, err := clusterScope.GetClusterMaasMachines()
	if err != nil {
		return errors.Wrapf(err, "Unable to list all maas machines")
	}

	preferredSubnets, err := clusterScope.GetPreferredSubnets()
	if err != nil {
		return errors.Wrapf(err, "unable to find preferred subnets")
	}

	// Build desired IPs first (no MAAS call). Track two separate counts:
	//   existingCPCount — CPs that exist and are NOT being deleted (DeletionTimestamp zero).
	//                     Used to distinguish a transient power-off from a genuine scale-down.
	//   runningCPCount  — CPs that are powered on and running.
	//                     Used to detect preferred-subnet misconfiguration.
	var existingCPCount, runningCPCount int
	var runningIpAddresses []string
	for _, m := range machines {
		if !IsControlPlaneMachine(m) {
			continue
		}
		if m.DeletionTimestamp.IsZero() {
			existingCPCount++
		}
		isRunningHealthy := IsRunning(m)
		if !m.DeletionTimestamp.IsZero() || !isRunningHealthy {
			continue
		}
		runningCPCount++
		machineIP := getExternalMachineIP(clusterScope.Logger, preferredSubnets, m)
		if machineIP != "" {
			runningIpAddresses = append(runningIpAddresses, machineIP)
		}
	}

	if len(runningIpAddresses) == 0 {
		if existingCPCount > 0 {
			// CP objects exist but are transiently unavailable (powered off / IPs not yet
			// assigned / filtered by preferred subnets). Preserve existing DNS records so
			// the cluster remains reachable during the flap; DNS self-heals when CPs recover.
			if runningCPCount > 0 {
				clusterScope.Info("CP machines are running but no IPs match preferred subnets; preserving existing DNS records",
					"runningCPs", runningCPCount)
			} else {
				clusterScope.Info("No running CP machines found; preserving existing DNS records",
					"existingCPs", existingCPCount)
			}
			return nil
		}
		// No CP objects exist or all are pending deletion (scale-down / rolling replacement
		// with no new CPs provisioned yet). Fall through to clear DNS records.
		clusterScope.Info("All CP machines absent or pending deletion; clearing DNS records")
	}

	// Deduplicate before hashing so the annotation always represents the applied IP *set*.
	// updateResourceIPs deduplicates via a set internally; without this step the hash could
	// differ between reconciles if duplicate IPs appear/disappear while MAAS state is unchanged,
	// causing spurious re-syncs.
	runningIpAddresses = dedupStringSlice(runningIpAddresses)

	// Fetch DNS resource once — needed for both drift check and potential update.
	dnsResource, err := dnssvc.GetDNSResource()
	if err != nil {
		return errors.Wrap(err, "Unable to get the dns resource")
	}

	// Early-exit only when the annotation hash matches AND MAAS state agrees with desired IPs.
	// Verifying MAAS state catches external drift (e.g. DNS records manually wiped while the
	// desired set was unchanged, so the hash never changed).
	desiredHash := infrautil.StableHashStringSlice(runningIpAddresses)
	if clusterScope.MaasCluster.Annotations != nil && clusterScope.MaasCluster.Annotations[lastAppliedAnn] == desiredHash {
		if !dnssvc.IsDriftDetected(dnsResource, runningIpAddresses) {
			return nil
		}
		clusterScope.Info("DNS drift detected: MAAS state diverges from last-applied annotation; forcing re-sync",
			"desiredIPs", runningIpAddresses)
	}

	// Use optimized update with in-resource idempotency
	updated, err := dnssvc.UpdateDNSAttachmentsWithResource(dnsResource, runningIpAddresses)
	if err != nil {
		return err
	}

	// Persist last-applied hash (whether MAAS needed update or was already in-sync)
	if clusterScope.MaasCluster.Annotations == nil {
		clusterScope.MaasCluster.Annotations = map[string]string{}
	}
	clusterScope.MaasCluster.Annotations[lastAppliedAnn] = desiredHash

	if updated {
		clusterScope.Info("DNS attachments updated; will continue monitoring for changes")
	}

	return nil
}

// dedupStringSlice returns a new slice with duplicate elements removed, preserving order.
func dedupStringSlice(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// IsControlPlaneMachine checks machine is a control plane node.
func IsControlPlaneMachine(m *infrav1beta1.MaasMachine) bool {
	_, ok := m.ObjectMeta.Labels[clusterv1.MachineControlPlaneLabel]
	return ok
}

// IsRunning returns if the machine is running
func IsRunning(m *infrav1beta1.MaasMachine) bool {
	if !m.Status.MachinePowered {
		return false
	}

	state := m.Status.MachineState
	return state != nil && infrav1beta1.MachineRunningStates.Has(string(*state))
}

func getExternalMachineIP(log logr.Logger, preferredSubnets []string, machine *infrav1beta1.MaasMachine) string {
	for _, i := range machine.Status.Addresses {
		if i.Type == clusterv1.MachineExternalIP && addressInPreferredSubnets(log, preferredSubnets, i.Address) {
			return i.Address
		}
	}
	return ""
}

func addressInPreferredSubnets(log logr.Logger, preferredSubnets []string, address string) bool {
	if len(preferredSubnets) == 0 {
		return true
	}

	for _, subnet := range preferredSubnets {
		_, subnetParsed, err := net.ParseCIDR(subnet)
		if err != nil {
			log.Error(err, "unable to parse subent", "subnet", subnet)
			continue
		}

		addressParsed := net.ParseIP(address)
		if subnetParsed.Contains(addressParsed) {
			return true
		} else {
			continue
		}
	}

	return false
}

func (r *MaasClusterReconciler) reconcileNormal(_ context.Context, clusterScope *scope.ClusterScope) (ctrl.Result, error) {
	clusterScope.Info("Reconciling MaasCluster")

	maasCluster := clusterScope.MaasCluster

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(maasCluster, infrav1beta1.ClusterFinalizer) {
		controllerutil.AddFinalizer(maasCluster, infrav1beta1.ClusterFinalizer)
		return ctrl.Result{}, nil
	}

	dnsService := dns.NewService(clusterScope)

	if err := dnsService.ReconcileDNS(); err != nil {
		clusterScope.Error(err, "failed to reconcile load balancer")
		conditions.MarkFalse(maasCluster, infrav1beta1.DNSReadyCondition, infrav1beta1.DNSFailedReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, err
	}

	if maasCluster.Status.Network.DNSName == "" {
		conditions.MarkFalse(maasCluster, infrav1beta1.DNSReadyCondition, infrav1beta1.WaitForDNSNameReason, clusterv1.ConditionSeverityInfo, "")
		clusterScope.Info("Waiting on API server DNS name")
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}
	

	maasCluster.Spec.ControlPlaneEndpoint = infrav1beta1.APIEndpoint{
		Host: maasCluster.Status.Network.DNSName,
		Port: clusterScope.APIServerPort(),
	}

	maasCluster.Status.Ready = true

	// Mark the maasCluster ready
	conditions.MarkTrue(maasCluster, infrav1beta1.DNSReadyCondition)

	if err := r.reconcileDNSAttachments(clusterScope, dnsService); err != nil {
		if errors.Is(err, ErrRequeueDNS) {
			return ctrl.Result{}, nil
			//return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		clusterScope.Error(err, "failed to reconcile load balancer")
		return reconcile.Result{}, err

	}

	clusterScope.ReconcileMaasClusterWhenAPIServerIsOnline()
	if k, _ := clusterScope.IsAPIServerOnline(); !k {
		conditions.MarkFalse(maasCluster, infrav1beta1.APIServerAvailableCondition, infrav1beta1.APIServerNotReadyReason, clusterv1.ConditionSeverityWarning, "")
		return ctrl.Result{}, nil
	}

	conditions.MarkTrue(maasCluster, infrav1beta1.APIServerAvailableCondition)
	clusterScope.Info("API Server is available")

	// Set API server readiness label on target cluster's kube-system namespace
	if err := r.ensureClusterReadinessLabel(clusterScope); err != nil {
		clusterScope.Error(err, "failed to set API server readiness label")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if clusterScope.IsLXDHostEnabled() {
		// Check readiness label on the TARGET cluster (not management)
		remoteClient, err := clusterScope.GetWorkloadClusterClient(context.TODO())
		if err != nil {
			clusterScope.Info("Target cluster client not available yet; will retry")
			conditions.MarkFalse(maasCluster, infrav1beta1.LXDReadyCondition, infrav1beta1.LXDSetupPendingReason, clusterv1.ConditionSeverityInfo, "Waiting for target cluster client")
			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}
		ns := &corev1.Namespace{}
		if e := remoteClient.Get(context.TODO(), types.NamespacedName{Name: "kube-system"}, ns); e != nil {
			clusterScope.Info("kube-system namespace not found on target; will retry")
			conditions.MarkFalse(maasCluster, infrav1beta1.LXDReadyCondition, infrav1beta1.LXDSetupPendingReason, clusterv1.ConditionSeverityInfo, "Waiting for kube-system on target cluster")
			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if ns.Labels == nil || ns.Labels[infrautil.APIServerReadinessLabel] != "true" {
			clusterScope.Info("Target cluster not ready for DaemonSet deployment - API server readiness label not found")
			conditions.MarkFalse(maasCluster, infrav1beta1.LXDReadyCondition, infrav1beta1.LXDSetupPendingReason, clusterv1.ConditionSeverityInfo, "Waiting for API server readiness label on target cluster")
			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}

		clusterScope.Info("Target cluster ready for DaemonSet deployment")

		// Ensure LXD initializer DaemonSet exists/absent as needed
		if err := r.ensureLXDInitializerDS(context.Background(), clusterScope); err != nil {
			clusterScope.Error(err, "failed to reconcile LXD initializer DaemonSet")
			return reconcile.Result{}, err
		}

		lxdService := lxd.NewService(clusterScope)
		if err := lxdService.ReconcileLXD(); err != nil {
			clusterScope.Error(err, "failed to reconcile LXD hosts")
			conditions.MarkFalse(maasCluster, infrav1beta1.LXDReadyCondition, infrav1beta1.LXDFailedReason, clusterv1.ConditionSeverityError, err.Error())
			return reconcile.Result{}, err
		}
		conditions.MarkTrue(maasCluster, infrav1beta1.LXDReadyCondition)
		clusterScope.Info("LXD hosts are available")
	}

	return ctrl.Result{}, nil
}

// ensureClusterReadinessLabel sets the API server readiness label on the target cluster's kube-system namespace
func (r *MaasClusterReconciler) ensureClusterReadinessLabel(clusterScope *scope.ClusterScope) error {
	return clusterScope.EnsureClusterReadinessLabel()
}

// SetupWithManager will add watches for this controller
func (r *MaasClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	recover := true
	if r.GenericEventChannel == nil {
		r.GenericEventChannel = make(chan event.GenericEvent)
	}

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1beta1.MaasCluster{}).
		WithOptions(controller.Options{
			RecoverPanic: &recover,
		}).
		Watches(
			&infrav1beta1.MaasMachine{},
			handler.EnqueueRequestsFromMapFunc(r.controlPlaneMachineToCluster),
		).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(
				util.ClusterToInfrastructureMapFunc(context.Background(), infrav1beta1.GroupVersion.WithKind("MaasCluster"), mgr.GetClient(), &infrav1beta1.MaasCluster{}),
			),
			//predicates.ClusterUnpaused(mgr.GetScheme(), r.Log),
		).
		WithEventFilter(predicates.ResourceNotPaused(mgr.GetScheme(), r.Log)).
		Build(r)
	if err != nil {
		return err
	}

	if err := c.Watch(
		source.Channel(r.GenericEventChannel, &handler.EnqueueRequestForObject{}),
	); err != nil {
		return err
	}

	return err

}

// controlPlaneMachineToCluster is a handler.ToRequestsFunc to be used
// to enqueue requests for reconciliation for MaasCluster to update
// its status.apiEndpoints field.
func (r *MaasClusterReconciler) controlPlaneMachineToCluster(_ context.Context, o client.Object) []ctrl.Request {
	maasMachine, ok := o.(*infrav1beta1.MaasMachine)
	if !ok {
		r.Log.Error(nil, fmt.Sprintf("expected a MaasMachine but got a %T", o))
		return nil
	}
	if !IsControlPlaneMachine(maasMachine) {
		return nil
	}

	ctx := context.TODO()

	// Fetch the CAPI Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, maasMachine.ObjectMeta)
	if err != nil {
		r.Log.Error(err, "MaasMachine is missing cluster label or cluster does not exist",
			"namespace", maasMachine.Namespace, "name", maasMachine.Name)
		return nil
	}

	// Fetch the MaasCluster
	maasCluster := &infrav1beta1.MaasCluster{}
	maasClusterKey := client.ObjectKey{
		Namespace: maasMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, maasClusterKey, maasCluster); err != nil {
		r.Log.Error(err, "failed to get MaasCluster",
			"namespace", maasClusterKey.Namespace, "name", maasClusterKey.Name)
		return nil
	}

	return []ctrl.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: maasClusterKey.Namespace,
			Name:      maasClusterKey.Name,
		},
	}}
}
