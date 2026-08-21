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

package gateway

import (
	"context"
	"strings"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/trace"

	routingalgorithms "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

var redisClient = redis.NewClient(&redis.Options{})

// Test_handleRequestHeaders tests the HandleRequestHeaders function for various scenarios
func Test_handleRequestHeaders(t *testing.T) {
	// Initialize routing algorithms
	routingalgorithms.Init()

	// testResponse represents the expected response values from HandleRequestHeaders
	type testResponse struct {
		statusCode envoyTypePb.StatusCode
		headers    []*configPb.HeaderValueOption
		routingCtx *types.RoutingContext
		user       utils.User
		rpm        int64
	}

	// testCase represents a test case with its validation function
	type testCase struct {
		name           string
		requestHeaders []*configPb.HeaderValue
		user           utils.User
		expected       testResponse
		validate       func(*testing.T, *testCase, *extProcPb.ProcessingResponse, utils.User, *types.RoutingContext, int64)
	}

	// defaultSuccessValidator provides common assertions for successful request header processing.
	defaultSuccessValidator := func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, user utils.User, routingCtx *types.RoutingContext, rpm int64) {
		assert.Equal(t, tt.expected.statusCode, envoyTypePb.StatusCode_OK)
		assert.Equal(t, tt.expected.headers, resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders())
		assert.Equal(t, tt.expected.user, user)
		assert.NotNil(t, routingCtx)
		assert.Equal(t, tt.expected.routingCtx.ReqPath, routingCtx.ReqPath)
		assert.Equal(t, tt.expected.routingCtx.ReqHeaders, routingCtx.ReqHeaders)
		assert.Equal(t, tt.expected.rpm, rpm)
	}

	const (
		traceID           = "4bf92f3577b34da6a3ce929d0e0e4736"
		fallbackRequestID = "550e8400-e29b-41d4-a716-446655440000"
	)

	// Define test cases for different routing and error scenarios
	tests := []testCase{
		{
			name: "invalid strategy - passes through to request body (validation deferred)",
			requestHeaders: []*configPb.HeaderValue{
				{
					Key:      HeaderRoutingStrategy,
					RawValue: []byte("not-found-strategy"),
				},
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
				},
				routingCtx: &types.RoutingContext{
					ReqHeaders: map[string]string{HeaderRoutingStrategy: "not-found-strategy"},
				},
				user: utils.User{},
				rpm:  0,
			},
			validate: defaultSuccessValidator,
		},
		{
			name: "not found user in redis cache - should return error",
			requestHeaders: []*configPb.HeaderValue{
				{
					Key:      userKey,
					RawValue: []byte("test-user"),
				},
				{
					Key:      HeaderRoutingStrategy,
					RawValue: []byte("random"),
				},
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_InternalServerError,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderErrorUser, RawValue: []byte("true")}},
					{Header: &configPb.HeaderValue{Key: "Content-Type", Value: "application/json"}},
				},
				routingCtx: nil,
				user:       utils.User{},
				rpm:        0,
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, user utils.User, routingCtx *types.RoutingContext, rpm int64) {
				// Validate request headers info
				assert.Equal(t, tt.expected.statusCode, resp.GetImmediateResponse().GetStatus().GetCode())
				assert.Equal(t, tt.expected.headers, resp.GetImmediateResponse().GetHeaders().GetSetHeaders())
				assert.Equal(t, tt.expected.user, user)
				assert.Nil(t, routingCtx)
				assert.Equal(t, tt.expected.rpm, rpm)
				// Verify no special headers are set
				for _, header := range resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders() {
					assert.NotEqual(t, HeaderWentIntoReqHeaders, header.Header.Key)
				}
			},
		},
		{
			name: "empty username to request should return request info",
			requestHeaders: []*configPb.HeaderValue{
				{
					Key:      userKey,
					RawValue: []byte(""),
				},
				{
					Key:      pathKey,
					RawValue: []byte("test-path"),
				},
				{
					Key:      authorizationKey,
					RawValue: []byte("token:test-token"),
				},
				{
					Key:      HeaderRoutingStrategy,
					RawValue: []byte("random"),
				},
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
				},
				routingCtx: &types.RoutingContext{
					ReqPath:    "test-path",
					ReqHeaders: map[string]string{authorizationKey: "token:test-token", HeaderRoutingStrategy: "random"},
				},
				user: utils.User{},
				rpm:  0,
			},
			validate: defaultSuccessValidator,
		},
		{
			name: "extract mixed-case x-session-id for session affinity routing",
			requestHeaders: []*configPb.HeaderValue{
				{
					Key:      strings.ToUpper(HeaderSessionID),
					RawValue: []byte("MTI3LjAuMC4xOjg4OTk="),
				},
				{
					Key:      HeaderRoutingStrategy,
					RawValue: []byte("session-affinity"),
				},
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
				},
				routingCtx: &types.RoutingContext{
					ReqHeaders: map[string]string{
						HeaderSessionID:       "MTI3LjAuMC4xOjg4OTk=",
						HeaderRoutingStrategy: "session-affinity",
					},
				},
				user: utils.User{},
				rpm:  0,
			},
			validate: defaultSuccessValidator,
		},
		{
			name: "extract mixed-case opaque session key for session affinity routing",
			requestHeaders: []*configPb.HeaderValue{
				{
					Key:      strings.ToUpper(HeaderSessionKey),
					RawValue: []byte("agent-session-42"),
				},
				{
					Key:      HeaderRoutingStrategy,
					RawValue: []byte("session-affinity"),
				},
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
				},
				routingCtx: &types.RoutingContext{
					ReqHeaders: map[string]string{
						HeaderSessionKey:      "agent-session-42",
						HeaderRoutingStrategy: "session-affinity",
					},
				},
				user: utils.User{},
				rpm:  0,
			},
			validate: defaultSuccessValidator,
		},
		{
			name: "inherit trace ID from traceparent when root span has no trace ID",
			requestHeaders: []*configPb.HeaderValue{
				{
					Key:      HeaderTraceParent,
					Value:    "00-" + traceID + "-00f067aa0ba902b7-01",
					RawValue: []byte("00-" + traceID + "-00f067aa0ba902b7-01"),
				},
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
				},
				routingCtx: &types.RoutingContext{
					ReqHeaders: map[string]string{
						HeaderTraceParent: "00-" + traceID + "-00f067aa0ba902b7-01",
					},
				},
				user: utils.User{},
				rpm:  0,
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, user utils.User, routingCtx *types.RoutingContext, rpm int64) {
				defaultSuccessValidator(t, tt, resp, user, routingCtx, rpm)
				assert.Equal(t, traceID, routingCtx.RequestID)
				assert.NotEqual(t, fallbackRequestID, routingCtx.RequestID)
			},
		},
	}

	for _, tt := range tests {
		// Run each test case as a subtest
		t.Run(tt.name, func(subtest *testing.T) {
			subtest.Parallel()
			// Add panic recovery for subtests too
			subtest.Cleanup(func() {
				if r := recover(); r != nil {
					subtest.Errorf("Subtest %v panicked: %v", tt.name, r)
					subtest.FailNow()
				}
			})

			// Create server with mock cache
			server := &Server{
				redisClient: redisClient,
			}

			// Create request for the test case
			req := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{Headers: tt.requestHeaders},
					},
				},
			}

			rootSpan := trace.SpanFromContext(context.Background())
			resp, user, rpm, routingCtx := server.HandleRequestHeaders(
				context.Background(),
				fallbackRequestID,
				rootSpan,
				req,
			)

			// Validate response using test-specific validation function
			tt.validate(subtest, &tt, resp, user, routingCtx, rpm)
		})
	}
}

