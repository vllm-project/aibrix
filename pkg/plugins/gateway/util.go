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

package gateway

import (
	"os"
	"strings"

	"github.com/bytedance/sonic"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/openai/openai-go/v3"
	"k8s.io/klog/v2"
)

var (
	POD_NAME = os.Getenv("POD_NAME")
)

const jsonNull = "null"

// getChatCompletionsMessage returns message for chat completions object
func getChatCompletionsMessage(requestID string, chatCompletionObj openai.ChatCompletionNewParams) (string, *extProcPb.ProcessingResponse) {
	if len(chatCompletionObj.Messages) == 0 {
		klog.ErrorS(nil, "no messages in the request body", "requestID", requestID)
		return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "no messages in the request body", "", "messages", HeaderErrorRequestBodyProcessing, "true")
	}
	var builder strings.Builder
	for i, m := range chatCompletionObj.Messages {
		if i > 0 {
			builder.WriteString(" ")
		}
		switch content := m.GetContent().AsAny().(type) {
		case *string:
			builder.WriteString(*content)
		default:
			if jsonBytes, err := sonic.Marshal(content); err == nil {
				builder.Write(jsonBytes)
			} else {
				klog.ErrorS(err, "error marshalling message content", "requestID", requestID, "message", m)
				return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error marshalling message content", "", "messages", HeaderErrorRequestBodyProcessing, "true")
			}
		}
	}
	return builder.String(), nil
}

func buildEnvoyProxyHeaders(headers []*configPb.HeaderValueOption, keyValues ...string) []*configPb.HeaderValueOption {
	if len(keyValues)%2 != 0 {
		return headers
	}

	for i := 0; i < len(keyValues); {
		headers = append(headers,
			&configPb.HeaderValueOption{
				Header: &configPb.HeaderValue{
					Key:      keyValues[i],
					RawValue: []byte(keyValues[i+1]),
				},
			},
		)
		i += 2
	}

	return headers
}
