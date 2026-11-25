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
	"math/rand/v2"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	k8sdiscovery "k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// Values of the check label of aibrix_gateway_discovery_verification_failures_total.
const (
	// DiscoveryCheckReachability is a one-item consistent LIST of Pods. It
	// proves the API server answers this replica, not that the informer watch
	// is still delivering events. It alone drives aibrix_gateway_discovery_unreachable.
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

// ConsistencyVerifier is the model-list verification the consistency check
// drives. discovery.ModelListHealth implements it.
type ConsistencyVerifier interface {
	// EnsureVerified returns nil when the cache matches (possibly reusing a
	// fresh proof, with no request), discovery.ErrModelListVerificationDeferred
	// during a recent failure's retry delay (no request), and any other error
	// when a verification ran and failed.
	EnsureVerified(ctx context.Context) error
	// LastMatched returns when the latest successful verification started.
	LastMatched() time.Time
}

// consistencyOutcome is what one consistency check observed.
type consistencyOutcome int

const (
	consistencyMatched  consistencyOutcome = iota
	consistencyFailed                      // a verification ran and failed
	consistencyDeferred                    // no request: a recent failure's retry delay
)

// DiscoveryCheckOptions configures a DiscoveryChecker.
type DiscoveryCheckOptions struct {
	// Interval between reachability checks. Each one is bounded by half of
	// it, so a slow check ends before the next one is due.
	Interval time.Duration
	// UnreachableThreshold is the age of the last successful reachability
	// check at which the replica reports the API server unreachable. It must be
	// at least twice Interval.
	UnreachableThreshold time.Duration
	// Reachability is required.
	Reachability func(context.Context) error
	// Consistency is optional. It runs on its own goroutine, once per
	// Interval and backing off after failures, and never under the
	// reachability deadline: a full LIST must bound itself, as
	// ModelListHealth.EnsureVerified does. Its result does not affect Unreachable.
	Consistency ConsistencyVerifier
	// WatchResourceVersion is optional. It returns the resource version the
	// Pod informer has reached, as KubernetesProvider.PodWatchResourceVersion
	// does, and must not make an API request. The checker samples it once per
	// Interval and exports the time since it last changed. It does not affect
	// Unreachable.
	WatchResourceVersion func() string
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
	consistency  ConsistencyVerifier
	watchRV      func() string
	now          func() time.Time
	// wait blocks for d, or until a reconnect wake or ctx ends, and reports
	// which. jitter returns the extra delay after a wake. Tests replace both
	// to drive the consistency loop step by step.
	wait   func(ctx context.Context, d time.Duration, wake <-chan struct{}) waitResult
	jitter func() time.Duration
	// started is when the checker was created, right after informer sync.
	started time.Time

	mu           sync.Mutex
	lastVerified time.Time
	// failureStreak counts consecutive reachability failures, so the first
	// one is logged at the default level.
	failureStreak int
	// logFirstFailure logs the first reachability failure of a streak; tests
	// replace it to count calls.
	logFirstFailure func(err error)
	// reportedUnreachable is the state last logged, so each change of state
	// is logged once.
	reportedUnreachable bool
	// reachabilityFailed records that the last reachability check failed, so
	// the next success can wake the consistency loop out of its backoff.
	reachabilityFailed bool
	// reconnected wakes the consistency loop when reachability recovers.
	reconnected chan struct{}
	// lastWatchRV is the Pod watch resource version seen by the latest
	// sample, and watchProgressed when a sample first saw it.
	lastWatchRV     string
	watchProgressed time.Time

	failures       *prometheus.CounterVec
	age            prometheus.GaugeFunc
	unreachable    prometheus.GaugeFunc
	consistencyAge prometheus.GaugeFunc
	watchProgress  prometheus.GaugeFunc
	// watchProgressSupported is always registered, so a replica without the
	// watch progress age says so instead of looking like a missing scrape.
	watchProgressSupported prometheus.GaugeFunc
}

// NewDiscoveryChecker returns a checker whose last success is now, because it
// is created after the informers have synced. Call Register, then Run.
func NewDiscoveryChecker(opts DiscoveryCheckOptions) (*DiscoveryChecker, error) {
	if opts.Interval <= 0 {
		return nil, errors.New("discovery check interval must be positive")
	}
	if opts.UnreachableThreshold < 2*opts.Interval {
		// A healthy check may run for half an interval, so a scrape during it
		// can see an age of up to 1.5 intervals.
		return nil, fmt.Errorf("discovery unreachable threshold %v must be at least twice the check interval %v",
			opts.UnreachableThreshold, opts.Interval)
	}
	if opts.Reachability == nil {
		return nil, errors.New("discovery reachability check is required")
	}
	c := &DiscoveryChecker{
		interval:     opts.Interval,
		timeout:      opts.Interval / 2,
		threshold:    opts.UnreachableThreshold,
		maxBackoff:   max(opts.Interval, discoveryConsistencyMaxBackoff),
		reachability: opts.Reachability,
		consistency:  opts.Consistency,
		watchRV:      opts.WatchResourceVersion,
		now:          time.Now,
		wait:         waitOrWake,
		reconnected:  make(chan struct{}, 1),
	}
	c.logFirstFailure = func(err error) {
		klog.InfoS("Discovery reachability check failed; the replica reports the API server unreachable if this continues",
			"threshold", c.threshold, "err", err)
	}
	c.jitter = func() time.Duration { return time.Duration(rand.Int64N(int64(c.interval))) }
	c.started = c.now()
	c.lastVerified = c.started
	c.failures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_verification_failures_total",
		Help:      "Failed discovery checks of this gateway replica against the Kubernetes API server, by check (reachability: a one-item Pod LIST; consistency: the cached Pods and ModelAdapters against a full LIST, only with --model-list-discovery-max-age)",
	}, []string{"check"})
	c.failures.WithLabelValues(DiscoveryCheckReachability)
	c.age = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_reachability_age_seconds",
		Help:      "Seconds since the reachability check last succeeded, counted from the start of that check, or from informer sync if none has succeeded since",
	}, func() float64 { return c.ReachabilityAge().Seconds() })
	c.unreachable = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_unreachable",
		Help:      "1 when aibrix_gateway_discovery_reachability_age_seconds has reached --discovery-unreachable-threshold: this replica cannot read from the Kubernetes API server and keeps routing on its last known Pod list; 0 otherwise. A watch that is connected but stuck does not show here; see aibrix_gateway_discovery_watch_progress_age_seconds and aibrix_gateway_discovery_consistency_last_verified_age_seconds",
	}, func() float64 {
		if c.Unreachable() {
			return 1
		}
		return 0
	})
	if c.consistency != nil {
		c.failures.WithLabelValues(DiscoveryCheckConsistency)
		c.consistencyAge = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_discovery_consistency_last_verified_age_seconds",
			Help:      "Seconds since the cached Pods and ModelAdapters last matched a full LIST, counted from the start of that LIST, whichever caller (this check or /v1/models) ran it; from gateway start if none has. Only with --model-list-discovery-max-age. It rises by up to the max age between LISTs; a rise well beyond that means the cache is not converging",
		}, func() float64 { return c.ConsistencyAge().Seconds() })
	}
	c.watchProgressSupported = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Subsystem: constants.AibrixSubsystemName,
		Name:      "gateway_discovery_watch_progress_supported",
		Help:      "1 when this replica exports aibrix_gateway_discovery_watch_progress_age_seconds, because the API server reported Kubernetes 1.31 or newer at startup; 0 otherwise, including when its version could not be read. The 1.31 floor is observed behaviour, not a known mechanism: on 1.29 an idle Pod watch did not advance even with the reachability check",
	}, func() float64 {
		if c.watchRV != nil {
			return 1
		}
		return 0
	})
	if c.watchRV != nil {
		c.lastWatchRV = c.watchRV()
		c.watchProgressed = c.started
		c.watchProgress = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_discovery_watch_progress_age_seconds",
			Help:      "Seconds since the Pod informer's resource version last moved (a watch event, a bookmark, or a relist), sampled every --discovery-check-interval, so it can read up to one interval high; from gateway start if it has not moved. With no Pod changes it relies on the reachability check's LIST to keep watch bookmarks advancing about once a minute. A rise well beyond that while aibrix_gateway_discovery_unreachable is 0 suggests a stuck Pod watch. Exported only when the API server reported Kubernetes 1.31 or newer at startup (see aibrix_gateway_discovery_watch_progress_supported): that floor is observed behaviour, not a known mechanism. Export only: readiness does not use it",
		}, func() float64 { return c.WatchProgressAge().Seconds() })
	}
	return c, nil
}

