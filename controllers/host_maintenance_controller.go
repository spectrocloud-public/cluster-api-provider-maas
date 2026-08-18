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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	maint "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
)

// HMCMaintenanceReconciler handles host maintenance operations via ConfigMap triggers
type HMCMaintenanceReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Namespace is the controller namespace (namespaced deployment)
	Namespace string
	// GenericEventChannel allows external triggers to enqueue reconcile requests
	GenericEventChannel chan event.GenericEvent
}

// EvacuationCompletedAnnotation marks a MaasMachine that has already completed
// host evacuation. When present, the controller will not re-add the evacuation
// finalizer or re-start a maintenance session.
const EvacuationCompletedAnnotation = "maas.lxd.io/evacuation-completed"

// Reconcile handles both MaasMachine evacuation finalizers and ConfigMap triggers
func (r *HMCMaintenanceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	r.Namespace = request.Namespace

	// Try MaasMachine first
	var maasMachine infrav1beta1.MaasMachine
	if err := r.Get(ctx, request.NamespacedName, &maasMachine); err == nil {
		// Successfully got MaasMachine, handle evacuation
		return r.reconcileMaasMachine(ctx, &maasMachine)
	}

	// Try ConfigMap
	var configMap corev1.ConfigMap
	if err := r.Get(ctx, request.NamespacedName, &configMap); err == nil {
		// Successfully got ConfigMap, handle maintenance triggers
		return r.reconcileConfigMap(ctx, request)
	}

	// Neither resource found, ignore
	r.Log.V(1).Info("No MaasMachine or ConfigMap found", "name", request.Name, "namespace", request.Namespace)
	return ctrl.Result{}, nil
}

// reconcileConfigMap handles ConfigMap reconciliation (fallback scenario)
// This is triggered when:
// 1. External operator manually creates/updates the hcp-maintenance-session ConfigMap with trigger fields
// 2. Session needs to be monitored/managed independently of MaasMachine lifecycle
func (r *HMCMaintenanceReconciler) reconcileConfigMap(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	// Only process the maintenance session ConfigMap
	if request.Name != maint.SessionCMName {
		return ctrl.Result{}, nil
	}

	// Load or create session
	st, cm, err := maint.LoadSession(ctx, r.Client, r.Namespace)
	if err != nil {
		r.Log.Error(err, "load session")
		return ctrl.Result{}, err
	}

	// Optional external trigger via CM (manual operator intervention)
	if start, host := maint.ShouldStartFromTrigger(cm); start {
		r.Log.Info("External trigger detected in ConfigMap", "host", host)
		st, err = maint.StartSession(ctx, r.Client, r.Namespace, host)
		if err != nil {
			r.Log.Error(err, "start session")
			return ctrl.Result{}, err
		}
		r.Log.Info("HMC session started from external trigger", "opId", st.OpID, "host", host)
	}

	// No active session: nothing to do
	if st.Status != maint.StatusActive || st.CurrentHost == "" || st.OpID == "" {
		r.Log.V(1).Info("No active session or missing required fields",
			"status", st.Status, "host", st.CurrentHost, "opId", st.OpID)
		return ctrl.Result{}, nil
	}

	// Check if session has exceeded max active sessions (should be 1)
	if st.ActiveSessions > 1 {
		r.Log.Error(nil, "Multiple active sessions detected, aborting",
			"activeSessions", st.ActiveSessions)
		return ctrl.Result{}, fmt.Errorf("multiple active sessions detected: %d", st.ActiveSessions)
	}

	// Tag host with maintenance markers using real MAAS client
	maasClient, err := maint.NewMAASClient(r.Client, r.Namespace)
	if err != nil {
		r.Log.Error(err, "failed to create MAAS client")
		return ctrl.Result{}, err
	}
	tags := maint.NewTagService(maasClient)
	if err := maint.EnsureHostMaintenanceTags(tags, st.CurrentHost, st.OpID); err != nil {
		r.Log.Error(err, "ensure host maintenance tags", "host", st.CurrentHost)
		return ctrl.Result{}, err
	}
	r.Log.Info("host maintenance tags ensured from ConfigMap reconciliation",
		"host", st.CurrentHost, "opId", st.OpID, "activeSessions", st.ActiveSessions)

	return ctrl.Result{}, nil
}

