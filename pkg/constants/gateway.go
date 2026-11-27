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

package constants

const (
	// HeaderSessionID identifies the backend selected for session-affinity routing.
	HeaderSessionID = "x-session-id"

	// HeaderSessionKey carries a caller-owned opaque key, used by session-affinity
	// routing and by the PD router's session-delta prefill charge.
	HeaderSessionKey = "x-aibrix-session-key"

	// EnvGatewayMaxResponseBodyBytes caps the size, in bytes, of a non-streamed
	// response body that the gateway buffers per request before extracting token
	// usage. Once a body grows past the cap the gateway stops buffering it: the default
	// router streams the body, so Envoy has already sent the 200 and the earlier chunks
	// and the request cannot be failed -- instead the remaining chunks are forwarded
	// unchanged and token usage is skipped, so the request goes uncharged (surfaced by
	// the gateway_response_buffer_overflow_total counter). This bounds per-request
	// memory. A value of 0 (or negative) disables the cap, restoring the previous
	// unbounded behavior.
	EnvGatewayMaxResponseBodyBytes = "AIBRIX_GATEWAY_MAX_RESPONSE_BODY_BYTES"
)
