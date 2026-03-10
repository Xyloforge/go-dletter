package dletter

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestCalcWait(t *testing.T) {
	// Zero initial always produces zero wait regardless of attempt.
	for attempt := 0; attempt < 10; attempt++ {
		if w := calcWait(0, attempt); w != 0 {
			t.Errorf("attempt %d: expected 0 with zero initial, got %v", attempt, w)
		}
	}

	// Wait must never exceed 30s cap + 10% jitter (33s total).
	const hardCap = 33 * time.Second
	for attempt := 0; attempt < 40; attempt++ {
		if w := calcWait(time.Hour, attempt); w > hardCap {
			t.Errorf("attempt %d: wait %v exceeds hard cap %v", attempt, w, hardCap)
		}
	}

	// With tiny initial (1ns), jitter base rounds to 0 → deterministic doubling.
	prev := time.Duration(0)
	for attempt := 0; attempt < 6; attempt++ {
		w := calcWait(time.Nanosecond, attempt)
		if w < prev {
			t.Errorf("wait decreased at attempt %d: %v < %v", attempt, w, prev)
		}
		prev = w
	}
}

func TestReplay_StateFileResume(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test_resume.log")
	// Compress=false prevents async lumberjack rename racing with our state file.
	l, err := New(tmpFile, WithCompress(false), WithMaxAge(0), WithMaxBackups(0))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const total = 5
	for i := 0; i < total; i++ {
		Log(l, ReservationDeadletter{Type: fmt.Sprintf("id_%d", i), Qty: i}, errors.New("err"), 0)
	}

	// Rotate now so we know the backup file path before calling Replay.
	// Sleep >1ms so that Replay's internal Rotate() gets a different lumberjack
	// timestamp — otherwise both backups collide on the same millisecond filename
	// and os.Rename silently overwrites the first backup with the empty second one.
	if err := l.Rotate(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	backups, err := l.backupFiles()
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected 1 backup, got %d: %v", len(backups), err)
	}
	backupPath := backups[0]

	// Simulate a crash after 3 items by writing the state file directly.
	// saveStateFile is package-internal and uses the same format the real code uses.
	stateF, err := os.OpenFile(backupPath+"-state", os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		t.Fatal(err)
	}
	saveStateFile(stateF, 3)
	stateF.Close()

	// Single Replay: must skip lines 0-2 and process only lines 3 and 4.
	var count int32
	if err := l.Replay(context.Background(), func(payload []byte) error {
		atomic.AddInt32(&count, 1)
		return nil
	}, ReplayOptions{}); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&count); got != total-3 {
		t.Errorf("expected %d items after resume from line 3, got %d", total-3, got)
	}
}

func TestReplay_GzipBackup(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test_gz.log")
	// Disable cleanup so lumberjack doesn't remove the manually created .gz backup.
	l, err := New(tmpFile, WithMaxAge(0), WithMaxBackups(0))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Write a valid .gz backup manually — must match the filename pattern.
	gzPath := filepath.Join(tmpDir, "test_gz-2020-01-01T00-00-00.000.log.gz")
	func() {
		f, err := os.Create(gzPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		gz := gzip.NewWriter(f)
		defer gz.Close()
		for i := 0; i < 3; i++ {
			line := fmt.Sprintf(`{"ts":1,"attempt":0,"reason":"test","payload":{"type":"gz_%d","qty":1}}`+"\n", i)
			gz.Write([]byte(line)) //nolint:errcheck
		}
	}()

	var count int32
	handler := func(payload []byte) error {
		atomic.AddInt32(&count, 1)
		return nil
	}
	if err := l.Replay(context.Background(), handler, ReplayOptions{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Errorf("expected 3 items from gz backup, got %d", got)
	}
}

func TestReplay_BackupMtimeOrder(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test_order.log")
	// Disable age/count cleanup so lumberjack doesn't remove the manually created backups.
	l, err := New(tmpFile, WithCompress(false), WithMaxAge(0), WithMaxBackups(0))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	writeBackup := func(path, typ string) {
		t.Helper()
		line := fmt.Sprintf(`{"ts":1,"attempt":0,"reason":"test","payload":{"type":%q,"qty":1}}`+"\n", typ)
		if err := os.WriteFile(path, []byte(line), 0644); err != nil {
			t.Fatal(err)
		}
	}

	fileA := filepath.Join(tmpDir, "test_order-2020-01-01T00-00-01.000.log")
	fileB := filepath.Join(tmpDir, "test_order-2020-01-01T00-00-02.000.log")
	writeBackup(fileA, "first")
	writeBackup(fileB, "second")

	now := time.Now()
	os.Chtimes(fileA, now.Add(-2*time.Hour), now.Add(-2*time.Hour)) //nolint:errcheck
	os.Chtimes(fileB, now.Add(-1*time.Hour), now.Add(-1*time.Hour)) //nolint:errcheck

	var order []string
	handler := func(payload []byte) error {
		var v struct {
			Type string `json:"type"`
		}
		json.Unmarshal(payload, &v) //nolint:errcheck
		order = append(order, v.Type)
		return nil
	}

	if err := l.Replay(context.Background(), handler, ReplayOptions{}); err != nil {
		t.Fatal(err)
	}

	if len(order) != 2 {
		t.Fatalf("expected 2 items, got %d: %v", len(order), order)
	}
	if order[0] != "first" || order[1] != "second" {
		t.Errorf("wrong replay order: got %v, want [first second]", order)
	}
}
