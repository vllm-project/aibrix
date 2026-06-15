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

package pd

import "github.com/bytedance/sonic"

// SonicJSONInt64 unmarshals JSON numbers into map[string]any as int64 (not float64), so large
// integer fields (e.g. ctx_request_id, disagg_request_id in disaggregated_params) survive
// marshal/unmarshal without float64 precision loss.
var SonicJSONInt64 = sonic.Config{UseInt64: true}.Froze()
