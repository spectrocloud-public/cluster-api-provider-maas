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
package maintenance

import (
	"context"
	"strings"

	"github.com/spectrocloud/maas-client-go/maasclient"
)

const (
	// TagHostMaintenance marks a host as under maintenance (drain source).
	TagHostMaintenance = "maas-lxd-host-maintenance"
	// TagHostNoSchedule prevents new placements on the host during maintenance.
	TagHostNoSchedule = "maas-lxd-host-noschedule"
	// TagHostOpPrefix prefixes the maintenance session opId stored as a tag.
	TagHostOpPrefix = "maas-lxd-hcp-op-"

	// TagVMControlPlane marks a VM as a control-plane VM.
	TagVMControlPlane = "maas-lxd-wlc-cp"
	// TagVMClusterPrefix prefixes the owning WLC cluster identifier.
	TagVMClusterPrefix = "maas-lxd-wlc-"
	// TagVMReadyOpPrefix prefixes the ready acknowledgement for a given session opId.
	TagVMReadyOpPrefix = "maas-lxd-ready-op-"
)

// BuildOpTag builds the session opId tag for hosts.
func BuildOpTag(opID string) string { return TagHostOpPrefix + opID }

// ParseOpTag returns the opId from a slice of tags if present.
func ParseOpTag(in []string) (string, bool) {
	for _, t := range in {
		if strings.HasPrefix(t, TagHostOpPrefix) && len(t) > len(TagHostOpPrefix) {
			return t[len(TagHostOpPrefix):], true
		}
	}
	return "", false
}

// SanitizeID converts an arbitrary identifier (e.g., clusterId) into a MAAS tag-safe
// token: lowercase, [a-z0-9-] only, collapsing invalid sequences into '-'.
func SanitizeID(id string) string {
	if id == "" {
		return id
	}
	b := make([]rune, 0, len(id))
	prevDash := false
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b = append(b, r)
			prevDash = false
			continue
		}
		if !prevDash {
			b = append(b, '-')
			prevDash = true
		}
	}
	// trim leading/trailing '-'
	s := strings.Trim(string(b), "-")
	// optional: limit length to 63 chars
	if len(s) > 63 {
		s = s[:63]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		return "x"
	}
	return s
}

// BuildReadyHostTag builds the per-WLC readiness host tag for the given session.
// Example: maas-lxd-ready-<clusterId>-op-<opID>
func BuildReadyHostTag(clusterID, opID string) string {
	return TagVMReadyOpPrefix + SanitizeID(clusterID) + "-op-" + opID
}

// BuildVMReadyOpTag builds the VM-level readiness tag for the given opID.
// Example: maas-lxd-ready-op-<opID>
func BuildVMReadyOpTag(opID string) string {
	return TagVMReadyOpPrefix + opID
}

// GetHostOpID reads MAAS host tags and returns the active maintenance opID if present.
// It looks for tags with prefix maas-lxd-hcp-op-<uuid>.
func GetHostOpID(ctx context.Context, client maasclient.ClientSetInterface, hostSystemID string) (string, bool, error) {
	if client == nil || hostSystemID == "" {
		return "", false, nil
	}
	m, err := client.Machines().Machine(hostSystemID).Get(ctx)
	if err != nil {
		return "", false, err
	}
	opID, ok := ParseOpTag(m.Tags())
	return opID, ok, nil
}

// TagVMReadyOp ensures the VM is tagged with maas-lxd-ready-op-<opID>.
// It is idempotent and best-effort: creates the tag if missing, then assigns it.
func TagVMReadyOp(ctx context.Context, client maasclient.ClientSetInterface, systemID, opID string) error {
	if client == nil || systemID == "" || opID == "" {
		return nil
	}
	tag := BuildVMReadyOpTag(opID)
	ts := client.Tags()
	if ts == nil {
		return nil
	}
	// Ignore error from Create: tag may already exist, which is safe and expected.
	_ = ts.Create(ctx, tag)
	return ts.Assign(ctx, tag, systemID)
}
