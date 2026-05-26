// Package orchestrator turns a NodeMaintenance spec into safe, ordered work.
//
// The orchestrator is intentionally side-effect-light at the storage layer:
// it mutates the in-memory NodeMaintenance.Status struct passed in, and lets
// the caller (the controller) persist that status in a single Update.
//
// Responsibilities:
//   - resolving the target node set (NodeNames / NodeSelector / AllNodes),
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

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
	"github.com/AnirudhKabra/klusterOne/internal/actions"
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

// Init seeds Status.Nodes / Status.Phase / Status.StartTime on the very first
// reconcile and populates Status.Summary so the printer columns light up
// immediately. It is idempotent: subsequent calls (after Nodes is populated)
// are no-ops and return seeded=false.
//
// The controller calls Init before Step on a freshly applied NodeMaintenance,
// writes status, and re-queues. This guarantees the user sees a non-blank
// row (Phase=InProgress, Total=N, Pending=N) within one reconcile of apply,
// independent of how long the first action takes.
func (o *Orchestrator) Init(ctx context.Context, nm *kov1alpha1.NodeMaintenance) (bool, error) {
	seeded, err := o.initStatus(ctx, nm)
	if err != nil {
		return false, fmt.Errorf("init status: %w", err)
	}
	if seeded {
		o.rollup(nm)
	}
	return seeded, nil
}

// Step performs a single advance of the run:
//  1. Initializes status.Nodes if empty.
//  2. Promotes Pending → InProgress up to the effective budget.
//  3. Runs the next un-completed action on each InProgress node.
//  4. Rolls per-node phases up into the top-level Phase.
//
// It returns true if the caller should re-queue (more work remains), false
// once the run has reached a terminal phase.
func (o *Orchestrator) Step(ctx context.Context, nm *kov1alpha1.NodeMaintenance) (bool, error) {
	logger := log.FromContext(ctx).WithValues("nodeMaintenance", nm.Name)

	if _, err := o.initStatus(ctx, nm); err != nil {
		return false, fmt.Errorf("init status: %w", err)
	}

	/*
		After initStatus:

		status:
		phase: InProgress
		startTime: 2026-05-25T14:30:00Z
		nodes:
			- { name: node-a, phase: Pending, lastTransitionTime: 14:30:00 }
			- { name: node-b, phase: Pending, lastTransitionTime: 14:30:00 }
			- { name: node-c, phase: Pending, lastTransitionTime: 14:30:00 }
	*/

	o.admit(nm)

	/*
		After first epoch of admin()

		status:
		phase: InProgress
		startTime: 2026-05-25T14:30:00Z
		nodes:
			- { name: node-a, phase: InProgress, lastTransitionTime: 14:30:05 }   # promoted
			- { name: node-b, phase: InProgress, lastTransitionTime: 14:30:05 }   # promoted
			- { name: node-c, phase: Pending,    lastTransitionTime: 14:30:00 }   # untouched
	*/

	if err := o.runActions(ctx, nm); err != nil {
		logger.Error(err, "action execution returned error; per-node status reflects details")
	}

	done := o.rollup(nm)
	return !done, nil
}

// EffectiveActions returns the action list the orchestrator actually runs.
// When the user left Actions empty but attached a Script, we synthesize a
// safe default sequence of [Cordon, Script, Uncordon].
func EffectiveActions(spec *kov1alpha1.NodeMaintenanceSpec) []kov1alpha1.ActionSpec {
	if len(spec.Actions) > 0 {
		return spec.Actions
	}
	if spec.Script != nil {
		return []kov1alpha1.ActionSpec{
			{Type: kov1alpha1.ActionCordon},
			{Type: kov1alpha1.ActionScript},
			{Type: kov1alpha1.ActionUncordon},
		}
	}
	return nil
}

// initStatus seeds status.Nodes on the first reconcile and stamps StartTime.
// Returns seeded=true when this call actually performed the seeding so the
// caller can decide whether to persist status before doing more work.
func (o *Orchestrator) initStatus(ctx context.Context, nm *kov1alpha1.NodeMaintenance) (bool, error) {
	if len(nm.Status.Nodes) > 0 {
		return false, nil
	}
	// Targets is a pure projection of spec — stamp it whether or not we
	// find target nodes, so the "Targets" column populates even when the
	// run resolves to zero nodes and goes straight to Completed.
	nm.Status.Targets = nm.Spec.SummarizeTargets()

	names, err := o.resolveNodes(ctx, &nm.Spec)
	if err != nil {
		return false, err
	}
	if len(names) == 0 {
		nm.Status.Phase = kov1alpha1.PhaseCompleted
		now := metav1.Now()
		nm.Status.StartTime = &now
		nm.Status.CompletionTime = &now
		return true, nil
	}

	now := metav1.Now()
	nm.Status.StartTime = &now
	nm.Status.Phase = kov1alpha1.PhaseInProgress
	for _, n := range names {
		nm.Status.Nodes = append(nm.Status.Nodes, kov1alpha1.NodeStatus{
			Name:               n,
			Phase:              kov1alpha1.PhasePending,
			LastTransitionTime: &now,
		})
	}
	return true, nil
}

