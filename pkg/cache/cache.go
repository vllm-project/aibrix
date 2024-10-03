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
	"errors"
	"fmt"
	"sync"

	crdinformers "github.com/aibrix/aibrix/pkg/client/informers/externalversions"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/aibrix/aibrix/pkg/client/clientset/versioned"
	v1alpha1scheme "github.com/aibrix/aibrix/pkg/client/clientset/versioned/scheme"
	"k8s.io/client-go/kubernetes/scheme"
)

var once sync.Once

// type global
type Cache struct {
	mu                sync.RWMutex
	initialized       bool
	pods              map[string]*v1.Pod
	podToModelMapping map[string]map[string]struct{} // pod_name: map[model_name]struct{}
	modelToPodMapping map[string]map[string]*v1.Pod  // model_name: map[pod_name]*v1.Pod
	podRequestTracker map[string]int
}

var (
	instance Cache
)

const (
	modelIdentifier = "model.aibrix.ai/name"
)

func GetCache() (*Cache, error) {
	if !instance.initialized {
		return nil, errors.New("cache is not initialized")
	}
	return &instance, nil
}

func NewCache(config *rest.Config, stopCh <-chan struct{}) *Cache {
	once.Do(func() {

		if err := v1alpha1scheme.AddToScheme(scheme.Scheme); err != nil {
			panic(err)
		}

		k8sClientSet, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err)
		}

		crdClientSet, err := v1alpha1.NewForConfig(config)
		if err != nil {
			panic(err)
		}

		factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)
		crdFactory := crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)

		podInformer := factory.Core().V1().Pods().Informer()
		modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()

		defer runtime.HandleCrash()
		factory.Start(stopCh)
		crdFactory.Start(stopCh)

		// factory.WaitForCacheSync(stopCh)
		// crdFactory.WaitForCacheSync(stopCh)

		if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced, modelInformer.HasSynced) {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
			return
		}

		instance = Cache{
			initialized:       true,
			pods:              map[string]*v1.Pod{},
			podToModelMapping: map[string]map[string]struct{}{},
			modelToPodMapping: map[string]map[string]*v1.Pod{},
			podRequestTracker: map[string]int{},
		}

		if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    instance.addPod,
			UpdateFunc: instance.updatePod,
			DeleteFunc: instance.deletePod,
		}); err != nil {
			panic(err)
		}

		if _, err = modelInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    instance.addModelAdapter,
			UpdateFunc: instance.updateModelAdapter,
			DeleteFunc: instance.deleteModelAdapter,
		}); err != nil {
			panic(err)
		}
	})

	return &instance
}

func (c *Cache) addPod(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pod := obj.(*v1.Pod)
	// only track pods with model deployments
	modelName, ok := pod.Labels[modelIdentifier]
	if !ok {
		return
	}

	c.pods[pod.Name] = pod
	c.addPodAndModelMapping(pod.Name, modelName)
	klog.V(4).Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
	c.debugInfo()
}

