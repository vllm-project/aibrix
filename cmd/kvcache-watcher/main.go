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

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const DefaultKVCacheServerPort = 9600
const KVCacheLabelKeyIdentifier = "kvcache.orchestration.aibrix.ai/name"
const KVCacheLabelKeyRole = "kvcache.orchestration.aibrix.ai/role"
const KVCacheLabelValueRoleCache = "cache"
const KVCacheLabelValueRoleMetadata = "metadata"
const KVCacheLabelValueRoleKVWatcher = "kvwatcher"

type NodeInfo struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

type ClusterNodes struct {
	Nodes   []NodeInfo `json:"nodes"`
	Version int64      `json:"version"`
}

func main() {
	ctx := context.Background()

	// read environment variables from env
	namespace := os.Getenv("WATCH_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	kvClusterId := os.Getenv("WATCH_KVCACHE_CLUSTER") // e.g., "kvcache.aibrix.ai=llama4"

	redisAddr := os.Getenv("REDIS_ADDR")
	redisPass := os.Getenv("REDIS_PASSWORD")
	// Database to be selected after connecting to the server.
	redisDatabaseStr := os.Getenv("REDIS_DATABASE")
	redisDatabase, err := strconv.Atoi(redisDatabaseStr)
	if err != nil {
		klog.Warningf("Conversion error: %v", err)
		// Use default database
		redisDatabase = 0
	}

	// create Kubernetes client
	var config *rest.Config
	var kubeConfig string
	kFlag := flag.Lookup("kubeconfig")
	if kFlag != nil {
		kubeConfig = kFlag.Value.String()
	} else {
		klog.Warning("kubeconfig flag not defined")
	}

	if kubeConfig == "" {
		klog.Info("using in-cluster configuration")
		config, err = rest.InClusterConfig()
	} else {
		klog.Infof("using configuration from '%s'", kubeConfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	}
	if err != nil {
		klog.Fatalf("Failed to read kube configs: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create clientset: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPass,
		DB:       redisDatabase,
	})

	// Create informer factory
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 15*time.Second,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			if kvClusterId != "" {
				kvClusterLabel := fmt.Sprintf("%s=%s", KVCacheLabelKeyIdentifier, kvClusterId)
				kvClusterRoleLabel := fmt.Sprintf("%s=%s", KVCacheLabelKeyRole, KVCacheLabelValueRoleCache)
				opts.LabelSelector = fmt.Sprintf("%s,%s", kvClusterLabel, kvClusterRoleLabel)
				klog.Infof(opts.LabelSelector)
			}
		}),
	)

	podInformer := factory.Core().V1().Pods().Informer()

	_, err = podInformer.AddEventHandler(&cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { syncPods(ctx, rdb, podInformer, kvClusterId) },
		UpdateFunc: func(oldObj, newObj interface{}) { syncPods(ctx, rdb, podInformer, kvClusterId) },
		DeleteFunc: func(obj interface{}) { syncPods(ctx, rdb, podInformer, kvClusterId) },
	})
	if err != nil {
		return
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	klog.Info("Starting pod registration watcher...")
	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

	<-stopCh
}

func syncPods(ctx context.Context, rdb *redis.Client, informer cache.SharedIndexInformer, selector string) {
	pods := informer.GetStore().List()

	var nodeInfos []NodeInfo
	for _, obj := range pods {
		pod := obj.(*corev1.Pod)
		if _, ok := pod.Labels[KVCacheLabelKeyIdentifier]; !ok {
			continue
		}

		if value, ok := pod.Labels[KVCacheLabelKeyRole]; !ok || value != KVCacheLabelValueRoleCache {
			continue
		}

		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}

		ip := pod.Status.PodIP
		if ip == "" {
			continue
		}
		nodeInfos = append(nodeInfos, NodeInfo{
			Name: pod.Name,
			Addr: ip,
			Port: DefaultKVCacheServerPort,
		})
	}

	// get existing nodes
	val, err := rdb.Get(ctx, "kvcache_nodes").Result()
	// TODO: debug, whether it's correct or not.
	klog.Infof("redis get result: %v", val)
	existing := ClusterNodes{}
	if err == nil {
		_ = json.Unmarshal([]byte(val), &existing)
	}

	// only change if there's node update
	if isNodeListEqual(existing.Nodes, nodeInfos) {
		klog.Infof("No changes to node list, skipping update")
		return
	}

	newData := ClusterNodes{
		Nodes:   nodeInfos,
		Version: existing.Version + 1,
	}
	encoded, _ := json.Marshal(newData)
	klog.Infof("nodes: %v, encoded %v", nodeInfos, encoded)
	err = rdb.Set(ctx, "kvcache_nodes", encoded, 0).Err()
	if err != nil {
		klog.Errorf("Failed to write Redis: %v", err)
	}
	klog.Infof("Updated Redis node info with %d nodes", len(nodeInfos))
}

func isNodeListEqual(a, b []NodeInfo) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]NodeInfo{}
	for _, n := range a {
		m[n.Name] = n
	}
	for _, n := range b {
		if m[n.Name] != n {
			return false
		}
	}
	return true
}
