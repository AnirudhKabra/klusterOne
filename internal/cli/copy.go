package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
)

// Hard cap on file size for push/pull. The Script action stores its payload
// in a ConfigMap, which Kubernetes caps at ~1 MiB total. Base64 inflates the
// payload by ~33%, plus there's shell-wrapper overhead. 700 KiB leaves a
// comfortable margin.
const maxCopyBytes = 700 * 1024

const (
	pullSentinelBegin = "---KO-FILE-BEGIN---"
	pullSentinelEnd   = "---KO-FILE-END---"
)

// RunPush copies a local file onto target nodes via a one-shot NodeMaintenance.
// The file is base64-encoded into a script that decodes it into place on each
// node. No cordon/uncordon — a file write should not disrupt workloads.
func RunPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kubectl nm push <local-path> <remote-path> [target-flags] [options]")
		fmt.Fprintln(os.Stderr, "       Copies a local file onto target nodes via a one-shot NodeMaintenance.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Targets (pick one):")
		fmt.Fprintln(os.Stderr, "  --all-nodes              every node in the cluster")
		fmt.Fprintln(os.Stderr, "  --selector k=v,...       label-selected nodes")
		fmt.Fprintln(os.Stderr, "  --nodes a,b,c            explicit node names")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Notes: ~700 KiB ceiling per file (ConfigMap limit).")
	}

	var (
		allNodes  bool
		selector  string
		nodesCSV  string
		mode      string
		keep      bool
		timeout   time.Duration
		nmName    string
		namespace string
	)
	fs.BoolVar(&allNodes, "all-nodes", false, "Target every node.")
	fs.StringVar(&selector, "selector", "", "Label selector, e.g. role=worker.")
	fs.StringVar(&nodesCSV, "nodes", "", "Comma-separated explicit node names.")
	fs.StringVar(&mode, "mode", "", "Octal mode for the destination file (default: copy local mode).")
	fs.BoolVar(&keep, "keep", false, "Don't delete the NodeMaintenance after success.")
	fs.DurationVar(&timeout, "timeout", 5*time.Minute, "Overall wait timeout for the rollout.")
	fs.StringVar(&nmName, "name", "", "Override NodeMaintenance name (default: kone-push-<rand>).")
	fs.StringVar(&namespace, "namespace", "ko-system", "Runner namespace.")

	if hasHelpFlag(args) {
		fs.Usage()
		return nil
	}
	if len(args) < 2 {
		fs.Usage()
		return fmt.Errorf("missing positional arguments")
	}
	localPath := args[0]
	remotePath := args[1]
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	if !allNodes && selector == "" && nodesCSV == "" {
		return fmt.Errorf("pick a target: --all-nodes, --selector k=v, or --nodes a,b,c")
	}
	if !strings.HasPrefix(remotePath, "/") {
		return fmt.Errorf("remote path must be absolute, got %q", remotePath)
	}

	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", localPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory; push handles single files only", localPath)
	}
	body, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", localPath, err)
	}
	encoded := base64.StdEncoding.EncodeToString(body)
	if len(encoded) > maxCopyBytes {
		return fmt.Errorf("file too large: %d raw bytes (base64 %d). Limit is ~%d raw bytes",
			len(body), len(encoded), (maxCopyBytes*3)/4)
	}
	if mode == "" {
		mode = fmt.Sprintf("%04o", info.Mode().Perm())
	}

	script := buildPushScript(remotePath, encoded, mode)

	if nmName == "" {
		nmName = "kone-push-" + randSuffix()
	}

	return runCopyNM(ctx, copyRunSpec{
		nmName:     nmName,
		namespace:  namespace,
		script:     script,
		allNodes:   allNodes,
		selector:   selector,
		nodesCSV:   nodesCSV,
		timeout:    timeout,
		keep:       keep,
		announce:   fmt.Sprintf("push %s → %s (%d bytes, mode %s)", localPath, remotePath, len(body), mode),
		printNodes: true,
	})
}

