// Command manager is the klusterOne (ko-controller) binary. It registers the
// NodeMaintenance scheme, wires the action registry, and starts the
// controller-runtime manager.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
	"github.com/AnirudhKabra/klusterOne/internal/actions"
	"github.com/AnirudhKabra/klusterOne/internal/controller"
	"github.com/AnirudhKabra/klusterOne/internal/orchestrator"
)

var (
	scheme = runtime.NewScheme() // Creating a new empty scheme...
)

/*
	- Kubernetes APIs communicate using raw JSON/YAML objects, while the controller code works with Go structs.
	- The Scheme connects these two worlds by teaching the operator which Kubernetes object maps to which Go type.

	For example, when the API server sends a Pod object, the Scheme tells the controller to decode it into corev1.Pod. Similarly, when it sees a custom resource like NodeMaintenance, it maps it to kov1alpha1.NodeMaintenance.
*/

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme)) //Adding built-in k8s resources to the scheme...
	utilruntime.Must(kov1alpha1.AddToScheme(scheme))     //Adding our custom resource to the scheme...
}

// runnerNamespace is the namespace where the Script action creates runner
// Pods and materializes the backing script ConfigMaps from
// spec.script.inline. It is a fixed convention, not a tunable — the
// corresponding `RunnerNamespace` constant in `internal/cli/clients.go`
// must stay in sync. The CLI only reads from this namespace, never writes
// ConfigMaps to it; see docs/security.md.
const runnerNamespace = "ko-system"

func main() {
	var (
		metricsAddr        string
		probeAddr          string
		drainTimeout       time.Duration
		requeueInterval    time.Duration
		runnerImage        string
		runnerKeepPods     bool
		runnerPollInterval time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.DurationVar(&drainTimeout, "drain-default-timeout", 5*time.Minute, "Default per-node drain timeout when not set in the spec.")
	flag.DurationVar(&requeueInterval, "requeue-interval", 10*time.Second, "How often to re-reconcile in-progress NodeMaintenance objects.")
	flag.StringVar(&runnerImage, "runner-image", "alpine:3.19", "Default container image for the Script runner Pod.")
	flag.BoolVar(&runnerKeepPods, "runner-keep-pods", false, "Keep Script runner Pods around after they terminate (debug).")
	flag.DurationVar(&runnerPollInterval, "runner-poll-interval", 5*time.Second, "How often the Script action polls the runner Pod for terminal phase.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)

	cfg := ctrl.GetConfigOrDie() // Fetching KUBECONFIG...

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr}, //In future, we can add custommetrics to the manager...
	})
	if err != nil {
		logger.Error(err, "unable to start manager")
		os.Exit(1)
	}

	/*
		With `ctrl.NewManager`

		controller-runtime automatically starts something called informers and watches in the background.

		These informers continuously watch Kubernetes resources and maintain a local in-memory cache inside the operator process.

		So reads become local memory lookups instead of network API calls.

							Kubernetes API Server
						(Source of Truth / Live State)
									│
									│ 1. Initial LIST
									▼
							┌────────────────────┐
							│    Informer        │
							│ (started by mgr)   │
							└────────────────────┘
									│
									│ Stores objects
									▼
							┌────────────────────┐
							│ Shared Local Cache │
							│   (in-memory)      │
							└────────────────────┘
									▲
					Reads happen here │
									│
				┌─────────────────────┼─────────────────────┐
				│                     │                     │
				▼                     ▼                     ▼
		Reconcile #1          Reconcile #2          Reconcile #3
		mgr.GetClient()       mgr.GetClient()       mgr.GetClient()

		Initial startup flow:
			1. Operator starts -> Manager starts
			2. Manager starts informers
			3. Informer performs LIST request
			4. Kubernetes returns all objects
			5. Informer stores them in local cache
			6. Reconciler reads from cache

		Continuous update flow:
			Kubernetes Object Changes
					│
					▼
			API Server emits WATCH event
					│
					▼
			Informer receives event
					│
					▼
			Cache automatically updated
					│
					▼
		Future reconciles see latest state
	*/

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "unable to build kube client")
		os.Exit(1)
	}

	registry := actions.NewRegistry()
	registry.Register(kov1alpha1.ActionCordon, &actions.Cordon{Client: kubeClient})
	registry.Register(kov1alpha1.ActionUncordon, &actions.Uncordon{Client: kubeClient})
	registry.Register(kov1alpha1.ActionDrain, &actions.Drain{
		Client:         kubeClient,
		DefaultTimeout: drainTimeout,
		PollInterval:   5 * time.Second,
	})
	registry.Register(kov1alpha1.ActionScript, &actions.Script{
		Client:          kubeClient,
		RunnerNamespace: runnerNamespace,
		DefaultImage:    runnerImage,
		PollInterval:    runnerPollInterval,
		KeepPods:        runnerKeepPods,
	})

	reconciler := &controller.NodeMaintenanceReconciler{
		Client:          mgr.GetClient(),
		Orchestrator:    orchestrator.New(kubeClient, registry),
		Kube:            kubeClient,
		RunnerNamespace: runnerNamespace,
		RequeueInterval: requeueInterval,
	}

	/*
		The reconciler uses mgr.GetClient() for the NodeMaintenance CR only. Reads come from a local cache, so they are fast. Writes go to the API server, and the cache updates later through watch events.

		The orchestrator uses kubeClient for Nodes, Pods, and ConfigMaps. It always talks directly to the API server for both reads and writes, so it is always up to date and good for real-time actions like cordon, drain, and eviction.
	*/

	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to set up reconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	logger.Info("starting klusterOne (ko-controller) manager",
		"runnerNamespace", runnerNamespace,
		"runnerImage", runnerImage)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited with error")
		os.Exit(1)
	}

	/*
		mgr.Start() runs the manager, and the manager already has your controller registered (from Complete(r)), so it knows when to trigger Reconcile() based on events from the Kubernetes cache/watch system.

		When NodeMaintenance object is created/updated:
			Kubernetes API Server
					↓
			Informer (watch stream)
					↓
			Local cache updated
					↓
			Event generated (Add/Update/Delete)
					↓
			Controller queue
					↓
			Reconciler triggered → r.Reconcile()
	*/
}
