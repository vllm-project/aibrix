package main

import (
	"flag"
	"io/fs"
	"log"
	"os"

	"github.com/gin-gonic/gin"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	portal "github.com/vllm-project/aibrix/apps/portal/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func main() {
	var addr string
	var kubeconfig string
	var mock bool

	flag.StringVar(&addr, "addr", ":8080", "listen address for the portal server")
	flag.BoolVar(&mock, "mock", false, "run with fake in-memory K8s client and sample data (no cluster needed)")
	flag.Parse()

	// Use remaining arg or KUBECONFIG env as kubeconfig path
	if args := flag.Args(); len(args) > 0 {
		kubeconfig = args[0]
	}
	if kubeconfig == "" {
		if v, ok := os.LookupEnv("KUBECONFIG"); ok {
			kubeconfig = v
		}
	}

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

	var c client.Client
	if mock {
		c = buildMockClient(scheme)
		log.Println("running in mock mode with sample data")
	} else {
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

		c, err = client.New(restConfig, client.Options{Scheme: scheme})
		if err != nil {
			log.Fatalf("failed to create kubernetes client: %v", err)
		}
	}

	gin.SetMode(gin.ReleaseMode)

	// Embed frontend static files
	if distFS, err := fs.Sub(embeddedDist, "dist"); err == nil {
		portal.StaticFS = distFS
		log.Println("serving embedded frontend")
	}

	log.Printf("starting portal server on %s", addr)
	if err := portal.NewRouter(c).Run(addr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func buildMockClient(scheme *runtime.Scheme) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(
			&modelv1alpha1.ModelAdapter{
				ObjectMeta: metav1.ObjectMeta{Name: "llama-lora", Namespace: "default"},
				Spec: modelv1alpha1.ModelAdapterSpec{
					BaseModel:   ptr.To("meta-llama/Llama-2-7b-hf"),
					ArtifactURL: "s3://models/llama-lora-weights",
					Replicas:    ptr.To(int32(2)),
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "llama-serve"}},
				},
				Status: modelv1alpha1.ModelAdapterStatus{
					Phase:           modelv1alpha1.ModelAdapterRunning,
					ReadyReplicas:   2,
					DesiredReplicas: 2,
					Instances:       []string{"llama-serve-pod-1", "llama-serve-pod-2"},
				},
			},
			&modelv1alpha1.ModelAdapter{
				ObjectMeta: metav1.ObjectMeta{Name: "qwen-lora", Namespace: "default"},
				Spec: modelv1alpha1.ModelAdapterSpec{
					BaseModel:   ptr.To("Qwen/Qwen-7B"),
					ArtifactURL: "s3://models/qwen-lora-weights",
					Replicas:    ptr.To(int32(1)),
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "qwen-serve"}},
				},
				Status: modelv1alpha1.ModelAdapterStatus{
					Phase:         modelv1alpha1.ModelAdapterPending,
					ReadyReplicas: 0,
				},
			},
			&orchestrationv1alpha1.RayClusterFleet{
				ObjectMeta: metav1.ObjectMeta{Name: "ray-fleet-1", Namespace: "default"},
				Spec: orchestrationv1alpha1.RayClusterFleetSpec{
					Replicas: ptr.To(int32(3)),
				},
				Status: orchestrationv1alpha1.RayClusterFleetStatus{
					Replicas:          3,
					ReadyReplicas:     2,
					AvailableReplicas: 2,
				},
			},
			&orchestrationv1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{Name: "kvcache-1", Namespace: "default"},
				Spec:       orchestrationv1alpha1.KVCacheSpec{Mode: "distributed"},
				Status:     orchestrationv1alpha1.KVCacheStatus{ReadyReplicas: 3},
			},
			&orchestrationv1alpha1.PodSet{
				ObjectMeta: metav1.ObjectMeta{Name: "podset-1", Namespace: "default"},
				Spec:       orchestrationv1alpha1.PodSetSpec{PodGroupSize: 4},
				Status: orchestrationv1alpha1.PodSetStatus{
					ReadyPods: 4,
					TotalPods: 4,
					Phase:     orchestrationv1alpha1.PodSetPhaseReady,
				},
			},
		).
		Build()
}
