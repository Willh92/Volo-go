package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

func TestUnixSocketListenerPermissionsLockAndGracefulShutdown(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "volo.sock")

	store, err := sqlx.Open(context.Background(), sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "srv.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(context.Background(), store); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		ListenSocket: &config.ListenSocketSpec{
			Path: sockPath,
			Perm: 0o660,
		},
		ShutdownTimeout: 5 * time.Second,
	}

	handle, err := OpenListener(cfg)
	if err != nil {
		t.Fatalf("OpenListener unix failed: %v", err)
	}

	// Verify socket file permissions (0660) and lock file permissions (0600)
	sockInfo, err := os.Lstat(sockPath)
	if err != nil {
		t.Fatalf("lstat socket: %v", err)
	}
	if sockInfo.Mode().Perm() != 0o660 {
		t.Fatalf("expected socket perm 0660, got %04o", sockInfo.Mode().Perm())
	}
	lockInfo, err := os.Lstat(sockPath + ".lock")
	if err != nil {
		t.Fatalf("lstat lock: %v", err)
	}
	if lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected lock perm 0600, got %04o", lockInfo.Mode().Perm())
	}

	// Starting a second listener on an active socket MUST fail without unlinking the live socket!
	if _, dupErr := OpenListener(cfg); dupErr == nil {
		t.Fatal("expected duplicate unix socket bind to fail while live")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", NewHealthzHandler(store, time.Now().Add(-3*time.Second)))

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- ServeWithGracefulShutdown(ctx, handle, mux, 3*time.Second)
	}()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}

	resp, err := client.Get("http://unix/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over unix socket failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /healthz, got %d", resp.StatusCode)
	}
	var hr HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		t.Fatal(err)
	}
	if hr.Status != "ok" || hr.Database != "ok" || hr.UptimeSec < 2 {
		t.Fatalf("unexpected healthz payload: %+v", hr)
	}

	// Trigger graceful shutdown and verify socket & lock files are unlinked cleanly
	cancel()
	if err := <-doneCh; err != nil {
		t.Fatalf("graceful shutdown error: %v", err)
	}
	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("expected unix socket %q to be unlinked on shutdown, got err=%v", sockPath, err)
	}
	if _, err := os.Lstat(sockPath + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("expected lock file to be unlinked on shutdown, got err=%v", err)
	}
}
