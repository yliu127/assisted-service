package v1beta1

import (
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ClusterSpecSyncedCondition string = "SpecSynced"

	ClusterCompletedCondition string = hivev1.ClusterInstallCompleted

	ClusterRequirementsMetCondition  string = hivev1.ClusterInstallRequirementsMet
	ClusterReadyReason               string = "ClusterIsReady"
	ClusterReadyMsg                  string = "The cluster is ready to begin the installation"
	ClusterNotReadyReason            string = "ClusterNotReady"
	ClusterNotReadyMsg               string = "The cluster is not ready to begin the installation"
	ClusterAlreadyInstallingReason   string = "ClusterAlreadyInstalling"
	ClusterAlreadyInstallingMsg      string = "The cluster requirements are met"
	ClusterInstallationStoppedReason string = "ClusterInstallationStopped"
	ClusterInstallationStoppedMsg    string = "The cluster installation stopped"
	ClusterInsufficientAgentsReason  string = "InsufficientAgents"
	ClusterInsufficientAgentsMsg     string = "The cluster currently requires %d agents but only %d have registered"
	ClusterUnapprovedAgentsReason    string = "UnapprovedAgents"
	ClusterUnapprovedAgentsMsg       string = "The installation is pending on the approval of %d agents"
	ClusterAdditionalAgentsReason    string = "AdditionalAgents"
	ClusterAdditionalAgentsMsg       string = "The cluster currently requires exactly %d agents but have %d registered"

	ClusterValidatedCondition        string = "Validated"
	ClusterValidationsOKMsg          string = "The cluster's validations are passing"
	ClusterValidationsUnknownMsg     string = "The cluster's validations have not yet been calculated"
	ClusterValidationsFailingMsg     string = "The cluster's validations are failing:"
	ClusterValidationsUserPendingMsg string = "The cluster's validations are pending for user:"

	ClusterFailedCondition string = hivev1.ClusterInstallFailed
	ClusterFailedReason    string = "InstallationFailed"
	ClusterFailedMsg       string = "The installation failed:"
	ClusterNotFailedReason string = "InstallationNotFailed"
	ClusterNotFailedMsg    string = "The installation has not failed"

	ClusterStoppedCondition       string = hivev1.ClusterInstallStopped
	ClusterStoppedFailedReason    string = "InstallationFailed"
	ClusterStoppedFailedMsg       string = "The installation has stopped due to error"
	ClusterStoppedCanceledReason  string = "InstallationCancelled"
	ClusterStoppedCanceledMsg     string = "The installation has stopped because it was cancelled"
	ClusterStoppedCompletedReason string = "InstallationCompleted"
	ClusterStoppedCompletedMsg    string = "The installation has stopped because it completed successfully"
	ClusterNotStoppedReason       string = "InstallationNotStopped"
	ClusterNotStoppedMsg          string = "The installation is waiting to start or in progress"

	ClusterInstalledReason              string = "InstallationCompleted"
	ClusterInstalledMsg                 string = "The installation has completed:"
	ClusterInstallationFailedReason     string = "InstallationFailed"
	ClusterInstallationFailedMsg        string = "The installation has failed:"
	ClusterInstallationNotStartedReason string = "InstallationNotStarted"
	ClusterInstallationNotStartedMsg    string = "The installation has not yet started"
	ClusterInstallationOnHoldReason     string = "InstallationOnHold"
	ClusterInstallationOnHoldMsg        string = "The installation is on hold. To unhold set holdInstallation to false"
	ClusterInstallationInProgressReason string = "InstallationInProgress"
	ClusterInstallationInProgressMsg    string = "The installation is in progress:"
	ClusterUnknownStatusReason          string = "UnknownStatus"
	ClusterUnknownStatusMsg             string = "The installation status is currently not recognized:"

	ClusterValidationsPassingReason     string = "ValidationsPassing"
	ClusterValidationsUnknownReason     string = "ValidationsUnknown"
	ClusterValidationsFailingReason     string = "ValidationsFailing"
	ClusterValidationsUserPendingReason string = "ValidationsUserPending"

	ClusterNotAvailableReason string = "NotAvailable"
	ClusterNotAvailableMsg    string = "Information not available"

	ClusterSyncedOkReason     string = "SyncOK"
	ClusterSyncedOkMsg        string = "The Spec has been successfully applied"
	ClusterBackendErrorReason string = "BackendError"
	ClusterBackendErrorMsg    string = "The Spec could not be synced due to backend error:"
	ClusterInputErrorReason   string = "InputError"
	ClusterInputErrorMsg      string = "The Spec could not be synced due to an input error:"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// AgentClusterInstall represents a request to provision an agent based cluster.
//
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=agentclusterinstalls,shortName=aci
type AgentClusterInstall struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentClusterInstallSpec   `json:"spec"`
	Status AgentClusterInstallStatus `json:"status,omitempty"`
}

