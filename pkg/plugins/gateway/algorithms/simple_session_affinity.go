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

package routingalgorithms

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"net"
	"strconv"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	RouterSessionAffinity types.RoutingAlgorithm = "session-affinity"
	// NOTE: sessionIDHeader must strictly match types.HeaderSessionID
	// defined in pkg/plugins/gateway/types.go to prevent routing failures.
	sessionIDHeader string = "x-session-id"
)

func init() {
	Register(RouterSessionAffinity, NewSessionAffinityRouter)
}

type sessionAffinityRouter struct{}

func NewSessionAffinityRouter() (types.Router, error) {
	return &sessionAffinityRouter{}, nil
}

// Route implements session affinity by attempting to route requests to the same pod
// using a session ID stored in the request header. The session ID encodes the target
// pod's address as "IP:Port". If no valid session exists, it falls back to a randomly selected ready pod.
func (r *sessionAffinityRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	if ctx.ReqHeaders == nil {
		klog.V(4).InfoS("No request or headers, skipping session affinity",
			"request_id", ctx.RequestID)
		return r.fallbackRoute(ctx, readyPodList)
	}

	sessionID := ctx.ReqHeaders[sessionIDHeader]
	var targetAddr string

	if sessionID != "" {
		decoded, err := base64.StdEncoding.DecodeString(sessionID)
		if err != nil {
			klog.ErrorS(err, "Invalid session ID format",
				"request_id", ctx.RequestID, "session_id", sessionID)
		} else {
			targetAddr = string(decoded)
		}
	}

	// If find a decoded target address, try to match ready pod
	if targetAddr != "" {
		for _, pod := range readyPodList.All() {
			port := utils.GetModelPortForPod(ctx.RequestID, pod)
			if port == 0 {
				continue
			}

			addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port)))
			if addr == targetAddr {
				ctx.SetTargetPod(pod)
				r.setSessionHeader(ctx, addr) // refresh or keep same
				klog.V(4).InfoS("Session affinity matched address", "request_id", ctx.RequestID, "addr", addr)
				return ctx.TargetAddress(), nil
			}
		}
	}

	// Session ID missing, invalid, or pod not ready → fallback
	klog.V(4).InfoS("Session affinity failed, falling back", "request_id", ctx.RequestID, "session_id", sessionID)
	return r.fallbackRoute(ctx, readyPodList)
}

func (r *sessionAffinityRouter) setSessionHeader(ctx *types.RoutingContext, addr string) {
	if ctx.RespHeaders == nil {
		ctx.RespHeaders = make(map[string]string)
	}
	ctx.RespHeaders[sessionIDHeader] = base64.StdEncoding.EncodeToString([]byte(addr))
}

// fallbackRoute selects a random ready pod and returns its IP:Port as the target address.
// It also sets the session ID in the response so the client can stick to this pod next time.
func (r *sessionAffinityRouter) fallbackRoute(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()
	rand.Shuffle(len(pods), func(i, j int) { pods[i], pods[j] = pods[j], pods[i] })

	for _, selected := range pods {
		port := utils.GetModelPortForPod(ctx.RequestID, selected)
		// A routable pod must have a valid IP and port.
		if port == 0 || selected.Status.PodIP == "" {
			klog.V(4).Infof("Fallback skipping pod %s with invalid "+
				"network address (IP: %s, Port: %d)", selected.Name, selected.Status.PodIP, port)
			continue
		}
		addr := net.JoinHostPort(selected.Status.PodIP, strconv.Itoa(int(port)))
		ctx.SetTargetPod(selected)
		r.setSessionHeader(ctx, addr)
		klog.V(5).Infof("Fallback to random pod: %s (%s)", selected.Name, addr)
		return ctx.TargetAddress(), nil
	}
	return "", fmt.Errorf("no fallback pod found with a valid network address")
}

// ScoreAll computes the scores for all ready pods in a single batch operation.
// The pod with session affinity will receive score 1, others will receive score 0.
func (r *sessionAffinityRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	if ctx.ReqHeaders == nil {
		for i := range pods {
			scores[i] = 0
			scored[i] = true
		}
		return scores, scored, nil
	}

	sessionID := ctx.ReqHeaders[sessionIDHeader]
	var targetAddr string

	if sessionID != "" {
		decoded, err := base64.StdEncoding.DecodeString(sessionID)
		if err != nil {
			klog.V(4).ErrorS(err, "Invalid session ID format")
		} else {
			targetAddr = string(decoded)
		}
	}

	for i, pod := range pods {
		port := utils.GetModelPortForPod(ctx.RequestID, pod)
		if port == 0 {
			scores[i] = 0
			scored[i] = true
			continue
		}

		addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port)))
		if targetAddr != "" && addr == targetAddr {
			scores[i] = 1
		} else {
			scores[i] = 0
		}
		scored[i] = true
	}

	return scores, scored, nil
}

// Polarity returns whether higher or lower score is better.
func (r *sessionAffinityRouter) Polarity() types.Polarity {
	return types.PolarityMost
}

// PostRouteUpdate ensures the session header is set on the response *after* the final target pod is chosen.
// This is necessary for multi-strategy routing where ScoreAll is read-only.
func (r *sessionAffinityRouter) PostRouteUpdate(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) error {
	port := utils.GetModelPortForPod(ctx.RequestID, targetPod)
	if port == 0 {
		return nil
	}
	addr := net.JoinHostPort(targetPod.Status.PodIP, strconv.Itoa(int(port)))
	r.setSessionHeader(ctx, addr)
	return nil
}
