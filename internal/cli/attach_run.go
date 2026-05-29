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

// maxInlineScriptBytes caps the script body we accept on
// spec.script.inline. The materialized ConfigMap has a hard 1 MiB ceiling
// in the API server, plus we want headroom for the CR's own metadata and
// status. 900 KiB leaves room for both without surprises and matches the
// (~700 KiB-after-base64) ceiling that `push` / `pull` already use.
const maxInlineScriptBytes = 900 * 1024

// hasScript reports whether the live NM has a script body the controller
// can execute. Empty inline means `kubectl nm run` would materialize an
// empty CM and run a no-op on every node — which looks like Completed in
// `kubectl get nm`. We treat that as a user error and surface it before
// issuing the patch.
func hasScript(u *unstructured.Unstructured) bool {
	inline, _, _ := unstructured.NestedString(u.Object, "spec", "script", "inline")
	return inline != ""
}

// RunAttach patches the script body on an existing NodeMaintenance.
//
// The new body is written to spec.script.inline on the CR. The controller
// re-materializes the backing ConfigMap on its next reconcile pass.
//
// Why patch the CR rather than the CM directly? Putting the script body on
// the CR means an attach (a) bumps metadata.generation and shows up in
// kube-apiserver audit logs alongside every other spec mutation, and (b)
// only needs nodemaintenances.ko.io permissions — the operator never has to
// be granted configmaps.update on ko-system. That is the same RBAC bar as
// kubectl nm create / pause / run, which closes the silent-tamper vector
// where someone with ko-system ConfigMap write could rewrite scripts behind
// the NM author's back.
func RunAttach(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kubectl nm attach <name> <script-path>")
		fmt.Fprintln(os.Stderr, "       Patches spec.script.inline on an existing NodeMaintenance.")
		fmt.Fprintln(os.Stderr, "       Safe to run while paused; the controller picks up the new body on the next Script action.")
	}

	if hasHelpFlag(args) {
		fs.Usage()
		return nil
	}
	if len(args) < 2 {
		fs.Usage()
		return fmt.Errorf("missing positional arguments")
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
	if len(body) > maxInlineScriptBytes {
		return fmt.Errorf("script too large: %d bytes (limit %d). "+
			"Inline scripts must fit in a single ConfigMap; "+
			"split the work across multiple NMs or fetch a larger payload from inside the script.",
			len(body), maxInlineScriptBytes)
	}

	clients, err := newClients()
	if err != nil {
		return err
	}

	// Get the NM up front so we can produce friendlier errors than the
	// patch alone would, and so we can tailor the success message to the
	// NM's current phase/pause state.
	u, err := clients.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("nodemaintenance %q not found", name)
		}
		return fmt.Errorf("get nm/%s: %w", name, err)
	}
	paused, _, _ := unstructured.NestedBool(u.Object, "spec", "paused")
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")

	// JSON merge patch (RFC 7396): set the new inline body on the CR. The
	// controller re-materializes the backing CM on its next reconcile.
	patchObj := map[string]any{
		"spec": map[string]any{
			"script": map[string]any{
				"inline": string(body),
			},
		},
	}
	patchBytes, err := json.Marshal(patchObj)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	if _, err := clients.Dyn.Resource(NodeMaintenanceGVR).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patch nm/%s: %w", name, err)
	}

	fmt.Printf("nodemaintenance.ko.io/%s script updated (%d bytes)\n", name, len(body))

	// Surface the most common "what's next" / "wait, didn't this just race?"
	// concerns. The controller materializes the new body on the next
	// reconcile pass; what that means for the user depends on whether the
	// run is paused, in-flight, or already done.
	switch {
	case paused:
		fmt.Printf("  next: kubectl nm run %s   # NM is paused; the new script will apply on resume\n", name)
	case phase == "InProgress":
		fmt.Fprintf(os.Stderr,
			"  warning: NM is in phase InProgress. Nodes already executing the Script action "+
				"finish with the old body; pending nodes pick up the new one. "+
				"Use `kubectl nm pause %s` first if you want a clean cutover.\n", name)
	case phase == "Completed" || phase == "Failed":
		fmt.Fprintf(os.Stderr,
			"  note: NM has already finished (phase=%s); the new script will not run. "+
				"Create a new NM or delete this one and re-create.\n", phase)
	}
	return nil
}

// RunRun unpauses an existing NodeMaintenance and clears any pause-reason
// annotation. Idempotent: when the NM is already running, prints a friendly
// message and returns without issuing a patch.
//
// Refuses to start an NM that has no script body attached (empty
// spec.script.inline). Without this check, the controller would
// materialize an empty CM and run a no-op script on every targeted node,
// and the run would show as Completed — the kind of "everything's fine!"
// failure mode that hides real bugs.
func RunRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kubectl nm run <name>")
		fmt.Fprintln(os.Stderr, "       Unpauses an existing NodeMaintenance (clears spec.paused).")
	}

	if hasHelpFlag(args) {
		fs.Usage()
		return nil
	}
	name, rest, err := splitPositional(args, "run", "<name>")
	if err != nil {
		return err
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}

	clients, err := newClients()
	if err != nil {
		return err
	}

	u, err := clients.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("nodemaintenance %q not found", name)
		}
		return fmt.Errorf("get nm/%s: %w", name, err)
	}
	if !hasScript(u) {
		return fmt.Errorf(
			"nodemaintenance %q has no script attached. Provide one with:\n  kubectl nm attach %s <script-path>",
			name, name)
	}

	return setPaused(ctx, clients, name, false, "")
}

// RunPause pauses an existing NodeMaintenance by patching spec.paused=true.
// With --reason, the supplied string is stamped onto the NM as the
// ko.io/pause-reason annotation; without --reason, any existing annotation is
// cleared. Idempotent: a no-op when the NM is already paused with the same
// (or no) reason.
func RunPause(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var reason string
	fs.StringVar(&reason, "reason", "",
		"Free-form reason for pausing; stored as the ko.io/pause-reason annotation on the NM.")

	if hasHelpFlag(args) {
		fs.Usage()
		return nil
	}
	name, rest, err := splitPositional(args, "pause", "<name>")
	if err != nil {
		return err
	}
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
