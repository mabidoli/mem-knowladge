# Improvement Area 5: Observability Stack

## Problem Statement

The original architecture has zero mentions of:
1. Metrics collection and dashboards
2. Distributed tracing
3. Structured logging
4. Alerting rules
5. Health checks beyond basic connectivity

This is a **MEDIUM-HIGH SEVERITY** gap that makes debugging and monitoring impossible in production.

---

## Observability Pillars

| Pillar | Purpose | Technology |
|--------|---------|------------|
| **Metrics** | Quantitative system health | Prometheus + Grafana |
| **Logs** | Event details and debugging | Structured JSON logs |
| **Traces** | Request flow across components | OpenTelemetry |
| **Alerts** | Proactive incident detection | Prometheus Alertmanager |

---

## 1. Metrics Implementation

### Prometheus Metrics Definition

```go
package metrics

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    // Counters
    MemoriesCreated = promauto.NewCounter(prometheus.CounterOpts{
        Namespace: "ums",
        Name:      "memories_created_total",
        Help:      "Total number of memories created",
    })

    MemoriesRecalled = promauto.NewCounter(prometheus.CounterOpts{
        Namespace: "ums",
        Name:      "memories_recalled_total",
        Help:      "Total number of recall operations",
    })

    MemoriesForgotten = promauto.NewCounter(prometheus.CounterOpts{
        Namespace: "ums",
        Name:      "memories_forgotten_total",
        Help:      "Total number of forget operations",
    })

    ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Namespace: "ums",
        Name:      "errors_total",
        Help:      "Total errors by type",
    }, []string{"operation", "error_type"})

    // Histograms
    SaveLatency = promauto.NewHistogram(prometheus.HistogramOpts{
        Namespace: "ums",
        Name:      "save_latency_seconds",
        Help:      "Time to save a memory (including embedding)",
        Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
    })

    RecallLatency = promauto.NewHistogram(prometheus.HistogramOpts{
        Namespace: "ums",
        Name:      "recall_latency_seconds",
        Help:      "Time to recall memories",
        Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0},
    })

    EmbeddingLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Namespace: "ums",
        Name:      "embedding_latency_seconds",
        Help:      "Time to generate embeddings",
        Buckets:   []float64{0.05, 0.1, 0.2, 0.5, 1.0, 2.0},
    }, []string{"provider"}) // "openai", "ollama", "cache_hit"

    Neo4jQueryLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Namespace: "ums",
        Name:      "neo4j_query_latency_seconds",
        Help:      "Neo4j query execution time",
        Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5},
    }, []string{"query_type"}) // "create", "vector_search", "update"

    // Gauges
    BufferUtilization = promauto.NewGauge(prometheus.GaugeOpts{
        Namespace: "ums",
        Name:      "buffer_utilization_ratio",
        Help:      "Write buffer utilization (0-1)",
    })

    ActiveWorkers = promauto.NewGauge(prometheus.GaugeOpts{
        Namespace: "ums",
        Name:      "active_workers",
        Help:      "Number of active worker goroutines",
    })

    MemoryCount = promauto.NewGauge(prometheus.GaugeOpts{
        Namespace: "ums",
        Name:      "memory_count",
        Help:      "Total memories in database",
    })

    // Summary for percentiles
    RecallResultCount = promauto.NewSummary(prometheus.SummaryOpts{
        Namespace:  "ums",
        Name:       "recall_result_count",
        Help:       "Number of memories returned per recall",
        Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
    })
)
```

### Metrics Collection Points

```go
func (ms *MemorySystem) SaveMemory(ctx context.Context, content string) error {
    timer := prometheus.NewTimer(metrics.SaveLatency)
    defer timer.ObserveDuration()

    // Update buffer gauge
    metrics.BufferUtilization.Set(float64(len(ms.queue)) / float64(cap(ms.queue)))

    // ... save logic ...

    if err != nil {
        metrics.ErrorsTotal.WithLabelValues("save", categorizeError(err)).Inc()
        return err
    }

    metrics.MemoriesCreated.Inc()
    return nil
}

func (ms *MemorySystem) RecallMemories(ctx context.Context, query string) ([]Memory, error) {
    timer := prometheus.NewTimer(metrics.RecallLatency)
    defer timer.ObserveDuration()

    // ... recall logic ...

    metrics.MemoriesRecalled.Inc()
    metrics.RecallResultCount.Observe(float64(len(results)))

    return results, nil
}
```

### Metrics Endpoint

