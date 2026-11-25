package controllers

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	// embed template
	_ "embed"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/util"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/util/trust"
)

const (
	lxdInitializedLabel      = "lxdhost.cluster.com/initialized"
	lxdVersionLabel          = "lxdhost.cluster.com/lxd-version"
	lxdUpgradeStatusLabel    = "lxdhost.cluster.com/upgrade-status"
	lxdDeferRefreshLabel     = "lxdhost.cluster.com/defer-refresh"
	lxdUpgradeDetailAnnot    = "lxdhost.cluster.com/upgrade-detail"
	lxdAutoRefreshAnnotation = "infrastructure.cluster.x-k8s.io/lxd-auto-refresh"
	lxdTargetAnnotation      = "infrastructure.cluster.x-k8s.io/lxd-target-version"
	lxdBlockedStatus         = "blocked-target"
	lxdStoreBlockedStatus    = "blocked-store"

	// lxdMinServerVersion is the first 5.0 server version that reports the
	// CVE-2026-28384 fix. The fix shipped as interim snap 5.0.6-e49d9f4, and the
	// original 5.0.6 release reports the same server version without it.
	lxdMinServerVersion = "5.0.7"

	// lxdRecommendedVersion is the 5.0 server version validated against MAAS 3.6/3.7
	// as an in-place refresh target, including from 5.0.3 with VMs running.
	lxdRecommendedVersion = "5.0.10"

	// lxdLatestTarget installs what the channel offers, if it is at least the
	// recommended version.
	lxdLatestTarget = "latest"
)

// lxdKnownRevisions maps LXD server versions to their snap revision per architecture.
// The store installs these by revision after the channel has moved on.
var lxdKnownRevisions = map[string]map[string]string{
	"5.0.10": {"amd64": "40911", "arm64": "40912"},
	"5.0.11": {"amd64": "41283", "arm64": "41286"},
}

var lxdVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// lxdVersionPolicy decides whether a host is refreshed and what it is refreshed to.
type lxdVersionPolicy struct {
	AutoRefresh bool
	// Target is a server version or lxdLatestTarget.
	Target string
	// RefreshBelow is the server version below which a host is refreshed: Target, or
	// the recommended version when Target is lxdLatestTarget.
	RefreshBelow string
	// TargetError explains why the configured target was rejected. AutoRefresh is
	// false when it is set.
	TargetError string
}

