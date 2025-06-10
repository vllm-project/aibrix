/*
Copyright 2024 The Aibrix Team.

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

	"github.com/stretchr/testify/assert"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	loraName = "text2sql-lora-2"
)

func TestModelAdapter(t *testing.T) {
	adapter := createModelAdapterConfig("text2sql-lora-2", "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	t.Cleanup(func() {
		assert.NoError(t, v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{}))
		assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true,
			func(ctx context.Context) (done bool, err error) {
				adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(context.Background(),
					adapter.Name, v1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				return false, nil
			}))
	})

	// create model adapter
	t.Log("creating model adapter")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	assert.NoError(t, err)
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)
	oldPod := adapter.Status.Instances[0]

	// delete pod and ensure model adapter is rescheduled
	t.Log("deleting pod instance to force model adapter rescheduling")
	assert.NoError(t, k8sClient.CoreV1().Pods("default").Delete(context.Background(), oldPod, v1.DeleteOptions{}))
	validateAllPodsAreReady(t, k8sClient, 3)
	time.Sleep(3 * time.Second)
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)
	newPod := adapter.Status.Instances[0]

	assert.NotEqual(t, newPod, oldPod, "ensure old and new pods are different")

	// run inference for model adapter
	validateInference(t, loraName)
}

func createModelAdapterConfig(name, model string) *modelv1alpha1.ModelAdapter {
	return &modelv1alpha1.ModelAdapter{
		ObjectMeta: v1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"model.aibrix.ai/name": name,
				"model.aibrix.ai/port": "8000",
			},
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			BaseModel: &model,
			PodSelector: &v1.LabelSelector{
				MatchLabels: map[string]string{
					"model.aibrix.ai/name": model,
				},
			},
			ArtifactURL: "huggingface://yard1/llama-2-7b-sql-lora-test",
			AdditionalConfig: map[string]string{
				"api-key": "test-key-1234567890",
			},
		},
	}
}

func validateModelAdapter(t *testing.T, client *v1alpha1.Clientset, name string) *modelv1alpha1.ModelAdapter {
	var adapter *modelv1alpha1.ModelAdapter
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = client.ModelV1alpha1().ModelAdapters("default").Get(context.Background(), name, v1.GetOptions{})
			if err != nil || adapter.Status.Phase != modelv1alpha1.ModelAdapterRunning {
				return false, nil
			}
			return true, nil
		}))
	assert.True(t, len(adapter.Status.Instances) > 0, "model adapter scheduled on atleast One pod")
	return adapter
}