```go
import (
    "net/http"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

func StartMetricsServer(addr string) {
    mux := http.NewServeMux()
    mux.Handle("/metrics", promhttp.Handler())
    mux.HandleFunc("/health", healthHandler)

    go http.ListenAndServe(addr, mux)
}
```

---

## 2. Structured Logging

### Logger Setup

```go
package logging

import (
    "os"
    "go.uber.org/zap"
    "go.uber.org/zap/zapcore"
)

var Logger *zap.Logger

func InitLogger(level string, jsonOutput bool) error {
    var config zap.Config

    if jsonOutput {
        config = zap.NewProductionConfig()
    } else {
        config = zap.NewDevelopmentConfig()
    }

    // Parse level
    var zapLevel zapcore.Level
    if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
        zapLevel = zapcore.InfoLevel
    }
    config.Level = zap.NewAtomicLevelAt(zapLevel)

    // Add standard fields
    config.InitialFields = map[string]interface{}{
        "service": "universal-memory-system",
        "version": Version,
    }

    var err error
    Logger, err = config.Build()
    return err
}

// Convenience wrappers
func Info(msg string, fields ...zap.Field) {
    Logger.Info(msg, fields...)
}

func Error(msg string, err error, fields ...zap.Field) {
    fields = append(fields, zap.Error(err))
    Logger.Error(msg, fields...)
}

func WithContext(ctx context.Context) *zap.Logger {
    // Extract trace ID if present
    if traceID := trace.SpanContextFromContext(ctx).TraceID(); traceID.IsValid() {
        return Logger.With(zap.String("trace_id", traceID.String()))
    }
    return Logger
}
```

### Log Patterns

```go
// Good: Structured, searchable
logging.Info("memory created",
    zap.String("memory_id", id),
    zap.Int("content_length", len(content)),
    zap.Strings("tags", tags),
    zap.Duration("latency", elapsed),
)

// Good: Error with context
logging.Error("neo4j write failed", err,
    zap.String("operation", "create_memory"),
    zap.Int("retry_attempt", attempt),
    zap.String("memory_id", id),
)

// Bad: Unstructured
log.Printf("Created memory %s with %d chars", id, len(content))
```

### Log Output Example

```json
{
  "level": "info",
  "ts": 1704326400.123,
  "caller": "memory/service.go:142",
  "msg": "memory created",
  "service": "universal-memory-system",
  "version": "1.0.0",
  "trace_id": "abc123def456",
  "memory_id": "mem_789",
  "content_length": 256,
  "tags": ["project", "meeting"],
  "latency": 0.145
}
```

---

## 3. Distributed Tracing (OpenTelemetry)

### Tracer Setup

```go
package tracing

import (
    "context"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/sdk/resource"
    "go.opentelemetry.io/otel/sdk/trace"
    semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

func InitTracer(serviceName, endpoint string) (func(), error) {
    ctx := context.Background()

    // Create OTLP exporter
    exporter, err := otlptrace.New(ctx,
        otlptracegrpc.NewClient(
            otlptracegrpc.WithEndpoint(endpoint),
            otlptracegrpc.WithInsecure(),
        ),
    )
    if err != nil {
        return nil, err
    }

    // Create resource
    res, err := resource.New(ctx,
        resource.WithAttributes(
            semconv.ServiceName(serviceName),
            semconv.ServiceVersion(Version),
        ),
    )
    if err != nil {
        return nil, err
    }

    // Create tracer provider
    tp := trace.NewTracerProvider(
        trace.WithBatcher(exporter),
        trace.WithResource(res),
    )

    otel.SetTracerProvider(tp)

    // Return cleanup function
    return func() {
        tp.Shutdown(context.Background())
    }, nil
}

var Tracer = otel.Tracer("universal-memory-system")
```

### Traced Operations

```go
import (
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/trace"
)

func (ms *MemorySystem) RecallMemories(ctx context.Context, query string) ([]Memory, error) {
    ctx, span := tracing.Tracer.Start(ctx, "RecallMemories",
        trace.WithAttributes(
            attribute.Int("query_length", len(query)),
        ),
    )
    defer span.End()

    // Trace embedding generation
    embedding, err := ms.embedWithTrace(ctx, query)
    if err != nil {
        span.RecordError(err)
        return nil, err
    }

    // Trace database query
    results, err := ms.vectorSearchWithTrace(ctx, embedding)
    if err != nil {
        span.RecordError(err)
        return nil, err
    }

    span.SetAttributes(attribute.Int("result_count", len(results)))
    return results, nil
}

func (ms *MemorySystem) embedWithTrace(ctx context.Context, text string) ([]float32, error) {
    ctx, span := tracing.Tracer.Start(ctx, "GenerateEmbedding",
        trace.WithAttributes(
            attribute.String("provider", ms.embeddingProvider.Name()),
        ),
    )
    defer span.End()

    vec, err := ms.embeddingProvider.Embed(ctx, text)
    if err != nil {
        span.RecordError(err)
    }
    return vec, err
}

func (ms *MemorySystem) vectorSearchWithTrace(ctx context.Context, vec []float32) ([]Memory, error) {
    ctx, span := tracing.Tracer.Start(ctx, "Neo4jVectorSearch")
    defer span.End()

    // ... query execution ...

    span.SetAttributes(
        attribute.Int("candidates_scanned", 50),
        attribute.Float64("top_score", results[0].Score),
    )
    return results, nil
}
```

