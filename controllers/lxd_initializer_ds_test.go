package controllers

import (
	"os"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
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
		{name: "floor proves the CVE fix", fragment: "LXD_MIN_VERSION:-5.0.7", want: true},
		{name: "tested refresh target", fragment: "LXD_APPROVED_VERSIONS:-5.0.10", want: true},
		{name: "reads the refresh target from the channel", fragment: "snap info lxd", want: true},
		{name: "retries a failed store read before the long wait", fragment: "(attempt ${attempt}/4)", want: true},
		{name: "keeps the store error for the log", fragment: "snap info lxd 2>/dev/null", want: false},
		{name: "refreshes to the resolved revision", fragment: "snap refresh lxd --revision=\"${TARGET_REV}\"", want: true},
		{name: "keeps tracking the tested channel", fragment: "snap switch --channel=\"${LXD_CHANNEL}\" lxd", want: true},
		{name: "verifies the exact version installed", fragment: "\"${NEW}\" != \"${TARGET_VERSION}\"", want: true},
		{name: "waits on an unapproved channel version", fragment: "BLOCKED_STATUS=blocked-untested", want: true},
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
