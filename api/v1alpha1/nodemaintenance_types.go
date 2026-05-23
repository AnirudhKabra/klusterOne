package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ActionType identifies a node lifecycle action.
type ActionType string

const (
	ActionCordon   ActionType = "Cordon"
	ActionDrain    ActionType = "Drain"
	ActionUncordon ActionType = "Uncordon"
)

// Phase is the high-level state of a NodeMaintenance object or a single node.
type Phase string

const (
	PhasePending    Phase = "Pending"
	PhaseInProgress Phase = "InProgress"
	PhaseCompleted  Phase = "Completed"
	PhaseFailed     Phase = "Failed"
)

// NodeMaintenanceSpec is the desired state of a node maintenance run.
type NodeMaintenanceSpec struct {
	// NodeSelector selects target nodes by labels. Ignored when NodeNames is set.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// NodeNames is an explicit list of node names to act on. Takes precedence
	// over NodeSelector when non-empty.
	// +optional
	NodeNames []string `json:"nodeNames,omitempty"`

	// Actions is the ordered list of actions executed against every target
	// node. Each node moves through the full sequence before being marked
	// Completed.
	Actions []ActionSpec `json:"actions"`

	// Strategy controls global safety constraints during the run.
	// +optional
	Strategy Strategy `json:"strategy,omitempty"`
}

// ActionSpec selects an action and configures its options.
type ActionSpec struct {
	// Type is the action kind. One of Cordon, Drain, Uncordon.
	Type ActionType `json:"type"`

	// DrainOptions tunes the Drain action. Ignored for other types.
	// +optional
	DrainOptions *DrainOptions `json:"drainOptions,omitempty"`
}

// DrainOptions configures pod eviction behavior.
type DrainOptions struct {
	// GracePeriodSeconds for pod eviction. Defaults to the pod's terminationGracePeriodSeconds.
	// +optional
	GracePeriodSeconds *int64 `json:"gracePeriodSeconds,omitempty"`

	// TimeoutSeconds is the wall-clock budget for the drain to complete on a node.
	// +optional
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`

	// IgnoreDaemonSets skips DaemonSet-managed pods (recommended).
	// +optional
	IgnoreDaemonSets bool `json:"ignoreDaemonSets,omitempty"`
}

// Strategy controls how many nodes can be in-flight at once.
type Strategy struct {
	// MaxUnavailable is the maximum number of nodes that can be in any
	// non-terminal phase (cordoned/draining/etc.) at the same time. Defaults
	// to 1 when unset or non-positive.
	// +optional
	MaxUnavailable int `json:"maxUnavailable,omitempty"`
}

// NodeMaintenanceStatus is the observed state of a maintenance run.
type NodeMaintenanceStatus struct {
	// Phase is the high-level phase of this maintenance run.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Nodes is the per-node progress table. The orchestrator only writes
	// here; readers can rely on this being a stable view of the run.
	// +optional
	Nodes []NodeStatus `json:"nodes,omitempty"`

	// StartTime is set when the run first transitions out of Pending.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is set when all nodes reach a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// NodeStatus tracks a single node through the action sequence.
type NodeStatus struct {
	Name               string       `json:"name"`
	Phase              Phase        `json:"phase"`
	CurrentAction      string       `json:"currentAction,omitempty"`
	CompletedActions   []string     `json:"completedActions,omitempty"`
	Message            string       `json:"message,omitempty"`
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nm

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
