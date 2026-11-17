package inference

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"

	"github.com/joustmania/agent/internal/genaimetrics"
)

// GenAI client metrics (semconv gen_ai.client.operation.duration and
// gen_ai.client.token.usage) for the offline proposer/retro calls, so their
// token spend shows up next to the in-game decide backends' (same metric names,
// told apart by the instrumentation scope). Instruments are created once from
// the global meter provider; the global delegates to the real provider once
// otel.go installs it, so creation order does not matter.
var (
	metricsOnce sync.Once
	opDuration  genaiconv.ClientOperationDuration
	tokenUsage  genaiconv.ClientTokenUsage
)

func instruments() (genaiconv.ClientOperationDuration, genaiconv.ClientTokenUsage) {
	metricsOnce.Do(func() {
		meter := otel.Meter(instrumentationName)
		// On error the constructors return a no-op instrument; metrics are best
		// effort and never fail a call.
		opDuration, _ = genaiconv.NewClientOperationDuration(meter, genaimetrics.OperationDurationBucketsOption())
		tokenUsage, _ = genaiconv.NewClientTokenUsage(meter, genaimetrics.TokenUsageBucketsOption())
	})
	return opDuration, tokenUsage
}

func (c *Claude) metricAttrs(requestModel, responseModel string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		opDuration.AttrRequestModel(requestModel),
		opDuration.AttrServerAddress(c.host),
		opDuration.AttrServerPort(c.port),
	}
	if responseModel != "" {
		attrs = append(attrs, opDuration.AttrResponseModel(responseModel))
	}
	return attrs
}

// recordDuration records gen_ai.client.operation.duration for one call; errType
// is empty on success.
func (c *Claude) recordDuration(ctx context.Context, requestModel, responseModel, errType string, d time.Duration) {
	dur, _ := instruments()
	attrs := c.metricAttrs(requestModel, responseModel)
	if errType != "" {
		attrs = append(attrs, dur.AttrErrorType(genaiconv.ErrorTypeAttr(errType)))
	}
	dur.Record(context.WithoutCancel(ctx), d.Seconds(),
		genaiconv.OperationNameChat, genaiconv.ProviderNameAnthropic, attrs...)
}

// recordTokens records gen_ai.client.token.usage (input incl. cached, and
// output) for one call that returned usage.
func (c *Claude) recordTokens(ctx context.Context, requestModel, responseModel string, input, output int) {
	_, usage := instruments()
	attrs := c.metricAttrs(requestModel, responseModel)
	ctx = context.WithoutCancel(ctx)
	usage.Record(ctx, int64(input), genaiconv.OperationNameChat, genaiconv.ProviderNameAnthropic,
		genaiconv.TokenTypeInput, attrs...)
	usage.Record(ctx, int64(output), genaiconv.OperationNameChat, genaiconv.ProviderNameAnthropic,
		genaiconv.TokenTypeOutput, attrs...)
}
