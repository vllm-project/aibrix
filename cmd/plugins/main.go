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
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/aibrix/aibrix/pkg/plugins/gateway"
	"github.com/aibrix/aibrix/pkg/utils"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	grpc_port int
)

func main() {
	flag.IntVar(&grpc_port, "port", 50052, "gRPC port")
	flag.Parse()

	// Connect to Redis
	redisClient := utils.GetRedisClient()

	fmt.Println("Starting cache")
	stopCh := make(chan struct{})
	defer close(stopCh)
	var config *rest.Config
	var err error

	// ref: https://github.com/kubernetes-sigs/controller-runtime/issues/878#issuecomment-1002204308
	kubeConfig := flag.Lookup("kubeconfig").Value.String()
	if kubeConfig == "" {
		log.Printf("using in-cluster configuration")
		config, err = rest.InClusterConfig()
	} else {
		log.Printf("using configuration from '%s'", kubeConfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	}

	if err != nil {
		panic(err)
	}

	cache.NewCache(config, stopCh)

	// Connect to K8s cluster
	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("Error creating kubernetes client:", err)
	}

	// grpc server init
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", grpc_port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()

	extProcPb.RegisterExternalProcessorServer(s, gateway.NewServer(redisClient, k8sClient))
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