// targetRevisions renders the known revisions of the version a refresh pins, as
// "arch=revision" pairs, for the initializer script.
func (p lxdVersionPolicy) targetRevisions() string {
	revs := lxdKnownRevisions[p.RefreshBelow]
	pairs := make([]string, 0, len(revs))
	for arch, rev := range revs {
		pairs = append(pairs, arch+"="+rev)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

//go:embed templates/lxd_initializer_ds.yaml.processed
var lxdInitTemplate string

//go:embed templates/lxd_initializer_rbac.yaml
var lxdInitRBACTemplate string

func render(replacements map[string]string, tmpl string) string {
	for k, v := range replacements {
		tmpl = strings.ReplaceAll(tmpl, k, v)
	}
	return tmpl
}

// ensureLXDInitializerDS creates or deletes the DaemonSet that initialises LXD on all nodes (control-plane and worker)
func (r *MaasClusterReconciler) ensureLXDInitializerDS(ctx context.Context, clusterScope *scope.ClusterScope) error {
	cluster := clusterScope.MaasCluster

	dsName := fmt.Sprintf("lxd-initializer-%s", cluster.Name)
	dsNamespace := cluster.Namespace

	// Always operate on the TARGET cluster client
	remoteClient, err := r.getTargetClient(ctx, clusterScope)
	if err != nil {
		return err
	}

	// If feature is off or cluster is being deleted, we're done
	if !clusterScope.IsLXDHostEnabled() || !cluster.ObjectMeta.DeletionTimestamp.IsZero() {
		return nil
	}

	// Gate: ensure pivot completed. Require mgmt namespace to have clusterEnv=target
	isTarget, clusterEnv := r.namespaceIsTarget(ctx, dsNamespace)
	if !isTarget {
		r.Log.Info("Namespace not marked as target; deferring LXD initializer", "namespace", dsNamespace, "clusterEnv", clusterEnv)
		return nil
	}

	// Gate: derive desired CP count from MaasCloudConfig; fallback to KCP
	desiredCP, _ := r.computeDesiredControlPlane(ctx, dsNamespace, cluster.Name)
	policy := lxdPolicyFor(cluster)

	// Without current defer labels a refresh pod could start on a host being repaved,
	// so do not create or update the DaemonSet until they are in sync.
	capiClusterName := cluster.Name
	if clusterScope.Cluster != nil {
		capiClusterName = clusterScope.Cluster.Name
	}
	if err := r.syncLXDDeferRefresh(ctx, cluster.Namespace, capiClusterName, remoteClient); err != nil {
		if policy.AutoRefresh {
			return fmt.Errorf("failed to sync LXD defer-refresh labels: %w", err)
		}
		r.Log.Error(err, "failed to sync LXD defer-refresh labels", "namespace", dsNamespace)
	}

	// New gate: proceed if any node needs initialization. This avoids deadlock during upgrades
	// when an old node is NotReady due to HMC constraints but new nodes must be initialized.
	if !r.anyNodeNeedsInitialization(ctx, remoteClient, policy) {
		r.Log.Info("All nodes already labeled initialized; considering DS cleanup", "namespace", dsNamespace, "ds", dsName)
		if done, err := r.maybeShortCircuitDelete(ctx, remoteClient, dsNamespace, desiredCP, dsName, policy); err != nil {
			r.Log.Error(err, "failed to maybe short circuit delete", "namespace", dsNamespace, "ds", dsName)
			return err
		} else if done {
			r.Log.Info("deleted existing initializer DS - all nodes are ready and initialized", "namespace", dsNamespace, "ds", dsName)
			return nil
		}
		r.Log.Info("no nodes need initialization; skipping DS creation", "namespace", dsNamespace, "ds", dsName)
		return nil
	}

	//TODO: Check if we require this
	// if ok := r.enoughNodesReady(ctx, remoteClient, desiredCP, readyByKCP); !ok {
	// 	return nil
	// }

	if err := r.deleteExistingInitializerDS(ctx, remoteClient, dsNamespace); err != nil {
		r.Log.Error(err, "failed to delete existing initializer DS", "namespace", dsNamespace, "ds", dsName)
		return err
	}

	// Ensure RBAC resources are created on the target cluster
	if err := r.ensureLXDInitializerRBACOnTarget(ctx, remoteClient, dsNamespace); err != nil {
		r.Log.Error(err, "failed to ensure LXD initializer RBAC", "namespace", dsNamespace, "ds", dsName)
		return fmt.Errorf("failed to ensure LXD initializer RBAC: %v", err)
	}

	if done, err := r.maybeShortCircuitDelete(ctx, remoteClient, dsNamespace, desiredCP, dsName, policy); err != nil {
		r.Log.Error(err, "failed to maybe short circuit delete", "namespace", dsNamespace, "ds", dsName)
		return err
	} else if done {
		r.Log.Info("deleted existing initializer DS - all nodes are ready and initialized", "namespace", dsNamespace, "ds", dsName)
		return nil
	}

	ds, err := r.renderDaemonSetForCluster(clusterScope, dsName, dsNamespace)
	if err != nil {
		r.Log.Error(err, "failed to render DaemonSet for cluster", "namespace", dsNamespace, "ds", dsName)
		return err
	}

	// Do not set owner refs across clusters; just create/patch on target cluster.
	// Mutate existing DaemonSet so changes to template/spec take effect on reconcile.
	current := &appsv1.DaemonSet{}
	current.Name = dsName
	current.Namespace = dsNamespace

	_, err = controllerutil.CreateOrPatch(ctx, remoteClient, current, func() error {
		// Preserve immutable selector if already present; align labels.
		current.Labels = ds.Labels
		current.Annotations = ds.Annotations

		// Update pod template and mutable spec fields
		current.Spec.Template = ds.Spec.Template
		current.Spec.UpdateStrategy = ds.Spec.UpdateStrategy
		current.Spec.MinReadySeconds = ds.Spec.MinReadySeconds
		current.Spec.RevisionHistoryLimit = ds.Spec.RevisionHistoryLimit

		// Initialize selector if missing (only valid on create)
		if current.Spec.Selector == nil || len(current.Spec.Selector.MatchLabels) == 0 {
			current.Spec.Selector = ds.Spec.Selector
		}
		// Ensure template labels include selector labels
		if current.Spec.Selector != nil && len(current.Spec.Selector.MatchLabels) > 0 {
			if current.Spec.Template.Labels == nil {
				current.Spec.Template.Labels = map[string]string{}
			}
			for k, v := range current.Spec.Selector.MatchLabels {
				current.Spec.Template.Labels[k] = v
			}
		}
		return nil
	})
	if err != nil {
		r.Log.Error(err, "failed to create/patch DaemonSet", "namespace", dsNamespace, "ds", dsName)
		return err
	}
	r.Log.Info("created/patched DaemonSet", "namespace", dsNamespace, "ds", dsName)
	return nil
}

// ensureLXDInitializerRBACOnTarget creates the RBAC resources for lxd-initializer on the target cluster
func (r *MaasClusterReconciler) ensureLXDInitializerRBACOnTarget(ctx context.Context, remoteClient client.Client, namespace string) error {
	// Parse RBAC template into separate resources
	rbacYaml := strings.ReplaceAll(lxdInitRBACTemplate, "namespace: default", fmt.Sprintf("namespace: %s", namespace))

	// Split the YAML into separate documents
	docs := strings.Split(rbacYaml, "---")

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		// Parse as unstructured object to handle different resource types
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
			return fmt.Errorf("failed to unmarshal RBAC resource: %v", err)
		}

		// Set namespace for namespaced resources
		if obj.GetKind() == "ServiceAccount" {
			obj.SetNamespace(namespace)
		}

		// CreateOrPatch fetches the live object before mutate, so copy the desired
		// rules onto it. An empty mutate leaves an already-installed ClusterRole
		// without the lease permission the refresh needs.
		desired := obj.DeepCopy()
		_, err := controllerutil.CreateOrPatch(ctx, remoteClient, obj, func() error {
			if rules, ok := desired.Object["rules"]; ok {
				obj.Object["rules"] = rules
			}
			if roleRef, ok := desired.Object["roleRef"]; ok {
				obj.Object["roleRef"] = roleRef
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to create/patch %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}

	return nil
}

// getTargetClient returns the workload cluster client or a wrapped error
func (r *MaasClusterReconciler) getTargetClient(ctx context.Context, clusterScope *scope.ClusterScope) (client.Client, error) {
	remoteClient, err := clusterScope.GetWorkloadClusterClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get target cluster client: %v", err)
	}
	return remoteClient, nil
}

// namespaceIsTarget checks if the management namespace is annotated as clusterEnv=target
func (r *MaasClusterReconciler) namespaceIsTarget(ctx context.Context, namespace string) (bool, string) {
	mgmtNS := &corev1.Namespace{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: namespace}, mgmtNS); err != nil {
		return false, ""
	}
	if mgmtNS.Annotations == nil {
		return false, ""
	}
	v := strings.TrimSpace(mgmtNS.Annotations["clusterEnv"])
	return v == "target", v
}

// computeDesiredControlPlane determines desired control-plane replicas and ready count from KubeadmControlPlane
func (r *MaasClusterReconciler) computeDesiredControlPlane(ctx context.Context, namespace, clusterName string) (int32, int32) {
	desiredCP := int32(1)
	readyByKCP := int32(0)

	// Use KubeadmControlPlane as the authoritative source
	kcpList := &unstructured.UnstructuredList{}
	kcpList.SetGroupVersionKind(schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta1", Kind: "KubeadmControlPlaneList"})
	if err := r.Client.List(ctx, kcpList, client.InNamespace(namespace), client.MatchingLabels{
		"cluster.x-k8s.io/cluster-name": clusterName,
	}); err == nil {
		if len(kcpList.Items) > 0 {
			item := kcpList.Items[0]
			if v, found, _ := unstructured.NestedInt64(item.Object, "spec", "replicas"); found && v > 0 {
				desiredCP = util.SafeInt64ToInt32(v)
			}
			if v, found, _ := unstructured.NestedInt64(item.Object, "status", "readyReplicas"); found && v >= 0 {
				readyByKCP = util.SafeInt64ToInt32(v)
			}
		}
	}

	return desiredCP, readyByKCP
}

// enoughNodesReady checks the target cluster for Ready nodes (both control-plane and worker)
func (r *MaasClusterReconciler) enoughNodesReady(ctx context.Context, remoteClient client.Client, desiredCP, readyByKCP int32) bool {
	nodeList := &corev1.NodeList{}
	// Check all nodes, not just control-plane
	if err := remoteClient.List(ctx, nodeList); err == nil {
		ready := 0
		for _, n := range nodeList.Items {
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					ready++
					break
				}
			}
		}
		// Require at least the desired CP count of nodes to be ready
		if int64(len(nodeList.Items)) < int64(desiredCP) || int64(ready) < int64(desiredCP) {
			r.Log.Info("Not enough nodes present/ready yet; skipping DS for now", "desiredCP", desiredCP, "readyByKCP", readyByKCP, "nodeList", len(nodeList.Items), "ready", ready)
			return false
		}
	}
	return true
}

