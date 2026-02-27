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
