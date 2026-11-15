package theme

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/extpkg"
	"github.com/Willh92/Volo-go/internal/security"
)

const (
	themeUpdateCacheTTL       = 30 * time.Minute
	themeUpdateErrorRetryTTL  = 60 * time.Second
	themeUpdateLeaderTimeout  = 15 * time.Second
	themeDownloadMaxDuration  = 2 * time.Minute
	themeUpdateMaxConcurrency = 4

	defaultOfficialThemesCatalogURL = "https://raw.githubusercontent.com/Willh92/Volo-go/main/themes/catalog.json"
)

type cachedThemeUpdate struct {
	status    extpkg.UpdateStatus
	expiresAt time.Time
}

type themeUpdateCall struct {
	done   chan struct{}
	status extpkg.UpdateStatus
}

// SetHostVersion sets or updates the running host engine SemVer used for MinHostVersion compatibility gates.
func (e *Engine) SetHostVersion(ver string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.hostVersion = strings.TrimSpace(ver)
	e.mu.Unlock()
}

// SetOutboundDialer configures the SSRF-hardened dialer used by theme update checks and URL installations.
func (e *Engine) SetOutboundDialer(d *security.OutboundSafeDialer) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.outboundDialer = d
	e.mu.Unlock()
}

// SetAllowLoopbackForTests permits loopback HTTP test servers during unit tests.
func (e *Engine) SetAllowLoopbackForTests(allow bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.allowLoopbackForTests = allow
	e.mu.Unlock()
}

// SetOfficialCatalogURL overrides the official themes catalog URL used when legacy v1.0.2 official themes omit theme.json update config.
func (e *Engine) SetOfficialCatalogURL(raw string) {
	if e == nil {
		return
	}
	trimmed := strings.TrimSpace(raw)
	e.officialCatalogURL.Store(&trimmed)
	e.clearAllUpdateStatus()
}

func (e *Engine) effectiveOfficialCatalogURL() string {
	if e != nil {
		if ptr := e.officialCatalogURL.Load(); ptr != nil && *ptr != "" {
			return *ptr
		}
	}
	return defaultOfficialThemesCatalogURL
}

func (e *Engine) clearAllUpdateStatus() {
	if e == nil {
		return
	}
	e.updateMu.Lock()
	clear(e.updateCache)
	e.updateMu.Unlock()
}

func (e *Engine) effectiveHostVersion() string {
	if e == nil {
		return ""
	}
	e.mu.RLock()
	v := e.hostVersion
	e.mu.RUnlock()
	return v
}

func (e *Engine) ensureMetaFetcher() *extpkg.MetadataFetcher {
	e.mu.RLock()
	f := e.metaFetcher
	e.mu.RUnlock()
	if f != nil {
		return f
	}
	e.mu.Lock()
	if e.metaFetcher == nil {
		e.metaFetcher = extpkg.NewMetadataFetcher()
	}
	f = e.metaFetcher
	e.mu.Unlock()
	return f
}

func (e *Engine) effectiveOutboundDialer() *security.OutboundSafeDialer {
	if e == nil {
		return &security.OutboundSafeDialer{Timeout: 10 * time.Second}
	}
	e.mu.RLock()
	base := e.outboundDialer
	allowLoop := e.allowLoopbackForTests
	e.mu.RUnlock()
	if base != nil {
		return &security.OutboundSafeDialer{
			AllowedHosts:          nil,
			Timeout:               base.Timeout,
			Resolver:              base.Resolver,
			AllowLoopbackForTests: base.AllowLoopbackForTests || allowLoop,
		}
	}
	return &security.OutboundSafeDialer{
		Timeout:               10 * time.Second,
		AllowLoopbackForTests: allowLoop,
	}
}

func isKnownOfficialCatalogTheme(mf Manifest) bool {
	if mf.IsBuiltin || mf.ID != "volo-journal" {
		return false
	}
	author := strings.TrimSpace(mf.Author)
	return author == "" || strings.EqualFold(author, "Volo-go") || strings.EqualFold(author, "Volo Studio")
}

func (e *Engine) effectiveThemeUpdateConfig(mf Manifest) *extpkg.UpdateConfig {
	if mf.IsBuiltin {
		return mf.Update
	}
	if mf.Update != nil {
		return mf.Update
	}
	if isKnownOfficialCatalogTheme(mf) {
		return &extpkg.UpdateConfig{
			Strategy: extpkg.StrategyVoloCatalog,
			URL:      e.effectiveOfficialCatalogURL(),
		}
	}
	return nil
}

