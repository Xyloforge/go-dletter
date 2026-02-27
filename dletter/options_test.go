package dletter

import (
	"path/filepath"
	"testing"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestOptions(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test_opts.log")
	l, err := New(tmpFile, WithMaxSize(10), WithMaxBackups(2), WithMaxAge(7), WithCompress(false))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	lj, ok := l.retryWriter.(*lumberjack.Logger)
	if !ok {
		t.Fatalf("expected writer to be *lumberjack.Logger")
	}
	if lj.MaxSize != 10 || lj.MaxBackups != 2 || lj.MaxAge != 7 || lj.Compress != false {
		t.Errorf("options not applied correctly: %+v", lj)
	}
}
