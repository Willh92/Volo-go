package plugin

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxPluginLogBytes = 1024 * 1024 // 1 MiB rolling threshold per plugin log file
	maxLogMessageLen  = 4096
)

// LogEntry represents a single structured JSONL line in ./data/logs/plugins/<id>.jsonl.
type LogEntry struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"`
	PluginID  string `json:"plugin_id"`
	Event     string `json:"event,omitempty"`
	Message   string `json:"msg"`
}

// PluginLogger manages isolated, size-bounded JSONL rolling log files per plugin.
type PluginLogger struct {
	mu      sync.Mutex
	baseDir string
}

// NewPluginLogger initializes the isolated plugin log directory (<dataDir>/logs/plugins).
func NewPluginLogger(dataDir string) *PluginLogger {
	dir := filepath.Join(dataDir, "logs", "plugins")
	_ = os.MkdirAll(dir, 0o750)
	return &PluginLogger{baseDir: dir}
}

func (l *PluginLogger) logPath(pluginID string) (string, bool) {
	if !validPluginIDRe.MatchString(pluginID) {
		return "", false
	}
	return filepath.Join(l.baseDir, pluginID+".jsonl"), true
}

func truncateLogUTF8Bytes(s string, maxBytes int) (string, bool) {
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

// Write appends a structured JSONL entry and rotates <id>.jsonl -> <id>.jsonl.1 if size exceeds 1 MiB.
func (l *PluginLogger) Write(pluginID string, level string, event string, msg string) {
	if l == nil || l.baseDir == "" {
		return
	}
	path, ok := l.logPath(pluginID)
	if !ok {
		return
	}
	trimmed := strings.TrimSpace(msg)
	if prefix, truncated := truncateLogUTF8Bytes(trimmed, maxLogMessageLen); truncated {
		trimmed = prefix + "...[truncated]"
	}
	entry := LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     strings.ToLower(strings.TrimSpace(level)),
		PluginID:  pluginID,
		Event:     strings.TrimSpace(event),
		Message:   trimmed,
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	_ = os.MkdirAll(l.baseDir, 0o750)
	if st, sErr := os.Stat(path); sErr == nil && st.Size() >= maxPluginLogBytes {
		_ = os.Rename(path, path+".1")
	}
	f, oErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if oErr != nil {
		return
	}
	_, _ = f.Write(append(raw, '\n'))
	_ = f.Close()
}

// Tail returns up to limit recent LogEntry records in chronological order.
func (l *PluginLogger) Tail(pluginID string, limit int) ([]LogEntry, error) {
	if l == nil || l.baseDir == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	path, ok := l.logPath(pluginID)
	if !ok {
		return nil, nil
	}

	l.mu.Lock()
	f, err := os.Open(path)
	l.mu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return []LogEntry{}, nil
		}
		return nil, err
	}
	defer f.Close()

	ring := make([]LogEntry, 0, limit)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 8192), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ent LogEntry
		if err := json.Unmarshal([]byte(line), &ent); err == nil {
			if len(ring) == limit {
				copy(ring, ring[1:])
				ring[limit-1] = ent
			} else {
				ring = append(ring, ent)
			}
		}
	}
	return ring, scanner.Err()
}

// RemoveLogs deletes log files for an uninstalled plugin.
func (l *PluginLogger) RemoveLogs(pluginID string) {
	if l == nil || l.baseDir == "" {
		return
	}
	path, ok := l.logPath(pluginID)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = os.Remove(path)
	_ = os.Remove(path + ".1")
}

// ClearLogs truncates/removes runtime JSONL log files for a specific plugin.
func (l *PluginLogger) ClearLogs(pluginID string) error {
	if l == nil || l.baseDir == "" {
		return nil
	}
	path, ok := l.logPath(pluginID)
	if !ok {
		return os.ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = os.Remove(path)
	_ = os.Remove(path + ".1")
	return nil
}

// PruneExpiredLogs removes JSONL log entries older than maxAge across all plugin log files.
// A maxAge <= 0 disables pruning (retains logs indefinitely).
func (l *PluginLogger) PruneExpiredLogs(maxAge time.Duration) {
	if l == nil || l.baseDir == "" || maxAge <= 0 {
		return
	}
	cutoff := time.Now().Add(-maxAge)

	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := os.ReadDir(l.baseDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".jsonl.1") {
			continue
		}
		fullPath := filepath.Join(l.baseDir, name)
		prunePluginLogFileLocked(fullPath, cutoff)
	}
}

func prunePluginLogFileLocked(fullPath string, cutoff time.Time) {
	f, err := os.Open(fullPath)
	if err != nil {
		return
	}
	var kept []string
	dropped := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 8192), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ent LogEntry
		if uErr := json.Unmarshal([]byte(line), &ent); uErr == nil && ent.Timestamp != "" {
			if ts, pErr := time.Parse(time.RFC3339, ent.Timestamp); pErr == nil && ts.Before(cutoff) {
				dropped = true
				continue
			}
		}
		kept = append(kept, line)
	}
	scanErr := scanner.Err()
	_ = f.Close()
	if scanErr != nil {
		return
	}
	if len(kept) == 0 {
		_ = os.Remove(fullPath)
		return
	}
	if !dropped {
		return
	}
	tmpPath := fullPath + ".tmp"
	if wErr := os.WriteFile(tmpPath, []byte(strings.Join(kept, "\n")+"\n"), 0o640); wErr == nil {
		_ = os.Rename(tmpPath, fullPath)
	} else {
		_ = os.Remove(tmpPath)
	}
}
