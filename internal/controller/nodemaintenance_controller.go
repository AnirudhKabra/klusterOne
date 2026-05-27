// Package controller wires the orchestrator into a controller-runtime
// reconciler. The reconcile loop is intentionally thin: it only loads the
// object, asks the orchestrator to take one Step, persists status, and
// requeues if more work remains.
package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
	"github.com/AnirudhKabra/klusterOne/internal/orchestrator"
)

// runnerNamespace is where script ConfigMaps and runner Pods live. It is a
// fixed convention — the controller no longer accepts a flag to change it
// and the CLI has no override. Both ends must agree, so they share the
// literal "ko-system" (mirrored as `RunnerNamespace` in the CLI).
const runnerNamespace = "ko-system"

// NodeMaintenanceReconciler reconciles NodeMaintenance objects.
type NodeMaintenanceReconciler struct {
	client.Client
	Orchestrator *orchestrator.Orchestrator

	// Kube is an uncached typed client used for namespaced ConfigMap work
	// (script CM ownership adoption). The cached `client.Client` is fine
	// for the cluster-scoped NodeMaintenance CR but would spin up a
	// cluster-wide ConfigMap informer the moment we touched a CM through
	// it — and our RBAC only grants ConfigMap access inside ko-system,
	// which would leave the cache permanently un-synced and block every
	// reconcile on WaitForCacheSync.
	Kube kubernetes.Interface

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

	// Adopt the script ConfigMap before we do anything else, so that
	// `kubectl delete nm <name>` cascades to its script CM via Kubernetes
	// garbage collection — independent of how the NM was created (CLI,
	// `kubectl apply -f`, etc.). Idempotent and intentionally non-fatal: a
	// transient API error just means we'll try again next reconcile.
	if err := r.ensureScriptCMOwnership(ctx, &nm); err != nil {
		logger.V(1).Info("failed to set script ConfigMap ownerReference (will retry)", "error", err.Error())
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

// ensureScriptCMOwnership adds an ownerReference to the script ConfigMap
// pointing at this NodeMaintenance, so Kubernetes garbage-collects the CM
// whenever the NM is deleted. The NM is cluster-scoped and the CM is
// namespaced — that direction of ownership is explicitly supported by the
// Kubernetes GC subsystem.
//
// Idempotent: returns nil immediately when our UID is already in the CM's
// ownerReferences. Safe for declaratively-applied NMs (`kubectl apply -f`)
// whose backing CM was authored by the user without an ownerRef.
//
// Falls through with nil for NMs that don't reference a CM (inline scripts,
// no Script action, etc.) or whose CM doesn't exist yet — in those cases
// there's nothing to adopt.
func (r *NodeMaintenanceReconciler) ensureScriptCMOwnership(ctx context.Context, nm *kov1alpha1.NodeMaintenance) error {
	if nm.Spec.Script == nil || nm.Spec.Script.ConfigMapRef == nil {
		return nil
	}
	ref := nm.Spec.Script.ConfigMapRef
	if ref.Name == "" {
		return nil
	}
	if r.Kube == nil {
		// Unit-test path: no typed client wired in. Skip adoption rather
		// than panic — GC just won't cascade in that environment.
		return nil
	}
	// The CR's ScriptConfigMapRef carries only a Name; the runner namespace
	// is fixed — both the controller and the CLI agree on `ko-system`. We
	// intentionally use the uncached typed client here so we don't trip
	// controller-runtime into starting a cluster-wide ConfigMap informer
	// (our RBAC scopes ConfigMap access to ko-system only).
	cms := r.Kube.CoreV1().ConfigMaps(runnerNamespace)
	cm, err := cms.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		// CM not yet created (e.g. NM applied first, CM lagging) — nothing
		// to adopt this reconcile pass; we'll try again next time.
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	for _, o := range cm.OwnerReferences {
		if o.UID == nm.UID {
			return nil
		}
	}

	cm.OwnerReferences = append(cm.OwnerReferences, metav1.OwnerReference{
		APIVersion: kov1alpha1.GroupVersion.String(),
		Kind:       "NodeMaintenance",
		Name:       nm.Name,
		UID:        nm.UID,
	})
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		// Conflict means someone else just touched the CM (e.g. the CLI's
		// upsertScriptConfigMap). Retry next reconcile against the fresh
		// resourceVersion.
		if apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
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
