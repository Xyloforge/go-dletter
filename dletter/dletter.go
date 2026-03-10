package dletter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/valyala/fastjson"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Logger handles writing failed items to a recovery file
type Logger struct {
	filename    string
	retryMu     sync.Mutex
	retryWriter io.WriteCloser
	deadMu      sync.Mutex
	deadWriter  io.WriteCloser
	pool        sync.Pool
	parserPool  fastjson.ParserPool
}

type Envelope struct {
	Timestamp int64      `json:"ts"`
	Reason    string     `json:"reason"`
	Attempt   int        `json:"attempt"`
	Payload   RawPayload `json:"payload"`
}

type RawPayload []byte

func (r RawPayload) AppendLog(buf []byte) []byte {
	return append(buf, r...)
}

func (r *RawPayload) UnmarshalJSON(data []byte) error {
	*r = append((*r)[0:0], data...)
	return nil
}

type Loggable interface {
	AppendLog(buf []byte) []byte
}

func newWithWriter(retryWriter, deadWriter io.WriteCloser) *Logger {
	return &Logger{
		retryWriter: retryWriter,
		deadWriter:  deadWriter,
		pool: sync.Pool{
			New: func() any {
				b := make([]byte, 0, 1024)
				return &b
			},
		},
	}
}

func New(filename string, opts ...Option) (*Logger, error) {
	lj := &lumberjack.Logger{
		Filename:   filename,
		MaxSize:    100,
		MaxBackups: 5,
		MaxAge:     14,
		Compress:   true,
	}

	permanentFailFile := "permanent-" + filepath.Base(filename)
	lj2 := &lumberjack.Logger{
		Filename:   filepath.Join(filepath.Dir(filename), permanentFailFile),
		MaxSize:    100,
		MaxBackups: 5,
		MaxAge:     14,
		Compress:   true,
	}

	for _, applyOpt := range opts {
		applyOpt(lj)
		applyOpt(lj2)
	}

	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	l := newWithWriter(lj, lj2)
	l.filename = filename
	return l, nil
}

func Log[T Loggable](l *Logger, data T, reason error, attempt int) error {
	if l == nil {
		return errors.New("logger is nil")
	}
	pBuf := l.pool.Get().(*[]byte)
	buf := (*pBuf)[:0]

	buf = append(buf, []byte(`{"ts":`)...)
	buf = strconv.AppendInt(buf, time.Now().Unix(), 10)
	buf = append(buf, []byte(`,"attempt":`)...)
	buf = strconv.AppendInt(buf, int64(attempt), 10)
	buf = append(buf, []byte(`,"reason":`)...)

	buf = append(buf, '"')
	buf = appendEscapedJSON(buf, reason.Error())
	buf = append(buf, '"')

	buf = append(buf, []byte(`,"payload":`)...)
	buf = data.AppendLog(buf)
	buf = append(buf, []byte("}\n")...)

	l.retryMu.Lock()
	_, err := l.retryWriter.Write(buf)
	l.retryMu.Unlock()

	*pBuf = buf[:0]
	l.pool.Put(pBuf)
	return err
}

func (l *Logger) LogPermanent(data []byte, reason string) error {
	if l == nil {
		return errors.New("logger is nil")
	}
	pBuf := l.pool.Get().(*[]byte)
	buf := (*pBuf)[:0]

	buf = append(buf, []byte(`{"ts":`)...)
	buf = strconv.AppendInt(buf, time.Now().Unix(), 10)
	buf = append(buf, []byte(`,"reason":`)...)

	buf = append(buf, '"')
	buf = appendEscapedJSON(buf, reason)
	buf = append(buf, '"')

	buf = append(buf, []byte(`,"payload":`)...)
	buf = append(buf, data...)
	buf = append(buf, []byte("}\n")...)

	l.deadMu.Lock()
	_, err := l.deadWriter.Write(buf)
	l.deadMu.Unlock()

	*pBuf = buf[:0]
	l.pool.Put(pBuf)
	return err
}

// Close closes the file handle
func (l *Logger) Close() error {
	l.retryMu.Lock()
	defer l.retryMu.Unlock()
	l.deadMu.Lock()
	defer l.deadMu.Unlock()

	errs := make([]error, 0, 2)
	if l.retryWriter != nil {
		errs = append(errs, l.retryWriter.Close())
	}
	if l.deadWriter != nil {
		errs = append(errs, l.deadWriter.Close())
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

const hexChars = "0123456789abcdef"

func appendEscapedJSON(buf []byte, s string) []byte {
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x80 {
			// Non-ASCII: validate and pass through the full UTF-8 sequence.
			// utf8.DecodeRuneInString returns RuneError+size=1 for invalid bytes.
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				buf = append(buf, `\ufffd`...)
			} else {
				buf = append(buf, s[i:i+size]...)
			}
			i += size
			continue
		}
		i++
		switch c {
		case '"':
			buf = append(buf, `\"`...)
		case '\\':
			buf = append(buf, `\\`...)
		case '\n':
			buf = append(buf, `\n`...)
		case '\r':
			buf = append(buf, `\r`...)
		case '\t':
			buf = append(buf, `\t`...)
		case '\b':
			buf = append(buf, `\b`...)
		case '\f':
			buf = append(buf, `\f`...)
		default:
			if c < 0x20 {
				// Remaining control characters: \u00XX
				buf = append(buf, '\\', 'u', '0', '0', hexChars[c>>4], hexChars[c&0xf])
			} else {
				buf = append(buf, c)
			}
		}
	}
	return buf
}

func (l *Logger) Rotate() error {
	l.retryMu.Lock()
	defer l.retryMu.Unlock()

	if lj, ok := l.retryWriter.(*lumberjack.Logger); ok {
		return lj.Rotate()
	}
	return errors.New("underlying writer does not support rotation")
}
