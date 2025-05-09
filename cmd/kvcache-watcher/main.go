/*
Copyright 2025 The Aibrix Team.

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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/vllm-project/aibrix/pkg/utils"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/buraksezer/consistent"
	"github.com/cespare/xxhash/v2"
	"github.com/redis/go-redis/v9"
)

const KVCacheLabelKeyIdentifier = "kvcache.orchestration.aibrix.ai/name"
const KVCacheLabelKeyRole = "kvcache.orchestration.aibrix.ai/role"
const KVCacheLabelValueRoleCache = "cache"
const HPKVRedisNodeMemberKey = "hpkv_cluster_metadata"
const InfiniStoreRedisNodeMemberKey = "kvcache_nodes"

const networkStatusAnnotation = "k8s.volcengine.com/network-status"

var (
	config    *rest.Config
	clientset *kubernetes.Clientset
)

var (
	kvCacheBackend                    string
	kvCacheWatchNS                    string
	kvCacheWatchClusterId             string
	kvCacheServerRDMAPort             int
	kvCacheServerAdminPort            int
	consistentHashingTotalSlots       int
	consistentHashingVirtualNodeCount int
)

var (
	metadataVersionGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kvcache_cluster_version",
		Help: "Current version of the KVCache cluster metadata.",
	}, []string{"name"})

	metadataUpgradeCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kvcache_metadata_version_upgrade_counter",
		Help: "Number of times the metadata version has been upgraded.",
	}, []string{"name"})

	metadataUpdateSkippedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kvcache_metadata_update_skipped",
		Help: "Number of times there's no valid pods and skip the metadata updates.",
	}, []string{"name"})

	metadataUpdateFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kvcache_metadata_update_failures_total",
		Help: "Total number of failures when updating cluster metadata to Redis.",
	}, []string{"name"})

	podStatusPhaseGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kvcache_pod_status_phase_total",
			Help: "Number of KVCache pods per status phase.",
		},
		[]string{"name", "phase"},
	)
)

type SlotRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type NodeInfo struct {
	Name  string      `json:"name"`
	Addr  string      `json:"addr"`
	Port  int         `json:"port"`
	Slots []SlotRange `json:"slots"`
}

type ClusterNodes struct {
	Nodes   []NodeInfo `json:"nodes"`
	Version int64      `json:"version"`
}

type hasher struct{}

func (h hasher) Sum64(data []byte) uint64 {
	return xxhash.Sum64(data)
}

const vnodeDelimiter = "-vnode-"

type VirtualNode struct {
	name string
}

func NewVirtualNode(podName string, index int) VirtualNode {
	return VirtualNode{
		name: fmt.Sprintf("%s%s%d", podName, vnodeDelimiter, index),
	}
}

// ParseVirtualNode parses a virtual node name like "podname-vnode-42" into a VirtualNode struct.
// Returns error if the format is invalid.
func ParseVirtualNode(name string) (VirtualNode, error) {
	if !strings.Contains(name, vnodeDelimiter) {
		return VirtualNode{}, fmt.Errorf("invalid virtual node format: %s", name)
	}
	parts := strings.Split(name, vnodeDelimiter)
	if len(parts) != 2 {
		return VirtualNode{}, fmt.Errorf("failed to split virtual node name: %s", name)
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return VirtualNode{}, fmt.Errorf("invalid virtual index in name: %s", name)
	}
	return VirtualNode{name: name}, nil
}

func (v VirtualNode) String() string {
	return v.name
}

// PodName returns the physical pod name from the virtual node name.
// Example: kvcache-sts-0-vnode-42 → kvcache-sts-0
func (v VirtualNode) PodName() string {
	parts := strings.Split(v.name, vnodeDelimiter)
	if len(parts) == 2 {
		return parts[0]
	}
	return v.name // fallback: treat whole as podName
}

// Index returns the virtual node index.
// Example: kvcache-sts-0-vnode-42 → 42
func (v VirtualNode) Index() int {
	parts := strings.Split(v.name, vnodeDelimiter)
	if len(parts) == 2 {
		if index, err := strconv.Atoi(parts[1]); err == nil {
			return index
		}
	}
	return -1
}

type KVCacheBackend interface {
	GetRedisKey() string
	ExtractIP(ctx context.Context, pod *corev1.Pod) (string, error)
}

type HPKVBackend struct{}

func (b HPKVBackend) GetRedisKey() string {
	return HPKVRedisNodeMemberKey
}

func (b HPKVBackend) ExtractIP(ctx context.Context, pod *corev1.Pod) (string, error) {
	return GetRDMAIP(ctx, pod)
}

type InfiniStoreBackend struct{}

func (b InfiniStoreBackend) GetRedisKey() string {
	return InfiniStoreRedisNodeMemberKey
}

func (b InfiniStoreBackend) ExtractIP(ctx context.Context, pod *corev1.Pod) (string, error) {
	return pod.Status.PodIP, nil
}

func NewKVCacheBackend(backend string) KVCacheBackend {
	switch backend {
	case "infinistore":
		return InfiniStoreBackend{}
	case "hpkv":
		fallthrough
	default:
		return HPKVBackend{}
	}
}

func main() {
	ctx := context.Background()
	parseFlags()

	// Register Prometheus metrics
	prometheus.MustRegister(metadataVersionGauge)
	prometheus.MustRegister(metadataUpgradeCounter)
	prometheus.MustRegister(metadataUpdateSkippedCounter)
	prometheus.MustRegister(metadataUpdateFailures)
	prometheus.MustRegister(podStatusPhaseGauge)

	// Expose /metrics endpoint
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		klog.Info("Starting Prometheus metrics server on :8000")
		if err := http.ListenAndServe(":8000", nil); err != nil {
			klog.Fatalf("Failed to start Prometheus HTTP server: %v", err)
		}
	}()

	// read environment variables from env
	redisAddr := os.Getenv("REDIS_ADDR")
	redisPass := os.Getenv("REDIS_PASSWORD")
	redisDatabase := utils.LoadEnvInt("REDIS_DATABASE", 0)
	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPass,
		DB:       redisDatabase,
	})

	// create Kubernetes client
	var kubeConfig string
	var err error
	kFlag := flag.Lookup("kubeconfig")
	if kFlag != nil {
		kubeConfig = kFlag.Value.String()
	} else {
		klog.Warning("kubeconfig flag not defined")
	}

	if kubeConfig == "" {
		klog.Info("using in-cluster configuration")
		config, err = rest.InClusterConfig()
	} else {
		klog.Infof("using configuration from '%s'", kubeConfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	}
	if err != nil {
		klog.Fatalf("Failed to read kube configs: %v", err)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create clientset: %v", err)
	}

	// Create informer factory
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 15*time.Second,
		informers.WithNamespace(kvCacheWatchNS),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			if kvCacheWatchClusterId != "" {
				kvClusterLabel := fmt.Sprintf("%s=%s", KVCacheLabelKeyIdentifier, kvCacheWatchClusterId)
				kvClusterRoleLabel := fmt.Sprintf("%s=%s", KVCacheLabelKeyRole, KVCacheLabelValueRoleCache)
				opts.LabelSelector = fmt.Sprintf("%s,%s", kvClusterLabel, kvClusterRoleLabel)
			}
		}),
	)

	// TODO: moved to NewTypedRateLimitingQueue introduced in controller-runtime v0.15.x
	//nolint:staticcheck // SA1004 ignore this! Compatibility: will migrate to NewTypedRateLimitingQueue later
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	podInformer := factory.Core().V1().Pods().Informer()
	_, err = podInformer.AddEventHandler(&cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			queue.Add(kvCacheWatchClusterId)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			queue.Add(kvCacheWatchClusterId)
		},
		DeleteFunc: func(obj interface{}) {
			queue.Add(kvCacheWatchClusterId)
		},
	})
	if err != nil {
		return
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	klog.Info("Starting pod registration watcher...")
	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

	backendImpl := NewKVCacheBackend(kvCacheBackend)

	// Start queue worker in goroutine
	go func() {
		for {
			raw, shutdown := queue.Get()
			if shutdown {
				return
			}
			key := raw.(string)
			func(key string) {
				defer queue.Done(key)

				if err := syncPods(ctx, rdb, podInformer, key, backendImpl); err != nil {
					klog.Errorf("syncPods failed for %s: %v, retrying...", key, err)
					queue.AddRateLimited(key)
				} else {
					queue.Forget(key)
				}
			}(key)
		}
	}()

	<-stopCh
}

func parseFlags() {
	flag.StringVar(
		&kvCacheBackend,
		"kvcache-backend",
		"hpkv",
		"KV backend implementation to use. Supported: 'hpkv', 'infinistore'.",
	)

	flag.StringVar(
		&kvCacheWatchNS,
		"kvcache-watch-namespace",
		utils.LoadEnv("AIBRIX_KVCACHE_WATCH_NAMESPACE", "default"),
		"Kubernetes namespace to watch for KVCache pods.",
	)

	flag.StringVar(
		&kvCacheWatchClusterId,
		"kvcache-watch-cluster-id",
		os.Getenv("AIBRIX_KVCACHE_WATCH_CLUSTER"),
		"Value of the 'kvcache.orchestration.aibrix.ai/name' label to identify the KV cache cluster.",
	)

	flag.IntVar(
		&kvCacheServerRDMAPort,
		"kvcache-server-rdma-port",
		utils.LoadEnvInt("AIBRIX_KVCACHE_RDMA_PORT", 18512),
		"RDMA service port used by the KVCache data servers.",
	)

	flag.IntVar(
		&kvCacheServerAdminPort,
		"kvcache-server-admin-port",
		utils.LoadEnvInt("AIBRIX_KVCACHE_ADMIN_PORT", 9100),
		"Admin port used for control kv cache server.",
	)

	flag.IntVar(
		&consistentHashingTotalSlots,
		"consistent-hashing-total-slots",
		4096,
		"Total number of slots in the consistent hashing ring.",
	)

	flag.IntVar(
		&consistentHashingVirtualNodeCount,
		"consistent-hashing-virtual-node-count",
		100,
		"Number of virtual nodes per physical KVCache pod for consistent hashing.",
	)

	flag.Parse()

	klog.Infof("=== Parsed Flags ===")
	flag.VisitAll(func(f *flag.Flag) {
		klog.Infof("%s: %s", f.Name, f.Value)
	})
}

func syncPods(
	ctx context.Context,
	rdb *redis.Client,
	informer cache.SharedIndexInformer,
	kvClusterId string,
	kvb KVCacheBackend,
) error {
	pods := informer.GetStore().List()
	klog.Infof("%d pods Found in kvcache cluster %s", len(pods), kvClusterId)

	validPods := make([]corev1.Pod, 0)
	phaseCounts := map[corev1.PodPhase]int{}
	for _, obj := range pods {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			klog.Warningf("unexpected object type in informer cache: %T", obj)
			continue
		}

		phaseCounts[pod.Status.Phase]++

		// skip pods not belong to current kvcache cluster and kvcache server
		if _, ok := pod.Labels[KVCacheLabelKeyIdentifier]; !ok {
			continue
		}

		if value, ok := pod.Labels[KVCacheLabelKeyRole]; !ok || value != KVCacheLabelValueRoleCache {
			continue
		}

		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			validPods = append(validPods, *pod)
		}
	}

	// export pod phase counts
	podStatusPhaseGauge.Reset()
	for phase, count := range phaseCounts {
		podStatusPhaseGauge.WithLabelValues(kvClusterId, string(phase)).Set(float64(count))
	}

	if len(validPods) == 0 {
		klog.Warningf("No valid KVCache pods found after filtering for cluster %v", kvClusterId)
		metadataUpdateSkippedCounter.WithLabelValues(kvClusterId).Inc()
		return nil
	}

	nodeSlots := calculateSlotDistribution(validPods, consistentHashingTotalSlots, consistentHashingVirtualNodeCount)
	currentNodes := make([]NodeInfo, 0)
	for _, pod := range validPods {
		ip, err := kvb.ExtractIP(ctx, &pod)
		if err != nil {
			klog.ErrorS(err, "Failed to get RDMA IP for pod", "pod", pod.Name)
			continue
		}

		currentNodes = append(currentNodes, NodeInfo{
			Name:  pod.Name,
			Addr:  ip,
			Port:  kvCacheServerRDMAPort,
			Slots: mergeSlots(nodeSlots[pod.Name], consistentHashingTotalSlots),
		})
	}

	redisKey := kvb.GetRedisKey()

	// get existing nodes
	val, err := rdb.Get(ctx, redisKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("failed to get existing data from redis %v", err)
	}
	existingClusterNodes := ClusterNodes{}
	_ = json.Unmarshal([]byte(val), &existingClusterNodes)
	klog.Infof("redis get result: key %s, value %s", redisKey, val)

	needUpdate := !isNodeListEqual(currentNodes, existingClusterNodes.Nodes)
	if !needUpdate {
		klog.Infof("Node list unchanged, skipping update, current version: %d", existingClusterNodes.Version)
		metadataVersionGauge.WithLabelValues(kvClusterId).Set(float64(existingClusterNodes.Version))
		return nil
	}

	newVersion := int64(1)
	if val != "" {
		newVersion = existingClusterNodes.Version + 1
		metadataUpgradeCounter.WithLabelValues(kvClusterId).Inc()
	}

	newData := ClusterNodes{
		Nodes:   currentNodes,
		Version: newVersion,
	}

	jsonData, err := json.Marshal(newData)
	if err != nil {
		return fmt.Errorf("failed to marshal nodes data: %v", err)
	}

	// write to redis using pipeline
	pipe := rdb.TxPipeline()
	pipe.Set(ctx, redisKey, jsonData, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		metadataUpdateFailures.WithLabelValues(kvClusterId).Inc()
		return fmt.Errorf("redis transaction failed: %v", err)
	}

	metadataVersionGauge.WithLabelValues(kvClusterId).Set(float64(newVersion))
	klog.InfoS("Successfully updated cluster nodes", "version", newVersion, "nodeCount", len(currentNodes))
	return nil
}

func calculateSlotDistribution(pods []corev1.Pod, totalSlots int, virtualNodeCount int) map[string][]int {
	// init hash ring
	cfg := consistent.Config{
		PartitionCount:    totalSlots,
		ReplicationFactor: virtualNodeCount,
		Hasher:            hasher{},
	}
	c := consistent.New(nil, cfg)

	// add nodes to consistent hash ring
	nodeMap := make(map[string]struct{})
	for _, pod := range pods {
		for i := 0; i < virtualNodeCount; i++ {
			member := NewVirtualNode(pod.Name, i)
			c.Add(member)
		}
		nodeMap[pod.Name] = struct{}{}
	}

	// link slots to node
	slotDistribution := make(map[string][]int)
	for slot := 0; slot < totalSlots; slot++ {
		key := []byte(strconv.Itoa(slot))
		member := c.LocateKey(key)
		if member != nil {
			vn, err := ParseVirtualNode(member.String())
			if err != nil {
				klog.Infof("Invalid virtual node %s found, skip it", member.String())
			}
			nodeName := vn.PodName()
			if _, exists := nodeMap[nodeName]; exists {
				slotDistribution[nodeName] = append(slotDistribution[nodeName], slot)
			}
		}
	}

	return slotDistribution
}

// GetRDMAIP tries to get RDMA IP from annotation, falls back to exec inside the pod.
func GetRDMAIP(ctx context.Context, pod *corev1.Pod) (string, error) {
	// TODO: make this dynamic
	ifName := "eth1"

	if ip, ok := getRDMAIPFromAnnotation(pod, ifName); ok {
		return ip, nil
	}

	return getRDMAIPFromExec(ctx, pod, ifName)
}

type networkStatusEntry struct {
	CNIName    string `json:"cniName"`
	DeviceInfo struct {
		IfName string   `json:"ifName"`
		IPs    []string `json:"ips"`
		MAC    string   `json:"mac"`
	} `json:"deviceInfo"`
}

// getRDMAIPFromAnnotation attempts to extract RDMA IP from the annotation
func getRDMAIPFromAnnotation(pod *corev1.Pod, ifName string) (string, bool) {
	raw := pod.Annotations[networkStatusAnnotation]
	if raw == "" {
		return "", false
	}

	var entries []networkStatusEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return "", false
	}

	for _, entry := range entries {
		if entry.CNIName == "rdma" && entry.DeviceInfo.IfName == ifName && len(entry.DeviceInfo.IPs) > 0 {
			ip := strings.TrimSpace(entry.DeviceInfo.IPs[0])
			if net.ParseIP(ip) != nil {
				return ip, true
			}
		}
	}
	return "", false
}

// getRDMAIPFromExec falls back to using `kubectl exec` inside the pod to fetch IP
func getRDMAIPFromExec(ctx context.Context, pod *corev1.Pod, ifName string) (string, error) {
	// 1. prepare exec requests
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("exec").
		Param("container", "kvcache-server").
		Param("stdin", "false").
		Param("stdout", "true").
		Param("stderr", "true").
		Param("tty", "false")

	cmd := []string{
		"sh", "-c", fmt.Sprintf("ip addr show dev %s | grep 'inet ' | awk '{print $2}' | awk -F/ '{print $1}'", ifName),
	}
	for _, c := range cmd {
		req.Param("command", c)
	}

	// 2. execute the exec request
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to create executor: %v", err)
	}

	// 3. get outputs
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		return "", fmt.Errorf("exec error: %v, stderr: %s", err, stderr.String())
	}

	// 5. retrieve the ip address
	ip := strings.TrimSpace(stdout.String())
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("invalid IP format from exec: %s", ip)
	}
	return ip, nil
}

// combine the slots range
func mergeSlots(slots []int, totalSlots int) []SlotRange {
	if len(slots) == 0 {
		return nil
	}

	sort.Ints(slots)
	ranges := []SlotRange{{Start: slots[0], End: slots[0]}}

	for _, slot := range slots[1:] {
		last := &ranges[len(ranges)-1]
		if slot == last.End+1 {
			last.End = slot
		} else {
			ranges = append(ranges, SlotRange{Start: slot, End: slot})
		}
	}

	// handle ring case（4095 → 0）
	if totalSlots > 0 && ranges[0].Start == 0 && ranges[len(ranges)-1].End == totalSlots-1 {
		first := ranges[0]
		last := ranges[len(ranges)-1]
		return []SlotRange{
			{Start: last.Start, End: first.End},
		}
	}

	return ranges
}

func isNodeListEqual(a, b []NodeInfo) bool {
	if len(a) != len(b) {
		return false
	}

	nodeMap := make(map[string]NodeInfo)
	for _, n := range a {
		nodeMap[n.Name] = n
	}

	for _, n := range b {
		existing, ok := nodeMap[n.Name]
		if !ok || !slotRangesEqual(existing.Slots, n.Slots) {
			return false
		}
	}
	return true
}

func slotRangesEqual(a, b []SlotRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Start != b[i].Start || a[i].End != b[i].End {
			return false
		}
	}
	return true
}
