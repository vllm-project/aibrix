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

package roleset

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	controllerdrain "github.com/vllm-project/aibrix/pkg/controller/drain"
	"github.com/vllm-project/aibrix/pkg/trafficdrain"
)

var sharedTrafficGate = sync.OnceValue(trafficdrain.NewFromEnvironment)

func (r *RoleSetReconciler) validateTrafficRoleMigration(ctx context.Context, owner *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) error {
	if !role.Stateful && (role.PodGroupSize == nil || *role.PodGroupSize <= 1) {
		return nil
	}
	pods, err := getRolePods(ctx, r.Client, owner.Namespace, owner.Name, role.Name)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if protectedTrafficPod(pod) {
			return fmt.Errorf("role %s must retire protected Pods before changing workload type", role.Name)
		}
	}
	return nil
}

func protectedTrafficPod(pod *v1.Pod) bool {
	return pod != nil && pod.Annotations[aibrixconst.TrafficDrainProtocolAnnotationKey] == aibrixconst.TrafficDrainProtocolVersion
}

func (s *StatelessRoleSyncer) trafficGate() *trafficdrain.Store {
	if s.gate != nil {
		return s.gate
	}
	return sharedTrafficGate()
}

func podTrafficWait(pod *v1.Pod) (time.Duration, error) {
	wait, err := time.ParseDuration(pod.Annotations[aibrixconst.TrafficScaleInMaxWaitAnnotationKey])
	if err != nil || wait <= 0 {
		return 0, fmt.Errorf("pod %s has no valid traffic drain wait", pod.Name)
	}
	return wait, nil
}

func prepareTrafficPodCreation(pod *v1.Pod) {
	if _, configured := pod.Annotations[aibrixconst.TrafficScaleInMaxWaitAnnotationKey]; !configured {
		return
	}
	pod.Annotations[aibrixconst.TrafficDrainProtocolAnnotationKey] = aibrixconst.TrafficDrainProtocolVersion
	delete(pod.Annotations, aibrixconst.TrafficDrainEpochAnnotationKey)
	delete(pod.Annotations, aibrixconst.TrafficScaleInRetryAtAnnotationKey)
}

func (s *StatelessRoleSyncer) prepareTrafficEpochs(ctx context.Context, owner *orchestrationv1alpha1.RoleSet, pods []*v1.Pod) (controllerdrain.Result, error) {
	var result controllerdrain.Result
	for _, pod := range pods {
		if version := pod.Annotations[aibrixconst.TrafficDrainProtocolAnnotationKey]; version != "" && version != aibrixconst.TrafficDrainProtocolVersion {
			return result, fmt.Errorf("pod %s has unsupported traffic drain protocol %s", pod.Name, version)
		}
		if !protectedTrafficPod(pod) {
			if _, configured := pod.Annotations[aibrixconst.TrafficScaleInMaxWaitAnnotationKey]; configured {
				return result, fmt.Errorf("pod %s must be recreated to initialize protected traffic", pod.Name)
			}
		}
		if !protectedTrafficPod(pod) || pod.DeletionTimestamp != nil || pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded {
			continue
		}
		epoch := pod.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey]
		if epoch == "" {
			value, err := s.trafficGate().Initialize(ctx, string(pod.UID))
			if err != nil {
				return result, err
			}
			before := pod.DeepCopy()
			pod.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey] = value
			if err = s.cli.Patch(ctx, pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return result, err
			}
			result.Merge(controllerdrain.Result{Changed: true, RequeueAfter: trafficDrainPoll})
			continue
		}
		state, err := s.trafficGate().Read(ctx, string(pod.UID), epoch)
		if err != nil && !errors.Is(err, trafficdrain.ErrExpired) {
			return result, fmt.Errorf("pod %s: %w", pod.Name, err)
		}
		if state.Phase == trafficdrain.Sealed {
			r, err := s.deleteSealedTrafficPod(ctx, pod)
			result.Merge(r)
			if err != nil {
				return result, err
			}
		} else if errors.Is(err, trafficdrain.ErrExpired) {
			wait, err := podTrafficWait(pod)
			if err != nil {
				return result, err
			}
			r, err := s.cancelTrafficPod(ctx, owner, pod, state.Generation, wait)
			result.Merge(r)
			if err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func (s *StatelessRoleSyncer) deleteSealedTrafficPod(ctx context.Context, pod *v1.Pod) (controllerdrain.Result, error) {
	if err := s.cli.Delete(ctx, pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
		return controllerdrain.Result{}, err
	}
	// Forgetting a sealed identity cannot reopen it: a missing published epoch
	// rejects admission and is never initialized again.
	_ = s.trafficGate().Forget(ctx, string(pod.UID), pod.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey])
	return controllerdrain.Result{Changed: true, RequeueAfter: trafficDrainPoll}, nil
}

