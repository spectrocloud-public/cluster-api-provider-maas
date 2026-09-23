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
	"encoding/json"
	"fmt"
	"time"

	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8suuid "k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Keys used in the session ConfigMap data (internal use)
	cmKeyOpID                       = "opId"
	cmKeyStatus                     = "status"
	cmKeyStartedAt                  = "startedAt"
	cmKeyCurrentHost                = "currentHost"
	cmKeyProgress                   = "progress"
	cmKeyActiveSessions             = "activeSessions"             // Max 1 or 0
	cmKeyAffectedWLCClusters        = "affectedWLCClusters"        // JSON array of cluster IDs
	cmKeyPendingReadyVMReplacements = "pendingReadyVMReplacements" // JSON array of VM system IDs
	cmKeyNewVMSystemID              = "newVMSystemID"              // New VM system ID for replacement
	cmKeyEvictionStarted            = "evictionStarted"            // Set when Step 4 claims the session and starts eviction

	// Exported keys for external use (e.g., machine.go, vmevacuation_controller.go)
	CmKeyOpID             = cmKeyOpID
	CmKeyStatus           = cmKeyStatus
	CmKeyNewVMSystemID    = cmKeyNewVMSystemID
	CmKeyCurrentHost      = cmKeyCurrentHost
	CmKeyStartedAt        = cmKeyStartedAt
	CmKeyEvictionStarted  = cmKeyEvictionStarted

	// Optional trigger keys to initiate a session
	CmKeyTriggerStart = "start"
	CmKeyTriggerHost  = "hostSystemID"
)

// LoadSession loads the session ConfigMap and returns parsed state if present.
// If the ConfigMap doesn't exist, it returns an empty state with nil ConfigMap and no error.
func LoadSession(ctx context.Context, c client.Client, namespace string) (State, *corev1.ConfigMap, error) {
	key := types.NamespacedName{Namespace: namespace, Name: SessionCMName}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, key, cm); err != nil {
		if apierrs.IsNotFound(err) {
			return State{}, nil, nil
		}
		return State{}, nil, err
	}
	st := State{}
	if cm.Data == nil {
		return st, cm, nil
	}
	st.OpID = cm.Data[cmKeyOpID]
	st.Status = Status(cm.Data[cmKeyStatus])
	st.CurrentHost = cm.Data[cmKeyCurrentHost]

	// Parse timestamp
	if ts := cm.Data[cmKeyStartedAt]; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			st.StartedAt = t
		}
	}

	// Parse active sessions count
	if activeSessions := cm.Data[cmKeyActiveSessions]; activeSessions != "" {
		var count int
		if _, err := fmt.Sscanf(activeSessions, "%d", &count); err == nil {
			st.ActiveSessions = count
		}
	}

	// Parse affected WLC clusters
	if affectedClusters := cm.Data[cmKeyAffectedWLCClusters]; affectedClusters != "" {
		var clusters []string
		if err := json.Unmarshal([]byte(affectedClusters), &clusters); err == nil {
			st.AffectedWLCClusters = clusters
		}
	}

	// Parse pending ready VM replacements
	if pendingVMs := cm.Data[cmKeyPendingReadyVMReplacements]; pendingVMs != "" {
		var vms []string
		if err := json.Unmarshal([]byte(pendingVMs), &vms); err == nil {
			st.PendingReadyVMReplacements = vms
		}
	}

	return st, cm, nil
}

