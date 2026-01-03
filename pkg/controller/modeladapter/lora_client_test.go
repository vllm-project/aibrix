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

package modeladapter

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLoadAdapter(t *testing.T) {
	tests := []struct {
		name               string
		enableSidecar      bool
		ma                 *modelv1alpha1.ModelAdapter
		pod                *corev1.Pod
		port               int
		modelApiStatusCode int
		modelApiResponse   string
		loadApiStatusCode  int
		loadApiWantUrl     string

		wantErr    bool
		wantExists bool
		wantLoaded bool
	}{
		{
			name:          "pod with vllm and without sidecar - model loaded ok",
			enableSidecar: false,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:               newPod("127.0.0.1", VLLMEngine, false),
			port:              8000,
			modelApiResponse:  prepareModelApiResponseWithOneModel("vllm", "qwen2-5-0-5b"),
			loadApiStatusCode: 200,
			loadApiWantUrl:    "/v1/load_lora_adapter",
			wantErr:           false,
			wantExists:        false,
			wantLoaded:        true,
		},
		{
			name:          "pod with vllm and without sidecar - model exists ok",
			enableSidecar: false,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:               newPod("127.0.0.1", VLLMEngine, false),
			port:              8000,
			modelApiResponse:  prepareModelApiResponseWithTwoModels("vllm", "qwen2-5-0-5b", "qwen-lora-test"),
			loadApiStatusCode: 200,
			wantErr:           false,
			wantExists:        true,
			wantLoaded:        false,
		},
		{
			name:          "pod with vllm and without sidecar - model loaded returns error",
			enableSidecar: false,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:               newPod("127.0.0.1", VLLMEngine, false),
			port:              8000,
			modelApiResponse:  prepareModelApiResponseWithOneModel("vllm", "qwen2-5-0-5b"),
			loadApiStatusCode: 500,
			loadApiWantUrl:    "/v1/load_lora_adapter",
			wantErr:           true,
			wantExists:        false,
			wantLoaded:        false,
		},
		{
			name:          "pod with vllm and without sidecar - get model returns error",
			enableSidecar: false,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:                newPod("127.0.0.1", VLLMEngine, false),
			port:               8000,
			modelApiStatusCode: 500,
			wantErr:            true,
			wantExists:         false,
			wantLoaded:         false,
		},
		// other tests
		{
			name:          "pod with vllm and with sidecar - model loaded ok",
			enableSidecar: true,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:               newPod("127.0.0.1", VLLMEngine, true),
			port:              8080,
			modelApiResponse:  prepareModelApiResponseWithOneModel("vllm", "qwen2-5-0-5b"),
			loadApiStatusCode: 200,
			loadApiWantUrl:    "/v1/lora_adapter/load",
			wantErr:           false,
			wantExists:        false,
			wantLoaded:        true,
		},
		{
			name:          "pod with sglang and without sidecar - model loaded ok",
			enableSidecar: false,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:               newPod("127.0.0.1", SGLangEngine, false),
			port:              8000,
			modelApiResponse:  prepareModelApiResponseWithOneModel("vllm", "qwen2-5-0-5b"),
			loadApiStatusCode: 200,
			loadApiWantUrl:    "/load_lora_adapter",
			wantErr:           false,
			wantExists:        false,
			wantLoaded:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := mockServer(t, tt.pod.Status.PodIP, tt.port, func(w http.ResponseWriter, r *http.Request) {
				// GET model API
				if r.URL.Path == `/v1/models` {
					if tt.modelApiResponse != "" {
						_, _ = w.Write([]byte(tt.modelApiResponse))
					} else {
						w.WriteHeader(tt.modelApiStatusCode)
					}
					return
				}

				// POST load adapter API
				if r.URL.Path != tt.loadApiWantUrl {
					t.Errorf("load api path mis-match, want=%s, got=%s", tt.loadApiWantUrl, r.URL.Path)
				} else {
					w.WriteHeader(tt.loadApiStatusCode)
				}
			})
			defer server.Close()

			loraClient := NewLoraClient(
				config.RuntimeConfig{
					EnableRuntimeSidecar: tt.enableSidecar,
				},
			)

			loaded, exists, err := loraClient.LoadAdapter(tt.ma, tt.pod) // test

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantExists, exists)
				assert.Equal(t, tt.wantLoaded, loaded)
			}
		})
	}
}