// reconcileMaasMachine handles MaasMachine reconciliation for evacuation finalizers
func (r *HMCMaintenanceReconciler) reconcileMaasMachine(ctx context.Context, maasMachine *infrav1beta1.MaasMachine) (ctrl.Result, error) {
	log := r.Log.WithValues("maasmachine", maasMachine.Name)

	// Only process host machines (not VMs)
	if maasMachine.Spec.Parent != nil && *maasMachine.Spec.Parent != "" {
		return ctrl.Result{}, nil // This is a VM, skip
	}

	// Get the owner CAPI Machine
	var capiMachine clusterv1.Machine
	ownerRef := maasMachine.GetOwnerReferences()
	if len(ownerRef) == 0 {
		log.V(1).Info("MaasMachine has no owner references")
		return ctrl.Result{}, nil
	}

	// Find the Machine owner reference
	var machineOwnerRef *metav1.OwnerReference
	for _, ref := range ownerRef {
		if ref.Kind == "Machine" && ref.APIVersion == clusterv1.GroupVersion.String() {
			machineOwnerRef = &ref
			break
		}
	}

	if machineOwnerRef == nil {
		log.V(1).Info("MaasMachine has no Machine owner reference")
		return ctrl.Result{}, nil
	}

	// Get the CAPI Machine
	machineKey := types.NamespacedName{
		Name:      machineOwnerRef.Name,
		Namespace: maasMachine.Namespace,
	}
	if err := r.Get(ctx, machineKey, &capiMachine); err != nil {
		log.Error(err, "failed to get owner CAPI Machine", "machine", machineOwnerRef.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if CAPI Machine is NOT being deleted
	if capiMachine.DeletionTimestamp.IsZero() {
		// CAPI Machine is not being deleted, simply return
		return ctrl.Result{}, nil
	}

	// If evacuation already completed, do not start a new session
	if maasMachine.Annotations != nil && maasMachine.Annotations[EvacuationCompletedAnnotation] != "" {
		log.V(1).Info("Evacuation already completed for host, skipping")
		return ctrl.Result{}, nil
	}

	// Evacuation finalizer should have been added proactively during reconcileNormal
	// If it's missing, log a warning but don't try to add it (Kubernetes won't allow it during deletion)
	if !controllerutil.ContainsFinalizer(maasMachine, HostEvacuationFinalizer) {
		log.Error(nil, "⚠️ Evacuation finalizer missing - should have been added proactively during reconcileNormal",
			"maasMachine", maasMachine.Name,
			"finalizers", maasMachine.Finalizers)
		// Cannot add finalizer during deletion - let the machine be deleted without evacuation
		return ctrl.Result{}, nil
	}

	// Start maintenance session if not already active (only once when deletion starts)
	st, _, err := maint.LoadSession(ctx, r.Client, maasMachine.Namespace)
	if err != nil {
		log.Error(err, "failed to load maintenance session")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// If no active session exists, start one
	if st.OpID == "" || st.Status != maint.StatusActive {
		if maasMachine.Spec.SystemID != nil {
			st, err = maint.StartSession(ctx, r.Client, maasMachine.Namespace, *maasMachine.Spec.SystemID)
			if err != nil {
				log.Error(err, "failed to start maintenance session")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}
			log.Info("Maintenance session started", "opId", st.OpID, "host", st.CurrentHost)
		}
	}

	log.Info("Processing host evacuation for MaasMachine deletion", "opID", st.OpID)

	// Get system ID
	if maasMachine.Spec.SystemID == nil {
		log.Error(nil, "MaasMachine has no systemID")
		return ctrl.Result{}, fmt.Errorf("maasMachine has no systemID")
	}
	hostSystemID := *maasMachine.Spec.SystemID

	// Ensure maintenance tags are present
	maasClient, err := maint.NewMAASClient(r.Client, maasMachine.Namespace)
	if err != nil {
		log.Error(err, "failed to create MAAS client")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	tagService := maint.NewTagService(maasClient)

	if err := maint.EnsureHostMaintenanceTags(tagService, hostSystemID, st.OpID); err != nil {
		log.Error(err, "failed to ensure maintenance tags")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	log.Info("Maintenance tags ensured", "host", hostSystemID, "opId", st.OpID)

	// Create host maintenance service
	hmcService, err := NewHostMaintenanceService(r.Client, maasMachine.Namespace, r.Recorder)
	if err != nil {
		log.Error(err, "failed to create host maintenance service")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// Check evacuation gates
	evacuationReady, err := hmcService.CheckEvacuationGates(ctx, maasMachine, log)
	if err != nil {
		log.Error(err, "failed to check evacuation gates")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	if !evacuationReady {
		log.Info("Evacuation gates not met, blocking deletion",
			"host", hostSystemID,
			"requeueAfter", 10*time.Second)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Evacuation gates are met, clear tags and remove finalizer
	if err := hmcService.ClearMaintenanceTagsAndRemoveFinalizer(ctx, maasMachine, log); err != nil {
		log.Error(err, "failed to clear maintenance tags and remove finalizer")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// Mark evacuation completed on the MaasMachine to prevent future sessions
	if maasMachine.Annotations == nil {
		maasMachine.Annotations = map[string]string{}
	}
	maasMachine.Annotations[EvacuationCompletedAnnotation] = st.OpID
	if err := r.Update(ctx, maasMachine); err != nil {
		log.Error(err, "failed to annotate MaasMachine as evacuation completed")
		// Not fatal for host cleanup; continue to complete the session
	}

	// Complete the maintenance session
	log.Info("Completing maintenance session", "opID", st.OpID, "host", hostSystemID)
	if err := maint.CompleteSession(ctx, r.Client, maasMachine.Namespace); err != nil {
		log.Error(err, "failed to complete maintenance session", "opID", st.OpID)
		// Don't block on session completion failure
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// Verify session was completed
	completedSession, _, err := maint.LoadSession(ctx, r.Client, maasMachine.Namespace)
	if err != nil {
		log.Error(err, "failed to verify session completion")
	} else {
		log.Info("Session completion verified",
			"status", completedSession.Status,
			"currentHost", completedSession.CurrentHost,
			"activeSessions", completedSession.ActiveSessions,
			"affectedWLCClusters", completedSession.AffectedWLCClusters,
			"pendingReadyVMReplacements", completedSession.PendingReadyVMReplacements)
	}

	log.Info("Host evacuation completed successfully", "host", hostSystemID, "opID", st.OpID)
	return ctrl.Result{}, nil
}

func (r *HMCMaintenanceReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	recover := true
	options.RecoverPanic = &recover

	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("hmc-controller")
	}

	if r.GenericEventChannel == nil {
		r.GenericEventChannel = make(chan event.GenericEvent)
	}

	c, err := ctrl.NewControllerManagedBy(mgr).
		Named("hmc").
		For(&infrav1beta1.MaasMachine{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetName() == maint.SessionCMName {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}}
				}
				return nil
			}),
		).
		WithOptions(options).
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
