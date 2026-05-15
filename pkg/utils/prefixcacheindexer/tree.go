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
	"sort"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	evictionDuration = 5 * time.Minute // NOTE: hardcoded eviction period
)

type TreeNode struct {
	mu            sync.RWMutex // Add mutex for thread safety
	id            int
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
	contextLength int // total length from root to this node
	depth         int
	modelToPods   map[string]map[string]time.Time // model -> {podName -> lastAccessTime}
}

// GetModelToPods returns a deep copy of the model-to-pods mapping.
// Callers can safely iterate the returned maps without holding any lock.
func (n *TreeNode) GetModelToPods() map[string]map[string]time.Time {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.modelToPods == nil {
		return nil
	}
	snapshot := make(map[string]map[string]time.Time, len(n.modelToPods))
	for model, pods := range n.modelToPods {
		podsCopy := make(map[string]time.Time, len(pods))
		for k, v := range pods {
			podsCopy[k] = v
		}
		snapshot[model] = podsCopy
	}
	return snapshot
}

func (n *TreeNode) InitAndUpdateModelPod(model string, podName string, timestamp time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.modelToPods == nil {
		n.modelToPods = make(map[string]map[string]time.Time)
	}
	if n.modelToPods[model] == nil {
		n.modelToPods[model] = make(map[string]time.Time)
	}
	n.modelToPods[model][podName] = timestamp
	klog.InfoS("Updated mapping for model %s, pod %s in node(%d)", "model", model, "podName", podName, "nodeID", n.id)
}

func (n *TreeNode) GetRefCounter() []int {
	return n.refCounter
}

func (n *TreeNode) GetLoad() int {
	return n.load
}

func (n *TreeNode) GetLastAccess() time.Time {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.lastAccess
}

func (n *TreeNode) GetEvictedPods() map[int]bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	snapshot := make(map[int]bool, len(n.evictedPods))
	for k, v := range n.evictedPods {
		snapshot[k] = v
	}
	return snapshot
}

func (n *TreeNode) GetCachedPods() map[int]bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	snapshot := make(map[int]bool, len(n.cachedPods))
	for k, v := range n.cachedPods {
		snapshot[k] = v
	}
	return snapshot
}

func (n *TreeNode) GetParent() *TreeNode {
	return n.parent
}

func (n *TreeNode) GetKey() []int {
	return n.key
}

func (n *TreeNode) GetValue() []int {
	return n.value
}

func (n *TreeNode) NumTokens() int {
	return len(n.value)
}

func (n *TreeNode) ContextLength() int {
	return n.contextLength
}

func (n *TreeNode) GetDepth() int {
	return n.depth
}

func (n *TreeNode) GetID() int {
	return n.id
}

func (n *TreeNode) GetChildren() map[int]*TreeNode {
	n.mu.RLock()
	defer n.mu.RUnlock()
	snapshot := make(map[int]*TreeNode, len(n.children))
	for k, v := range n.children {
		snapshot[k] = v
	}
	return snapshot
}

func (n *TreeNode) ResetEvictedPods() {
	n.evictedPods = make(map[int]bool)
}

func (n *TreeNode) ResetCachedPods() {
	n.cachedPods = make(map[int]bool)
}

func (n *TreeNode) ResetRefCounter(numPods int) {
	n.refCounter = make([]int, numPods)
}

func (n *TreeNode) RemovePodsNotInCurrentPodSet(currentPodSet map[string]bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	podsChanged := false
	for model, podMap := range n.modelToPods {
		for podName := range podMap {
			if !currentPodSet[podName] {
				delete(podMap, podName)
				klog.InfoS("Removing pod: %s", "podName", podName, "model", model)
				podsChanged = true
			}
		}
		if len(podMap) == 0 {
			delete(n.modelToPods, model)
			klog.InfoS("Removed model from node(%d): %s", "nodeID", n.id, "model", model)
		}
	}
	return podsChanged
}

func (n *TreeNode) HasPodForModel(model, podName string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	pods, ok := n.modelToPods[model]
	if !ok {
		return false
	}
	_, exists := pods[podName]
	return exists
}

func (n *TreeNode) HasValidPods(currentPodSet map[string]bool) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, podMap := range n.modelToPods {
		for podName := range podMap {
			if currentPodSet[podName] {
				return true
			}
		}
	}
	return false
}

func (n *TreeNode) GetModelToPodCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.modelToPods)
}

