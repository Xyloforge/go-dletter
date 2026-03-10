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

// 1. Define your payload type and implement dletter.Loggable.
//    AppendLog must serialise the value into buf without heap allocations.
type Order struct {
    ID  string `json:"id"`
    Qty int    `json:"qty"`
}

func (o Order) AppendLog(buf []byte) []byte {
    buf = append(buf, `{"id":"`...)
    buf = append(buf, o.ID...)
    buf = append(buf, `","qty":`...)
    buf = strconv.AppendInt(buf, int64(o.Qty), 10)
    buf = append(buf, '}')
    return buf
}

func main() {
    // 2. Create a logger. Both the retry log and the permanent-failure log
    //    are created automatically under the same directory.
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

    // 3. Record a failure (e.g. inside your business logic when an op fails).
    if err := saveOrder(order); err != nil {
        if logErr := dletter.Log(dlq, order, err, attemptNumber); logErr != nil {
            log.Printf("CRITICAL: DLQ write failed: %v", logErr)
        }
    }

    // 4. Replay in the background (e.g. in a goroutine at startup).
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
func Log[T Loggable](l *Logger, data T, reason error, attempt int) error
```

Writes a failed item to the retry log. Thread-safe.

- `data` — the payload; must implement `Loggable`.
- `reason` — the error that caused the failure.
- `attempt` — current attempt count (start at 1).

Each line written to disk is a JSON envelope:

```json
{"ts":1700000000,"attempt":1,"reason":"database timeout","payload":{...}}
```

---

### `dletter.Loggable`

```go
type Loggable interface {
    AppendLog(buf []byte) []byte
}
```

Implement this on your payload type. Append the JSON representation of the value to `buf` and return the result. Avoid allocating — reuse `buf` directly.

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

### `(*Logger).LogPermanent`

```go
func (l *Logger) LogPermanent(data []byte, reason string) error
```

Writes a raw JSON payload directly to the permanent-failure log. Useful when you want to route items to permanent failure outside of the automatic retry flow.

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

## Running Tests

```bash
make test     # run all tests
make bench    # run benchmarks with allocation stats
make example  # run the example application
```

---

## License

MIT
