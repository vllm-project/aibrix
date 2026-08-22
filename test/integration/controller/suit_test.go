/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"

	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	// Import internal APIs
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	// Import local APIs
	autoscalingapi "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orcheapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"

	// Import external dependencies APIs
	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/controller"
	"github.com/vllm-project/aibrix/pkg/features"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	cfg           *rest.Config         // Config for connecting to the test control plane (API server)
	k8sClient     client.Client        // Client for interacting with Kubernetes resources
	testEnv       *envtest.Environment // Test environment that spins up etcd and kube-apiserver
	ctx           context.Context      // Context to control the lifecycle of the controller manager
	cancel        context.CancelFunc   // Function to cancel the context and stop the manager
	stopCh        chan struct{}        // Channel to signal shutdown of custom caches (e.g., informers)
	dynamicClient dynamic.Interface
)

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	// Initialize klog flags and set verbosity to 0 (suppress all info logs)
	klog.InitFlags(nil)
	_ = flag.Set("v", "0")
	_ = flag.Set("logtostderr", "false")
	_ = flag.Set("alsologtostderr", "false")

	// Configure controller-runtime logger to only show errors
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(false)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")

	testEnv = &envtest.Environment{
		// Paths to CRD YAML files required for the tests
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),                           // Aibrix CRDs
			filepath.Join("..", "..", "..", "config", "dependency", "kuberay-operator", "crds"), // KubeRay CRDs (dependency)
			filepath.Join("..", "..", "..", "config", "dependency", "podgroup", "crds"),
		},

		ErrorIfCRDPathMissing: true,
		// The BinaryAssetsDirectory is only required if you want to run the tests directly
		// without call the makefile target test. If not informed it will look for the
		// default path defined in controller-runtime which is /usr/local/kubebuilder/.
		// Note that you must have the required binaries setup under the bin directory to perform
		// the tests directly. When we run make test it will be setup and used automatically.
		BinaryAssetsDirectory: filepath.Join("..", "..", "..", "bin", "k8s",
			fmt.Sprintf("1.30.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}

	var err error
	// Start the control plane (etcd + kube-apiserver)
	By("starting the control plane")
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred(), "Failed to start the control plane")
	Expect(cfg).NotTo(BeNil(), "Kubeconfig should not be nil")

	// Create a new scheme and register all required API types
	By("registering API schemes")
	//+kubebuilder:scaffold:scheme
	Expect(modelapi.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(orcheapi.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(autoscalingapi.AddToScheme(scheme.Scheme)).To(Succeed())

	Expect(rayv1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(gatewayv1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(gatewayv1beta1.AddToScheme(scheme.Scheme)).To(Succeed())

	Expect(clientgoscheme.AddToScheme(scheme.Scheme)).To(Succeed()) // Core Kubernetes types
	Expect(appsv1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(autoscalingv2.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(discoveryv1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(corev1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(admissionv1.AddToScheme(scheme.Scheme)).To(Succeed())

	// Initialize the client using the test config and registered scheme
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred(), "Failed to create Kubernetes client")
	Expect(k8sClient).NotTo(BeNil(), "Kubernetes client should not be nil")

	dynamicClient = dynamic.NewForConfigOrDie(testEnv.Config)

	By("setting up controller manager")
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme.Scheme,
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred(), "Failed to create controller manager")

	// Initialize custom cache layer (e.g., informers) with a stop channel
	By("initializing custom cache layer")
	stopCh = make(chan struct{})
	cache.InitWithOptions(cfg, stopCh, cache.InitOptions{})

	// Initialize controller logic
	features.InitControllers("*")
	Expect(controller.Initialize(mgr)).To(Succeed())
	runtimeConfig := config.NewRuntimeConfig(false, false, "leastAdapters")
	Expect(controller.SetupWithManager(mgr, runtimeConfig)).To(Succeed())

	By("starting controller manager")
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()

})

var _ = AfterSuite(func() {
	By("tearing down the test environment")

	// Signal custom cache layer to stop
	if stopCh != nil {
		close(stopCh)
	}

	// Cancel the manager context to stop reconcilers
	if cancel != nil {
		cancel()
	}

	time.Sleep(2 * time.Second)

	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