func TestUnloadAdapter(t *testing.T) {
	tests := []struct {
		name             string
		enableSidecar    bool
		ma               *modelv1alpha1.ModelAdapter
		pod              *corev1.Pod
		port             int
		serverCalled     bool
		unloadApiWantUrl string
		wantErr          bool
	}{
		{
			name:          "pod with vllm and without sidecar - model unloaded ok",
			enableSidecar: false,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:              newPod("127.0.0.1", VLLMEngine, false),
			port:             8000,
			unloadApiWantUrl: "/v1/unload_lora_adapter",
			wantErr:          false,
		},
		{
			name:          "pod with vllm and with sidecar - model unloaded ok",
			enableSidecar: true,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:              newPod("127.0.0.1", VLLMEngine, true),
			port:             8080,
			unloadApiWantUrl: "/v1/lora_adapter/unload",
			wantErr:          false,
		},
		{
			name:          "pod with sglang and without sidecar - model unloaded ok",
			enableSidecar: false,
			ma: &modelv1alpha1.ModelAdapter{
				ObjectMeta: v1.ObjectMeta{
					Name: "qwen-lora-test",
				},
			},
			pod:              newPod("127.0.0.1", SGLangEngine, false),
			port:             8000,
			unloadApiWantUrl: "/unload_lora_adapter",
			wantErr:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := mockServer(t, tt.pod.Status.PodIP, tt.port, func(w http.ResponseWriter, r *http.Request) {
				tt.serverCalled = true
				assert.Equal(t, tt.unloadApiWantUrl, r.URL.Path)
			})
			defer server.Close()

			loraClient := NewLoraClient(
				config.RuntimeConfig{
					EnableRuntimeSidecar: tt.enableSidecar,
				},
			)

			err := loraClient.UnloadAdapter(tt.ma, tt.pod) // test

			assert.True(t, tt.serverCalled)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func newPod(ip string, engineType string, enableSidecar bool) *corev1.Pod {
	containers := []corev1.Container{
		{Name: "llm-model"},
	}
	if enableSidecar {
		containers = append(containers, corev1.Container{Name: "aibrix-runtime"})
	}
	return &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-pod",
			Labels: map[string]string{
				constants.ModelLabelEngine: engineType,
			},
		},
		Spec: corev1.PodSpec{
			Containers: containers,
		},
		Status: corev1.PodStatus{
			PodIP: ip,
		},
	}
}

func mockServer(t *testing.T, ip string, port int, handler http.HandlerFunc) *httptest.Server {
	addr := fmt.Sprintf("%s:%d", ip, port)

	ts := httptest.NewUnstartedServer(handler)
	_ = ts.Listener.Close()

	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Failed to listen on %s: %v", addr, err)
	}
	ts.Listener = l

	ts.Start()
	return ts
}

func prepareModelApiResponseWithOneModel(engine string, model string) string {
	return fmt.Sprintf(`{
  "object": "list",
  "data": [
    {
      "id": "%s",
      "object": "model",
      "created": 1765479369,
      "owned_by": "%s",
      "root": "dummy/path",
      "parent": null,
      "max_model_len": 32768
    }
  ]
}`, model, engine)
}

func prepareModelApiResponseWithTwoModels(engine string, model1 string, model2 string) string {
	return fmt.Sprintf(`{
  "object": "list",
  "data": [
    {
      "id": "%s",
      "object": "model",
      "created": 1765479369,
      "owned_by": "%s",
      "root": "dummy/path",
      "parent": null,
      "max_model_len": 32768
    },
    {
      "id": "%s",
      "object": "model",
      "created": 1765479369,
      "owned_by": "%s",
      "root": "dummy/path",
      "parent": "qwen-coder-1-5b-instruct",
      "max_model_len": null
    }
  ]
}`, model1, engine, model2, engine)
}
