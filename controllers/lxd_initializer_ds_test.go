package controllers

import (
	"os"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
)

// TestEnsureLXDScript guards the host LXD bootstrap against the PCP-7159 regressions:
// probing `command -v lxd` (fooled by the Ubuntu 24.04 lxd-installer stub) and pinning
// an architecture-specific snap revision.
func TestEnsureLXDScript(t *testing.T) {
	g := NewGomegaWithT(t)

	raw, err := os.ReadFile("templates/lxd_initializer_ds.yaml")
	g.Expect(err).ToNot(HaveOccurred())
	script := string(raw)

	tests := []struct {
		name     string
		fragment string
		want     bool
	}{
		{name: "probes the snap binary", fragment: "bash -lc 'test -x /snap/bin/lxd'", want: true},
		{name: "installs from the verified channel", fragment: "--channel=\"${LXD_CHANNEL}\"", want: true},
		{name: "channel is the MAAS-verified series", fragment: "LXD_CHANNEL=5.0/stable", want: true},
		{name: "holds snap updates", fragment: "snap refresh --hold lxd", want: true},
		{name: "refresh is off unless enabled", fragment: "AUTO_REFRESH=\"${AUTO_REFRESH:-false}\"", want: true},
		{name: "recommended refresh target", fragment: "LXD_TARGET_VERSION:-5.0.10", want: true},
		{name: "pins the target by its revision for this architecture", fragment: "rev=\"$(known_revision || true)\"", want: true},
		{name: "latest never goes below the recommended version", fragment: "version_ge \"${chan_version}\" \"${LXD_RECOMMENDED_VERSION}\"", want: true},
		{name: "refreshes only hosts older than the target", fragment: "version_ge \"${CUR}\" \"${REFRESH_BELOW}\"", want: true},
		{name: "records the version when autoRefresh is off", fragment: "if [[ -n \"${CUR}\" ]]; then label_version \"${CUR}\"; fi", want: true},
		{name: "reads the refresh target from the channel", fragment: "snap info lxd", want: true},
		{name: "retries a failed store read before the long wait", fragment: "(attempt ${attempt}/4)", want: true},
		{name: "keeps the store error for the log", fragment: "snap info lxd 2>/dev/null", want: false},
		{name: "refreshes to the resolved revision", fragment: "snap refresh lxd --revision=\"${TARGET_REV}\"", want: true},
		{name: "keeps tracking the tested channel", fragment: "snap switch --channel=\"${LXD_CHANNEL}\" lxd", want: true},
		{name: "verifies the exact version installed", fragment: "\"${new}\" != \"${target_version}\" || \"${new_rev}\" != \"${target_rev}\"", want: true},
		{name: "waits on a target it cannot resolve", fragment: "BLOCKED_STATUS=blocked-target", want: true},
		{name: "records a sticky failure label", fragment: "lxdhost.cluster.com/upgrade-status", want: true},
		{name: "stamps the server version", fragment: "lxdhost.cluster.com/lxd-version", want: true},
		{name: "waits while a host is being repaved", fragment: "lxdhost.cluster.com/defer-refresh", want: true},
		{name: "sets snapd proxy from the containerd drop-in", fragment: "proxy.http", want: true},
		{name: "serialises refresh with a lease", fragment: "coordination.k8s.io/v1/namespaces/${LOCK_NAMESPACE}/leases/${LOCK_NAME}", want: true},
		{name: "renews the lease while refreshing", fragment: "start_lease_renewal\n", want: true},
		{name: "releases the lease only while holding it", fragment: "[[ \"${current}\" != \"${NODE_NAME}\" ]]", want: true},
		{name: "retries recording a failure", fragment: "until patch_node \"$(status_patch \"${status}\" \"${detail}\")\"; do", want: true},
		{name: "records every failure durably", fragment: "mark_status \"failed-", want: false},
		{name: "pauses the queue when nodes cannot be listed", fragment: "treating the refresh queue as paused", want: true},
		{name: "reads the whole node list before matching", fragment: "| grep -qE '\"lxdhost.cluster.com/upgrade-status\"", want: false},
		{name: "rechecks the repave label under the lease", fragment: "WAIT_REASON=\"host is being repaved\"", want: true},
		{name: "waits instead of finishing when blocked under the lease", fragment: "releasing it and waiting", want: true},
		{name: "reads the whole journal before matching", fragment: "| grep -qF \"${JMARK}\"", want: false},
		{name: "uses the host curl before installing one", fragment: "K8S_CURL=(host /usr/bin/curl)", want: true},
		{name: "keeps lease files where the host curl can read them", fragment: "WORK=/run/lxd-initializer", want: true},
		{name: "does not keep lease files in the container tmp", fragment: "/tmp/lease", want: false},
		{name: "escapes the status detail as JSON", fragment: "$(json_escape \"$2\")", want: true},
		{name: "records the refresh before it starts", fragment: "mark_refreshing \"${TARGET_VERSION}\" \"${TARGET_REV}\" \"${REFRESH_SINCE}\"", want: true},
		{name: "verifies a refresh interrupted by a pod restart", fragment: "if [[ \"${PREV}\" == \"${REFRESHING_STATUS}\" ]]; then", want: true},
		{name: "pauses the queue on an unverified refresh", fragment: "\"(failed|refreshing)", want: true},
		{name: "records an unreachable snap store", fragment: "mark_status \"${STORE_BLOCKED_STATUS}\"", want: true},
		{name: "configures the snapd proxy only before a refresh", fragment: "leave snapd's proxy alone otherwise.\n          configure_snap_proxy\n", want: true},
		{name: "does not refresh to the channel tip", fragment: "snap refresh lxd --channel=", want: false},
		{name: "does not probe PATH for lxd", fragment: "bash -lc 'command -v lxd", want: false},
		{name: "does not pin an exact snapd version", fragment: "snapd=", want: false},
		{name: "does not revert the snap", fragment: "snap revert", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(strings.Contains(script, tt.fragment)).To(Equal(tt.want))
		})
	}

	g.Expect(regexp.MustCompile(`--revision=[0-9]`).MatchString(script)).To(BeFalse(), "a snap revision must not be hardcoded")
}

