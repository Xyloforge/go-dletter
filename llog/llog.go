package llog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"
)

var (
	webhookURL = os.Getenv("GOOGLE_CHAT_WEBHOOK")
	// Use custom pretty handler
	logger       = slog.New(NewPrettyHandler(os.Stdout))
	stackBufPool = sync.Pool{New: func() any { b := make([]byte, 4096); return &b }}
)

// ANSI Colors
const (
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Blue   = "\033[34m"
	Cyan   = "\033[36m"
	Gray   = "\033[90m"
	White  = "\033[97m"
)

// PrettyHandler for development
type PrettyHandler struct {
	w       io.Writer
	mu      *sync.Mutex
	bufPool *sync.Pool
	attrs   []slog.Attr
	groups  []string
}

func NewPrettyHandler(w io.Writer) *PrettyHandler {
	return &PrettyHandler{
		w:       w,
		mu:      &sync.Mutex{},
		bufPool: &sync.Pool{New: func() any { b := make([]byte, 0, 1024); return &b }},
	}
}

func (h *PrettyHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *PrettyHandler) Handle(_ context.Context, r slog.Record) error {
	levelColor := White
	switch r.Level {
	case slog.LevelDebug:
		levelColor = Gray
	case slog.LevelInfo:
		levelColor = Green
	case slog.LevelWarn:
		levelColor = Yellow
	case slog.LevelError:
		levelColor = Red
	}

	pBuf := h.bufPool.Get().(*[]byte)
	buf := (*pBuf)[:0]
	buf = append(buf, Gray...)
	buf = r.Time.AppendFormat(buf, "15:04:05")
	buf = append(buf, " "...)
	buf = append(buf, Reset...)
	buf = append(buf, levelColor...)
	buf = append(buf, r.Level.String()...)
	buf = append(buf, " "...)
	buf = append(buf, Reset...)
	buf = append(buf, White...)
	buf = append(buf, r.Message...)
	buf = append(buf, Reset...)

	// Build group prefix for nested groups
	var prefix string
	if len(h.groups) > 0 {
		for _, g := range h.groups {
			prefix += g + "."
		}
	}

	// Add handler attrs
	for _, a := range h.attrs {
		buf = h.appendAttr(buf, a, prefix)
	}

	// Attributes
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, a, prefix)
		return true
	})

	buf = append(buf, '\n')

	// h.mu.Lock()
	_, err := h.w.Write(buf)
	// h.mu.Unlock()

	*pBuf = buf
	h.bufPool.Put(pBuf)
	return err
}

func (h *PrettyHandler) clone() *PrettyHandler {
	c := &PrettyHandler{
		w:       h.w,
		mu:      h.mu,
		bufPool: h.bufPool,
	}
	if len(h.attrs) > 0 {
		c.attrs = make([]slog.Attr, len(h.attrs))
		copy(c.attrs, h.attrs)
	}
	if len(h.groups) > 0 {
		c.groups = make([]string, len(h.groups))
		copy(c.groups, h.groups)
	}
	return c
}

func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := h.clone()
	c.attrs = append(c.attrs, attrs...)
	return c
}

func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := h.clone()
	c.groups = append(c.groups, name)
	return c
}

func (h *PrettyHandler) appendAttr(buf []byte, a slog.Attr, prefix string) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}

	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, attr := range a.Value.Group() {
			buf = h.appendAttr(buf, attr, prefix)
		}
		return buf
	}

	buf = append(buf, ' ')
	if prefix != "" {
		buf = append(buf, prefix...)
	}
	buf = append(buf, a.Key...)
	buf = append(buf, '=')
	buf = h.appendValue(buf, a.Value)
	buf = append(buf, Reset...)
	return buf
}

// Info logs a standard message to the console.
func Info(ctx context.Context, msg string, args ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelInfo, msg, args...)
}

func Debug(ctx context.Context, msg string, args ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelDebug, msg, args...)
}

func Warn(ctx context.Context, msg string, args ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelWarn, msg, args...)
}

// Error logs an error, prints a stack trace, and sends a notification to Google Chat if configured.
func Error(ctx context.Context, err error, msg string, args ...slog.Attr) {
	// 1. Log to Console with Stack Trace
	pBuf := stackBufPool.Get().(*[]byte)
	buf := (*pBuf)[:cap(*pBuf)]
	n := runtime.Stack(buf, false)
	stackTrace := string(buf[:n])
	attrs := append(
		[]slog.Attr{
			slog.String("error", err.Error()),
			slog.String("stack", stackTrace),
		},
		args...,
	)
	logger.LogAttrs(ctx, slog.LevelError, msg, attrs...)
	*pBuf = buf
	stackBufPool.Put(pBuf)

	// 2. Send to Google Chat if Env is set
	if webhookURL != "" {
		go sendToGoogleChat(err, msg, stackTrace)
	}
}

type googleChatMessage struct {
	Text string `json:"text"`
}

func sendToGoogleChat(err error, msg string, stack string) {
	payload := googleChatMessage{
		Text: fmt.Sprintf("🚨 *Error in System*\n*Message:* %s\n*Error:* %v\n*Time:* %s\n\n*Trace:* \n```\n%s\n```",
			msg, err, time.Now().Format(time.RFC3339), stack),
	}

	body, _ := json.Marshal(payload)
	resp, httpErr := http.Post(webhookURL, "application/json", bytes.NewBuffer(body))
	if httpErr != nil {
		fmt.Printf("Failed to send Google Chat alert: %v\n", httpErr)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Google Chat returned non-200 status: %d\n", resp.StatusCode)
	}
}

// Pretty returns a pretty-printed JSON string of the value
func Pretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(b)
}

func (h *PrettyHandler) appendValue(buf []byte, v slog.Value) []byte {
	switch v.Kind() {
	case slog.KindString:
		buf = append(buf, Gray...)
		return append(buf, v.String()...)
	case slog.KindInt64:
		buf = append(buf, Cyan...)
		return strconv.AppendInt(buf, v.Int64(), 10)
	case slog.KindBool:
		buf = append(buf, Yellow...)
		return strconv.AppendBool(buf, v.Bool())
	default:
		// Fallback for complex types (rarely hit in hot paths)
		buf = append(buf, Red...)
		return append(buf, fmt.Sprint(v.Any())...)
	}
}

func Fatal(ctx context.Context, err error, msg string, args ...slog.Attr) {
	Error(ctx, err, msg, args...)
	os.Exit(1)
}
