package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/extpkg"
	"github.com/Willh92/Volo-go/internal/i18n"
)

const (
	defaultOfficialPluginsCatalogURL = "https://raw.githubusercontent.com/Willh92/Volo-go/main/plugins/catalog.json"
	defaultOfficialThemesCatalogURL  = "https://raw.githubusercontent.com/Willh92/Volo-go/main/themes/catalog.json"
	defaultOfficialLocalesCatalogURL = "https://raw.githubusercontent.com/Willh92/Volo-go/main/locales/catalog.json"
	statusClientClosedRequest        = 499
	maxLocalCatalogBytes             = 128 << 10
)

// SetCatalogURLsForTest overrides the remote catalog URLs used when ?refresh=1 is requested in tests.
func (s *AppServer) SetCatalogURLsForTest(pluginsURL string, themesURL string, localesURL ...string) {
	if s == nil {
		return
	}
	s.pluginsCatalogURL = strings.TrimSpace(pluginsURL)
	s.themesCatalogURL = strings.TrimSpace(themesURL)
	if len(localesURL) > 0 {
		s.localesCatalogURL = strings.TrimSpace(localesURL[0])
	}
	if s.deps.Plugins != nil {
		s.deps.Plugins.SetOfficialCatalogURL(s.pluginsCatalogURL)
	}
	if s.deps.Themes != nil {
		s.deps.Themes.SetOfficialCatalogURL(s.themesCatalogURL)
	}
}

type adminCatalogEntry struct {
	ID                   string  `json:"id"`
	Code                 string  `json:"code,omitempty"`
	Version              string  `json:"version"`
	MinHostVersion       string  `json:"min_host_version,omitempty"`
	Runtime              string  `json:"runtime,omitempty"`
	Distribution         string  `json:"distribution,omitempty"`
	BundledInCoreArchive bool    `json:"bundled_in_core_archive,omitempty"`
	BundledInDockerImage bool    `json:"bundled_in_docker_image,omitempty"`
	DownloadURL          string  `json:"download_url,omitempty"`
	ChecksumURL          string  `json:"checksum_url,omitempty"`
	AssetName            string  `json:"asset_name,omitempty"`
	SHA256               string  `json:"sha256,omitempty"`
	ReleaseURL           string  `json:"release_url,omitempty"`
	Homepage             string  `json:"homepage,omitempty"`
	ApproxZipMB          float64 `json:"approx_zip_mb,omitempty"`
	ApproxSizeBytes      int64   `json:"approx_size_bytes,omitempty"`
	DisplayName          string  `json:"display_name"`
	DisplaySummary       string  `json:"display_summary"`
	Installed            bool    `json:"installed"`
	InstalledVersion     string  `json:"installed_version,omitempty"`
	IsBuiltin            bool    `json:"is_builtin,omitempty"`
	HasUpdate            bool    `json:"has_update"`
	CompatibleWithHost   bool    `json:"compatible_with_host"`
}

func writeExtensionOpError(w http.ResponseWriter, err error, overwrite bool) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		writeJSON(w, statusClientClosedRequest, map[string]any{
			"error":    err.Error(),
			"canceled": true,
		})
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"error": err.Error(),
		})
		return true
	}
	if errors.Is(err, extpkg.ErrDownloadCanceled) {
		writeJSON(w, statusClientClosedRequest, map[string]any{
			"error":    err.Error(),
			"canceled": true,
		})
		return true
	}
	if !overwrite && isAlreadyExistsInstallErr(err) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    err.Error(),
			"conflict": true,
		})
		return true
	}
	writeErrJSON(w, http.StatusBadRequest, err.Error())
	return true
}

func parseOptionalCheckUpdatesRequest(w http.ResponseWriter, r *http.Request, defaultForce bool) (ids []string, force bool, ok bool) {
	force = defaultForce
	if r.ContentLength != 0 {
		var req struct {
			IDs   []string `json:"ids"`
			Force *bool    `json:"force"`
		}
		if !decodeJSONBody(w, r, 16384, &req) {
			return nil, false, false
		}
		ids = req.IDs
		if req.Force != nil {
			force = *req.Force
		}
	}
	if qForce := strings.TrimSpace(r.URL.Query().Get("force")); qForce != "" {
		switch strings.ToLower(qForce) {
		case "1", "true", "yes":
			force = true
		case "0", "false", "no":
			force = false
		}
	}
	return ids, force, true
}

func readBoundedLocalCatalog(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLocalCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxLocalCatalogBytes {
		return nil, errors.New("local catalog.json exceeds 128 KiB limit")
	}
	return raw, nil
}

