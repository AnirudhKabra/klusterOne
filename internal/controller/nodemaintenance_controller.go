// Package controller wires the orchestrator into a controller-runtime
// reconciler. The reconcile loop is intentionally thin: it only loads the
// object, asks the orchestrator to take one Step, persists status, and
// requeues if more work remains.
package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
	"github.com/AnirudhKabra/klusterOne/internal/actions"
	"github.com/AnirudhKabra/klusterOne/internal/orchestrator"
)

// NodeMaintenanceReconciler reconciles NodeMaintenance objects.
type NodeMaintenanceReconciler struct {
	client.Client
	Orchestrator *orchestrator.Orchestrator

	// Kube is a direct typed client used to materialize the script
	// ConfigMap from spec.script.inline before any action runs. The
	// controller-runtime cached client would spin up a cluster-wide
	// ConfigMap informer the moment we touched a CM through it, which
	// we don't want — the controller only has ConfigMap RBAC inside
	// the runner namespace. A typed client also keeps writes synchronous
	// so the CM is observable to `kubectl get cm` on the next list.
	Kube kubernetes.Interface

	// RunnerNamespace is where the script ConfigMap (and runner Pod)
	// live. Fixed convention shared with the Script action and the CLI;
	// must match cmd/manager/main.go's `runnerNamespace` constant.
	RunnerNamespace string

	// RequeueInterval controls how often we re-enter Reconcile while a run
	// is making progress.
	RequeueInterval time.Duration

	// PausedRequeueInterval is the (longer) cadence we use when a NM is
	// paused — just enough to notice when it gets unpaused without burning
	// CPU.
	PausedRequeueInterval time.Duration
}

// +kubebuilder:rbac:groups=ko.io,resources=nodemaintenances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ko.io,resources=nodemaintenances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

/*
   Reconcile flow (one Step per call)
   ----------------------------------

            controller-runtime work queue
                        │
                        ▼ dequeue "<name>"
              ┌───────────────────┐
              │ Reconcile(ctx,req)│
              └─────────┬─────────┘
                        ▼
                   r.Get(nm)                 ← informer cache
                        │
                        ▼
                terminal phase?              ── yes ──► return
                        │ no
                        ▼
                  status empty?              ── yes ──► Init → persist → requeue
                        │ no
                        ▼
              spec.script != nil?            ── yes ──► EnsureScriptConfigMap
                        │                                (idempotent, non-fatal)
                        ▼
                  spec.paused?               ── yes ──► requeue (no Step)
                        │ no
                        ▼
              Orchestrator.Step              ── admit → runActions → rollup
                        │
                        ▼
              r.Status().Update              ── informer re-enqueues us

   Re-entry triggers:
     • informer Update event (sub-second, after r.Status().Update)
     • RequeueAfter: r.RequeueInterval (safety floor)
*/

// Reconcile drives a single NodeMaintenance one step closer to its desired state.
func (r *NodeMaintenanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var nm kov1alpha1.NodeMaintenance
	if err := r.Get(ctx, req.NamespacedName, &nm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if nm.Status.Phase == kov1alpha1.PhaseCompleted || nm.Status.Phase == kov1alpha1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	// First-reconcile fast path: seed status (Phase / Targets / Total /
	// Pending) so `kubectl get nm` lights up immediately.
	//
	// Runs *before* the paused short-circuit on purpose — paused NMs are
	// what operators inspect during the `create --paused` → `attach` →
	// `run` review window. A blank row there is the worst first impression.
	if len(nm.Status.Nodes) == 0 && nm.Status.Phase == "" {
		seeded, err := r.Orchestrator.Init(ctx, &nm)
		if err != nil {
			logger.Error(err, "failed to initialize NodeMaintenance status")
			return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
		}
		if seeded {
			if err := r.Status().Update(ctx, &nm); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{Requeue: true}, nil
				}
				logger.Error(err, "failed to persist seeded NodeMaintenance status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Render spec.script.inline into ko-system as a ConfigMap. Idempotent;
	// non-fatal on transient errors (we retry next pass, and Script.Execute
	// re-runs the same helper just before the runner Pod as a safety net).
	//
	// Runs *before* the paused short-circuit on purpose:
	//   • pause gates execution (no runner Pod);
	//   • it does NOT gate rendering (CM still observable).
	// The CM's ownerRef points back at the NM, so deleting a paused NM
	// still cleans up via GC.
	if nm.Spec.Script != nil && r.Kube != nil {
		if _, _, err := actions.EnsureScriptConfigMap(ctx, r.Kube, &nm, r.RunnerNamespace); err != nil {
			logger.V(1).Info("failed to materialize script ConfigMap (will retry)", "error", err.Error())
		}
	}

	if nm.Spec.Paused {
		logger.V(1).Info("NodeMaintenance is paused; skipping step")
		return ctrl.Result{RequeueAfter: r.pausedRequeue()}, nil
	}

	requeue, stepErr := r.Orchestrator.Step(ctx, &nm)
	// Step returns (true, nil) — meaning "requeue me."

	if err := r.Status().Update(ctx, &nm); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "failed to update NodeMaintenance status")
		return ctrl.Result{}, err
	}
	/*
	   r.Status().Update — what happens next:

	     us              apiserver               informer
	     ──              ────────                ────────
	     PUT ──────────► store in etcd
	                     emit MODIFIED ────────► update local cache
	                                             enqueue req on workqueue
	                                                      │
	                                                      ▼
	                                             Reconcile() fires again
	                                             (next r.Get hits the cache)
	*/

	if stepErr != nil {
		return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
	}
	if requeue {
		return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
	}
	return ctrl.Result{}, nil
}

func (r *NodeMaintenanceReconciler) pausedRequeue() time.Duration {
	if r.PausedRequeueInterval > 0 {
		return r.PausedRequeueInterval
	}
	return 15 * time.Second
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *NodeMaintenanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RequeueInterval <= 0 {
		r.RequeueInterval = 10 * time.Second
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kov1alpha1.NodeMaintenance{}).
		Complete(r)
}

/*
   Wiring (read left → right):

     NewControllerManagedBy(mgr)   configure a controller on this manager
     .For(&NodeMaintenance{})      watch NodeMaintenance resources
     .Complete(r)                  register r; starts with mgr

   Net: any change to a NodeMaintenance enqueues r.Reconcile().
*/