// resolveNodes returns the deterministic, sorted list of target node names.
// AllNodes wins over NodeNames wins over NodeSelector.
func (o *Orchestrator) resolveNodes(ctx context.Context, spec *kov1alpha1.NodeMaintenanceSpec) ([]string, error) {
	if spec.AllNodes {
		return o.listAllNodes(ctx, labels.Everything())
	}
	if len(spec.NodeNames) > 0 {
		out := append([]string(nil), spec.NodeNames...)
		sort.Strings(out)
		return out, nil
	}
	if len(spec.NodeSelector) > 0 {
		return o.listAllNodes(ctx, labels.SelectorFromSet(labels.Set(spec.NodeSelector)))
	}
	return nil, nil
}

func (o *Orchestrator) listAllNodes(ctx context.Context, sel labels.Selector) ([]string, error) {
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

// admit promotes Pending nodes to InProgress while respecting the budget.
// AtOnce widens the budget to the full node set; otherwise MaxUnavailable
// (defaulted to 1) applies.
func (o *Orchestrator) admit(nm *kov1alpha1.NodeMaintenance) {
	budget := effectiveBudget(nm)

	inFlight := 0
	for i := range nm.Status.Nodes {
		if nm.Status.Nodes[i].Phase == kov1alpha1.PhaseInProgress {
			inFlight++
		}
	}

	now := metav1.Now()
	for i := range nm.Status.Nodes {
		if inFlight >= budget {
			return
		}
		ns := &nm.Status.Nodes[i]
		if ns.Phase != kov1alpha1.PhasePending {
			continue
		}
		ns.Phase = kov1alpha1.PhaseInProgress
		ns.LastTransitionTime = &now
		inFlight++
	}
}

func effectiveBudget(nm *kov1alpha1.NodeMaintenance) int {
	if nm.Spec.Strategy.AtOnce {
		return len(nm.Status.Nodes)
	}
	if nm.Spec.Strategy.MaxUnavailable > 0 {
		return nm.Spec.Strategy.MaxUnavailable
	}
	return 1
}

// runActions advances every InProgress node by exactly one action per Step.
// On error the node is marked Failed and other nodes are unaffected.
func (o *Orchestrator) runActions(ctx context.Context, nm *kov1alpha1.NodeMaintenance) error {
	var firstErr error
	for i := range nm.Status.Nodes {
		ns := &nm.Status.Nodes[i]
		if ns.Phase != kov1alpha1.PhaseInProgress {
			continue
		}
		if err := o.advanceNode(ctx, nm, ns); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// advanceNode runs the next un-completed action against a single node.
func (o *Orchestrator) advanceNode(ctx context.Context, nm *kov1alpha1.NodeMaintenance, ns *kov1alpha1.NodeStatus) error {
	logger := log.FromContext(ctx).WithValues("node", ns.Name)
	plan := EffectiveActions(&nm.Spec)
	idx := len(ns.CompletedActions)
	if idx >= len(plan) {
		now := metav1.Now()
		ns.Phase = kov1alpha1.PhaseCompleted
		ns.CurrentAction = ""
		ns.LastTransitionTime = &now
		return nil
	}

	spec := plan[idx]
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

	if err := action.Execute(ctx, nm, node, ns, spec); err != nil {
		return o.failNode(ns, fmt.Errorf("%s: %w", spec.Type, err))
	}

	now := metav1.Now()
	ns.CompletedActions = append(ns.CompletedActions, string(spec.Type))
	ns.LastTransitionTime = &now
	ns.Message = ""

	if len(ns.CompletedActions) == len(plan) {
		ns.Phase = kov1alpha1.PhaseCompleted
		ns.CurrentAction = ""
	}
	return nil
}

func (o *Orchestrator) failNode(ns *kov1alpha1.NodeStatus, cause error) error {
	now := metav1.Now()
	ns.Phase = kov1alpha1.PhaseFailed
	ns.Message = cause.Error()
	ns.LastTransitionTime = &now
	return cause
}

// rollup computes the run-level Phase from per-node phases, stamps
// CompletionTime when terminal, and writes Status.Summary (per-phase counts
// surfaced via the "Done"/"Total" printer columns). Returns true when
// terminal.
func (o *Orchestrator) rollup(nm *kov1alpha1.NodeMaintenance) bool {
	var s kov1alpha1.StatusSummary
	for _, ns := range nm.Status.Nodes {
		s.Total++
		switch ns.Phase {
		case kov1alpha1.PhaseCompleted:
			s.Completed++
		case kov1alpha1.PhaseFailed:
			s.Failed++
		case kov1alpha1.PhaseInProgress:
			s.InProgress++
		default:
			s.Pending++
		}
	}
	nm.Status.Summary = s

	if s.Total == 0 {
		return nm.Status.Phase == kov1alpha1.PhaseCompleted
	}

	if s.Pending == 0 && s.InProgress == 0 {
		if s.Failed > 0 {
			nm.Status.Phase = kov1alpha1.PhaseFailed
		} else {
			nm.Status.Phase = kov1alpha1.PhaseCompleted
		}
		if nm.Status.CompletionTime == nil {
			now := metav1.Now()
			nm.Status.CompletionTime = &now
		}
		return true
	}

	nm.Status.Phase = kov1alpha1.PhaseInProgress
	return false
}
