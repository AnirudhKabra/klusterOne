package actions

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
)

// mirrorPodAnnotation marks a static pod created by the kubelet from a manifest
// on disk. These pods can't be evicted via the API and are skipped during drain.
const mirrorPodAnnotation = "kubernetes.io/config.mirror"

// Drain evicts all evictable pods off a node and waits for them to terminate.
//
// "Evictable" excludes:
//   - mirror pods (kubelet-managed),
//   - DaemonSet-owned pods (when IgnoreDaemonSets is true),
//   - already-terminated pods.
//
// PodDisruptionBudgets are honored automatically by the Eviction API: if a
// PDB blocks an eviction, the API server returns 429 and we retry on the next
// poll cycle (or fail when the timeout elapses).
type Drain struct {
	Client kubernetes.Interface

	// DefaultTimeout caps a single Execute() call when DrainOptions doesn't
	// specify TimeoutSeconds.
	DefaultTimeout time.Duration

	// PollInterval controls how often we re-list pods while waiting for them
	// to disappear.
	PollInterval time.Duration
}

func (d *Drain) Name() string { return string(kov1alpha1.ActionDrain) }

func (d *Drain) Execute(
	ctx context.Context,
	_ *kov1alpha1.NodeMaintenance,
	node *corev1.Node,
	_ *kov1alpha1.NodeStatus,
	spec kov1alpha1.ActionSpec,
) error {
	opts := drainOptionsOrDefault(spec.DrainOptions)
	timeout := d.DefaultTimeout
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	poll := d.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}

	deadline := time.Now().Add(timeout)
	// Carry context across iterations so the timeout error can blame the
	// most recent cause (PDB-blocked pods or a transient list failure)
	// rather than leaving the operator guessing.
	var (
		lastBlocked []string
		lastListErr error
		lastCount   int
	)

	for {
		remaining, listErr := d.podsToEvict(ctx, node.Name, opts)
		switch {
		case listErr == nil:
			lastListErr = nil
			lastCount = len(remaining)
			if len(remaining) == 0 {
				return nil
			}
		case isTransientAPIErr(listErr):
			// Apiserver hiccup. The orchestrator does not retry actions
			// (see orchestrator.advanceNode → failNode), so a transient
			// 503/504 on list must not kill an otherwise drainable node;
			// remember the error so the timeout message can surface it.
			lastListErr = listErr
			remaining = nil
		default:
			return fmt.Errorf("list pods on %s: %w", node.Name, listErr)
		}

		// Only (re-)evict pods that aren't already shutting down. A successful
		// eviction sets DeletionTimestamp; the pod then lingers on the node
		// until the kubelet finishes its grace period, and re-evicting it in
		// the meantime is just API-server spam. Pods with no DeletionTimestamp
		// are either fresh or were PDB-blocked (429 without delete), so they
		// get retried on this pass.
		var blocked []string
		for i := range remaining {
			if remaining[i].DeletionTimestamp != nil {
				continue
			}
			isBlocked, err := d.evict(ctx, &remaining[i], opts)
			if err != nil {
				return fmt.Errorf("evict %s/%s: %w", remaining[i].Namespace, remaining[i].Name, err)
			}
			if isBlocked {
				blocked = append(blocked, fmt.Sprintf("%s/%s", remaining[i].Namespace, remaining[i].Name))
			}
		}
		if listErr == nil {
			// Only refresh lastBlocked when we actually saw the pod list this
			// pass; otherwise the report would falsely look empty.
			lastBlocked = blocked
		}

		if time.Now().After(deadline) {
			return drainTimeoutError(node.Name, lastCount, lastBlocked, lastListErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func drainTimeoutError(node string, remaining int, blocked []string, listErr error) error {
	if listErr != nil {
		return fmt.Errorf("drain timed out on %s: last pod list failed transiently: %w", node, listErr)
	}
	if len(blocked) > 0 {
		return fmt.Errorf("drain timed out on %s: %d pods remaining (blocked by PDB: %s)",
			node, remaining, strings.Join(blocked, ", "))
	}
	return fmt.Errorf("drain timed out on %s: %d pods remaining", node, remaining)
}

// isTransientAPIErr reports whether err is a class of apiserver failure that
// is expected to clear on its own (overload, leader election, brief network
// blip). Used to keep the drain loop alive across short-lived hiccups instead
// of failing the whole node.
func isTransientAPIErr(err error) bool {
	return apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err)
}

func (d *Drain) podsToEvict(ctx context.Context, nodeName string, opts kov1alpha1.DrainOptions) ([]corev1.Pod, error) {
	sel := fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
	list, err := d.Client.CoreV1().Pods(metav1.NamespaceAll).List(
		ctx, metav1.ListOptions{FieldSelector: sel},
	)
	if err != nil {
		return nil, err
	}

	var out []corev1.Pod
	for _, p := range list.Items {
		if isMirrorPod(&p) {
			continue
		}
		if isOwnedByDaemonSet(&p) && opts.IgnoreDaemonSets {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// evict attempts a single eviction against the API server. Returns
// (blocked, err) where blocked=true means a PodDisruptionBudget rejected the
// request (HTTP 429 from the Eviction API). Transient apiserver failures
// (503/504/timeout) are swallowed silently and rely on the outer loop to
// retry on the next pass — this matters because the orchestrator marks the
// node Failed on any returned error.
func (d *Drain) evict(ctx context.Context, p *corev1.Pod, opts kov1alpha1.DrainOptions) (blocked bool, err error) {
	delOpts := &metav1.DeleteOptions{}
	if opts.GracePeriodSeconds != nil {
		grace := *opts.GracePeriodSeconds
		delOpts.GracePeriodSeconds = &grace
	}
	eviction := &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
		DeleteOptions: delOpts,
	}
	err = d.Client.PolicyV1().Evictions(p.Namespace).Evict(ctx, eviction)
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return false, nil
	case apierrors.IsTooManyRequests(err):
		return true, nil
	case apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err):
		return false, nil
	default:
		return false, err
	}
}

func drainOptionsOrDefault(in *kov1alpha1.DrainOptions) kov1alpha1.DrainOptions {
	if in == nil {
		return kov1alpha1.DrainOptions{IgnoreDaemonSets: true}
	}
	return *in
}

func isMirrorPod(p *corev1.Pod) bool {
	_, ok := p.Annotations[mirrorPodAnnotation]
	return ok
}

func isOwnedByDaemonSet(p *corev1.Pod) bool {
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}
