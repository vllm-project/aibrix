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
		windowDuration: 3 * time.Minute,
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

func (p *prefixCacheAndLoadRouter) evictionLoop() {
	ticker := time.NewTicker(50 * time.Millisecond) // NOTE: eviction event interval is hardcoded
	for range ticker.C {
		p.cache.Evict(time.Now())
		p.histogram.removeOldEntries(time.Now())
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

	tokens, err := utils.TokenizeInputText(message)
	if err != nil {
		return "", err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Single traversal to find/create node
	node := p.cache.AddPrefix(tokens, model, "", false)

	// Get matched pods
	var matchedPods []*v1.Pod
	if blockPods, ok := node.ModelToPods[model]; ok {
		for podName := range blockPods {
			for _, pod := range readyPods {
				if pod.Name == podName {
					matchedPods = append(matchedPods, pod)
				}
			}
		}
	}

	var targetPod *v1.Pod
	if len(node.GetValue()) > len(tokens)/2 && len(matchedPods) > 0 {
		minLoad := -1
		for _, pod := range matchedPods {
			load := p.histogram.getPodLoad(pod)
			if minLoad == -1 || load < minLoad {
				minLoad = load
				targetPod = pod
			}
		}
	}

	if targetPod == nil {
		minLoad := -1
		for _, pod := range readyPods {
			load := p.histogram.getPodLoad(pod)
			if minLoad == -1 || load < minLoad {
				minLoad = load
				targetPod = pod
			}
		}
	}

	if targetPod == nil {
		return "", fmt.Errorf("no suitable pod found")
	}

	// Update pod mapping in the same node
	if blockPods, ok := node.ModelToPods[model]; !ok {
		node.ModelToPods[model] = map[string]time.Time{
			targetPod.Name: time.Now(),
		}
	} else {
		blockPods[targetPod.Name] = time.Now()
	}

	// Update histogram
	p.histogram.update(time.Now(), node, node, targetPod.Name, defaultDecodingLength)

	klog.InfoS("prefix cache and load route",
		"message", message,
		"tokens", len(tokens),
		"matched_tokens", len(node.GetValue()),
		"matched_pods", len(matchedPods),
		"target_pod", targetPod.Status.PodIP)

	return getPodAddress(targetPod.Status.PodIP)
}

// Helper function to calculate pod load from histogram
func (h *SlidingWindowHistogram) getPodLoad(pod *v1.Pod) int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	load := 0
	for node, count := range h.nodeToCount {
		if blockPods, ok := node.ModelToPods[pod.Name]; ok && len(blockPods) > 0 {
			load += count
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

func (p *prefixCacheAndLoadRouter) updatePodAllocation(node *prefixcacheindexer.TreeNode, podID int) {
	if p.podAllocations[node] == nil {
		p.podAllocations[node] = make(map[int]bool)
	}
	p.podAllocations[node][podID] = true
}
