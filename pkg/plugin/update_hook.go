package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	pluginUpdateCacheTTL       = 30 * time.Minute
	pluginUpdateErrorRetryTTL  = 60 * time.Second
	pluginUpdateHookTimeout    = 5 * time.Second
	pluginUpdateMaxConcurrency = 4

	defaultOfficialPluginsCatalogURL = "https://raw.githubusercontent.com/Willh92/Volo-go/main/plugins/catalog.json"
)

type cachedPluginUpdate struct {
	status    extpkg.UpdateStatus
	expiresAt time.Time
}

type pluginUpdateCall struct {
	done   chan struct{}
	status extpkg.UpdateStatus
}

func (m *Manager) effectiveResolver() security.ResolverFunc {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	res := m.customResolver
	base := m.outboundDialer
	m.mu.RUnlock()
	if res != nil {
		return res
	}
	if base != nil {
		return base.Resolver
	}
	return nil
}

// SetHostVersion sets or updates the running host engine SemVer used for MinHostVersion compatibility gates.
func (m *Manager) SetHostVersion(ver string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.hostVersion = strings.TrimSpace(ver)
	m.mu.Unlock()
}

func (m *Manager) effectiveHostVersion() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	v := m.hostVersion
	m.mu.RUnlock()
	return v
}

// SetOutboundDialer sets the SSRF-safe outbound dialer used for remote plugin downloads and declarative update checks.
func (m *Manager) SetOutboundDialer(d *security.OutboundSafeDialer) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.outboundDialer = d
	m.mu.Unlock()
}

// SetOfficialCatalogURL overrides the official plugins catalog URL used when legacy v1.0.2 official plugins omit manifest.update.
func (m *Manager) SetOfficialCatalogURL(raw string) {
	if m == nil {
		return
	}
	trimmed := strings.TrimSpace(raw)
	m.officialCatalogURL.Store(&trimmed)
	m.clearAllUpdateStatus()
}

func (m *Manager) effectiveOfficialCatalogURL() string {
	if m != nil {
		if ptr := m.officialCatalogURL.Load(); ptr != nil && *ptr != "" {
			return *ptr
		}
	}
	return defaultOfficialPluginsCatalogURL
}

func (m *Manager) effectiveOutboundDialer() *security.OutboundSafeDialer {
	if m == nil {
		return &security.OutboundSafeDialer{Timeout: 10 * time.Second}
	}
	m.mu.RLock()
	base := m.outboundDialer
	allowLoop := m.allowLoopbackForTests
	m.mu.RUnlock()
	resolver := m.effectiveResolver()

	if base != nil {
		return &security.OutboundSafeDialer{
			AllowedHosts:          nil,
			Timeout:               base.Timeout,
			Resolver:              resolver,
			AllowLoopbackForTests: base.AllowLoopbackForTests || allowLoop,
		}
	}
	return &security.OutboundSafeDialer{
		Timeout:               10 * time.Second,
		Resolver:              resolver,
		AllowLoopbackForTests: allowLoop,
	}
}

func (m *Manager) pluginInstallLock(pluginID string) *sync.Mutex {
	m.installLocksMu.Lock()
	defer m.installLocksMu.Unlock()
	if m.installLocks == nil {
		m.installLocks = make(map[string]*sync.Mutex)
	}
	lk, ok := m.installLocks[pluginID]
	if !ok {
		lk = &sync.Mutex{}
		m.installLocks[pluginID] = lk
	}
	return lk
}

