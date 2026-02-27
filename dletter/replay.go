package dletter

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// ReplayOptions configures the behavior of the replay mechanism.
type ReplayOptions struct {
	MaxAttempts int
	InitialWait time.Duration
}

// Handler defines a callback matching each log item parsed out.
type Handler func(payload []byte) error

// Replay reads recorded payloads out securely sequentially from the disk, blocking context and pushing items against handler.
// Handlers who error again are recorded automatically back to disk log, advancing attempt counts.
func (l *Logger) Replay(ctx context.Context, handler Handler, opts ReplayOptions) error {
	if err := l.Rotate(); err != nil {
		return fmt.Errorf("failed to rotate active log: %w", err)
	}

	backups, err := l.backupFiles()
	if err != nil {
		return err
	}

	payloadBuf := make([]byte, 0, 1024)

	for _, backupPath := range backups {
		if err := l.replayFile(ctx, backupPath, handler, opts, &payloadBuf); err != nil {
			return err
		}
	}

	return nil
}

func (l *Logger) replayFile(ctx context.Context, path string, handler Handler, opts ReplayOptions, payloadBuf *[]byte) error {
	rc, err := openBackup(path)
	if err != nil {
		return err
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		parser := l.parserPool.Get()

		v, err := parser.ParseBytes(scanner.Bytes())
		if err != nil {
			l.parserPool.Put(parser)
			continue
		}

		attempt := v.GetInt("attempt")
		*payloadBuf = v.Get("payload").MarshalTo((*payloadBuf)[:0])

		if opts.MaxAttempts > 0 && attempt >= opts.MaxAttempts {
			l.parserPool.Put(parser)
			l.LogPermanent(*payloadBuf, v.Get("reason").String())
			continue
		}

		l.parserPool.Put(parser)

		wait := calcWait(opts.InitialWait, attempt)

		time.Sleep(wait)

		curAttempt := attempt + 1
		if err := handler(*payloadBuf); err != nil {
			Log(l, RawPayload(*payloadBuf), err, curAttempt)
		}
		*payloadBuf = (*payloadBuf)[:0]
	}

	os.Remove(path)
	return nil
}

func calcWait(initial time.Duration, attempt int) time.Duration {
	const maxWait = 30 * time.Second

	wait := initial * time.Duration(1<<uint(attempt))
	if wait > maxWait {
		wait = maxWait
	}

	jitterBase := int64(wait / 10)
	if jitterBase > 0 {
		wait += time.Duration(rand.Int63n(jitterBase))
	}
	return wait
}

func (l *Logger) backupFiles() ([]string, error) {
	dir := filepath.Dir(l.filename)
	base := filepath.Base(l.filename)
	ext := filepath.Ext(base)
	prefix := base[:len(base)-len(ext)]

	pattern := filepath.Join(dir, prefix+"-*"+ext)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	gzPattern := filepath.Join(dir, prefix+"-*"+ext+".gz")
	gzMatches, err := filepath.Glob(gzPattern)
	if err != nil {
		return nil, err
	}

	return append(matches, gzMatches...), nil
}

func openBackup(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	if filepath.Ext(path) == ".gz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return &gzReadCloser{gz: gz, f: f}, nil
	}

	return f, nil
}

// need a wrapper to close both gz reader and underlying file
type gzReadCloser struct {
	gz *gzip.Reader
	f  *os.File
}

func (g *gzReadCloser) Read(p []byte) (n int, err error) { return g.gz.Read(p) }
func (g *gzReadCloser) Close() error {
	g.gz.Close()
	return g.f.Close()
}
