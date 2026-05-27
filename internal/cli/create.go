package cli

import (
	"context"
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
	fs.StringVar(&scriptPath, "script", "", "Path to a script file (creates a ConfigMap).")
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

	nm := buildNM(name, scriptBody, image, timeout, paused, allNodes, atOnce,
		maxUnavailable, selector, nodes, includeCordon, includeDrain, includeUncord, inPod)

	if dryRun || outputYAML {
		return writeYAML(os.Stdout, nm)
	}

	clients, err := newClients()
	if err != nil {
		return err
	}
	return applyNM(ctx, clients, nm)
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

// applyNM creates the NodeMaintenance (or updates it if it already exists)
// and ensures the backing ConfigMap exists with the script body. The CLI
// always uses an *external* ConfigMap rather than the inline path so that
// `kubectl nm attach` can later overwrite it without touching the CR.
func applyNM(ctx context.Context, c *Clients, nm *kov1alpha1.NodeMaintenance) error {
	// Always use the canonical runner namespace. The annotation is still
	// emitted for the controller's benefit but is no longer user-tunable.
	runnerNS := RunnerNamespace

	if err := ensureNamespace(ctx, c, runnerNS); err != nil {
		return fmt.Errorf("ensure namespace %s: %w", runnerNS, err)
	}

	cmName := fmt.Sprintf("nm-%s-script", nm.Name)
	body := ""
	if nm.Spec.Script != nil {
		body = nm.Spec.Script.Inline
	}
	if err := upsertScriptConfigMap(ctx, c, runnerNS, cmName, nm.Name, body); err != nil {
		return fmt.Errorf("upsert script configmap: %w", err)
	}

	// Now flip the spec to reference the ConfigMap instead of inlining the
	// script, then push the CR.
	if nm.Spec.Script != nil {
		nm.Spec.Script.Inline = ""
		nm.Spec.Script.ConfigMapRef = &kov1alpha1.ScriptConfigMapRef{
			Name: cmName,
		}
	}

	u, err := toUnstructured(nm)
	if err != nil {
		return err
	}

	var live *unstructured.Unstructured
	existing, getErr := c.Dyn.Resource(NodeMaintenanceGVR).Get(ctx, nm.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(getErr):
		live, err = c.Dyn.Resource(NodeMaintenanceGVR).Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create NodeMaintenance: %w", err)
		}
		fmt.Printf("nodemaintenance.ko.io/%s created\n", nm.Name)
	case getErr != nil:
		return fmt.Errorf("get NodeMaintenance: %w", getErr)
	default:
		u.SetResourceVersion(existing.GetResourceVersion())
		live, err = c.Dyn.Resource(NodeMaintenanceGVR).Update(ctx, u, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update NodeMaintenance: %w", err)
		}
		fmt.Printf("nodemaintenance.ko.io/%s updated\n", nm.Name)
	}

	if err := setConfigMapOwner(ctx, c, runnerNS, cmName, live); err != nil {
		fmt.Fprintf(os.Stderr, "warning: configmap/%s ownerReferences not set (CM will not be garbage-collected with the NM): %v\n", cmName, err)
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

func ensureNamespace(ctx context.Context, c *Clients, ns string) error {
	_, err := c.Kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = c.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func upsertScriptConfigMap(ctx context.Context, c *Clients, ns, name, owner, body string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"ko.io/owner": owner},
		},
		Data: map[string]string{"script.sh": body},
	}
	_, err := c.Kube.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, gerr := c.Kube.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		existing.Data = cm.Data
		existing.Labels = cm.Labels
		_, err = c.Kube.CoreV1().ConfigMaps(ns).Update(ctx, existing, metav1.UpdateOptions{})
	}
	return err
}

// setConfigMapOwner ensures the script ConfigMap carries an ownerReference to
// the owning NodeMaintenance, so Kubernetes garbage-collects it whenever the
// NM is deleted. Cluster-scoped owner + namespaced dependent is an
// explicitly-supported direction.
//
// Idempotent: a no-op when the CM already references the live NM's UID. Also
// safe for "old" CMs that pre-date this fix — the first time we touch them
// (e.g. via kubectl nm attach), we'll backfill the ownerRef.
func setConfigMapOwner(ctx context.Context, c *Clients, ns, cmName string, nm *unstructured.Unstructured) error {
	if nm == nil || nm.GetUID() == "" {
		return fmt.Errorf("owner NodeMaintenance has no UID")
	}
	cm, err := c.Kube.CoreV1().ConfigMaps(ns).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	desired := metav1.OwnerReference{
		APIVersion: nm.GetAPIVersion(),
		Kind:       nm.GetKind(),
		Name:       nm.GetName(),
		UID:        nm.GetUID(),
	}
	for _, r := range cm.OwnerReferences {
		if r.UID == desired.UID {
			return nil
		}
	}
	cm.OwnerReferences = append(cm.OwnerReferences, desired)
	if _, err := c.Kube.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}
