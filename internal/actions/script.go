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

	cmName, cmKey, err := s.ensureConfigMap(ctx, nm)
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

	switch terminal.Status.Phase {
	case corev1.PodSucceeded:
		s.gcPod(ctx, terminal)
		return nil
	case corev1.PodFailed:
		tail := s.tailLogs(ctx, terminal)
		s.gcPod(ctx, terminal)
		return fmt.Errorf("script failed (exit=%s): %s", exitStr(exit), tail)
	default:
		s.gcPod(ctx, terminal)
		return fmt.Errorf("runner pod in unexpected phase %q", terminal.Status.Phase)
	}
}

// ensureConfigMap returns the (configmap name, key) the runner Pod should
// mount. When the user gave us an inline script we materialize a ConfigMap
// in the runner namespace; when they gave us a ConfigMapRef we just pass it
// through. The ref is always resolved against the runner namespace because
// Pods can only mount ConfigMaps from their own namespace.
func (s *Script) ensureConfigMap(ctx context.Context, nm *kov1alpha1.NodeMaintenance) (string, string, error) {
	if ref := nm.Spec.Script.ConfigMapRef; ref != nil {
		key := ref.Key
		if key == "" {
			key = defaultScriptKey
		}
		return ref.Name, key, nil
	}

	if nm.Spec.Script.Inline == "" {
		return "", "", fmt.Errorf("script: neither inline nor configMapRef set")
	}

	name := fmt.Sprintf("nm-%s-script", nm.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.RunnerNamespace,
			Labels: map[string]string{
				labelOwner: nm.Name,
			},
		},
		Data: map[string]string{
			defaultScriptKey: nm.Spec.Script.Inline,
		},
	}

	_, err := s.Client.CoreV1().ConfigMaps(s.RunnerNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = s.Client.CoreV1().ConfigMaps(s.RunnerNamespace).Update(ctx, cm, metav1.UpdateOptions{})
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
	common.HostPID = true
	common.HostNetwork = true
	common.HostIPC = true
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