// anyNodeNeedsInitialization returns true if any node (control-plane or worker) needs initialization.
// Only checks Ready nodes to avoid false positives from nodes that aren't ready yet.
func (r *MaasClusterReconciler) anyNodeNeedsInitialization(ctx context.Context, remoteClient client.Client, policy lxdVersionPolicy) bool {
	nodeList := &corev1.NodeList{}
	// Check all nodes, not just control-plane, to include worker nodes
	if err := remoteClient.List(ctx, nodeList); err != nil {
		r.Log.Info("Failed to list nodes; proceeding to create initializer DS to be safe", "error", err)
		return true
	}
	if len(nodeList.Items) == 0 {
		r.Log.Info("No nodes reported yet; proceeding with initializer DS")
		return true
	}

	for _, n := range nodeList.Items {
		// Check if node is Ready
		isReady := false
		for _, condition := range n.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}

		// If node is Ready but not initialized, or it is below the LXD floor, it needs work.
		if isReady && nodeNeedsLXDWork(n, policy) {
			// Determine node role for logging
			nodeRole := "worker"
			if n.Labels != nil && n.Labels["node-role.kubernetes.io/control-plane"] != "" {
				nodeRole = "control-plane"
			}
			r.Log.Info("Ready node requires LXD initialization", "node", n.Name, "role", nodeRole)
			return true
		}

		// If node is not Ready yet but has the label, log a warning (might be stale)
		if !isReady && n.Labels != nil && n.Labels[lxdInitializedLabel] == "true" {
			r.Log.Info("Node has initialization label but is not Ready - may need re-initialization", "node", n.Name)
		}
	}
	return false
}