// Register adds the checker's metrics to reg.
func (c *DiscoveryChecker) Register(reg prometheus.Registerer) error {
	collectors := []prometheus.Collector{c.failures, c.age, c.unreachable, c.watchProgressSupported}
	if c.consistencyAge != nil {
		collectors = append(collectors, c.consistencyAge)
	}
	if c.watchProgress != nil {
		collectors = append(collectors, c.watchProgress)
	}
	for _, collector := range collectors {
		if err := reg.Register(collector); err != nil {
			return err
		}
	}
	return nil
}

// ReachabilityAge is the time since the reachability check last succeeded.
func (c *DiscoveryChecker) ReachabilityAge() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Sub(c.lastVerified)
}

// ConsistencyAge is the time since the cache last matched a full LIST, read
// from the verifier so a reused proof does not count as a new match.
func (c *DiscoveryChecker) ConsistencyAge() time.Duration {
	if c.consistency == nil {
		return 0
	}
	matched := c.consistency.LastMatched()
	if matched.IsZero() {
		// No match yet: count from gateway start, like the reachability age.
		// A match from before the checker started (an early /v1/models
		// verification) is real and is kept.
		matched = c.started
	}
	return c.now().Sub(matched)
}

// WatchProgressAge is the time since a sample first saw the current Pod watch
// resource version, or since the checker started if none has changed. It is 0
// without a WatchResourceVersion source.
func (c *DiscoveryChecker) WatchProgressAge() time.Duration {
	if c.watchRV == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Sub(c.watchProgressed)
}

