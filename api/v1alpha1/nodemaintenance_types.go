package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ActionType identifies a node lifecycle action.
// +kubebuilder:validation:Enum=Cordon;Drain;Uncordon;Script
type ActionType string

const (
	ActionCordon   ActionType = "Cordon"
	ActionDrain    ActionType = "Drain"
	ActionUncordon ActionType = "Uncordon"
	ActionScript   ActionType = "Script"
)

// Phase is the high-level state of a NodeMaintenance object or a single node.
// +kubebuilder:validation:Enum=Pending;InProgress;Completed;Failed
type Phase string

const (
	PhasePending    Phase = "Pending"
	PhaseInProgress Phase = "InProgress"
	PhaseCompleted  Phase = "Completed"
	PhaseFailed     Phase = "Failed"
)

// NodeMaintenanceSpec is the desired state of a node maintenance run.
type NodeMaintenanceSpec struct {
	// Paused, when true, stops the controller from advancing the run. The
	// CLI uses this for the attach-then-run workflow:
	//   kubectl nm create ... --paused
	//   kubectl nm attach <name> ./script.sh
	//   kubectl nm run <name>
	// +optional
	Paused bool `json:"paused,omitempty"`

	// AllNodes, when true, targets every node in the cluster and ignores
	// NodeSelector / NodeNames.
	// +optional
	AllNodes bool `json:"allNodes,omitempty"`

	// NodeSelector selects target nodes by labels. Ignored when NodeNames is
	// set or AllNodes is true.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// NodeNames is an explicit list of node names to act on. Takes precedence
	// over NodeSelector when non-empty. Ignored when AllNodes is true.
	// +optional
	NodeNames []string `json:"nodeNames,omitempty"`

	// Script attaches a single user script to this run. When non-nil and
	// Actions is empty, the controller synthesizes a default action sequence
	// of [Cordon, Script, Uncordon].
	// +optional
	Script *ScriptSpec `json:"script,omitempty"`

	// Actions is the ordered list of actions executed against every target
	// node. When empty and Script is set, defaults to [Cordon, Script, Uncordon].
	// +optional
	Actions []ActionSpec `json:"actions,omitempty"`

	// Strategy controls global safety constraints during the run.
	// +optional
	Strategy Strategy `json:"strategy,omitempty"`
}

// ScriptSpec describes a single user-supplied script to execute on each
// target node. Exactly one of Inline or ConfigMapRef should be set; if both
// are set, ConfigMapRef wins.
type ScriptSpec struct {
	// Inline is the script body. When set, the controller materializes it
	// into a ConfigMap named "nm-<nm-name>-script" in the runner namespace.
	// +optional
	Inline string `json:"inline,omitempty"`

	// ConfigMapRef points at a pre-existing ConfigMap holding the script.
	// +optional
	ConfigMapRef *ScriptConfigMapRef `json:"configMapRef,omitempty"`

	// Image is the runner container image. Defaults to "alpine:3.19".
	// +optional
	Image string `json:"image,omitempty"`

	// TimeoutSeconds caps a single per-node script execution. Defaults to 600.
	// +optional
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`

	// RunOnHost, when true (default), executes the script in the host mount
	// namespace via nsenter, giving it access to host binaries and filesystem.
	// When false, the script runs inside the runner Pod only.
	// +optional
	RunOnHost *bool `json:"runOnHost,omitempty"`

	// Env is a list of name/value pairs passed to the script's environment.
	// +optional
	Env []EnvVar `json:"env,omitempty"`
}

// ScriptConfigMapRef references the ConfigMap entry that contains the script.
// The ConfigMap is always read from the controller's runner namespace — Pods
// can only mount ConfigMaps from their own namespace, and the runner Pod is
// created in that namespace.
type ScriptConfigMapRef struct {
	Name string `json:"name"`
	// Key defaults to "script.sh" when empty.
	// +optional
	Key string `json:"key,omitempty"`
}

// EnvVar is a simple name/value pair passed into the runner Pod.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ActionSpec selects an action and configures its options.
type ActionSpec struct {
	// Type is the action kind. One of Cordon, Drain, Uncordon, Script.
	Type ActionType `json:"type"`

	// DrainOptions tunes the Drain action. Ignored for other types.
	// +optional
	DrainOptions *DrainOptions `json:"drainOptions,omitempty"`
}

// DrainOptions configures pod eviction behavior.
type DrainOptions struct {
	// +optional
	GracePeriodSeconds *int64 `json:"gracePeriodSeconds,omitempty"`
	// +optional
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
	// +optional
	IgnoreDaemonSets bool `json:"ignoreDaemonSets,omitempty"`
}

// Strategy controls how many nodes can be in-flight at once.
type Strategy struct {
	// MaxUnavailable is the maximum number of nodes that can be in any
	// non-terminal phase at the same time. Defaults to 1 when unset.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxUnavailable int `json:"maxUnavailable,omitempty"`

	// AtOnce, when true, runs against every target node in parallel. Wins
	// over MaxUnavailable.
	// +optional
	AtOnce bool `json:"atOnce,omitempty"`
}

// NodeMaintenanceStatus is the observed state of a maintenance run.
type NodeMaintenanceStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	Nodes []NodeStatus `json:"nodes,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Summary rolls per-node phase counts up for at-a-glance progress in
	// `kubectl get nm`. Recomputed on every reconcile from Nodes.
	// +optional
	Summary StatusSummary `json:"summary,omitempty"`
}

