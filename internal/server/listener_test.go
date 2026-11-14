package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

func TestUnixSocketListenerPermissionsLockAndGracefulShutdown(t *testing.T) {
	tmp := t.TempDir()
	shortDir, err := os.MkdirTemp("", "vsock-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	sockPath := filepath.Join(shortDir, "volo.sock")

	// Overlong AF_UNIX paths (> sun_path limit) must fail cleanly before creating parent dirs or lockfiles.
	longParent := filepath.Join(shortDir, strings.Repeat("d", 110))
	longSock := filepath.Join(longParent, "volo.sock")
	if _, longErr := OpenListener(&config.Config{
		ListenSocket: &config.ListenSocketSpec{Path: longSock, Perm: 0o660},
	}); longErr == nil || !strings.Contains(longErr.Error(), "sun_path") {
		t.Fatalf("expected sun_path length error for %q, got: %v", longSock, longErr)
	}
	if _, stErr := os.Lstat(longParent); !os.IsNotExist(stErr) {
		t.Fatalf("expected overlong socket parent dir %q NOT to be created", longParent)
	}

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
	if runtime.GOOS != "windows" && sockInfo.Mode().Perm() != 0o660 {
		t.Fatalf("expected socket perm 0660, got %04o", sockInfo.Mode().Perm())
	}
	lockInfo, err := os.Lstat(sockPath + ".lock")
	if err != nil {
		t.Fatalf("lstat lock: %v", err)
	}
	if runtime.GOOS != "windows" && lockInfo.Mode().Perm() != 0o600 {
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

func TestUnixSocketSymlinkParentLeafRejectionStaleRecoveryAndModes(t *testing.T) {
	tmp, err := os.MkdirTemp("", "vsock-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })

	// 1. Symlinked parent directory (like /var/run -> /run or macOS /tmp -> /private/tmp) MUST succeed,
	//    and SocketMode "0600" / "660" + SocketGroup MUST be applied cleanly.
	realRunDir := filepath.Join(tmp, "run")
	if err := os.MkdirAll(realRunDir, 0o750); err != nil {
		t.Fatal(err)
	}
	symVarRun := filepath.Join(tmp, "var-run")
	if err := os.Symlink(realRunDir, symVarRun); err != nil {
		t.Skipf("symlinks unavailable in test environment: %v", err)
	}

	sockViaSymParent := filepath.Join(symVarRun, "volo", "volo.sock")
	gidStr := ""
	if gid := os.Getgid(); gid >= 0 {
		gidStr = fmtInt(gid)
	}
	cfgSym0600 := &config.Config{
		ListenSocket: &config.ListenSocketSpec{Path: sockViaSymParent},
		SocketMode:   "0600",
		SocketGroup:  gidStr,
	}
	h1, err := OpenListener(cfgSym0600)
	if err != nil {
		t.Fatalf("expected symlinked parent directory to be allowed, got: %v", err)
	}
	st1, err := os.Lstat(filepath.Join(realRunDir, "volo", "volo.sock"))
	if err != nil {
		_ = h1.Close()
		t.Fatalf("lstat socket in resolved parent: %v", err)
	}
	if runtime.GOOS != "windows" && st1.Mode().Perm() != 0o600 {
		_ = h1.Close()
		t.Fatalf("expected socket mode 0600, got %04o", st1.Mode().Perm())
	}
	_ = h1.Close()

	// Also verify 3-digit base-8 SocketMode "660" works and invalid group fails cleanly.
	cfgSym660 := &config.Config{
		ListenSocket: &config.ListenSocketSpec{Path: sockViaSymParent},
		SocketMode:   "660",
	}
	h660, err := OpenListener(cfgSym660)
	if err != nil {
		t.Fatalf("OpenListener with SocketMode=660 failed: %v", err)
	}
	st660, err := os.Lstat(filepath.Join(realRunDir, "volo", "volo.sock"))
	if err != nil {
		_ = h660.Close()
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st660.Mode().Perm() != 0o660 {
		_ = h660.Close()
		t.Fatalf("expected socket mode 0660 for '660', got %04o", st660.Mode().Perm())
	}
	_ = h660.Close()

	cfgBadGrp := &config.Config{
		ListenSocket: &config.ListenSocketSpec{Path: sockViaSymParent},
		SocketMode:   "0660",
		SocketGroup:  "nonexistent_volo_group_999999",
	}
	if _, err := OpenListener(cfgBadGrp); err == nil {
		t.Fatal("expected nonexistent SocketGroup to return an error")
	}

	// 2. Leaf symlink at socketPath MUST be rejected without modifying the symlink target!
	victimFile := filepath.Join(tmp, "victim.txt")
	if err := os.WriteFile(victimFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	leafSymSock := filepath.Join(symVarRun, "volo", "evil.sock")
	if err := os.Symlink(victimFile, leafSymSock); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenListener(&config.Config{
		ListenSocket: &config.ListenSocketSpec{Path: leafSymSock},
		SocketMode:   "0660",
	}); err == nil {
		t.Fatal("expected leaf symlink at socketPath to be rejected")
	}
	if data, err := os.ReadFile(victimFile); err != nil || string(data) != "keep" {
		t.Fatalf("victim target was unexpectedly altered: err=%v data=%q", err, string(data))
	}

	// 3. Leaf symlink at socketPath+".lock" MUST also be rejected!
	lockTestSock := filepath.Join(realRunDir, "lockcheck.sock")
	if err := os.Symlink(victimFile, lockTestSock+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenListener(&config.Config{
		ListenSocket: &config.ListenSocketSpec{Path: lockTestSock},
		SocketMode:   "0660",
	}); err == nil {
		t.Fatal("expected leaf symlink at .sock.lock to be rejected")
	}
	if data, err := os.ReadFile(victimFile); err != nil || string(data) != "keep" {
		t.Fatalf("victim lock target was unexpectedly altered: err=%v data=%q", err, string(data))
	}

	// 4. Stale socket recovery + live socket lock protection:
	staleSock := filepath.Join(realRunDir, "stale.sock")
	rawUnixLn, err := net.ListenUnix("unix", &net.UnixAddr{Name: staleSock, Net: "unix"})
	if err != nil {
		t.Fatalf("create raw unix socket: %v", err)
	}
	rawUnixLn.SetUnlinkOnClose(false)
	_ = rawUnixLn.Close()
	if st, err := os.Lstat(staleSock); err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected stale socket file to remain on disk before recovery, err=%v", err)
	}

	cfgStale := &config.Config{
		ListenSocket: &config.ListenSocketSpec{Path: staleSock},
		SocketMode:   "0660",
	}
	recoveredHandle, err := OpenListener(cfgStale)
	if err != nil {
		t.Fatalf("expected stale unix socket to be recovered cleanly, got: %v", err)
	}
	defer recoveredHandle.Close()

	// While recoveredHandle is live, a second OpenListener MUST fail and leave recoveredHandle intact!
	if _, dupErr := OpenListener(cfgStale); dupErr == nil {
		t.Fatal("expected live socket protection to block duplicate bind")
	}
	conn, dialErr := net.DialTimeout("unix", staleSock, time.Second)
	if dialErr != nil {
		t.Fatalf("expected live socket to remain connectable after blocked duplicate bind: %v", dialErr)
	}
	_ = conn.Close()
}

func TestEmergencyFallbackErrorHTMLDarkModeCSS(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEmergencyFallbackErrorHTML(rec, http.StatusServiceUnavailable, "Service <busy>")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	body := rec.Body.String()
	wantDarkCSS := "@media(prefers-color-scheme:dark){body{background:#121316;color:#e5e7eb}p{color:#9ca3af}}"
	if !strings.Contains(body, wantDarkCSS) {
		t.Fatalf("expected emergency fallback HTML to contain dark-mode CSS %q, got: %s", wantDarkCSS, body)
	}
	if !strings.Contains(body, "Service &lt;busy&gt;") {
		t.Fatalf("expected HTML-escaped message in emergency fallback HTML, got: %s", body)
	}
}

func fmtInt(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
