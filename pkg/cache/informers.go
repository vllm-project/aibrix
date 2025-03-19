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

	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	v1alpha1scheme "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/scheme"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	modelIdentifier = "model.aibrix.ai/name"
	nodeType        = "ray.io/node-type"
	nodeWorker      = "worker"
)

func initCacheInformers(instance *Store, config *rest.Config, stopCh <-chan struct{}) error {
	if err := v1alpha1scheme.AddToScheme(scheme.Scheme); err != nil {
		return err
	}

	k8sClientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	crdClientSet, err := v1alpha1.NewForConfig(config)
	if err != nil {
		return err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)
	crdFactory := crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)

	podInformer := factory.Core().V1().Pods().Informer()
	modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()

	defer runtime.HandleCrash()
	factory.Start(stopCh)
	crdFactory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced, modelInformer.HasSynced) {
		return errors.New("timed out waiting for caches to sync")
	}

	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    instance.addPod,
		UpdateFunc: instance.updatePod,
		DeleteFunc: instance.deletePod,
	}); err != nil {
		return err
	}

	if _, err = modelInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    instance.addModelAdapter,
		UpdateFunc: instance.updateModelAdapter,
		DeleteFunc: instance.deleteModelAdapter,
	}); err != nil {
		return err
	}

	return nil
}

func (c *Store) addPod(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pod := obj.(*v1.Pod)
	// only track pods with model deployments
	modelName, ok := pod.Labels[modelIdentifier]
	if !ok {
		return
	}
	// ignore worker pods
	nodeType, ok := pod.Labels[nodeType]
	if ok && nodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "name", pod.Name)
		return
	}

	c.Pods[pod.Name] = pod
	c.addPodAndModelMappingLocked(pod.Name, modelName)
	klog.V(4).Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
	c.metricsDebugInfo()
}

func (c *Store) updatePod(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	oldModelName, oldOk := oldPod.Labels[modelIdentifier]
	newModelName, newOk := newPod.Labels[modelIdentifier]

	if !oldOk && !newOk {
		return // No model information to track in either old or new pod
	}

	// Remove old mappings if present
	if oldOk {
		delete(c.Pods, oldPod.Name)
		c.deletePodAndModelMapping(oldPod.Name, oldModelName)
	}

	// ignore worker pods
	nodeType, ok := oldPod.Labels[nodeType]
	if ok && nodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "name", oldPod.Name)
		return
	}

	// ignore worker pods
	nodeType, ok = newPod.Labels[nodeType]
	if ok && nodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "name", newPod.Name)
		return
	}

	// Add new mappings if present
	if newOk {
		c.Pods[newPod.Name] = newPod
		c.addPodAndModelMappingLocked(newPod.Name, newModelName)
	}

	klog.V(4).Infof("POD UPDATED: %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
	c.metricsDebugInfo()
}

func (c *Store) deletePod(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pod := obj.(*v1.Pod)
	_, ok := pod.Labels[modelIdentifier]
	if !ok {
		return
	}

	// delete base model and associated lora models on this pod
	if models, ok := c.PodToModelMapping[pod.Name]; ok {
		for modelName := range models {
			c.deletePodAndModelMapping(pod.Name, modelName)
		}
	}
	delete(c.Pods, pod.Name)
	delete(c.PodMetrics, pod.Name)
	delete(c.PodModelMetrics, pod.Name)

	klog.V(4).Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
	c.metricsDebugInfo()
}

func (c *Store) addModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.addPodAndModelMappingLocked(pod, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
	c.metricsDebugInfo()
}

func (c *Store) updateModelAdapter(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldModel := oldObj.(*modelv1alpha1.ModelAdapter)
	newModel := newObj.(*modelv1alpha1.ModelAdapter)

	for _, pod := range oldModel.Status.Instances {
		c.deletePodAndModelMapping(pod, oldModel.Name)
	}

	for _, pod := range newModel.Status.Instances {
		c.addPodAndModelMappingLocked(pod, newModel.Name)
	}

	klog.V(4).Infof("MODELADAPTER UPDATED. %s/%s %s", oldModel.Namespace, oldModel.Name, newModel.Status.Phase)
	c.metricsDebugInfo()
}

func (c *Store) deleteModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.deletePodAndModelMapping(pod, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER DELETED: %s/%s", model.Namespace, model.Name)
	c.metricsDebugInfo()
}

func (c *Store) addPodAndModelMappingLocked(podName, modelName string) {
	pod, ok := c.Pods[podName]
	if !ok {
		klog.Errorf("pod %s does not exist in internal-cache", podName)
		return
	}

	models, ok := c.PodToModelMapping[podName]
	if !ok {
		c.PodToModelMapping[podName] = map[string]struct{}{
			modelName: {},
		}
	} else {
		models[modelName] = struct{}{}
		c.PodToModelMapping[podName] = models
	}

	pods, ok := c.ModelToPodMapping[modelName]
	if !ok {
		c.ModelToPodMapping[modelName] = map[string]*v1.Pod{
			podName: pod,
		}
	} else {
		pods[podName] = pod
		c.ModelToPodMapping[modelName] = pods
	}
}

func (c *Store) deletePodAndModelMapping(podName, modelName string) {
	if models, ok := c.PodToModelMapping[podName]; ok {
		delete(models, modelName)
		if len(models) != 0 {
			c.PodToModelMapping[podName] = models
		} else {
			delete(c.PodToModelMapping, podName)
		}
	}

	if pods, ok := c.ModelToPodMapping[modelName]; ok {
		delete(pods, podName)
		if len(pods) != 0 {
			c.ModelToPodMapping[modelName] = pods
		} else {
			delete(c.ModelToPodMapping, modelName)
		}
	}
}
