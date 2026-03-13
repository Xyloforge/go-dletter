# go-dletter

A persistent dead-letter queue (DLQ) for Go services. When an operation fails, `dletter` writes the payload to disk so it can be replayed later — with automatic exponential-backoff retries and crash-safe resume.

```
go get github.com/Xyloforge/go-dletter
```

Requires **Go 1.22+** (uses generics and `slices`).

---

## Why

In any distributed system, operations fail transiently: database timeouts, downstream APIs being unavailable, network blips. Without a DLQ, those payloads are lost silently. With `dletter` you get:

- **Zero-loss writes** — failed items are persisted to disk before the error is returned to the caller.
- **Automatic retries** — a background worker replays items with exponential backoff (capped at 30 s).
- **Permanent-failure tracking** — items that exhaust their retry budget move to a separate log for investigation.
- **Crash-safe replay** — a state file tracks the last successfully processed line, so a restart doesn't reprocess everything from the beginning.

---

## Quick Start

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "time"

    "github.com/Xyloforge/go-dletter/dletter"
)

type Order struct {
    ID  string `json:"id"`
    Qty int    `json:"qty"`
}

func main() {
    // 1. Create a logger.
    dlq, err := dletter.New("logs/orders.log",
        dletter.WithMaxSize(50),     // rotate at 50 MB
        dletter.WithMaxBackups(10),  // keep 10 rotated files
        dletter.WithMaxAge(30),      // delete backups older than 30 days
        dletter.WithCompress(true),  // gzip rotated files
    )
    if err != nil {
        log.Fatal(err)
    }
    defer dlq.Close()

    // 2. Record a failure — any struct with json tags works.
    order := Order{ID: "ord-42", Qty: 5}
    if err := dletter.Log(dlq, order, errors.New("db timeout"), 1); err != nil {
        log.Printf("CRITICAL: DLQ write failed: %v", err)
    }

    // 3. Replay in the background.
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
    defer cancel()

    err = dlq.Replay(ctx, func(payload []byte) error {
        var o Order
        if err := json.Unmarshal(payload, &o); err != nil {
            return fmt.Errorf("corrupted payload: %w", err)
        }
        return saveOrder(o)
    }, dletter.ReplayOptions{
        MaxAttempts: 5,
        InitialWait: time.Second,
    })
    if err != nil {
        log.Printf("replay stopped: %v", err)
    }
}
```

A runnable version is in [cmd/example/main.go](cmd/example/main.go).

---

## API

### `dletter.New`

```go
func New(filename string, opts ...Option) (*Logger, error)
```

Creates a `Logger` that writes to `<filename>` (retry log) and `permanent-<filename>` (permanent-failure log) in the same directory. The directory is created if it does not exist.

**Options**

| Option | Default | Description |
|---|---|---|
| `WithMaxSize(mb int)` | 100 MB | Rotate when the active log reaches this size |
| `WithMaxBackups(n int)` | 5 | Maximum number of rotated backup files to keep |
| `WithMaxAge(days int)` | 14 | Delete backups older than this many days |
| `WithCompress(bool)` | true | Gzip-compress rotated backup files |

---

### `dletter.Log`

```go
func Log[T any](l *Logger, data T, reason error, attempt int) error
```

Writes a failed item to the retry log. Thread-safe.

- `data` — any value with json tags. If it implements `Loggable`, the zero-allocation path is used instead of `json.Marshal`.
- `reason` — the error that caused the failure.
- `attempt` — current attempt count (start at 1).

Each line written to disk is a JSON envelope:

```json
{"ts":1700000000,"attempt":1,"reason":"database timeout","payload":{...}}
```

---

### `dletter.Loggable` (optional)

```go
type Loggable interface {
    AppendLog(buf []byte) []byte
}
```

Optional interface for zero-allocation serialization. If your payload type implements `Loggable`, `Log` will use `AppendLog` instead of `json.Marshal`. See [Performance: Zero-Allocation Serialization](#performance-zero-allocation-serialization) below.

---

### `(*Logger).Replay`

```go
func (l *Logger) Replay(ctx context.Context, handler Handler, opts ReplayOptions) error
```

Rotates the active log, then replays every backup file (oldest first). For each item:

1. Waits with exponential backoff: `initialWait × 2^attempt` (max 30 s + ≤10 % jitter).
2. Calls `handler(payload)`.
3. If `handler` returns an error, the item is re-logged with an incremented attempt count.
4. If `attempt >= MaxAttempts`, the item is moved to the permanent-failure log instead.

Progress is checkpointed after each line so that a crash mid-replay resumes from where it left off.

**`ReplayOptions`**

| Field | Description |
|---|---|
| `MaxAttempts int` | Move to permanent-failure log after this many attempts (0 = unlimited) |
| `InitialWait time.Duration` | Base duration for the first retry wait |

**`Handler`**

```go
type Handler func(payload []byte) error
```

Return `nil` on success, an error if the item still can't be processed (it will be re-queued).

---

### `dletter.LogPermanent`

```go
func LogPermanent[T any](l *Logger, data T, reason string) error
```

Writes a failed item to the permanent-failure log. Thread-safe.

- `data` — any value with json tags. If it implements `Loggable`, the zero-allocation path is used instead of `json.Marshal`.
- `reason` — a human-readable cause of the permanent failure.

---

### `(*Logger).Close`

```go
func (l *Logger) Close() error
```

Flushes and closes both log file handles. Always call this (e.g. via `defer`) to avoid data loss.

---

### `(*Logger).Rotate`

```go
func (l *Logger) Rotate() error
```

Manually triggers rotation of the active retry log. Called automatically by `Replay`.

---

## Log File Layout

```
logs/
├── orders.log                        # active retry log (written by Log)
├── orders-2024-01-15T10-30-00.log.gz # rotated backup (replayed by Replay)
├── orders-2024-01-15T10-30-00.log.gz-state  # replay progress file (auto-deleted on success)
└── permanent-orders.log              # permanent-failure log (written by LogPermanent / Replay)
```

---

## Pattern: Retry Worker

Run `Replay` in a background goroutine that wakes up on a schedule:

```go
go func() {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    for range ticker.C {
        ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
        if err := dlq.Replay(ctx, handler, opts); err != nil {
            log.Printf("replay: %v", err)
        }
        cancel()
    }
}()
```

---

## Performance: Zero-Allocation Serialization

By default, `Log` serializes your payload with `json.Marshal`. For most services this is perfectly fine.

If you're logging at very high throughput and `json.Marshal` allocation pressure shows up in profiles, implement the `Loggable` interface on your type. `Log` detects it at runtime and switches to the zero-allocation path automatically:

```go
func (o Order) AppendLog(buf []byte) []byte {
    buf = append(buf, `{"id":"`...)
    buf = append(buf, o.ID...)
    buf = append(buf, `","qty":`...)
    buf = strconv.AppendInt(buf, int64(o.Qty), 10)
    buf = append(buf, '}')
    return buf
}

// Same call — Log detects Loggable and uses AppendLog automatically.
dletter.Log(dlq, order, err, 1)
```

---

## Running Tests

```bash
make test     # run all tests
make bench    # run benchmarks with allocation stats
make example  # run the example application
```

---

## Alternatives

| Approach | Trade-off |
|---|---|
| Manual retry loops | No persistence — if the process crashes, in-flight items are lost |
| Message queues (Kafka, RabbitMQ, SQS) | Reliable, but require external infrastructure and operational overhead |
| `go-retryablehttp` | HTTP-only; no disk persistence, no dead-letter tracking |
| **dletter** | Single-binary, zero-infra, disk-backed DLQ with automatic retries and crash-safe resume |

`dletter` is designed for services that need retry + persistence without adding a message broker to the stack.

---

## License

MIT
