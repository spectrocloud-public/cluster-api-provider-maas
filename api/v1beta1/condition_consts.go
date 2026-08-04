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

import clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

// Conditions and condition Reasons for the MAAS Machine object

const (
	// MachineDeployedCondition documents the status of the deployment of a machine

	MachineDeployedCondition clusterv1.ConditionType = "MachineDeployed"

	// WaitingForClusterInfrastructureReason (Severity=Info) documents a MachineMachine waiting for the cluster
	// infrastructure to be ready before starting to deploy the machine that provides the MachineMachine
	// infrastructure.
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"

	// WaitingForBootstrapDataReason (Severity=Info) documents a MachineMachine waiting for the bootstrap
	// script to be ready before starting to create the container that provides the MachineMachine infrastructure.
	WaitingForBootstrapDataReason = "WaitingForBootstrapData"

	// WaitingForStaticIPReason (Severity=Info) documents a MachineMachine waiting for the static IP
	// to be populated by an external controller before proceeding with machine deployment.
	WaitingForStaticIPReason = "WaitingForStaticIP"

	// MachineDeployingReason
	MachineDeployingReason = "MachineDeploying"

	// MachineTerminatedReason
	MachineTerminatedReason = "MachineTerminatedReason"

	// MachineDeployingReason
	MachinePoweredOffReason = "MachinePoweredOff"

	// MachineNotFoundReason used when the machine couldn't be retrieved.
	MachineNotFoundReason = "MachineNotFound"

	// MachineDeployFailedReason (Severity=Warning) documents a MachineMachine controller detecting
	// an error while deploying the MaaS machine that provides the MachineMachine infrastructure; those kind of
	// errors are usually transient and failed provisioning are automatically re-tried by the controller.
	MachineDeployFailedReason = "MachineDeployFailed"

	// MachineDeployStartedReason (Severity=Info) documents a MachineMachine controller started deploying
	MachineDeployStartedReason = "MachineDeployStartedReason"
)

const (
	// Only applicable to control plane machines. DNSAttachedCondition will report true when a control plane is successfully registered with an DNS
	// When set to false, severity can be an Error if the subnet is not found or unavailable in the instance's AZ
	DNSAttachedCondition clusterv1.ConditionType = "DNSAttached"

	DNSDetachPending = "DNSDetachPending"
	DNSAttachPending = "DNSAttachPending"
)

// Cluster Conditions

const (
	// DNSReadyCondition documents the availability of the container that implements the cluster DNS.
	DNSReadyCondition clusterv1.ConditionType = "LoadBalancerReady"

	// LoadBalancerProvisioningFailedReason (Severity=Warning) documents a MAASCluster controller detecting
	// dns reconcile failure will be retried
	DNSFailedReason = "LoadBalancerFailed"

	WaitForDNSNameReason = "WaitForDNSName"
)

const (
	// APIServerAvailableCondition documents whether API server is reachable
	APIServerAvailableCondition clusterv1.ConditionType = "APIServerAvailable"

	// APIServerNotReadyReason api server isn't responding
	APIServerNotReadyReason = "APIServerNotReady"
)

const (
	// LXDReadyCondition documents whether LXD hosts are properly configured
	LXDReadyCondition clusterv1.ConditionType = "LXDReady"

	// LXDFailedReason documents LXD host setup failure
	LXDFailedReason = "LXDFailed"

	// LXDSetupPendingReason documents LXD host setup in progress
	LXDSetupPendingReason = "LXDSetupPending"
)
