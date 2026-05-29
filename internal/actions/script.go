package actions

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
)

const (
	defaultRunnerImage   = "alpine:3.19"
	defaultScriptKey     = "script.sh"
	defaultScriptTimeout = 10 * time.Minute
	hostScriptsDir       = "/tmp/ko-controller"

	labelOwner   = "ko.io/owner"
	labelNode    = "ko.io/node"
	labelAction  = "ko.io/action"
	annotRunUUID = "ko.io/run-uid"

	// annotOutputCM, when set on a NodeMaintenance, asks the Script action
	// to persist the runner Pod's full stdout into a ConfigMap with that
	// name (in the runner namespace) before the Pod is garbage-collected.
	// This is how `kubectl nm pull` smuggles a file off a node without
	// racing the Pod's lifecycle: the CM is owned by the NM, so it lives
	// exactly as long as the NM does.
	annotOutputCM = "ko.io/output-configmap"

	// maxCapturedOutputBytes bounds how much of the runner Pod's stdout we
	// will copy into the output ConfigMap. ConfigMaps are capped at 1 MiB
	// by the API server; we leave ~50 KiB of headroom for metadata.
	maxCapturedOutputBytes = 1000 * 1024
)

// Script runs a user-supplied script against a single node by creating a
// pinned, tolerated Pod. With RunOnHost (the default) the script executes in
// the host mount namespace via nsenter; otherwise it runs inside the Pod.
//
// The action is blocking: Execute returns once the runner Pod has reached a
// terminal phase (Succeeded or Failed). On failure the last log chunk is
// stashed into NodeStatus.Message for visibility.
type Script struct {
	Client kubernetes.Interface

	// RunnerNamespace is where the Pod and (synthesized) ConfigMap live.
	RunnerNamespace string

	// DefaultImage is used when ScriptSpec.Image is empty.
	DefaultImage string

	// PollInterval controls how often we poll the runner Pod for terminal
	// state.
	PollInterval time.Duration

	// KeepPods, if true, leaves the runner Pod around after the action
	// finishes — useful for debugging.
	KeepPods bool

	// LogTailBytes caps the per-node log chunk captured into status.message
	// on failure. Defaults to 4 KiB.
	LogTailBytes int64
}

func (s *Script) Name() string { return string(kov1alpha1.ActionScript) }

