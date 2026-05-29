package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
)

// RunCreate builds a NodeMaintenance object from CLI flags and either prints
// it (--dry-run / -o yaml) or applies it to the cluster.
func RunCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		scriptPath     string
		inline         string
		allNodes       bool
		atOnce         bool
		maxUnavailable int
		selector       string
		nodes          string
		includeCordon  bool
		includeUncord  bool
		includeDrain   bool
		noCordon       bool
		noUncord       bool
		timeout        time.Duration
		image          string
		inPod          bool
		paused         bool
		dryRun         bool
		outputYAML     bool
	)
	fs.StringVar(&scriptPath, "script", "", "Path to a script file; body is placed in spec.script.inline on the NM.")
	fs.StringVar(&inline, "inline", "", "Inline script body (mutually exclusive with --script).")
	fs.BoolVar(&allNodes, "all-nodes", false, "Target every node in the cluster.")
	fs.BoolVar(&atOnce, "at-once", false, "Run on all targeted nodes in parallel.")
	fs.IntVar(&maxUnavailable, "max-unavailable", 0, "Max nodes in-flight (default 1; ignored with --at-once).")
	fs.StringVar(&selector, "selector", "", "Label selector, e.g. role=worker,zone=us-east.")
	fs.StringVar(&nodes, "nodes", "", "Comma-separated explicit node names.")
	fs.BoolVar(&includeCordon, "cordon", true, "Include Cordon action (disable with --cordon=false or --no-cordon).")
	fs.BoolVar(&includeUncord, "uncordon", true, "Include Uncordon action (disable with --uncordon=false or --no-uncordon).")
	fs.BoolVar(&includeDrain, "drain", false, "Include Drain action (between Cordon and Script).")
	fs.BoolVar(&noCordon, "no-cordon", false, "Alias for --cordon=false.")
	fs.BoolVar(&noUncord, "no-uncordon", false, "Alias for --uncordon=false.")
	fs.DurationVar(&timeout, "timeout", 10*time.Minute, "Per-node script execution timeout.")
	fs.StringVar(&image, "image", "", "Runner image (default alpine:3.19).")
	fs.BoolVar(&inPod, "in-pod", false, "Run inside the pod (do not nsenter to the host).")
	fs.BoolVar(&paused, "paused", false, "Create paused; resume with 'kubectl nm run'. Makes --script/--inline optional.")
	fs.BoolVar(&dryRun, "dry-run", false, "Print the generated NodeMaintenance YAML without applying.")
	fs.BoolVar(&outputYAML, "o", false, "Alias for --dry-run (YAML output).")

	if hasHelpFlag(args) {
		fs.Usage()
		return nil
	}

	name, rest, err := splitPositional(args, "create", "<name>")
	if err != nil {
		fs.Usage()
		return err
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}

	if scriptPath != "" && inline != "" {
		return fmt.Errorf("--script and --inline are mutually exclusive")
	}
	if scriptPath == "" && inline == "" && !paused {
		return fmt.Errorf("no script provided: pass --script PATH or --inline BODY, " +
			"or --paused to defer the script and attach it later with 'kubectl nm attach'")
	}

	// --no-cordon / --no-uncordon are explicit "turn it off" aliases. When set,
	// they override any default or explicit --cordon=true / --uncordon=true.
	if noCordon {
		includeCordon = false
	}
	if noUncord {
		includeUncord = false
	}

	scriptBody := inline
	if scriptPath != "" {
		b, err := os.ReadFile(scriptPath)
		if err != nil {
			return fmt.Errorf("read --script: %w", err)
		}
		scriptBody = string(b)
	}
	if len(scriptBody) > maxInlineScriptBytes {
		return fmt.Errorf("script too large: %d bytes (limit %d). "+
			"Inline scripts must fit in a single ConfigMap; "+
			"split the work across multiple NMs or fetch a larger payload from inside the script.",
			len(scriptBody), maxInlineScriptBytes)
	}

	nm := buildNM(name, scriptBody, image, timeout, paused, allNodes, atOnce,
		maxUnavailable, selector, nodes, includeCordon, includeDrain, includeUncord, inPod)

	if dryRun || outputYAML {
		return writeYAML(os.Stdout, nm)
	}

	clients, err := newClients()
	if err != nil {
		return err
	}
	if err := applyNM(ctx, clients, nm); err != nil {
		return err
	}

	// Hint at the rest of the two-phase workflow when the user opted into
	// it. `kubectl nm run` will refuse to start an NM with no script, so
	// this is also a reminder to attach before running.
	if paused && scriptBody == "" {
		fmt.Printf("  next: kubectl nm attach %s <script-path>\n", name)
		fmt.Printf("        kubectl nm run    %s\n", name)
	} else if paused {
		fmt.Printf("  next: kubectl nm run %s   # NM is paused\n", name)
	}
	return nil
}