func (s *AppServer) loadCatalogBytes(ctx context.Context, localPath string, remoteURL string, refresh bool) (raw []byte, source string, warning string) {
	fetcher := s.catalogFetcher
	if fetcher == nil {
		fetcher = extpkg.NewMetadataFetcher()
	}
	if refresh {
		if b, err := fetcher.FetchRaw(ctx, s.deps.OutboundDialer, remoteURL); err == nil {
			return b, "remote", ""
		} else {
			warning = err.Error()
		}
		if b, err := readBoundedLocalCatalog(localPath); err == nil {
			return b, "local", warning
		}
		return nil, "none", warning
	}

	if b, err := readBoundedLocalCatalog(localPath); err == nil {
		return b, "local", ""
	}
	if b, err := fetcher.FetchRaw(ctx, s.deps.OutboundDialer, remoteURL); err == nil {
		return b, "remote", ""
	} else {
		warning = err.Error()
	}
	return nil, "none", warning
}

type installedExtensionMeta struct {
	version string
	builtin bool
}

func (s *AppServer) buildAdminCatalogEntry(
	it extpkg.CatalogItem,
	repoURL string,
	itemID string,
	code string,
	runtimeStr string,
	locale string,
	installedMap map[string]installedExtensionMeta,
) adminCatalogEntry {
	dispName := strings.TrimSpace(extpkg.ResolveLocalizedAny(it.Name, locale))
	if dispName == "" {
		dispName = itemID
	}
	dispSummary := strings.TrimSpace(extpkg.ResolveLocalizedAny(it.Summary, locale))
	if dispSummary == "" {
		dispSummary = strings.TrimSpace(extpkg.ResolveLocalizedAny(it.Description, locale))
	}
	inst, isInst := installedMap[itemID]
	hasUpd := false
	if isInst && !inst.builtin {
		if cmp, ok := extpkg.CompareSemVer(inst.version, it.Version); ok && cmp < 0 {
			hasUpd = true
		}
	}
	return adminCatalogEntry{
		ID:                   itemID,
		Code:                 code,
		Version:              strings.TrimSpace(it.Version),
		MinHostVersion:       strings.TrimSpace(it.MinHostVersion),
		Runtime:              runtimeStr,
		Distribution:         strings.TrimSpace(it.Distribution),
		BundledInCoreArchive: it.BundledInCoreArchive,
		BundledInDockerImage: it.BundledInDockerImage,
		DownloadURL:          extpkg.SanitizeHTTPURL(it.DownloadURL),
		ChecksumURL:          extpkg.SanitizeHTTPURL(it.ChecksumURL),
		AssetName:            strings.TrimSpace(it.ReleaseAsset),
		SHA256:               strings.TrimSpace(it.SHA256),
		ReleaseURL:           extpkg.ResolveCatalogReleaseURL(repoURL, it),
		Homepage:             extpkg.SanitizeHTTPURL(it.Homepage),
		ApproxZipMB:          it.ApproxZipMB,
		ApproxSizeBytes:      it.ApproxSizeBytes,
		DisplayName:          dispName,
		DisplaySummary:       dispSummary,
		Installed:            isInst,
		InstalledVersion:     inst.version,
		IsBuiltin:            inst.builtin,
		HasUpdate:            hasUpd,
		CompatibleWithHost:   extpkg.IsHostCompatible(s.effectiveAppVersion(), it.MinHostVersion),
	}
}

func (s *AppServer) writeAdminCatalogResponse(w http.ResponseWriter, items []adminCatalogEntry, source, warning string) {
	if items == nil {
		items = []adminCatalogEntry{}
	}
	resp := map[string]any{
		"items":        items,
		"source":       source,
		"host_version": s.effectiveAppVersion(),
	}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *AppServer) handleAdminPluginsCatalog(w http.ResponseWriter, r *http.Request, locale string) {
	refresh := queryBool(r, "refresh")
	localPath := ""
	if dir := s.pluginsDir(); dir != "" {
		localPath = filepath.Join(dir, "catalog.json")
	}
	remoteURL := strings.TrimSpace(s.pluginsCatalogURL)
	if remoteURL == "" {
		remoteURL = defaultOfficialPluginsCatalogURL
	}

	raw, source, warning := s.loadCatalogBytes(r.Context(), localPath, remoteURL, refresh)
	if len(raw) == 0 {
		s.writeAdminCatalogResponse(w, nil, source, warning)
		return
	}

	repoURL, catItems, err := extpkg.ParseCatalogItems(raw)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	installedMap := make(map[string]installedExtensionMeta)
	if s.deps.Plugins != nil {
		for _, p := range s.deps.Plugins.List(locale) {
			installedMap[p.ID] = installedExtensionMeta{
				version: p.Version,
				builtin: strings.EqualFold(p.Kind, "builtin"),
			}
		}
	}

	out := make([]adminCatalogEntry, 0, len(catItems))
	for _, it := range catItems {
		itemID := strings.TrimSpace(it.ID)
		if itemID == "" {
			continue
		}
		out = append(out, s.buildAdminCatalogEntry(it, repoURL, itemID, "", strings.TrimSpace(it.Runtime), locale, installedMap))
	}
	s.writeAdminCatalogResponse(w, out, source, warning)
}

