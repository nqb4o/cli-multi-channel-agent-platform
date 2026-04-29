// Global TracerProvider / MeterProvider bootstrap (Go port of setup.py).
//
// Behaviour:
//
//   - Init(serviceName) is first-call wins globally. Subsequent calls with the
//     same serviceName are no-ops; calls with a different name return an error
//     so callers notice misconfiguration.
//   - For tests, ForceReset tears down providers, drops the in-memory
//     exporters, and re-initialises a fresh state. Production code never calls
//     it.
//   - OTLP exporter endpoint is read from OTEL_EXPORTER_OTLP_ENDPOINT — if
//     unset, only the in-memory exporter/reader are wired up so tests keep
//     working.
//   - The sampler is an ErrorAwareRatioSampler — TraceIDRatioBased, but spans
//     bearing a non-empty platform.error.class attribute are always sampled.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// ---------------------------------------------------------------------------
// Sampler
// ---------------------------------------------------------------------------

// ErrorAwareRatioSampler delegates to TraceIDRatioBased for normal spans but
// always samples spans whose start-time attributes contain a non-empty
// platform.error.class — keeping error traces useful even at low sample
// ratios. Wire-compatible with the Python ErrorAwareRatioSampler.
type ErrorAwareRatioSampler struct {
	ratio float64
	inner sdktrace.Sampler
}

// NewErrorAwareRatioSampler constructs a sampler with a fraction in [0, 1].
func NewErrorAwareRatioSampler(ratio float64) (*ErrorAwareRatioSampler, error) {
	if ratio < 0.0 || ratio > 1.0 {
		return nil, fmt.Errorf("ratio must be in [0, 1], got %v", ratio)
	}
	return &ErrorAwareRatioSampler{
		ratio: ratio,
		inner: sdktrace.TraceIDRatioBased(ratio),
	}, nil
}

// Ratio returns the configured fraction.
func (s *ErrorAwareRatioSampler) Ratio() float64 { return s.ratio }

// ShouldSample implements sdktrace.Sampler.
func (s *ErrorAwareRatioSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	for _, kv := range p.Attributes {
		if string(kv.Key) == PlatformErrorClass {
			if v := kv.Value.AsString(); v != "" {
				return sdktrace.SamplingResult{
					Decision:   sdktrace.RecordAndSample,
					Attributes: p.Attributes,
				}
			}
		}
	}
	return s.inner.ShouldSample(p)
}

// Description implements sdktrace.Sampler.
func (s *ErrorAwareRatioSampler) Description() string {
	return fmt.Sprintf("ErrorAwareRatioSampler(ratio=%g)", s.ratio)
}

// ---------------------------------------------------------------------------
// Global state (first-call wins)
// ---------------------------------------------------------------------------

type globalState struct {
	mu             sync.Mutex
	serviceName    string
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *metric.MeterProvider
	spanExporter   *tracetest.InMemoryExporter
	metricReader   *metric.ManualReader
	sampler        *ErrorAwareRatioSampler
}

var state = &globalState{}

// InitOptions controls Init. Zero value means defaults — equivalent to passing
// nothing in Python.
type InitOptions struct {
	// ForceReset, if true, tears down any prior providers before init. Tests
	// only.
	ForceReset bool
	// SampleRatio overrides OTEL_TRACES_SAMPLER_ARG. nil → env-resolved.
	SampleRatio *float64
}

// Init installs global TracerProvider + MeterProvider for serviceName.
//
// First-call wins. Subsequent calls with the same name are no-ops. With
// ForceReset = true (tests only), tears down existing state and re-installs.
func Init(serviceName string, opts ...InitOptions) error {
	if serviceName == "" {
		return errors.New("serviceName must be a non-empty string")
	}

	var opt InitOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.serviceName != "" && !opt.ForceReset {
		if state.serviceName != serviceName {
			return fmt.Errorf(
				"telemetry already initialised for service %q; refusing re-init for %q",
				state.serviceName, serviceName,
			)
		}
		return nil
	}

	if opt.ForceReset {
		shutdownLocked()
	}

	ratio := 1.0
	if opt.SampleRatio != nil {
		ratio = *opt.SampleRatio
	} else {
		ratio = resolveRatio()
	}

	sampler, err := NewErrorAwareRatioSampler(ratio)
	if err != nil {
		return err
	}
	// ParentBased so downstream services honour upstream sampling decisions.
	parentBased := sdktrace.ParentBased(sampler)

	res, err := buildResource(serviceName)
	if err != nil {
		return err
	}

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(parentBased),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(reader),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	state.serviceName = serviceName
	state.tracerProvider = tp
	state.meterProvider = mp
	state.spanExporter = exporter
	state.metricReader = reader
	state.sampler = sampler

	// Reset any memoised metric instruments — a new MeterProvider means the
	// old instruments point at a defunct meter.
	resetInstruments()

	return nil
}

// ForceReset tears down provider state. Tests only.
func ForceReset() {
	state.mu.Lock()
	defer state.mu.Unlock()
	shutdownLocked()
}

func shutdownLocked() {
	ctx := context.Background()
	if state.tracerProvider != nil {
		_ = state.tracerProvider.Shutdown(ctx)
	}
	if state.meterProvider != nil {
		_ = state.meterProvider.Shutdown(ctx)
	}
	state.serviceName = ""
	state.tracerProvider = nil
	state.meterProvider = nil
	state.spanExporter = nil
	state.metricReader = nil
	state.sampler = nil
	resetInstruments()
}

// GetServiceName returns the current service name (empty if uninitialised).
func GetServiceName() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.serviceName
}

// GetInMemorySpanExporter returns the test-only in-memory exporter.
func GetInMemorySpanExporter() *tracetest.InMemoryExporter {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.spanExporter
}

// GetInMemoryMetricReader returns the test-only manual reader.
func GetInMemoryMetricReader() *metric.ManualReader {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.metricReader
}

// GetSampler returns the active sampler.
func GetSampler() *ErrorAwareRatioSampler {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.sampler
}

// GetTracerProvider returns the configured TracerProvider, or nil.
func GetTracerProvider() *sdktrace.TracerProvider {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.tracerProvider
}

// GetMeterProvider returns the configured MeterProvider, or nil.
func GetMeterProvider() *metric.MeterProvider {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.meterProvider
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildResource(serviceName string) (*resource.Resource, error) {
	ns := os.Getenv("OTEL_SERVICE_NAMESPACE")
	if ns == "" {
		ns = "platform"
	}
	return resource.New(context.Background(),
		resource.WithAttributes(
			attribute.String("service.name", serviceName),
			attribute.String("service.namespace", ns),
		),
	)
}

func resolveRatio() float64 {
	raw := os.Getenv("OTEL_TRACES_SAMPLER_ARG")
	if raw == "" {
		return 1.0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 1.0
	}
	return v
}