// recoverInterruptedSwaps cleans up orphaned staging/download artifacts in pluginsDir and
// self-heals any interrupted directory swap where `.backup-<id>-*` survived without `<id>`.
func (m *Manager) recoverInterruptedSwaps() {
	if m == nil || strings.TrimSpace(m.pluginsDir) == "" {
		return
	}
	entries, err := os.ReadDir(m.pluginsDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		name := ent.Name()
		fullPath := filepath.Join(m.pluginsDir, name)
		if strings.HasPrefix(name, ".staging-") || strings.HasPrefix(name, ".volo-plugin-dl-") {
			_ = os.RemoveAll(fullPath)
			continue
		}
		if !ent.IsDir() || !strings.HasPrefix(name, ".backup-") {
			continue
		}
		// Format: .backup-<pluginID>-<txid>
		rest := strings.TrimPrefix(name, ".backup-")
		lastDash := strings.LastIndexByte(rest, '-')
		if lastDash <= 0 {
			_ = os.RemoveAll(fullPath)
			continue
		}
		pid := rest[:lastDash]
		if !validPluginIDRe.MatchString(pid) {
			_ = os.RemoveAll(fullPath)
			continue
		}
		targetDir := filepath.Join(m.pluginsDir, pid)
		if _, statErr := os.Stat(targetDir); os.IsNotExist(statErr) {
			if rErr := os.Rename(fullPath, targetDir); rErr == nil {
				slog.Warn("plugin: recovered interrupted upgrade directory from backup", "plugin_id", pid)
				if m.logger != nil {
					m.logger.Write(pid, "warn", "recovery", "restored plugin directory after interrupted upgrade")
				}
			}
		} else if statErr == nil {
			_ = os.RemoveAll(fullPath)
		}
	}
}

func detectUpdateHookHint(kind string, manifest Manifest, pluginDir string, builtin BuiltinPlugin) bool {
	if kind == "builtin" {
		if builtin == nil {
			return false
		}
		_, ok := builtin.(UpdateChecker)
		return ok
	}
	if manifest.Runtime != RuntimeGoja || strings.TrimSpace(pluginDir) == "" {
		return false
	}
	entryPath := filepath.Join(pluginDir, filepath.FromSlash(manifest.Entry))
	f, err := os.Open(entryPath)
	if err != nil {
		return false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxZipSingleBytes))
	if err != nil {
		return false
	}
	return bytes.Contains(raw, []byte("onCheckUpdate"))
}

func isKnownOfficialCatalogPlugin(mf Manifest) bool {
	switch mf.ID {
	case "sensitive-words", "kanbanniang":
	default:
		return false
	}
	author := strings.TrimSpace(mf.Author)
	return author == "" || strings.EqualFold(author, "Volo Studio") || strings.EqualFold(author, "Volo-go")
}

func (m *Manager) effectivePluginUpdateConfigLocked(mp *managedPlugin) *extpkg.UpdateConfig {
	if mp == nil {
		return nil
	}
	if mp.manifest.Update != nil {
		return mp.manifest.Update
	}
	hasHook := mp.hasUpdateHookHint
	if mp.gojaRT != nil {
		hasHook = mp.gojaRT.hasCallable("onCheckUpdate")
	}
	if mp.kind == "external" && !hasHook && isKnownOfficialCatalogPlugin(mp.manifest) {
		return &extpkg.UpdateConfig{
			Strategy: extpkg.StrategyVoloCatalog,
			URL:      m.effectiveOfficialCatalogURL(),
		}
	}
	return nil
}

func (m *Manager) pluginUpdateCapabilityLocked(mp *managedPlugin) string {
	if mp == nil {
		return extpkg.CapabilityNone
	}
	upd := m.effectivePluginUpdateConfigLocked(mp)
	if upd != nil && upd.Strategy == extpkg.StrategyNone {
		return extpkg.CapabilityNone
	}
	hasDeclarative := upd != nil && (upd.Strategy == extpkg.StrategyVoloCatalog ||
		upd.Strategy == extpkg.StrategyManifestJSON ||
		upd.Strategy == extpkg.StrategyGitHubRelease ||
		upd.Strategy == extpkg.StrategyHook)
	hasHook := mp.hasUpdateHookHint
	if mp.gojaRT != nil {
		hasHook = mp.gojaRT.hasCallable("onCheckUpdate")
	}
	if !hasDeclarative && !hasHook {
		return extpkg.CapabilityNone
	}
	if mp.kind == "builtin" {
		return extpkg.CapabilityCheckOnly
	}
	if upd != nil && !upd.OnlineAllowed() {
		return extpkg.CapabilityCheckOnly
	}
	return extpkg.CapabilityOnline
}

func (m *Manager) invalidateUpdateStatus(pluginID string) {
	if m == nil {
		return
	}
	m.updateMu.Lock()
	delete(m.updateCache, pluginID)
	m.updateMu.Unlock()
}

