package controllers

import (
	"os"
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
		{name: "floor is the CVE fix", fragment: "LXD_MIN_VERSION:-5.0.6", want: true},
		{name: "refreshes below the floor on the tested channel", fragment: "snap refresh lxd --channel=\"${LXD_CHANNEL}\"", want: true},
		{name: "records a sticky failure label", fragment: "lxdhost.cluster.com/upgrade-status", want: true},
		{name: "stamps the server version", fragment: "lxdhost.cluster.com/lxd-version", want: true},
		{name: "waits while a host is being repaved", fragment: "lxdhost.cluster.com/defer-refresh", want: true},
		{name: "sets snapd proxy from the containerd drop-in", fragment: "proxy.http", want: true},
		{name: "serialises refresh with a lease", fragment: "coordination.k8s.io/v1/namespaces/${LOCK_NAMESPACE}/leases/${LOCK_NAME}", want: true},
		{name: "does not probe PATH for lxd", fragment: "bash -lc 'command -v lxd", want: false},
		{name: "does not pin a snap revision", fragment: "--revision=", want: false},
		{name: "does not pin an exact snapd version", fragment: "snapd=", want: false},
		{name: "does not revert the snap", fragment: "snap revert", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(strings.Contains(script, tt.fragment)).To(Equal(tt.want))
		})
	}
}