// deleteExistingInitializerDS removes any DaemonSets with old labeling in the namespace
func (r *MaasClusterReconciler) deleteExistingInitializerDS(ctx context.Context, remoteClient client.Client, namespace string) error {
	dsList := &appsv1.DaemonSetList{}
	if err := remoteClient.List(ctx, dsList, client.InNamespace(namespace), client.MatchingLabels{
		"app": "lxd-initializer",
	}); err != nil {
		return fmt.Errorf("failed to list DaemonSets: %v", err)
	}

	for _, ds := range dsList.Items {
		if err := remoteClient.Delete(ctx, &ds); err != nil {
			return fmt.Errorf("failed to delete DaemonSet %s: %v", ds.Name, err)
		}
	}
	return nil
}

// maybeShortCircuitDelete deletes the DS if all nodes are already initialized
// BUT only if we have exactly desiredCP nodes - avoids deleting during maintenance when new nodes are joining
func (r *MaasClusterReconciler) maybeShortCircuitDelete(ctx context.Context, remoteClient client.Client, namespace string, desiredCP int32, dsName string, policy lxdVersionPolicy) (bool, error) {
	shortCircuitNodes := &corev1.NodeList{}
	// Check all nodes, not just control-plane
	if err := remoteClient.List(ctx, shortCircuitNodes); err != nil || len(shortCircuitNodes.Items) == 0 {
		return false, nil
	}

	readyCount := 0
	for _, n := range shortCircuitNodes.Items {
		if nodeIsReady(n) {
			readyCount++
		}
	}

	// Delete the DS only when every node is initialized and, when refresh is on,
	// every ready node is at or above the floor with no failed verification.
	totalNodes := len(shortCircuitNodes.Items)
	if lxdRefreshSettled(shortCircuitNodes.Items, policy) {
		shortCircuitDSList := &appsv1.DaemonSetList{}
		if err := remoteClient.List(ctx, shortCircuitDSList, client.InNamespace(namespace), client.MatchingLabels{"app": dsName}); err == nil {
			for _, ds := range shortCircuitDSList.Items {
				_ = remoteClient.Delete(ctx, &ds)
			}
		}
		r.Log.Info("Deleted LXD initializer DaemonSet - all nodes initialized",
			"desiredCP", desiredCP, "totalNodes", totalNodes, "readyNodes", readyCount)
		return true, nil
	}
	return false, nil
}

