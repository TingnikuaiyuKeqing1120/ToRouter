package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"torouter/internal/console"
	"torouter/internal/store"
)

// maxLogSize caps the log before it rotates, so a resident process cannot fill
// the disk.
const maxLogSize = 4 << 20

// Logger writes process-level events to a file in the data directory. It is for
// start-up, port changes, shutdown and failures; per-request records live in
// the database and the web console.
//
// Every line goes through the shared redaction, because upstream error text can
// contain prompts or credentials.
type Logger struct {
	path  string
	store *store.Store
	mu    sync.Mutex
	// cached and lastScan throttle the store reads that gather values to mask,
	// so an error storm cannot hammer the database.
	lastScan time.Time
	cached   []string
}

// NewLogger prepares the log file next to the database.
func NewLogger(dataDir string, st *store.Store) (*Logger, error) {
	l := &Logger{path: LogPath(dataDir), store: st}
	if _, err := os.Stat(l.path); err == nil {
		l.rotate()
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	// Route the default logger into the same file. The tray build has no
	// stderr, so without this every package-level slog line (gateway warnings,
	// OAuth diagnostics) silently vanishes for GUI users.
	slog.SetDefault(slog.New(fileHandler{l: l}))
	return l, nil
}

// fileHandler adapts Logger to slog so library-level logging shares the file,
// the rotation and the credential masking.
type fileHandler struct{ l *Logger }

func (h fileHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h fileHandler) Handle(_ context.Context, r slog.Record) error {
	fields := make([]any, 0, r.NumAttrs()*2)
	r.Attrs(func(a slog.Attr) bool {
		fields = append(fields, a.Key, a.Value.Any())
		return true
	})
	h.l.write(r.Level.String(), r.Message, fields...)
	return nil
}

func (h fileHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h fileHandler) WithGroup(_ string) slog.Handler      { return h }

func (l *Logger) rotate() {
	info, err := os.Stat(l.path)
	if err != nil || info.Size() < maxLogSize {
		return
	}
	previous := l.path + ".1"
	_ = os.Remove(previous)
	// Keep the current file rather than losing the record if the rename fails.
	_ = os.Rename(l.path, previous)
}

// LogStartupFailure appends a start-up failure to the process log. A GUI build
// has nowhere else to record it, and a dialog the user dismisses leaves no
// trace otherwise.
func LogStartupFailure(dataDir string, err error) {
	if err == nil {
		return
	}
	logger, openErr := NewLogger(dataDir, nil)
	if openErr != nil {
		return
	}
	logger.Error("startup failed", "error", err)
}

// Event records a normal lifecycle step.
func (l *Logger) Event(name string, fields ...any) { l.write("INFO", name, fields...) }

// Warn records a recoverable problem the user should know about.
func (l *Logger) Warn(name string, fields ...any) { l.write("WARN", name, fields...) }

// Error records a failure.
func (l *Logger) Error(name string, fields ...any) { l.write("ERROR", name, fields...) }

func (l *Logger) write(level, name string, fields ...any) {
	if l == nil {
		return
	}
	var line strings.Builder
	line.WriteString(level + " " + time.Now().Format("2006-01-02 15:04:05.000") + " " + console.Redact(name, l.gather()...))
	for i := 0; i+1 < len(fields); i += 2 {
		key := strings.TrimSpace(console.Redact(fmt.Sprint(fields[i])))
		line.WriteString(" " + key + "=" + quote(console.Redact(fmt.Sprint(fields[i+1]), l.gather()...)))
	}
	line.WriteString("\n")

	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotate()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line.String())
}

func quote(value string) string {
	if value == "" {
		return `""`
	}
	// Newlines break the one-line-per-event format; show them escaped so a
	// multi-line upstream body stays on its own single line.
	value = strings.ReplaceAll(value, "\n", `\n`)
	if strings.ContainsAny(value, " \t\r\n\"") {
		return `"` + strings.ReplaceAll(value, `"`, `'`) + `"`
	}
	return value
}

// gather collects credential-shaped values to mask, refreshed at most once a
// second.
func (l *Logger) gather() []string {
	if time.Since(l.lastScan) < time.Second {
		return l.cached
	}
	l.lastScan = time.Now()
	l.cached = nil
	if l.store == nil {
		return l.cached
	}
	if settings, err := l.store.GetSettings(); err == nil && settings.GatewayKey != "" {
		l.cached = append(l.cached, settings.GatewayKey)
	}
	if channels, err := l.store.ListChannels(); err == nil {
		for _, c := range channels {
			if c.APIKey != "" {
				l.cached = append(l.cached, c.APIKey)
			}
			// An OAuth channel carries two: the long-lived GitHub token and the
			// short-lived bearer. Both must be masked wherever an upstream
			// error, a request echo or a diagnostic happens to carry them.
			if c.OAuth != nil {
				if c.OAuth.GitHubToken != "" {
					l.cached = append(l.cached, c.OAuth.GitHubToken)
				}
				if c.OAuth.AccessToken != "" {
					l.cached = append(l.cached, c.OAuth.AccessToken)
				}
			}
		}
	}
	return l.cached
}

// Close exists so callers never have to nil-check the logger; the file is
// opened per write.
func (l *Logger) Close() {}

// OpenPath hands a path to the operating system's default handler.
func OpenPath(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	return openWithShell((&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String())
}
