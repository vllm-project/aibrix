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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	unplacedClaim = "unplaced-claim"
	unplacedModel = "unplaced-model"
	// No pod carries this pool label, so a claim that selects it is never
	// placed.
	unplacedPoolName = "modelclaim-e2e-no-such-pool"
)

// TestModelClaimNotPlacedYetIsRetryable checks the answer for the model of a
// claim that is not placed. The gateway knows the claim from the ModelClaim
// object alone, since no pod advertises it, and answers 503 with Retry-After
// rather than 400. Once the claim is deleted, the model is one that nobody
// serves again.
func TestModelClaimNotPlacedYetIsRetryable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, aibrixClient := initializeClient(ctx, t)
	claims := aibrixClient.ModelV1alpha1().ModelClaims(lifecycleNamespace)

	// A claim left by an earlier run keeps its name until its finalizer is
	// gone, so it is deleted and waited for before this one is made.
	_ = claims.Delete(ctx, unplacedClaim, metav1.DeleteOptions{})
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := claims.Get(ctx, unplacedClaim, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		}), "a leftover %s was not deleted", unplacedClaim)
	_, err := claims.Create(ctx, &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: unplacedClaim, Namespace: lifecycleNamespace},
		Spec: modelv1alpha1.ModelClaimSpec{
			ModelName: ptr.To(unplacedModel),
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				constants.ModelPoolLabelName: unplacedPoolName,
			}},
			ArtifactURL: "huggingface://aibrix/" + unplacedModel,
			Engine:      "vllm",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = claims.Delete(context.Background(), unplacedClaim, metav1.DeleteOptions{})
	})

	// The reason is whatever the controller gives on the claim's Scheduled
	// condition, so only the phase is checked.
	var lastStatus int
	var lastBody, retryAfter string
	err = wait.PollUntilContextTimeout(ctx, time.Second, 60*time.Second, true,
		func(context.Context) (bool, error) {
			response, body, err := sendLifecycleModelRequest(unplacedModel)
			if err != nil {
				lastBody = err.Error()
				return false, nil
			}
			lastStatus = response.StatusCode
			lastBody = string(body)
			retryAfter = response.Header.Get("Retry-After")
			_ = response.Body.Close()
			return lastStatus == http.StatusServiceUnavailable &&
				strings.Contains(lastBody, "model "+unplacedModel+" is pending ("), nil
		})
	require.NoError(
		t, err, "model %s was not answered as pending; last status=%d body=%s",
		unplacedModel, lastStatus, lastBody,
	)
	assert.Equal(t, "10", retryAfter)

	require.NoError(t, claims.Delete(ctx, unplacedClaim, metav1.DeleteOptions{}))
	waitForLifecycleModelStatus(t, unplacedModel, http.StatusBadRequest, 60*time.Second)
}