// renderDaemonSetForCluster renders the DS YAML from template using cluster config and returns a DaemonSet object
func (r *MaasClusterReconciler) renderDaemonSetForCluster(clusterScope *scope.ClusterScope, dsName, namespace string) (*appsv1.DaemonSet, error) {
	cluster := clusterScope.MaasCluster
	cfg := clusterScope.GetLXDConfig()

	sb := cfg.StorageBackend
	if sb == "" {
		sb = "zfs"
	}
	ss := cfg.StorageSize
	if ss == "" {
		ss = "50"
	}
	nb := cfg.NetworkBridge
	skip := "true"
	if cfg.SkipNetworkUpdate != nil && !*cfg.SkipNetworkUpdate {
		skip = "false"
	}

	nt := cfg.NICType
	if nt == "" {
		nt = "bridged"
	}
	np := cfg.NICParent
	// Deterministic per-cluster trust password derived from cluster UID
	tp := trust.DeriveTrustPassword(string(cluster.UID))

	policy := lxdPolicyFor(cluster)
	auto := "true"
	if !policy.AutoRefresh {
		auto = "false"
	}

	rendered := render(map[string]string{
		"${STORAGE_BACKEND}":       sb,
		"${STORAGE_SIZE}":          ss,
		"${NETWORK_BRIDGE}":        nb,
		"${SKIP_NETWORK_UPDATE}":   skip,
		"${TRUST_PASSWORD}":        tp,
		"${NIC_TYPE}":              nt,
		"${NIC_PARENT}":            np,
		"${AUTO_REFRESH}":            auto,
		"${LXD_TARGET_VERSION}":      policy.Target,
		"${LXD_TARGET_REVISIONS}":    policy.targetRevisions(),
		"${LXD_RECOMMENDED_VERSION}": lxdRecommendedVersion,
	}, lxdInitTemplate)

	ds := &appsv1.DaemonSet{}
	if err := yaml.Unmarshal([]byte(rendered), ds); err != nil {
		return nil, err
	}

	// ensure names/labels are cluster-specific without touching the image name
	ds.Name = dsName
	if ds.Labels == nil {
		ds.Labels = map[string]string{}
	}
	ds.Labels["app"] = dsName
	ds.Spec.Selector.MatchLabels["app"] = dsName
	ds.Spec.Template.Labels["app"] = dsName
	ds.Namespace = namespace

	return ds, nil
}

// lxdAutoRefreshEnabled reports whether installed snaps older than the target are refreshed.
// It is off unless enabled. The spec field wins; the annotation is how Palette pack
// values opt in before the module bump.
func lxdAutoRefreshEnabled(cluster *infrav1beta1.MaasCluster) bool {
	if cluster != nil && cluster.Spec.LXDConfig != nil && cluster.Spec.LXDConfig.AutoRefresh != nil {
		return *cluster.Spec.LXDConfig.AutoRefresh
	}
	return cluster != nil && cluster.Annotations != nil &&
		strings.EqualFold(strings.TrimSpace(cluster.Annotations[lxdAutoRefreshAnnotation]), "true")
}

