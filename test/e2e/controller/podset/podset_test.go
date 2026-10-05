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

package e2e

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

const (
	podSetDrainTimeoutSeconds = int32(30)
	podSetDrainTimeout        = 30 * time.Second
	podSetCancelMinMargin     = 15 * time.Second
	podSetEarlyDeleteMargin   = 2 * time.Second
	podSetTimestampTolerance  = 2 * time.Second
)

func TestPodSetScaleAndDrainLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	harness := newPodSetHarness(t, ctx)
	name := "scale-drain"
	podSet := newPodSet(harness.namespace, name, 2, podSetDrainTimeoutSeconds)
	harness.createPodSet(ctx, t, podSet)

	if _, err := harness.waitForReadyPods(ctx, name, 2); err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForStatus(ctx, name, orchestrationv1alpha1.PodSetPhaseReady, 2, 2); err != nil {
		t.Fatal(err)
	}

	if err := harness.updatePodSetSize(ctx, name, 3); err != nil {
		t.Fatalf("scale PodSet %s/%s to 3: %v", harness.namespace, name, err)
	}
	threePods, err := harness.waitForReadyPods(ctx, name, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForStatus(ctx, name, orchestrationv1alpha1.PodSetPhaseReady, 3, 3); err != nil {
		t.Fatal(err)
	}
	originalUIDs, err := podUIDsByIndex(threePods)
	if err != nil {
		t.Fatalf("snapshot PodSet Pod UIDs: %v", err)
	}

	if err := harness.updatePodSetSize(ctx, name, 2); err != nil {
		t.Fatalf("begin first PodSet scale-down: %v", err)
	}
	drainingPod, err := harness.waitForScaleInDrain(ctx, name, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !drainingPod.DeletionTimestamp.IsZero() {
		t.Fatalf("Pod %s entered deletion before drain timeout", drainingPod.Name)
	}
	firstDrainStart, err := drainStartTime(drainingPod)
	if err != nil {
		t.Fatal(err)
	}
	firstDeadline := firstDrainStart.Add(podSetDrainTimeout)
	if remaining := time.Until(firstDeadline); remaining < podSetCancelMinMargin {
		t.Fatalf("only %s remained before first drain deadline; need at least %s to test cancellation safely",
			remaining, podSetCancelMinMargin)
	}

	if err := harness.updatePodSetSize(ctx, name, 3); err != nil {
		t.Fatalf("restore PodSet before drain timeout: %v", err)
	}
	if err := harness.waitForDrainCancellation(ctx, name, originalUIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.waitForReadyPods(ctx, name, 3); err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForStatus(ctx, name, orchestrationv1alpha1.PodSetPhaseReady, 3, 3); err != nil {
		t.Fatal(err)
	}

	if err := harness.updatePodSetSize(ctx, name, 2); err != nil {
		t.Fatalf("begin second PodSet scale-down: %v", err)
	}
	drainingPod, err = harness.waitForScaleInDrain(ctx, name, 2)
	if err != nil {
		t.Fatal(err)
	}
	drainingUID := drainingPod.UID
	if drainingUID == types.UID("") {
		t.Fatalf("draining Pod %s has an empty UID", drainingPod.Name)
	}
	secondDrainStart, err := drainStartTime(drainingPod)
	if err != nil {
		t.Fatal(err)
	}
	secondDeadline := secondDrainStart.Add(podSetDrainTimeout)
	if err := harness.ensurePodNotDeletingUntil(
		ctx,
		name,
		drainingUID,
		secondDeadline.Add(-podSetEarlyDeleteMargin),
	); err != nil {
		t.Fatal(err)
	}
	deletingPod, err := harness.waitForPodDeleting(ctx, name, drainingUID)
	if err != nil {
		t.Fatal(err)
	}
	earliestDeletion := secondDeadline.Add(-podSetTimestampTolerance)
	if deletingPod.DeletionTimestamp.Time.Before(earliestDeletion) {
		t.Fatalf("Pod %s began deleting at %s, before drain deadline %s (tolerance %s)",
			deletingPod.Name,
			deletingPod.DeletionTimestamp.Time,
			secondDeadline,
			podSetTimestampTolerance,
		)
	}
	if err := harness.waitForPodUIDGone(ctx, name, drainingUID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.waitForReadyPods(ctx, name, 2); err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForStatus(ctx, name, orchestrationv1alpha1.PodSetPhaseReady, 2, 2); err != nil {
		t.Fatal(err)
	}
}

func TestPodSetRecreatesManuallyDeletedPod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	harness := newPodSetHarness(t, ctx)
	name := "manual-delete"
	podSet := newPodSet(harness.namespace, name, 2, 0)
	harness.createPodSet(ctx, t, podSet)

	pods, err := harness.waitForReadyPods(ctx, name, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForStatus(ctx, name, orchestrationv1alpha1.PodSetPhaseReady, 2, 2); err != nil {
		t.Fatal(err)
	}
	uids, err := podUIDsByIndex(pods)
	if err != nil {
		t.Fatalf("snapshot PodSet Pod UIDs: %v", err)
	}
	deletedUID := uids[1]
	if deletedUID == types.UID("") {
		t.Fatal("PodSet did not create an index-1 Pod")
	}
	if err := harness.deletePodByUID(ctx, name, deletedUID); err != nil {
		t.Fatalf("delete PodSet Pod with UID %s: %v", deletedUID, err)
	}

	replacement, err := harness.waitForReplacement(ctx, name, 1, deletedUID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.UID == deletedUID {
		t.Fatalf("replacement Pod retained deleted UID %s", deletedUID)
	}
	if err := harness.ensureStableReadyPods(ctx, name, 2, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForStatus(ctx, name, orchestrationv1alpha1.PodSetPhaseReady, 2, 2); err != nil {
		t.Fatal(err)
	}
}

func TestPodSetDeletionCleansOwnedPods(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	harness := newPodSetHarness(t, ctx)
	name := "foreground-delete"
	podSet := newPodSet(harness.namespace, name, 2, 0)
	harness.createPodSet(ctx, t, podSet)

	if _, err := harness.waitForReadyPods(ctx, name, 2); err != nil {
		t.Fatal(err)
	}
	if err := harness.deletePodSetForeground(ctx, name); err != nil {
		t.Fatalf("foreground-delete PodSet %s/%s: %v", harness.namespace, name, err)
	}
	if err := harness.waitForPodSetGone(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForNoPods(ctx, name); err != nil {
		t.Fatal(err)
	}
}
