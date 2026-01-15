package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	ServiceName  string
	OTLPEndpoint string
	Enabled      bool
}

var loggerProvider *sdklog.LoggerProvider

type errorHandler struct{}

func (e errorHandler) Handle(err error) {
	fmt.Fprintf(os.Stderr, "OTEL ERROR: %v\n", err)
}

func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled || cfg.OTLPEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	otel.SetErrorHandler(errorHandler{})

	res, _ := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
		),
	)

	traceExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.OTLPEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(traceExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	logExporter, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpoint(cfg.OTLPEndpoint),
		otlploghttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	loggerProvider = sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExporter)),
		sdklog.WithResource(res),
	)

	return func(ctx context.Context) error {
		tp.Shutdown(ctx)
		if loggerProvider != nil {
			loggerProvider.Shutdown(ctx)
		}
		return nil
	}, nil
}

func Tracer(name string) trace.Tracer {
	return otel.GetTracerProvider().Tracer(name)
}

type VMLogWriter struct {
	FunctionID string
	InstanceID string
	IsStderr   bool
	span       trace.Span
	mu         sync.Mutex
	buffer     []byte
}

func NewVMLogWriter(functionID, instanceID string, isStderr bool) *VMLogWriter {
	return &VMLogWriter{
		FunctionID: functionID,
		InstanceID: instanceID,
		IsStderr:   isStderr,
		buffer:     make([]byte, 0, 4096),
	}
}

func (w *VMLogWriter) SetSpan(span trace.Span) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.span = span
}

func (w *VMLogWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.IsStderr {
		os.Stderr.Write(p)
	} else {
		os.Stdout.Write(p)
	}

	w.buffer = append(w.buffer, p...)

	for {
		idx := -1
		for i, b := range w.buffer {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx == -1 {
			break
		}

		line := string(w.buffer[:idx])
		w.buffer = w.buffer[idx+1:]

		if strings.TrimSpace(line) != "" {
			w.emitLog(line)
		}
	}

	return len(p), nil
}

func (w *VMLogWriter) emitLog(line string) {
	if w.span != nil {
		eventName := "vm.stdout"
		if w.IsStderr {
			eventName = "vm.stderr"
		}
		w.span.AddEvent(eventName, trace.WithAttributes(
			attribute.String("message", line),
			attribute.String("function.id", w.FunctionID),
			attribute.String("instance.id", w.InstanceID),
		))
	}

	if loggerProvider != nil {
		logger := loggerProvider.Logger("vm-output")
		var rec log.Record
		rec.SetTimestamp(time.Now())
		rec.SetBody(log.StringValue(line))
		rec.AddAttributes(
			log.String("function.id", w.FunctionID),
			log.String("instance.id", w.InstanceID),
			log.String("source", "vm"),
		)
		if w.IsStderr {
			rec.SetSeverity(log.SeverityError)
			rec.AddAttributes(log.String("stream", "stderr"))
		} else {
			rec.SetSeverity(log.SeverityInfo)
			rec.AddAttributes(log.String("stream", "stdout"))
		}
		logger.Emit(context.Background(), rec)
	}
}

func (w *VMLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.buffer) > 0 {
		line := string(w.buffer)
		w.buffer = w.buffer[:0]
		if strings.TrimSpace(line) != "" {
			w.emitLog(line)
		}
	}
}

func CombinedWriter(functionID, instanceID string, isStderr bool) io.Writer {
	return NewVMLogWriter(functionID, instanceID, isStderr)
}