// AgentClusterInstallSpec defines the desired state of the AgentClusterInstall.
type AgentClusterInstallSpec struct {

	// ImageSetRef is a reference to a ClusterImageSet. The release image specified in the ClusterImageSet will be used
	// to install the cluster.
	ImageSetRef hivev1.ClusterImageSetReference `json:"imageSetRef"`

	// ClusterDeploymentRef is a reference to the ClusterDeployment associated with this AgentClusterInstall.
	ClusterDeploymentRef corev1.LocalObjectReference `json:"clusterDeploymentRef"`

	// ClusterMetadata contains metadata information about the installed cluster. It should be populated once the cluster install is completed. (it can be populated sooner if desired, but Hive will not copy back to ClusterDeployment until the Installed condition goes True.
	ClusterMetadata *hivev1.ClusterMetadata `json:"clusterMetadata,omitempty"`

	// ManifestsConfigMapRef is a reference to user-provided manifests to
	// add to or replace manifests that are generated by the installer.
	ManifestsConfigMapRef *corev1.LocalObjectReference `json:"manifestsConfigMapRef,omitempty"`

	// Networking is the configuration for the pod network provider in
	// the cluster.
	Networking Networking `json:"networking"`

	// SSHPublicKey will be added to all cluster hosts for use in debugging.
	// +optional
	SSHPublicKey string `json:"sshPublicKey,omitempty"`

	// ProvisionRequirements defines configuration for when the installation is ready to be launched automatically.
	ProvisionRequirements ProvisionRequirements `json:"provisionRequirements"`

	// ControlPlane is the configuration for the machines that comprise the
	// control plane.
	// +optional
	ControlPlane *AgentMachinePool `json:"controlPlane,omitempty"`

	// Compute is the configuration for the machines that comprise the
	// compute nodes.
	// +optional
	Compute []AgentMachinePool `json:"compute,omitempty"`

	// APIVIP is the virtual IP used to reach the OpenShift cluster's API.
	// +optional
	APIVIP string `json:"apiVIP,omitempty"`

	// IngressVIP is the virtual IP used for cluster ingress traffic.
	// +optional
	IngressVIP string `json:"ingressVIP,omitempty"`

	// HoldInstallation will prevent installation from happening when true.
	// Inspection and validation will proceed as usual, but once the RequirementsMet condition is true,
	// installation will not begin until this field is set to false.
	// +optional
	HoldInstallation bool `json:"holdInstallation,omitempty"`
}

// AgentClusterInstallStatus defines the observed state of the AgentClusterInstall.
type AgentClusterInstallStatus struct {
	// Conditions includes more detailed status for the cluster install.
	// +optional
	Conditions []hivev1.ClusterInstallCondition `json:"conditions,omitempty"`

	// ControlPlaneAgentsDiscovered is the number of Agents currently linked to this ClusterDeployment.
	// +optional
	ControlPlaneAgentsDiscovered int `json:"controlPlaneAgentsDiscovered,omitempty"`
	// ControlPlaneAgentsDiscovered is the number of Agents currently linked to this ClusterDeployment that are ready for use.
	// +optional
	ControlPlaneAgentsReady int `json:"controlPlaneAgentsReady,omitempty"`
	// WorkerAgentsDiscovered is the number of worker Agents currently linked to this ClusterDeployment.
	// +optional
	WorkerAgentsDiscovered int `json:"workerAgentsDiscovered,omitempty"`
	// WorkerAgentsDiscovered is the number of worker Agents currently linked to this ClusterDeployment that are ready for use.
	// +optional
	WorkerAgentsReady int `json:"workerAgentsReady,omitempty"`

	ConnectivityMajorityGroups string `json:"connectivityMajorityGroups,omitempty"`
	// MachineNetwork is the list of IP address pools for machines.
	// +optional
	MachineNetwork []MachineNetworkEntry `json:"machineNetwork,omitempty"`
	// DebugInfo includes information for debugging the installation process.
	// +optional
	DebugInfo DebugInfo `json:"debugInfo"`
}