// StatusSummary holds per-phase node counts derived from Status.Nodes.
type StatusSummary struct {
	// +optional
	Total int32 `json:"total,omitempty"`
	// +optional
	Pending int32 `json:"pending,omitempty"`
	// +optional
	InProgress int32 `json:"inProgress,omitempty"`
	// +optional
	Completed int32 `json:"completed,omitempty"`
	// +optional
	Failed int32 `json:"failed,omitempty"`
}

// NodeStatus tracks a single node through the action sequence.
type NodeStatus struct {
	Name               string       `json:"name"`
	Phase              Phase        `json:"phase"`
	CurrentAction      string       `json:"currentAction,omitempty"`
	CompletedActions   []string     `json:"completedActions,omitempty"`
	Message            string       `json:"message,omitempty"`
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// ScriptPodName is the name of the runner Pod that executed (or is
	// executing) the Script action for this node. Useful for `kubectl nm logs`.
	// +optional
	ScriptPodName string `json:"scriptPodName,omitempty"`

	// ScriptExitCode is the exit code of the script container once the
	// Script action terminates. Set to a pointer so "unset" and "0" are
	// distinguishable.
	// +optional
	ScriptExitCode *int32 `json:"scriptExitCode,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nm
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="Targets",type=string,JSONPath=`.metadata.annotations.ko\.io/targets`
// +kubebuilder:printcolumn:name="Done",type=integer,JSONPath=`.status.summary.completed`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.summary.total`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.summary.pending`,priority=1
// +kubebuilder:printcolumn:name="InProgress",type=integer,JSONPath=`.status.summary.inProgress`,priority=1
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.summary.failed`,priority=1
// +kubebuilder:printcolumn:name="AllNodes",type=boolean,JSONPath=`.spec.allNodes`,priority=1
// +kubebuilder:printcolumn:name="Selector",type=string,JSONPath=`.spec.nodeSelector`,priority=1
// +kubebuilder:printcolumn:name="NodeNames",type=string,JSONPath=`.spec.nodeNames`,priority=1

// NodeMaintenance is the Schema for the nodemaintenances API.
type NodeMaintenance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeMaintenanceSpec   `json:"spec,omitempty"`
	Status NodeMaintenanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeMaintenanceList contains a list of NodeMaintenance.
type NodeMaintenanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeMaintenance `json:"items"`
}