// RunPull reads a single file from a single node back to the local machine
// by routing it through the runner pod's stdout. The script base64-encodes
// the file between sentinel markers; the CLI fetches the pod's logs after
// completion and decodes them locally.
func RunPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kubectl nm pull <remote-path> <local-path> --node <node> [options]")
		fmt.Fprintln(os.Stderr, "       Reads a file from a single node into the local machine via runner-pod logs.")
	}

	var (
		node      string
		keep      bool
		timeout   time.Duration
		nmName    string
		namespace string
	)
	fs.StringVar(&node, "node", "", "Source node (required).")
	fs.BoolVar(&keep, "keep", false, "Don't delete the NodeMaintenance after success.")
	fs.DurationVar(&timeout, "timeout", 5*time.Minute, "Overall wait timeout.")
	fs.StringVar(&nmName, "name", "", "Override NodeMaintenance name (default: kone-pull-<rand>).")
	fs.StringVar(&namespace, "namespace", "ko-system", "Runner namespace.")

	if hasHelpFlag(args) {
		fs.Usage()
		return nil
	}
	if len(args) < 2 {
		fs.Usage()
		return fmt.Errorf("missing positional arguments")
	}
	remotePath := args[0]
	localPath := args[1]
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if node == "" {
		return fmt.Errorf("--node is required for pull")
	}
	if !strings.HasPrefix(remotePath, "/") {
		return fmt.Errorf("remote path must be absolute, got %q", remotePath)
	}

	script := buildPullScript(remotePath)

	if nmName == "" {
		nmName = "kone-pull-" + randSuffix()
	}

	clients, err := newClients()
	if err != nil {
		return err
	}

	if err := createCopyNM(ctx, clients, nmName, namespace, script, false, "", node, int64(timeout/time.Second)); err != nil {
		return err
	}
	fmt.Printf("nodemaintenance.ko.io/%s created (pull %s:%s → %s)\n", nmName, node, remotePath, localPath)

	if err := waitForNMCompletion(ctx, clients, nmName, timeout); err != nil {
		return err
	}

	podName, err := getRunnerPodName(ctx, clients, nmName, node)
	if err != nil {
		return err
	}
	logs, err := fetchPodLogs(ctx, clients, namespace, podName)
	if err != nil {
		return fmt.Errorf("fetch pod logs: %w", err)
	}
	payload, err := extractPayload(logs)
	if err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if err := os.WriteFile(localPath, decoded, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", localPath, err)
	}
	fmt.Printf("wrote %d bytes to %s\n", len(decoded), localPath)

	if !keep {
		_ = clients.Dyn.Resource(NodeMaintenanceGVR).Delete(ctx, nmName, metav1.DeleteOptions{})
	}
	return nil
}

// copyRunSpec gathers the inputs runCopyNM needs. push uses this; pull does
// the steps inline because it has to fetch logs in between.
type copyRunSpec struct {
	nmName     string
	namespace  string
	script     string
	allNodes   bool
	selector   string
	nodesCSV   string
	timeout    time.Duration
	keep       bool
	announce   string
	printNodes bool
}

func runCopyNM(ctx context.Context, s copyRunSpec) error {
	clients, err := newClients()
	if err != nil {
		return err
	}
	if err := createCopyNM(ctx, clients, s.nmName, s.namespace, s.script, s.allNodes, s.selector, s.nodesCSV, int64(s.timeout/time.Second)); err != nil {
		return err
	}
	fmt.Printf("nodemaintenance.ko.io/%s created (%s)\n", s.nmName, s.announce)

	if err := waitForNMCompletion(ctx, clients, s.nmName, s.timeout); err != nil {
		if s.printNodes {
			_ = printNodeSummary(ctx, clients, s.nmName)
		}
		return err
	}
	if s.printNodes {
		if err := printNodeSummary(ctx, clients, s.nmName); err != nil {
			return err
		}
	}
	if !s.keep {
		_ = clients.Dyn.Resource(NodeMaintenanceGVR).Delete(ctx, s.nmName, metav1.DeleteOptions{})
	}
	return nil
}

func createCopyNM(ctx context.Context, c *Clients, name, namespace, script string, allNodes bool, selector, nodesCSV string, timeoutSec int64) error {
	if err := ensureNamespace(ctx, c, namespace); err != nil {
		return fmt.Errorf("ensure namespace %s: %w", namespace, err)
	}
	cmName := fmt.Sprintf("nm-%s-script", name)
	if err := upsertScriptConfigMap(ctx, c, namespace, cmName, name, script); err != nil {
		return fmt.Errorf("upsert script configmap: %w", err)
	}

	nm := buildCopyNM(name, namespace, allNodes, selector, nodesCSV, timeoutSec)
	nm.Spec.Script.ConfigMapRef = &kov1alpha1.ScriptConfigMapRef{Name: cmName}

	u, err := toUnstructured(nm)
	if err != nil {
		return err
	}
	if _, err := c.Dyn.Resource(NodeMaintenanceGVR).Create(ctx, u, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("a NodeMaintenance named %q already exists; pass --name to override", name)
		}
		return fmt.Errorf("create NodeMaintenance: %w", err)
	}
	return nil
}