type DebugInfo struct {
	// EventsURL specifies an HTTP/S URL that contains events which occured during the cluster installation process
	// +optional
	EventsURL string `json:"eventsURL"`

	// LogsURL specifies a url for download controller logs tar file.
	// +optional
	LogsURL string `json:"logsURL"`
	// Current state of the AgentClusterInstall
	State string `json:"state,omitempty"`
	//Additional information pertaining to the status of the AgentClusterInstall
	// +optional
	StateInfo string `json:"stateInfo,omitempty"`
}

// Networking defines the pod network provider in the cluster.
type Networking struct {
	// MachineNetwork is the list of IP address pools for machines.
	// +optional
	MachineNetwork []MachineNetworkEntry `json:"machineNetwork,omitempty"`

	// ClusterNetwork is the list of IP address pools for pods.
	// Default is 10.128.0.0/14 and a host prefix of /23.
	//
	// +optional
	ClusterNetwork []ClusterNetworkEntry `json:"clusterNetwork,omitempty"`

	// ServiceNetwork is the list of IP address pools for services.
	// Default is 172.30.0.0/16.
	// NOTE: currently only one entry is supported.
	//
	// +kubebuilder:validation:MaxItems=1
	// +optional
	ServiceNetwork []string `json:"serviceNetwork,omitempty"`

	//NetworkType is the Container Network Interface (CNI) plug-in to install
	//The default value is OpenShiftSDN for IPv4 and OVNKubernetes for IPv6
	//
	// +kubebuilder:validation:Enum=OpenShiftSDN;OVNKubernetes
	// +optional
	NetworkType string `json:"networkType,omitempty"`
}

// MachineNetworkEntry is a single IP address block for node IP blocks.
type MachineNetworkEntry struct {
	// CIDR is the IP block address pool for machines within the cluster.
	CIDR string `json:"cidr"`
}

// ClusterNetworkEntry is a single IP address block for pod IP blocks. IP blocks
// are allocated with size 2^HostSubnetLength.
type ClusterNetworkEntry struct {
	// CIDR is the IP block address pool.
	CIDR string `json:"cidr"`

	// HostPrefix is the prefix size to allocate to each node from the CIDR.
	// For example, 24 would allocate 2^8=256 adresses to each node. If this
	// field is not used by the plugin, it can be left unset.
	// +optional
	HostPrefix int32 `json:"hostPrefix,omitempty"`
}

// ProvisionRequirements defines configuration for when the installation is ready to be launched automatically.
type ProvisionRequirements struct {

	// ControlPlaneAgents is the number of matching approved and ready Agents with the control plane role
	// required to launch the install. Must be either 1 or 3.
	ControlPlaneAgents int `json:"controlPlaneAgents"`

	// WorkerAgents is the minimum number of matching approved and ready Agents with the worker role
	// required to launch the install.
	// +kubebuilder:validation:Minimum=0
	// +optional
	WorkerAgents int `json:"workerAgents,omitempty"`
}

// HyperthreadingMode is the mode of hyperthreading for a machine.
// +kubebuilder:validation:Enum="";Enabled;Disabled
type HyperthreadingMode string

const (
	// HyperthreadingEnabled indicates that hyperthreading is enabled.
	HyperthreadingEnabled HyperthreadingMode = "Enabled"
	// HyperthreadingDisabled indicates that hyperthreading is disabled.
	HyperthreadingDisabled HyperthreadingMode = "Disabled"
)

const (
	MasterAgentMachinePool string = "master"
	WorkerAgentMachinePool string = "worker"
)

// AgentMachinePool is a pool of machines to be installed.
type AgentMachinePool struct {
	// Hyperthreading determines the mode of hyperthreading that machines in the
	// pool will utilize.
	// Default is for hyperthreading to be enabled.
	//
	// +kubebuilder:default=Enabled
	// +optional
	Hyperthreading HyperthreadingMode `json:"hyperthreading,omitempty"`

	// Name is the name of the machine pool.
	// For the control plane machine pool, the name will always be "master".
	// For the compute machine pools, the only valid name is "worker".
	Name string `json:"name"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// AgentClusterInstallList contains a list of AgentClusterInstalls
type AgentClusterInstallList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentClusterInstall `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentClusterInstall{}, &AgentClusterInstallList{})
}
