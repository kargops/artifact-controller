package controller

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/generator"
	"github.com/kargops/artifact-controller/internal/store"
	"github.com/kargops/artifact-controller/internal/store/fake"
)

var (
	testEnv   *envtest.Environment
	k8sClient client.Client // direct (uncached) client for assertions
	fakeStore *fake.Store
	testCtx   context.Context
)

const testNS = "artifact-tests"

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "suite:", err)
		os.Exit(1)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("SKIP internal/controller: KUBEBUILDER_ASSETS not set; run 'make test' to download envtest binaries")
		os.Exit(0)
	}
	logf.SetLogger(zap.New(zap.WriteTo(io.Discard)))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	die(err)

	scheme := runtime.NewScheme()
	die(clientgoscheme.AddToScheme(scheme))
	die(artifactsv1.AddToScheme(scheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	die(err)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	die(err)

	fakeStore = fake.New()
	reg := store.NewRegistry()
	fake.Register(reg, fakeStore)
	eval, err := generator.NewEvaluator()
	die(err)

	rec := &ArtifactReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Registry:   reg,
		Eval:       eval,
		Recorder:   mgr.GetEventRecorderFor("artifact-controller"),
		FieldOwner: "artifact-controller",
	}
	die(rec.SetupWithManager(mgr))

	var cancel context.CancelFunc
	testCtx, cancel = context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(testCtx); err != nil {
			fmt.Fprintln(os.Stderr, "manager:", err)
			os.Exit(1)
		}
	}()

	die(k8sClient.Create(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}))

	gomega.SetDefaultEventuallyTimeout(45 * time.Second)
	gomega.SetDefaultEventuallyPollingInterval(150 * time.Millisecond)

	code := m.Run()
	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}