### Trace Visualization

```
RecallMemories (145ms)
├── GenerateEmbedding (98ms)
│   └── provider: openai
└── Neo4jVectorSearch (42ms)
    ├── candidates_scanned: 50
    └── top_score: 0.89
```

---

## 4. Alerting Rules

### Prometheus Alert Rules

```yaml
# alerts.yml
groups:
  - name: ums_alerts
    rules:
      # High latency
      - alert: UMSHighRecallLatency
        expr: histogram_quantile(0.95, rate(ums_recall_latency_seconds_bucket[5m])) > 0.5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Memory recall p95 latency > 500ms"
          description: "p95 recall latency is {{ $value }}s"

      # Buffer nearly full
      - alert: UMSBufferNearlyFull
        expr: ums_buffer_utilization_ratio > 0.8
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Write buffer > 80% full"
          description: "Buffer utilization is {{ $value | humanizePercentage }}"

      # Buffer completely full
      - alert: UMSBufferFull
        expr: ums_buffer_utilization_ratio >= 1.0
        for: 30s
        labels:
          severity: critical
        annotations:
          summary: "Write buffer is completely full"
          description: "Memories are being dropped!"

      # High error rate
      - alert: UMSHighErrorRate
        expr: rate(ums_errors_total[5m]) > 1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Error rate > 1/second"
          description: "Operation {{ $labels.operation }} failing with {{ $labels.error_type }}"

      # Neo4j connection issues
      - alert: UMSNeo4jConnectionError
        expr: increase(ums_errors_total{error_type="connection"}[5m]) > 5
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Neo4j connection errors detected"

      # Embedding API issues
      - alert: UMSEmbeddingAPIErrors
        expr: rate(ums_errors_total{operation="embedding"}[5m]) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Embedding API experiencing errors"

      # No memories created
      - alert: UMSNoMemoriesCreated
        expr: increase(ums_memories_created_total[1h]) == 0
        for: 1h
        labels:
          severity: info
        annotations:
          summary: "No memories created in last hour"
```

---

## 5. Grafana Dashboard

### Dashboard JSON (Key Panels)

```json
{
  "title": "Universal Memory System",
  "panels": [
    {
      "title": "Operations / Second",
      "type": "graph",
      "targets": [
        {"expr": "rate(ums_memories_created_total[1m])", "legendFormat": "Create"},
        {"expr": "rate(ums_memories_recalled_total[1m])", "legendFormat": "Recall"},
        {"expr": "rate(ums_memories_forgotten_total[1m])", "legendFormat": "Forget"}
      ]
    },
    {
      "title": "Latency (p50, p95, p99)",
      "type": "graph",
      "targets": [
        {"expr": "histogram_quantile(0.50, rate(ums_recall_latency_seconds_bucket[5m]))", "legendFormat": "p50"},
        {"expr": "histogram_quantile(0.95, rate(ums_recall_latency_seconds_bucket[5m]))", "legendFormat": "p95"},
        {"expr": "histogram_quantile(0.99, rate(ums_recall_latency_seconds_bucket[5m]))", "legendFormat": "p99"}
      ]
    },
    {
      "title": "Buffer Utilization",
      "type": "gauge",
      "targets": [
        {"expr": "ums_buffer_utilization_ratio"}
      ],
      "thresholds": [
        {"value": 0.5, "color": "green"},
        {"value": 0.8, "color": "yellow"},
        {"value": 0.95, "color": "red"}
      ]
    },
    {
      "title": "Error Rate by Type",
      "type": "graph",
      "targets": [
        {"expr": "rate(ums_errors_total[5m])", "legendFormat": "{{operation}} - {{error_type}}"}
      ]
    },
    {
      "title": "Embedding Latency by Provider",
      "type": "heatmap",
      "targets": [
        {"expr": "rate(ums_embedding_latency_seconds_bucket[5m])"}
      ]
    },
    {
      "title": "Total Memories",
      "type": "stat",
      "targets": [
        {"expr": "ums_memory_count"}
      ]
    }
  ]
}
```