func (e *Engine) themeUpdateCapability(mf Manifest) string {
	upd := e.effectiveThemeUpdateConfig(mf)
	if upd == nil || upd.Strategy == "" || upd.Strategy == extpkg.StrategyNone {
		return extpkg.CapabilityNone
	}
	if mf.IsBuiltin || !upd.OnlineAllowed() {
		return extpkg.CapabilityCheckOnly
	}
	return extpkg.CapabilityOnline
}

// recoverInterruptedThemeSwaps runs once on Engine startup to clean orphaned temporary artifacts
// and self-heal any interrupted swap where `.theme-old-<id>` survived without `<id>`.
func (e *Engine) recoverInterruptedThemeSwaps() {
	if e == nil || strings.TrimSpace(e.themesDir) == "" {
		return
	}
	entries, err := os.ReadDir(e.themesDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		name := ent.Name()
		fullPath := filepath.Join(e.themesDir, name)
		if strings.HasPrefix(name, ".theme-stage-") || strings.HasPrefix(name, ".volo-theme-dl-") {
			_ = os.RemoveAll(fullPath)
			continue
		}
		if !ent.IsDir() || !strings.HasPrefix(name, ".theme-old-") {
			continue
		}
		tid := strings.TrimPrefix(name, ".theme-old-")
		if !validThemeIDRe.MatchString(tid) || tid == DefaultThemeID {
			_ = os.RemoveAll(fullPath)
			continue
		}
		targetDir := filepath.Join(e.themesDir, tid)
		if _, statErr := os.Stat(targetDir); os.IsNotExist(statErr) {
			if rErr := os.Rename(fullPath, targetDir); rErr == nil {
				slog.Warn("theme: recovered interrupted upgrade directory from backup", "theme_id", tid)
			}
		} else if statErr == nil {
			_ = os.RemoveAll(fullPath)
		}
	}
}

func (e *Engine) invalidateUpdateStatus(themeID string) {
	if e == nil {
		return
	}
	e.updateMu.Lock()
	delete(e.updateCache, themeID)
	e.updateMu.Unlock()
}

func (e *Engine) getCachedUpdateStatus(themeID string, currentVersion string) *extpkg.UpdateStatus {
	if e == nil {
		return nil
	}
	e.updateMu.RLock()
	entry, ok := e.updateCache[themeID]
	e.updateMu.RUnlock()
	if !ok || entry.status.CurrentVersion != currentVersion {
		return nil
	}
	cp := entry.status
	return &cp
}

func (e *Engine) getManifest(themeID string) (Manifest, bool) {
	if e == nil {
		return Manifest{}, false
	}
	e.mu.RLock()
	th, ok := e.themes[strings.TrimSpace(themeID)]
	var mf Manifest
	if ok && th != nil {
		mf = redactManifestSecrets(th.manifest)
	}
	e.mu.RUnlock()
	return mf, ok && th != nil
}

// GetLocalized returns a single locale-resolved theme view (including update_capability and cached update_status).
func (e *Engine) GetLocalized(themeID string, locale string) (LocalizedTheme, bool) {
	target := strings.TrimSpace(themeID)
	for _, item := range e.ListLocalized(locale) {
		if item.ID == target {
			return item, true
		}
	}
	return LocalizedTheme{}, false
}

// CheckThemeUpdate checks update availability for a single loaded theme.
// Themes without an `"update"` block (including built-in `volo-studio`) return StateUnsupported immediately with zero network traffic.
func (e *Engine) CheckThemeUpdate(ctx context.Context, themeID string, force bool, locale string) (extpkg.UpdateStatus, error) {
	if e == nil {
		return extpkg.UpdateStatus{}, errors.New("theme: engine unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tid := strings.TrimSpace(themeID)
	mf, ok := e.getManifest(tid)
	if !ok {
		return extpkg.UpdateStatus{}, fmt.Errorf("theme %q not found", tid)
	}

	capab := e.themeUpdateCapability(mf)
	if capab == extpkg.CapabilityNone {
		return extpkg.UpdateStatus{
			State:              extpkg.StateUnsupported,
			CurrentVersion:     mf.Version,
			CompatibleWithHost: true,
		}, nil
	}
	if err := ctx.Err(); err != nil {
		return extpkg.UpdateStatus{}, err
	}

	now := time.Now()
	e.updateMu.Lock()
	if e.updateCache == nil {
		e.updateCache = make(map[string]cachedThemeUpdate)
	}
	if e.updateInflight == nil {
		e.updateInflight = make(map[string]*themeUpdateCall)
	}
	if !force {
		if cached, has := e.updateCache[mf.ID]; has && cached.status.CurrentVersion == mf.Version && now.Before(cached.expiresAt) {
			st := cached.status
			e.updateMu.Unlock()
			return st, nil
		}
	}
	if call, active := e.updateInflight[mf.ID]; active {
		e.updateMu.Unlock()
		select {
		case <-ctx.Done():
			return extpkg.UpdateStatus{}, ctx.Err()
		case <-call.done:
			return call.status, nil
		}
	}
	call := &themeUpdateCall{done: make(chan struct{})}
	e.updateInflight[mf.ID] = call
	e.updateMu.Unlock()

	leaderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), themeUpdateLeaderTimeout)
	go func() {
		defer cancel()
		status := e.runSingleThemeUpdateCheck(leaderCtx, mf, locale)
		ttl := themeUpdateCacheTTL
		if status.State == extpkg.StateError {
			ttl = themeUpdateErrorRetryTTL
		}
		_, stillLoaded := e.getManifest(mf.ID)
		skipErrorCache := status.State == extpkg.StateError && (leaderCtx.Err() != nil || ctx.Err() != nil)

		e.updateMu.Lock()
		if stillLoaded && !skipErrorCache {
			e.updateCache[mf.ID] = cachedThemeUpdate{
				status:    status,
				expiresAt: time.Now().Add(ttl),
			}
		}
		call.status = status
		delete(e.updateInflight, mf.ID)
		close(call.done)
		e.updateMu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return extpkg.UpdateStatus{}, ctx.Err()
	case <-call.done:
		return call.status, nil
	}
}