func (s *AppServer) handleAdminThemesCatalog(w http.ResponseWriter, r *http.Request, locale string) {
	refresh := queryBool(r, "refresh")
	localPath := ""
	if dir := s.themesDir(); dir != "" {
		localPath = filepath.Join(dir, "catalog.json")
	}
	remoteURL := strings.TrimSpace(s.themesCatalogURL)
	if remoteURL == "" {
		remoteURL = defaultOfficialThemesCatalogURL
	}

	raw, source, warning := s.loadCatalogBytes(r.Context(), localPath, remoteURL, refresh)
	if len(raw) == 0 {
		s.writeAdminCatalogResponse(w, nil, source, warning)
		return
	}

	repoURL, catItems, err := extpkg.ParseCatalogItems(raw)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	installedMap := make(map[string]installedExtensionMeta)
	if s.deps.Themes != nil {
		for _, th := range s.deps.Themes.ListLocalized(locale) {
			installedMap[th.ID] = installedExtensionMeta{
				version: th.Version,
				builtin: th.IsBuiltin,
			}
		}
	}

	out := make([]adminCatalogEntry, 0, len(catItems))
	for _, it := range catItems {
		itemID := strings.TrimSpace(it.ID)
		if itemID == "" {
			continue
		}
		out = append(out, s.buildAdminCatalogEntry(it, repoURL, itemID, "", "", locale, installedMap))
	}
	s.writeAdminCatalogResponse(w, out, source, warning)
}

func (s *AppServer) handleAdminLocalesCatalog(w http.ResponseWriter, r *http.Request, locale string) {
	refresh := queryBool(r, "refresh")
	localPath := ""
	if dir := s.localesDir(); dir != "" {
		cand := filepath.Join(dir, "catalog.json")
		if _, err := os.Stat(cand); err == nil {
			localPath = cand
		}
	}
	if localPath == "" {
		if cand := filepath.Join("locales", "catalog.json"); cand != "" {
			if _, err := os.Stat(cand); err == nil {
				localPath = cand
			}
		}
	}
	remoteURL := strings.TrimSpace(s.localesCatalogURL)
	if remoteURL == "" {
		remoteURL = defaultOfficialLocalesCatalogURL
	}

	raw, source, warning := s.loadCatalogBytes(r.Context(), localPath, remoteURL, refresh)
	if len(raw) == 0 {
		s.writeAdminCatalogResponse(w, nil, source, warning)
		return
	}

	repoURL, catItems, err := extpkg.ParseCatalogItems(raw)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	installedMap := make(map[string]installedExtensionMeta)
	if s.deps.Catalog != nil {
		for _, p := range s.deps.Catalog.Packs() {
			ver := strings.TrimSpace(p.Version)
			if ver == "" {
				ver = "1.0.0"
			}
			installedMap[p.Code] = installedExtensionMeta{
				version: ver,
				builtin: p.Builtin && !p.HasOverride,
			}
		}
	}

	out := make([]adminCatalogEntry, 0, len(catItems))
	for _, it := range catItems {
		rawID := strings.TrimSpace(it.ID)
		if rawID == "" {
			continue
		}
		code, vErr := i18n.ValidateLocaleCode(rawID)
		if vErr != nil {
			continue
		}
		out = append(out, s.buildAdminCatalogEntry(it, repoURL, code, code, "", locale, installedMap))
	}
	s.writeAdminCatalogResponse(w, out, source, warning)
}

func (s *AppServer) resolveInstallSHA256(ctx context.Context, rawSHA256, checksumURL, assetName, rawURL string) (string, error) {
	sha := strings.TrimSpace(rawSHA256)
	if sha != "" || strings.TrimSpace(checksumURL) == "" {
		return sha, nil
	}
	fetcher := s.catalogFetcher
	if fetcher == nil {
		fetcher = extpkg.NewMetadataFetcher()
	}
	return extpkg.ResolveCatalogChecksum(ctx, fetcher, s.deps.OutboundDialer, checksumURL, assetName, rawURL)
}

