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

	furyv1alpha1 "github.com/fury/fury-controller/api/v1alpha1"
	"github.com/fury/fury-controller/internal/orchestrator"
)

// NodeMaintenanceReconciler reconciles NodeMaintenance objects.
type NodeMaintenanceReconciler struct {
	client.Client
	Orchestrator *orchestrator.Orchestrator

	// RequeueInterval controls how often we re-enter Reconcile while a run
	// is making progress. Drain naturally blocks inside Step; this just
	// caps how long we wait if Step returned without progress.
	RequeueInterval time.Duration
}

// +kubebuilder:rbac:groups=fury.io,resources=nodemaintenances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fury.io,resources=nodemaintenances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

// Reconcile drives a single NodeMaintenance one step closer to its desired state.
func (r *NodeMaintenanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var nm furyv1alpha1.NodeMaintenance
	if err := r.Get(ctx, req.NamespacedName, &nm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if nm.Status.Phase == furyv1alpha1.PhaseCompleted || nm.Status.Phase == furyv1alpha1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	requeue, stepErr := r.Orchestrator.Step(ctx, &nm)

	if err := r.Status().Update(ctx, &nm); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "failed to update NodeMaintenance status")
		return ctrl.Result{}, err
	}

	if stepErr != nil {
		return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
	}
	if requeue {
		return ctrl.Result{RequeueAfter: r.RequeueInterval}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *NodeMaintenanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RequeueInterval <= 0 {
		r.RequeueInterval = 10 * time.Second
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&furyv1alpha1.NodeMaintenance{}).
		Complete(r)
}