func (m *Manager) clearAllUpdateStatus() {
	if m == nil {
		return
	}
	m.updateMu.Lock()
	clear(m.updateCache)
	m.updateMu.Unlock()
}

func (m *Manager) getCachedUpdateStatus(pluginID string, currentVersion string) *extpkg.UpdateStatus {
	if m == nil {
		return nil
	}
	m.updateMu.RLock()
	entry, ok := m.updateCache[pluginID]
	m.updateMu.RUnlock()
	if !ok || entry.status.CurrentVersion != currentVersion {
		return nil
	}
	cp := entry.status
	return &cp
}

// CheckPluginUpdate checks update availability for a single registered plugin.
// Plugins with update_capability == "none" return StateUnsupported immediately with zero network or VM work.
func (m *Manager) CheckPluginUpdate(ctx context.Context, pluginID string, force bool, locale string) (extpkg.UpdateStatus, error) {
	if m == nil {
		return extpkg.UpdateStatus{}, errors.New("plugin: manager unavailable")
	}
	if err := ctx.Err(); err != nil {
		return extpkg.UpdateStatus{}, err
	}
	mp, ok := m.getPlugin(strings.TrimSpace(pluginID))
	if !ok {
		return extpkg.UpdateStatus{}, fmt.Errorf("plugin %q not found", pluginID)
	}

	mp.mu.Lock()
	pid := mp.manifest.ID
	curVer := mp.manifest.Version
	capab := m.pluginUpdateCapabilityLocked(mp)
	mp.mu.Unlock()

	if capab == extpkg.CapabilityNone {
		return extpkg.UpdateStatus{
			State:              extpkg.StateUnsupported,
			CurrentVersion:     curVer,
			CompatibleWithHost: true,
		}, nil
	}

	now := time.Now()
	m.updateMu.Lock()
	if !force {
		if cached, has := m.updateCache[pid]; has && cached.status.CurrentVersion == curVer && now.Before(cached.expiresAt) {
			st := cached.status
			m.updateMu.Unlock()
			return st, nil
		}
	}
	if call, active := m.updateInflight[pid]; active {
		m.updateMu.Unlock()
		select {
		case <-ctx.Done():
			return extpkg.UpdateStatus{}, ctx.Err()
		case <-call.done:
			return call.status, nil
		}
	}
	call := &pluginUpdateCall{done: make(chan struct{})}
	m.updateInflight[pid] = call
	m.updateMu.Unlock()

	go func() {
		leaderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()

		status := m.runSinglePluginUpdateCheck(leaderCtx, mp, locale)
		skipErrorCache := status.State == extpkg.StateError && (leaderCtx.Err() != nil || ctx.Err() != nil)

		// Snapshot installation presence BEFORE acquiring leaf lock m.updateMu.
		_, stillInstalled := m.getPlugin(pid)

		m.updateMu.Lock()
		if stillInstalled && !skipErrorCache {
			ttl := pluginUpdateCacheTTL
			if status.State == extpkg.StateError {
				ttl = pluginUpdateErrorRetryTTL
			}
			m.updateCache[pid] = cachedPluginUpdate{
				status:    status,
				expiresAt: time.Now().Add(ttl),
			}
		}
		call.status = status
		delete(m.updateInflight, pid)
		close(call.done)
		m.updateMu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return extpkg.UpdateStatus{}, ctx.Err()
	case <-call.done:
		return call.status, nil
	}
}