---

## 6. Health Check Endpoint

```go
type HealthStatus struct {
    Status      string            `json:"status"` // "healthy", "degraded", "unhealthy"
    Checks      map[string]Check  `json:"checks"`
    Version     string            `json:"version"`
    Uptime      string            `json:"uptime"`
}

type Check struct {
    Status  string `json:"status"`
    Message string `json:"message,omitempty"`
    Latency string `json:"latency,omitempty"`
}

func (ms *MemorySystem) HealthCheck() HealthStatus {
    status := HealthStatus{
        Status:  "healthy",
        Version: Version,
        Uptime:  time.Since(startTime).String(),
        Checks:  make(map[string]Check),
    }

    // Check Neo4j
    start := time.Now()
    if err := ms.driver.VerifyConnectivity(context.Background()); err != nil {
        status.Checks["neo4j"] = Check{
            Status:  "unhealthy",
            Message: err.Error(),
        }
        status.Status = "unhealthy"
    } else {
        status.Checks["neo4j"] = Check{
            Status:  "healthy",
            Latency: time.Since(start).String(),
        }
    }

    // Check buffer
    bufferRatio := float64(len(ms.queue)) / float64(cap(ms.queue))
    if bufferRatio > 0.95 {
        status.Checks["buffer"] = Check{
            Status:  "unhealthy",
            Message: fmt.Sprintf("Buffer %.0f%% full", bufferRatio*100),
        }
        status.Status = "unhealthy"
    } else if bufferRatio > 0.8 {
        status.Checks["buffer"] = Check{
            Status:  "degraded",
            Message: fmt.Sprintf("Buffer %.0f%% full", bufferRatio*100),
        }
        if status.Status == "healthy" {
            status.Status = "degraded"
        }
    } else {
        status.Checks["buffer"] = Check{
            Status: "healthy",
        }
    }

    // Check embedding service
    start = time.Now()
    _, err := ms.embedder.Embed(context.Background(), "health check")
    if err != nil {
        status.Checks["embedding"] = Check{
            Status:  "degraded",
            Message: "Using fallback provider",
        }
        if status.Status == "healthy" {
            status.Status = "degraded"
        }
    } else {
        status.Checks["embedding"] = Check{
            Status:  "healthy",
            Latency: time.Since(start).String(),
        }
    }

    return status
}
```

---

## Deployment Configuration

### Docker Compose Addition

```yaml
services:
  # ... existing neo4j service ...

  prometheus:
    image: prom/prometheus:latest
    ports:
      - "9090:9090"
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
      - ./alerts.yml:/etc/prometheus/alerts.yml
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'

  grafana:
    image: grafana/grafana:latest
    ports:
      - "3000:3000"
    volumes:
      - ./grafana/dashboards:/var/lib/grafana/dashboards
    environment:
      - GF_SECURITY_ADMIN_PASSWORD=admin

  jaeger:
    image: jaegertracing/all-in-one:latest
    ports:
      - "16686:16686"  # UI
      - "4317:4317"    # OTLP gRPC
```

### prometheus.yml

```yaml
global:
  scrape_interval: 15s

alerting:
  alertmanagers:
    - static_configs:
        - targets: ['alertmanager:9093']

rule_files:
  - "alerts.yml"

scrape_configs:
  - job_name: 'ums'
    static_configs:
      - targets: ['host.docker.internal:8080']
```

---

## Action Items

- [ ] Add prometheus/client_golang dependency
- [ ] Define all metrics in `metrics` package
- [ ] Instrument save, recall, forget operations
- [ ] Setup zap structured logging
- [ ] Add OpenTelemetry tracing
- [ ] Create Prometheus alert rules
- [ ] Build Grafana dashboard JSON
- [ ] Implement health check endpoint
- [ ] Add docker-compose services for monitoring stack
- [ ] Document runbook for each alert
- [ ] Load test and verify metrics accuracy

---

## References

- [Prometheus Go Client](https://github.com/prometheus/client_golang)
- [Zap Logger](https://github.com/uber-go/zap)
- [OpenTelemetry Go](https://opentelemetry.io/docs/instrumentation/go/)
- [Grafana Dashboard Best Practices](https://grafana.com/docs/grafana/latest/dashboards/build-dashboards/best-practices/)
