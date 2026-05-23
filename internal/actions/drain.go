package actions

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"

	furyv1alpha1 "github.com/fury/fury-controller/api/v1alpha1"
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

func (d *Drain) Name() string { return string(furyv1alpha1.ActionDrain) }

func (d *Drain) Execute(ctx context.Context, node *corev1.Node, spec furyv1alpha1.ActionSpec) error {
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

	pods, err := d.podsToEvict(ctx, node.Name, opts)
	if err != nil {
		return fmt.Errorf("list pods on %s: %w", node.Name, err)
	}

	for i := range pods {
		if err := d.evict(ctx, &pods[i], opts); err != nil {
			return fmt.Errorf("evict %s/%s: %w", pods[i].Namespace, pods[i].Name, err)
		}
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining, err := d.podsToEvict(ctx, node.Name, opts)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("drain timed out on %s: %d pods remaining", node.Name, len(remaining))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (d *Drain) podsToEvict(ctx context.Context, nodeName string, opts furyv1alpha1.DrainOptions) ([]corev1.Pod, error) {
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

func (d *Drain) evict(ctx context.Context, p *corev1.Pod, opts furyv1alpha1.DrainOptions) error {
	delOpts := &metav1.DeleteOptions{}
	if opts.GracePeriodSeconds != nil {
		grace := *opts.GracePeriodSeconds
		delOpts.GracePeriodSeconds = &grace
	}
	eviction := &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
		DeleteOptions: delOpts,
	}
	err := d.Client.PolicyV1().Evictions(p.Namespace).Evict(ctx, eviction)
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return nil
	case apierrors.IsTooManyRequests(err):
		// PDB blocked us; the wait loop will retry the eviction next pass.
		return nil
	default:
		return err
	}
}

func drainOptionsOrDefault(in *furyv1alpha1.DrainOptions) furyv1alpha1.DrainOptions {
	if in == nil {
		return furyv1alpha1.DrainOptions{IgnoreDaemonSets: true}
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
