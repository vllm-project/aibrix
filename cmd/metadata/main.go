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

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metadata"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

func main() {
	redisClient := utils.GetRedisClient()
	defer func() {
		if err := redisClient.Close(); err != nil {
			klog.Warningf("Error closing Redis client: %v", err)
		}
	}()

	klog.Info("starting cache")
	stopCh := make(chan struct{})
	defer close(stopCh)
	var config *rest.Config
	var err error

	// ref: https://github.com/kubernetes-sigs/controller-runtime/issues/878#issuecomment-1002204308
	kubeConfig := flag.Lookup("kubeconfig").Value.String()
	if kubeConfig == "" {
		klog.Info("using in-cluster configuration")
		config, err = rest.InClusterConfig()
	} else {
		klog.Infof("using configuration from '%s'", kubeConfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	}

	if err != nil {
		panic(err)
	}

	cache.InitWithOptions(config, stopCh, cache.InitOptions{
		RedisClient: redisClient,
		// EnableKVSync defaults to false
		// ModelRouterProvider defaults to nil
	})

	klog.Info("Starting listening on port 8090")
	srv := metadata.NewHTTPServer(":8090", redisClient)
	klog.Fatal(srv.ListenAndServe())
}