func nodeIsReady(n corev1.Node) bool {
	for _, condition := range n.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// lxdPolicyFor builds the refresh policy for a cluster. The spec field wins over the
// annotation Palette writes from pack values. An invalid target turns the refresh off
// rather than installing something the customer did not ask for.
func lxdPolicyFor(cluster *infrav1beta1.MaasCluster) lxdVersionPolicy {
	policy := lxdVersionPolicy{
		AutoRefresh:  lxdAutoRefreshEnabled(cluster),
		Target:       lxdRecommendedVersion,
		RefreshBelow: lxdRecommendedVersion,
	}

	target := ""
	if cluster != nil && cluster.Spec.LXDConfig != nil && cluster.Spec.LXDConfig.TargetVersion != "" {
		target = cluster.Spec.LXDConfig.TargetVersion
	} else if cluster != nil && cluster.Annotations != nil {
		target = cluster.Annotations[lxdTargetAnnotation]
	}
	target = strings.TrimSpace(target)

	switch {
	case target == "":
	case strings.EqualFold(target, lxdLatestTarget):
		policy.Target = lxdLatestTarget
	case !lxdVersionPattern.MatchString(target):
		policy.TargetError = fmt.Sprintf("targetVersion %q is not a version such as 5.0.11 or latest", target)
	case compareDotVersions(target, lxdMinServerVersion) < 0:
		policy.TargetError = fmt.Sprintf("targetVersion %s is below %s, the first version with the CVE-2026-28384 fix", target, lxdMinServerVersion)
	default:
		policy.Target = target
		policy.RefreshBelow = target
	}
	if policy.TargetError != "" {
		policy.AutoRefresh = false
	}
	return policy
}

// nodeNeedsLXDWork is true when a Ready node still needs install or an in-place refresh.
func nodeNeedsLXDWork(n corev1.Node, policy lxdVersionPolicy) bool {
	if n.Labels == nil || n.Labels[lxdInitializedLabel] != "true" {
		return true
	}
	if !policy.AutoRefresh {
		return false
	}
	if strings.HasPrefix(n.Labels[lxdUpgradeStatusLabel], "failed") {
		return true
	}
	if n.Labels[lxdDeferRefreshLabel] == "true" {
		return true
	}
	return lxdVersionBelow(n.Labels[lxdVersionLabel], policy.RefreshBelow)
}

// lxdRefreshSettled is true when the DaemonSet can be removed.
func lxdRefreshSettled(nodes []corev1.Node, policy lxdVersionPolicy) bool {
	if len(nodes) == 0 {
		return false
	}
	for _, n := range nodes {
		if n.Labels == nil || n.Labels[lxdInitializedLabel] != "true" {
			return false
		}
		if !policy.AutoRefresh || !nodeIsReady(n) {
			continue
		}
		if strings.HasPrefix(n.Labels[lxdUpgradeStatusLabel], "failed") || n.Labels[lxdDeferRefreshLabel] == "true" {
			return false
		}
		if lxdVersionBelow(n.Labels[lxdVersionLabel], policy.RefreshBelow) {
			return false
		}
	}
	return true
}

// lxdVersionBelow treats a missing version as below, so an unchecked host is refreshed.
func lxdVersionBelow(version, floor string) bool {
	if version == "" {
		return true
	}
	return compareDotVersions(version, floor) < 0
}

// lxdHostsBelowSecurityMinimum lists initialized nodes whose recorded LXD version is
// below lxdMinServerVersion, and those with no recorded version.
func lxdHostsBelowSecurityMinimum(nodes []corev1.Node) (below, unknown []string) {
	for _, n := range nodes {
		if n.Labels == nil || n.Labels[lxdInitializedLabel] != "true" {
			continue
		}
		switch v := n.Labels[lxdVersionLabel]; {
		case v == "":
			unknown = append(unknown, n.Name)
		case compareDotVersions(v, lxdMinServerVersion) < 0:
			below = append(below, n.Name)
		}
	}
	return below, unknown
}

func compareDotVersions(a, b string) int {
	as := splitDotVersion(a)
	bs := splitDotVersion(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func splitDotVersion(v string) []int {
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	return out
}

func lxdRefreshFailedNodeNames(nodes []corev1.Node) []string {
	var names []string
	for _, n := range nodes {
		if n.Labels != nil && strings.HasPrefix(n.Labels[lxdUpgradeStatusLabel], "failed") {
			names = append(names, n.Name)
		}
	}
	return names
}

// lxdRefreshBlockedNodes lists nodes whose refresh is waiting with the given status,
// with the detail the initializer recorded (for example, what the channel offers).
func lxdRefreshBlockedNodes(nodes []corev1.Node, status string) (names []string, detail string) {
	for _, n := range nodes {
		if n.Labels == nil || n.Labels[lxdUpgradeStatusLabel] != status {
			continue
		}
		names = append(names, n.Name)
		if detail == "" && n.Annotations != nil {
			detail = n.Annotations[lxdUpgradeDetailAnnot]
		}
	}
	return names, detail
}

// syncLXDDeferRefresh marks target nodes whose bare metal is being replaced so the
// initializer pod waits instead of refreshing LXD under a repave. Any lookup error
// returns before labels change: unknown maintenance state must not clear a label.
func (r *MaasClusterReconciler) syncLXDDeferRefresh(ctx context.Context, namespace, clusterName string, remoteClient client.Client) error {
	deferNodes := map[string]struct{}{}
	deferProviderIDs := map[string]struct{}{}

	machines := &infrav1beta1.MaasMachineList{}
	if err := r.Client.List(ctx, machines, client.InNamespace(namespace), client.MatchingLabels{
		clusterv1.ClusterNameLabel: clusterName,
	}); err != nil {
		return err
	}
	for i := range machines.Items {
		mm := &machines.Items[i]
		// A repave starts when the owning Machine is deleted. The evacuation finalizer
		// is no signal: it is added to every HCP host up front.
		machine, err := capiutil.GetOwnerMachine(ctx, r.Client, mm.ObjectMeta)
		switch {
		case apierrors.IsNotFound(err):
			// The owning Machine is already gone, so this host is on its way out.
			if mm.Spec.ProviderID != nil && *mm.Spec.ProviderID != "" {
				deferProviderIDs[*mm.Spec.ProviderID] = struct{}{}
			}
			continue
		case err != nil:
			return fmt.Errorf("failed to get owner Machine of MaasMachine %s: %w", mm.Name, err)
		case machine == nil:
			continue
		}
		if mm.DeletionTimestamp.IsZero() && machine.DeletionTimestamp.IsZero() {
			continue
		}
		if machine.Status.NodeRef.Name != "" {
			deferNodes[machine.Status.NodeRef.Name] = struct{}{}
		}
		if mm.Spec.ProviderID != nil && *mm.Spec.ProviderID != "" {
			deferProviderIDs[*mm.Spec.ProviderID] = struct{}{}
		}
	}

	st, _, err := maintenance.LoadSession(ctx, r.Client, namespace)
	if err != nil {
		return fmt.Errorf("failed to load host maintenance session: %w", err)
	}
	sessionSuffix := ""
	if st.Status == maintenance.StatusActive && st.CurrentHost != "" {
		sessionSuffix = "/" + st.CurrentHost
	}

	nodeList := &corev1.NodeList{}
	if err := remoteClient.List(ctx, nodeList); err != nil {
		return err
	}
	var patchErrs []error
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		_, want := deferNodes[n.Name]
		if _, ok := deferProviderIDs[n.Spec.ProviderID]; ok && n.Spec.ProviderID != "" {
			want = true
		}
		if sessionSuffix != "" && strings.HasSuffix(n.Spec.ProviderID, sessionSuffix) {
			want = true
		}
		has := n.Labels != nil && n.Labels[lxdDeferRefreshLabel] == "true"
		if want == has {
			continue
		}
		orig := n.DeepCopy()
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		if want {
			n.Labels[lxdDeferRefreshLabel] = "true"
		} else {
			delete(n.Labels, lxdDeferRefreshLabel)
		}
		if err := remoteClient.Patch(ctx, n, client.MergeFrom(orig)); err != nil {
			patchErrs = append(patchErrs, fmt.Errorf("node %s: %w", n.Name, err))
		}
	}
	return kerrors.NewAggregate(patchErrs)
}
