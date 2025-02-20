package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/aibrix/aibrix/pkg/client/clientset/versioned"
	"github.com/stretchr/testify/assert"
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
		assert.NoError(t, v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(), adapter.Name, v1.DeleteOptions{}))
		wait.PollImmediate(1*time.Second, 30*time.Second,
			func() (done bool, err error) {
				adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(context.Background(), adapter.Name, v1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				return false, nil
			})
	})

	// create model adapter
	fmt.Println("creating model adapter")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(), adapter, v1.CreateOptions{})
	assert.NoError(t, err)
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)
	oldPod := adapter.Status.Instances[0]

	// delete pod and ensure model adapter is rescheduled
	fmt.Println("deleting pod instance to force model adapter rescheduling")
	assert.NoError(t, k8sClient.CoreV1().Pods("default").Delete(context.Background(), oldPod, v1.DeleteOptions{}))
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
	wait.PollImmediate(1*time.Second, 30*time.Second,
		func() (done bool, err error) {
			adapter, err = client.ModelV1alpha1().ModelAdapters("default").Get(context.Background(), name, v1.GetOptions{})
			if err != nil || adapter.Status.Phase != modelv1alpha1.ModelAdapterRunning {
				return false, nil
			}
			return true, nil
		})
	assert.True(t, len(adapter.Status.Instances) > 0, "model adapter scheduled on atleast one pod")
	return adapter
}