func (s *StatelessRoleSyncer) cancelTrafficPod(ctx context.Context, owner *orchestrationv1alpha1.RoleSet, pod *v1.Pod, generation int64, cooldown time.Duration) (controllerdrain.Result, error) {
	state, err := s.trafficGate().Cancel(ctx, string(pod.UID), pod.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey], generation, cooldown)
	if errors.Is(err, trafficdrain.ErrConflict) {
		return controllerdrain.Result{RequeueAfter: trafficDrainPoll}, nil
	}
	if err != nil {
		return controllerdrain.Result{}, err
	}
	before := pod.DeepCopy()
	for _, key := range []string{aibrixconst.PodDrainingAnnotationKey, aibrixconst.PodDrainStartTimeAnnotationKey, aibrixconst.PodDrainReasonAnnotationKey, aibrixconst.PodDrainTargetActionAnnotationKey} {
		delete(pod.Annotations, key)
	}
	if cooldown > 0 {
		pod.Annotations[aibrixconst.TrafficScaleInRetryAtAnnotationKey] = time.UnixMilli(state.RetryAtMS).UTC().Format(time.RFC3339Nano)
	} else {
		delete(pod.Annotations, aibrixconst.TrafficScaleInRetryAtAnnotationKey)
	}
	if err = s.cli.Patch(ctx, pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return controllerdrain.Result{}, err
	}
	if s.recorder != nil {
		s.recorder.Eventf(owner, v1.EventTypeNormal, controllerdrain.EventCancelled, "cancelled coordinated drain for pod %s", pod.Name)
	}
	return controllerdrain.Result{Changed: true, RequeueAfter: max(cooldown, trafficDrainPoll)}, nil
}

