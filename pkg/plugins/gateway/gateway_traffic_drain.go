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
	"context"
	"errors"
	"fmt"
	"sync"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/trafficdrain"
	"github.com/vllm-project/aibrix/pkg/types"
)

func needsTrafficAdmission(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}
	_, configured := pod.Annotations[constants.TrafficScaleInMaxWaitAnnotationKey]
	return configured || pod.Annotations[constants.TrafficDrainProtocolAnnotationKey] != "" || pod.Annotations[constants.TrafficDrainEpochAnnotationKey] != ""
}

func (s *Server) selectAndAdmitTarget(ctx context.Context, rc *types.RoutingContext, pods types.PodList, filter, requestID, model string) (string, *extProcPb.ProcessingResponse) {
	address, err := s.selectTargetPod(ctx, rc, pods, filter)
	if address == "" || err != nil {
		if response := buildTargetSelectionErrorResponse(rc, requestID, model, err); response != nil {
			return "", response
		}
		klog.ErrorS(err, "failed to select target pod", "requestID", requestID, "routingStrategy", rc.Algorithm, "model", model, "routingDuration", rc.GetRoutingDelay())
		return "", buildRoutingErrorResponse(rc, requestID, envoyTypePb.StatusCode_ServiceUnavailable, "error on selecting target pod", ErrorCodeServiceUnavailable, "", HeaderErrorRouting, "true")
	}
	if response := s.enforceReplicaInflight(ctx, model, rc); response != nil {
		return "", response
	}
	if rc.TrafficDrainRouting && !rc.TrafficDrainManaged {
		if err := s.admitTrafficTarget(rc, rc.TargetPod()); err != nil {
			return "", buildTargetSelectionErrorResponse(rc, requestID, model, err)
		}
	}
	return address, nil
}

func (s *Server) trafficGate() *trafficdrain.Store {
	s.trafficDrainOnce.Do(func() {
		if s.trafficDrainStore == nil {
			s.trafficDrainStore = trafficdrain.New(s.redisClient)
		}
	})
	return s.trafficDrainStore
}

func prepareTrafficRouting(rc *types.RoutingContext, pods []*v1.Pod, algorithm types.RoutingAlgorithm) (types.RoutingAlgorithm, error) {
	protected := false
	for _, pod := range pods {
		protected = protected || needsTrafficAdmission(pod)
	}
	if !protected {
		return algorithm, nil
	}
	path := pathWithoutQuery(rc.ReqPath)
	if path != "/v1/chat/completions" && path != "/v1/completions" {
		return algorithm, errors.New("traffic-aware drain supports synchronous text completions only")
	}
	if algorithm == routing.RouterNotSet {
		algorithm = routing.RouterLeastRequest
	}
	if algorithm != routing.RouterLeastRequest && algorithm != routing.RouterLeastRequestTopK {
		return algorithm, errors.New("traffic-aware drain requires least-request or least-request-top-k routing")
	}
	rc.TrafficDrainRouting = true
	rc.Algorithm = algorithm
	return algorithm, nil
}

func releaseTrafficReservation(rc *types.RoutingContext) {
	if rc != nil && rc.TrafficDrainRelease != nil {
		release := rc.TrafficDrainRelease
		rc.TrafficDrainRelease = nil
		release()
	}
}

func (s *Server) admitTrafficTarget(rc *types.RoutingContext, pod *v1.Pod) error {
	if !needsTrafficAdmission(pod) {
		return nil
	}
	epoch := pod.Annotations[constants.TrafficDrainEpochAnnotationKey]
	if pod.Annotations[constants.TrafficDrainProtocolAnnotationKey] != constants.TrafficDrainProtocolVersion || epoch == "" || pod.UID == "" {
		return types.ErrTargetTrafficDraining
	}
	token, err := trafficdrain.NewToken()
	if err != nil {
		return fmt.Errorf("%w: %v", types.ErrTargetTrafficDraining, err)
	}
	gate := s.trafficGate()
	uid := string(pod.UID)
	if err = gate.Admit(rc.Context, uid, epoch, token); err != nil {
		// A lost reply may have reserved the token. No request was dispatched,
		// so releasing this exact token is safe; failure leaves it pinned.
		_ = gate.Release(context.Background(), uid, epoch, token)
		return fmt.Errorf("%w: %v", types.ErrTargetTrafficDraining, err)
	}
	var once sync.Once
	rc.TrafficDrainManaged = true
	rc.TrafficDrainRelease = func() {
		once.Do(func() {
			if err := gate.Release(context.Background(), uid, epoch, token); err != nil {
				klog.ErrorS(err, "Traffic reservation release was not confirmed", "podUID", uid)
			}
		})
	}
	return nil
}

func (s *Server) admitTrafficAndReplica(rc *types.RoutingContext, pod *v1.Pod, limit int64) error {
	if err := s.admitTrafficTarget(rc, pod); err != nil {
		return err
	}
	if limit > 0 {
		rc.ReplicaInflightChecked = true
		if !s.admitReplicaInflight(rc, pod, limit) {
			releaseTrafficReservation(rc)
			rc.TrafficDrainManaged = false
			return types.ErrTargetReplicaInflightExceeded
		}
	}
	return nil
}

func responseTransportEnded(req *extProcPb.ProcessingRequest) bool {
	return req.GetResponseHeaders() != nil && req.GetResponseHeaders().GetEndOfStream() ||
		req.GetResponseBody() != nil && req.GetResponseBody().GetEndOfStream() || req.GetResponseTrailers() != nil
}

func (s *Server) finishTrafficTransport(st *processState, req *extProcPb.ProcessingRequest) {
	if st.routerCtx == nil || !st.routerCtx.TrafficDrainManaged {
		return
	}
	if responseTransportEnded(req) {
		releaseTrafficReservation(st.routerCtx)
		st.completed = true
	} else if st.completed {
		// A logical SSE terminator is not the end of the HTTP transport. Keep
		// consuming callbacks until EOS, and never release on an ambiguous EOF.
		st.trafficSemanticDone = true
		st.completed = false
	}
}

func (s *Server) rollbackTrafficAdmission(rc *types.RoutingContext) {
	releaseTrafficReservation(rc)
	rc.TrafficDrainManaged = false
	if rc.TrafficDrainRouting && rc.ReplicaInflightAdmitted && rc.HasRouted() {
		if pod := rc.TargetPod(); pod != nil {
			s.cache.ReleasePodRunningRequest(pod.Name, pod.Namespace)
		}
		rc.ReplicaInflightAdmitted = false
	}
}