func (e *Engine) runSingleThemeUpdateCheck(ctx context.Context, mf Manifest, locale string) extpkg.UpdateStatus {
	nowMs := time.Now().UnixMilli()
	hostVer := e.effectiveHostVersion()
	dialer := e.effectiveOutboundDialer()
	capab := e.themeUpdateCapability(mf)
	effUpd := e.effectiveThemeUpdateConfig(mf)

	if effUpd == nil || effUpd.Strategy == "" || effUpd.Strategy == extpkg.StrategyNone {
		return extpkg.UpdateStatus{
			State:              extpkg.StateUnsupported,
			CurrentVersion:     mf.Version,
			CompatibleWithHost: true,
		}
	}
	fetcher := e.ensureMetaFetcher()

	rawRes, err := fetcher.CheckDeclarative(ctx, dialer, mf.ID, *effUpd, locale)
	if err != nil {
		return extpkg.UpdateStatus{
			State:              extpkg.StateError,
			CurrentVersion:     mf.Version,
			CompatibleWithHost: true,
			CheckedAt:          nowMs,
			Error:              err.Error(),
		}
	}
	norm, nErr := extpkg.NormalizeUpdateStatus(
		mf.Version,
		hostVer,
		capab,
		mf.Homepage,
		dialer.AllowLoopbackForTests,
		rawRes,
		nowMs,
	)
	if nErr != nil {
		return extpkg.UpdateStatus{
			State:              extpkg.StateError,
			CurrentVersion:     mf.Version,
			CompatibleWithHost: true,
			CheckedAt:          nowMs,
			Error:              nErr.Error(),
		}
	}
	return norm
}