// CheckUpdates checks updates across the requested plugin IDs (or all installed plugins when ids is empty)
// with bounded concurrency (max 4 workers) and silent skipping of unsupported plugins.
func (m *Manager) CheckUpdates(ctx context.Context, ids []string, force bool, locale string) (extpkg.BatchCheckSummary, error) {
	out := extpkg.BatchCheckSummary{
		Statuses: make(map[string]extpkg.UpdateStatus),
	}
	if m == nil {
		return out, nil
	}

	var targets []*managedPlugin
	if len(ids) > 0 {
		seen := make(map[string]bool, len(ids))
		for _, rawID := range ids {
			pid := strings.TrimSpace(rawID)
			if pid == "" || seen[pid] {
				continue
			}
			seen[pid] = true
			if mp, ok := m.getPlugin(pid); ok {
				targets = append(targets, mp)
			}
		}
	} else {
		m.mu.RLock()
		targets = make([]*managedPlugin, 0, len(m.plugins))
		for _, mp := range m.plugins {
			targets = append(targets, mp)
		}
		m.mu.RUnlock()
		sort.Slice(targets, func(i, j int) bool {
			return targets[i].manifest.ID < targets[j].manifest.ID
		})
	}

	var mu sync.Mutex
	sem := make(chan struct{}, pluginUpdateMaxConcurrency)
	var wg sync.WaitGroup

	for _, mp := range targets {
		mp.mu.Lock()
		capab := m.pluginUpdateCapabilityLocked(mp)
		curVer := mp.manifest.Version
		pid := mp.manifest.ID
		mp.mu.Unlock()

		if capab == extpkg.CapabilityNone {
			mu.Lock()
			out.Unsupported++
			out.Statuses[pid] = extpkg.UpdateStatus{
				State:              extpkg.StateUnsupported,
				CurrentVersion:     curVer,
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
			st, err := m.CheckPluginUpdate(ctx, targetID, force, locale)
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
		}(pid)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func (m *Manager) runSinglePluginUpdateCheck(ctx context.Context, mp *managedPlugin, locale string) extpkg.UpdateStatus {
	mp.mu.Lock()
	mf := mp.manifest
	effUpd := m.effectivePluginUpdateConfigLocked(mp)
	capab := m.pluginUpdateCapabilityLocked(mp)
	kind := mp.kind
	pluginDir := mp.pluginDir
	builtinImpl := mp.builtin
	warmRT := mp.gojaRT
	pubCfgCopy := make(map[string]any, len(mp.publicConfig))
	for k, v := range mp.publicConfig {
		pubCfgCopy[k] = v
	}
	mp.mu.Unlock()

	nowMs := time.Now().UnixMilli()
	hostVer := m.effectiveHostVersion()
	dialer := m.effectiveOutboundDialer()

	errStatus := func(err error) extpkg.UpdateStatus {
		msg := err.Error()
		if m.logger != nil {
			m.logger.Write(mf.ID, "warn", "check_update", msg)
		}
		return extpkg.UpdateStatus{
			State:              extpkg.StateError,
			CurrentVersion:     mf.Version,
			CompatibleWithHost: true,
			CheckedAt:          nowMs,
			Error:              msg,
		}
	}

	var rawRes extpkg.UpdateCheckResult
	var checkErr error

	// Tier 1: Native Go declarative strategies (zero JS VM overhead).
	if effUpd != nil && (effUpd.Strategy == extpkg.StrategyVoloCatalog ||
		effUpd.Strategy == extpkg.StrategyManifestJSON ||
		effUpd.Strategy == extpkg.StrategyGitHubRelease) {
		rawRes, checkErr = m.metaFetcher.CheckDeclarative(ctx, dialer, mf.ID, *effUpd, locale)
	} else if kind == "builtin" {
		// Tier 2A: Compiled Go BuiltinPlugin UpdateChecker interface.
		checker, ok := builtinImpl.(UpdateChecker)
		if !ok {
			return extpkg.UpdateStatus{
				State:              extpkg.StateUnsupported,
				CurrentVersion:     mf.Version,
				CompatibleWithHost: true,
			}
		}
		hookCtx, cancel := context.WithTimeout(ctx, pluginUpdateHookTimeout)
		defer cancel()
		checkErr = safeCallBuiltin(mf.ID, "CheckUpdate", func() error {
			var cErr error
			rawRes, cErr = checker.CheckUpdate(hookCtx, locale)
			return cErr
		})
	} else {
		// Tier 2B: Sandboxed Goja JS onCheckUpdate(ctx) hook.
		// Never trips the plugin's production circuit breaker if remote update API fails!
		var updateURL string
		if effUpd != nil {
			updateURL = effUpd.URL
		}
		hookPayload := map[string]any{
			"plugin_id":       mf.ID,
			"current_version": mf.Version,
			"host_version":    hostVer,
			"locale":          locale,
			"update_url":      updateURL,
			"config":          pubCfgCopy,
		}

		targetRT := warmRT
		ephemeral := false
		if targetRT == nil {
			var rtErr error
			targetRT, rtErr = newGojaPluginRuntime(gojaRuntimeOptions{
				manifest:              mf,
				pluginDir:             pluginDir,
				kv:                    mp.kv,
				logger:                m.logger,
				hostI18n:              m.catalog,
				getConfig:             mp.getConfigMap,
				unsealSecret:          m.unsealSecretValue,
				getSiteInfo:           m.getSiteInfo,
				customResolver:        m.effectiveResolver(),
				allowLoopbackForTests: dialer.AllowLoopbackForTests,
			})
			if rtErr != nil {
				return errStatus(rtErr)
			}
			ephemeral = true
		}
		if ephemeral {
			defer targetRT.closeRuntime()
		}

		if !targetRT.hasCallable("onCheckUpdate") {
			mp.mu.Lock()
			mp.hasUpdateHookHint = false
			mp.mu.Unlock()
			return extpkg.UpdateStatus{
				State:              extpkg.StateUnsupported,
				CurrentVersion:     mf.Version,
				CompatibleWithHost: true,
			}
		}

		outVal, _, invErr := targetRT.invoke(ctx, pluginUpdateHookTimeout, "onCheckUpdate", hookPayload)
		if invErr != nil {
			checkErr = invErr
		} else {
			rawRes, checkErr = coerceJSUpdateCheckResult(outVal, mf.Version, locale)
		}
	}

	if checkErr != nil {
		return errStatus(checkErr)
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
		return errStatus(nErr)
	}
	return norm
}

func coerceJSUpdateCheckResult(val any, currentVersion string, locale string) (extpkg.UpdateCheckResult, error) {
	if val == nil {
		noUpdate := false
		return extpkg.UpdateCheckResult{
			LatestVersion: currentVersion,
			HasUpdate:     &noUpdate,
		}, nil
	}
	m, ok := val.(map[string]any)
	if !ok {
		return extpkg.UpdateCheckResult{}, errors.New("onCheckUpdate must return an object or null")
	}
	pickStr := func(keys ...string) string {
		for _, k := range keys {
			if raw, exists := m[k]; exists && raw != nil {
				if s, sOk := raw.(string); sOk && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	pickBoolPtr := func(keys ...string) *bool {
		for _, k := range keys {
			if raw, exists := m[k]; exists && raw != nil {
				if b, bOk := raw.(bool); bOk {
					v := b
					return &v
				}
			}
		}
		return nil
	}

	latest := pickStr("latest_version", "version", "tag_name")
	if latest == "" {
		return extpkg.UpdateCheckResult{}, errors.New("onCheckUpdate result missing latest_version")
	}
	var changelog string
	for _, ck := range []string{"changelog", "release_notes", "notes"} {
		if raw, exists := m[ck]; exists && raw != nil {
			if resolved := extpkg.ResolveLocalizedAny(raw, locale); resolved != "" {
				changelog = resolved
				break
			}
		}
	}
	return extpkg.UpdateCheckResult{
		LatestVersion:   latest,
		HasUpdate:       pickBoolPtr("has_update"),
		OnlineUpdatable: pickBoolPtr("online_updatable", "allow_online_update"),
		MinHostVersion:  pickStr("min_host_version"),
		DownloadURL:     pickStr("download_url", "asset_url", "zip_url"),
		ChecksumURL:     pickStr("checksum_url"),
		SHA256:          pickStr("sha256", "checksum"),
		ReleaseURL:      pickStr("release_url", "homepage", "html_url"),
		Changelog:       changelog,
		PublishedAt:     pickStr("published_at"),
	}, nil
}

// InstallFromURL streams a remote plugin .zip directly to disk under SSRF + stall-watchdog protection,
// verifies optional SHA-256, and installs it transactionally with zero archive heap buffering.
func (m *Manager) InstallFromURL(
	ctx context.Context,
	rawURL string,
	expectedSHA256 string,
	overwrite bool,
	enableAfter bool,
	locale string,
) (PluginSummary, error) {
	return m.installFromURLInternal(ctx, rawURL, expectedSHA256, installZipOptions{
		overwrite:   overwrite,
		enableAfter: enableAfter,
		locale:      locale,
	})
}

func (m *Manager) installFromURLInternal(
	ctx context.Context,
	rawURL string,
	expectedSHA256 string,
	opts installZipOptions,
) (PluginSummary, error) {
	if strings.TrimSpace(m.pluginsDir) == "" {
		return PluginSummary{}, errors.New("plugin: external plugins directory is not configured")
	}
	dl, err := extpkg.StreamVerifyZip(ctx, m.effectiveOutboundDialer(), extpkg.DownloadOptions{
		URL:            rawURL,
		ExpectedSHA256: expectedSHA256,
		TempDir:        m.pluginsDir,
		TempPrefix:     ".volo-plugin-dl-",
		MaxBytes:       MaxZipArchiveBytes,
		MaxDuration:    5 * time.Minute,
	})
	if err != nil {
		return PluginSummary{}, err
	}
	defer dl.Cleanup()

	return m.installFromZipInternal(ctx, dl.File, dl.Size, opts)
}

// UpgradePlugin performs a verified 1-click online upgrade for an already-installed external plugin,
// enforcing update capability, host compatibility, SHA-256 verification, manifest ID pinning, and atomic rollback.
func (m *Manager) UpgradePlugin(ctx context.Context, pluginID string, locale string) (PluginSummary, error) {
	pid := strings.TrimSpace(pluginID)
	mp, ok := m.getPlugin(pid)
	if !ok {
		return PluginSummary{}, fmt.Errorf("plugin %q not found", pid)
	}
	mp.mu.Lock()
	kind := mp.kind
	capab := m.pluginUpdateCapabilityLocked(mp)
	curVer := mp.manifest.Version
	wasEnabled := mp.enabled && mp.circuitState != CircuitOpen
	mp.mu.Unlock()

	if kind == "builtin" || capab != extpkg.CapabilityOnline {
		return PluginSummary{}, fmt.Errorf("plugin %q does not support online updates", pid)
	}

	var st extpkg.UpdateStatus
	if cached := m.getCachedUpdateStatus(pid, curVer); cached != nil && cached.State != extpkg.StateUnchecked {
		st = *cached
	} else {
		var err error
		st, err = m.CheckPluginUpdate(ctx, pid, false, locale)
		if err != nil {
			return PluginSummary{}, err
		}
	}

	if st.State == extpkg.StateError {
		return PluginSummary{}, fmt.Errorf("plugin %q update check failed: %s", pid, st.Error)
	}
	if !st.HasUpdate {
		return PluginSummary{}, fmt.Errorf("plugin %q is already up to date (v%s)", pid, curVer)
	}
	if !st.OnlineUpdatable || strings.TrimSpace(st.DownloadURL) == "" {
		return PluginSummary{}, fmt.Errorf("plugin %q v%s requires manual update (no online package URL)", pid, st.LatestVersion)
	}
	if !st.CompatibleWithHost {
		return PluginSummary{}, fmt.Errorf("plugin %q v%s requires Volo-go >= %s", pid, st.LatestVersion, st.MinHostVersion)
	}

	expectedSHA := strings.TrimSpace(st.SHA256)
	if expectedSHA == "" && strings.TrimSpace(st.ChecksumURL) != "" {
		resolvedSHA, cErr := extpkg.ResolveCatalogChecksum(
			ctx,
			m.metaFetcher,
			m.effectiveOutboundDialer(),
			st.ChecksumURL,
			st.DownloadURL,
			fmt.Sprintf("plugin-%s-v%s.zip", pid, st.LatestVersion),
			fmt.Sprintf("%s-%s.zip", pid, st.LatestVersion),
			fmt.Sprintf("plugin-%s.zip", pid),
		)
		if cErr != nil {
			return PluginSummary{}, fmt.Errorf("plugin %q: resolve checksum from %s: %w", pid, st.ChecksumURL, cErr)
		}
		expectedSHA = resolvedSHA
	}

	return m.installFromURLInternal(ctx, st.DownloadURL, expectedSHA, installZipOptions{
		overwrite:           true,
		enableAfter:         wasEnabled,
		expectedPluginID:    pid,
		expectedPrevVersion: curVer,
		disallowDowngrade:   true,
		locale:              locale,
	})
}
