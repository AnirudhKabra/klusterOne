// Package cli implements the kubectl-nm subcommands.
package cli

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Clients bundles the two kube client flavors the subcommands need:
//   - typed (kubernetes.Interface) for Pod/ConfigMap/Log work,
//   - dynamic (no scheme registration) for the NodeMaintenance CRD.
type Clients struct {
	Kube kubernetes.Interface
	Dyn  dynamic.Interface
}

// GVR for the NodeMaintenance custom resource.
var NodeMaintenanceGVR = schema.GroupVersionResource{
	Group:    "ko.io",
	Version:  "v1alpha1",
	Resource: "nodemaintenances",
}

// RunnerNamespace is where the controller spawns runner Pods and where the
// CLI creates the backing script ConfigMaps. It is a fixed convention; the
// controller's `runnerNamespace` constant (cmd/manager/main.go) is the
// matching half. The namespace must be labelled to allow privileged Pod
// Security — the runner Pod is privileged + hostPID/hostNetwork/hostIPC.
const RunnerNamespace = "ko-system"

// newClients resolves kubeconfig the same way kubectl does
// ($KUBECONFIG → ~/.kube/config → in-cluster) and builds both client flavors.
func newClients() (*Clients, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kube client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return &Clients{Kube: kube, Dyn: dyn}, nil
}

func loadConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err == nil {
		return cfg, nil
	}
	// Last resort: in-cluster.
	if inCluster, icErr := rest.InClusterConfig(); icErr == nil {
		return inCluster, nil
	}
	return nil, fmt.Errorf("load kubeconfig: %w", err)
}
