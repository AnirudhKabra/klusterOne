// Package orchestrator turns a NodeMaintenance spec into safe, ordered work.
//
// The orchestrator is intentionally side-effect-light at the storage layer:
// it mutates the in-memory NodeMaintenance.Status struct passed in, and lets
// the caller (the controller) persist that status in a single Update.
//
// Responsibilities:
//   - resolving the target node set (via NodeNames or NodeSelector),
//   - admitting pending nodes into "InProgress" within the maxUnavailable budget,
//   - running the next pending action on every in-flight node (one action per Step),
//   - rolling up per-node phases into a top-level Phase.
package orchestrator

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"

	furyv1alpha1 "github.com/fury/fury-controller/api/v1alpha1"
	"github.com/fury/fury-controller/internal/actions"
)

// Orchestrator drives a NodeMaintenance run forward by one step per call.
type Orchestrator struct {
	Kube     kubernetes.Interface
	Registry *actions.Registry
}

// New builds an orchestrator from its dependencies.
func New(kube kubernetes.Interface, reg *actions.Registry) *Orchestrator {
	return &Orchestrator{Kube: kube, Registry: reg}
}

// Step performs a single advance of the run:
//  1. Initializes status.Nodes if empty.
//  2. Promotes Pending → InProgress up to MaxUnavailable.
//  3. Runs the next un-completed action on each InProgress node.
//  4. Rolls per-node phases up into the top-level Phase.
//
// It returns true if the caller should re-queue (more work remains), false
// once the run has reached a terminal phase.
func (o *Orchestrator) Step(ctx context.Context, nm *furyv1alpha1.NodeMaintenance) (bool, error) {
	logger := log.FromContext(ctx).WithValues("nodeMaintenance", nm.Name)

	if err := o.initStatus(ctx, nm); err != nil {
		return false, fmt.Errorf("init status: %w", err)
	}

	o.admit(nm)

	if err := o.runActions(ctx, nm); err != nil {
		logger.Error(err, "action execution returned error; per-node status reflects details")
	}

	done := o.rollup(nm)
	return !done, nil
}

// initStatus seeds status.Nodes on the first reconcile and stamps StartTime.
func (o *Orchestrator) initStatus(ctx context.Context, nm *furyv1alpha1.NodeMaintenance) error {
	if len(nm.Status.Nodes) > 0 {
		return nil
	}
	names, err := o.resolveNodes(ctx, &nm.Spec)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		nm.Status.Phase = furyv1alpha1.PhaseCompleted
		now := metav1.Now()
		nm.Status.StartTime = &now
		nm.Status.CompletionTime = &now
		return nil
	}

	now := metav1.Now()
	nm.Status.StartTime = &now
	nm.Status.Phase = furyv1alpha1.PhaseInProgress
	for _, n := range names {
		nm.Status.Nodes = append(nm.Status.Nodes, furyv1alpha1.NodeStatus{
			Name:               n,
			Phase:              furyv1alpha1.PhasePending,
			LastTransitionTime: &now,
		})
	}
	return nil
}

// resolveNodes returns the deterministic, sorted list of target node names.
// NodeNames takes precedence; otherwise we resolve NodeSelector against the
// API. An empty selector with no NodeNames matches all nodes (operator opt-in).
func (o *Orchestrator) resolveNodes(ctx context.Context, spec *furyv1alpha1.NodeMaintenanceSpec) ([]string, error) {
	if len(spec.NodeNames) > 0 {
		out := append([]string(nil), spec.NodeNames...)
		sort.Strings(out)
		return out, nil
	}
	sel := labels.SelectorFromSet(labels.Set(spec.NodeSelector))
	list, err := o.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names, nil
}