// CheckUpdates checks updates across the requested theme IDs (or all loaded themes when ids is empty)
// with bounded concurrency (max 4 workers) and silent skipping of unsupported themes.
func (e *Engine) CheckUpdates(ctx context.Context, ids []string, force bool, locale string) (extpkg.BatchCheckSummary, error) {
	out := extpkg.BatchCheckSummary{
		Statuses: make(map[string]extpkg.UpdateStatus),
	}
	if e == nil {
		return out, nil
	}

	var targets []Manifest
	if len(ids) > 0 {
		seen := make(map[string]bool, len(ids))
		for _, rawID := range ids {
			tid := strings.TrimSpace(rawID)
			if tid == "" || seen[tid] {
				continue
			}
			seen[tid] = true
			if mf, ok := e.getManifest(tid); ok {
				targets = append(targets, mf)
			}
		}
	} else {
		targets = e.List()
		sort.Slice(targets, func(i, j int) bool {
			return targets[i].ID < targets[j].ID
		})
	}

	var mu sync.Mutex
	sem := make(chan struct{}, themeUpdateMaxConcurrency)
	var wg sync.WaitGroup

	for _, mf := range targets {
		capab := e.themeUpdateCapability(mf)
		if capab == extpkg.CapabilityNone {
			mu.Lock()
			out.Unsupported++
			out.Statuses[mf.ID] = extpkg.UpdateStatus{
				State:              extpkg.StateUnsupported,
				CurrentVersion:     mf.Version,
				CompatibleWithHost: true,
			}
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(targetID string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			st, err := e.CheckThemeUpdate(ctx, targetID, force, locale)
			if err != nil {
				return
			}
			mu.Lock()
			out.Statuses[targetID] = st
			switch st.State {
			case extpkg.StateUnsupported:
				out.Unsupported++
			case extpkg.StateError:
				out.Checked++
				out.Errors++
			case extpkg.StateUpdateAvailable:
				out.Checked++
				out.Updates++
			default:
				out.Checked++
			}
			mu.Unlock()
		}(mf.ID)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// InstallFromURL streams a remote theme `.zip` archive directly to disk under SSRF + stall-watchdog protection,
// verifies optional SHA-256, validates templates & host compatibility, and installs it transactionally.
func (e *Engine) InstallFromURL(ctx context.Context, rawURL string, expectedSHA256 string, overwrite bool) (Manifest, error) {
	return e.installFromURLInternal(ctx, rawURL, expectedSHA256, installThemeZipOptions{
		overwrite: overwrite,
	})
}

func (e *Engine) installFromURLInternal(ctx context.Context, rawURL string, expectedSHA256 string, opts installThemeZipOptions) (Manifest, error) {
	if e == nil || strings.TrimSpace(e.themesDir) == "" {
		return Manifest{}, errors.New("theme: external themes directory is not configured")
	}
	dl, err := extpkg.StreamVerifyZip(ctx, e.effectiveOutboundDialer(), extpkg.DownloadOptions{
		URL:            rawURL,
		ExpectedSHA256: expectedSHA256,
		TempDir:        e.themesDir,
		TempPrefix:     ".volo-theme-dl-",
		MaxBytes:       maxThemeTotalBytes,
		MaxDuration:    themeDownloadMaxDuration,
	})
	if err != nil {
		return Manifest{}, err
	}
	defer dl.Cleanup()

	return e.installZipInternal(ctx, dl.File, dl.Size, opts)
}

// UpgradeTheme performs a verified 1-click online upgrade for an already-installed external theme,
// enforcing update capability, host compatibility, SHA-256 verification, theme ID pinning, and atomic rollback.
func (e *Engine) UpgradeTheme(ctx context.Context, themeID string, locale string) (Manifest, error) {
	tid := strings.TrimSpace(themeID)
	mf, ok := e.getManifest(tid)
	if !ok {
		return Manifest{}, fmt.Errorf("theme %q not found", tid)
	}
	capab := e.themeUpdateCapability(mf)
	if mf.IsBuiltin || capab != extpkg.CapabilityOnline {
		return Manifest{}, fmt.Errorf("theme %q does not support online updates", tid)
	}

	var st extpkg.UpdateStatus
	if cached := e.getCachedUpdateStatus(tid, mf.Version); cached != nil && cached.State != extpkg.StateUnchecked {
		st = *cached
	} else {
		var err error
		st, err = e.CheckThemeUpdate(ctx, tid, false, locale)
		if err != nil {
			return Manifest{}, err
		}
	}

	if st.State == extpkg.StateError {
		return Manifest{}, fmt.Errorf("theme %q update check failed: %s", tid, st.Error)
	}
	if !st.HasUpdate {
		return Manifest{}, fmt.Errorf("theme %q is already up to date (v%s)", tid, mf.Version)
	}
	if !st.OnlineUpdatable || strings.TrimSpace(st.DownloadURL) == "" {
		return Manifest{}, fmt.Errorf("theme %q v%s requires manual update (no online package URL)", tid, st.LatestVersion)
	}
	if !st.CompatibleWithHost {
		return Manifest{}, fmt.Errorf("theme %q v%s requires Volo-go >= %s", tid, st.LatestVersion, st.MinHostVersion)
	}

	expectedSHA := strings.TrimSpace(st.SHA256)
	if expectedSHA == "" && strings.TrimSpace(st.ChecksumURL) != "" {
		resolvedSHA, cErr := extpkg.ResolveCatalogChecksum(
			ctx,
			e.ensureMetaFetcher(),
			e.effectiveOutboundDialer(),
			st.ChecksumURL,
			st.DownloadURL,
			fmt.Sprintf("theme-%s-v%s.zip", tid, st.LatestVersion),
			fmt.Sprintf("%s-%s.zip", tid, st.LatestVersion),
			fmt.Sprintf("theme-%s.zip", tid),
		)
		if cErr != nil {
			return Manifest{}, fmt.Errorf("theme %q resolve checksum_url: %w", tid, cErr)
		}
		expectedSHA = resolvedSHA
	}

	return e.installFromURLInternal(ctx, st.DownloadURL, expectedSHA, installThemeZipOptions{
		overwrite:           true,
		expectedThemeID:     tid,
		expectedPrevVersion: mf.Version,
		disallowDowngrade:   true,
	})
}
