// Package actions defines the pluggable unit of work the orchestrator
// executes against a single node. Each action is independent, idempotent
// where possible, and only mutates cluster state through the Kubernetes API.
package actions

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
)

// Action is the contract every node-lifecycle step implements.
//
// Execute MUST be:
//   - idempotent (re-running on a node already in the desired state is a no-op),
//   - best-effort blocking (return only when the action is observably complete
//     for that node, or return an error),
//   - free of cross-node coordination (the orchestrator owns concurrency).
//
// nm and ns are passed in so actions can read run-wide config (e.g. the
// attached Script) and stamp per-node telemetry (e.g. ScriptPodName) without
// the orchestrator having to learn about every action's internals.
type Action interface {
	// Name returns the action identifier used in CRD specs and status.
	Name() string

	// Execute applies the action to a single node.
	Execute(
		ctx context.Context,
		nm *kov1alpha1.NodeMaintenance,
		node *corev1.Node,
		ns *kov1alpha1.NodeStatus,
		spec kov1alpha1.ActionSpec,
	) error
}

// Registry maps an ActionType to its implementation. The controller builds
// one at startup and looks actions up per reconcile.
type Registry struct {
	byName map[kov1alpha1.ActionType]Action
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[kov1alpha1.ActionType]Action)}
}

// Register adds a single action. Later registrations overwrite earlier ones
// for the same name, which lets tests swap in fakes.
func (r *Registry) Register(name kov1alpha1.ActionType, a Action) {
	r.byName[name] = a
}

// Get returns the action for name, or an error if no such action is registered.
func (r *Registry) Get(name kov1alpha1.ActionType) (Action, error) {
	a, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return a, nil
}
