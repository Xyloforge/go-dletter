package llog

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

func BenchmarkError(b *testing.B) {
	err := errors.New("test error")
	for i := 0; i < b.N; i++ {
		Error(context.Background(), err, "message", slog.Int("ABC: ", i+1))

	}

}

func BenchmarkLogInfo(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Benchmark the hot path, not the "Emergency" path
		Info(ctx, "test message", slog.Int("count", i))
	}
}
