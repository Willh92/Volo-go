package backup

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
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
	"github.com/Willh92/Volo-go/pkg/plugin"
)

// SchedulerOptions configures the rolling local + remote (S3 / WebDAV / Plugin) backup scheduler.
type SchedulerOptions struct {
	Store          *sqlx.Store
	Settings       *repo.SettingsManager
	Plugins        *plugin.Manager
	BackupDir      string
	UploadDir      string
	OutboundDialer *security.OutboundSafeDialer
	Logger         io.Writer
}

// RunResult summarizes a single rolling backup execution cycle.
type RunResult struct {
	LocalPath         string
	PrunedCount       int
	RemoteUploaded    bool
	RemoteTarget      string
	RemotePrunedCount int
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
		rPruned, pErr := s.pruneRemoteAutoBackups(ctx, cfg, sec, keep)
		res.RemotePrunedCount = rPruned
		if pErr != nil && s.opts.Logger != nil {
			fmt.Fprintf(s.opts.Logger, "volo backup scheduler: remote prune warning: %v\n", pErr)
		}
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

	proto := strings.ToLower(strings.TrimSpace(cfg.RemoteProtocol))
	switch proto {
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
		sp, spID, ok := s.resolveBackupStoragePlugin(proto)
		if !ok {
			return "", fmt.Errorf("unsupported remote backup protocol %q", cfg.RemoteProtocol)
		}
		urlOrKey, err := sp.Put(ctx, "backups/"+filename, f, st.Size(), "application/zip")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(urlOrKey) != "" {
			return urlOrKey, nil
		}
		return "plugin:" + spID + "/backups/" + filename, nil
	}
}

func (s *Scheduler) resolveBackupStoragePlugin(proto string) (plugin.StorageProvider, string, bool) {
	if s.opts.Plugins == nil {
		return nil, "", false
	}
	reg := s.opts.Plugins.Providers()
	if reg == nil {
		return nil, "", false
	}
	if strings.HasPrefix(proto, "plugin:") {
		id := strings.TrimSpace(strings.TrimPrefix(proto, "plugin:"))
		if sp, ok := reg.StorageByID(id); ok {
			return sp, id, true
		}
		return nil, "", false
	}
	if proto == "plugin" {
		if s.opts.Settings != nil {
			mediaDriver := strings.TrimSpace(s.opts.Settings.Snapshot().Storage.ActiveDriver)
			if mediaDriver != "" && mediaDriver != "local" && mediaDriver != "s3" {
				if sp, ok := reg.StorageByID(mediaDriver); ok {
					return sp, mediaDriver, true
				}
			}
		}
		ids := reg.StorageProviderIDs()
		if len(ids) > 0 {
			if sp, ok := reg.StorageByID(ids[0]); ok {
				return sp, ids[0], true
			}
		}
		return nil, "", false
	}
	if proto != "" {
		if sp, ok := reg.StorageByID(proto); ok {
			return sp, proto, true
		}
	}
	return nil, "", false
}

func newWebDAVClient(rawBase string, safeDialer *security.OutboundSafeDialer, timeout time.Duration) (*url.URL, *http.Client, error) {
	base := strings.TrimSpace(rawBase)
	if base == "" {
		return nil, nil, errors.New("webdav_url is required")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, nil, fmt.Errorf("invalid webdav_url %q", base)
	}
	if safeDialer != nil {
		if _, err := safeDialer.ValidateOutboundURL(u.String()); err != nil {
			return nil, nil, err
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
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("webdav: stopped after 3 redirects")
			}
			if len(via) > 0 && strings.EqualFold(via[len(via)-1].URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
				return errors.New("webdav: refusing insecure https to http scheme downgrade redirect")
			}
			if safeDialer != nil {
				_, vErr := safeDialer.ValidateOutboundURL(req.URL.String())
				return vErr
			}
			return nil
		},
	}
	return u, client, nil
}

