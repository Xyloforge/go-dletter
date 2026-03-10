// Package dletter implements a persistent dead-letter queue for Go applications.
//
// When a service fails to process an item (e.g. database timeout, external API
// unavailable), the item would normally be lost. dletter solves this by writing
// failed items to disk immediately and replaying them later with exponential
// backoff retries.
//
// # Overview
//
// A [Logger] manages two JSONL log files:
//   - A retry log (<filename>.log) for items still within the retry budget.
//   - A permanent-failure log (permanent-<filename>.log) for items that have
//     exhausted all attempts.
//
// Both files are rotated automatically by lumberjack.
//
// # Quick Start
//
//	type Order struct{ ID string }
//
//	func (o Order) AppendLog(buf []byte) []byte {
//	    return append(buf, `{"id":"`+o.ID+`"}`...)
//	}
//
//	// Create a logger
//	dlq, err := dletter.New("logs/orders.log",
//	    dletter.WithMaxSize(50),        // rotate at 50 MB
//	    dletter.WithMaxBackups(10),
//	    dletter.WithCompress(true),
//	)
//	if err != nil { ... }
//	defer dlq.Close()
//
//	// Record a failure
//	dletter.Log(dlq, Order{ID: "ord-42"}, processingErr, attemptNumber)
//
//	// Replay later (e.g. in a background goroutine)
//	err = dlq.Replay(ctx, func(payload []byte) error {
//	    return processOrder(payload)
//	}, dletter.ReplayOptions{MaxAttempts: 5, InitialWait: time.Second})
//
// # Implementing Loggable
//
// Types passed to [Log] must implement [Loggable], which serialises the value
// into the provided byte slice without heap allocations:
//
//	func (o Order) AppendLog(buf []byte) []byte {
//	    buf = append(buf, `{"id":"`...)
//	    buf = append(buf, o.ID...)
//	    buf = append(buf, '"', '}')
//	    return buf
//	}
//
// # Replay Behaviour
//
// [Logger.Replay] rotates the active log, discovers backup files sorted by
// modification time, and processes each line with exponential backoff
// (capped at 30 s + ≤10 % jitter). A state file is written alongside each
// backup so that a crash mid-replay resumes from the correct line rather than
// reprocessing everything from the start.
//
// Items that have reached [ReplayOptions.MaxAttempts] are moved to the
// permanent-failure log instead of being retried.
package dletter
