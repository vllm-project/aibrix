/*
Copyright 2026 The Aibrix Team.

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
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// Values of the check label of aibrix_gateway_discovery_verification_failures_total.
const (
	// DiscoveryCheckReachability is a one-item consistent LIST of Pods. It
	// proves the API server and etcd answer this replica, not that the
	// informer watch is still delivering events.
	DiscoveryCheckReachability = "reachability"
	// DiscoveryCheckConsistency compares the cached Pods and ModelAdapters
	// with a full LIST (see discovery.ModelListHealth). It runs only when
	// --model-list-discovery-max-age is set.
	DiscoveryCheckConsistency = "consistency"
)

// DiscoveryCheckOptions configures a DiscoveryChecker.
type DiscoveryCheckOptions struct {
	// Interval between checks. Each check is bounded by half of it, so a slow
	// check ends before the next one is due and checks never overlap.
	Interval time.Duration
	// StaleThreshold is the age of the last successful check at which the
	// replica reports itself stale. It must be greater than Interval.
	StaleThreshold time.Duration
	// Reachability is required.
	Reachability func(context.Context) error
	// Consistency is optional. It runs after a successful Reachability check.
	Consistency func(context.Context) error
}

// DiscoveryChecker verifies, on a ticker and independently of informer
// events, that this replica can still read from the Kubernetes API server.
// A replica that cannot keeps routing on its last Pod list without any other
// signal: client-go retries the watch inside ListAndWatch and logs it only at
// V(4), and the gRPC health status is never re-evaluated after startup.
type DiscoveryChecker struct {
	interval     time.Duration
	timeout      time.Duration
	threshold    time.Duration
	reachability func(context.Context) error
	consistency  func(context.Context) error
	now          func() time.Time

	mu           sync.Mutex
	lastVerified time.Time
	// reportedStale is the stale state last logged, so each change of state
	// is logged once.
	reportedStale bool

	failures *prometheus.CounterVec
	age      prometheus.GaugeFunc
	stale    prometheus.GaugeFunc
}

// NewDiscoveryChecker returns a checker whose last success is now, because it
// is created after the informers have synced. Call Register, then Run.
func NewDiscoveryChecker(opts DiscoveryCheckOptions) (*DiscoveryChecker, error) {
	if opts.Interval <= 0 {
		return nil, errors.New("discovery check interval must be positive")
	}
	if opts.StaleThreshold <= opts.Interval {
		return nil, fmt.Errorf("discovery stale threshold %v must be greater than the check interval %v",
			opts.StaleThreshold, opts.Interval)
	}
	if opts.Reachability == nil {
		return nil, errors.New("discovery reachability check is required")
	}
	c := &DiscoveryChecker{
		interval:     opts.Interval,
		timeout:      opts.Interval / 2,
		threshold:    opts.StaleThreshold,
		reachability: opts.Reachability,
		consistency:  opts.Consistency,
		now:          time.Now,
	}
	c.lastVerified = c.now()
	c.failures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_verification_failures_total",
		Help:      "Failed discovery checks of this gateway replica against the Kubernetes API server, by check (reachability: a one-item Pod LIST; consistency: the cached Pods and ModelAdapters against a full LIST, only with --model-list-discovery-max-age)",
	}, []string{"check"})
	c.failures.WithLabelValues(DiscoveryCheckReachability)
	if c.consistency != nil {
		c.failures.WithLabelValues(DiscoveryCheckConsistency)
	}
	c.age = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_last_verified_age_seconds",
		Help:      "Seconds since every enabled discovery check last succeeded, counted from the start of that check, or from informer sync if none has succeeded since",
	}, func() float64 { return c.Age().Seconds() })
	c.stale = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_stale",
		Help:      "1 when aibrix_gateway_discovery_last_verified_age_seconds has reached --discovery-stale-threshold, so this replica may be routing on an outdated Pod list; 0 otherwise",
	}, func() float64 {
		if c.Stale() {
			return 1
		}
		return 0
	})
	return c, nil
}

// Register adds the checker's metrics to reg.
func (c *DiscoveryChecker) Register(reg prometheus.Registerer) error {
	for _, collector := range []prometheus.Collector{c.failures, c.age, c.stale} {
		if err := reg.Register(collector); err != nil {
			return err
		}
	}
	return nil
}

// Age is the time since every enabled check last succeeded.
func (c *DiscoveryChecker) Age() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Sub(c.lastVerified)
}

// Stale reports whether Age has reached the stale threshold.
func (c *DiscoveryChecker) Stale() bool {
	return c.Age() >= c.threshold
}

// Run checks once per interval until stopCh closes. Checks run one at a time
// on this goroutine, and each ends within half the interval, so a check never
// overlaps the next tick. That includes waiting for a model-list verification
// that a /v1/models request started (ModelListHealth.EnsureVerified honors the
// deadline while it waits).
func (c *DiscoveryChecker) Run(stopCh <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	// The first check runs after one interval, not at once: the checker is
	// created right after the informers synced, which was itself a successful
	// read, so the starting age is already accurate. Staleness is measured
	// from the last success, so an earlier check would not detect it sooner.
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CheckOnce(ctx)
		}
	}
}

// CheckOnce runs every enabled check under one deadline and returns whether
// all of them succeeded. The consistency check is skipped when the API server
// is unreachable, because it would fail for the same reason.
func (c *DiscoveryChecker) CheckOnce(ctx context.Context) bool {
	ok := c.check(ctx)
	c.logStaleChange()
	return ok
}

func (c *DiscoveryChecker) check(ctx context.Context) bool {
	started := c.now()
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.reachability(checkCtx); err != nil {
		c.recordFailure(ctx, DiscoveryCheckReachability, err)
		return false
	}
	if c.consistency != nil {
		if err := c.consistency(checkCtx); err != nil {
			c.recordFailure(ctx, DiscoveryCheckConsistency, err)
			return false
		}
	}
	c.mu.Lock()
	// A LIST may answer from the moment the request started, so freshness is
	// counted from then, as ModelListHealth does.
	if started.After(c.lastVerified) {
		c.lastVerified = started
	}
	c.mu.Unlock()
	return true
}

func (c *DiscoveryChecker) logStaleChange() {
	c.mu.Lock()
	age := c.now().Sub(c.lastVerified)
	stale := age >= c.threshold
	changed := stale != c.reportedStale
	c.reportedStale = stale
	c.mu.Unlock()
	if !changed {
		return
	}
	if stale {
		klog.Warningf("Gateway discovery is stale: no successful check for %v (threshold %v); "+
			"routing continues on the last known Pod list", age.Round(time.Second), c.threshold)
	} else {
		klog.InfoS("Gateway discovery recovered", "age", age.Round(time.Second))
	}
}

func (c *DiscoveryChecker) recordFailure(ctx context.Context, check string, err error) {
	if ctx.Err() != nil {
		// Shutting down: not a discovery failure.
		return
	}
	c.failures.WithLabelValues(check).Inc()
	if logger := klog.V(2); logger.Enabled() {
		logger.InfoS("Discovery check failed", "check", check, "age", c.Age(), "err", err)
	}
}

// PodListReachability returns a reachability check: one LIST of Pods with
// limit=1. resourceVersion is left unset, which asks for a consistent read: the
// API server confirms etcd's current revision before answering, whether it
// then serves the list from etcd or from its watch cache. resourceVersion=0
// would accept whatever the watch cache holds, and older API servers ignore
// the limit for it. The gateway role can already list Pods.
//
// client-go shares one transport between clients built from the same config,
// so this request usually travels on the same HTTP/2 connection as the
// informer watches it vouches for.
func PodListReachability(client kubernetes.Interface) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{Limit: 1})
		return err
	}
}
