package dletter

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
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

	stateF, startLine := getOrCreateStateFile(path)
	if stateF == nil {
		return fmt.Errorf("failed creating state file for back up file")
	}
	defer stateF.Close()

	scanner := bufio.NewScanner(rc)
	scanLine := int64(0)
	for scanner.Scan() {
		if scanLine < startLine {
			scanLine++
			continue
		}

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
			LogPermanent(l, RawPayload(*payloadBuf), v.Get("reason").String())
			continue
		}

		l.parserPool.Put(parser)

		wait := calcWait(opts.InitialWait, attempt)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		timer.Stop()

		curAttempt := attempt + 1
		if err := handler(*payloadBuf); err != nil {
			Log(l, RawPayload(*payloadBuf), err, curAttempt)
		}
		saveStateFile(stateF, scanLine+1)
		*payloadBuf = (*payloadBuf)[:0]
		scanLine++
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}

	os.Remove(path)
	os.Remove(stateF.Name())
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
		wait += time.Duration(rand.Int64N(jitterBase))
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

	all := append(matches, gzMatches...)
	return sortByMtime(all)
}

func sortByMtime(paths []string) ([]string, error) {
	type entry struct {
		path  string
		mtime time.Time
	}
	entries := make([]entry, 0, len(paths))
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
		entries = append(entries, entry{p, info.ModTime()})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		return a.mtime.Compare(b.mtime)
	})
	sorted := make([]string, len(entries))
	for i, e := range entries {
		sorted[i] = e.path
	}
	return sorted, nil
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

func saveStateFile(f *os.File, lineNumber int64) {
	if f == nil {
		return
	}
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(lineNumber))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(lineNumber)^0xDEADBEEFCAFEBABE)
	f.Seek(0, io.SeekStart)
	f.Write(buf)
}

func getOrCreateStateFile(replayFilePath string) (f *os.File, lineNumber int64) {
	fileDir := filepath.Dir(replayFilePath)
	fileName := filepath.Base(replayFilePath)
	statePath := filepath.Join(fileDir, fileName+"-state")

	f, err := os.OpenFile(statePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, 0
	}

	buf := make([]byte, 16)
	if _, err := io.ReadFull(f, buf); err != nil {
		// file is new/empty, start from 0
		return f, 0
	}

	line := binary.LittleEndian.Uint64(buf[0:8])
	check := binary.LittleEndian.Uint64(buf[8:16])
	if check != (line ^ 0xDEADBEEFCAFEBABE) {
		return f, 0 // corrupted, start from beginning
	}

	return f, int64(line)
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
