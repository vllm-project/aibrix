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

package routingalgorithms

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aibrix/aibrix/pkg/plugins/gateway/prefixcacheindexer"
	"github.com/aibrix/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	defaultDecodingLength = 45
	slidingWindowPeriod   = 3 * time.Minute
	evictionLoopInterval  = 1000 * time.Millisecond
)

var (
	dummy_var = getDummyVar()
)

func getDummyVar() int {
	dummay_var := 0
	klog.Info("getDummyVar, dummay_var: ", dummay_var)
	return dummay_var
}

type SlidingWindowHistogram struct {
	mu             sync.RWMutex
	windowDuration time.Duration
	histogram      map[*prefixcacheindexer.TreeNode]int
	nodeToCount    map[*prefixcacheindexer.TreeNode]int
	hitTokens      map[*prefixcacheindexer.TreeNode]int
	promptTokens   map[*prefixcacheindexer.TreeNode]int
	decodingSize   map[*prefixcacheindexer.TreeNode]int
	timestamps     []histogramEntry
	numPods        int
	podAllocations map[*prefixcacheindexer.TreeNode]map[int]bool
}

type histogramEntry struct {
	timestamp time.Time
	node      *prefixcacheindexer.TreeNode
	leafNode  *prefixcacheindexer.TreeNode
}

type prefixCacheAndLoadRouter struct {
	cache          *prefixcacheindexer.LPRadixCache
	histogram      *SlidingWindowHistogram
	numPods        int
	mu             sync.RWMutex
	podAllocations map[*prefixcacheindexer.TreeNode]map[int]bool
}

// TODO: It needs to read the running pods accordingly.
// Also, the radix tree cache does not support varying number of pods.
// The tree data structure should be updated in real time with varying number of pods.
// Especially when a pod is removed, the corresponding TreeNode should be removed from the RadixTree and from the related data structures in SlidingWindowHistogram.
func NewPrefixCacheAndLoadRouter() (Router, error) {
	numPods := 2 // FIXME: Hardcoded for now. not just this but the entire tree cache implementation...
	histogram := &SlidingWindowHistogram{
		windowDuration: slidingWindowPeriod,
		histogram:      make(map[*prefixcacheindexer.TreeNode]int),
		nodeToCount:    make(map[*prefixcacheindexer.TreeNode]int),
		hitTokens:      make(map[*prefixcacheindexer.TreeNode]int),
		promptTokens:   make(map[*prefixcacheindexer.TreeNode]int),
		decodingSize:   make(map[*prefixcacheindexer.TreeNode]int),
		numPods:        numPods,
		podAllocations: make(map[*prefixcacheindexer.TreeNode]map[int]bool),
	}

	router := &prefixCacheAndLoadRouter{
		cache:          prefixcacheindexer.NewLPRadixCache(numPods),
		histogram:      histogram,
		numPods:        numPods,
		podAllocations: make(map[*prefixcacheindexer.TreeNode]map[int]bool),
	}

	// Start eviction ticker
	go router.evictionLoop()

	return router, nil
}

func (h *SlidingWindowHistogram) removeEvictedNodes(nodes []*prefixcacheindexer.TreeNode) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Create a map for faster lookup
	nodeMap := make(map[*prefixcacheindexer.TreeNode]bool)
	for _, node := range nodes {
		nodeMap[node] = true
	}

	// Filter timestamps
	newTimestamps := make([]histogramEntry, 0)
	for _, entry := range h.timestamps {
		if !nodeMap[entry.node] {
			newTimestamps = append(newTimestamps, entry)
		}
	}
	h.timestamps = newTimestamps

	// Remove from all maps
	for node := range nodeMap {
		delete(h.histogram, node)
		delete(h.nodeToCount, node)
		delete(h.hitTokens, node)
		delete(h.promptTokens, node)
		delete(h.decodingSize, node)
		delete(h.podAllocations, node)
	}
}

func (h *SlidingWindowHistogram) removeOldEntries(currentTime time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	windowStart := currentTime.Add(-h.windowDuration)
	newTimestamps := make([]histogramEntry, 0)

	for _, entry := range h.timestamps {
		if entry.timestamp.After(windowStart) {
			newTimestamps = append(newTimestamps, entry)
		} else {
			node := entry.node
			leafNode := entry.leafNode

			h.histogram[node] -= leafNode.ContextLength()
			h.nodeToCount[node]--
			h.hitTokens[node] -= leafNode.ContextLength() - leafNode.NumTokens()
			h.promptTokens[node] -= leafNode.ContextLength()

			if h.histogram[node] <= 0 {
				delete(h.histogram, node)
				delete(h.nodeToCount, node)
				delete(h.hitTokens, node)
				delete(h.promptTokens, node)
				delete(h.decodingSize, node)
				delete(h.podAllocations, node)
			}
		}
	}
	h.timestamps = newTimestamps
}

