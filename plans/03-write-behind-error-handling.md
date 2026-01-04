# Improvement Area 3: Write-Behind Error Handling & Graceful Shutdown

## Problem Statement

The original architecture's Write-Behind implementation has critical gaps:
1. No graceful shutdown - buffered memories lost on process termination
2. No dead letter queue - failed writes silently dropped
3. No backpressure signaling - LLM sees timeout, not "system busy"
4. No retry logic for transient failures

This is a **MEDIUM-HIGH SEVERITY** gap that risks data loss.

---

## Current Implementation (Flawed)

```go
var memoryQueue = make(chan MemoryPayload, 100)

func worker(id int, driver neo4j.Driver) {
    for payload := range memoryQueue {
        session := driver.NewSession(...)
        _, err := session.ExecuteWrite(...)
        session.Close(context.TODO())
        // ERROR: failures silently ignored
        // ERROR: no retry logic
        // ERROR: no metrics
    }
}
```

### Failure Scenarios Not Handled

| Scenario | Current Behavior | Correct Behavior |
|----------|------------------|------------------|
| Process killed (SIGTERM) | Buffer lost | Drain buffer, then exit |
| Neo4j temporarily down | Write dropped | Retry with backoff |
| Embedding API error | Write dropped | Retry or DLQ |
| Buffer full | Handler blocks forever | Return error to LLM |
| Worker panic | Silent crash | Recover, log, continue |

---

## Proposed Implementation

### 1. Graceful Shutdown Handler

```go
package main

import (
    "context"
    "os"
    "os/signal"
    "sync"
    "syscall"
    "time"
)

type MemorySystem struct {
    queue       chan MemoryPayload
    wg          sync.WaitGroup
    shutdownCh  chan struct{}
    driver      neo4j.Driver
    workerCount int
}

func NewMemorySystem(driver neo4j.Driver, bufferSize, workers int) *MemorySystem {
    ms := &MemorySystem{
        queue:       make(chan MemoryPayload, bufferSize),
        shutdownCh:  make(chan struct{}),
        driver:      driver,
        workerCount: workers,
    }

    // Start workers
    for i := 0; i < workers; i++ {
        ms.wg.Add(1)
        go ms.worker(i)
    }

    // Setup signal handler
    go ms.handleSignals()

    return ms
}

func (ms *MemorySystem) handleSignals() {
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

    <-sigCh
    log.Info("Shutdown signal received, draining buffer...")

    // Signal workers to stop accepting new work
    close(ms.shutdownCh)

    // Close queue to signal workers to drain
    close(ms.queue)

    // Wait for workers with timeout
    done := make(chan struct{})
    go func() {
        ms.wg.Wait()
        close(done)
    }()

    select {
    case <-done:
        log.Info("Buffer drained successfully")
    case <-time.After(30 * time.Second):
        log.Error("Shutdown timeout, some memories may be lost")
    }

    os.Exit(0)
}
```

### 2. Worker with Retry Logic

```go
func (ms *MemorySystem) worker(id int) {
    defer ms.wg.Done()
    defer func() {
        if r := recover(); r != nil {
            log.Errorf("Worker %d panicked: %v", id, r)
            // Restart worker
            ms.wg.Add(1)
            go ms.worker(id)
        }
    }()

    for payload := range ms.queue {
        err := ms.processWithRetry(payload)
        if err != nil {
            ms.sendToDeadLetterQueue(payload, err)
        }
    }
}

func (ms *MemorySystem) processWithRetry(payload MemoryPayload) error {
    var lastErr error

    for attempt := 0; attempt < MaxRetries; attempt++ {
        err := ms.persistMemory(payload)
        if err == nil {
            metrics.MemoriesWritten.Inc()
            return nil
        }

        lastErr = err

        // Check if error is retryable
        if !isRetryable(err) {
            return err
        }

        // Exponential backoff: 100ms, 200ms, 400ms, 800ms
        backoff := time.Duration(100<<attempt) * time.Millisecond
        log.Warnf("Retry %d/%d after %v: %v", attempt+1, MaxRetries, backoff, err)

        select {
        case <-time.After(backoff):
        case <-ms.shutdownCh:
            return fmt.Errorf("shutdown during retry")
        }
    }

    return fmt.Errorf("max retries exceeded: %w", lastErr)
}

func isRetryable(err error) bool {
    // Network errors, temporary unavailable
    if neo4j.IsTransientError(err) {
        return true
    }
    // Connection refused, timeout
    var netErr net.Error
    if errors.As(err, &netErr) && netErr.Temporary() {
        return true
    }
    return false
}
```

