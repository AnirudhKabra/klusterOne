package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// RunStatus prints a compact view of an NM: phase, paused, plus per-node table.
func RunStatus(ctx context.Context, args []string) error {
	name, rest, err := splitPositional(args, "status", "<name>")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
		return err
	}

	clients, err := newClients()
	if err != nil {
		return err
	}

	u, err := clients.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("nodemaintenance %q not found", name)
	}
	if err != nil {
		return err
	}

	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	paused, _, _ := unstructured.NestedBool(u.Object, "spec", "paused")
	startTime, _, _ := unstructured.NestedString(u.Object, "status", "startTime")
	completionTime, _, _ := unstructured.NestedString(u.Object, "status", "completionTime")

	fmt.Printf("Name:           %s\n", name)
	fmt.Printf("Phase:          %s\n", orDash(phase))
	fmt.Printf("Paused:         %v\n", paused)
	if reason := u.GetAnnotations()[pauseReasonAnnotation]; reason != "" {
		fmt.Printf("PauseReason:    %s\n", reason)
	}
	if startTime != "" {
		fmt.Printf("StartTime:      %s\n", startTime)
	}
	if completionTime != "" {
		fmt.Printf("CompletionTime: %s\n", completionTime)
	}

	nodes, _, _ := unstructured.NestedSlice(u.Object, "status", "nodes")
	if len(nodes) == 0 {
		fmt.Println("\n(no per-node status yet)")
		return nil
	}

	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tPHASE\tACTION\tCOMPLETED\tEXIT\tMESSAGE")
	for _, raw := range nodes {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		nName, _, _ := unstructured.NestedString(m, "name")
		nPhase, _, _ := unstructured.NestedString(m, "phase")
		nAction, _, _ := unstructured.NestedString(m, "currentAction")
		completed, _, _ := unstructured.NestedStringSlice(m, "completedActions")
		exit, _, _ := unstructured.NestedInt64(m, "scriptExitCode")
		exitStr := "-"
		if _, found, _ := unstructured.NestedFieldNoCopy(m, "scriptExitCode"); found {
			exitStr = fmt.Sprintf("%d", exit)
		}
		msg, _, _ := unstructured.NestedString(m, "message")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			nName, orDash(nPhase), orDash(nAction),
			strings.Join(completed, ","),
			exitStr,
			truncate(msg, 60),
		)
	}
	return tw.Flush()
}

// RunLogs streams the runner Pod logs for a given NM (and optionally a single
// node). When --follow is set, we tail the live container.
func RunLogs(ctx context.Context, args []string) error {
	name, rest, err := splitPositional(args, "logs", "<name>")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		node    string
		follow  bool
		runnerN string
	)
	fs.StringVar(&node, "node", "", "Only show logs for this node.")
	fs.BoolVar(&follow, "f", false, "Follow logs.")
	fs.StringVar(&runnerN, "runner-namespace", "", "Override runner namespace (default: annotation or ko-system).")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	clients, err := newClients()
	if err != nil {
		return err
	}

	u, err := clients.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get nm/%s: %w", name, err)
	}

	ns := runnerN
	if ns == "" {
		ns = u.GetAnnotations()["ko.io/runner-namespace"]
	}
	if ns == "" {
		ns = "ko-system"
	}

	nodes, _, _ := unstructured.NestedSlice(u.Object, "status", "nodes")
	any := false
	for _, raw := range nodes {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		nName, _, _ := unstructured.NestedString(m, "name")
		if node != "" && nName != node {
			continue
		}
		podName, _, _ := unstructured.NestedString(m, "scriptPodName")
		if podName == "" {
			continue
		}
		any = true
		fmt.Printf("==> %s (pod %s/%s) <==\n", nName, ns, podName)
		if err := streamPodLogs(ctx, clients, ns, podName, follow); err != nil {
			fmt.Fprintf(os.Stderr, "  (logs error: %v)\n", err)
		}
	}
	if !any {
		return fmt.Errorf("no runner pod found in status (yet)")
	}
	return nil
}

func streamPodLogs(ctx context.Context, c *Clients, ns, name string, follow bool) error {
	req := c.Kube.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{
		Container: "run",
		Follow:    follow,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = io.Copy(os.Stdout, stream)
	return err
}

// scriptCMRefFromUnstructured pulls (name, namespace, key) out of
// spec.script.configMapRef on the unstructured NM object.
func scriptCMRefFromUnstructured(u *unstructured.Unstructured) (name, namespace, key string, err error) {
	name, _, _ = unstructured.NestedString(u.Object, "spec", "script", "configMapRef", "name")
	namespace, _, _ = unstructured.NestedString(u.Object, "spec", "script", "configMapRef", "namespace")
	key, _, _ = unstructured.NestedString(u.Object, "spec", "script", "configMapRef", "key")
	return name, namespace, key, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
