package dletter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkReplay(b *testing.B) {
	tmpFile := filepath.Join(b.TempDir(), "test_replay_bench.log")
	l, err := New(tmpFile)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	payload := ReservationDeadletter{
		Type: "reservation_fail",
		Qty:  10,
	}
	benchErr := errors.New("timeout error occurred")

	for i := 0; i < 1000; i++ {
		_ = Log(l, payload, benchErr, 0)
	}

	opts := ReplayOptions{
		MaxAttempts: 1,
		InitialWait: 0,
	}
	ctx := context.Background()
	handler := func(payload []byte) error { return nil }

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = l.Replay(ctx, handler, opts)
	}
}

func TestReplay(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test_replay.log")
	permFile := filepath.Join(tmpDir, "permanent-"+"test_replay.log")
	l, err := New(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	Log(l, ReservationDeadletter{Type: "id_1", Qty: 10}, errors.New("err1"), 0)
	Log(l, ReservationDeadletter{Type: "id_fail", Qty: 10}, errors.New("err2"), 0)
	Log(l, ReservationDeadletter{Type: "id_fail_max", Qty: 10}, errors.New("err3"), 3)
	Log(l, ReservationDeadletter{Type: "id_4", Qty: 10}, errors.New("err4"), 0)
	Log(l, ReservationDeadletter{Type: "id_5", Qty: 10}, errors.New("err5"), 0)

	var count int32
	handler := func(payload []byte) error {
		atomic.AddInt32(&count, 1)
		str := string(payload)
		if strings.Contains(str, "fail") {
			return errors.New("fail again")
		}
		return nil
	}

	opts := ReplayOptions{
		MaxAttempts: 3,
		InitialWait: time.Millisecond * 2,
	}

	ctx := context.Background()
	err = l.Replay(ctx, handler, opts)
	if err != nil {
		t.Fatal(err)
	}

	if atomic.LoadInt32(&count) != 4 {
		t.Errorf("expected replay handler to be called 4 times, got %d", count)
	}

	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 lines in file, got %d from content: %q", len(lines), string(content))
	}
	if !strings.Contains(lines[0], "fail") {
		t.Fatalf("expected last line  contain fail, got %q", lines[0])
	}

	permContent, err := os.ReadFile(permFile)
	if err != nil {
		t.Fatal(err)
	}
	permLines := strings.Split(strings.TrimSpace(string(permContent)), "\n")
	if len(permLines) != 1 {
		t.Fatalf("expected 1 lines in file, got %d from content: %q", len(permLines), string(permContent))
	}
	if !strings.Contains(permLines[0], "fail") {
		t.Fatalf("expected last line  contain fail, got %q", permLines[0])
	}
}

func TestReplayContextCancel(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test_replay_cancel.log")
	l, _ := New(tmpFile)
	defer l.Close()

	Log(l, ReservationDeadletter{Type: "id_1", Qty: 10}, errors.New("err1"), 0)
	Log(l, ReservationDeadletter{Type: "id_2", Qty: 10}, errors.New("err2"), 0)

	ctx, cancel := context.WithCancel(context.Background())
	var count int32
	handler := func(payload []byte) error {
		atomic.AddInt32(&count, 1)
		cancel()
		return nil
	}

	err := l.Replay(ctx, handler, ReplayOptions{InitialWait: 0})
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if atomic.LoadInt32(&count) != 1 {
		t.Errorf("expected 1 execution before cancel, got %d", count)
	}
}