func TestHandleRequestHeaders_PrefersRootSpanTraceIDOverTraceparent(t *testing.T) {
	const (
		rootTraceID   = "4bf92f3577b34da6a3ce929d0e0e4736"
		headerTraceID = "70f5c2d315194bb7a3ec6ad92e15b131"
		spanID        = "00f067aa0ba902b7"
	)

	parsedRootTraceID, err := trace.TraceIDFromHex(rootTraceID)
	if !assert.NoError(t, err) {
		return
	}
	parsedSpanID, err := trace.SpanIDFromHex(spanID)
	if !assert.NoError(t, err) {
		return
	}

	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: parsedRootTraceID,
		SpanID:  parsedSpanID,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	rootSpan := trace.SpanFromContext(ctx)

	traceparent := "00-" + headerTraceID + "-" + spanID + "-01"
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{
						Key:      HeaderTraceParent,
						Value:    traceparent,
						RawValue: []byte(traceparent),
					},
				}},
			},
		},
	}

	server := &Server{}
	requestID := rootSpan.SpanContext().TraceID().String()
	_, _, _, routingCtx := server.HandleRequestHeaders(ctx, requestID, rootSpan, req)

	if assert.NotNil(t, routingCtx) {
		assert.Equal(t, rootTraceID, routingCtx.RequestID)
		assert.NotEqual(t, headerTraceID, routingCtx.RequestID)
		assert.Equal(t, traceparent, routingCtx.ReqHeaders[HeaderTraceParent])
	}
}