// GetPodsForModel returns a copy of the pod map for the given model.
func (n *TreeNode) GetPodsForModel(model string) map[string]time.Time {
	n.mu.RLock()
	defer n.mu.RUnlock()
	pods, exists := n.modelToPods[model]
	if !exists {
		return nil
	}
	snapshot := make(map[string]time.Time, len(pods))
	for k, v := range pods {
		snapshot[k] = v
	}
	return snapshot
}

func (n *TreeNode) AddOrUpdatePodForModel(model string, podName string, timestamp time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.modelToPods == nil {
		n.modelToPods = make(map[string]map[string]time.Time)
	}
	if _, exists := n.modelToPods[model]; !exists {
		n.modelToPods[model] = make(map[string]time.Time)
	}
	n.modelToPods[model][podName] = timestamp
	klog.V(4).InfoS("Added/Updated pod %s for model %s in node(%d)", "podName", podName, "model", model, "nodeID", n.id)
}

func (n *TreeNode) RemovePodsNotInSet(currentPodSet map[string]bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	podsChanged := false
	deleted_pods := make([]string, 0)
	for model, podMap := range n.modelToPods {
		for podName := range podMap {
			if podName == "" {
				klog.Warningf("Found empty pod name in node(%d) model(%s)", n.id, model)
			}
			if !currentPodSet[podName] {
				delete(podMap, podName)
				klog.InfoS("Removing pod: %s", "podName", podName)
				podsChanged = true
				deleted_pods = append(deleted_pods, podName)
			}
		}
		if len(podMap) == 0 {
			delete(n.modelToPods, model)
		}
	}
	if podsChanged {
		klog.InfoS("Removed pods from node(%d): %v", "nodeID", n.id, "deletedPods", deleted_pods)
	}
	return podsChanged
}

func (c *LPRadixCache) NewTreeNode(numPods int, parent *TreeNode, key []int, value []int) *TreeNode {
	// Create the node with initialized maps and slices
	node := &TreeNode{
		id:          c.nextNodeID,
		children:    make(map[int]*TreeNode),
		parent:      parent,
		key:         make([]int, len(key)),   // Allocate space for key
		value:       make([]int, len(value)), // Allocate space for value (using len(value), not len(key))
		refCounter:  make([]int, numPods),
		load:        1,
		lastAccess:  time.Now(),
		evictedPods: make(map[int]bool),
		cachedPods:  make(map[int]bool),
		modelToPods: make(map[string]map[string]time.Time),
		// Pods:          make(map[string]time.Time),
		depth:         0,
		contextLength: 0,
	}

	// Increment node ID for next creation
	klog.V(5).InfoS("Created a new node(%d) with key: %v and value: %v", "nodeID", node.id, "key", key, "value", value)
	c.nextNodeID++

	// Set depth and context length based on parent
	if parent != nil {
		node.depth = parent.depth + 1
		node.contextLength = parent.contextLength + len(key)
	}

	// Copy key and value slices
	if len(key) > 0 {
		copy(node.key, key)
	}
	if len(value) > 0 {
		copy(node.value, value)
	}

	return node
}

func (c *LPRadixCache) PrettyPrint() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	c.prettyPrintHelper(c.rootNode, "", true)
}

// GetAllNodes returns a snapshot copy of the internal node map.
// Callers can safely iterate the returned map without holding any lock.
func (c *LPRadixCache) GetAllNodes() map[int]*TreeNode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot := make(map[int]*TreeNode, len(c.allNodes))
	for k, v := range c.allNodes {
		snapshot[k] = v
	}
	return snapshot
}

func (c *LPRadixCache) GetAllPodsInNode(node *TreeNode) []string {
	node.mu.RLock()
	defer node.mu.RUnlock()
	allPodsInNode := make([]string, 0)
	for _, pods := range node.modelToPods {
		for podName := range pods {
			allPodsInNode = append(allPodsInNode, podName)
		}
	}
	return allPodsInNode
}

// This can be used for debugging purpose
func (c *LPRadixCache) prettyPrintHelper(node *TreeNode, prefix string, isLast bool) {
	if node == nil {
		return
	}
	marker := "└── "
	if !isLast {
		marker = "├── "
	}
	childPrefix := prefix + "    "
	if !isLast {
		childPrefix = prefix + "│   "
	}
	allPodsInNode := c.GetAllPodsInNode(node)
	klog.V(5).Infof("%s%s[Node: %d, Load: %d, Pods: %v, Depth: %d]", prefix, marker, node.id, node.load, allPodsInNode, node.depth)
	childKeys := make([]int, 0, len(node.children))
	for k := range node.children {
		childKeys = append(childKeys, k)
	}
	sort.Ints(childKeys)
	for i, key := range childKeys {
		isLastChild := i == len(childKeys)-1
		c.prettyPrintHelper(node.children[key], childPrefix, isLastChild)
	}
}

