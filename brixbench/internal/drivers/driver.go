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