func TestHandleRequestHeadersBearerTokenAuth(t *testing.T) {
	t.Run("valid bearer token is accepted and preserved for upstream", func(t *testing.T) {
		server := &Server{
			apiKeyAuth: &apiKeyAuthConfig{
				token: "secret-1",
			},
		}

		req := &extProcPb.ProcessingRequest{
			Request: &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
						{Key: "Authorization", RawValue: []byte("Bearer secret-1")},
						{Key: pathKey, RawValue: []byte(PathCompletions)},
						{Key: HeaderRoutingStrategy, RawValue: []byte("random")},
					}},
				},
			},
		}

		rootSpan := trace.SpanFromContext(context.TODO())
		resp, user, rpm, routingCtx := server.HandleRequestHeaders(context.Background(), "test-request-id", rootSpan, req)

		assert.Nil(t, resp.GetImmediateResponse())
		assert.Equal(t, utils.User{}, user)
		assert.EqualValues(t, 0, rpm)
		assert.Equal(t, PathCompletions, routingCtx.ReqPath)
		assert.Equal(t, map[string]string{"Authorization": "Bearer secret-1", HeaderRoutingStrategy: "random"}, routingCtx.ReqHeaders)
		assert.Empty(t, resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders())
	})

	t.Run("missing bearer token is rejected", func(t *testing.T) {
		server := &Server{
			apiKeyAuth: &apiKeyAuthConfig{
				token: "secret-1",
			},
		}

		req := &extProcPb.ProcessingRequest{
			Request: &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
						{Key: pathKey, RawValue: []byte(PathCompletions)},
					}},
				},
			},
		}

		rootSpan := trace.SpanFromContext(context.TODO())
		resp, user, rpm, routingCtx := server.HandleRequestHeaders(context.Background(), "test-request-id", rootSpan, req)

		assert.Equal(t, envoyTypePb.StatusCode_Unauthorized, resp.GetImmediateResponse().GetStatus().GetCode())
		assert.Equal(t, utils.User{}, user)
		assert.EqualValues(t, 0, rpm)
		assert.Nil(t, routingCtx)
		assert.Contains(t, resp.GetImmediateResponse().GetBody(), ErrorCodeInvalidAPIKey)
		assert.Contains(t, resp.GetImmediateResponse().GetBody(), `"param":"api_key"`)
	})

	t.Run("non-bearer authorization header is rejected", func(t *testing.T) {
		server := &Server{
			apiKeyAuth: &apiKeyAuthConfig{
				token: "secret-1",
			},
		}

		req := &extProcPb.ProcessingRequest{
			Request: &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
						{Key: "Authorization", RawValue: []byte("secret-1")},
						{Key: pathKey, RawValue: []byte(PathCompletions)},
					}},
				},
			},
		}

		rootSpan := trace.SpanFromContext(context.TODO())
		resp, user, rpm, routingCtx := server.HandleRequestHeaders(context.Background(), "test-request-id", rootSpan, req)

		assert.Equal(t, envoyTypePb.StatusCode_Unauthorized, resp.GetImmediateResponse().GetStatus().GetCode())
		assert.Equal(t, utils.User{}, user)
		assert.EqualValues(t, 0, rpm)
		assert.Nil(t, routingCtx)
		assert.Contains(t, resp.GetImmediateResponse().GetBody(), ErrorCodeInvalidAPIKey)
	})

	t.Run("wrong bearer token is rejected", func(t *testing.T) {
		server := &Server{
			apiKeyAuth: &apiKeyAuthConfig{
				token: "secret-1",
			},
		}

		req := &extProcPb.ProcessingRequest{
			Request: &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
						{Key: "Authorization", RawValue: []byte("Bearer wrong-token")},
						{Key: pathKey, RawValue: []byte(PathCompletions)},
					}},
				},
			},
		}

		rootSpan := trace.SpanFromContext(context.TODO())
		resp, user, rpm, routingCtx := server.HandleRequestHeaders(context.Background(), "test-request-id", rootSpan, req)

		assert.Equal(t, envoyTypePb.StatusCode_Unauthorized, resp.GetImmediateResponse().GetStatus().GetCode())
		assert.Equal(t, utils.User{}, user)
		assert.EqualValues(t, 0, rpm)
		assert.Nil(t, routingCtx)
		assert.Contains(t, resp.GetImmediateResponse().GetBody(), ErrorCodeInvalidAPIKey)
		assert.Contains(t, resp.GetImmediateResponse().GetBody(), `"param":"api_key"`)
	})
}
