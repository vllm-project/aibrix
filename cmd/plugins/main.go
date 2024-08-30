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
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aibrix/aibrix/pkg/plugins/gateway"
	ratelimiter "github.com/aibrix/aibrix/pkg/plugins/gateway/rate_limiter"
	redis "github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
)

// Create Redis Client
var (
	grpc_port  int
	redis_host = getEnv("REDIS_HOST", "localhost")
	redis_port = getEnv("REDIS_PORT", "6379")
)

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete

func createClient(kubeconfigPath string) (kubernetes.Interface, error) {
	var kubeconfig *rest.Config

	if kubeconfigPath != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("unable to load kubeconfig from %s: %v", kubeconfigPath, err)
		}
		kubeconfig = config
	} else {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("unable to load in-cluster config: %v", err)
		}
		kubeconfig = config
	}

	client, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create a client: %v", err)
	}

	return client, nil
}

// TODO (varun): one or multi plugin ext_proc
func main() {
	kubeconfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	flag.IntVar(&grpc_port, "port", 50052, "gRPC port")
	flag.Parse()

	// Connect to Redis
	client := redis.NewClient(&redis.Options{
		Addr: redis_host + ":" + redis_port,
		DB:   0, // Default DB
	})
	pong, err := client.Ping(context.Background()).Result()
	if err != nil {
		log.Fatal("Error connecting to Redis:", err)
	}
	fmt.Println("Connected to Redis:", pong)

	// Connect to K8s cluster
	k8sClient, err := createClient(*kubeconfig)
	if err != nil {
		log.Fatal("Error creating kubernetes client:", err)
	}

	// grpc server init
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", grpc_port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()

	extProcPb.RegisterExternalProcessorServer(s, gateway.NewServer(
		ratelimiter.NewRedisAccountRateLimiter("aibrix", client, 1*time.Minute),
		k8sClient,
	))
	healthPb.RegisterHealthServer(s, &gateway.HealthServer{})

	log.Println("Starting gRPC server on port :50052")

	// shutdown
	var gracefulStop = make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGTERM)
	signal.Notify(gracefulStop, syscall.SIGINT)
	go func() {
		sig := <-gracefulStop
		log.Printf("caught sig: %+v", sig)
		log.Println("Wait for 1 second to finish processing")
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	if err := s.Serve(lis); err != nil {
		panic(err)
	}
}
