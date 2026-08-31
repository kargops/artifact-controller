package main

import (
	"context"
	"flag"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/controller"
	"github.com/kargops/artifact-controller/internal/generator"
	"github.com/kargops/artifact-controller/internal/store"
	"github.com/kargops/artifact-controller/internal/store/ami"
	"github.com/kargops/artifact-controller/internal/store/fake"
	"github.com/kargops/artifact-controller/internal/store/httpstore"
	"github.com/kargops/artifact-controller/internal/store/oci"
	"github.com/kargops/artifact-controller/internal/store/repomanager"
	"github.com/kargops/artifact-controller/internal/store/s3"
)

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		concurrent           int
		enableFakeStore      bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "Metrics endpoint bind address ('0' disables).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe bind address.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election.")
	flag.IntVar(&concurrent, "concurrent", 4, "Concurrent reconciles.")
	flag.BoolVar(&enableFakeStore, "enable-fake-store", false,
		"Register the in-memory 'fake' store driver. For local demos only: it keeps artifacts "+
			"in controller memory, so nothing can ever satisfy them in a real cluster.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "add client-go scheme")
		os.Exit(1)
	}
	if err := artifactsv1.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "add artifacts.kargops.dev scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "artifact-controller.artifacts.kargops.dev",
		// Hand the lease back on graceful shutdown instead of making the
		// successor wait out the ~15s expiry — otherwise every rollout costs
		// that long in stalled reconciliation.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "create manager")
		os.Exit(1)
	}

	registry := store.NewRegistry()
	s3.Register(registry)
	oci.Register(registry)
	ami.Register(registry)
	// The http driver reads credentials from secrets in the controller's own
	// namespace only — a class cannot point it at another namespace's secrets.
	secretResolver := func(ctx context.Context, name string) (map[string][]byte, error) {
		var secret corev1.Secret
		key := types.NamespacedName{Namespace: controllerNamespace(), Name: name}
		if err := mgr.GetClient().Get(ctx, key, &secret); err != nil {
			return nil, err
		}
		return secret.Data, nil
	}
	httpstore.Register(registry, secretResolver)
	// Both repository managers take credentials the same way the http driver
	// does: from the controller's own namespace, never a class-named one
	// elsewhere.
	repomanager.Register(registry, secretResolver)
	if enableFakeStore {
		fake.Register(registry, fake.New())
		setupLog.Info("WARNING: the in-memory fake store driver is enabled; do not use it in production")
	}

	eval, err := generator.NewEvaluator()
	if err != nil {
		setupLog.Error(err, "create CEL evaluator")
		os.Exit(1)
	}

	reconciler := &controller.ArtifactReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Registry:                registry,
		Eval:                    eval,
		Recorder:                mgr.GetEventRecorderFor("artifact-controller"),
		FieldOwner:              "artifact-controller",
		MaxConcurrentReconciles: concurrent,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "setup reconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "add healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "add readyz")
		os.Exit(1)
	}

	setupLog.Info("starting artifact-controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

// controllerNamespace is where the http driver looks for credential secrets.
// Confining it to the controller's own namespace means a class — which is
// cluster-scoped, and so writable by anyone with cluster-level access — cannot
// name a secret in someone else's namespace and have it read out.
func controllerNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "artifact-system"
}