func buildCopyNM(name, namespace string, allNodes bool, selector, nodesCSV string, timeoutSec int64) *kov1alpha1.NodeMaintenance {
	runOnHost := true
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	image := "alpine:3.19"

	spec := kov1alpha1.NodeMaintenanceSpec{
		AllNodes: allNodes,
		Script: &kov1alpha1.ScriptSpec{
			Image:          image,
			TimeoutSeconds: &timeoutSec,
			RunOnHost:      &runOnHost,
		},
		Actions: []kov1alpha1.ActionSpec{
			{Type: kov1alpha1.ActionScript},
		},
	}
	if !allNodes {
		if nodesCSV != "" {
			for _, n := range strings.Split(nodesCSV, ",") {
				n = strings.TrimSpace(n)
				if n != "" {
					spec.NodeNames = append(spec.NodeNames, n)
				}
			}
		} else if selector != "" {
			spec.NodeSelector = parseSelector(selector)
		}
	}

	nm := &kov1alpha1.NodeMaintenance{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kov1alpha1.GroupVersion.String(),
			Kind:       "NodeMaintenance",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	anns := map[string]string{
		"ko.io/runner-namespace": namespace,
	}
	if t := spec.SummarizeTargets(); t != "" {
		anns["ko.io/targets"] = t
	}
	nm.SetAnnotations(anns)
	return nm
}

func buildPushScript(remotePath, b64Payload, mode string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
dst=%q
tmp="${dst}.tmp.$$"
mkdir -p "$(dirname "$dst")"
base64 -d > "$tmp" <<'_KO_PAYLOAD_'
%s
_KO_PAYLOAD_
chmod %s "$tmp"
mv -f "$tmp" "$dst"
echo "pushed $(wc -c < "$dst") bytes to $dst"
`, remotePath, b64Payload, mode)
}

func buildPullScript(remotePath string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
src=%q
if [ ! -f "$src" ]; then
	echo "no such file: $src" >&2
	exit 2
fi
echo %q
base64 -w0 "$src"
echo
echo %q
`, remotePath, pullSentinelBegin, pullSentinelEnd)
}

func waitForNMCompletion(ctx context.Context, c *Clients, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		u, err := c.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get nm/%s: %w", name, err)
		}
		phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
		switch kov1alpha1.Phase(phase) {
		case kov1alpha1.PhaseCompleted:
			return nil
		case kov1alpha1.PhaseFailed:
			return fmt.Errorf("nodemaintenance %s failed; inspect: kubectl nm status %s", name, name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for completion (current phase: %q)", timeout, phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func printNodeSummary(ctx context.Context, c *Clients, name string) error {
	u, err := c.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	nodes, _, _ := unstructured.NestedSlice(u.Object, "status", "nodes")
	if len(nodes) == 0 {
		return nil
	}
	for _, raw := range nodes {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		nName, _, _ := unstructured.NestedString(m, "name")
		nPhase, _, _ := unstructured.NestedString(m, "phase")
		exit, _, _ := unstructured.NestedInt64(m, "scriptExitCode")
		msg, _, _ := unstructured.NestedString(m, "message")
		fmt.Printf("  %-32s  %-10s  exit=%d  %s\n", nName, nPhase, exit, truncate(msg, 60))
	}
	return nil
}

func getRunnerPodName(ctx context.Context, c *Clients, nmName, node string) (string, error) {
	u, err := c.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, nmName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	nodes, _, _ := unstructured.NestedSlice(u.Object, "status", "nodes")
	for _, raw := range nodes {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		nName, _, _ := unstructured.NestedString(m, "name")
		if nName != node {
			continue
		}
		pod, _, _ := unstructured.NestedString(m, "scriptPodName")
		if pod == "" {
			return "", fmt.Errorf("scriptPodName not recorded for node %s", node)
		}
		return pod, nil
	}
	return "", fmt.Errorf("node %s not present in status.nodes", node)
}

func fetchPodLogs(ctx context.Context, c *Clients, namespace, pod string) ([]byte, error) {
	req := c.Kube.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: "run"})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return io.ReadAll(stream)
}

func extractPayload(logs []byte) (string, error) {
	s := string(logs)
	bIdx := strings.Index(s, pullSentinelBegin)
	eIdx := strings.Index(s, pullSentinelEnd)
	if bIdx < 0 || eIdx < 0 || eIdx <= bIdx {
		return "", fmt.Errorf("payload sentinels not found in pod logs (file probably missing or script failed)")
	}
	payload := s[bIdx+len(pullSentinelBegin) : eIdx]
	return strings.TrimSpace(payload), nil
}

func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:8]
	}
	return hex.EncodeToString(b[:])
}