// admit promotes Pending nodes to InProgress while respecting MaxUnavailable.
// Promotion order matches the (sorted) Status.Nodes slice for determinism.
func (o *Orchestrator) admit(nm *furyv1alpha1.NodeMaintenance) {
	budget := nm.Spec.Strategy.MaxUnavailable
	if budget <= 0 {
		budget = 1
	}

	inFlight := 0
	for i := range nm.Status.Nodes {
		if nm.Status.Nodes[i].Phase == furyv1alpha1.PhaseInProgress {
			inFlight++
		}
	}

	now := metav1.Now()
	for i := range nm.Status.Nodes {
		if inFlight >= budget {
			return
		}
		ns := &nm.Status.Nodes[i]
		if ns.Phase != furyv1alpha1.PhasePending {
			continue
		}
		ns.Phase = furyv1alpha1.PhaseInProgress
		ns.LastTransitionTime = &now
		inFlight++
	}
}

// runActions advances every InProgress node by exactly one action per Step.
// On error the node is marked Failed and other nodes are unaffected.
func (o *Orchestrator) runActions(ctx context.Context, nm *furyv1alpha1.NodeMaintenance) error {
	var firstErr error
	for i := range nm.Status.Nodes {
		ns := &nm.Status.Nodes[i]
		if ns.Phase != furyv1alpha1.PhaseInProgress {
			continue
		}
		if err := o.advanceNode(ctx, nm, ns); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// advanceNode runs the next un-completed action against a single node.
func (o *Orchestrator) advanceNode(ctx context.Context, nm *furyv1alpha1.NodeMaintenance, ns *furyv1alpha1.NodeStatus) error {
	logger := log.FromContext(ctx).WithValues("node", ns.Name)
	idx := len(ns.CompletedActions)
	if idx >= len(nm.Spec.Actions) {
		now := metav1.Now()
		ns.Phase = furyv1alpha1.PhaseCompleted
		ns.CurrentAction = ""
		ns.LastTransitionTime = &now
		return nil
	}

	spec := nm.Spec.Actions[idx]
	action, err := o.Registry.Get(spec.Type)
	if err != nil {
		return o.failNode(ns, fmt.Errorf("resolve action: %w", err))
	}

	node, err := o.Kube.CoreV1().Nodes().Get(ctx, ns.Name, metav1.GetOptions{})
	if err != nil {
		return o.failNode(ns, fmt.Errorf("get node: %w", err))
	}

	ns.CurrentAction = string(spec.Type)
	logger.Info("executing action", "action", spec.Type)

	if err := action.Execute(ctx, node, spec); err != nil {
		return o.failNode(ns, fmt.Errorf("%s: %w", spec.Type, err))
	}

	now := metav1.Now()
	ns.CompletedActions = append(ns.CompletedActions, string(spec.Type))
	ns.LastTransitionTime = &now
	ns.Message = ""

	if len(ns.CompletedActions) == len(nm.Spec.Actions) {
		ns.Phase = furyv1alpha1.PhaseCompleted
		ns.CurrentAction = ""
	}
	return nil
}

func (o *Orchestrator) failNode(ns *furyv1alpha1.NodeStatus, cause error) error {
	now := metav1.Now()
	ns.Phase = furyv1alpha1.PhaseFailed
	ns.Message = cause.Error()
	ns.LastTransitionTime = &now
	return cause
}

// rollup computes the run-level Phase from per-node phases and stamps
// CompletionTime when terminal. Returns true when terminal.
func (o *Orchestrator) rollup(nm *furyv1alpha1.NodeMaintenance) bool {
	if len(nm.Status.Nodes) == 0 {
		return nm.Status.Phase == furyv1alpha1.PhaseCompleted
	}

	allTerminal := true
	anyFailed := false
	for _, ns := range nm.Status.Nodes {
		switch ns.Phase {
		case furyv1alpha1.PhaseCompleted:
		case furyv1alpha1.PhaseFailed:
			anyFailed = true
		default:
			allTerminal = false
		}
	}

	if !allTerminal {
		nm.Status.Phase = furyv1alpha1.PhaseInProgress
		return false
	}

	if anyFailed {
		nm.Status.Phase = furyv1alpha1.PhaseFailed
	} else {
		nm.Status.Phase = furyv1alpha1.PhaseCompleted
	}
	if nm.Status.CompletionTime == nil {
		now := metav1.Now()
		nm.Status.CompletionTime = &now
	}
	return true
}
