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

	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
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
	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    instance.addPod,
		UpdateFunc: instance.updatePod,
		DeleteFunc: instance.deletePod,
	}); err != nil {
		return err
	}

	modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()
	if _, err = modelInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    instance.addModelAdapter,
		UpdateFunc: instance.updateModelAdapter,
		DeleteFunc: instance.deleteModelAdapter,
	}); err != nil {
		return err
	}

	factory.Start(stopCh)
	crdFactory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced, modelInformer.HasSynced) {
		return errors.New("timed out waiting for caches to sync")
	}
	return nil
}

func (c *Store) addPod(obj interface{}) {
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

	c.mu.Lock()
	defer c.mu.Unlock()

	metaPod := c.addPodLocked(pod)
	c.addPodAndModelMappingLocked(metaPod, modelName)

	klog.V(4).Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
	c.debugInfo()
}

func (c *Store) updatePod(oldObj interface{}, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	_, oldOk := oldPod.Labels[modelIdentifier]
	key := fmt.Sprintf("%s/%s", oldPod.Namespace, oldPod.Name)
	_, existed := c.metaPods.Load(key) // Make sure nothing left.
	newModelName, newOk := newPod.Labels[modelIdentifier]

	if !oldOk && !existed && !newOk {
		return // No model information to track in either old or new pod
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// TODO: No in place update is handled here

	// Remove old mappings if present, no adapter will be inherited. (Adapters will be rescaned and readded later)
	if oldOk || existed {
		odlMetaPod := c.deletePodLocked(oldPod.Name, oldPod.Namespace)
		if odlMetaPod != nil {
			for _, modelName := range odlMetaPod.Models.Array() {
				c.deletePodAndModelMappingLocked(odlMetaPod.Name, odlMetaPod.Namespace, modelName, 1)
			}
		}
	}

	// ignore worker pods
	oldNodeType := oldPod.Labels[nodeType]
	newNodeType := newPod.Labels[nodeType]
	if oldNodeType == nodeWorker || newNodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "old pod", oldPod.Name, "new pod", newPod.Name)
		return
	}

	// Add new mappings if present
	if newOk {
		metaPod := c.addPodLocked(newPod)
		c.addPodAndModelMappingLocked(metaPod, newModelName)
	}

	klog.V(4).Infof("POD UPDATED: %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
	c.debugInfo()
}

