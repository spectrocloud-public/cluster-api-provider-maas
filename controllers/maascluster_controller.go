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
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
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
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

// MaasClusterReconciler reconciles a MaasCluster object
type MaasClusterReconciler struct {
	client.Client
	Log                 logr.Logger
	Scheme              *runtime.Scheme
	Recorder            record.EventRecorder
	GenericEventChannel chan event.GenericEvent
	Tracker             clustercache.ClusterCache
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=maasclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=maasclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;patch;update
// Required to resolve contract-versioned object references (v1beta2): the contract
// helper reads CustomResourceDefinition metadata to determine the served apiVersion.
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

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
	failureDomains := make([]clusterv1.FailureDomain, 0, len(maasCluster.Spec.FailureDomains))
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

func (r *MaasClusterReconciler) reconcileDNSAttachments(clusterScope *scope.ClusterScope, dnssvc *dns.Service) error {
	machines, err := clusterScope.GetClusterMaasMachines()
	if err != nil {
		return errors.Wrapf(err, "Unable to list all maas machines")
	}

	var runningIpAddresses []string

	currentIPs, err := dnssvc.GetAPIServerDNSRecords()
	if err != nil {
		return errors.Wrap(err, "Unable to get the dns resources")
	}

	machinesPendingAttachment := make([]*infrav1beta1.MaasMachine, 0)
	machinesPendingDetachment := make([]*infrav1beta1.MaasMachine, 0)
	preferredInterfaceName := clusterScope.MaasCluster.Spec.APIServerInterfaceName

	for _, m := range machines {
		if !IsControlPlaneMachine(m) {
			continue
		}

		isRunningHealthy := IsRunning(m)
		var machineIP string

		if !m.DeletionTimestamp.IsZero() || !isRunningHealthy {
			machineIP = getExternalMachineIP(m)
		} else {
			machineIP, err = selectMachineIPForDNS(m, preferredInterfaceName, dnssvc)
			if err != nil {
				return errors.Wrapf(err, "unable to select API server IP for machine %q", m.Name)
			}
		}

		attached := currentIPs.Has(machineIP)

		if !m.DeletionTimestamp.IsZero() || !isRunningHealthy {
			if attached {
				clusterScope.Info("Cleaning up IP on unhealthy machine", "machine", m.Name)
				machinesPendingDetachment = append(machinesPendingDetachment, m)
			}
		} else if isRunningHealthy {
			if !attached {
				clusterScope.Info("Healthy machine without DNS attachment; attaching.", "machine", m.Name)
				machinesPendingAttachment = append(machinesPendingAttachment, m)
			}

			runningIpAddresses = append(runningIpAddresses, machineIP)
		}
		//r.Recorder.Eventf(machineScope.MaasMachine, corev1.EventTypeNormal, "SuccessfulDetachControlPlaneDNS",
		//	"Control plane instance %q is de-registered from load balancer", i.ID)
		//runningIpAddresses = append(runningIpAddresses, m.)
	}

	if err := dnssvc.UpdateDNSAttachments(runningIpAddresses); err != nil {
		return err
	} else if len(machinesPendingAttachment) > 0 || len(machinesPendingDetachment) > 0 {
		clusterScope.Info("Pending DNS attachments or detachments; will retry again")
		return ErrRequeueDNS
	}

	clusterScope.MaasCluster.Status.Network.PublishedInterfaceName = preferredInterfaceName

	return nil
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

func getExternalMachineIP(machine *infrav1beta1.MaasMachine) string {
	for _, i := range machine.Status.Addresses {
		if i.Type == clusterv1.MachineExternalIP {
			return i.Address
		}
	}
	return ""
}

type apiServerMachineIPResolver interface {
	GetMachineIPForInterface(systemID, interfaceName string) (string, error)
}

func selectMachineIPForDNS(machine *infrav1beta1.MaasMachine, interfaceName string, resolver apiServerMachineIPResolver) (string, error) {
	if interfaceName == "" {
		return getExternalMachineIP(machine), nil
	}

	if machine.Spec.SystemID == nil || *machine.Spec.SystemID == "" {
		return "", errors.Errorf("systemID not set for machine %q", machine.Name)
	}

	if resolver == nil {
		return "", errors.New("machine IP resolver is required")
	}

	return resolver.GetMachineIPForInterface(*machine.Spec.SystemID, interfaceName)
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
		conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.DNSReadyCondition, Status: metav1.ConditionFalse, Reason: infrav1beta1.DNSFailedReason, Message: err.Error()})
		return reconcile.Result{}, err
	}

	if maasCluster.Status.Network.DNSName == "" {
		conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.DNSReadyCondition, Status: metav1.ConditionFalse, Reason: infrav1beta1.WaitForDNSNameReason})
		clusterScope.Info("Waiting on API server DNS name")
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}

	maasCluster.Spec.ControlPlaneEndpoint = infrav1beta1.APIEndpoint{
		Host: maasCluster.Status.Network.DNSName,
		Port: clusterScope.APIServerPort(),
	}

	maasCluster.Status.Ready = true
	maasCluster.Status.Initialization.Provisioned = ptr.To(true)

	// Mark the maasCluster ready
	conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.DNSReadyCondition, Status: metav1.ConditionTrue, Reason: infrav1beta1.DNSReadyReason})

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
		conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.APIServerAvailableCondition, Status: metav1.ConditionFalse, Reason: infrav1beta1.APIServerNotReadyReason})
		return ctrl.Result{}, nil
	}

	conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.APIServerAvailableCondition, Status: metav1.ConditionTrue, Reason: infrav1beta1.APIServerAvailableReason})
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
			conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.LXDReadyCondition, Status: metav1.ConditionFalse, Reason: infrav1beta1.LXDSetupPendingReason, Message: "Waiting for target cluster client"})
			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}
		ns := &corev1.Namespace{}
		if e := remoteClient.Get(context.TODO(), types.NamespacedName{Name: "kube-system"}, ns); e != nil {
			clusterScope.Info("kube-system namespace not found on target; will retry")
			conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.LXDReadyCondition, Status: metav1.ConditionFalse, Reason: infrav1beta1.LXDSetupPendingReason, Message: "Waiting for kube-system on target cluster"})
			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if ns.Labels == nil || ns.Labels[infrautil.APIServerReadinessLabel] != "true" {
			clusterScope.Info("Target cluster not ready for DaemonSet deployment - API server readiness label not found")
			conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.LXDReadyCondition, Status: metav1.ConditionFalse, Reason: infrav1beta1.LXDSetupPendingReason, Message: "Waiting for API server readiness label on target cluster"})
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
			conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.LXDReadyCondition, Status: metav1.ConditionFalse, Reason: infrav1beta1.LXDFailedReason, Message: err.Error()})
			return reconcile.Result{}, err
		}
		conditions.Set(maasCluster, metav1.Condition{Type: infrav1beta1.LXDReadyCondition, Status: metav1.ConditionTrue, Reason: infrav1beta1.LXDReadyReason})
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
