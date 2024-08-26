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

package cache

import (
	"fmt"
	"log"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"

	v1alpha1scheme "github.com/aibrix/aibrix/pkg/client/clientset/versioned/scheme"
	"k8s.io/client-go/kubernetes/scheme"
)

var once sync.Once

// type global
type Cache struct {
	mu   sync.RWMutex
	pods map[string]*v1.Pod
}

var (
	instance   Cache
	kubeconfig string
)

func NewCache(stopCh <-chan struct{}) *Cache {
	once.Do(func() {
		var config *rest.Config
		var err error

		if kubeconfig == "" {
			log.Printf("using in-cluster configuration")
			config, err = rest.InClusterConfig()
		} else {
			log.Printf("using configuration from '%s'", kubeconfig)
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		}

		if err != nil {
			panic(err)
		}

		v1alpha1scheme.AddToScheme(scheme.Scheme)

		k8sClientSet, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err)
		}

		factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 10*time.Second, informers.WithNamespace("default"))
		podInformer := factory.Core().V1().Pods().Informer()

		defer runtime.HandleCrash()
		go factory.Start(stopCh)

		if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced) {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
			return
		}

		// crdClientSet, err = v1alpha1.NewForConfig(config)
		// if err != nil {
		// 	panic(err)
		// }

		instance = Cache{
			pods: map[string]*v1.Pod{},
		}

		podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    instance.addPod,
			UpdateFunc: instance.updatePod,
			DeleteFunc: instance.deletePod,
		})
	})

	return &instance
}

func (c *Cache) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pods[pod.Name] = pod
	klog.Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
}

func (c *Cache) updatePod(oldObj interface{}, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)
	klog.Infof(
		"POD UPDATED. %s/%s %s",
		oldPod.Namespace, oldPod.Name, newPod.Status.Phase,
	)
}

func (c *Cache) deletePod(obj interface{}) {
	pod := obj.(*v1.Pod)
	klog.Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
}
