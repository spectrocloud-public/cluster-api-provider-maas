/*
Copyright 2020 The Kubernetes Authors.

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

package v1beta1

// Conditions and condition Reasons for the MAAS Machine object.
// Condition types are plain strings to match metav1.Condition.Type, and every
// condition must carry a non-empty Reason (metav1.Condition requirement).

const (
	// MachineDeployedCondition documents the status of the deployment of a machine
	MachineDeployedCondition string = "MachineDeployed"

	// MachineDeployedReason documents a machine that has been successfully deployed.
	MachineDeployedReason = "MachineDeployed"

	// DeletedReason documents an object that is being deleted.
	DeletedReason = "Deleted"

	// WaitingForClusterInfrastructureReason documents a MachineMachine waiting for the cluster
	// infrastructure to be ready before starting to deploy the machine that provides the MachineMachine
	// infrastructure.
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"

	// WaitingForBootstrapDataReason documents a MachineMachine waiting for the bootstrap
	// script to be ready before starting to create the container that provides the MachineMachine infrastructure.
	WaitingForBootstrapDataReason = "WaitingForBootstrapData"

	// MachineDeployingReason
	MachineDeployingReason = "MachineDeploying"

	// MachineTerminatedReason
	MachineTerminatedReason = "MachineTerminatedReason"

	// MachinePoweredOffReason
	MachinePoweredOffReason = "MachinePoweredOff"

	// MachineStateUndefinedReason documents a machine in an unhandled/undefined MAAS state.
	MachineStateUndefinedReason = "MachineStateUndefined"

	// MachineNotFoundReason used when the machine couldn't be retrieved.
	MachineNotFoundReason = "MachineNotFound"

	// MachineDeployFailedReason documents a MachineMachine controller detecting
	// an error while deploying the MaaS machine that provides the MachineMachine infrastructure; those kind of
	// errors are usually transient and failed provisioning are automatically re-tried by the controller.
	MachineDeployFailedReason = "MachineDeployFailed"

	// MachineDeployStartedReason documents a MachineMachine controller started deploying
	MachineDeployStartedReason = "MachineDeployStartedReason"
)

const (
	// Only applicable to control plane machines. DNSAttachedCondition will report true when a control plane is successfully registered with an DNS
	// When set to false, severity can be an Error if the subnet is not found or unavailable in the instance's AZ
	DNSAttachedCondition string = "DNSAttached"

	// DNSAttachedReason documents a control plane that is registered with DNS.
	DNSAttachedReason = "DNSAttached"

	DNSDetachPending = "DNSDetachPending"
	DNSAttachPending = "DNSAttachPending"
)

// Cluster Conditions

const (
	// DNSReadyCondition documents the availability of the container that implements the cluster DNS.
	DNSReadyCondition string = "LoadBalancerReady"

	// DNSReadyReason documents that the cluster DNS is ready.
	DNSReadyReason = "LoadBalancerReady"

	// DNSFailedReason documents a MAASCluster controller detecting
	// dns reconcile failure will be retried
	DNSFailedReason = "LoadBalancerFailed"

	// DNSIPResolutionFailedReason documents a DNS IP resolution failure.
	DNSIPResolutionFailedReason = "DNSIPResolutionFailed"

	WaitForDNSNameReason = "WaitForDNSName"
)

const (
	// APIServerAvailableCondition documents whether API server is reachable
	APIServerAvailableCondition string = "APIServerAvailable"

	// APIServerAvailableReason documents that the API server is reachable.
	APIServerAvailableReason = "APIServerAvailable"

	// APIServerNotReadyReason api server isn't responding
	APIServerNotReadyReason = "APIServerNotReady"
)

const (
	// LXDReadyCondition documents whether LXD hosts are properly configured
	LXDReadyCondition string = "LXDReady"

	// LXDReadyReason documents that LXD hosts are properly configured.
	LXDReadyReason = "LXDReady"

	// LXDFailedReason documents LXD host setup failure
	LXDFailedReason = "LXDFailed"

	// LXDSetupPendingReason documents LXD host setup in progress
	LXDSetupPendingReason = "LXDSetupPending"
)
