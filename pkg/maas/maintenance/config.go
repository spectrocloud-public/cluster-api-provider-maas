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

import "time"

// ForcePolicy determines how HMC proceeds when evacuation gates are not
// satisfied within configured timeouts.
type ForcePolicy string

const (
	// ForceHalt stops the maintenance session and requires operator action.
	ForceHalt ForcePolicy = "halt"
	// ForceAllow proceeds to decommission despite missing acks (unsafe).
	ForceAllow ForcePolicy = "force"
	// ForceRelax allows relaxing placement constraints and retrying.
	ForceRelax ForcePolicy = "relax"
)

// HMCConfig configures Host Maintenance Controller timeouts and policy.
type HMCConfig struct {
	PerWLCMoveTimeout       time.Duration
	PerHostWaveTimeout      time.Duration
	EvacuationCheckInterval time.Duration
	Policy                  ForcePolicy
}

// VECConfig configures VM Evacuation Controller timeouts and checks.
type VECConfig struct {
	MoveTimeout  time.Duration
	RetryBackoff time.Duration
	CheckAPIPath string
}

// ReleaseConfig configures VM release retries and per-call timeouts.
type ReleaseConfig struct {
	CallTimeout    time.Duration
	RetryBudget    time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// DefaultReleaseConfig returns the S4 defaults.
func DefaultReleaseConfig() ReleaseConfig {
	return ReleaseConfig{
		CallTimeout:    3 * time.Minute,
		RetryBudget:    15 * time.Minute,
		InitialBackoff: 30 * time.Second,
		MaxBackoff:     5 * time.Minute,
	}
}

// DefaultHMCConfig returns conservative defaults for HMC operation.
func DefaultHMCConfig() HMCConfig {
	return HMCConfig{
		PerWLCMoveTimeout:       20 * time.Minute,
		PerHostWaveTimeout:      60 * time.Minute,
		EvacuationCheckInterval: 30 * time.Second,
		Policy:                  ForceHalt,
	}
}

// DefaultVECConfig returns conservative defaults for VEC operation.
func DefaultVECConfig() VECConfig {
	return VECConfig{
		MoveTimeout:  20 * time.Minute,
		RetryBackoff: 30 * time.Second,
		CheckAPIPath: "/readyz",
	}
}