func (c *Store) deletePod(obj interface{}) {
	var namespace, name string
	var hasModelLabel bool
	switch obj := obj.(type) {
	case *v1.Pod:
		namespace, name = obj.Namespace, obj.Name
		_, hasModelLabel = obj.Labels[modelIdentifier]
	case cache.DeletedFinalStateUnknown:
		if pod, ok := obj.Obj.(*v1.Pod); ok {
			namespace, name = pod.Namespace, pod.Name
			_, hasModelLabel = pod.Labels[modelIdentifier]
			break
		}

		// We can usually ignore cases where the object contained in the tombstone isn't a *v1.Pod. However,
		// since the following logic in this function still works fine with just the namespace and name,
		// let's try parsing the tombstone.Key here for added robustness.
		var err error
		namespace, name, err = cache.SplitMetaNamespaceKey(obj.Key)
		if err != nil {
			klog.ErrorS(err, "couldn't get pod's namespace and name from tombstone", "key", obj.Key)
			return
		}
	}
	key := fmt.Sprintf("%s/%s", namespace, name)
	_, existed := c.metaPods.Load(key)
	if !hasModelLabel && !existed {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// delete base model and associated lora models on this pod
	metaPod := c.deletePodLocked(name, namespace)
	if metaPod != nil {
		for _, modelName := range metaPod.Models.Array() {
			c.deletePodAndModelMappingLocked(name, namespace, modelName, 1)
		}
	}

	klog.V(4).Infof("POD DELETED: %s/%s", namespace, name)
	c.debugInfo()
}

func (c *Store) addModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.addPodAndModelMappingLockedByName(pod, model.Namespace, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
	c.debugInfo()
}

func (c *Store) updateModelAdapter(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldModel := oldObj.(*modelv1alpha1.ModelAdapter)
	newModel := newObj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range oldModel.Status.Instances {
		// the namespace of the pod is same as the namespace of model
		c.deletePodAndModelMappingLocked(pod, oldModel.Namespace, oldModel.Name, 0)
	}

	for _, pod := range newModel.Status.Instances {
		c.addPodAndModelMappingLockedByName(pod, newModel.Namespace, newModel.Name)
	}

	klog.V(4).Infof("MODELADAPTER UPDATED. %s/%s %s", oldModel.Namespace, oldModel.Name, newModel.Status.Phase)
	c.debugInfo()
}

func (c *Store) deleteModelAdapter(obj interface{}) {
	model, ok := obj.(*modelv1alpha1.ModelAdapter)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		model, ok = tombstone.Obj.(*modelv1alpha1.ModelAdapter)
		if !ok {
			return
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, pod := range model.Status.Instances {
		// the namespace of the pod is same as the namespace of model
		c.deletePodAndModelMappingLocked(pod, model.Namespace, model.Name, 0)
	}

	klog.V(4).Infof("MODELADAPTER DELETED: %s/%s", model.Namespace, model.Name)
	c.debugInfo()
}

func (c *Store) addPodLocked(pod *v1.Pod) *Pod {
	if c.bufferPod == nil {
		c.bufferPod = &Pod{
			Pod:    pod,
			Models: utils.NewRegistry[string](),
		}
	} else {
		c.bufferPod.Pod = pod
	}
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	metaPod, loaded := c.metaPods.LoadOrStore(key, c.bufferPod)
	if !loaded {
		c.bufferPod = nil
	}
	return metaPod
}

func (c *Store) addPodAndModelMappingLockedByName(podName, namespace, modelName string) {
	key := fmt.Sprintf("%s/%s", namespace, podName)
	pod, ok := c.metaPods.Load(key)
	if !ok {
		klog.Errorf("pod %s does not exist in internal-cache", podName)
		return
	}

	c.addPodAndModelMappingLocked(pod, modelName)
}

func (c *Store) addPodAndModelMappingLocked(metaPod *Pod, modelName string) {
	if c.bufferModel == nil {
		c.bufferModel = &Model{
			Pods: utils.NewRegistryWithArrayProvider(func(arr []*v1.Pod) *utils.PodArray { return &utils.PodArray{Pods: arr} }),
		}
	}
	metaModel, loaded := c.metaModels.LoadOrStore(modelName, c.bufferModel)
	if !loaded {
		c.bufferModel = nil
	}

	metaPod.Models.Store(modelName, modelName)
	key := fmt.Sprintf("%s/%s", metaPod.Namespace, metaPod.Name)
	metaModel.Pods.Store(key, metaPod.Pod)
}

func (c *Store) deletePodLocked(podName, podNamespace string) *Pod {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, _ := c.metaPods.LoadAndDelete(key)
	return metaPod
}

// deletePodAndModelMapping delete mappings between pods and model by specified names.
// If ignoreMapping > 0, podToModel mapping will be ignored.
// If ignoreMapping < 0, modelToPod mapping will be ignored
func (c *Store) deletePodAndModelMappingLocked(podName, namespace, modelName string, ignoreMapping int) {
	if ignoreMapping <= 0 {
		key := fmt.Sprintf("%s/%s", namespace, podName)
		if metaPod, ok := c.metaPods.Load(key); ok {
			metaPod.Models.Delete(modelName)
			// PodToModelMapping entry should only be deleted during pod deleting.
		}
	}

	if ignoreMapping >= 0 {
		if meta, ok := c.metaModels.Load(modelName); ok {
			key := fmt.Sprintf("%s/%s", namespace, podName)
			meta.Pods.Delete(key)
			if meta.Pods.Len() == 0 {
				c.metaModels.Delete(modelName)
			}
		}
	}
}
