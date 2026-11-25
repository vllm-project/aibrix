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

package gateway

import (
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
)

const (
	watchProgressMetric          = "aibrix_gateway_discovery_watch_progress_age_seconds"
	watchProgressSupportedMetric = "aibrix_gateway_discovery_watch_progress_supported"
)

// hasMetric reports whether name is registered.
func (r *replicaChecker) hasMetric(name string) bool {
	families, err := r.reg.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, family := range families {
		if family.GetName() == name {
			return true
		}
	}
	return false
}

// minorAtLeast131 reads the API server's version the plain way, from its
// Major and Minor fields, so the test does not check the gate with itself.
func minorAtLeast131(client kubernetes.Interface) (bool, string) {
	info, err := client.Discovery().ServerVersion()
	Expect(err).NotTo(HaveOccurred())
	major, err := strconv.Atoi(info.Major)
	Expect(err).NotTo(HaveOccurred())
	minor, err := strconv.Atoi(strings.TrimRight(info.Minor, "+"))
	Expect(err).NotTo(HaveOccurred())
	return major > 1 || (major == 1 && minor >= 31), info.GitVersion
}

// This guards against false alarms on a quiet cluster: with no Pod changes,
// the Pod watch only moves on bookmarks. On 1.31 and 1.37 those carried a
// newer resource version about once a minute while the reachability LIST
// ran, so the age should stay below 70 s. On 1.29 they did not, so the gauge
// must not be exported there at all.
var _ = Describe("Gateway discovery watch progress", Ordered, Label("gateway", "integration"), func() {
	const (
		// Shorter than the 10 s default so the gauge's sampling error (up to
		// one interval) stays small next to the 70 s bound.
		interval  = 2 * time.Second
		threshold = 6 * time.Second
		// With no advancing bookmark the age would pass 70 s a minute before
		// the end. The first bookmark is due about 60 s after the watch
		// opens, the second about 120 s.
		observe = 130 * time.Second
		maxAge  = 70.0
	)
	var (
		testEnv       *envtest.Environment
		provider      *discovery.KubernetesProvider
		replica       *replicaChecker
		stopCh        chan struct{}
		supported     bool
		serverVersion string
	)

	BeforeAll(func() {
		testEnv = &envtest.Environment{}
		cfg, err := testEnv.Start()
		Expect(err).NotTo(HaveOccurred())

		stopCh = make(chan struct{})
		provider = discovery.NewKubernetesProvider(cfg).WithModelAdapters(false)
		Expect(provider.Watch(func(discovery.WatchEvent) {}, stopCh)).To(Succeed())

		client, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		supported, serverVersion = minorAtLeast131(client)
		checker, err := cache.NewDiscoveryChecker(cache.DiscoveryCheckOptions{
			Interval:             interval,
			UnreachableThreshold: threshold,
			Reachability:         cache.PodListReachability(client),
			// The gateway's own gate: one /version read.
			WatchResourceVersion: cache.KubernetesWatchProgress(cfg, provider, interval/2),
		})
		Expect(err).NotTo(HaveOccurred())
		reg := prometheus.NewRegistry()
		Expect(checker.Register(reg)).To(Succeed())
		replica = &replicaChecker{checker: checker, reg: reg, stopCh: make(chan struct{})}
		go checker.Run(replica.stopCh)
	})

	AfterAll(func() {
		if replica != nil {
			close(replica.stopCh)
		}
		if stopCh != nil {
			close(stopCh)
		}
		if testEnv != nil {
			Expect(testEnv.Stop()).To(Succeed())
		}
	})

	It("exports the age only when the API server is 1.31 or newer", func() {
		GinkgoWriter.Printf("API server %s, expected supported=%v\n", serverVersion, supported)
		Expect(replica.hasMetric(watchProgressMetric)).To(Equal(supported))
		want := 0.0
		if supported {
			want = 1
		}
		Expect(replica.metric(watchProgressSupportedMetric)).To(Equal(want))
	})

	It("stays below 70 s on an idle cluster while the reachability check runs", func() {
		if !supported {
			Skip("the age is not exported on API server " + serverVersion + "; it needs 1.31 or newer")
		}
		start := time.Now()
		startVersion := provider.PodWatchResourceVersion()
		versions := map[string]bool{startVersion: true}
		peak := 0.0
		Consistently(func() float64 {
			versions[provider.PodWatchResourceVersion()] = true
			age := replica.metric(watchProgressMetric)
			peak = max(peak, age)
			return age
		}, observe, time.Second).Should(BeNumerically("<", maxAge))
		GinkgoWriter.Printf("watch progress: %d distinct Pod resource versions in %v, peak age %.1fs\n",
			len(versions), time.Since(start).Round(time.Second), peak)

		// Over 130 s the bound already implies a reset; say so directly. A
		// second one is due near 120 s, too close to the end to require.
		Expect(len(versions)).To(BeNumerically(">=", 2),
			"expected at least one advancing bookmark after %v", startVersion)
		Expect(replica.unreachable()).To(BeZero())
	})
})