func (s *AppServer) handleAdminPluginInstallURL(w http.ResponseWriter, r *http.Request, actor *domain.User, locale string) {
	extendStreamingDeadlines(w, 15*time.Minute)
	var req struct {
		URL         string `json:"url"`
		SHA256      string `json:"sha256"`
		ChecksumURL string `json:"checksum_url"`
		AssetName   string `json:"asset_name"`
		Overwrite   bool   `json:"overwrite"`
		Enable      bool   `json:"enable"`
	}
	if !decodeJSONBody(w, r, 8192, &req) {
		return
	}
	if !req.Overwrite {
		req.Overwrite = queryBool(r, "overwrite")
	}
	if !req.Enable {
		req.Enable = queryBool(r, "enable")
	}

	resolvedSHA, csErr := s.resolveInstallSHA256(r.Context(), req.SHA256, req.ChecksumURL, req.AssetName, req.URL)
	if writeExtensionOpError(w, csErr, req.Overwrite) {
		return
	}

	sum, err := s.deps.Plugins.InstallFromURL(r.Context(), req.URL, resolvedSHA, req.Overwrite, req.Enable, locale)
	if writeExtensionOpError(w, err, req.Overwrite) {
		return
	}
	s.invalidateContentAndMediaCaches()
	s.recordAuditLog(r, actor, "plugin.install_url", "plugin", sum.ID, map[string]any{
		"version":   sum.Version,
		"url":       strings.TrimSpace(req.URL),
		"overwrite": req.Overwrite,
		"enable":    req.Enable,
	})
	list := s.deps.Plugins.List(locale)
	writeJSON(w, http.StatusCreated, map[string]any{
		"plugin":    sum,
		"items":     list,
		"plugins":   list,
		"cron_jobs": s.deps.Plugins.CronJobs(),
	})
}

func (s *AppServer) handleAdminThemeInstallURL(w http.ResponseWriter, r *http.Request, actor *domain.User, locale string) {
	extendStreamingDeadlines(w, 15*time.Minute)
	var req struct {
		URL         string `json:"url"`
		SHA256      string `json:"sha256"`
		ChecksumURL string `json:"checksum_url"`
		AssetName   string `json:"asset_name"`
		Overwrite   bool   `json:"overwrite"`
		Activate    bool   `json:"activate"`
		Target      string `json:"target"`
	}
	if !decodeJSONBody(w, r, 8192, &req) {
		return
	}
	if !req.Overwrite {
		req.Overwrite = queryBool(r, "overwrite")
	}
	if !req.Activate {
		req.Activate = queryBool(r, "activate")
	}

	resolvedSHA, csErr := s.resolveInstallSHA256(r.Context(), req.SHA256, req.ChecksumURL, req.AssetName, req.URL)
	if writeExtensionOpError(w, csErr, req.Overwrite) {
		return
	}

	mf, err := s.deps.Themes.InstallFromURL(r.Context(), req.URL, resolvedSHA, req.Overwrite)
	if writeExtensionOpError(w, err, req.Overwrite) {
		return
	}
	if req.Activate && s.deps.Settings != nil {
		target := strings.ToLower(strings.TrimSpace(req.Target))
		patchMap := map[string]any{"active_theme": mf.ID}
		if target == "mobile" {
			patchMap = map[string]any{"mobile_theme": mf.ID}
		} else if target == "both" {
			patchMap = map[string]any{"active_theme": mf.ID, "mobile_theme": mf.ID}
		}
		if patchBytes, mErr := json.Marshal(patchMap); mErr == nil {
			_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, patchBytes, 0)
		}
	}
	s.invalidateContentAndMediaCaches()
	s.recordAuditLog(r, actor, "theme.install_url", "theme", mf.ID, map[string]any{
		"version":   mf.Version,
		"url":       strings.TrimSpace(req.URL),
		"overwrite": req.Overwrite,
		"activate":  req.Activate,
	})
	s.writeAppearanceThemeResponse(w, r.Context(), http.StatusCreated, locale, false, mf)
}

