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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	atomic_ext "go.uber.org/atomic"
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
	modelIdentifier = constants.ModelLabelName
	nodeType        = "ray.io/node-type"
	nodeWorker      = "worker"
	podGroupIndex   = "stormservice.orchestration.aibrix.ai/pod-group-index"
)

type adapterEdge struct {
	podKey    string
	modelName string
}

type adapterRecord struct {
	modelName string
	specBase  string
	instances []string
	edges     map[adapterEdge]struct{}
}

var (
	modelAdapterResyncMaxRetries    = 30
	modelAdapterResyncRetryInterval = 1 * time.Second
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

	// After cache sync, resync all ModelAdapters to ensure pod mappings are correct
	// This handles the case where ModelAdapters were processed before their pods were cached
	instance.resyncModelAdapters(modelInformer.GetStore(), stopCh)

	// Log cache state after initialization
	klog.Infof("Cache initialization completed. Models: %v", instance.ListModels())

	return nil
}

// getModelNameFromPod retrieves model name from pod labels first, then annotations.
// This supports cases where model names contain characters invalid for K8s labels (e.g., '/').
func getModelNameFromPod(pod *v1.Pod) (string, bool) {
	return constants.ModelNameFromMetadata(pod.Labels, pod.Annotations)
}

func (c *Store) modelClaimBindingsFromPod(pod *v1.Pod) map[string]utils.ModelClaimBinding {
	if c.disableModelClaimBindings {
		return nil
	}
	return utils.ModelClaimBindingsFromPod(pod)
}

func (c *Store) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)
	// Track pods that serve a model either through the standard deployment label
	// or through ModelClaim runtime annotations.
	modelName, ok := getModelNameFromPod(pod)
	modelClaims := c.modelClaimBindingsFromPod(pod)
	if !ok && len(modelClaims) == 0 {
		klog.V(4).InfoS("ignored pod without model label or annotation", "name", pod.Name)
		return
	}

	// ignore worker pod
	if isWorkerPod(pod) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.beginMembershipEventLocked()
	defer c.endMembershipEventLocked()

	metaPod := c.addPodLocked(pod)
	if ok {
		c.addPodAndModelMappingLocked(metaPod, modelName)
	}
	podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
	for servedModel, binding := range modelClaims {
		c.modelClaims.set(podKey, servedModel, binding)
		if binding.Port > 0 {
			c.addPodAndModelMappingLocked(metaPod, servedModel)
		}
	}
	c.linkPodToAdaptersLocked(metaPod, podKey)

	klog.V(4).Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
	c.debugInfo()

	// Notify KV event manager
	if c.kvEventManager != nil {
		c.kvEventManager.OnPodAdd(pod)
	}
}

