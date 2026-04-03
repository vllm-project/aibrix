package main

import (
	"flag"
	"log"

	"github.com/gin-gonic/gin"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	portal "github.com/vllm-project/aibrix/pkg/portal"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	var addr string
	var kubeconfig string

	flag.StringVar(&addr, "addr", ":8080", "listen address for the portal server")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig file (uses in-cluster config if empty)")
	flag.Parse()

	scheme := runtime.NewScheme()
	if err := modelv1alpha1.AddToScheme(scheme); err != nil {
		log.Fatalf("failed to add model scheme: %v", err)
	}
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		log.Fatalf("failed to add orchestration scheme: %v", err)
	}
	if err := autoscalingv1alpha1.AddToScheme(scheme); err != nil {
		log.Fatalf("failed to add autoscaling scheme: %v", err)
	}

	var restConfig *rest.Config
	var err error
	if kubeconfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		log.Fatalf("failed to build kubernetes config: %v", err)
	}

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("failed to create kubernetes client: %v", err)
	}

	gin.SetMode(gin.ReleaseMode)

	log.Printf("starting portal server on %s", addr)
	if err := portal.NewRouter(c).Run(addr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
