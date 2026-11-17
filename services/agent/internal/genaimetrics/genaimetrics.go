// Package genaimetrics holds the histogram bucket boundaries the GenAI semantic
// conventions advise for gen_ai.client.operation.duration and
// gen_ai.client.token.usage, shared by every agent package that records them:
// the decide backends (through decide/backends/internal/chathttp and the claude
// backend) and the offline inference backend.
package genaimetrics

import "go.opentelemetry.io/otel/metric"

// OperationDurationBuckets and TokenUsageBuckets are the advised boundaries.
// genaiconv does not apply them, so without these the SDK defaults
// (0, 5, 10, 25, ...) are used (#1434).
var (
	OperationDurationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	TokenUsageBuckets        = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
)

// OperationDurationBucketsOption sets OperationDurationBuckets on a histogram.
func OperationDurationBucketsOption() metric.Float64HistogramOption {
	return metric.WithExplicitBucketBoundaries(OperationDurationBuckets...)
}

// TokenUsageBucketsOption sets TokenUsageBuckets on a histogram.
func TokenUsageBucketsOption() metric.Int64HistogramOption {
	return metric.WithExplicitBucketBoundaries(TokenUsageBuckets...)
}
