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
	"math"
	"sort"
	"sync"
	"time"

	"github.com/aibrix/aibrix/pkg/plugins/gateway/prefixcacheindexer"
	"github.com/aibrix/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	defaultDecodingLength = 45 // NOTE: decode length is hardcoded. Preble as well.
	slidingWindowPeriod   = 3 * time.Minute
	evictionLoopInterval  = 1000 * time.Millisecond
	targetGPU             = "V100" // A6000
)

var (
	dummy_var = getDummyVar()
)

func getDummyVar() int {
	dummay_var := 0
	klog.Info("Test getDummyVar, dummay_var: ", dummay_var)
	return dummay_var
}

type SlidingWindowHistogram struct {
	mu                         sync.RWMutex
	windowDuration             time.Duration
	histogram                  map[*prefixcacheindexer.TreeNode]int
	nodeToCount                map[*prefixcacheindexer.TreeNode]int
	hitTokens                  map[*prefixcacheindexer.TreeNode]int
	promptTokens               map[*prefixcacheindexer.TreeNode]int
	decodingSize               map[*prefixcacheindexer.TreeNode]int
	timestamps                 []histogramEntry
	numPods                    int
	podAllocations             map[*prefixcacheindexer.TreeNode]map[int]bool
	currentPrefillCostPerPod   map[string]float64 // pod name -> prefill cost
	currentDecodeLengthsPerPod map[string]int     // pod name -> total decode length
	perNodePrefillCost         map[*prefixcacheindexer.TreeNode]float64
	perNodeTotalDecodeLengths  map[*prefixcacheindexer.TreeNode]int
	avgTimePerTokenPerPod      map[string][]float64 // pod name -> list of time/token measurements
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

// Find all prefix matches with their depths
type prefixMatch struct {
	node        *prefixcacheindexer.TreeNode
	pods        []*v1.Pod
	matchLength int
	depth       int
}

type PrefillTimeParams struct {
	NumRequests      int
	NumBatchedTokens int
	TotalContext     int
	InputIDLens      []int
	NumUniqueKV      int
	SeqLens          []int
}

func mistral7BA6000LinearTime(numBatchedTokens int) float64 {
	if numBatchedTokens >= 384 {
		return (0.10842571*float64(numBatchedTokens) + 4.209777054806409) / 1000.0
	} else if numBatchedTokens >= 192 {
		return (-118 + 1.25*float64(numBatchedTokens) - 2.56e-3*math.Pow(float64(numBatchedTokens), 2)) / 1000.0
	}
	return 22.0 / 1000.0
}

func mistral7BA6000AttentionTime(numReqs, totalContext, numUniqueKV int) float64 {
	if numUniqueKV == 0 {
		numUniqueKV = totalContext
	}

	var forwardTime float64
	if totalContext <= 1024 {
		forwardTime = 0.32
	} else {
		forwardTime = 1.86e-4*float64(totalContext) + 0.159
		if float64(numUniqueKV)/float64(numReqs) <= 1024 && numReqs*numUniqueKV <= 32*256*2048 {
			forwardTime /= 2
		}
	}
	return forwardTime / 1000.0
}

// Adjusted for V100 characteristics
func mistral7BV100LinearTime(numBatchedTokens int) float64 {
	if numBatchedTokens >= 384 {
		// Increased coefficient due to lower compute power
		// ~2.5x increase for linear component due to slower tensor cores
		return (0.27106428*float64(numBatchedTokens) + 10.52444263) / 1000.0
	} else if numBatchedTokens >= 192 {
		// Adjusted quadratic coefficient to reflect V100's architecture
		return (-295 + 3.125*float64(numBatchedTokens) - 6.4e-3*math.Pow(float64(numBatchedTokens), 2)) / 1000.0
	}
	// Base latency increased
	return 55.0 / 1000.0
}

func mistral7BV100AttentionTime(numReqs, totalContext, numUniqueKV int) float64 {
	if numUniqueKV == 0 {
		numUniqueKV = totalContext
	}

	var forwardTime float64
	if totalContext <= 1024 {
		// Increased base attention time for shorter sequences
		forwardTime = 0.80
	} else {
		// Increased linear coefficient and base time for attention
		// Memory bandwidth is better but compute is slower
		forwardTime = 4.65e-4*float64(totalContext) + 0.398
		if float64(numUniqueKV)/float64(numReqs) <= 1024 && numReqs*numUniqueKV <= 32*256*2048 {
			forwardTime /= 2
		}
	}
	return forwardTime / 1000.0
}

func (h *SlidingWindowHistogram) getPrefillCost(node *prefixcacheindexer.TreeNode) float64 {
	missRate := 1.0
	if h.promptTokens[node] > 0 {
		missRate = 1.0 - (float64(h.hitTokens[node]) / float64(h.promptTokens[node]))
	}
	numTokens := node.NumTokens()
	contextLength := node.ContextLength()
	baseTime := 0.0
	if targetGPU == "A6000" {
		baseTime = mistral7BA6000LinearTime(numTokens) + mistral7BA6000AttentionTime(1, contextLength, numTokens)
	} else if targetGPU == "V100" {
		baseTime = mistral7BV100LinearTime(numTokens) + mistral7BV100AttentionTime(1, contextLength, numTokens)
	} else {
		klog.Warningf("Unknown target GPU: %s. Assume V100 as default", targetGPU)
		baseTime = mistral7BV100LinearTime(numTokens) + mistral7BV100AttentionTime(1, contextLength, numTokens)
	}
	var attnQuad float64
	if numTokens >= 4096 {
		attnQuad = (-7.37 + 3.86e-3*float64(numTokens) + 2.16e-6*math.Pow(float64(numTokens), 2)) / 1000.0
	}
	prefillTime := (baseTime + attnQuad) / 0.9
	numGPUs := len(node.ModelToPods) // You might need to adjust this based on your actual GPU allocation tracking
	return missRate * float64(h.nodeToCount[node]) * prefillTime / float64(numGPUs)
}

// TODO: It needs to read the running pods accordingly.
// Also, the radix tree cache does not support varying number of pods.
// The tree data structure should be updated in real time with varying number of pods.
// Especially when a pod is removed, the corresponding TreeNode should be removed from the RadixTree and from the related data structures in SlidingWindowHistogram.
func NewPrefixCacheAndLoadRouter() (Router, error) {
	numPods := 2 // FIXME: Hardcoded for now. not just this but the entire tree cache implementation...
	histogram := &SlidingWindowHistogram{
		windowDuration:             slidingWindowPeriod,
		histogram:                  make(map[*prefixcacheindexer.TreeNode]int),
		nodeToCount:                make(map[*prefixcacheindexer.TreeNode]int),
		hitTokens:                  make(map[*prefixcacheindexer.TreeNode]int),
		promptTokens:               make(map[*prefixcacheindexer.TreeNode]int),
		decodingSize:               make(map[*prefixcacheindexer.TreeNode]int),
		numPods:                    numPods,
		podAllocations:             make(map[*prefixcacheindexer.TreeNode]map[int]bool),
		currentPrefillCostPerPod:   make(map[string]float64),
		currentDecodeLengthsPerPod: make(map[string]int),
		perNodePrefillCost:         make(map[*prefixcacheindexer.TreeNode]float64),
		perNodeTotalDecodeLengths:  make(map[*prefixcacheindexer.TreeNode]int),
		avgTimePerTokenPerPod:      make(map[string][]float64),
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

func (h *SlidingWindowHistogram) getSimplePrefillCost(node *prefixcacheindexer.TreeNode) float64 {
	missRate := 1.0
	if h.promptTokens[node] > 0 {
		missRate = 1.0 - (float64(h.hitTokens[node]) / float64(h.promptTokens[node]))
	}
	// Simplified prefill time calculation - you may want to use a more sophisticated model
	prefillTime := float64(node.NumTokens()) * float64(node.ContextLength()) * 0.001
	return missRate * float64(h.nodeToCount[node]) * prefillTime
}

func (h *SlidingWindowHistogram) getNodeCost(node *prefixcacheindexer.TreeNode, podName string) float64 {
	// prefillCost := h.getSimplePrefillCost(node)
	prefillCost := h.getPrefillCost(node)
	// Get median time per token for the pod
	timePerToken := 0.15 // default value
	if times, ok := h.avgTimePerTokenPerPod[podName]; ok && len(times) > 0 {
		sort.Float64s(times)
		timePerToken = times[len(times)/2] // median
	}
	outputLen := h.decodingSize[node]
	decodeCost := float64(outputLen) * timePerToken
	return prefillCost + decodeCost
}

func (h *SlidingWindowHistogram) getCurrentAllocationCostPerPod() map[string]float64 {
	costs := make(map[string]float64)
	for node := range h.histogram {
		// Iterate through all models and their pods for this node
		for _, modelPods := range node.ModelToPods {
			for podName := range modelPods {
				costs[podName] += h.getNodeCost(node, podName)
			}
		}
	}
	return costs
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

	trimmedMessage := utils.TrimMessage(message)
	klog.Infof("Trimmed message: '%s'", trimmedMessage)
	tokens, err := utils.TokenizeInputText(trimmedMessage)
	if err != nil {
		return "", err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	klog.Info("AddPrefix to the tree: ", tokens)
	node, matchedTokens, _ := p.cache.AddPrefix(tokens, model, "", false)
	var matchedPods []*v1.Pod // matchedpods will have all the pods that have the target model
	var matchedPodsNames []string
	if modelPods, ok := node.ModelToPods[model]; ok {
		klog.Infof("node.ModelToPods[model]: %v", modelPods)
		for podName := range modelPods {
			for _, pod := range readyPods {
				if pod.Name == podName { // Filter ready pods among all pods having the target model
					matchedPods = append(matchedPods, pod)
					matchedPodsNames = append(matchedPodsNames, pod.Name)
				} else {
					klog.Warning("There is a matching pod but not in ready state: ", pod.Name)
					klog.Warning("Still force to add the pod to matchedPods")
					matchedPods = append(matchedPods, pod)
					matchedPodsNames = append(matchedPodsNames, pod.Name)
				}
			}
		}
	}

	var targetPod *v1.Pod
	matchRatio := float64(len(matchedTokens)) / float64(len(tokens))
	prefix_routing_threshold := 0.5
	klog.Infof("Total tokens: %d, Matched tokens: %d, Matching ratio: %.2f, # Matched pods: %d, Matched pods: %v",
		len(tokens), len(matchedTokens), matchRatio, len(matchedPods), matchedPodsNames)

	if matchRatio > prefix_routing_threshold {
		// Prefix aware routing
		klog.Infof("Do prefix-aware routing! (matching ratio: %.2f > %.2f)", matchRatio, prefix_routing_threshold)
		var prefixMatches []prefixMatch

		// Find all prefix matches with their token lengths
		currentNode := node
		for currentNode != nil {
			if modelPods, ok := currentNode.ModelToPods[model]; ok {
				var nodePods []*v1.Pod
				for podName := range modelPods {
					for _, pod := range readyPods {
						if pod.Name == podName {
							nodePods = append(nodePods, pod)
						}
					}
				}
				if len(nodePods) > 0 {
					prefixMatches = append(prefixMatches, prefixMatch{
						node:        currentNode,
						pods:        nodePods,
						depth:       currentNode.GetDepth(),
						matchLength: currentNode.ContextLength(), // Use existing context_length
					})
					klog.Infof("Found matching pod(s) in node with key %v, total match length: %d",
						currentNode.GetKey(), currentNode.ContextLength())
				}
			}
			currentNode = currentNode.GetParent()
		}

		// Sort by actual token match length
		sort.Slice(prefixMatches, func(i, j int) bool {
			return prefixMatches[i].matchLength > prefixMatches[j].matchLength
		})

		// Choose pod from the longest match first
		if len(prefixMatches) > 0 {
			longestMatch := prefixMatches[0]
			minLoad := -1
			for _, pod := range longestMatch.pods {
				load := p.histogram.getPodLoad(pod)
				if minLoad == -1 || load < minLoad {
					minLoad = load
					targetPod = pod
				}
			}
			klog.Infof("Selected pod %s from longest matching node with match length %d", targetPod.Name, longestMatch.matchLength)
		} else {
			token_in_string, err := utils.DetokenizeText(tokens)
			matched_tokens_in_string, _ := utils.DetokenizeText(matchedTokens)
			if err != nil {
				klog.Errorf("DetokenizeTexts failed: %s, tokens: '%v', matchedTokens: '%v', model: %s", err, token_in_string, matched_tokens_in_string, model)
			} else {
				klog.Infof("No matched pods found for tokens: '%v', matchedTokens: '%v', model: %s", token_in_string, matched_tokens_in_string, model)
				klog.Infof("Go to cost model based routing!")
			}
		}
	}

	// If prefix aware routing failed or not applicable, do cost model based routing
	if targetPod == nil {
		klog.Infof("Do cost model based routing! (matching ratio: %.2f, len(matchedPods): %d)", matchRatio, len(matchedPods))
		podCosts := p.histogram.getCurrentAllocationCostPerPod() // Get current costs per pod
		minCost := math.MaxFloat64
		for _, pod := range readyPods {
			cost := podCosts[pod.Name]
			klog.Infof("Pod: %s, Cost: %f", pod.Name, cost)
			if cost < minCost {
				minCost = cost
				targetPod = pod
			}
		}
		klog.Infof("Lowest cost pod: %s", targetPod.Name)
	}

	if targetPod == nil {
		return "", fmt.Errorf("no suitable pod found")
	}

	// Update pod mapping in ALL nodes from matched node to root
	currentNode := node
	for currentNode != nil {
		if modelPods, ok := currentNode.ModelToPods[model]; !ok {
			currentNode.ModelToPods[model] = map[string]time.Time{
				targetPod.Name: time.Now(),
			}
		} else {
			modelPods[targetPod.Name] = time.Now()
		}
		currentNode = currentNode.GetParent()
	}

	// Update histogram
	p.histogram.update(time.Now(), node, node, targetPod.Name, defaultDecodingLength)

	klog.InfoS("target_pod_name", targetPod.Name, "target_pod_ip", targetPod.Status.PodIP)
	p.cache.PrettyPrint()
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

	// Update costs
	oldCost := h.perNodePrefillCost[node]
	// newCost := h.getSimplePrefillCost(node)
	newCost := h.getPrefillCost(node)
	h.currentPrefillCostPerPod[podName] -= oldCost
	h.currentPrefillCostPerPod[podName] += newCost
	h.perNodePrefillCost[node] = newCost

	h.currentDecodeLengthsPerPod[podName] += decodingLength
	h.perNodeTotalDecodeLengths[node] += decodingLength
}