func (s *Script) Execute(
	ctx context.Context,
	nm *kov1alpha1.NodeMaintenance,
	node *corev1.Node,
	ns *kov1alpha1.NodeStatus,
	_ kov1alpha1.ActionSpec,
) error {
	logger := log.FromContext(ctx).WithValues("node", node.Name, "nm", nm.Name)

	if nm.Spec.Script == nil {
		return fmt.Errorf("spec.script is required for the Script action")
	}

	cmName, cmKey, err := EnsureScriptConfigMap(ctx, s.Client, nm, s.RunnerNamespace)
	if err != nil {
		return fmt.Errorf("prepare script configmap: %w", err)
	}

	podName := runnerPodName(nm.Name, node.Name)
	ns.ScriptPodName = podName

	pod, err := s.ensurePod(ctx, nm, node, podName, cmName, cmKey)
	if err != nil {
		return fmt.Errorf("create runner pod: %w", err)
	}

	timeout := defaultScriptTimeout
	if nm.Spec.Script.TimeoutSeconds != nil && *nm.Spec.Script.TimeoutSeconds > 0 {
		timeout = time.Duration(*nm.Spec.Script.TimeoutSeconds) * time.Second
	}
	poll := s.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}

	logger.Info("waiting for runner pod", "pod", pod.Name, "timeout", timeout)

	terminal, err := s.waitForPod(ctx, pod.Namespace, pod.Name, poll, timeout)
	if err != nil {
		return fmt.Errorf("wait for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	exit := containerExitCode(terminal)
	if exit != nil {
		ns.ScriptExitCode = exit
	}

	// Capture the log tail *before* GC so it survives the runner Pod's
	// deletion. This is what `kubectl nm logs` falls back to once the pod
	// is gone (i.e. always, unless KeepPods is set).
	tail := s.tailLogs(ctx, terminal)
	if tail != "" {
		ns.ScriptLogTail = tail
	}

	// Opt-in: copy the runner Pod's *full* stdout into a ConfigMap before
	// we delete the Pod. Used by `kubectl nm pull` to fetch the payload
	// out-of-band; the CM is owned by the NM and GC'd with it.
	if cmName := nm.GetAnnotations()[annotOutputCM]; cmName != "" {
		if err := s.captureOutput(ctx, nm, cmName, terminal); err != nil {
			// The CLI is waiting on this CM; failing the action is the
			// right signal. The pod is still cleaned up below.
			s.gcPod(ctx, terminal)
			return fmt.Errorf("capture output to configmap %s: %w", cmName, err)
		}
	}

	switch terminal.Status.Phase {
	case corev1.PodSucceeded:
		s.gcPod(ctx, terminal)
		return nil
	case corev1.PodFailed:
		s.gcPod(ctx, terminal)
		return fmt.Errorf("script failed (exit=%s): %s", exitStr(exit), tail)
	default:
		s.gcPod(ctx, terminal)
		return fmt.Errorf("runner pod in unexpected phase %q", terminal.Status.Phase)
	}
}

// captureOutput streams the runner Pod's full stdout into a ConfigMap that
// outlives the Pod. The CM carries an ownerReference back at the NM so
// Kubernetes GC removes it when the NM is deleted (no manual cleanup).
//
// Idempotent on the CM: a second call with the same NM uid is a no-op on
// the ownerReferences list; the Data is overwritten with the latest log
// snapshot.
func (s *Script) captureOutput(ctx context.Context, nm *kov1alpha1.NodeMaintenance, cmName string, pod *corev1.Pod) error {
	if pod == nil {
		return fmt.Errorf("no terminal pod to capture")
	}
	limit := int64(maxCapturedOutputBytes)
	req := s.Client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container:  "run",
		LimitBytes: &limit,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("open pod logs: %w", err)
	}
	defer stream.Close()
	body, err := io.ReadAll(stream)
	if err != nil {
		return fmt.Errorf("read pod logs: %w", err)
	}

	ownerRef := metav1.OwnerReference{
		APIVersion: kov1alpha1.GroupVersion.String(),
		Kind:       "NodeMaintenance",
		Name:       nm.Name,
		UID:        nm.UID,
	}
	// BinaryData keeps arbitrary bytes safe; pod logs are usually UTF-8
	// but base64-of-binary payloads (push/pull) need byte fidelity.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: s.RunnerNamespace,
			Labels: map[string]string{
				labelOwner: nm.Name,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		BinaryData: map[string][]byte{"output": body},
	}

	_, err = s.Client.CoreV1().ConfigMaps(s.RunnerNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := s.Client.CoreV1().ConfigMaps(s.RunnerNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		existing.BinaryData = cm.BinaryData
		existing.Labels = cm.Labels
		hasOwner := false
		for _, o := range existing.OwnerReferences {
			if o.UID == nm.UID {
				hasOwner = true
				break
			}
		}
		if !hasOwner {
			existing.OwnerReferences = append(existing.OwnerReferences, ownerRef)
		}
		_, err = s.Client.CoreV1().ConfigMaps(s.RunnerNamespace).Update(ctx, existing, metav1.UpdateOptions{})
	}
	return err
}

// EnsureScriptConfigMap materializes nm.Spec.Script.Inline into a
// ConfigMap "nm-<nm-name>-script" in runnerNS. Returns the (name,
// scriptKey) the runner Pod should mount.
//
// Single source of truth for the script-CM invariant. Two callers:
//
//   - controller reconcile loop  — every pass, so the rendered CM is
//     observable in ko-system as soon as the NM is applied (paused
//     NMs included).
//   - Script.Execute             — defensive re-sync immediately
//     before launching the runner Pod.
//
// Each write stamps the CM with:
//
//   - label  `ko.io/owner=<nm-name>`  — queryable, and mirrored onto
//     the runner Pod's labels so `kubectl get cm,pod -l ko.io/owner=X`
//     returns the full action set.
//   - ownerReference back to the NM  — Kubernetes GC removes the CM
//     when the NM is deleted. Cluster-scoped → namespaced ownership
//     is explicitly supported by the GC subsystem.
//
// Idempotent on Data (overwritten from current Inline), Labels, and
// OwnerReferences (UID-deduped; stale ORs preserved for GC to resolve).
//
// Empty Inline is OK: it preserves the
// `kubectl nm create --paused` → `kubectl nm attach` → `kubectl nm run`
// flow. The CLI blocks `run` on empty Inline, so the no-op path is
// closed there, not here.
//
// REQUIRES: nm.Spec.Script != nil (Inline is dereferenced).
//
// The lockdown VAP in config/admission/configmap_lockdown.yaml is what
// actually restricts mutation of any CM in runnerNS to the controller
// SA (+ kube-system GC + namespace-controller for cleanup); it gates
// by namespace, not by the owner label above.
func EnsureScriptConfigMap(ctx context.Context, kube kubernetes.Interface, nm *kov1alpha1.NodeMaintenance, runnerNS string) (string, string, error) {
	name := fmt.Sprintf("nm-%s-script", nm.Name)
	ownerRef := metav1.OwnerReference{
		APIVersion: kov1alpha1.GroupVersion.String(),
		Kind:       "NodeMaintenance",
		Name:       nm.Name,
		UID:        nm.UID,
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: runnerNS,
			Labels: map[string]string{
				labelOwner: nm.Name,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Data: map[string]string{
			defaultScriptKey: nm.Spec.Script.Inline,
		},
	}

	cms := kube.CoreV1().ConfigMaps(runnerNS)
	_, err := cms.Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Preserve any pre-existing ownerRefs (e.g. from a stale, unrelated
		// CM with the same name) while making sure ours is present. A blind
		// .Update with the freshly-constructed object would wipe them.
		existing, getErr := cms.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return "", "", getErr
		}
		existing.Data = cm.Data
		existing.Labels = cm.Labels
		hasOwner := false
		for _, o := range existing.OwnerReferences {
			if o.UID == nm.UID {
				hasOwner = true
				break
			}
		}
		if !hasOwner {
			existing.OwnerReferences = append(existing.OwnerReferences, ownerRef)
		}
		_, err = cms.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return "", "", err
	}
	return name, defaultScriptKey, nil
}

// ensurePod creates the runner Pod (idempotent). If a Pod with the same name
// already exists (likely because we already started this action on a previous
// reconcile pass) we return it as-is — Execute will then just wait for it to
// reach a terminal phase.
func (s *Script) ensurePod(
	ctx context.Context,
	nm *kov1alpha1.NodeMaintenance,
	node *corev1.Node,
	podName, cmName, cmKey string,
) (*corev1.Pod, error) {
	if existing, err := s.Client.CoreV1().Pods(s.RunnerNamespace).Get(ctx, podName, metav1.GetOptions{}); err == nil {
		return existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	pod := s.buildPod(nm, node, podName, cmName, cmKey)
	created, err := s.Client.CoreV1().Pods(s.RunnerNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return s.Client.CoreV1().Pods(s.RunnerNamespace).Get(ctx, podName, metav1.GetOptions{})
	}
	return created, err
}

// buildPod assembles the runner Pod spec. Branches on RunOnHost:
//   - RunOnHost (default): privileged, hostPID/Net/IPC, initContainer stages
//     the script onto a hostPath, main container nsenter's into PID 1 and
//     runs it on the host.
//   - in-pod: plain pod that mounts the ConfigMap and runs the script in its
//     own filesystem.
func (s *Script) buildPod(
	nm *kov1alpha1.NodeMaintenance,
	node *corev1.Node,
	podName, cmName, cmKey string,
) *corev1.Pod {
	image := nm.Spec.Script.Image
	if image == "" {
		image = s.DefaultImage
	}
	if image == "" {
		image = defaultRunnerImage
	}

	runOnHost := true
	if nm.Spec.Script.RunOnHost != nil {
		runOnHost = *nm.Spec.Script.RunOnHost
	}

	env := make([]corev1.EnvVar, 0, len(nm.Spec.Script.Env)+1)
	env = append(env, corev1.EnvVar{Name: "NODE_NAME", Value: node.Name})
	for _, e := range nm.Spec.Script.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}

	scriptVol := corev1.Volume{
		Name: "script",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				DefaultMode:          ptrInt32(0o755),
			},
		},
	}

	labels := map[string]string{
		labelOwner:  nm.Name,
		labelNode:   sanitizeLabel(node.Name),
		labelAction: string(kov1alpha1.ActionScript),
	}

	common := corev1.PodSpec{
		NodeName:      node.Name,
		RestartPolicy: corev1.RestartPolicyNever,
		Tolerations: []corev1.Toleration{
			{Operator: corev1.TolerationOpExists},
		},
		Volumes: []corev1.Volume{scriptVol},
	}

	if !runOnHost {
		common.Containers = []corev1.Container{{
			Name:            "run",
			Image:           image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Env:             env,
			Command:         []string{"/bin/sh"},
			Args:            []string{"/scripts/" + cmKey},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "script", MountPath: "/scripts", ReadOnly: true},
			},
			Resources: defaultRunnerResources(),
		}}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        podName,
				Namespace:   s.RunnerNamespace,
				Labels:      labels,
				Annotations: map[string]string{annotRunUUID: string(nm.UID)},
			},
			Spec: common,
		}
	}

	// runOnHost path: stage the script onto a hostPath, then nsenter into
	// PID 1 and execute it from there.
	//
	// We only request HostPID, not HostNetwork / HostIPC. The trick is
	// that `nsenter --target 1 --mount --uts --ipc --net --pid` (see the
	// Args below) reads namespace fds out of /proc/1/ns/* and setns(2)'s
	// into each one at runtime — so the script ends up in the host's
	// net/ipc/uts/mount namespaces *regardless* of what the Pod started
	// in. All `nsenter` requires from us is:
	//
	//   1. /proc/1 must actually be host PID 1, not the container's PID
	//      1 — that's what HostPID buys us.
	//   2. CAP_SYS_ADMIN to setns() into namespaces we didn't start in —
	//      we already get that via securityContext.privileged on the
	//      main container.
	//
	// HostNetwork/HostIPC on the Pod spec would only widen the *runner's
	// own* pre-nsenter context (the init container's `cp`, kubectl
	// exec'ing into the Pod for debugging, etc.). The script itself sees
	// no difference. Leaving them off shrinks the Pod's blast radius for
	// free and keeps the Pod easier to whitelist narrowly in PSS / Falco
	// / kube-bench scans.
	common.HostPID = true
	common.Volumes = append(common.Volumes, corev1.Volume{
		Name: "host-scripts",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: hostScriptsDir,
				Type: hostPathType(corev1.HostPathDirectoryOrCreate),
			},
		},
	})

	stagedName := fmt.Sprintf("%s.sh", podName)
	common.InitContainers = []corev1.Container{{
		Name:            "stage",
		Image:           "busybox:1.36",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c"},
		Args: []string{
			fmt.Sprintf("cp /scripts/%s /host-scripts/%s && chmod 0755 /host-scripts/%s",
				cmKey, stagedName, stagedName),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "script", MountPath: "/scripts", ReadOnly: true},
			{Name: "host-scripts", MountPath: "/host-scripts"},
		},
		Resources: defaultRunnerResources(),
	}}

	// Run nsenter through a tiny shell wrapper so the staged script is
	// removed from the host after execution, regardless of exit code. We
	// can't use `trap … EXIT; exec nsenter …` because exec replaces the
	// shell and the trap never fires; capturing rc and re-exiting works in
	// BusyBox ash (alpine / busybox) and preserves the exit code that
	// containerExitCode() reads into NodeStatus.ScriptExitCode.
	staged := fmt.Sprintf("%s/%s", hostScriptsDir, stagedName)
	common.Containers = []corev1.Container{{
		Name:            "run",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			Privileged: ptrBool(true),
		},
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(
			`/usr/bin/nsenter --target 1 --mount --uts --ipc --net --pid -- /bin/sh %s; rc=$?; rm -f %s; exit $rc`,
			staged, staged,
		)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "host-scripts", MountPath: hostScriptsDir},
		},
		Resources: defaultRunnerResources(),
	}}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   s.RunnerNamespace,
			Labels:      labels,
			Annotations: map[string]string{annotRunUUID: string(nm.UID)},
		},
		Spec: common,
	}
}