type LPRadixCache struct {
	mu       sync.RWMutex
	rootNode *TreeNode
	numPods  int
	// allocatedSize []int // not being used. if it is not going to be used, it will be removed permanently.
	allNodes   map[int]*TreeNode
	nextNodeID int
	startTime  time.Time
}

func NewLPRadixCache(numPods int) *LPRadixCache {
	cache := &LPRadixCache{
		numPods: numPods,
		// allocatedSize: make([]int, numPods), // not being used. if it is not going to be used, it will be removed permanently.
		allNodes:   make(map[int]*TreeNode),
		nextNodeID: 0,
		startTime:  time.Now(),
	}
	cache.reset()
	return cache
}

func (c *LPRadixCache) reset() {
	root := c.NewTreeNode(c.numPods, nil, []int{}, []int{})
	for i := range root.refCounter {
		root.refCounter[i] = 1
	}
	c.rootNode = root
	c.allNodes = make(map[int]*TreeNode)
	c.allNodes[root.id] = root
}

// GetNode adds internal method to get node
func (c *LPRadixCache) GetNode(tokens []int) *TreeNode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	node, _ := c.matchPrefixHelper(c.rootNode, tokens)
	return node
}

func (c *LPRadixCache) MatchPrefix(inputTokens []int, model string, pods []*v1.Pod) ([]int, []int, []*v1.Pod) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	node, matchedTokens := c.matchPrefixHelper(c.rootNode, inputTokens)
	if node == nil || len(matchedTokens) == 0 {
		return []int{}, inputTokens, nil
	}

	unmatchedTokens := inputTokens[len(matchedTokens):]

	// Find matching pods. Copy the inner map under lock to avoid concurrent
	// map read/write with AddOrUpdatePodForModel from other goroutines.
	var matchedPods []*v1.Pod
	node.mu.RLock()
	var podsCopy map[string]time.Time
	if innerMap, ok := node.modelToPods[model]; ok {
		podsCopy = make(map[string]time.Time, len(innerMap))
		for k, v := range innerMap {
			podsCopy[k] = v
		}
	}
	node.mu.RUnlock()
	if podsCopy != nil {
		for _, pod := range pods {
			if _, ok := podsCopy[pod.Name]; ok {
				if matchedPods == nil {
					matchedPods = make([]*v1.Pod, 0, len(pods))
				}
				matchedPods = append(matchedPods, pod)
				klog.InfoS("Matched pod for node(%d): %s", "nodeID", node.id, "podName", pod.Name)
			}
		}
	}
	klog.InfoS("MatchPrefix - node(%d) key: %v, matched tokens: %v, model pods: %v", "nodeID", node.id, "key", node.key, "matchedTokens", matchedTokens, "modelToPods", podsCopy)
	return matchedTokens, unmatchedTokens, matchedPods
}

func (c *LPRadixCache) MatchPrefixNodeReadOnly(inputTokens []int) (*TreeNode, []int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.matchPrefixHelperReadOnly(c.rootNode, inputTokens)
}

// This is being used by GetNode
// Uses iterative traversal to avoid stack overflow with deep trees (e.g. 30K+ token inputs).
func (c *LPRadixCache) matchPrefixHelper(node *TreeNode, tokens []int) (*TreeNode, []int) {
	if len(tokens) == 0 {
		return node, nil
	}

	// One timestamp for the whole match path so all touched nodes share a
	// consistent LRU time and we avoid calling time.Now() per hop.
	now := time.Now()
	touchLastAccess := func(n *TreeNode) {
		n.mu.Lock()
		n.lastAccess = now
		n.mu.Unlock()
	}

	bestNode := node
	totalMatched := 0
	current := node

	for totalMatched < len(tokens) {
		touchLastAccess(current)
		remaining := tokens[totalMatched:]
		child, ok := current.children[remaining[0]]
		if !ok {
			break
		}
		prefixLen := matchLen(child.key, remaining)
		if prefixLen == 0 {
			break
		}

		if prefixLen == len(child.key) {
			// Complete match with child's key
			totalMatched += prefixLen
			bestNode = child
			if totalMatched == len(tokens) {
				// Final matched node is child; next iteration would not run.
				touchLastAccess(child)
				return child, tokens[:totalMatched]
			}
			current = child
			continue
		}
		// Partial match with child's key — return child as best node; update it.
		totalMatched += prefixLen
		touchLastAccess(child)
		return child, tokens[:totalMatched]
	}

	if totalMatched > 0 {
		return bestNode, tokens[:totalMatched]
	}
	return node, nil
}

