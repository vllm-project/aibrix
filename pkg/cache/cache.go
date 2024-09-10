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
	"strings"
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
	mu                       sync.RWMutex
	initialized              bool
	pods                     map[string]*v1.Pod
	modelAdapterToPodMapping map[string][]string
	podToModelAdapterMapping map[string]map[string]struct{}
}

var (
	instance   Cache
	kubeconfig string
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
		modeInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()

		defer runtime.HandleCrash()
		factory.Start(stopCh)
		crdFactory.Start(stopCh)

		// factory.WaitForCacheSync(stopCh)
		// crdFactory.WaitForCacheSync(stopCh)

		if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced, modeInformer.HasSynced) {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
			return
		}

		instance = Cache{
			initialized:              true,
			pods:                     map[string]*v1.Pod{},
			modelAdapterToPodMapping: map[string][]string{},
			podToModelAdapterMapping: map[string]map[string]struct{}{},
		}

		if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    instance.addPod,
			UpdateFunc: instance.updatePod,
			DeleteFunc: instance.deletePod,
		}); err != nil {
			panic(err)
		}

		if _, err = modeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    instance.addModel,
			UpdateFunc: instance.updateModel,
			DeleteFunc: instance.deleteModel,
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
	c.pods[pod.Name] = pod
	c.podToModelAdapterMapping[pod.Name] = map[string]struct{}{}
	klog.Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
}

func (c *Cache) updatePod(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)
	klog.Infof("POD UPDATED. %s/%s %s", oldPod.Namespace, oldPod.Name, newPod.Status.Phase)
}

func (c *Cache) deletePod(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pod := obj.(*v1.Pod)
	delete(c.pods, pod.Name)
	klog.Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
}

func (c *Cache) addModel(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	c.modelAdapterToPodMapping[model.Name] = model.Status.Instances
	c.addModelAdapterMapping(model)

	klog.Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
}

func (c *Cache) updateModel(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldModel := oldObj.(*modelv1alpha1.ModelAdapter)
	newModel := newObj.(*modelv1alpha1.ModelAdapter)
	c.modelAdapterToPodMapping[newModel.Name] = newModel.Status.Instances
	c.deleteModelAdapterMapping(oldModel)
	c.addModelAdapterMapping(newModel)

	klog.Infof("MODELADAPTER UPDATED. %s/%s %s", oldModel.Namespace, oldModel.Name, newModel.Status.Phase)
}

func (c *Cache) deleteModel(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	delete(c.modelAdapterToPodMapping, model.Name)
	c.deleteModelAdapterMapping(model)

	klog.Infof("MODELADAPTER DELETED: %s/%s", model.Namespace, model.Name)
}

func (c *Cache) addModelAdapterMapping(model *modelv1alpha1.ModelAdapter) {
	for _, pod := range model.Status.Instances {
		models, ok := c.podToModelAdapterMapping[pod]
		if !ok {
			c.podToModelAdapterMapping[pod] = map[string]struct{}{
				model.Name: {},
			}
			continue
		}

		models[model.Name] = struct{}{}
		c.podToModelAdapterMapping[pod] = models
	}
}

func (c *Cache) deleteModelAdapterMapping(model *modelv1alpha1.ModelAdapter) {
	for _, pod := range model.Status.Instances {
		modelAdapters := c.podToModelAdapterMapping[pod]
		delete(modelAdapters, model.Name)
		c.podToModelAdapterMapping[pod] = modelAdapters
	}
}

func (c *Cache) debugInfo() {
	for model, instances := range c.modelAdapterToPodMapping {
		klog.Infof("modelName: %s, instances: %v", model, instances)
	}

	for pod, models := range c.podToModelAdapterMapping {
		if !strings.HasPrefix(pod, "llama") {
			continue
		}

		modelsArr := []string{}
		for m := range models {
			modelsArr = append(modelsArr, m)
		}

		klog.Infof("podName: %s, modelAdapters: %v", pod, modelsArr)
	}
}

func (c *Cache) GetPods() map[string]*v1.Pod {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.pods
}

func (c *Cache) GetPodToModelAdapterMapping() map[string]map[string]struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.podToModelAdapterMapping
}
