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

package prefixcacheindexer

import (
	"sync"
	"time"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type TreeNode struct {
	id            uuid.UUID
	children      map[int]*TreeNode
	parent        *TreeNode
	value         []int
	key           []int
	refCounter    []int
	load          int
	lastAccess    time.Time
	evictedPods   map[int]bool
	cachedPods    map[int]bool
	isLeaf        bool
	contextLength int
	depth         int
	ModelToPods   map[string]map[string]time.Time // model -> {podName -> lastAccessTime}
}

func (n *TreeNode) NumTokens() int {
	return len(n.value)
}

func (n *TreeNode) ContextLength() int {
	return n.contextLength
}

func NewTreeNode(numPods int) *TreeNode {
	return &TreeNode{
		id:          uuid.New(),
		children:    make(map[int]*TreeNode),
		refCounter:  make([]int, numPods),
		evictedPods: make(map[int]bool),
		cachedPods:  make(map[int]bool),
		lastAccess:  time.Now(),
		ModelToPods: make(map[string]map[string]time.Time),
	}
}

type LPRadixCache struct {
	mu            sync.RWMutex
	rootNode      *TreeNode
	numPods       int
	allocatedSize []int
	allNodes      map[uuid.UUID]*TreeNode
}

func NewLPRadixCache(numPods int) *LPRadixCache {
	cache := &LPRadixCache{
		numPods:       numPods,
		allocatedSize: make([]int, numPods),
		allNodes:      make(map[uuid.UUID]*TreeNode),
	}
	cache.reset()
	return cache
}

func (c *LPRadixCache) reset() {
	root := NewTreeNode(c.numPods)
	root.value = []int{}
	root.key = []int{}
	for i := range root.refCounter {
		root.refCounter[i] = 1
	}
	c.rootNode = root
	c.allNodes = make(map[uuid.UUID]*TreeNode)
	c.allNodes[root.id] = root
}

// matchLen returns the length of matching prefix between two slices
func matchLen(key, seq []int) int {
	i := 0
	for i < len(key) && i < len(seq) {
		if key[i] != seq[i] {
			break
		}
		i++
	}
	return i
}

// Implementation of PrefixCacheIndexer interface
func (c *LPRadixCache) MatchPrefix(inputTokens []int, model string, pods []*v1.Pod) ([]int, []int, []*v1.Pod) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	node := c.matchPrefixHelper(c.rootNode, inputTokens)
	if node == nil {
		return nil, inputTokens, pods
	}

	matchedTokens := inputTokens[:len(node.value)]
	unmatchedTokens := inputTokens[len(node.value):]

	// Filter pods based on model mapping like in hash-based impl
	var matchedPods []*v1.Pod
	if blockPods, ok := node.ModelToPods[model]; ok {
		for _, pod := range pods {
			if _, ok := blockPods[pod.Name]; ok {
				matchedPods = append(matchedPods, pod)
				klog.Info("Matched pod: ", pod.Name)
			}
		}
	}

	return matchedTokens, unmatchedTokens, matchedPods
}

// Add internal method to get node
func (c *LPRadixCache) GetNode(tokens []int) *TreeNode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.matchPrefixHelper(c.rootNode, tokens)
}

func (c *LPRadixCache) matchPrefixHelper(node *TreeNode, key []int) *TreeNode {
	node.lastAccess = time.Now()
	if len(key) == 0 {
		return node
	}
	klog.Info("Matching prefix: ", key)
	if child, ok := node.children[key[0]]; ok {
		prefixLen := matchLen(child.key, key)
		if prefixLen < len(child.key) {
			return nil
		}
		return c.matchPrefixHelper(child, key[prefixLen:])
	}
	return node
}

func (c *LPRadixCache) AddPrefix(unMatchedTokens []int, model, podName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	node := c.insertHelper(c.rootNode, unMatchedTokens, unMatchedTokens)

	// Update model-to-pod mapping
	if blockPods, ok := node.ModelToPods[model]; !ok {
		node.ModelToPods[model] = map[string]time.Time{
			podName: time.Now(),
		}
	} else {
		blockPods[podName] = time.Now()
	}
}

func (c *LPRadixCache) insertHelper(node *TreeNode, key []int, value []int) *TreeNode {
	node.lastAccess = time.Now()
	node.load++

	if len(key) == 0 {
		return node
	}

	if child, ok := node.children[key[0]]; ok {
		prefixLen := matchLen(child.key, key)
		if prefixLen == len(child.key) {
			if prefixLen == len(key) {
				child.load++
				return child
			}
			return c.insertHelper(child, key[prefixLen:], value[prefixLen:])
		}
		// Split node case
		newNode := c.splitNode(key, child, prefixLen)
		return c.insertHelper(newNode, key[prefixLen:], value[prefixLen:])
	}

	newNode := NewTreeNode(c.numPods)
	newNode.parent = node
	newNode.value = value
	newNode.key = make([]int, len(key))
	copy(newNode.key, key)
	newNode.load = 1
	newNode.depth = node.depth + 1
	newNode.contextLength = node.contextLength + len(key)

	node.children[key[0]] = newNode
	c.allNodes[newNode.id] = newNode

	return newNode
}

func (c *LPRadixCache) Evict(now time.Time) {
	// Basic time-based eviction
	c.mu.Lock()
	defer c.mu.Unlock()

	const evictionDuration = 60 * time.Minute
	for id, node := range c.allNodes {
		if now.Sub(node.lastAccess) > evictionDuration {
			// Here we can't implement proper Pod-specific eviction
			// due to interface limitations
			delete(c.allNodes, id)
			if node.parent != nil {
				delete(node.parent.children, node.key[0])
			}
		}
	}
}

func (c *LPRadixCache) splitNode(key []int, child *TreeNode, splitLen int) *TreeNode {
	newNode := NewTreeNode(c.numPods)
	newNode.children = map[int]*TreeNode{child.key[splitLen]: child}
	newNode.key = child.key[:splitLen]
	newNode.parent = child.parent
	newNode.load = child.load
	newNode.depth = child.depth
	newNode.contextLength = child.parent.contextLength + splitLen
	newNode.value = child.value[:splitLen]

	copy(newNode.refCounter, child.refCounter)
	for k, v := range child.cachedPods {
		newNode.cachedPods[k] = v
	}
	for k, v := range child.evictedPods {
		newNode.evictedPods[k] = v
	}

	child.parent = newNode
	child.key = child.key[splitLen:]
	child.value = child.value[splitLen:]
	child.depth = newNode.depth + 1

	newNode.parent.children[key[0]] = newNode
	c.allNodes[newNode.id] = newNode
	return newNode
}
