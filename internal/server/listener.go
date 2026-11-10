package server

import (
	"context"
	"crypto/tls"
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
	// TLSConfig is non-nil when native HTTPS is enabled (VOLO_TLS_CERT_FILE/VOLO_TLS_KEY_FILE);
	// NetListener then stays a raw TCP listener and ServeWithGracefulShutdown serves TLS (h2 + http/1.1).
	TLSConfig   *tls.Config
	tlsReloader *certReloader
	// Optional plain-HTTP -> HTTPS 308 redirect listener (VOLO_TLS_HTTP_REDIRECT_ADDR).
	RedirectListener net.Listener
	redirectHandler  http.Handler
	cleanupOnce      sync.Once
	cleanupFn        func()
}

func (h *ListenerHandle) Close() error {
	err := h.NetListener.Close()
	if h.RedirectListener != nil {
		_ = h.RedirectListener.Close()
	}
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
// When native TLS is configured, the certificate/key pair is validated up front (fail fast) and the
// optional HTTP->HTTPS redirect listener is bound as well.
func OpenListener(cfg *config.Config) (*ListenerHandle, error) {
	if cfg == nil {
		return nil, errors.New("server: nil config")
	}
	if cfg.ListenSocket != nil && strings.TrimSpace(cfg.ListenSocket.Path) != "" {
		if cfg.TLSEnabled() {
			return nil, errors.New("server: native TLS cannot be combined with a Unix socket listener")
		}
		return openUnixSocketListener(cfg.ListenSocket, cfg.SocketGroup, cfg.SocketMode)
	}
	var (
		tlsCfg   *tls.Config
		reloader *certReloader
	)
	if cfg.TLSEnabled() {
		var err error
		if tlsCfg, reloader, err = buildTLSConfig(cfg); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("server: listen tcp %s: %w", cfg.ListenAddr, err)
	}
	handle := &ListenerHandle{
		NetListener: ln,
		Network:     "tcp",
		Address:     ln.Addr().String(),
		TLSConfig:   tlsCfg,
		tlsReloader: reloader,
	}
	if tlsCfg != nil {
		redirectLn, redirectHandler, err := openTLSRedirectListener(cfg, ln.Addr())
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
		handle.RedirectListener = redirectLn
		handle.redirectHandler = redirectHandler
	}
	return handle, nil
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
// When handle.TLSConfig is set it serves HTTPS (HTTP/2 via ALPN "h2", HTTP/1.1 fallback), and the
// optional HTTP->HTTPS redirect server is drained together with the main server.
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
	if handle.TLSConfig != nil {
		srv.TLSConfig = handle.TLSConfig
	}

	var redirectSrv *http.Server
	redirectErrCh := make(chan error, 1)
	if handle.RedirectListener != nil && handle.redirectHandler != nil {
		redirectSrv = newRedirectServer(handle.redirectHandler)
		go func() {
			if err := redirectSrv.Serve(handle.RedirectListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				redirectErrCh <- fmt.Errorf("server: https redirect listener: %w", err)
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if srv.TLSConfig != nil {
			// Empty cert/key paths: certificates come from TLSConfig.GetCertificate (hot reload).
			// ServeTLS also wires up the bundled HTTP/2 server for "h2" ALPN negotiation.
			err = srv.ServeTLS(handle.NetListener, "", "")
		} else {
			err = srv.Serve(handle.NetListener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	shutdownRedirect := func(shutCtx context.Context) {
		if redirectSrv == nil {
			return
		}
		if err := redirectSrv.Shutdown(shutCtx); err != nil {
			_ = redirectSrv.Close()
		}
	}

	select {
	case err := <-errCh:
		closeCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		shutdownRedirect(closeCtx)
		return err
	case err := <-redirectErrCh:
		_ = srv.Close()
		<-errCh
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		redirectDone := make(chan struct{})
		go func() {
			shutdownRedirect(shutCtx)
			close(redirectDone)
		}()
		if err := srv.Shutdown(shutCtx); err != nil {
			_ = srv.Close()
			<-redirectDone
			return fmt.Errorf("server: graceful shutdown timed out: %w", err)
		}
		<-redirectDone
		return <-errCh
	}
}