func TestRenderDaemonSetLXDRefreshEnv(t *testing.T) {
	cluster := func(annotations map[string]string) *infrav1beta1.MaasCluster {
		return &infrav1beta1.MaasCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "hcp-1", Namespace: "hcp", Annotations: annotations},
			Spec:       infrav1beta1.MaasClusterSpec{LXDConfig: &infrav1beta1.LXDConfig{}},
		}
	}

	tests := []struct {
		name    string
		cluster *infrav1beta1.MaasCluster
		want    map[string]string
	}{
		{name: "default is off with the recommended pinned target", cluster: cluster(nil), want: map[string]string{
			"AUTO_REFRESH": "false", "LXD_TARGET_VERSION": "5.0.10", "LXD_TARGET_REVISIONS": "amd64=40911,arm64=40912", "LXD_RECOMMENDED_VERSION": "5.0.10",
		}},
		{name: "customer target pins its own revisions", cluster: cluster(map[string]string{lxdAutoRefreshAnnotation: "true", lxdTargetAnnotation: "5.0.11"}), want: map[string]string{
			"AUTO_REFRESH": "true", "LXD_TARGET_VERSION": "5.0.11", "LXD_TARGET_REVISIONS": "amd64=41283,arm64=41286",
		}},
		{name: "latest keeps the recommended revisions as the fallback", cluster: cluster(map[string]string{lxdAutoRefreshAnnotation: "true", lxdTargetAnnotation: "latest"}), want: map[string]string{
			"AUTO_REFRESH": "true", "LXD_TARGET_VERSION": "latest", "LXD_TARGET_REVISIONS": "amd64=40911,arm64=40912",
		}},
		{name: "invalid target is rendered with the refresh off", cluster: cluster(map[string]string{lxdAutoRefreshAnnotation: "true", lxdTargetAnnotation: "5.0.3"}), want: map[string]string{
			"AUTO_REFRESH": "false", "LXD_TARGET_VERSION": "5.0.10",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			r := &MaasClusterReconciler{}
			ds, err := r.renderDaemonSetForCluster(&scope.ClusterScope{MaasCluster: tt.cluster}, "lxd-initializer-hcp-1", "hcp")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(ds.Spec.Template.Spec.InitContainers).ToNot(BeEmpty())
			env := map[string]string{}
			for _, e := range ds.Spec.Template.Spec.InitContainers[0].Env {
				env[e.Name] = e.Value
			}
			for k, v := range tt.want {
				g.Expect(env).To(HaveKeyWithValue(k, v))
			}
		})
	}
}