### 3. Dead Letter Queue

```go
type DeadLetter struct {
    Payload   MemoryPayload
    Error     string
    Timestamp time.Time
    Attempts  int
}

type DeadLetterQueue struct {
    file   *os.File
    mu     sync.Mutex
    encoder *json.Encoder
}

func NewDeadLetterQueue(path string) (*DeadLetterQueue, error) {
    f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return nil, err
    }
    return &DeadLetterQueue{
        file:    f,
        encoder: json.NewEncoder(f),
    }, nil
}

func (dlq *DeadLetterQueue) Add(payload MemoryPayload, err error) {
    dlq.mu.Lock()
    defer dlq.mu.Unlock()

    dl := DeadLetter{
        Payload:   payload,
        Error:     err.Error(),
        Timestamp: time.Now(),
        Attempts:  MaxRetries,
    }

    if encErr := dlq.encoder.Encode(dl); encErr != nil {
        log.Errorf("Failed to write to DLQ: %v", encErr)
    }

    metrics.DeadLetterCount.Inc()
}

// Reprocessor for manual recovery
func (dlq *DeadLetterQueue) Reprocess(ms *MemorySystem) error {
    // Read DLQ file, attempt reprocessing
    // Move to separate "processed" file on success
    // ...
}
```

### 4. Backpressure Signaling to MCP

```go
func (ms *MemorySystem) SaveMemory(ctx context.Context, content string, importance int) (*mcp.CallToolResult, error) {
    payload := MemoryPayload{
        Content:    content,
        Importance: importance,
        Timestamp:  time.Now(),
    }

    // Check if system is shutting down
    select {
    case <-ms.shutdownCh:
        return mcp.NewToolResultError("Memory system is shutting down"), nil
    default:
    }

    // Non-blocking send with timeout
    select {
    case ms.queue <- payload:
        metrics.MemoriesQueued.Inc()
        return mcp.NewToolResultText(fmt.Sprintf(
            "Memory queued (buffer: %d/%d)",
            len(ms.queue), cap(ms.queue),
        )), nil

    case <-time.After(100 * time.Millisecond):
        metrics.BufferFullErrors.Inc()
        return mcp.NewToolResultError(
            "Memory system overloaded. Please retry in a moment.",
        ), nil

    case <-ctx.Done():
        return mcp.NewToolResultError("Request cancelled"), nil
    }
}
```

### 5. Health Check Endpoint

```go
func (ms *MemorySystem) HealthCheck() HealthStatus {
    bufferUsage := float64(len(ms.queue)) / float64(cap(ms.queue))

    status := HealthStatus{
        Healthy:     true,
        BufferUsage: bufferUsage,
        BufferSize:  len(ms.queue),
        BufferCap:   cap(ms.queue),
    }

    // Unhealthy if buffer > 80% full
    if bufferUsage > 0.8 {
        status.Healthy = false
        status.Message = "Buffer nearly full"
    }

    // Check Neo4j connectivity
    if err := ms.driver.VerifyConnectivity(context.Background()); err != nil {
        status.Healthy = false
        status.Message = "Database connection lost"
    }

    return status
}

// Expose as MCP resource
func (ms *MemorySystem) RegisterHealthResource(s *server.MCPServer) {
    s.AddResource(mcp.NewResource(
        "memory://health",
        "Memory System Health Status",
    ), func(ctx context.Context, req mcp.ReadResourceRequest) (string, error) {
        status := ms.HealthCheck()
        json, _ := json.Marshal(status)
        return string(json), nil
    })
}
```

---

## Configuration

```go
type Config struct {
    BufferSize      int           `env:"UMS_BUFFER_SIZE" default:"100"`
    WorkerCount     int           `env:"UMS_WORKER_COUNT" default:"4"`
    MaxRetries      int           `env:"UMS_MAX_RETRIES" default:"3"`
    RetryBaseDelay  time.Duration `env:"UMS_RETRY_DELAY" default:"100ms"`
    ShutdownTimeout time.Duration `env:"UMS_SHUTDOWN_TIMEOUT" default:"30s"`
    DLQPath         string        `env:"UMS_DLQ_PATH" default:"./dlq.jsonl"`
}
```

---

## Testing Strategy

