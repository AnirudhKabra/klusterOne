package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// pauseReasonAnnotation captures the operator-supplied reason for pausing an
// NM run. It is set by `kubectl nm pause --reason ...`, cleared by either
// `kubectl nm pause` (no reason) or `kubectl nm run`, and is advisory only —
// the controller does not read it.
const pauseReasonAnnotation = "ko.io/pause-reason"

// RunAttach overwrites the ConfigMap backing the script for an existing NM.
// The NM is looked up by name; we read spec.script.configMapRef and write the
// new file contents into that ConfigMap. The NM CR is NOT modified — just the
// data behind it — so the controller will pick up the new script on the next
// Script action invocation.
func RunAttach(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	if len(args) < 2 {
		return fmt.Errorf("usage: kubectl nm attach <name> <script-path>")
	}
	name := args[0]
	scriptPath := args[1]
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected extra arguments: %v", fs.Args())
	}

	body, err := os.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", scriptPath, err)
	}

	clients, err := newClients()
	if err != nil {
		return err
	}

	u, err := clients.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get nm/%s: %w", name, err)
	}

	cmName, cmNS, _, err := scriptCMRefFromUnstructured(u)
	if err != nil {
		return fmt.Errorf("inspect spec.script.configMapRef: %w", err)
	}
	if cmName == "" {
		// Fallback to the convention used by `kubectl nm create`.
		cmName = fmt.Sprintf("nm-%s-script", name)
	}
	if cmNS == "" {
		cmNS = "ko-system"
	}

	if err := upsertScriptConfigMap(ctx, clients, cmNS, cmName, name, string(body)); err != nil {
		return fmt.Errorf("update configmap %s/%s: %w", cmNS, cmName, err)
	}
	fmt.Printf("configmap/%s in namespace %s updated (%d bytes)\n", cmName, cmNS, len(body))
	return nil
}

// RunRun unpauses an existing NodeMaintenance and clears any pause-reason
// annotation. Idempotent: when the NM is already running, prints a friendly
// message and returns without issuing a patch.
func RunRun(ctx context.Context, args []string) error {
	name, rest, err := splitPositional(args, "run", "<name>")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
		return err
	}

	clients, err := newClients()
	if err != nil {
		return err
	}
	return setPaused(ctx, clients, name, false, "")
}

// RunPause pauses an existing NodeMaintenance by patching spec.paused=true.
// With --reason, the supplied string is stamped onto the NM as the
// ko.io/pause-reason annotation; without --reason, any existing annotation is
// cleared. Idempotent: a no-op when the NM is already paused with the same
// (or no) reason.
func RunPause(ctx context.Context, args []string) error {
	name, rest, err := splitPositional(args, "pause", "<name>")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var reason string
	fs.StringVar(&reason, "reason", "",
		"Free-form reason for pausing; stored as the ko.io/pause-reason annotation on the NM.")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	clients, err := newClients()
	if err != nil {
		return err
	}
	return setPaused(ctx, clients, name, true, reason)
}

// setPaused flips spec.paused on an NM and manages the ko.io/pause-reason
// annotation. When pausing, the supplied reason is written (or cleared if
// empty); when unpausing, the annotation is always cleared.
//
// The operation is idempotent: if the desired (paused, reason) tuple already
// matches the live object, we print a friendly message and return without
// issuing a patch.
func setPaused(ctx context.Context, c *Clients, name string, paused bool, reason string) error {
	u, err := c.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("nodemaintenance %q not found", name)
	}
	if err != nil {
		return fmt.Errorf("get nm/%s: %w", name, err)
	}

	current, _, _ := unstructured.NestedBool(u.Object, "spec", "paused")
	currentReason := u.GetAnnotations()[pauseReasonAnnotation]

	// The reason only carries semantics while paused; unpausing clears it.
	desiredReason := ""
	if paused {
		desiredReason = reason
	}

	if current == paused && currentReason == desiredReason {
		switch {
		case paused && currentReason != "":
			fmt.Printf("nodemaintenance.ko.io/%s already paused (reason: %s)\n", name, currentReason)
		case paused:
			fmt.Printf("nodemaintenance.ko.io/%s already paused\n", name)
		default:
			fmt.Printf("nodemaintenance.ko.io/%s already running\n", name)
		}
		return nil
	}

	// JSON merge patch (RFC 7396): null deletes the key. Encode "no reason"
	// as a nil interface{} so it marshals to JSON null.
	var annotationValue any
	if desiredReason != "" {
		annotationValue = desiredReason
	}
	patchObj := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				pauseReasonAnnotation: annotationValue,
			},
		},
		"spec": map[string]any{
			"paused": paused,
		},
	}
	patchBytes, err := json.Marshal(patchObj)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	if _, err := c.Dyn.Resource(NodeMaintenanceGVR).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patch nm/%s: %w", name, err)
	}

	switch {
	case paused && desiredReason != "":
		fmt.Printf("nodemaintenance.ko.io/%s paused (reason: %s)\n", name, desiredReason)
	case paused:
		fmt.Printf("nodemaintenance.ko.io/%s paused\n", name)
	default:
		fmt.Printf("nodemaintenance.ko.io/%s unpaused\n", name)
	}
	return nil
}
