package backup

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/s3client"
	"github.com/Willh92/Volo-go/internal/security"
)

// SchedulerOptions configures the rolling local + remote (S3 / WebDAV) backup scheduler.
type SchedulerOptions struct {
	Store          *sqlx.Store
	Settings       *repo.SettingsManager
	BackupDir      string
	UploadDir      string
	OutboundDialer *security.OutboundSafeDialer
	Logger         io.Writer
}

// RunResult summarizes a single rolling backup execution cycle.
type RunResult struct {
	LocalPath      string
	PrunedCount    int
	RemoteUploaded bool
	RemoteTarget   string
}

// Scheduler executes periodic streaming local backups, enforces `LocalKeepCount` retention,
// and streams archives to S3 (SigV4) or WebDAV without buffering whole ZIPs in RAM.
type Scheduler struct {
	opts    SchedulerOptions
	mu      sync.Mutex
	lastRun time.Time

	stateMu sync.Mutex
	closed  bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewScheduler constructs a rolling backup scheduler.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	return &Scheduler{
		opts:   opts,
		stopCh: make(chan struct{}),
	}
}

// Start launches the background check loop until ctx is canceled or Close is called.
func (s *Scheduler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return
	}
	if s.stopCh == nil {
		s.stopCh = make(chan struct{})
	}
	stopCh := s.stopCh
	s.wg.Add(1)
	s.stateMu.Unlock()

	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				cfg := s.opts.Settings.Snapshot().Backup
				if !cfg.LocalAutoEnabled {
					continue
				}
				intervalHrs := cfg.LocalIntervalHrs
				if intervalHrs <= 0 {
					intervalHrs = 24
				}
				s.mu.Lock()
				due := s.lastRun.IsZero() || time.Since(s.lastRun) >= time.Duration(intervalHrs)*time.Hour
				s.mu.Unlock()
				if !due {
					continue
				}
				runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
				if _, err := s.runCycle(runCtx); err != nil && s.opts.Logger != nil {
					fmt.Fprintf(s.opts.Logger, "volo backup scheduler: cycle failed: %v\n", err)
				}
				cancel()
			}
		}
	}()
}

// Close stops the background scheduler loop and waits for any in-progress backup cycle to finish
// (or returns ctx.Err() if the shutdown deadline expires first).
func (s *Scheduler) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.stateMu.Lock()
	if !s.closed {
		s.closed = true
		if s.stopCh != nil {
			close(s.stopCh)
		}
	}
	s.stateMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunOnce creates a streaming local backup archive, prunes older `volo-auto-*.zip` files beyond
// `LocalKeepCount`, and streams the archive to S3 or WebDAV if `RemoteEnabled` is active.
func (s *Scheduler) RunOnce(ctx context.Context) (*RunResult, error) {
	if s == nil {
		return nil, errors.New("backup scheduler is nil")
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil, errors.New("backup scheduler is closed")
	}
	s.wg.Add(1)
	s.stateMu.Unlock()
	defer s.wg.Done()

	return s.runCycle(ctx)
}

func (s *Scheduler) runCycle(ctx context.Context) (*RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.opts.BackupDir, 0o750); err != nil {
		return nil, err
	}
	cfg := s.opts.Settings.Snapshot().Backup
	now := time.Now().UTC()
	filename := fmt.Sprintf("volo-auto-%s-%03d.zip", now.Format("20060102-150405"), now.Nanosecond()/1e6)
	localPath := filepath.Join(s.opts.BackupDir, filename)

	if _, err := CreateToFile(ctx, s.opts.Store, localPath, CreateOptions{
		IncludeMedia: cfg.IncludeMediaInAuto,
		UploadDir:    s.opts.UploadDir,
	}); err != nil {
		return nil, err
	}
	s.lastRun = now

	keep := cfg.LocalKeepCount
	if keep <= 0 {
		keep = 7
	}
	pruned, _ := pruneAutoBackups(s.opts.BackupDir, keep)

	res := &RunResult{
		LocalPath:   localPath,
		PrunedCount: pruned,
	}

	if cfg.RemoteEnabled {
		sec, err := s.opts.Settings.GetBackupSecrets(ctx)
		if err != nil {
			return res, err
		}
		target, err := s.uploadArchiveStream(ctx, cfg, sec, localPath, filename)
		if err != nil {
			return res, err
		}
		res.RemoteUploaded = true
		res.RemoteTarget = target
	}
	return res, nil
}