func (p *prefixCacheAndLoadRouter) evictionLoop() {
	ticker := time.NewTicker(evictionLoopInterval)
	for range ticker.C {
		p.mu.Lock()
		evictedNodes := p.cache.Evict(time.Now())
		if len(evictedNodes) > 0 {
			p.histogram.removeEvictedNodes(evictedNodes)
		}
		p.histogram.removeOldEntries(time.Now())
		p.mu.Unlock()
	}
}

func (p *prefixCacheAndLoadRouter) Route(ctx context.Context, pods map[string]*v1.Pod, model, message string) (string, error) {
	readyPods := utils.FilterReadyPods(pods)
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}
	if len(readyPods) == 1 {
		for _, pod := range readyPods {
			return getPodAddress(pod.Status.PodIP)
		}
	}

	// input: {"content":"I like apple","role":"user"}
	// output: "I like apple"
	trimmedMessage := utils.TrimMessage(message)
	klog.Infof("Trimmed message: %s", trimmedMessage)
	tokens, err := utils.TokenizeInputText(trimmedMessage)
	if err != nil {
		return "", err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Single traversal to find/create node
	klog.Info("AddPrefix to the tree: ", tokens)
	node, matchedTokens, _ := p.cache.AddPrefix(tokens, model, "", false)

	var matchedPods []*v1.Pod // matchedpods will have all the pods that have the target model
	if modelPods, ok := node.ModelToPods[model]; ok {
		for podName := range modelPods {
			for _, pod := range readyPods {
				if pod.Name == podName { // Filter ready pods among all pods having the target model
					matchedPods = append(matchedPods, pod)
				}
			}
		}
	}

	var targetPod *v1.Pod
	matchRatio := float64(len(matchedTokens)) / float64(len(tokens))
	prefix_routing_threshold := 0.5
	klog.Infof("Total tokens: %d, Matched tokens: %d, Matching ratio: %f%%, # Matched pods: %d",
		len(tokens), len(matchedTokens), matchRatio, len(matchedPods))
	if matchRatio > prefix_routing_threshold && len(matchedPods) > 0 {
		klog.Infof("Do prefix-aware routing! (matching ratio: %f > %f)", matchRatio, prefix_routing_threshold)
		minLoad := -1
		for _, pod := range matchedPods {
			load := p.histogram.getPodLoad(pod)
			if minLoad == -1 || load < minLoad {
				minLoad = load
				targetPod = pod
			}
		}
		klog.Infof("Lowest load among all matched pods: %s", targetPod.Name)
	}

	if targetPod == nil {
		klog.Infof("Do cost model based routing! (matching ratio: %f <= %f)", matchRatio, prefix_routing_threshold)
		minLoad := -1
		for _, pod := range readyPods {
			load := p.histogram.getPodLoad(pod)
			klog.Infof("Pod: %s, Load: %d", pod.Name, load)
			if minLoad == -1 || load < minLoad {
				minLoad = load
				targetPod = pod
			}
		}
		klog.Infof("Lowest load pod: %s", targetPod.Name)
	}

	if targetPod == nil {
		return "", fmt.Errorf("no suitable pod found")
	}

	// Update pod mapping in the same node
	if modelPods, ok := node.ModelToPods[model]; !ok {
		node.ModelToPods[model] = map[string]time.Time{
			targetPod.Name: time.Now(),
		}
	} else {
		modelPods[targetPod.Name] = time.Now()
	}

	// Update histogram
	p.histogram.update(time.Now(), node, node, targetPod.Name, defaultDecodingLength)

	klog.InfoS(
		"target_pod_name", targetPod.Name,
	)

	return getPodAddress(targetPod.Status.PodIP)
}

// Compute the load in a pod fo a specific model based on the sliding window histogram
func (h *SlidingWindowHistogram) getPodLoad(pod *v1.Pod) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	load := 0
	for node, count := range h.nodeToCount {
		for _, podMap := range node.ModelToPods {
			if _, exists := podMap[pod.Name]; exists {
				load += count
				break // Found this pod in this node, no need to check other models
			}
		}
	}
	return load
}

// Update histogram to use pod name instead of pod ID
func (h *SlidingWindowHistogram) update(timestamp time.Time, node, leafNode *prefixcacheindexer.TreeNode, podName string, decodingLength int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.timestamps = append(h.timestamps, histogramEntry{
		timestamp: timestamp,
		node:      node,
		leafNode:  leafNode,
	})

	h.histogram[node] += leafNode.ContextLength()
	h.nodeToCount[node]++
	h.decodingSize[node] = decodingLength

	h.hitTokens[node] += leafNode.ContextLength() - leafNode.NumTokens()
	h.promptTokens[node] += leafNode.ContextLength()
}

func (p *prefixCacheAndLoadRouter) updatePodAllocation(node *prefixcacheindexer.TreeNode, podID int) {
	if p.podAllocations[node] == nil {
		p.podAllocations[node] = make(map[int]bool)
	}
	p.podAllocations[node][podID] = true
}