// SaveSession writes the given state back to the session ConfigMap.
// Creates the ConfigMap if it doesn't exist.
func SaveSession(ctx context.Context, c client.Client, namespace string, st State) error {
	key := types.NamespacedName{Namespace: namespace, Name: SessionCMName}
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, key, cm)
	if apierrs.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: SessionCMName, Namespace: namespace}}
	} else if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	// Save basic fields
	cm.Data[cmKeyOpID] = st.OpID
	cm.Data[cmKeyStatus] = string(st.Status)
	cm.Data[cmKeyCurrentHost] = st.CurrentHost
	cm.Data[cmKeyStartedAt] = st.StartedAt.UTC().Format(time.RFC3339)

	// Save active sessions count
	cm.Data[cmKeyActiveSessions] = fmt.Sprintf("%d", st.ActiveSessions)

	// Save affected WLC clusters as JSON
	if len(st.AffectedWLCClusters) > 0 {
		if clustersJSON, err := json.Marshal(st.AffectedWLCClusters); err == nil {
			cm.Data[cmKeyAffectedWLCClusters] = string(clustersJSON)
		} else {
			return errors.Wrapf(err, "failed to serialize affected wlc %s", st.AffectedWLCClusters)
		}
	} else {
		cm.Data[cmKeyAffectedWLCClusters] = "[]"
	}

	// Save pending ready VM replacements as JSON
	if len(st.PendingReadyVMReplacements) > 0 {
		if vmsJSON, err := json.Marshal(st.PendingReadyVMReplacements); err == nil {
			cm.Data[cmKeyPendingReadyVMReplacements] = string(vmsJSON)
		} else {
			return errors.Wrapf(err, "failed to serialize pending wlc %s", st.PendingReadyVMReplacements)
		}
	} else {
		cm.Data[cmKeyPendingReadyVMReplacements] = "[]"
	}

	// Ensure progress field exists
	if cm.Data[cmKeyProgress] == "" {
		cm.Data[cmKeyProgress] = "{}"
	}

	// Create or update
	if cm.UID == "" {
		return c.Create(ctx, cm)
	}
	return c.Update(ctx, cm)
}

// StartSession initializes a new session if none Active is present. It will
// generate a fresh opId and set status Active with ActiveSessions = 1.
func StartSession(ctx context.Context, c client.Client, namespace, currentHost string) (State, error) {
	st, cm, err := LoadSession(ctx, c, namespace)
	if err != nil {
		return State{}, err
	}
	// If there's already an active session, return it
	if cm != nil && st.Status == StatusActive && st.ActiveSessions == 1 {
		return st, nil
	}
	newID := string(k8suuid.NewUUID())
	st = State{
		OpID:                       newID,
		Status:                     StatusActive,
		StartedAt:                  time.Now().UTC(),
		CurrentHost:                currentHost,
		ActiveSessions:             1, // Mark session as active
		AffectedWLCClusters:        []string{},
		PendingReadyVMReplacements: []string{},
	}
	if err := SaveSession(ctx, c, namespace, st); err != nil {
		return State{}, err
	}
	return st, nil
}

// CompleteSession marks the current session as completed and clears all session data.
func CompleteSession(ctx context.Context, c client.Client, namespace string) error {
	st, _, err := LoadSession(ctx, c, namespace)
	if err != nil {
		return err
	}
	if st.Status == StatusCompleted {
		return nil // Already completed
	}

	// Clear all session data
	st.Status = StatusCompleted
	st.ActiveSessions = 0
	st.CurrentHost = "" // Clear the current host
	st.AffectedWLCClusters = []string{}
	st.PendingReadyVMReplacements = []string{}

	return SaveSession(ctx, c, namespace, st)
}

// ShouldStartFromTrigger inspects optional trigger fields in the session CM.
func ShouldStartFromTrigger(cm *corev1.ConfigMap) (bool, string) {
	if cm == nil || cm.Data == nil {
		return false, ""
	}
	if cm.Data[CmKeyTriggerStart] == "true" {
		return true, cm.Data[CmKeyTriggerHost]
	}
	return false, ""
}

// UpdateProgress stores arbitrary progress map as JSON in the session CM.
func UpdateProgress(cm *corev1.ConfigMap, progress map[string]string) {
	if cm == nil {
		return
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	if b, err := json.Marshal(progress); err == nil {
		cm.Data[cmKeyProgress] = string(b)
	}
}
