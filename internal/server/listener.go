package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
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

// OpenListener binds either VOLO_LISTEN_SOCKET (Unix Domain Socket with explicit chmod/chown,
// resolved parent dir + leaf symlink guards, live-process probe, and 0600 .sock.lock) or VOLO_LISTEN_ADDR (TCP).
func OpenListener(cfg *config.Config) (*ListenerHandle, error) {
	if cfg == nil {
		return nil, errors.New("server: nil config")
	}
	if cfg.ListenSocket != nil && strings.TrimSpace(cfg.ListenSocket.Path) != "" {
		return openUnixSocketListener(cfg.ListenSocket, cfg.SocketGroup, cfg.SocketMode)
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

func parseUnixSocketPerm(modeStr string, fallback os.FileMode) (os.FileMode, error) {
	trimmed := strings.TrimSpace(modeStr)
	if trimmed == "" {
		if fallback != 0 {
			if fallback > 0o777 {
				return 0, fmt.Errorf("server: invalid unix socket mode %04o (exceeds 0777)", fallback)
			}
			return fallback, nil
		}
		return config.DefaultUnixSocketPerm, nil
	}
	parsed, err := strconv.ParseUint(trimmed, 8, 32)
	if err != nil || parsed == 0 || parsed > 0o777 {
		return 0, fmt.Errorf("server: invalid unix socket octal mode %q (expected 0001..0777)", modeStr)
	}
	return os.FileMode(parsed), nil
}

func resolveSocketGroupGID(socketGroup string) (int, error) {
	grpStr := strings.TrimSpace(socketGroup)
	if grpStr == "" {
		return -1, nil
	}
	if gid, err := strconv.Atoi(grpStr); err == nil {
		if gid < 0 {
			return -1, fmt.Errorf("server: invalid negative unix socket group gid %q", socketGroup)
		}
		return gid, nil
	}
	grp, err := user.LookupGroup(grpStr)
	if err != nil {
		return -1, fmt.Errorf("server: lookup unix socket group %q: %w", grpStr, err)
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil || gid < 0 {
		return -1, fmt.Errorf("server: non-numeric unix socket group gid %q for group %q", grp.Gid, grpStr)
	}
	return gid, nil
}

func openUnixSocketListener(spec *config.ListenSocketSpec, socketGroup string, socketMode string) (*ListenerHandle, error) {
	cleanedPath := filepath.Clean(strings.TrimSpace(spec.Path))
	baseName := filepath.Base(cleanedPath)
	if cleanedPath == "" || baseName == "" || baseName == "." || baseName == ".." || baseName == string(filepath.Separator) {
		return nil, fmt.Errorf("server: invalid unix socket path %q", spec.Path)
	}

	perm, err := parseUnixSocketPerm(socketMode, spec.Perm)
	if err != nil {
		return nil, err
	}
	targetGID, err := resolveSocketGroupGID(socketGroup)
	if err != nil {
		return nil, err
	}

	// Reject leaf symlink immediately if cleanedPath already exists before parent creation.
	if preSt, preErr := os.Lstat(cleanedPath); preErr == nil && preSt.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("server: refusing symlink at socket path %q", cleanedPath)
	}

	parentDir := filepath.Dir(cleanedPath)
	if err := os.MkdirAll(parentDir, 0o750); err != nil {
		return nil, fmt.Errorf("server: create socket parent dir %q: %w", parentDir, err)
	}
	// Allow OS-standard symlinked parent directories (e.g. /var/run -> /run, macOS /tmp -> /private/tmp)
	// while keeping strict os.Lstat verification on leaf socket and lock files.
	parentReal, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		return nil, fmt.Errorf("server: resolve socket parent dir %q: %w", parentDir, err)
	}
	parentInfo, err := os.Lstat(parentReal)
	if err != nil {
		return nil, fmt.Errorf("server: stat resolved socket parent dir %q: %w", parentReal, err)
	}
	if !parentInfo.IsDir() {
		return nil, fmt.Errorf("server: socket parent %q is not a directory", parentReal)
	}

	realSockPath := filepath.Join(parentReal, baseName)
	sockPath := realSockPath
	if len(sockPath) > 100 && len(cleanedPath) <= 100 {
		sockPath = cleanedPath
	}

	// Check if leaf socket already exists: reject regular files/symlinks, and refuse clobbering an active socket!
	if st, err := os.Lstat(realSockPath); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("server: refusing symlink at socket path %q", cleanedPath)
		}
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("server: path %q exists and is not a unix socket", cleanedPath)
		}
		// Probe if another live process is actively listening on this Unix socket
		probeConn, dialErr := net.DialTimeout("unix", sockPath, 300*time.Millisecond)
		if dialErr == nil {
			_ = probeConn.Close()
			return nil, fmt.Errorf("server: unix socket %q is already in use by another running instance", cleanedPath)
		}
		if rmErr := os.Remove(realSockPath); rmErr != nil {
			return nil, fmt.Errorf("server: remove stale unix socket %q: %w", cleanedPath, rmErr)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("server: lstat socket %q: %w", cleanedPath, err)
	}

	lockPath := sockPath + ".lock"
	realLockPath := realSockPath + ".lock"
	if lInfo, lErr := os.Lstat(realLockPath); lErr == nil {
		if lInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("server: refusing symlinked socket lock file %q", lockPath)
		}
		if !lInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("server: socket lock path %q exists and is not a regular file", lockPath)
		}
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

	if targetGID >= 0 {
		if err := os.Chown(sockPath, -1, targetGID); err != nil {
			_ = ln.Close()
			_ = os.Remove(sockPath)
			_ = os.Remove(lockPath)
			return nil, fmt.Errorf("server: chown unix socket %q to group %q (gid %d): %w", sockPath, strings.TrimSpace(socketGroup), targetGID, err)
		}
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
