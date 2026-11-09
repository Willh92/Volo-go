package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

// ListenerHandle wraps either a TCP net.Listener or a hardened POSIX Unix Domain Socket listener
// along with deterministic lock/socket cleanup on graceful shutdown.
type ListenerHandle struct {
	_ [0]func()

	NetListener net.Listener
	Network     string // "tcp" | "unix"
	Address     string
	cleanupOnce sync.Once
	cleanupFn   func()
}

func (h *ListenerHandle) Close() error {
	err := h.NetListener.Close()
	h.Cleanup()
	return err
}

func (h *ListenerHandle) Cleanup() {
	if h == nil {
		return
	}
	h.cleanupOnce.Do(func() {
		if h.cleanupFn != nil {
			h.cleanupFn()
		}
	})
}

// OpenListener binds either VOLO_LISTEN_SOCKET (Unix Domain Socket with explicit chmod, parent symlink
// guard, live-process probe, and 0600 .sock.lock) or VOLO_LISTEN_ADDR (TCP).
func OpenListener(cfg *config.Config) (*ListenerHandle, error) {
	if cfg == nil {
		return nil, errors.New("server: nil config")
	}
	if cfg.ListenSocket != nil && strings.TrimSpace(cfg.ListenSocket.Path) != "" {
		return openUnixSocketListener(cfg.ListenSocket)
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("server: listen tcp %s: %w", cfg.ListenAddr, err)
	}
	return &ListenerHandle{
		NetListener: ln,
		Network:     "tcp",
		Address:     ln.Addr().String(),
	}, nil
}

func openUnixSocketListener(spec *config.ListenSocketSpec) (*ListenerHandle, error) {
	sockPath := filepath.Clean(spec.Path)
	parentDir := filepath.Dir(sockPath)

	if err := os.MkdirAll(parentDir, 0o750); err != nil {
		return nil, fmt.Errorf("server: create socket parent dir %q: %w", parentDir, err)
	}
	parentInfo, err := os.Lstat(parentDir)
	if err != nil {
		return nil, fmt.Errorf("server: stat socket parent dir %q: %w", parentDir, err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("server: refusing symlinked socket parent directory %q", parentDir)
	}

	// Check if sockPath already exists: reject regular files/symlinks, and refuse clobbering an active socket!
	if st, err := os.Lstat(sockPath); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("server: refusing symlink at socket path %q", sockPath)
		}
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("server: path %q exists and is not a unix socket", sockPath)
		}
		// Probe if another live process is actively listening on this Unix socket
		probeConn, dialErr := net.DialTimeout("unix", sockPath, 300*time.Millisecond)
		if dialErr == nil {
			_ = probeConn.Close()
			return nil, fmt.Errorf("server: unix socket %q is already in use by another running instance", sockPath)
		}
		if rmErr := os.Remove(sockPath); rmErr != nil {
			return nil, fmt.Errorf("server: remove stale unix socket %q: %w", sockPath, rmErr)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("server: lstat socket %q: %w", sockPath, err)
	}

	lockPath := sockPath + ".lock"
	if lInfo, lErr := os.Lstat(lockPath); lErr == nil && lInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("server: refusing symlinked socket lock file %q", lockPath)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("server: create socket lock %q: %w", lockPath, err)
	}
	_, _ = lockFile.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	_ = lockFile.Close()
	_ = os.Chmod(lockPath, 0o600)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("server: listen unix %q: %w", sockPath, err)
	}

	perm := spec.Perm
	if perm == 0 {
		perm = config.DefaultUnixSocketPerm
	}
	if err := os.Chmod(sockPath, perm); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("server: chmod unix socket %q to %04o: %w", sockPath, perm, err)
	}

	return &ListenerHandle{
		NetListener: ln,
		Network:     "unix",
		Address:     sockPath,
		cleanupFn: func() {
			_ = os.Remove(sockPath)
			_ = os.Remove(lockPath)
		},
	}, nil
}

// HealthResponse is the public, zero-secret readiness/liveness payload returned by GET /healthz.
type HealthResponse struct {
	_ [0]func()

	Status    string `json:"status"`
	Database  string `json:"database"`
	UptimeSec int64  `json:"uptime_sec"`
}

// NewHealthzHandler returns a fast liveness & DB readiness HTTP handler for /healthz.
func NewHealthzHandler(store *sqlx.Store, startedAt time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status := http.StatusOK
		resp := HealthResponse{
			Status:    "ok",
			Database:  "ok",
			UptimeSec: int64(time.Since(startedAt).Seconds()),
		}
		if store != nil {
			if err := store.Ping(ctx); err != nil {
				status = http.StatusServiceUnavailable
				resp.Status = "degraded"
				resp.Database = "unavailable"
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// ServeWithGracefulShutdown runs an hardened *http.Server on handle until ctx is canceled,
// then drains in-flight requests within shutdownTimeout and cleans up Unix socket files.
func ServeWithGracefulShutdown(ctx context.Context, handle *ListenerHandle, handler http.Handler, shutdownTimeout time.Duration) error {
	if shutdownTimeout <= 0 {
		shutdownTimeout = 15 * time.Second
	}
	defer handle.Cleanup()

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(handle.NetListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			_ = srv.Close()
			return fmt.Errorf("server: graceful shutdown timed out: %w", err)
		}
		return <-errCh
	}
}