func (s *AppServer) handleAdminLocaleInstallURL(w http.ResponseWriter, r *http.Request, actor *domain.User, locale string) {
	extendStreamingDeadlines(w, 5*time.Minute)
	var req struct {
		URL         string `json:"url"`
		SHA256      string `json:"sha256"`
		ChecksumURL string `json:"checksum_url"`
		AssetName   string `json:"asset_name"`
		Code        string `json:"code"`
		Overwrite   bool   `json:"overwrite"`
	}
	if !decodeJSONBody(w, r, 8192, &req) {
		return
	}
	if !req.Overwrite {
		req.Overwrite = queryBool(r, "overwrite")
	}
	extDir := s.ensureLocalesDirReady()
	if extDir == "" || s.deps.Catalog == nil {
		writeErrJSON(w, http.StatusBadRequest, "external locales directory is not configured")
		return
	}

	resolvedSHA, csErr := s.resolveInstallSHA256(r.Context(), req.SHA256, req.ChecksumURL, req.AssetName, req.URL)
	if writeExtensionOpError(w, csErr, req.Overwrite) {
		return
	}

	rawURL := strings.TrimSpace(req.URL)
	parsedURL, _ := url.Parse(rawURL)
	urlBase := ""
	if parsedURL != nil && parsedURL.Path != "" {
		urlBase = path.Base(parsedURL.Path)
	}
	isZip := strings.HasSuffix(strings.ToLower(urlBase), ".zip") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(req.AssetName)), ".zip")

	var (
		packSum   i18n.LocalePackSummary
		installed []i18n.LocalePackSummary
	)
	if isZip {
		dl, err := extpkg.StreamVerifyZip(r.Context(), s.deps.OutboundDialer, extpkg.DownloadOptions{
			URL:            rawURL,
			ExpectedSHA256: resolvedSHA,
			TempDir:        s.ensureTempDir(),
			MaxBytes:       16 << 20,
			MaxDuration:    time.Minute,
		})
		if writeExtensionOpError(w, err, req.Overwrite) {
			return
		}
		defer dl.Cleanup()
		packs, zErr := s.deps.Catalog.InstallPackZip(dl.File, dl.Size, req.Overwrite)
		if writeExtensionOpError(w, zErr, req.Overwrite) {
			return
		}
		installed = packs
		if len(packs) > 0 {
			packSum = packs[0]
		}
	} else {
		dl, err := extpkg.DownloadRawFile(r.Context(), s.deps.OutboundDialer, extpkg.DownloadOptions{
			URL:            rawURL,
			ExpectedSHA256: resolvedSHA,
			TempDir:        s.ensureTempDir(),
			MaxBytes:       4 << 20,
			MaxDuration:    time.Minute,
		})
		if writeExtensionOpError(w, err, req.Overwrite) {
			return
		}
		defer dl.Cleanup()
		rawBytes, rErr := io.ReadAll(io.LimitReader(dl.File, (4<<20)+1))
		if rErr != nil {
			writeErrJSON(w, http.StatusBadRequest, rErr.Error())
			return
		}
		codeHint := strings.TrimSpace(req.Code)
		if codeHint == "" {
			var peek map[string]string
			if json.Unmarshal(rawBytes, &peek) == nil && strings.TrimSpace(peek["meta.code"]) != "" {
				codeHint = strings.TrimSpace(peek["meta.code"])
			}
		}
		if codeHint == "" {
			candName := strings.TrimSpace(req.AssetName)
			if candName == "" {
				candName = urlBase
			}
			if candName != "" && candName != "." && candName != "/" {
				codeHint = strings.TrimSuffix(candName, path.Ext(candName))
			}
		}
		sum, jErr := s.deps.Catalog.InstallPackJSON(codeHint, rawBytes, req.Overwrite)
		if writeExtensionOpError(w, jErr, req.Overwrite) {
			return
		}
		packSum = sum
		installed = []i18n.LocalePackSummary{sum}
	}

	if s.deps.Themes != nil {
		_ = s.deps.Themes.Reload()
	}
	if s.deps.Content != nil {
		s.deps.Content.PurgeRuntimeCaches()
	}
	s.recordAuditLog(r, actor, "locale.install_url", "locale", packSum.Code, map[string]any{
		"url":       rawURL,
		"overwrite": req.Overwrite,
		"count":     len(installed),
	})
	var siteVer int64
	if s.deps.Settings != nil {
		_, siteVer, _ = s.deps.Settings.GetDomain(r.Context(), domain.SettingsDomainSite)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":           true,
		"pack":         packSum,
		"installed":    installed,
		"locales_dir":  extDir,
		"items":        s.deps.Catalog.Packs(),
		"locales":      s.deps.Catalog.Locales(),
		"site_version": siteVer,
	})
}