// SampleWatchProgress reads the Pod watch resource version and restarts the
// watch progress age if it moved. Run calls it once per interval.
func (c *DiscoveryChecker) SampleWatchProgress() {
	if c.watchRV == nil {
		return
	}
	rv := c.watchRV()
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if rv != c.lastWatchRV {
		c.lastWatchRV = rv
		c.watchProgressed = now
	}
}

// Unreachable reports whether ReachabilityAge has reached the threshold. It
// means this replica cannot read from the API server, not that its watch is
// stuck: a watch that is connected but not delivering events shows in the
// watch progress and consistency ages instead. It depends on reachability
// alone: the consistency check is exact, so churn during its LIST can fail it
// on a healthy replica.
func (c *DiscoveryChecker) Unreachable() bool {
	return c.ReachabilityAge() >= c.threshold
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
	// read, so the starting age is already accurate. Unreachability is measured
	// from the last success, so an earlier check would not detect it sooner.
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Sampled before the check, so a slow check does not delay it.
			c.SampleWatchProgress()
			c.CheckOnce(ctx)
		}
	}
}

// waitResult is how a consistency wait ended.
type waitResult int

const (
	waitElapsed waitResult = iota
	waitWoken
	waitCancelled
)

func waitOrWake(ctx context.Context, d time.Duration, wake <-chan struct{}) waitResult {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return waitCancelled
	case <-wake:
		return waitWoken
	case <-timer.C:
		return waitElapsed
	}
}

// runConsistency checks consistency every interval while it matches. After a
// failed verification it doubles the wait, up to maxBackoff, so a full LIST
// that keeps failing is not repeated every interval by every replica. A
// deferred result made no request, so it leaves the wait as it was.
//
// When reachability recovers from a failure, the wait starts over at one
// interval plus jitter, rather than running a LIST at once: the informers are
// usually still re-listing then, so an immediate check would mostly fail, and
// every replica would send it together.
func (c *DiscoveryChecker) runConsistency(ctx context.Context) {
	wait := c.interval
	for {
		switch c.wait(ctx, wait, c.reconnected) {
		case waitCancelled:
			return
		case waitWoken:
			wait = c.interval + c.jitter()
			continue
		}
		switch c.CheckConsistencyOnce(ctx) {
		case consistencyMatched:
			wait = c.interval
		case consistencyFailed:
			wait = min(2*wait, c.maxBackoff)
		}
	}
}

