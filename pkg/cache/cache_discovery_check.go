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
	// proves the API server answers this replica, not that the informer watch
	// is still delivering events. It alone drives aibrix_gateway_discovery_stale.
	DiscoveryCheckReachability = "reachability"
	// DiscoveryCheckConsistency compares the cached Pods and ModelAdapters
	// with a full LIST (see discovery.ModelListHealth). It runs only when
	// --model-list-discovery-max-age is set, and is reported separately.
	DiscoveryCheckConsistency = "consistency"
)

// discoveryConsistencyMaxBackoff caps the wait between consistency checks
// after repeated failures, so a LIST that keeps failing on a large cluster is
// retried rarely instead of every interval.
const discoveryConsistencyMaxBackoff = 5 * time.Minute

// DiscoveryCheckOptions configures a DiscoveryChecker.
type DiscoveryCheckOptions struct {
	// Interval between reachability checks. Each one is bounded by half of
	// it, so a slow check ends before the next one is due.
	Interval time.Duration
	// StaleThreshold is the age of the last successful reachability check at
	// which the replica reports itself stale. It must be greater than Interval.
	StaleThreshold time.Duration
	// Reachability is required.
	Reachability func(context.Context) error
	// Consistency is optional. It runs on its own goroutine, once per
	// Interval and backing off after failures, and never under the
	// reachability deadline: a full LIST must bound itself, as
	// ModelListHealth.EnsureVerified does. Its result does not affect Stale.
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
	maxBackoff   time.Duration
	reachability func(context.Context) error
	consistency  func(context.Context) error
	now          func() time.Time

	mu                  sync.Mutex
	lastVerified        time.Time
	consistencyVerified time.Time
	// reportedStale is the stale state last logged, so each change of state
	// is logged once.
	reportedStale bool
	// reachabilityFailed records that the last reachability check failed, so
	// the next success can wake the consistency loop out of its backoff.
	reachabilityFailed bool
	// reconnected wakes the consistency loop when reachability recovers.
	reconnected chan struct{}

	failures       *prometheus.CounterVec
	age            prometheus.GaugeFunc
	stale          prometheus.GaugeFunc
	consistencyAge prometheus.GaugeFunc
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
		maxBackoff:   max(opts.Interval, discoveryConsistencyMaxBackoff),
		reachability: opts.Reachability,
		consistency:  opts.Consistency,
		now:          time.Now,
		reconnected:  make(chan struct{}, 1),
	}
	c.lastVerified = c.now()
	c.consistencyVerified = c.lastVerified
	c.failures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_verification_failures_total",
		Help:      "Failed discovery checks of this gateway replica against the Kubernetes API server, by check (reachability: a one-item Pod LIST; consistency: the cached Pods and ModelAdapters against a full LIST, only with --model-list-discovery-max-age)",
	}, []string{"check"})
	c.failures.WithLabelValues(DiscoveryCheckReachability)
	c.age = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_last_verified_age_seconds",
		Help:      "Seconds since the reachability check last succeeded, counted from the start of that check, or from informer sync if none has succeeded since",
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
	if c.consistency != nil {
		c.failures.WithLabelValues(DiscoveryCheckConsistency)
		c.consistencyAge = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_discovery_consistency_last_verified_age_seconds",
			Help:      "Seconds since the cached Pods and ModelAdapters last matched a full LIST, counted from the start of that check; only with --model-list-discovery-max-age. A single failure can be churn during the LIST; a steady rise means the cache is not converging",
		}, func() float64 { return c.ConsistencyAge().Seconds() })
	}
	return c, nil
}

// Register adds the checker's metrics to reg.
func (c *DiscoveryChecker) Register(reg prometheus.Registerer) error {
	collectors := []prometheus.Collector{c.failures, c.age, c.stale}
	if c.consistencyAge != nil {
		collectors = append(collectors, c.consistencyAge)
	}
	for _, collector := range collectors {
		if err := reg.Register(collector); err != nil {
			return err
		}
	}
	return nil
}

// Age is the time since the reachability check last succeeded.
func (c *DiscoveryChecker) Age() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Sub(c.lastVerified)
}

// ConsistencyAge is the time since the consistency check last succeeded.
func (c *DiscoveryChecker) ConsistencyAge() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Sub(c.consistencyVerified)
}

// Stale reports whether Age has reached the stale threshold. It depends on
// reachability alone: the consistency check is exact, so churn during its
// LIST can fail it on a healthy replica.
func (c *DiscoveryChecker) Stale() bool {
	return c.Age() >= c.threshold
}

// Run checks reachability once per interval until stopCh closes, and runs the
// consistency check, if enabled, on its own goroutine. Reachability checks run
// one at a time and each ends within half the interval, so a check never
// overlaps the next tick, and a slow full LIST cannot delay them.
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
	if c.consistency != nil {
		go c.runConsistency(ctx)
	}
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

// runConsistency checks consistency every interval while it succeeds. After a
// failure it doubles the wait, up to maxBackoff, so a full LIST that keeps
// failing is not repeated every interval by every replica. When reachability
// recovers from a failure, it checks at once and resets the wait: failures
// during an outage say nothing about the cache once the API server is back.
func (c *DiscoveryChecker) runConsistency(ctx context.Context) {
	wait := c.interval
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.reconnected:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			wait = c.interval
		}
		if c.CheckConsistencyOnce(ctx) {
			wait = c.interval
		} else {
			wait = min(2*wait, c.maxBackoff)
		}
		timer.Reset(wait)
	}
}

// CheckOnce runs the reachability check under a deadline of half the interval
// and returns whether it succeeded.
func (c *DiscoveryChecker) CheckOnce(ctx context.Context) bool {
	started := c.now()
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	err := c.reachability(checkCtx)
	if err != nil {
		c.recordFailure(ctx, DiscoveryCheckReachability, err)
	} else {
		c.markVerified(&c.lastVerified, started)
	}
	c.mu.Lock()
	recovered := err == nil && c.reachabilityFailed
	c.reachabilityFailed = err != nil
	c.mu.Unlock()
	if recovered {
		select {
		case c.reconnected <- struct{}{}:
		default:
		}
	}
	c.logStaleChange()
	return err == nil
}

// CheckConsistencyOnce runs the consistency check and returns whether it
// succeeded. The checker sets no deadline: the check bounds itself, and a
// deadline here would end a slow LIST as if its caller had gone away, which
// ModelListHealth does not back off from.
func (c *DiscoveryChecker) CheckConsistencyOnce(ctx context.Context) bool {
	if c.consistency == nil {
		return false
	}
	started := c.now()
	if err := c.consistency(ctx); err != nil {
		c.recordFailure(ctx, DiscoveryCheckConsistency, err)
		return false
	}
	c.markVerified(&c.consistencyVerified, started)
	return true
}

// markVerified records a success. A LIST may answer from the moment the
// request started, so freshness is counted from then, as ModelListHealth does.
func (c *DiscoveryChecker) markVerified(verified *time.Time, started time.Time) {
	c.mu.Lock()
	if started.After(*verified) {
		*verified = started
	}
	c.mu.Unlock()
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
	klog.V(2).InfoS("Discovery check failed", "check", check, "err", err)
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