func (s *StatelessRoleSyncer) cancelUnselectedTraffic(ctx context.Context, owner *orchestrationv1alpha1.RoleSet, active, selected []*v1.Pod) (controllerdrain.Result, error) {
	keep := map[string]bool{}
	for _, pod := range selected {
		keep[string(pod.UID)] = true
	}
	var result controllerdrain.Result
	for _, pod := range active {
		if !protectedTrafficPod(pod) || keep[string(pod.UID)] {
			continue
		}
		if pod.Annotations[aibrixconst.PodDrainReasonAnnotationKey] == aibrixconst.PodDrainReasonRollout {
			continue
		}
		state, err := s.trafficGate().Read(ctx, string(pod.UID), pod.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey])
		if err != nil && !errors.Is(err, trafficdrain.ErrExpired) {
			return result, err
		}
		if state.Phase == trafficdrain.Draining {
			r, err := s.cancelTrafficPod(ctx, owner, pod, state.Generation, 0)
			result.Merge(r)
			if err != nil {
				return result, err
			}
		}
		if state.Phase == trafficdrain.Sealed {
			r, err := s.deleteSealedTrafficPod(ctx, pod)
			result.Merge(r)
			if err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func effectiveTrafficWait(configured time.Duration, pods []*v1.Pod) (time.Duration, error) {
	for _, pod := range pods {
		if !protectedTrafficPod(pod) {
			continue
		}
		wait, err := podTrafficWait(pod)
		if err != nil {
			return 0, err
		}
		configured = max(configured, wait)
	}
	return configured, nil
}

func (s *StatelessRoleSyncer) cancelScaleInDrains(ctx context.Context, owner *orchestrationv1alpha1.RoleSet, active, selected []*v1.Pod) (controllerdrain.Result, error) {
	var legacy []*v1.Pod
	for _, pod := range staleScaleInDrainingPods(active, selected) {
		if !protectedTrafficPod(pod) {
			legacy = append(legacy, pod)
		}
	}
	result, err := controllerdrain.CancelPods(ctx, s.cli, s.recorder, owner, legacy, aibrixconst.PodDrainReasonScaleIn)
	if err != nil {
		return result, err
	}
	coordinated, err := s.cancelUnselectedTraffic(ctx, owner, active, selected)
	result.Merge(coordinated)
	return result, err
}

func (s *StatelessRoleSyncer) coordinatedTrafficDelete(ctx context.Context, owner *orchestrationv1alpha1.RoleSet, pod *v1.Pod, spec *orchestrationv1alpha1.RoleDrainSpec, reason string) (controllerdrain.Result, error) {
	if pod.DeletionTimestamp != nil {
		return controllerdrain.Result{RequeueAfter: trafficDrainPoll}, nil
	}
	wait, err := podTrafficWait(pod)
	if err != nil {
		return controllerdrain.Result{}, err
	}
	gate := s.trafficGate()
	epoch := pod.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey]
	state, err := gate.Start(ctx, string(pod.UID), epoch, wait)
	if errors.Is(err, trafficdrain.ErrExpired) {
		return s.cancelTrafficPod(ctx, owner, pod, state.Generation, wait)
	}
	if errors.Is(err, trafficdrain.ErrConflict) {
		return controllerdrain.Result{RequeueAfter: trafficDrainPoll}, nil
	}
	if err != nil {
		return controllerdrain.Result{}, err
	}
	if state.Phase == trafficdrain.Sealed {
		return s.deleteSealedTrafficPod(ctx, pod)
	}
	minimum := time.Duration(0)
	if spec != nil && spec.TimeoutSeconds != nil {
		minimum = time.Duration(*spec.TimeoutSeconds) * time.Second
	}
	start, parseErr := time.Parse(time.RFC3339, pod.Annotations[aibrixconst.PodDrainStartTimeAnnotationKey])
	if pod.Annotations[aibrixconst.PodDrainingAnnotationKey] != "true" || parseErr != nil || start.After(time.Now()) {
		before := pod.DeepCopy()
		pod.Annotations[aibrixconst.PodDrainingAnnotationKey] = "true"
		pod.Annotations[aibrixconst.PodDrainStartTimeAnnotationKey] = time.Now().UTC().Format(time.RFC3339)
		pod.Annotations[aibrixconst.PodDrainReasonAnnotationKey] = reason
		pod.Annotations[aibrixconst.PodDrainTargetActionAnnotationKey] = aibrixconst.PodDrainTargetActionDelete
		if err = s.cli.Patch(ctx, pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return controllerdrain.Result{}, err
		}
		return controllerdrain.Result{Changed: true, RequeueAfter: max(minimum, trafficDrainPoll)}, nil
	}
	if time.Since(start) < minimum || state.Active != 0 {
		return controllerdrain.Result{RequeueAfter: trafficDrainPoll}, nil
	}
	// The engine sample is a conservative extra check. Only the atomic seal can
	// authorize deletion; it checks the generation and in-flight count again.
	load, err := readTrafficLoad(ctx, pod)
	if err != nil || load != 0 {
		return controllerdrain.Result{RequeueAfter: trafficDrainPoll}, nil
	}
	_, err = gate.Seal(ctx, string(pod.UID), epoch, state.Generation)
	if errors.Is(err, trafficdrain.ErrExpired) {
		return s.cancelTrafficPod(ctx, owner, pod, state.Generation, wait)
	}
	if errors.Is(err, trafficdrain.ErrConflict) {
		return controllerdrain.Result{RequeueAfter: trafficDrainPoll}, nil
	}
	if err != nil {
		return controllerdrain.Result{}, err
	}
	return s.deleteSealedTrafficPod(ctx, pod)
}

func (s *StatelessRoleSyncer) prepareStatelessScale(ctx context.Context, owner *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) ([]*v1.Pod, time.Duration, controllerdrain.Result, error) {
	wait, err := trafficScaleInTimeout(role)
	if err != nil {
		return nil, 0, controllerdrain.Result{}, err
	}
	cleaned, err := cleanupOrphanPodSets(ctx, s.cli, owner, role)
	if err != nil || cleaned {
		return nil, wait, controllerdrain.Result{Changed: cleaned}, err
	}
	pods, err := getRolePods(ctx, s.cli, owner.Namespace, owner.Name, role.Name)
	if err != nil {
		return nil, wait, controllerdrain.Result{}, err
	}
	prepared, err := s.prepareTrafficEpochs(ctx, owner, pods)
	if err != nil || prepared.Changed {
		return pods, wait, prepared, err
	}
	wait, err = effectiveTrafficWait(wait, pods)
	return pods, wait, prepared, err
}