// CheckOnce runs the reachability check under a deadline of half the interval
// and returns whether it succeeded.
func (c *DiscoveryChecker) CheckOnce(ctx context.Context) bool {
	started := c.now()
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	err := c.reachability(checkCtx)
	c.mu.Lock()
	recovered := err == nil && c.reachabilityFailed
	c.reachabilityFailed = err != nil
	if err == nil {
		// A LIST may answer from the moment the request started, so freshness
		// is counted from then, as ModelListHealth does.
		if started.After(c.lastVerified) {
			c.lastVerified = started
		}
		c.failureStreak = 0
	}
	// A cancel at shutdown is not a discovery failure: it neither extends the
	// streak nor logs.
	firstFailure := false
	if err != nil && ctx.Err() == nil {
		c.failureStreak++
		firstFailure = c.failureStreak == 1
	}
	c.mu.Unlock()
	if err != nil {
		c.recordFailure(ctx, DiscoveryCheckReachability, err)
	}
	if firstFailure {
		c.logFirstFailure(err)
	}
	if recovered {
		select {
		case c.reconnected <- struct{}{}:
		default:
		}
	}
	c.logReachabilityChange()
	return err == nil
}

// CheckConsistencyOnce runs the consistency check and reports what it
// observed. The checker sets no deadline: the check bounds itself, and a
// deadline here would end a slow LIST as if its caller had gone away, which
// ModelListHealth does not back off from. The consistency age is read from
// the verifier, so this records only failures.
func (c *DiscoveryChecker) CheckConsistencyOnce(ctx context.Context) consistencyOutcome {
	if c.consistency == nil {
		return consistencyDeferred
	}
	err := c.consistency.EnsureVerified(ctx)
	switch {
	case err == nil:
		return consistencyMatched
	case errors.Is(err, discovery.ErrModelListVerificationDeferred):
		// Another caller's verification just failed; this one made no request.
		return consistencyDeferred
	default:
		c.recordFailure(ctx, DiscoveryCheckConsistency, err)
		return consistencyFailed
	}
}

func (c *DiscoveryChecker) logReachabilityChange() {
	c.mu.Lock()
	age := c.now().Sub(c.lastVerified)
	unreachable := age >= c.threshold
	changed := unreachable != c.reportedUnreachable
	c.reportedUnreachable = unreachable
	c.mu.Unlock()
	if !changed {
		return
	}
	if unreachable {
		klog.Warningf("Gateway cannot reach the Kubernetes API server: no successful check for %v (threshold %v); "+
			"routing continues on the last known Pod list", age.Round(time.Second), c.threshold)
	} else {
		klog.InfoS("Gateway reaches the Kubernetes API server again", "age", age.Round(time.Second))
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

// watchProgressMinVersion is the oldest API server on which an idle Pod watch
// was observed to keep advancing while the reachability check runs (envtest
// 1.31 and 1.37; on 1.29 it did not, and 1.30 was not measured). It is an
// observed floor, not a known mechanism.
var watchProgressMinVersion = utilversion.MajorMinor(1, 31)

// WatchProgressSupported reports whether gitVersion, the API server's
// /version gitVersion such as "v1.31.0" or "v1.31.4-eks-1234", is at least
// watchProgressMinVersion. A version that does not parse is unsupported.
func WatchProgressSupported(gitVersion string) bool {
	v, err := utilversion.ParseGeneric(gitVersion)
	if err != nil {
		return false
	}
	return v.AtLeast(watchProgressMinVersion)
}

// KubernetesWatchProgress returns the Pod watch resource version source for
// the discovery checker when the API server is Kubernetes 1.31 or newer, and
// nil otherwise, including when /version cannot be read within timeout. It
// reads /version once and logs the decision with the detected version. Below
// 1.31 an idle Pod watch was observed not to advance even with the
// reachability check, so the age would rise on a healthy replica.
func KubernetesWatchProgress(config *rest.Config, provider *discovery.KubernetesProvider,
	timeout time.Duration) func() string {
	versionConfig := rest.CopyConfig(config)
	versionConfig.Timeout = timeout
	gitVersion := ""
	client, err := k8sdiscovery.NewDiscoveryClientForConfig(versionConfig)
	if err == nil {
		info, versionErr := client.ServerVersion()
		err = versionErr
		if err == nil {
			gitVersion = info.GitVersion
		}
	}
	switch {
	case err != nil:
		klog.InfoS("Discovery watch progress age not exported: cannot read the API server version", "err", err)
		return nil
	case !WatchProgressSupported(gitVersion):
		klog.InfoS("Discovery watch progress age not exported: below this API server version an idle Pod watch was observed not to advance",
			"serverVersion", gitVersion, "minimum", watchProgressMinVersion.String())
		return nil
	}
	klog.InfoS("Discovery watch progress age exported", "serverVersion", gitVersion, "minimum", watchProgressMinVersion.String())
	return provider.PodWatchResourceVersion
}
