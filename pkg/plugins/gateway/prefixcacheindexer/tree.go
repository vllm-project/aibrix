package prefixcacheindexer

import (
	"sync"
	"time"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
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
	evictedGPUs   map[int]bool
	cachedGPUs    map[int]bool
	isLeaf        bool
	contextLength int
	depth         int
	modelToPods   map[string]map[string]time.Time // model -> {pod -> lastAccessTime}
}

func NewTreeNode(numGPUs int) *TreeNode {
	return &TreeNode{
		id:          uuid.New(),
		children:    make(map[int]*TreeNode),
		refCounter:  make([]int, numGPUs),
		evictedGPUs: make(map[int]bool),
		cachedGPUs:  make(map[int]bool),
		lastAccess:  time.Now(),
		modelToPods: make(map[string]map[string]time.Time),
	}
}

type LPRadixCache struct {
	mu            sync.RWMutex
	rootNode      *TreeNode
	numGPUs       int
	allocatedSize []int
	allNodes      map[uuid.UUID]*TreeNode
}

func NewLPRadixCache(numGPUs int) *LPRadixCache {
	cache := &LPRadixCache{
		numGPUs:       numGPUs,
		allocatedSize: make([]int, numGPUs),
		allNodes:      make(map[uuid.UUID]*TreeNode),
	}
	cache.reset()
	return cache
}

func (c *LPRadixCache) reset() {
	root := NewTreeNode(c.numGPUs)
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
	if blockPods, ok := node.modelToPods[model]; ok {
		for _, pod := range pods {
			if _, ok := blockPods[pod.Name]; ok {
				matchedPods = append(matchedPods, pod)
			}
		}
	}

	return matchedTokens, unmatchedTokens, matchedPods
}

func (c *LPRadixCache) matchPrefixHelper(node *TreeNode, key []int) *TreeNode {
	node.lastAccess = time.Now()
	if len(key) == 0 {
		return node
	}

	if child, ok := node.children[key[0]]; ok {
		prefixLen := matchLen(child.key, key)
		if prefixLen < len(child.key) {
			return nil
		}
		return c.matchPrefixHelper(child, key[prefixLen:])
	}
	return node
}

func (c *LPRadixCache) AddPrefix(unMatchedTokens []int, model, pod string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	node := c.insertHelper(c.rootNode, unMatchedTokens, unMatchedTokens)

	// Update model-to-pod mapping
	if blockPods, ok := node.modelToPods[model]; !ok {
		node.modelToPods[model] = map[string]time.Time{
			pod: time.Now(),
		}
	} else {
		blockPods[pod] = time.Now()
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

	newNode := NewTreeNode(c.numGPUs)
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
			// Here we can't implement proper GPU-specific eviction
			// due to interface limitations
			delete(c.allNodes, id)
			if node.parent != nil {
				delete(node.parent.children, node.key[0])
			}
		}
	}
}

func (c *LPRadixCache) splitNode(key []int, child *TreeNode, splitLen int) *TreeNode {
	newNode := NewTreeNode(c.numGPUs)
	newNode.children = map[int]*TreeNode{child.key[splitLen]: child}
	newNode.key = child.key[:splitLen]
	newNode.parent = child.parent
	newNode.load = child.load
	newNode.depth = child.depth
	newNode.contextLength = child.parent.contextLength + splitLen
	newNode.value = child.value[:splitLen]

	copy(newNode.refCounter, child.refCounter)
	for k, v := range child.cachedGPUs {
		newNode.cachedGPUs[k] = v
	}
	for k, v := range child.evictedGPUs {
		newNode.evictedGPUs[k] = v
	}

	child.parent = newNode
	child.key = child.key[splitLen:]
	child.value = child.value[splitLen:]
	child.depth = newNode.depth + 1

	newNode.parent.children[key[0]] = newNode
	c.allNodes[newNode.id] = newNode
	return newNode
}