func (s *Scheduler) uploadArchiveStream(ctx context.Context, cfg domain.BackupSettings, sec domain.BackupSecrets, localZipPath string, filename string) (string, error) {
	f, err := os.Open(localZipPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}

	switch strings.ToLower(strings.TrimSpace(cfg.RemoteProtocol)) {
	case "s3":
		cli, err := s3client.NewClient(s3client.Config{
			Endpoint:  cfg.S3Endpoint,
			Region:    cfg.S3Region,
			Bucket:    cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey,
			SecretKey: sec.S3SecretKey,
			KeyPrefix: cfg.S3Prefix,
			PathStyle: cfg.S3PathStyle,
		}, s.opts.OutboundDialer)
		if err != nil {
			return "", err
		}
		return cli.PutStream(ctx, filename, f, st.Size(), "application/zip")

	case "webdav":
		return uploadWebDAVStream(ctx, cfg, sec.WebDAVPassword, f, st.Size(), filename, s.opts.OutboundDialer)

	default:
		return "", fmt.Errorf("unsupported remote backup protocol %q", cfg.RemoteProtocol)
	}
}

func uploadWebDAVStream(ctx context.Context, cfg domain.BackupSettings, password string, r io.ReadSeeker, size int64, filename string, safeDialer *security.OutboundSafeDialer) (string, error) {
	base := strings.TrimSpace(cfg.WebDAVURL)
	if base == "" {
		return "", errors.New("webdav_url is required")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("invalid webdav_url %q", base)
	}
	if safeDialer != nil {
		if _, err := safeDialer.ValidateOutboundURL(u.String()); err != nil {
			return "", err
		}
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if safeDialer != nil {
		tr.DialContext = safeDialer.DialContext
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("webdav: stopped after 3 redirects")
			}
			if safeDialer != nil {
				_, vErr := safeDialer.ValidateOutboundURL(req.URL.String())
				return vErr
			}
			return nil
		},
	}

	dirRel := strings.Trim(cfg.WebDAVDir, "/")
	if dirRel != "" {
		colURL := *u
		colURL.Path = path.Join(u.Path, dirRel) + "/"
		mkReq, mkErr := http.NewRequestWithContext(ctx, "MKCOL", colURL.String(), nil)
		if mkErr == nil {
			if cfg.WebDAVUser != "" {
				mkReq.SetBasicAuth(cfg.WebDAVUser, password)
			}
			if mkResp, dErr := client.Do(mkReq); dErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(mkResp.Body, 1024))
				_ = mkResp.Body.Close()
			}
		}
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	putURL := *u
	if dirRel != "" {
		putURL.Path = path.Join(u.Path, dirRel, filename)
	} else {
		putURL.Path = path.Join(u.Path, filename)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL.String(), r)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/zip")
	if cfg.WebDAVUser != "" {
		req.SetBasicAuth(cfg.WebDAVUser, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("webdav PUT status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return putURL.String(), nil
}

func pruneAutoBackups(backupDir string, keepCount int) (int, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return 0, err
	}
	var autos []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "volo-auto-") && strings.HasSuffix(name, ".zip") {
			autos = append(autos, name)
		}
	}
	if len(autos) <= keepCount {
		return 0, nil
	}
	// Lexicographical order matches chronological order (`volo-auto-YYYYMMDD-HHMMSS-mmm.zip`)
	sort.Strings(autos)
	toDelete := autos[:len(autos)-keepCount]
	pruned := 0
	for _, name := range toDelete {
		if err := os.Remove(filepath.Join(backupDir, name)); err == nil {
			pruned++
		}
	}
	return pruned, nil
}
