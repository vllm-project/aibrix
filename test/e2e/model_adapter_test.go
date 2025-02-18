package e2e

import (
	"context"
	"testing"
	"time"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	"github.com/openai/openai-go"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	loraName = "text2sql-lora-2"
)

func TestModelAdapter(t *testing.T) {
	var adapter *modelv1alpha1.ModelAdapter
	var err error
	adapter = createModelAdapterConfig("text2sql-lora-2", "llama2-7b")
	_, v1alpha1Client := initializeClient(context.Background(), t)

	t.Cleanup(func() {
		assert.NoError(t, v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(), adapter.Name, v1.DeleteOptions{}))
	})

	adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(), adapter, v1.CreateOptions{})
	assert.NoError(t, err)

	wait.PollImmediate(1*time.Second, 30*time.Second,
		func() (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(context.Background(), adapter.Name, v1.GetOptions{})
			if err != nil || adapter.Status.Phase != modelv1alpha1.ModelAdapterRunning {
				return false, nil
			}
			return true, nil
		})

	assert.True(t, len(adapter.Status.Instances) > 0, "model adapter scheduled on atleast one pod")

	client := createOpenAIClient(baseURL, apiKey)
	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		}),
		Model: openai.F(openai.ChatModel(loraName)),
	})
	if err != nil {
		t.Fatalf("chat completions failed: %v", err)
	}

	assert.Equal(t, loraName, chatCompletion.Model)
	assert.NotEmpty(t, chatCompletion.Choices, "chat completion has no choices returned")
	assert.NotNil(t, chatCompletion.Choices[0].Message.Content, "chat completion has no message returned")
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