// buildNM constructs the typed NodeMaintenance object. Defaults for action
// list mirror the orchestrator's behavior so the CLI's --no-cordon /
// --no-uncordon flags do something visible in the printed YAML.
func buildNM(
	name, scriptBody, image string,
	timeout time.Duration,
	paused, allNodes, atOnce bool,
	maxUnavailable int,
	selector, nodes string,
	includeCordon, includeDrain, includeUncordon bool,
	inPod bool,
) *kov1alpha1.NodeMaintenance {
	timeoutSec := int64(timeout / time.Second)
	if timeoutSec <= 0 {
		timeoutSec = 600
	}

	runOnHost := !inPod
	spec := kov1alpha1.NodeMaintenanceSpec{
		Paused:   paused,
		AllNodes: allNodes,
		Script: &kov1alpha1.ScriptSpec{
			Inline:         scriptBody,
			Image:          image,
			TimeoutSeconds: &timeoutSec,
			RunOnHost:      &runOnHost,
		},
		Strategy: kov1alpha1.Strategy{
			MaxUnavailable: maxUnavailable,
			AtOnce:         atOnce,
		},
	}

	if !allNodes {
		if nodes != "" {
			for _, n := range strings.Split(nodes, ",") {
				n = strings.TrimSpace(n)
				if n != "" {
					spec.NodeNames = append(spec.NodeNames, n)
				}
			}
		}
		if selector != "" && len(spec.NodeNames) == 0 {
			spec.NodeSelector = parseSelector(selector)
		}
	}

	if includeCordon {
		spec.Actions = append(spec.Actions, kov1alpha1.ActionSpec{Type: kov1alpha1.ActionCordon})
	}
	if includeDrain {
		spec.Actions = append(spec.Actions, kov1alpha1.ActionSpec{
			Type:         kov1alpha1.ActionDrain,
			DrainOptions: &kov1alpha1.DrainOptions{IgnoreDaemonSets: true},
		})
	}
	spec.Actions = append(spec.Actions, kov1alpha1.ActionSpec{Type: kov1alpha1.ActionScript})
	if includeUncordon {
		spec.Actions = append(spec.Actions, kov1alpha1.ActionSpec{Type: kov1alpha1.ActionUncordon})
	}

	nm := &kov1alpha1.NodeMaintenance{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kov1alpha1.GroupVersion.String(),
			Kind:       "NodeMaintenance",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	// ko.io/targets renders as the "Targets" printer column in
	// `kubectl get nm`, giving a one-glance summary of who this run touches.
	// Falls back to empty when no target field is set.
	if t := spec.SummarizeTargets(); t != "" {
		nm.SetAnnotations(map[string]string{"ko.io/targets": t})
	}
	return nm
}

func parseSelector(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		out[strings.TrimSpace(kv[:eq])] = strings.TrimSpace(kv[eq+1:])
	}
	return out
}

func writeYAML(w io.Writer, obj runtime.Object) error {
	b, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// applyNM creates the NodeMaintenance (or updates it if it already exists).
//
// The script body lives in spec.script.inline on the CR. The controller
// materializes the backing ConfigMap on its first reconcile pass — the CLI
// never touches ConfigMaps in ko-system, so an operator only needs RBAC on
// nodemaintenances.ko.io to create runs. That closes the silent-tamper
// vector where anyone with configmaps.update on ko-system could rewrite a
// script after the NM was authored. The companion ValidatingAdmissionPolicy
// in config/admission/configmap_lockdown.yaml enforces the other half: in
// ko-system, only the controller's ServiceAccount may CREATE/UPDATE/DELETE
// ConfigMaps at all (plus root-ca-cert-publisher for kube-root-ca.crt).
func applyNM(ctx context.Context, c *Clients, nm *kov1alpha1.NodeMaintenance) error {
	u, err := toUnstructured(nm)
	if err != nil {
		return err
	}

	existing, getErr := c.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, nm.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(getErr):
		if _, err := c.Dyn.Resource(NodeMaintenanceGVR).Create(ctx, u, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create NodeMaintenance: %w", err)
		}
		fmt.Printf("nodemaintenance.ko.io/%s created\n", nm.Name)
	case getErr != nil:
		return fmt.Errorf("get NodeMaintenance: %w", getErr)
	default:
		u.SetResourceVersion(existing.GetResourceVersion())
		if _, err := c.Dyn.Resource(NodeMaintenanceGVR).Update(ctx, u, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update NodeMaintenance: %w", err)
		}
		fmt.Printf("nodemaintenance.ko.io/%s updated\n", nm.Name)
	}
	return nil
}

func toUnstructured(obj runtime.Object) (*unstructured.Unstructured, error) {
	b, err := yaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	j, err := yaml.YAMLToJSON(b)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(j); err != nil {
		return nil, err
	}
	return u, nil
}