func (c *LPRadixCache) matchPrefixHelperReadOnly(node *TreeNode, tokens []int) (*TreeNode, []int) {
	if len(tokens) == 0 {
		return node, nil
	}

	bestNode := node
	totalMatched := 0
	current := node

	for totalMatched < len(tokens) {
		remaining := tokens[totalMatched:]
		child, ok := current.children[remaining[0]]
		if !ok {
			break
		}
		prefixLen := matchLen(child.key, remaining)
		if prefixLen == 0 {
			break
		}

		if prefixLen == len(child.key) {
			totalMatched += prefixLen
			bestNode = child
			if totalMatched == len(tokens) {
				return child, tokens[:totalMatched]
			}
			current = child
			continue
		}

		totalMatched += prefixLen
		return child, tokens[:totalMatched]
	}

	if totalMatched > 0 {
		return bestNode, tokens[:totalMatched]
	}
	return node, nil
}

func (c *LPRadixCache) AddPrefix(tokens []int, model string, podName string) (*TreeNode, []int, []int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Do insertion first
	node, matchedTokens, unmatchedTokens := c.insertHelper(c.rootNode, tokens, tokens)
	if node != nil && podName != "" {
		node.InitAndUpdateModelPod(model, podName, time.Now())
		current := node
		for current.parent != nil {
			current.parent.InitAndUpdateModelPod(model, podName, time.Now())
			current = current.parent
		}
		klog.V(5).InfoS("Updated mapping for model %s, pod %s in node(%d)", "model", model, "podName", podName, "nodeID", node.id, "key", node.key)

	}
	return node, matchedTokens, unmatchedTokens
}

// insertHelper inserts tokens into the radix tree iteratively.
// Uses iterative traversal to avoid stack overflow with deep trees (e.g. 30K+ token inputs).
func (c *LPRadixCache) insertHelper(node *TreeNode, key []int, value []int) (*TreeNode, []int, []int) {
	originalKey := key
	originalValue := value
	offset := 0
	current := node

	for {
		current.lastAccess = time.Now()
		current.load++
		timePassed := current.lastAccess.Sub(c.startTime).Seconds()
		klog.V(5).InfoS("Updated node last access", "nodeID", current.id, "timePassed", timePassed)

		remaining := originalKey[offset:]
		if len(remaining) == 0 {
			return current, nil, nil
		}

		// Check if one of the children matches the prefix
		child, ok := current.children[remaining[0]]
		if !ok {
			// No matching child, create new node
			klog.V(5).InfoS("No child matches any of the prefix. Create a new tree node")
			remainingValue := originalValue[offset:]
			newNode := c.NewTreeNode(c.numPods, current, remaining, remainingValue)
			current.children[remaining[0]] = newNode
			c.allNodes[newNode.id] = newNode
			if offset > 0 {
				return newNode, originalKey[:offset], originalKey[offset:]
			}
			return newNode, nil, originalKey
		}

		prefixLen := matchLen(child.key, remaining)

		// Case 1: Complete match with child's key
		if prefixLen == len(child.key) {
			if prefixLen == len(remaining) {
				klog.V(5).InfoS("Entire input tokens match the child node", "childNodeID", child.id)
				child.lastAccess = time.Now()
				child.load++
				return child, originalKey, nil
			}
			// Continue deeper iteratively
			klog.V(5).InfoS("Partial tokens match child node, continue deeper", "childNodeID", child.id)
			offset += prefixLen
			current = child
			continue
		}

		// Case 2: Partial match, need to split
		newNode := c.splitNode(remaining, child, prefixLen)
		offset += prefixLen
		if offset == len(originalKey) {
			return newNode, originalKey, nil
		}
		// Continue from the split node
		current = newNode
	}
}

func (c *LPRadixCache) doesExceededTTL(node *TreeNode, now time.Time) bool {
	timeSinceLastAccess := now.Sub(node.lastAccess)
	if timeSinceLastAccess > evictionDuration {
		klog.InfoS("Node(%d) exceeded TTL(%ds), time since last access: %.2f seconds", "nodeID", node.id, "evictionDurationSeconds", int(evictionDuration.Seconds()), "timeSinceLastAccessSeconds", timeSinceLastAccess.Seconds())
		return true
	}
	return false
}

