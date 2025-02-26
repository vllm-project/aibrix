/*
Copyright 2024 The Aibrix Team.

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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"time"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/features"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cert"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/controller"
	apiwebhook "github.com/vllm-project/aibrix/pkg/webhook"
	//+kubebuilder:scaffold:imports
)

const (
	defaultLeaseDuration              = 15 * time.Second
	defaultRenewDeadline              = 10 * time.Second
	defaultRetryPeriod                = 2 * time.Second
	defaultControllerCacheSyncTimeout = 2 * time.Minute
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	//restConfigQPS   = flag.Int("rest-config-qps", 30, "QPS of rest config.")
	//restConfigBurst = flag.Int("rest-config-burst", 50, "Burst of rest config.")
)

func init() {
	// Only register the base kubernetes scheme here
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	scheme.AddUnversionedTypes(metav1.SchemeGroupVersion, &metav1.UpdateOptions{}, &metav1.DeleteOptions{}, &metav1.CreateOptions{})
	//+kubebuilder:scaffold:scheme
}

func RegisterSchemas(scheme *runtime.Scheme) error {

	if features.IsControllerEnabled(features.PodAutoscalerController) {
		utilruntime.Must(autoscalingv1alpha1.AddToScheme(scheme))
	}

	if features.IsControllerEnabled(features.ModelAdapterController) {
		utilruntime.Must(modelv1alpha1.AddToScheme(scheme))
	}

	if features.IsControllerEnabled(features.DistributedInferenceController) {
		utilruntime.Must(orchestrationv1alpha1.AddToScheme(scheme))
		utilruntime.Must(rayclusterv1.AddToScheme(scheme))
	}

	scheme.AddUnversionedTypes(metav1.SchemeGroupVersion, &metav1.UpdateOptions{}, &metav1.DeleteOptions{}, &metav1.CreateOptions{})
	//+kubebuilder:scaffold:scheme

	return nil
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var leaderElectionNamespace string
	var leaseDuration time.Duration
	var renewDeadLine time.Duration
	var leaderElectionResourceLock string
	var leaderElectionId string
	var controllers string
	var enableRuntimeSidecar bool
	var debugMode bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false, "Whether you need to enable leader election.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "aibrix-system",
		"This determines the namespace in which the leader election configmap will be created, it will use in-cluster namespace if empty.")
	flag.DurationVar(&leaseDuration, "leader-election-lease-duration", defaultLeaseDuration,
		"leader-election-lease-duration is the duration that non-leader candidates will wait to force acquire leadership. This is measured against time of last observed ack. Default is 15 seconds.")
	flag.DurationVar(&renewDeadLine, "leader-election-renew-deadline", defaultRenewDeadline,
		"leader-election-renew-deadline is the duration that the acting controlplane will retry refreshing leadership before giving up. Default is 10 seconds.")
	flag.StringVar(&leaderElectionResourceLock, "leader-election-resource-lock", resourcelock.LeasesResourceLock,
		"leader-election-resource-lock determines which resource lock to use for leader election, defaults to \"leases\".")
	flag.StringVar(&leaderElectionId, "leader-election-id", "aibrix-controller-manager",
		"leader-election-id determines the name of the resource that leader election will use for holding the leader lock, Default is aibrix-controller-manager.")
	flag.StringVar(&controllers, "controllers", "*", "Comma-separated list of controllers to enable or disable, default value is * which indicates all controllers should be started.")
	flag.BoolVar(&enableRuntimeSidecar, "enable-runtime-sidecar", false,
		"If set, Runtime management API will be enabled for the metrics, model adapter and model downloading interactions, control plane will not talk to engine directly anymore")
	flag.BoolVar(&debugMode, "debug-mode", false,
		"If set, control plane will talk to localhost nodePort for testing purpose")

	// Initialize the klog
	klog.InitFlags(flag.CommandLine)
	defer klog.Flush()
	flag.Parse()

	// TODO: we will switch to textlogger or zap later
	ctrl.SetLogger(klogr.New()) // nolint:staticcheck

	// initialize the controllers
	if err := features.ValidateControllers(controllers); err != nil {
		setupLog.Error(err, "unable to validate the controllers, please type the right controller names through --controllers")
		os.Exit(1)
	}

	features.InitControllers(controllers)

	if err := RegisterSchemas(scheme); err != nil {
		setupLog.Error(err, "unable to register schemas")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	runtimeConfig := config.NewRuntimeConfig(enableRuntimeSidecar, debugMode)

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		},
		WebhookServer:              webhookServer,
		HealthProbeBindAddress:     probeAddr,
		LeaderElection:             enableLeaderElection,
		LeaderElectionID:           leaderElectionId,
		LeaderElectionNamespace:    leaderElectionNamespace,
		LeaderElectionResourceLock: leaderElectionResourceLock,
		LeaseDuration:              &leaseDuration,
		RenewDeadline:              &renewDeadLine,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	setupLog.Info("starting cache")
	stopCh := make(chan struct{})
	defer close(stopCh)
	var config *rest.Config

	// ref: https://github.com/kubernetes-sigs/controller-runtime/issues/878#issuecomment-1002204308
	kubeConfig := flag.Lookup("kubeconfig").Value.String()
	if kubeConfig == "" {
		setupLog.Info("using in-cluster configuration")
		config, err = rest.InClusterConfig()
	} else {
		setupLog.Info(fmt.Sprintf("using configuration from '%s'", kubeConfig))
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	}

	if err != nil {
		panic(err)
	}

	if features.IsControllerEnabled(features.ModelAdapterController) {
		// cache is enabled for model adapter scheduling.
		cache.NewCache(config, stopCh, nil)
	}

	certsReady := make(chan struct{})

	if err = cert.CertsManager(mgr, leaderElectionNamespace, certsReady); err != nil {
		setupLog.Error(err, "unable to setup cert rotation")
		os.Exit(1)
	}

	// Initialize controllers
	controller.Initialize()

	// Cert won't be ready until manager starts, so start a goroutine here which
	// will block until the cert is ready before setting up the controllers.
	// Controllers who register after manager starts will start directly.
	go setupControllers(mgr, runtimeConfig, certsReady)

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func setupControllers(mgr ctrl.Manager, runtimeConfig config.RuntimeConfig, certsReady chan struct{}) {
	// The controllers won't work until the webhooks are operating,
	// and the webhook won't work until the certs are all in places.
	setupLog.Info("waiting for the cert generation to complete")
	<-certsReady
	setupLog.Info("certs ready")

	// Kind controller registration is encapsulated inside the pkg/controller/controller.go
	// So here we can use more clean registration flow and there's no need to change logics in future.
	if err := controller.SetupWithManager(mgr, runtimeConfig); err != nil {
		setupLog.Error(err, "unable to setup controller")
		os.Exit(1)
	}

	if err := apiwebhook.SetupBackendRuntimeWebhook(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Model")
		os.Exit(1)
	}
}
