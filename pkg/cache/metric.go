package cache

type MetricValue struct {
	Value     float64          // For simple metrics (e.g., gauge or counter)
	Histogram *HistogramMetric // For histogram metrics
}

type HistogramMetric struct {
	Sum     float64
	Count   float64
	Buckets map[string]float64
}
