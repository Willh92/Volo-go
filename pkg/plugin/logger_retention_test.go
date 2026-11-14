package plugin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPluginLoggerClearAndPruneExpiredLogs(t *testing.T) {
	dir := t.TempDir()
	logger := NewPluginLogger(dir)

	oldLine := `{"ts":"` + time.Now().Add(-10*24*time.Hour).UTC().Format(time.RFC3339) + `","level":"info","plugin":"demo","msg":"old entry"}` + "\n"
	newLine := `{"ts":"` + time.Now().Add(-1*time.Hour).UTC().Format(time.RFC3339) + `","level":"warn","plugin":"demo","msg":"recent entry"}` + "\n"
	logFile := filepath.Join(dir, "logs", "plugins", "demo.jsonl")
	if err := os.WriteFile(logFile, []byte(oldLine+newLine), 0o600); err != nil {
		t.Fatal(err)
	}

	// Prune older than 3 days -> keeps recent entry, drops old entry
	logger.PruneExpiredLogs(3 * 24 * time.Hour)
	entries, err := logger.Tail("demo", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != "recent entry" {
		t.Fatalf("expected only recent entry after prune, got %+v", entries)
	}

	// ClearLogs removes all entries cleanly
	if err := logger.ClearLogs("demo"); err != nil {
		t.Fatalf("ClearLogs failed: %v", err)
	}
	entriesAfter, err := logger.Tail("demo", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != 0 {
		t.Fatalf("expected empty logs after ClearLogs, got %+v", entriesAfter)
	}
}
