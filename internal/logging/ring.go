package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	DefaultMaxEntries   = 400
	maxMessageRunesByte = 1024
)

// Entry represents a single runtime log line stored in the bounded in-memory ring buffer.
type Entry struct {
	Seq       uint64 `json:"seq"`
	Timestamp int64  `json:"timestamp"`
	Level     string `json:"level"`
	Source    string `json:"source,omitempty"`
	Message   string `json:"message"`
}

// RingBuffer is a concurrency-safe, fixed-capacity circular buffer of runtime log entries
// whose peak heap footprint is strictly bounded (~200 KiB at 400 entries).
type RingBuffer struct {
	mu      sync.RWMutex
	entries []Entry
	head    int
	count   int
	seq     uint64
}

var defaultBuffer = NewRingBuffer(DefaultMaxEntries)

// Default returns the process-wide runtime log ring buffer.
func Default() *RingBuffer {
	return defaultBuffer
}

// NewRingBuffer allocates a fixed-capacity ring buffer.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = DefaultMaxEntries
	}
	return &RingBuffer{
		entries: make([]Entry, capacity),
	}
}

func truncateUTF8Bytes(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	if maxBytes <= 0 {
		return "", true
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// Record appends a single runtime log entry into the circular buffer.
func (rb *RingBuffer) Record(level string, source string, message string) {
	if rb == nil {
		return
	}
	msg := strings.TrimSpace(message)
	if msg == "" {
		return
	}
	if prefix, truncated := truncateUTF8Bytes(msg, maxMessageRunesByte); truncated {
		msg = prefix + "..."
	}
	lvl := normalizeLevel(level)
	src := strings.TrimSpace(source)

	rb.mu.Lock()
	rb.seq++
	rb.entries[rb.head] = Entry{
		Seq:       rb.seq,
		Timestamp: time.Now().UnixMilli(),
		Level:     lvl,
		Source:    src,
		Message:   msg,
	}
	rb.head = (rb.head + 1) % len(rb.entries)
	if rb.count < len(rb.entries) {
		rb.count++
	}
	rb.mu.Unlock()
}

// Write implements io.Writer so legacy/scheduler io.Writer loggers feed into the ring buffer.
func (rb *RingBuffer) Write(p []byte) (int, error) {
	n := len(p)
	for _, line := range bytes.Split(bytes.TrimSpace(p), []byte{'\n'}) {
		s := strings.TrimSpace(string(line))
		if s == "" {
			continue
		}
		lvl := "INFO"
		low := strings.ToLower(s)
		if strings.Contains(low, "error") || strings.Contains(low, "failed") || strings.Contains(low, "panic") {
			lvl = "ERROR"
		} else if strings.Contains(low, "warn") {
			lvl = "WARN"
		}
		rb.Record(lvl, "runtime", s)
	}
	return n, nil
}

// Count returns the current number of buffered entries.
func (rb *RingBuffer) Count() int {
	if rb == nil {
		return 0
	}
	rb.mu.RLock()
	c := rb.count
	rb.mu.RUnlock()
	return c
}

// Clear resets the ring buffer.
func (rb *RingBuffer) Clear() {
	if rb == nil {
		return
	}
	rb.mu.Lock()
	for i := range rb.entries {
		rb.entries[i] = Entry{}
	}
	rb.head = 0
	rb.count = 0
	rb.mu.Unlock()
}

// List returns buffered entries in newest-first order, optionally filtered by level and substring query.
func (rb *RingBuffer) List(levelFilter string, query string, limit int) []Entry {
	if rb == nil {
		return []Entry{}
	}
	lvl := strings.ToUpper(strings.TrimSpace(levelFilter))
	if lvl == "ALL" {
		lvl = ""
	}
	q := strings.ToLower(strings.TrimSpace(query))
	const maxRingListCap = 4096
	if limit <= 0 || limit > maxRingListCap {
		limit = maxRingListCap
	}

	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.count == 0 || len(rb.entries) == 0 {
		return []Entry{}
	}
	if limit > len(rb.entries) {
		limit = len(rb.entries)
	}
	initCap := len(rb.entries)
	if initCap > 64 {
		initCap = 64
	}
	out := make([]Entry, 0, initCap)
	capN := len(rb.entries)
	for i := 0; i < rb.count; i++ {
		idx := (rb.head - 1 - i + capN) % capN
		ent := rb.entries[idx]
		if lvl != "" && ent.Level != lvl {
			continue
		}
		if q != "" {
			if !strings.Contains(strings.ToLower(ent.Message), q) &&
				!strings.Contains(strings.ToLower(ent.Source), q) &&
				!strings.Contains(strings.ToLower(ent.Level), q) {
				continue
			}
		}
		out = append(out, ent)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// FormatText renders entries in chronological order as plain-text log lines for export/copy.
func FormatText(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		ts := time.UnixMilli(e.Timestamp).Format("2006-01-02 15:04:05.000")
		if e.Source != "" {
			fmt.Fprintf(&b, "%s [%-5s] (%s) %s\n", ts, e.Level, e.Source, e.Message)
		} else {
			fmt.Fprintf(&b, "%s [%-5s] %s\n", ts, e.Level, e.Message)
		}
	}
	return b.String()
}

func normalizeLevel(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "DEBUG":
		return "DEBUG"
	case "WARN", "WARNING":
		return "WARN"
	case "ERROR", "ERR", "FATAL":
		return "ERROR"
	default:
		return "INFO"
	}
}

// TeeWriter duplicates writes to both an output writer (e.g. stderr) and a RingBuffer.
func TeeWriter(out io.Writer, rb *RingBuffer) io.Writer {
	if out == nil && rb == nil {
		return io.Discard
	}
	if out == nil {
		return rb
	}
	if rb == nil {
		return out
	}
	return io.MultiWriter(out, rb)
}

type ringSlogHandler struct {
	rb       *RingBuffer
	out      io.Writer
	outMu    *sync.Mutex
	minLevel slog.Level
	source   string
	preAttrs string
}

// NewSlogHandler creates a slog.Handler that writes concise lines to out and records entries into rb.
func NewSlogHandler(out io.Writer, rb *RingBuffer, minLevel slog.Level) slog.Handler {
	return &ringSlogHandler{
		rb:       rb,
		out:      out,
		outMu:    &sync.Mutex{},
		minLevel: minLevel,
	}
}

func (h *ringSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

func (h *ringSlogHandler) Handle(_ context.Context, r slog.Record) error {
	lvl := "INFO"
	switch {
	case r.Level >= slog.LevelError:
		lvl = "ERROR"
	case r.Level >= slog.LevelWarn:
		lvl = "WARN"
	case r.Level < slog.LevelInfo:
		lvl = "DEBUG"
	}

	src := h.source
	var b strings.Builder
	b.WriteString(r.Message)
	if h.preAttrs != "" {
		b.WriteByte(' ')
		b.WriteString(h.preAttrs)
	}
	r.Attrs(func(a slog.Attr) bool {
		a.Value = a.Value.Resolve()
		if a.Equal(slog.Attr{}) {
			return true
		}
		if a.Key == "component" || a.Key == "source" {
			if src == "" {
				src = a.Value.String()
				return true
			}
		}
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})

	msg := b.String()
	if h.rb != nil {
		h.rb.Record(lvl, src, msg)
	}
	if h.out != nil {
		ts := r.Time
		if ts.IsZero() {
			ts = time.Now()
		}
		line := fmt.Sprintf("%s [%-5s] %s\n", ts.Format("2006-01-02 15:04:05"), lvl, msg)
		if src != "" {
			line = fmt.Sprintf("%s [%-5s] (%s) %s\n", ts.Format("2006-01-02 15:04:05"), lvl, src, msg)
		}
		h.outMu.Lock()
		_, _ = io.WriteString(h.out, line)
		h.outMu.Unlock()
	}
	return nil
}

func (h *ringSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	var parts []string
	if next.preAttrs != "" {
		parts = append(parts, next.preAttrs)
	}
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		if a.Equal(slog.Attr{}) {
			continue
		}
		if (a.Key == "component" || a.Key == "source") && next.source == "" {
			next.source = a.Value.String()
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", a.Key, a.Value.Any()))
	}
	next.preAttrs = strings.Join(parts, " ")
	return &next
}

func (h *ringSlogHandler) WithGroup(name string) slog.Handler {
	next := *h
	if next.source == "" {
		next.source = strings.TrimSpace(name)
	}
	return &next
}
