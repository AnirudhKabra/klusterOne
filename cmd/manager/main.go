// Command manager is the Fury Controller binary. It registers the
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

	furyv1alpha1 "github.com/fury/fury-controller/api/v1alpha1"
	"github.com/fury/fury-controller/internal/actions"
	"github.com/fury/fury-controller/internal/controller"
	"github.com/fury/fury-controller/internal/orchestrator"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(furyv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		leaderElect     bool
		drainTimeout    time.Duration
		requeueInterval time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for HA deployments.")
	flag.DurationVar(&drainTimeout, "drain-default-timeout", 5*time.Minute, "Default per-node drain timeout when not set in the spec.")
	flag.DurationVar(&requeueInterval, "requeue-interval", 10*time.Second, "How often to re-reconcile in-progress NodeMaintenance objects.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)

	cfg := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "fury-controller.fury.io",
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		logger.Error(err, "unable to start manager")
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "unable to build kube client")
		os.Exit(1)
	}

	registry := actions.NewRegistry()
	registry.Register(furyv1alpha1.ActionCordon, &actions.Cordon{Client: kubeClient})
	registry.Register(furyv1alpha1.ActionUncordon, &actions.Uncordon{Client: kubeClient})
	registry.Register(furyv1alpha1.ActionDrain, &actions.Drain{
		Client:         kubeClient,
		DefaultTimeout: drainTimeout,
		PollInterval:   5 * time.Second,
	})

	reconciler := &controller.NodeMaintenanceReconciler{
		Client:          mgr.GetClient(),
		Orchestrator:    orchestrator.New(kubeClient, registry),
		RequeueInterval: requeueInterval,
	}
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

	logger.Info("starting Fury Controller manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