func uploadWebDAVStream(ctx context.Context, cfg domain.BackupSettings, password string, r io.ReadSeeker, size int64, filename string, safeDialer *security.OutboundSafeDialer) (string, error) {
	u, client, err := newWebDAVClient(cfg.WebDAVURL, safeDialer, 10*time.Minute)
	if err != nil {
		return "", err
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

func isAutoBackupZipName(name string) bool {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	return (strings.HasPrefix(name, "volo-auto-") || strings.HasPrefix(name, "volo-auto-backup-")) && strings.HasSuffix(name, ".zip")
}

func (s *Scheduler) pruneRemoteAutoBackups(ctx context.Context, cfg domain.BackupSettings, sec domain.BackupSecrets, keepCount int) (int, error) {
	if keepCount <= 0 {
		keepCount = 7
	}
	proto := strings.ToLower(strings.TrimSpace(cfg.RemoteProtocol))
	switch proto {
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
			return 0, err
		}
		objs, err := cli.ListObjects(ctx, "volo-auto-")
		if err != nil {
			return 0, err
		}
		var autos []s3client.ObjectInfo
		for _, obj := range objs {
			if isAutoBackupZipName(obj.Key) {
				autos = append(autos, obj)
			}
		}
		if len(autos) <= keepCount {
			return 0, nil
		}
		sort.Slice(autos, func(i, j int) bool {
			if !autos[i].LastModified.Equal(autos[j].LastModified) && !autos[i].LastModified.IsZero() && !autos[j].LastModified.IsZero() {
				return autos[i].LastModified.After(autos[j].LastModified)
			}
			return autos[i].Key > autos[j].Key
		})
		pruned := 0
		var firstErr error
		for _, old := range autos[keepCount:] {
			if dErr := cli.DeleteObject(ctx, old.Key); dErr != nil {
				firstErr = errors.Join(firstErr, dErr)
			} else {
				pruned++
			}
		}
		return pruned, firstErr

	case "webdav":
		return pruneWebDAVAutoBackups(ctx, cfg, sec.WebDAVPassword, keepCount, s.opts.OutboundDialer)

	default:
		return 0, nil
	}
}

func pruneWebDAVAutoBackups(ctx context.Context, cfg domain.BackupSettings, password string, keepCount int, safeDialer *security.OutboundSafeDialer) (int, error) {
	u, client, err := newWebDAVClient(cfg.WebDAVURL, safeDialer, 60*time.Second)
	if err != nil {
		return 0, err
	}
	dirRel := strings.Trim(cfg.WebDAVDir, "/")
	colURL := *u
	if dirRel != "" {
		colURL.Path = strings.TrimRight(path.Join(u.Path, dirRel), "/") + "/"
	} else {
		colURL.Path = strings.TrimRight(u.Path, "/") + "/"
	}

	const propfindXML = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:getlastmodified/></d:prop></d:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", colURL.String(), bytes.NewReader([]byte(propfindXML)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	if cfg.WebDAVUser != "" {
		req.SetBasicAuth(cfg.WebDAVUser, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	_ = resp.Body.Close()
	if readErr != nil {
		return 0, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("webdav PROPFIND status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	type davProp struct {
		LastModified string `xml:"getlastmodified"`
	}
	type davPropStat struct {
		Prop davProp `xml:"prop"`
	}
	type davResponse struct {
		Href      string        `xml:"href"`
		PropStats []davPropStat `xml:"propstat"`
		Direct    davProp       `xml:"prop"`
	}
	type davMultistatus struct {
		Responses []davResponse `xml:"response"`
	}

	var ms davMultistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return 0, fmt.Errorf("webdav PROPFIND parse XML: %w", err)
	}

	type davBackupEntry struct {
		name    string
		modTime time.Time
	}
	colClean := path.Clean(strings.TrimRight(colURL.Path, "/"))
	if colClean == "." {
		colClean = "/"
	}
	seen := make(map[string]bool)
	var autos []davBackupEntry
	for _, r := range ms.Responses {
		rawHref := strings.TrimSpace(r.Href)
		if rawHref == "" {
			continue
		}
		hrefPath := rawHref
		if pu, pErr := url.Parse(rawHref); pErr == nil && pu.Path != "" {
			hrefPath = pu.Path
		}
		if strings.HasSuffix(hrefPath, "/") {
			continue
		}
		var base string
		if strings.HasPrefix(hrefPath, "/") {
			parentClean := path.Clean(path.Dir(hrefPath))
			if parentClean != colClean && !strings.HasSuffix(parentClean, colClean) {
				continue
			}
			base = path.Base(hrefPath)
		} else {
			if strings.Contains(hrefPath, "/") {
				continue
			}
			base = hrefPath
		}
		if !isAutoBackupZipName(base) || seen[base] {
			continue
		}
		seen[base] = true

		rawMod := strings.TrimSpace(r.Direct.LastModified)
		for _, ps := range r.PropStats {
			if m := strings.TrimSpace(ps.Prop.LastModified); m != "" {
				rawMod = m
				break
			}
		}
		var modTime time.Time
		if rawMod != "" {
			if t, tErr := http.ParseTime(rawMod); tErr == nil {
				modTime = t.UTC()
			} else if t, tErr := time.Parse(time.RFC1123Z, rawMod); tErr == nil {
				modTime = t.UTC()
			} else if t, tErr := time.Parse(time.RFC3339, rawMod); tErr == nil {
				modTime = t.UTC()
			}
		}
		autos = append(autos, davBackupEntry{name: base, modTime: modTime})
	}

	if len(autos) <= keepCount {
		return 0, nil
	}
	sort.Slice(autos, func(i, j int) bool {
		if !autos[i].modTime.Equal(autos[j].modTime) && !autos[i].modTime.IsZero() && !autos[j].modTime.IsZero() {
			return autos[i].modTime.After(autos[j].modTime)
		}
		return autos[i].name > autos[j].name
	})

	pruned := 0
	var firstErr error
	for _, old := range autos[keepCount:] {
		delURL := *u
		delURL.Path = path.Join(colURL.Path, old.name)
		delReq, reqErr := http.NewRequestWithContext(ctx, http.MethodDelete, delURL.String(), nil)
		if reqErr != nil {
			firstErr = errors.Join(firstErr, reqErr)
			continue
		}
		if cfg.WebDAVUser != "" {
			delReq.SetBasicAuth(cfg.WebDAVUser, password)
		}
		delResp, doErr := client.Do(delReq)
		if doErr != nil {
			firstErr = errors.Join(firstErr, doErr)
			continue
		}
		delMsg, _ := io.ReadAll(io.LimitReader(delResp.Body, 512))
		_ = delResp.Body.Close()
		if delResp.StatusCode == http.StatusOK || delResp.StatusCode == http.StatusAccepted || delResp.StatusCode == http.StatusNoContent || delResp.StatusCode == http.StatusNotFound {
			pruned++
		} else {
			firstErr = errors.Join(firstErr, fmt.Errorf("webdav DELETE %q status %d: %s", old.name, delResp.StatusCode, strings.TrimSpace(string(delMsg))))
		}
	}
	return pruned, firstErr
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
		if isAutoBackupZipName(name) {
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
