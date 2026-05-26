// Package controller wires the orchestrator into a controller-runtime
// reconciler. The reconcile loop is intentionally thin: it only loads the
// object, asks the orchestrator to take one Step, persists status, and
// requeues if more work remains.
package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
	"github.com/AnirudhKabra/klusterOne/internal/orchestrator"
)

// NodeMaintenanceReconciler reconciles NodeMaintenance objects.
type NodeMaintenanceReconciler struct {
	client.Client
	Orchestrator *orchestrator.Orchestrator

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

                    ┌──────────────────────────────────────────┐
                    │            controller-runtime            │
                    │           (manager's work queue)         │
                    └────────────┬─────────────────────────────┘
                                 │ dequeue "default/example-script"
                                 ▼
                       ┌───────────────────┐
                       │ Reconcile(ctx,req)│   ← step boundary
                       └─────┬─────────────┘
                             │ r.Get(...)        ←  read from informer cache
                             │ (phase != terminal)
                             ▼
                       ┌───────────────────┐
                       │ Orchestrator.Step │   ← initStatus → admit → runActions → rollup
                       └─────┬─────────────┘
                             │ requeue, _ := ...
                             ▼
                       ┌───────────────────┐
                       │ r.Status().Update │   ← writes new status to API server
                       └─────┬─────────────┘
                             │
              two triggers go back into the queue:
                             │
                ┌────────────┴────────────┐
                ▼                         ▼
   Update event from informer    RequeueAfter: 10s
   (sub-second, usually)         (safety floor)
                │                         │
                └──────────┬──────────────┘
                           ▼
                 next Reconcile() fires
                           │
                           ▼
                (epoch 2 starts here)
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

	if nm.Spec.Paused {
		logger.V(1).Info("NodeMaintenance is paused; skipping step")
		return ctrl.Result{RequeueAfter: r.pausedRequeue()}, nil
	}

	// Fast first-reconcile path: when status has never been seeded, run only
	// Init (cheap, sub-second) and persist immediately so `kubectl get nm`
	// shows Phase/Total/Pending right away instead of leaving the row blank
	// for the duration of the first action.
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
		When you call r.Status().Update(), it:
		1. Writes the new status back to the API server
		2. Sends a watch event to all informers
		3. Caches the new status in memory
		4. Queues a Reconcile() call for this object
		5. Returns a Result{Requeue: true} to the controller-runtime work queue

		client (us)                API server                  informer (us, again)
		─────────                  ──────────                  ──────────────────────
		r.Status().Update ───PUT──►│
								│── store in etcd
								│── emit MODIFIED watch event ────────►│
								│                                      │── apply to local cache
																		│── enqueue req on workqueue
																						│
																						▼
																				Reconcile() fires again
																				r.Get() returns the new nm
																				(from cache, not API)
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
	ctrl.NewControllerManagedBy(mgr)
	→ start configuring a controller under this manager

	.For(&NodeMaintenance{})
	→ 	attach a "watch" NodeMaintenance resources

	.Complete(r)
	→ wire/register the controller with r, so it will start when the manager starts

	so, with this mgr knows, If NodeMaintenance changes, call r.Reconcile()
*/