func (s *Script) waitForPod(ctx context.Context, namespace, name string, poll, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.Now().Add(timeout)
	for {
		p, err := s.Client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return p, nil
		}
		if time.Now().After(deadline) {
			return p, fmt.Errorf("timed out after %s in phase %q", timeout, p.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (s *Script) tailLogs(ctx context.Context, pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	tail := s.LogTailBytes
	if tail <= 0 {
		tail = 4 * 1024
	}
	req := s.Client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container:  "run",
		LimitBytes: &tail,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Sprintf("(no logs: %v)", err)
	}
	defer stream.Close()
	body, err := io.ReadAll(stream)
	if err != nil {
		return fmt.Sprintf("(log read error: %v)", err)
	}
	return strings.TrimSpace(string(body))
}

func (s *Script) gcPod(ctx context.Context, pod *corev1.Pod) {
	if s.KeepPods || pod == nil {
		return
	}
	bg := metav1.DeletePropagationBackground
	_ = s.Client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		PropagationPolicy: &bg,
	})
}

// runnerPodName builds a deterministic 63-char-safe pod name from the NM
// name and the target node name.
func runnerPodName(nmName, nodeName string) string {
	raw := fmt.Sprintf("nm-%s-%s", nmName, nodeName)
	raw = strings.ToLower(raw)
	raw = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, raw)
	if len(raw) > validation.DNS1123LabelMaxLength {
		raw = raw[:validation.DNS1123LabelMaxLength]
	}
	return strings.TrimRight(raw, "-")
}

func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

func containerExitCode(pod *corev1.Pod) *int32 {
	if pod == nil {
		return nil
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "run" {
			continue
		}
		if cs.State.Terminated != nil {
			ec := cs.State.Terminated.ExitCode
			return &ec
		}
	}
	return nil
}

func exitStr(p *int32) string {
	if p == nil {
		return "?"
	}
	return fmt.Sprintf("%d", *p)
}

func ptrInt32(v int32) *int32 { return &v }
func ptrBool(v bool) *bool    { return &v }

func hostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }

func defaultRunnerResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
	}
}
