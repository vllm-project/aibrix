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

	// HeaderSessionKey carries a caller-owned opaque key for session-affinity routing.
	HeaderSessionKey = "x-aibrix-session-key"
)
