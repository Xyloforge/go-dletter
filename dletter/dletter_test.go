package dletter

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// errWriter is a no-op writer whose Close returns a configurable error.
type errWriter struct{ err error }

func (e *errWriter) Write(p []byte) (int, error) { return len(p), nil }
func (e *errWriter) Close() error                { return e.err }

// Dummy struct that implements Loggable for the test
type ReservationDeadletter struct {
	Type string
	Qty  int
}

func (r ReservationDeadletter) AppendLog(buf []byte) []byte {
	// Manual append logic (Zero Alloc)
	buf = append(buf, `{"type":"`...)
	buf = appendEscapedJSON(buf, r.Type) // Note: in real use, use properly mapped strings
	buf = append(buf, `","qty":10}`...)
	return buf
}

func BenchmarkLog(b *testing.B) {
	l, err := New(filepath.Join(b.TempDir(), "test_bench.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	payload := ReservationDeadletter{
		Type: "reservation_fail",
		Qty:  10,
	}

	benchErr := errors.New("timeout error occurred")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = Log(l, payload, benchErr, 0)
	}
}

func TestAppendEscapedJSON(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`hello "world"`, `hello \"world\"`},
		{`foo\bar`, `foo\\bar`},
		{"line1\nline2", `line1\nline2`},
		{"normal string", "normal string"},
		{"tab\there", `tab\there`},
		{"cr\rhere", `cr\rhere`},
		{"\bbackspace\f", `\bbackspace\f`},
		{"\x01\x02\x1f", `\u0001\u0002\u001f`},
		{"valid utf8 \u4e16\u754c", "valid utf8 \u4e16\u754c"},
		{"invalid utf8 \x80\xff", `invalid utf8 \ufffd\ufffd`},
	}

	for _, tt := range tests {
		result := appendEscapedJSON(nil, tt.input)
		if string(result) != tt.expected {
			t.Errorf("expected %s, got %s", tt.expected, string(result))
		}
	}
}

func TestLog(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test.log")
	l, err := New(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	payload := ReservationDeadletter{Type: "test_type"}
	err = Log(l, payload, errors.New("some \"error\" \\ \n new line"), 2)
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	var env Envelope
	if err := json.Unmarshal(content, &env); err != nil {
		t.Fatalf("Failed to unmarshal %s: %v", string(content), err)
	}

	if env.Attempt != 2 {
		t.Errorf("Expected attempt 2, got %d", env.Attempt)
	}

	if env.Reason != "some \"error\" \\ \n new line" {
		t.Errorf("Expected original error, got %q", env.Reason)
	}

	if string(env.Payload) != `{"type":"test_type","qty":10}` {
		t.Errorf("Unexpected payload: %q", string(env.Payload))
	}
}

func TestLogger_ConcurrencyAndRace(t *testing.T) {
	// Setup: Create a temporary folder just for this test so we don't clutter the system
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "concurrent_test.log")

	dlq, err := New(logPath)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer dlq.Close()

	goroutines := 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done() // Tell the WaitGroup we finished when this function exits

			payload := ReservationDeadletter{Type: "test", Qty: id}
			err := Log(dlq, payload, errors.New("concurrent test error"), 1)
			if err != nil {
				t.Errorf("Goroutine %d failed to log: %v", id, err)
			}
		}(i)
	}

	wg.Wait()

	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("Failed to open log file to verify: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
	}

	if lineCount != goroutines {
		t.Errorf("Expected %d lines in log, but found %d. We dropped data!", goroutines, lineCount)
	}
}

func TestLogPermanent_ConcurrentRace(t *testing.T) {
	tmpDir := t.TempDir()
	l, err := New(filepath.Join(tmpDir, "test_perm.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const goroutines = 500
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			payload := ReservationDeadletter{Type: "perm", Qty: id}
			if err := LogPermanent(l, payload, "max attempts exceeded"); err != nil {
				t.Errorf("goroutine %d: %v", id, err)
			}
		}(i)
	}
	wg.Wait()

	f, err := os.Open(filepath.Join(tmpDir, "permanent-test_perm.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		count++
	}
	if count != goroutines {
		t.Errorf("expected %d lines in permanent log, got %d", goroutines, count)
	}
}

func TestClose_BothErrors(t *testing.T) {
	err1 := errors.New("retry writer close error")
	err2 := errors.New("dead writer close error")
	l := newWithWriter(&errWriter{err1}, &errWriter{err2})

	err := l.Close()
	if err == nil {
		t.Fatal("expected error from Close, got nil")
	}
	if !errors.Is(err, err1) {
		t.Errorf("expected err1 (%v) in joined error: %v", err1, err)
	}
	if !errors.Is(err, err2) {
		t.Errorf("expected err2 (%v) in joined error: %v", err2, err)
	}
}

// PlainOrder does NOT implement Loggable — exercises the json.Marshal path.
type PlainOrder struct {
	ID  string `json:"id"`
	Qty int    `json:"qty"`
}

func TestLogWithPlainStruct(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test_plain.log")
	l, err := New(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	order := PlainOrder{ID: "ord-99", Qty: 3}
	if err := Log(l, order, errors.New("timeout"), 1); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	var env Envelope
	if err := json.Unmarshal(content, &env); err != nil {
		t.Fatalf("unmarshal: %v (raw: %s)", err, content)
	}

	if env.Attempt != 1 {
		t.Errorf("expected attempt 1, got %d", env.Attempt)
	}
	if env.Reason != "timeout" {
		t.Errorf("expected reason 'timeout', got %q", env.Reason)
	}
	if string(env.Payload) != `{"id":"ord-99","qty":3}` {
		t.Errorf("unexpected payload: %q", string(env.Payload))
	}
}

func TestLogWithLoggableStruct(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test_loggable.log")
	l, err := New(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	payload := ReservationDeadletter{Type: "loggable_test", Qty: 7}
	if err := Log(l, payload, errors.New("retry"), 2); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	var env Envelope
	if err := json.Unmarshal(content, &env); err != nil {
		t.Fatalf("unmarshal: %v (raw: %s)", err, content)
	}

	// ReservationDeadletter.AppendLog always writes qty:10 regardless of field value,
	// confirming the Loggable path was used (json.Marshal would write qty:7).
	if string(env.Payload) != `{"type":"loggable_test","qty":10}` {
		t.Errorf("expected Loggable path (qty:10), got payload: %q", string(env.Payload))
	}
}

// Unserializable contains a channel which json.Marshal cannot handle.
type Unserializable struct {
	Ch chan int
}

func TestLogWithUnserializableStruct(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test_unserializable.log")
	l, err := New(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	bad := Unserializable{Ch: make(chan int)}
	err = Log(l, bad, errors.New("fail"), 1)
	if err == nil {
		t.Fatal("expected error for unserializable struct, got nil")
	}
	if !errors.Is(err, err) {
		t.Errorf("expected marshal error, got: %v", err)
	}
}