### Unit Tests

```go
func TestGracefulShutdown(t *testing.T) {
    ms := NewMemorySystem(mockDriver, 10, 2)

    // Queue some memories
    for i := 0; i < 5; i++ {
        ms.queue <- MemoryPayload{Content: fmt.Sprintf("Memory %d", i)}
    }

    // Trigger shutdown
    syscall.Kill(syscall.Getpid(), syscall.SIGTERM)

    // Wait for drain
    time.Sleep(1 * time.Second)

    // Verify all processed
    assert.Equal(t, 0, len(ms.queue))
    assert.Equal(t, 5, mockDriver.WriteCount)
}

func TestRetryOnTransientError(t *testing.T) {
    failCount := 0
    mockDriver := &MockDriver{
        WriteFunc: func(p MemoryPayload) error {
            failCount++
            if failCount < 3 {
                return neo4j.NewTransientError("connection reset")
            }
            return nil
        },
    }

    ms := NewMemorySystem(mockDriver, 10, 1)
    ms.queue <- MemoryPayload{Content: "test"}

    time.Sleep(2 * time.Second)

    assert.Equal(t, 3, failCount) // Retried twice, succeeded third
}

func TestDeadLetterQueue(t *testing.T) {
    mockDriver := &MockDriver{
        WriteFunc: func(p MemoryPayload) error {
            return errors.New("permanent failure")
        },
    }

    dlqPath := t.TempDir() + "/dlq.jsonl"
    ms := NewMemorySystem(mockDriver, 10, 1)
    ms.dlq, _ = NewDeadLetterQueue(dlqPath)

    ms.queue <- MemoryPayload{Content: "will fail"}

    time.Sleep(5 * time.Second) // Allow retries to exhaust

    // Verify DLQ has entry
    data, _ := os.ReadFile(dlqPath)
    assert.Contains(t, string(data), "will fail")
}
```

### Integration Tests

```go
func TestNeo4jConnectionRecovery(t *testing.T) {
    // Start Neo4j
    container := startNeo4jContainer(t)
    defer container.Terminate()

    ms := NewMemorySystem(realDriver, 10, 2)

    // Queue memory
    ms.SaveMemory(ctx, "test memory", 5)
    time.Sleep(100 * time.Millisecond)

    // Kill Neo4j
    container.Stop()

    // Queue more (should buffer)
    ms.SaveMemory(ctx, "buffered memory", 5)

    // Restart Neo4j
    container.Start()
    time.Sleep(5 * time.Second)

    // Verify both memories persisted
    count := queryMemoryCount(realDriver)
    assert.Equal(t, 2, count)
}
```

---

## Metrics to Track

```go
var (
    MemoriesQueued = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "ums_memories_queued_total",
        Help: "Total memories added to queue",
    })
    MemoriesWritten = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "ums_memories_written_total",
        Help: "Total memories successfully written",
    })
    BufferFullErrors = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "ums_buffer_full_errors_total",
        Help: "Times queue was full when save attempted",
    })
    DeadLetterCount = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "ums_dead_letter_total",
        Help: "Memories sent to dead letter queue",
    })
    RetryAttempts = prometheus.NewHistogram(prometheus.HistogramOpts{
        Name:    "ums_retry_attempts",
        Help:    "Distribution of retry attempts before success",
        Buckets: []float64{0, 1, 2, 3},
    })
    BufferUtilization = prometheus.NewGauge(prometheus.GaugeOpts{
        Name: "ums_buffer_utilization_ratio",
        Help: "Current buffer usage (0-1)",
    })
)
```

---

## Action Items

- [ ] Implement `MemorySystem` struct with lifecycle management
- [ ] Add signal handler for SIGTERM/SIGINT
- [ ] Implement retry logic with exponential backoff
- [ ] Create Dead Letter Queue with file persistence
- [ ] Add non-blocking send with timeout in MCP handler
- [ ] Implement health check resource
- [ ] Add Prometheus metrics
- [ ] Write unit tests for shutdown, retry, DLQ
- [ ] Write integration tests for connection recovery
- [ ] Document DLQ reprocessing procedure

---

## References

- [Go Concurrency Patterns: Context](https://blog.golang.org/context)
- [Graceful Shutdown in Go](https://pkg.go.dev/os/signal)
- [Neo4j Go Driver Error Handling](https://neo4j.com/docs/go-manual/current/connect-advanced/)