func (c *Cache) updatePod(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldPod := oldObj.(*v1.Pod)
	oldModelName, ok := oldPod.Labels[modelIdentifier]
	if !ok {
		return
	}

	newPod := newObj.(*v1.Pod)
	newModelName, ok := oldPod.Labels[modelIdentifier]
	if !ok {
		return
	}

	delete(c.pods, oldPod.Name)
	c.pods[newPod.Name] = newPod
	c.deletePodAndModelMapping(oldPod.Name, oldModelName)
	c.addPodAndModelMapping(newPod.Name, newModelName)
	klog.V(4).Infof("POD UPDATED. %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
	c.debugInfo()
}

func (c *Cache) deletePod(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pod := obj.(*v1.Pod)
	_, ok := pod.Labels[modelIdentifier]
	if !ok {
		return
	}

	// delete base model and associated lora models on this pod
	if models, ok := c.podToModelMapping[pod.Name]; ok {
		for modelName := range models {
			c.deletePodAndModelMapping(pod.Name, modelName)
		}
	}
	delete(c.podToModelMapping, pod.Name)
	delete(c.pods, pod.Name)

	klog.V(4).Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
	c.debugInfo()
}

func (c *Cache) addModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.addPodAndModelMapping(pod, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
	c.debugInfo()
}

func (c *Cache) updateModelAdapter(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldModel := oldObj.(*modelv1alpha1.ModelAdapter)
	newModel := newObj.(*modelv1alpha1.ModelAdapter)

	for _, pod := range oldModel.Status.Instances {
		c.deletePodAndModelMapping(pod, oldModel.Name)
	}

	for _, pod := range newModel.Status.Instances {
		c.addPodAndModelMapping(pod, newModel.Name)
	}

	klog.V(4).Infof("MODELADAPTER UPDATED. %s/%s %s", oldModel.Namespace, oldModel.Name, newModel.Status.Phase)
	c.debugInfo()
}

func (c *Cache) deleteModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.deletePodAndModelMapping(pod, model.Name)
	}
	delete(c.modelToPodMapping, model.Name)

	klog.V(4).Infof("MODELADAPTER DELETED: %s/%s", model.Namespace, model.Name)
	c.debugInfo()
}

func (c *Cache) addPodAndModelMapping(podName, modelName string) {
	pod, ok := c.pods[podName]
	if !ok {
		klog.Errorf("pod %s does not exist in internal-cache", podName)
		return
	}

	models, ok := c.podToModelMapping[podName]
	if !ok {
		c.podToModelMapping[podName] = map[string]struct{}{
			modelName: {},
		}
	} else {
		models[modelName] = struct{}{}
		c.podToModelMapping[podName] = models
	}

	pods, ok := c.modelToPodMapping[modelName]
	if !ok {
		c.modelToPodMapping[modelName] = map[string]*v1.Pod{
			podName: pod,
		}
	} else {
		pods[podName] = pod
		c.modelToPodMapping[modelName] = pods
	}
}

func (c *Cache) deletePodAndModelMapping(podName, modelName string) {
	if models, ok := c.podToModelMapping[podName]; ok {
		delete(models, modelName)
		c.podToModelMapping[podName] = models
	}

	if pods, ok := c.modelToPodMapping[modelName]; ok {
		delete(pods, podName)
		c.modelToPodMapping[modelName] = pods
	}
}

func (c *Cache) debugInfo() {
	for _, pod := range c.pods {
		klog.V(4).Infof("pod: %s, podIP: %v", pod.Name, pod.Status.PodIP)
	}
	for podName, models := range c.podToModelMapping {
		var modelList string
		for modelName := range models {
			modelList += modelName + " "
		}
		klog.V(4).Infof("pod: %s, models: %s", podName, modelList)
	}
	for modelName, pods := range c.modelToPodMapping {
		var podList string
		for podName := range pods {
			podList += podName + " "
		}
		klog.V(4).Infof("model: %s, pods: %s", modelName, podList)
	}
}

func (c *Cache) GetPod(podName string) (*v1.Pod, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	pod, ok := c.pods[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return pod, nil
}

func (c *Cache) GetPods() map[string]*v1.Pod {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.pods
}

func (c *Cache) GetPodsForModel(modelName string) (map[string]*v1.Pod, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	podsMap, ok := c.modelToPodMapping[modelName]
	if !ok {
		return nil, fmt.Errorf("model does not exist in the cache: %s", modelName)
	}

	return podsMap, nil
}

func (c *Cache) GetModelsForPod(podName string) (map[string]struct{}, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	models, ok := c.podToModelMapping[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return models, nil
}

func (c *Cache) IncrPodRequestCount(podName string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.podRequestTracker[podName] += 1
	return c.podRequestTracker[podName]
}

func (c *Cache) DecrPodRequestCount(podName string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.podRequestTracker[podName] -= 1
	return c.podRequestTracker[podName]
}

func (c *Cache) GetPodRequestCount() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.podRequestTracker
}