func (c *LPRadixCache) Evict(now time.Time) []*TreeNode {
	c.mu.Lock()
	defer c.mu.Unlock()
	var nodesToEvict []*TreeNode
	for _, node := range c.allNodes {
		if node != c.rootNode {
			if c.doesExceededTTL(node, now) {
				if collected := c.collectNodeAndChildren(node); collected != nil {
					nodesToEvict = append(nodesToEvict, collected...)
				}
			}
		}
	}
	// Actually perform the eviction
	for _, node := range nodesToEvict {
		c.evictNode(node)
	}
	if len(nodesToEvict) > 0 {
		klog.V(4).InfoS("Evicted %d nodes", "nodeCount", len(nodesToEvict))
	}
	return nodesToEvict
}

func (c *LPRadixCache) collectNodeAndChildren(node *TreeNode) []*TreeNode {
	if node == c.rootNode {
		return nil
	}
	nodes := make([]*TreeNode, 0)
	stack := []*TreeNode{node}
	// BFS
	for len(stack) > 0 {
		current := stack[len(stack)-1] // top
		stack = stack[:len(stack)-1]   // pop
		nodes = append(nodes, current) // collect
		for _, child := range current.children {
			stack = append(stack, child)
		}
	}
	return nodes
}

// Fix for evictNode method in tree.go
func (c *LPRadixCache) evictNode(node *TreeNode) {
	if node == c.rootNode {
		return
	}

	// Snapshot the node's pod mappings before traversing parents, to avoid
	// reading current.modelToPods without current.mu (races with RemovePodsNotInSet).
	modelToPodsSnapshot := node.GetModelToPods()

	// Clean up pod mappings in parent nodes.
	// Acquire parent.mu to prevent concurrent map access from GetModelToPods callers.
	for parent := node.parent; parent != nil; parent = parent.parent {
		parent.mu.Lock()
		for model, pods := range modelToPodsSnapshot {
			if parentPods, ok := parent.modelToPods[model]; ok {
				for podName := range pods {
					delete(parentPods, podName)
				}
				if len(parentPods) == 0 {
					delete(parent.modelToPods, model)
				}
			}
		}
		parent.mu.Unlock()
	}

	// Remove node from parent's children.
	// Acquire parent.mu to prevent concurrent map access from GetChildren callers.
	if node.parent != nil {
		node.parent.mu.Lock()
		delete(node.parent.children, node.key[0])
		node.parent.mu.Unlock()
	}

	// Remove from allNodes map
	delete(c.allNodes, node.id)
	klog.InfoS("Evict node(%d)", "nodeID", node.id)

	// Clean up the node's references
	node.parent = nil
	node.children = nil
	node.modelToPods = nil
	node.evictedPods = nil
	node.cachedPods = nil
	node.value = nil
	node.key = nil
	node.refCounter = nil
}

func (c *LPRadixCache) splitNode(key []int, child *TreeNode, splitLen int) *TreeNode {
	// Create new node with split portions
	newNode := c.NewTreeNode(c.numPods, child.parent, child.key[:splitLen], child.value[:splitLen])

	// Update parent's reference to point to new node
	child.parent.children[key[0]] = newNode

	// Update child node
	remainingKey := make([]int, len(child.key)-splitLen)
	copy(remainingKey, child.key[splitLen:])
	child.key = remainingKey

	remainingValue := make([]int, len(child.value)-splitLen)
	copy(remainingValue, child.value[splitLen:])
	child.value = remainingValue

	// Update relationships
	child.parent = newNode
	newNode.children = make(map[int]*TreeNode)
	if len(child.key) > 0 {
		newNode.children[child.key[0]] = child
	}

	// Copy metadata
	newNode.load = child.load
	copy(newNode.refCounter, child.refCounter)

	// Copy pod mappings
	for k, v := range child.cachedPods {
		newNode.cachedPods[k] = v
	}
	for k, v := range child.evictedPods {
		newNode.evictedPods[k] = v
	}

	// Copy ModelToPods mapping to both nodes
	newNode.modelToPods = make(map[string]map[string]time.Time)
	for model, pods := range child.modelToPods {
		// Copy to new node (prefix node)
		newNode.modelToPods[model] = make(map[string]time.Time)
		for podName, lastAccess := range pods {
			newNode.modelToPods[model][podName] = lastAccess
		}
	}
	c.allNodes[newNode.id] = newNode
	return newNode
}

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
