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
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/vllm-project/aibrix/pkg/cache"
)

// cuttableProxy forwards TCP to the API server until Cut, which drops every
// open connection and refuses new ones while keeping the port open, so the
// client sees connection resets rather than a vanished host.
type cuttableProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
	cut    bool
}

func newCuttableProxy(target string) *cuttableProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	p := &cuttableProxy{ln: ln, target: target}
	go p.serve()
	return p
}

func (p *cuttableProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.cut {
			p.mu.Unlock()
			_ = client.Close()
			continue
		}
		upstream, err := net.Dial("tcp", p.target)
		if err != nil {
			p.mu.Unlock()
			_ = client.Close()
			continue
		}
		p.conns = append(p.conns, client, upstream)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
		go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
	}
}

func (p *cuttableProxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = true
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *cuttableProxy) Restore() {
	p.mu.Lock()
	p.cut = false
	p.mu.Unlock()
}

func (p *cuttableProxy) Close() { _ = p.ln.Close(); p.Cut() }

// replicaChecker is the discovery check of one gateway replica, with its own
// client, connection, and metrics registry.
type replicaChecker struct {
	checker *cache.DiscoveryChecker
	reg     *prometheus.Registry
	stopCh  chan struct{}
}

func startReplicaChecker(cfg *rest.Config, interval, threshold time.Duration) *replicaChecker {
	client, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	checker, err := cache.NewDiscoveryChecker(cache.DiscoveryCheckOptions{
		Interval:             interval,
		UnreachableThreshold: threshold,
		Reachability:         cache.PodListReachability(client),
	})
	Expect(err).NotTo(HaveOccurred())
	reg := prometheus.NewRegistry()
	Expect(checker.Register(reg)).To(Succeed())
	r := &replicaChecker{checker: checker, reg: reg, stopCh: make(chan struct{})}
	go checker.Run(r.stopCh)
	return r
}

func (r *replicaChecker) metric(name string) float64 {
	families, err := r.reg.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var sum float64
		for _, m := range family.GetMetric() {
			switch {
			case m.GetGauge() != nil:
				sum += m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				sum += m.GetCounter().GetValue()
			}
		}
		return sum
	}
	Fail("metric not registered: " + name)
	return 0
}

const (
	unreachableMetric = "aibrix_gateway_discovery_unreachable"
	ageMetric         = "aibrix_gateway_discovery_reachability_age_seconds"
	failuresMetric    = "aibrix_gateway_discovery_verification_failures_total"
)

func (r *replicaChecker) unreachable() float64 { return r.metric(unreachableMetric) }

var _ = Describe("Gateway discovery check", Ordered, Label("gateway", "integration"), func() {
	const (
		interval  = time.Second
		threshold = 3 * time.Second
	)
	var (
		testEnv   *envtest.Environment
		proxy     *cuttableProxy
		cutOff    *replicaChecker
		connected *replicaChecker
	)

	BeforeAll(func() {
		testEnv = &envtest.Environment{}
		cfg, err := testEnv.Start()
		Expect(err).NotTo(HaveOccurred())
		apiServer, err := url.Parse(cfg.Host)
		Expect(err).NotTo(HaveOccurred())

		proxy = newCuttableProxy(apiServer.Host)
		proxied := rest.CopyConfig(cfg)
		proxied.Host = strings.Replace(cfg.Host, apiServer.Host, proxy.ln.Addr().String(), 1)
		// The proxy listens on 127.0.0.1 like the API server, which its
		// serving certificate names, so TLS verification still holds.

		cutOff = startReplicaChecker(proxied, interval, threshold)
		connected = startReplicaChecker(cfg, interval, threshold)
	})

	AfterAll(func() {
		for _, r := range []*replicaChecker{cutOff, connected} {
			if r != nil {
				close(r.stopCh)
			}
		}
		if proxy != nil {
			proxy.Close()
		}
		if testEnv != nil {
			Expect(testEnv.Stop()).To(Succeed())
		}
	})

	It("reports both replicas reachable while the API server answers", func() {
		Consistently(func() float64 { return cutOff.unreachable() + connected.unreachable() },
			2*interval, 200*time.Millisecond).Should(BeZero())
		Expect(cutOff.metric(failuresMetric)).To(BeZero())
	})

	It("reports only the cut-off replica unreachable, within one interval plus the threshold", func() {
		proxy.Cut()
		cutAt := time.Now()
		Eventually(cutOff.unreachable, interval+threshold+2*time.Second, 100*time.Millisecond).Should(Equal(1.0))
		Expect(time.Since(cutAt)).To(BeNumerically(">=", threshold-interval),
			"not unreachable before the threshold, minus the time since its last success")
		Expect(cutOff.metric(failuresMetric)).To(BeNumerically(">=", 1))
		Expect(cutOff.metric(ageMetric)).To(BeNumerically(">=", threshold.Seconds()))

		Expect(connected.unreachable()).To(BeZero())
		Expect(connected.metric(ageMetric)).To(BeNumerically("<", threshold.Seconds()))
		Expect(connected.metric(failuresMetric)).To(BeZero())
	})

	It("reports the cut-off replica reachable again after it reconnects", func() {
		proxy.Restore()
		Eventually(cutOff.unreachable, 3*interval, 100*time.Millisecond).Should(BeZero())
		Expect(cutOff.metric(ageMetric)).To(BeNumerically("<", 2*interval.Seconds()))
		Expect(connected.unreachable()).To(BeZero())
	})
})