func (c *Store) updatePod(oldObj interface{}, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// calculate early to avoid unnecessary lock
	oldIsWorker := isWorkerPod(oldPod)
	newIsWorker := isWorkerPod(newPod)
	if oldIsWorker && newIsWorker {
		klog.InfoS("ignore worker pod update:", "old pod", oldPod.Name, "new pod", newPod.Name)
		return
	}

	_, oldOk := getModelNameFromPod(oldPod)
	oldPodKey := utils.GeneratePodKey(oldPod.Namespace, oldPod.Name)
	_, existed := c.metaPods.Load(oldPodKey) // Make sure nothing left.
	newModelName, newOk := getModelNameFromPod(newPod)
	newModelClaims := c.modelClaimBindingsFromPod(newPod)
	newHasModelInfo := newOk || len(newModelClaims) > 0

	if !oldOk && !existed && !newHasModelInfo {
		return // No model information to track in either old or new pod
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.beginMembershipEventLocked()
	defer c.endMembershipEventLocked()

	oldNodeType := oldPod.Labels[nodeType]
	newNodeType := newPod.Labels[nodeType]
	ignoredRayWorker := oldNodeType == nodeWorker || newNodeType == nodeWorker
	newEligible := !ignoredRayWorker && newHasModelInfo && !newIsWorker

	// Remove old pod entry and clear adapter edges on oldPodKey, but defer removing
	// departing model-to-pod mappings until after incoming mappings are registered
	// so a single-pod model never transiently drops to zero pods.
	var oldMetaPod *Pod
	var affectedAdapterModels []string
	if oldOk || existed {
		oldMetaPod = c.deletePodLocked(oldPod.Name, oldPod.Namespace)
		c.modelClaims.clearPod(oldPodKey)
		affectedAdapterModels = c.clearPodAdapterEdgesLocked(oldPodKey)
	}

	newPodKey := utils.GeneratePodKey(newPod.Namespace, newPod.Name)
	var metaPod *Pod
	if newEligible {
		metaPod = c.addPodLocked(newPod)
		if newOk {
			c.addPodAndModelMappingLocked(metaPod, newModelName)
		}
		for servedModel, binding := range newModelClaims {
			c.modelClaims.set(newPodKey, servedModel, binding)
			if binding.Port > 0 {
				c.addPodAndModelMappingLocked(metaPod, servedModel)
			}
		}
		c.linkPodToAdaptersLocked(metaPod, newPodKey)
	}

	if oldMetaPod != nil {
		for _, modelName := range oldMetaPod.Models.Array() {
			if newEligible && oldPodKey == newPodKey && metaPod != nil {
				if _, stillServed := metaPod.Models.Load(modelName); stillServed {
					c.reconcileModelBaseModelLocked(modelName)
					continue
				}
			}
			c.deletePodAndModelMappingLocked(oldMetaPod.Name, oldMetaPod.Namespace, modelName, 1)
			c.reconcileModelBaseModelLocked(modelName)
		}
	}
	for _, modelName := range affectedAdapterModels {
		c.reconcileModelBaseModelLocked(modelName)
	}

	if ignoredRayWorker {
		c.clearPodMetricsBackoff(oldPod.Namespace, oldPod.Name)
		c.clearPodMetricsScheduling(oldPod.Namespace, oldPod.Name)
		klog.InfoS("ignored ray worker pod", "old pod", oldPod.Name, "new pod", newPod.Name)
		return
	}

	if !newEligible {
		c.clearPodMetricsBackoff(oldPod.Namespace, oldPod.Name)
		c.clearPodMetricsScheduling(oldPod.Namespace, oldPod.Name)
	}

	klog.V(4).Infof("POD UPDATED: %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
	c.debugInfo()

	// Notify KV event manager
	if c.kvEventManager != nil {
		c.kvEventManager.OnPodUpdate(oldPod, newPod)
	}
}

func (c *Store) deletePod(obj interface{}) {
	var namespace, name string
	var hasModelInfo bool
	var pod *v1.Pod
	switch obj := obj.(type) {
	case *v1.Pod:
		pod = obj
		namespace, name = obj.Namespace, obj.Name
		_, hasModelInfo = getModelNameFromPod(obj)
		hasModelInfo = hasModelInfo || len(c.modelClaimBindingsFromPod(obj)) > 0
	case cache.DeletedFinalStateUnknown:
		if p, ok := obj.Obj.(*v1.Pod); ok {
			pod = p
			namespace, name = p.Namespace, p.Name
			_, hasModelInfo = getModelNameFromPod(p)
			hasModelInfo = hasModelInfo || len(c.modelClaimBindingsFromPod(p)) > 0
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
	podKey := utils.GeneratePodKey(namespace, name)
	_, existed := c.metaPods.Load(podKey)
	if !hasModelInfo && !existed {
		return
	}

	// Notify KV event manager first (before lock)
	if c.kvEventManager != nil && pod != nil {
		c.kvEventManager.OnPodDelete(pod)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	affectedAdapterModels := c.clearPodAdapterEdgesLocked(podKey)

	// delete base model and associated lora models on this pod
	metaPod := c.deletePodLocked(name, namespace)
	if metaPod != nil {
		for _, modelName := range metaPod.Models.Array() {
			c.deletePodAndModelMappingLocked(name, namespace, modelName, 1)
			c.reconcileModelBaseModelLocked(modelName)
		}
	}
	for _, modelName := range affectedAdapterModels {
		c.reconcileModelBaseModelLocked(modelName)
	}
	c.modelClaims.clearPod(podKey)

	c.clearPodMetricsBackoff(namespace, name)
	c.clearPodMetricsScheduling(namespace, name)
	rateCalculator.PurgeEntriesForPod(namespace, name)

	klog.V(4).Infof("POD DELETED: %s/%s", namespace, name)
	c.debugInfo()
}

func (c *Store) addModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beginMembershipEventLocked()
	defer c.endMembershipEventLocked()

	model := obj.(*modelv1alpha1.ModelAdapter)
	c.upsertModelAdapterLocked(nil, model)

	klog.V(4).Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
	c.debugInfo()
}

func (c *Store) updateModelAdapter(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beginMembershipEventLocked()
	defer c.endMembershipEventLocked()

	oldModel := oldObj.(*modelv1alpha1.ModelAdapter)
	newModel := newObj.(*modelv1alpha1.ModelAdapter)
	c.upsertModelAdapterLocked(oldModel, newModel)

	c.debugInfo()
}

func adapterSpecBaseModel(model *modelv1alpha1.ModelAdapter) string {
	if model == nil || model.Spec.BaseModel == nil {
		return ""
	}
	return strings.TrimSpace(*model.Spec.BaseModel)
}

func (c *Store) upsertModelAdapterLocked(oldModel, newModel *modelv1alpha1.ModelAdapter) {
	oldEdges := make(map[adapterEdge]struct{})
	var oldKey string
	if oldModel != nil {
		oldKey = utils.GeneratePodKey(oldModel.Namespace, oldModel.Name)
		if rec, ok := c.adapterRecords[oldKey]; ok {
			for edge := range rec.edges {
				oldEdges[edge] = struct{}{}
			}
		}
		for _, podName := range oldModel.Status.Instances {
			oldEdges[adapterEdge{
				podKey:    utils.GeneratePodKey(oldModel.Namespace, podName),
				modelName: oldModel.Name,
			}] = struct{}{}
		}
	}

	newKey := utils.GeneratePodKey(newModel.Namespace, newModel.Name)
	if prevRec, ok := c.adapterRecords[newKey]; ok && newKey != oldKey {
		for edge := range prevRec.edges {
			oldEdges[edge] = struct{}{}
		}
	}

	newEdges := make(map[adapterEdge]struct{}, len(newModel.Status.Instances))
	newInstances := make([]string, 0, len(newModel.Status.Instances))
	for _, podName := range newModel.Status.Instances {
		podKey := utils.GeneratePodKey(newModel.Namespace, podName)
		newInstances = append(newInstances, podKey)
		if metaPod, ok := c.metaPods.Load(podKey); ok {
			c.addPodAndModelMappingLocked(metaPod, newModel.Name)
			newEdges[adapterEdge{podKey: podKey, modelName: newModel.Name}] = struct{}{}
		} else {
			klog.Errorf("pod %s does not exist in internal-cache", podName)
		}
	}

	if oldKey != "" {
		delete(c.adapterRecords, oldKey)
	}
	if len(newModel.Status.Instances) > 0 {
		if c.adapterRecords == nil {
			c.adapterRecords = make(map[string]*adapterRecord)
		}
		c.adapterRecords[newKey] = &adapterRecord{
			modelName: newModel.Name,
			specBase:  adapterSpecBaseModel(newModel),
			instances: newInstances,
			edges:     newEdges,
		}
	} else {
		delete(c.adapterRecords, newKey)
	}

	for edge := range oldEdges {
		if _, kept := newEdges[edge]; kept {
			continue
		}
		if c.isModelEdgeStillReferencedLocked(edge.podKey, edge.modelName) {
			continue
		}
		podNamespace, podName, ok := utils.ParsePodKey(edge.podKey)
		if !ok {
			continue
		}
		c.deletePodAndModelMappingLocked(podName, podNamespace, edge.modelName, 0)
	}

	if oldModel != nil && oldModel.Name != newModel.Name {
		c.reconcileModelBaseModelLocked(oldModel.Name)
	}
	c.reconcileModelBaseModelLocked(newModel.Name)
}

func (c *Store) recordAdapterEdgeLocked(model *modelv1alpha1.ModelAdapter, podKey string) {
	if c.adapterRecords == nil {
		c.adapterRecords = make(map[string]*adapterRecord)
	}
	adapterKey := utils.GeneratePodKey(model.Namespace, model.Name)
	rec, ok := c.adapterRecords[adapterKey]
	if !ok {
		instances := make([]string, 0, len(model.Status.Instances))
		for _, podName := range model.Status.Instances {
			instances = append(instances, utils.GeneratePodKey(model.Namespace, podName))
		}
		rec = &adapterRecord{
			modelName: model.Name,
			specBase:  adapterSpecBaseModel(model),
			instances: instances,
			edges:     make(map[adapterEdge]struct{}),
		}
		c.adapterRecords[adapterKey] = rec
	} else {
		rec.modelName = model.Name
		rec.specBase = adapterSpecBaseModel(model)
	}
	rec.edges[adapterEdge{podKey: podKey, modelName: model.Name}] = struct{}{}
}

func (c *Store) clearPodAdapterEdgesLocked(podKey string) []string {
	if len(c.adapterRecords) == 0 {
		return nil
	}
	var affectedModels []string
	for _, rec := range c.adapterRecords {
		for edge := range rec.edges {
			if edge.podKey == podKey {
				delete(rec.edges, edge)
				affectedModels = append(affectedModels, edge.modelName)
			}
		}
	}
	return affectedModels
}

func (c *Store) linkPodToAdaptersLocked(metaPod *Pod, podKey string) {
	for _, rec := range c.adapterRecords {
		for _, instKey := range rec.instances {
			if instKey == podKey {
				c.addPodAndModelMappingLocked(metaPod, rec.modelName)
				if rec.edges == nil {
					rec.edges = make(map[adapterEdge]struct{})
				}
				rec.edges[adapterEdge{podKey: podKey, modelName: rec.modelName}] = struct{}{}
				c.reconcileModelBaseModelLocked(rec.modelName)
				break
			}
		}
	}
}

func (c *Store) isModelEdgeStillReferencedLocked(podKey, modelName string) bool {
	edge := adapterEdge{podKey: podKey, modelName: modelName}
	for _, rec := range c.adapterRecords {
		if _, ok := rec.edges[edge]; ok {
			return true
		}
	}
	metaPod, ok := c.metaPods.Load(podKey)
	if !ok || metaPod.Pod == nil {
		return false
	}
	if podModel, ok := getModelNameFromPod(metaPod.Pod); ok && podModel == modelName {
		return true
	}
	if bindings := c.modelClaimBindingsFromPod(metaPod.Pod); len(bindings) > 0 {
		if binding, ok := bindings[modelName]; ok && binding.Port > 0 {
			return true
		}
	}
	return false
}

// setModelBaseModelLocked records the adapter → base-model mapping on the cache
// entry. Caller must hold c.mu. Prefers spec.baseModel; falls back to the host
// pod's model.aibrix.ai/name label when spec is empty.
func (c *Store) setModelBaseModelLocked(model *modelv1alpha1.ModelAdapter) {
	if model == nil {
		return
	}
	c.reconcileModelBaseModelLocked(model.Name)
}

func (c *Store) reconcileModelBaseModelLocked(modelName string) {
	meta, ok := c.metaModels.Load(modelName)
	if !ok {
		return
	}
	if len(c.adapterRecords) == 0 {
		meta.BaseModel = ""
		return
	}
	keys := make([]string, 0, len(c.adapterRecords))
	for key, rec := range c.adapterRecords {
		if rec.modelName == modelName && len(rec.edges) > 0 {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		meta.BaseModel = ""
		return
	}
	sort.Strings(keys)

	for _, key := range keys {
		rec := c.adapterRecords[key]
		if rec.specBase != "" {
			meta.BaseModel = rec.specBase
			return
		}
	}
	for _, key := range keys {
		rec := c.adapterRecords[key]
		for _, podKey := range rec.instances {
			if _, active := rec.edges[adapterEdge{podKey: podKey, modelName: modelName}]; !active {
				continue
			}
			metaPod, ok := c.metaPods.Load(podKey)
			if !ok || metaPod.Pod == nil || metaPod.Labels == nil {
				continue
			}
			if v := strings.TrimSpace(metaPod.Labels[constants.ModelLabelName]); v != "" {
				meta.BaseModel = v
				return
			}
		}
	}
	meta.BaseModel = ""
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

	adapterKey := utils.GeneratePodKey(model.Namespace, model.Name)
	oldEdges := make(map[adapterEdge]struct{})
	if rec, ok := c.adapterRecords[adapterKey]; ok {
		for edge := range rec.edges {
			oldEdges[edge] = struct{}{}
		}
		delete(c.adapterRecords, adapterKey)
	}
	for _, podName := range model.Status.Instances {
		oldEdges[adapterEdge{
			podKey:    utils.GeneratePodKey(model.Namespace, podName),
			modelName: model.Name,
		}] = struct{}{}
	}
	for edge := range oldEdges {
		if c.isModelEdgeStillReferencedLocked(edge.podKey, edge.modelName) {
			continue
		}
		podNamespace, podName, ok := utils.ParsePodKey(edge.podKey)
		if !ok {
			continue
		}
		c.deletePodAndModelMappingLocked(podName, podNamespace, edge.modelName, 0)
	}
	c.reconcileModelBaseModelLocked(model.Name)

	c.debugInfo()
}

// addPodLocked adds/resumes pod into the cache.
func (c *Store) addPodLocked(pod *v1.Pod) *Pod {
	key := utils.GeneratePodKey(pod.Namespace, pod.Name)
	if c.bufferPod == nil {
		c.bufferPod = &Pod{
			Pod:    pod,
			Models: utils.NewRegistry[string](),
		}
	} else {
		c.bufferPod.Pod = pod
	}

	// A pod key that was deleted moments ago (a transient health-check flap on a
	// busy pod, or the delete+re-add updatePod does for every in-place K8s pod
	// update -- see deletePodLocked) gets its realtime counters resumed here
	// instead of restarting at zero. Without this, a pod that never actually
	// stopped serving would briefly look empty to least-request/load-balance
	// scoring right as it reappears, and get piled on with new requests on top
	// of whatever it was already running.
	//
	// Gated on the reappearing pod's IP matching the deleted one's: the same
	// name can legitimately be recreated as a genuinely different backend (e.g.
	// a StatefulSet pod restarted with a fresh process on a new IP), which must
	// start at zero rather than inherit a stale, unrelated count -- see
	// TestDoneRequestCountAfterSameNamePodRecreationDoesNotDecrementNewPod.
	//
	// podStatsLockFor(key) is held across the snapshot-resume-and-publish sequence
	// below so it can't interleave with a concurrent addPodStats/donePodStats for the
	// same key (which take the same lock around resolving and mutating the counters
	// this resumes) -- see podStatsLockFor's doc comment in cache_trace.go.
	mu := c.podStatsLockFor(key)
	mu.Lock()
	defer mu.Unlock()

	resumed := false
	if snap, ok := c.recentlyDeletedPods.LoadAndDelete(key); ok {
		if time.Since(snap.deletedAt) < recentlyDeletedPodGracePeriod &&
			snap.podIP != "" && snap.podIP == pod.Status.PodIP {
			c.bufferPod.runningRequests = atomic.LoadInt32(&snap.runningRequests)
			c.bufferPod.completedRequests = atomic.LoadInt64(&snap.completedRequests)
			c.bufferPod.completedOutputTokens = atomic.LoadInt64(&snap.completedOutputTokens)
			c.bufferPod.pendingLoadUtilization.Store(snap.pendingLoadUtilization.Load())
			c.bufferPod.statsGeneration = snap.statsGeneration
			// A same-IP resume means the pod never stopped serving, so its ramp clock
			// (see readySince in pod.go) continues too.
			c.bufferPod.readySince.Store(snap.readySince.Load())
			resumed = true
		}
	}
	if !resumed {
		c.bufferPod.statsGeneration = c.nextStatsGeneration.Add(1)
		c.bufferPod.readySince.Store(0)
	}
	// readySince starts when the pod is first seen routable, not when it is added: every update
	// (Pending -> Ready included) is a same-IP delete + re-add that resumes the snapshot above, so
	// stamping at add would let a pod that sat Pending longer than the ramp window arrive with the
	// ramp already spent. A not-yet-routable pod keeps 0 (routing never sees it); the stamp is kept
	// from then on, including across NotReady flaps on the same IP.
	if c.bufferPod.readySince.Load() == 0 && utils.FilterReadyPod(pod) {
		c.bufferPod.readySince.Store(time.Now().UnixNano())
	}
	// Running max for LatestPodReadySince. A resumed or not-yet-routable pod carries an old value
	// or 0, which storeIfGreater ignores. Safe before the LoadOrStore race below resolves.
	storeIfGreater(&c.lastPodReadySince, c.bufferPod.readySince.Load())

	metaPod, loaded := c.metaPods.LoadOrStore(key, c.bufferPod)
	if !loaded {
		c.bufferPod = nil
	} else {
		// An entry already existed, so this buffer wasn't consumed and will
		// be reused for an unrelated pod on some future call -- clear any
		// counters just seeded onto it so they don't leak into that pod.
		// lastPodReadySince is not rolled back: it reflects a genuine add attempt either way.
		c.bufferPod.runningRequests = 0
		c.bufferPod.completedRequests = 0
		c.bufferPod.completedOutputTokens = 0
		c.bufferPod.pendingLoadUtilization.Store(0)
		c.bufferPod.statsGeneration = 0
		c.bufferPod.readySince.Store(0)
	}
	return metaPod
}

// storeIfGreater raises *addr to val if val is larger. A plain Store could let an earlier
// timestamp overwrite a later one when addPodLocked calls for different pod keys race (each is
// serialized only by its own lock stripe). Maintains lastPodReadySince as a running max.
func storeIfGreater(addr *atomic.Int64, val int64) {
	for {
		cur := addr.Load()
		if val <= cur {
			return
		}
		if addr.CompareAndSwap(cur, val) {
			return
		}
	}
}

func (c *Store) addPodAndModelMappingLockedByName(podName, namespace, modelName string) {
	pod, ok := c.metaPods.Load(utils.GeneratePodKey(namespace, podName))
	if !ok {
		klog.Errorf("pod %s does not exist in internal-cache", podName)
		return
	}

	c.addPodAndModelMappingLocked(pod, modelName)
}

func (c *Store) beginMembershipEventLocked() {
	c.routerAttemptedInEvent = make(map[string]struct{})
}

func (c *Store) endMembershipEventLocked() {
	c.routerAttemptedInEvent = nil
}

func (c *Store) ensureModelQueueRouterLocked(metaModel *Model, modelName string) {
	if metaModel == nil || metaModel.QueueRouter != nil || c.modelRouterProvider == nil {
		return
	}
	if c.routerAttemptedInEvent != nil {
		if _, tried := c.routerAttemptedInEvent[modelName]; tried {
			return
		}
		c.routerAttemptedInEvent[modelName] = struct{}{}
	}
	router, err := c.modelRouterProvider(modelName)
	if err != nil {
		klog.Errorf("failed to initialize model-based queue router: %v", err)
		return
	}
	metaModel.QueueRouter = router
}

func (c *Store) addPodAndModelMappingLocked(metaPod *Pod, modelName string) {
	if c.bufferModel == nil {
		c.bufferModel = &Model{
			Pods:            utils.NewRegistryWithArrayProvider(func(arr []*v1.Pod) *utils.PodArray { return &utils.PodArray{Pods: arr} }),
			OutputPredictor: NewSimpleOutputPredictor(maxInputTokens, maxOutputTokens, movingWindow),
		}
	}
	metaModel, loaded := c.metaModels.LoadOrStore(modelName, c.bufferModel)
	if !loaded {
		c.bufferModel = nil
		klog.V(4).Infof("MODEL(ADAPTER) CREATED: %s", modelName)
	}
	c.ensureModelQueueRouterLocked(metaModel, modelName)

	metaPod.Models.Store(modelName, modelName)
	podKey := utils.GeneratePodKey(metaPod.Namespace, metaPod.Name)
	metaModel.Pods.Store(podKey, metaPod.Pod)

	klog.V(4).InfoS("Pod added to model", "model", modelName, "pod", podKey, "pods", metaModel.Pods.Len())
}

// deletedPodSnapshot preserves a deleted pod's realtime request-tracking
// counters so a fast re-add of the same key (see addPodLocked) can resume
// from where they left off instead of restarting at zero.
//
// It is stored and shared by pointer (not by value): a request that started
// before the delete can still complete while the pod key is absent from
// metaPods entirely -- mid-flap, before any re-add happens (see
// donePodStats). Its counters must be mutable in place, atomically, so that
// completion is reflected in whatever a later re-add resumes from, rather
// than being silently lost.
type deletedPodSnapshot struct {
	pod                    *Pod   // Orphaned cache pod; same statsGeneration. addPodStats mid-gap stores this as podStats.pod so donePodStats can resolve back to this snapshot.
	podIP                  string // Guards against reuse across a genuine pod recreation; see addPodLocked.
	statsGeneration        int64  // Copied from pod at delete; resume copies it onto the new *Pod, fresh add does not.
	runningRequests        int32  // atomic
	completedRequests      int64  // atomic
	completedOutputTokens  int64  // atomic
	pendingLoadUtilization atomic_ext.Float64
	readySince             atomic.Int64 // see Pod.readySince
	deletedAt              time.Time
}

// recentlyDeletedPodGracePeriod bounds how long a deletedPodSnapshot survives
// waiting for a matching re-add before it's dropped as a genuine deletion.
const recentlyDeletedPodGracePeriod = 60 * time.Second

func (c *Store) deletePodLocked(podName, podNamespace string) *Pod {
	key := utils.GeneratePodKey(podNamespace, podName)

	// See the matching lock in addPodLocked and podStatsLockFor's doc comment
	// (cache_trace.go): held across the snapshot-take-and-publish sequence below so
	// it can't interleave with a concurrent addPodStats/donePodStats for this key.
	mu := c.podStatsLockFor(key)
	mu.Lock()
	defer mu.Unlock()

	metaPod, ok := c.metaPods.LoadAndDelete(key)
	if ok {
		snap := &deletedPodSnapshot{
			pod:               metaPod,
			podIP:             metaPod.Status.PodIP,
			statsGeneration:   metaPod.statsGeneration,
			runningRequests:   atomic.LoadInt32(&metaPod.runningRequests),
			completedRequests: atomic.LoadInt64(&metaPod.completedRequests),
			deletedAt:         time.Now(),
		}
		snap.completedOutputTokens = atomic.LoadInt64(&metaPod.completedOutputTokens)
		snap.pendingLoadUtilization.Store(metaPod.pendingLoadUtilization.Load())
		snap.readySince.Store(metaPod.readySince.Load())
		c.recentlyDeletedPods.Store(key, snap)
		c.pruneExpiredDeletedPodSnapshotsLocked()
	}
	return metaPod
}

// pruneExpiredDeletedPodSnapshotsLocked drops snapshots whose grace period has
// elapsed. Run from deletePodLocked (rather than a dedicated ticker) so pods
// that never come back don't leak an entry here forever, while staying
// bounded by delete frequency instead of needing its own background goroutine.
func (c *Store) pruneExpiredDeletedPodSnapshotsLocked() {
	now := time.Now()
	c.recentlyDeletedPods.Range(func(key string, snap *deletedPodSnapshot) bool {
		if now.Sub(snap.deletedAt) >= recentlyDeletedPodGracePeriod {
			c.recentlyDeletedPods.Delete(key)
		}
		return true
	})
}

// deletePodAndModelMapping delete mappings between pods and model by specified names.
// If ignoreMapping > 0, podToModel mapping will be ignored.
// If ignoreMapping < 0, modelToPod mapping will be ignored
func (c *Store) deletePodAndModelMappingLocked(podName, namespace, modelName string, ignoreMapping int) {
	podKey := utils.GeneratePodKey(namespace, podName)
	if ignoreMapping <= 0 {
		if metaPod, ok := c.metaPods.Load(podKey); ok {
			metaPod.Models.Delete(modelName)
			// PodToModelMapping entry should only be deleted during pod deleting.
		}
	}

	if ignoreMapping >= 0 {
		if meta, ok := c.metaModels.Load(modelName); ok {
			meta.Pods.Delete(podKey)
			klog.V(4).InfoS("Pod removed from model", "model", modelName, "pod", podKey, "pods", meta.Pods.Len())

			if meta.Pods.Len() == 0 {
				c.metaModels.Delete(modelName)
				klog.V(4).Infof("MODEL(ADAPTER) DELETED: %s", modelName)
			}
		}
	}
}

// resyncModelAdapters processes all ModelAdapters from the informer store to ensure
// all pod mappings are correctly established after cache initialization.
// It retries missing pod mappings in batches so startup delay is bounded by
// maxRetries * retryInterval regardless of the number of adapters.
func (c *Store) resyncModelAdapters(store cache.Store, stopCh <-chan struct{}) {
	klog.Info("Resyncing ModelAdapters to ensure pod mappings are correct")

	adapters := make([]*modelv1alpha1.ModelAdapter, 0)
	for _, obj := range store.List() {
		if modelAdapter, ok := obj.(*modelv1alpha1.ModelAdapter); ok {
			adapters = append(adapters, modelAdapter)
		}
	}

	lastMissing := make(map[string][]string)
	for i := 0; i < modelAdapterResyncMaxRetries; i++ {
		klog.V(4).Infof("resyncModelAdapters retry attempt %d/%d", i+1, modelAdapterResyncMaxRetries)

		incompleteModels := 0
		lastMissing = make(map[string][]string)
		for _, modelAdapter := range adapters {
			missingPods := []string{}

			c.mu.Lock()
			c.beginMembershipEventLocked()
			for _, podName := range modelAdapter.Status.Instances {
				podKey := utils.GeneratePodKey(modelAdapter.Namespace, podName)
				if metaPod, exists := c.metaPods.Load(podKey); exists {
					c.addPodAndModelMappingLocked(metaPod, modelAdapter.Name)
					c.recordAdapterEdgeLocked(modelAdapter, podKey)
					klog.V(4).Infof("Resynced pod mapping for adapter %s, pod %s/%s",
						modelAdapter.Name, modelAdapter.Namespace, podName)
				} else {
					missingPods = append(missingPods, podName)
					klog.V(4).Infof("Pod %s/%s not found in cache for ModelAdapter %s during resync (attempt %d/%d)",
						modelAdapter.Namespace, podName, modelAdapter.Name, i+1, modelAdapterResyncMaxRetries)
				}
			}
			c.reconcileModelBaseModelLocked(modelAdapter.Name)
			c.endMembershipEventLocked()
			c.mu.Unlock()

			if len(missingPods) > 0 {
				incompleteModels++
				lastMissing[modelAdapter.Name] = missingPods
			}
		}

		if incompleteModels == 0 {
			break
		}

		if i == modelAdapterResyncMaxRetries-1 {
			break
		}

		if !waitForModelAdapterResyncRetry(stopCh, modelAdapterResyncRetryInterval) {
			klog.Warning("ModelAdapter resync interrupted by stop signal")
			return
		}
	}

	totalModels := len(adapters)
	completeModels := totalModels - len(lastMissing)
	for _, modelAdapter := range adapters {
		if missingPods, exists := lastMissing[modelAdapter.Name]; exists {
			klog.Errorf("Failed to find all pods for ModelAdapter %s after %d retries", modelAdapter.Name, modelAdapterResyncMaxRetries)
			klog.Errorf("Missing pods for ModelAdapter %s: %v", modelAdapter.Name, missingPods)
		} else {
			klog.V(4).Infof("ModelAdapter %s has all pod mappings established", modelAdapter.Name)
		}
	}
	klog.Infof("ModelAdapter mapping resync completed: %d total, %d complete, %d incomplete",
		totalModels, completeModels, len(lastMissing))
	klog.Info("ModelAdapter resync completed")
}

func waitForModelAdapterResyncRetry(stopCh <-chan struct{}, interval time.Duration) bool {
	if stopCh == nil {
		time.Sleep(interval)
		return true
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()

	select {
	case <-stopCh:
		return false
	case <-timer.C:
		return true
	}
}

func isWorkerPod(pod *v1.Pod) bool {
	nodeTyp, ok := pod.Labels[nodeType]
	if ok && nodeTyp == nodeWorker {
		klog.V(4).InfoS("ignored ray worker pod", "name", pod.Name)
		return true
	}

	pgIndex, ok := pod.Labels[podGroupIndex]
	if ok {
		pgIndexNumber, err := strconv.Atoi(pgIndex)
		if err != nil {
			klog.V(4).InfoS("ignored pod:", "name", pod.Name, "err", err)
		}
		if pgIndexNumber > 0 {
			klog.V(4).InfoS("ignored pod: podGroupIndex > 0", "name", pod.Name, "index", pgIndex)
			return true
		}
	}
	return false
}
