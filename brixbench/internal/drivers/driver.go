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

package drivers

import "context"

// Driver is an abstraction for running various benchmark tools like vllm-bench, genai-perf, etc.
type Driver interface {
	// Run executes the benchmark based on the provided configuration file or script path.
	// logDir specifies where to save the process output logs for debugging.
	Run(ctx context.Context, benchmarkPath string, logDir string) error

	// CollectMetrics parses and returns the benchmark execution results.
	CollectMetrics() (map[string]interface{}, error)

	// ResultPath returns the parsed benchmark result file path for the latest run.
	ResultPath() string
}
