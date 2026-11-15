package extpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/security"
)

const (
	// Update check strategies
	StrategyNone          = "none"
	StrategyVoloCatalog   = "volo-catalog"
	StrategyManifestJSON  = "manifest-json"
	StrategyGitHubRelease = "github-release"
	StrategyHook          = "hook"

	// Extension update capability levels
	CapabilityNone      = "none"       // Does not support update detection
	CapabilityCheckOnly = "check_only" // Supports detecting new versions/changelogs, but NOT online overwrite
	CapabilityOnline    = "online"     // Supports both update detection and 1-click transactional online update

	// Update check state values
	StateUnsupported     = "unsupported"
	StateUnchecked       = "unchecked"
	StateUpToDate        = "up_to_date"
	StateUpdateAvailable = "update_available"
	StateError           = "error"

	maxMetadataBodyBytes  = 128 << 10 // 128 KiB hard ceiling on update metadata JSON / SHA256SUMS.txt
	maxCachedURLResources = 32
	maxChangelogChars     = 2048
	defaultCheckTimeout   = 8 * time.Second
	leaderFetchTimeout    = 15 * time.Second
)

// UpdateConfig defines optional declarative update detection settings inside manifest.json / theme.json.
type UpdateConfig struct {
	Strategy          string `json:"strategy"`
	URL               string `json:"url,omitempty"`
	AssetPattern      string `json:"asset_pattern,omitempty"`
	TagPrefix         string `json:"tag_prefix,omitempty"`
	AllowOnlineUpdate *bool  `json:"allow_online_update,omitempty"`
}

// OnlineAllowed reports whether the manifest permits online updates (defaults to true when omitted).
func (c *UpdateConfig) OnlineAllowed() bool {
	if c == nil || c.AllowOnlineUpdate == nil {
		return true
	}
	return *c.AllowOnlineUpdate
}

// ValidateUpdateConfig validates and normalizes an optional UpdateConfig block.
// Pass allowHook=true for plugins and allowHook=false for declarative-only themes.
func ValidateUpdateConfig(cfg *UpdateConfig, allowHook bool, allowLoopback bool) error {
	if cfg == nil {
		return nil
	}
	cfg.Strategy = strings.ToLower(strings.TrimSpace(cfg.Strategy))
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.AssetPattern = strings.TrimSpace(cfg.AssetPattern)
	cfg.TagPrefix = strings.TrimSpace(cfg.TagPrefix)

	switch cfg.Strategy {
	case "", StrategyNone:
		cfg.Strategy = StrategyNone
		return nil
	case StrategyHook:
		if !allowHook {
			return fmt.Errorf("update strategy %q is not supported for this package type", StrategyHook)
		}
		if cfg.URL != "" {
			if _, err := ValidatePublicHTTPURL(cfg.URL, allowLoopback); err != nil {
				return fmt.Errorf("update.url: %w", err)
			}
		}
		return nil
	case StrategyVoloCatalog, StrategyManifestJSON, StrategyGitHubRelease:
		if cfg.URL == "" {
			return fmt.Errorf("update.url is required for strategy %q", cfg.Strategy)
		}
		if _, err := ValidatePublicHTTPURL(cfg.URL, allowLoopback); err != nil {
			return fmt.Errorf("update.url: %w", err)
		}
		if len(cfg.AssetPattern) > 128 {
			return fmt.Errorf("update.asset_pattern exceeds 128 characters")
		}
		if len(cfg.TagPrefix) > 64 {
			return fmt.Errorf("update.tag_prefix exceeds 64 characters")
		}
		return nil
	default:
		return fmt.Errorf("unsupported update.strategy %q", cfg.Strategy)
	}
}

// UpdateCheckResult is the raw result produced by either a declarative metadata fetch or a custom plugin hook.
type UpdateCheckResult struct {
	LatestVersion   string `json:"latest_version"`
	HasUpdate       *bool  `json:"has_update,omitempty"`
	OnlineUpdatable *bool  `json:"online_updatable,omitempty"`
	MinHostVersion  string `json:"min_host_version,omitempty"`
	DownloadURL     string `json:"download_url,omitempty"`
	ChecksumURL     string `json:"checksum_url,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	ReleaseURL      string `json:"release_url,omitempty"`
	Changelog       string `json:"changelog,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	ApproxSizeBytes int64  `json:"approx_size_bytes,omitempty"`
}

// UpdateStatus is the sanitized, cached update status attached to PluginSummary / LocalizedTheme.
type UpdateStatus struct {
	State              string `json:"state"` // unsupported | unchecked | up_to_date | update_available | error
	CurrentVersion     string `json:"current_version"`
	LatestVersion      string `json:"latest_version,omitempty"`
	HasUpdate          bool   `json:"has_update"`
	OnlineUpdatable    bool   `json:"online_updatable"`
	CompatibleWithHost bool   `json:"compatible_with_host"`
	MinHostVersion     string `json:"min_host_version,omitempty"`
	DownloadURL        string `json:"download_url,omitempty"`
	ChecksumURL        string `json:"checksum_url,omitempty"`
	SHA256             string `json:"sha256,omitempty"`
	ReleaseURL         string `json:"release_url,omitempty"`
	Changelog          string `json:"changelog,omitempty"`
	PublishedAt        string `json:"published_at,omitempty"`
	ApproxSizeBytes    int64  `json:"approx_size_bytes,omitempty"`
	CheckedAt          int64  `json:"checked_at,omitempty"`
	Error              string `json:"error,omitempty"`
}

// BatchCheckSummary summarizes a manual or batch CheckUpdates run for Admin UI feedback.
type BatchCheckSummary struct {
	Statuses    map[string]UpdateStatus `json:"statuses"`
	Checked     int                     `json:"checked"`
	Updates     int                     `json:"updates"`
	Unsupported int                     `json:"unsupported"`
	Errors      int                     `json:"errors"`
}

// NormalizeUpdateStatus sanitizes an UpdateCheckResult against the installed version, host version,
// and extension capability rules (enforcing check_only when builtin, disallowed, or missing valid download_url).
func NormalizeUpdateStatus(
	currentVersion string,
	hostVersion string,
	capability string,
	fallbackHomepage string,
	allowLoopback bool,
	res UpdateCheckResult,
	checkedAtMs int64,
) (UpdateStatus, error) {
	if capability == CapabilityNone {
		return UpdateStatus{
			State:              StateUnsupported,
			CurrentVersion:     currentVersion,
			CompatibleWithHost: true,
		}, nil
	}
	latest := strings.TrimSpace(res.LatestVersion)
	if latest == "" {
		return UpdateStatus{}, errors.New("update check returned empty latest_version")
	}
	latestClean := NormalizeVersion(latest)
	curClean := NormalizeVersion(currentVersion)

	hasUpdate := IsNewerVersion(curClean, latestClean)
	if res.HasUpdate != nil {
		if _, semverOK := CompareSemVer(latestClean, curClean); !semverOK {
			hasUpdate = *res.HasUpdate
		} else if !*res.HasUpdate {
			hasUpdate = false
		}
	}

	minHost := strings.TrimSpace(res.MinHostVersion)
	compat := IsHostCompatible(hostVersion, minHost)

	var cleanDL string
	if rawDL := SanitizeHTTPURL(res.DownloadURL); rawDL != "" {
		if u, err := ValidatePublicHTTPURL(rawDL, allowLoopback); err == nil {
			cleanDL = u.String()
		}
	}

	var cleanCS string
	if rawCS := SanitizeHTTPURL(res.ChecksumURL); rawCS != "" {
		if u, err := ValidatePublicHTTPURL(rawCS, allowLoopback); err == nil {
			cleanCS = u.String()
		}
	}

	var cleanSHA string
	if rawSHA := strings.TrimSpace(res.SHA256); rawSHA != "" {
		if sum, err := NormalizeSHA256(rawSHA); err == nil {
			cleanSHA = sum
		}
	}

	cleanRel := ""
	for _, cand := range []string{res.ReleaseURL, fallbackHomepage} {
		sanitized := SanitizeHTTPURL(cand)
		if sanitized == "" {
			continue
		}
		if u, err := ValidatePublicHTTPURL(sanitized, allowLoopback); err == nil {
			cleanRel = u.String()
			break
		}
	}

	onlineUpdatable := capability == CapabilityOnline && cleanDL != ""
	if res.OnlineUpdatable != nil && !*res.OnlineUpdatable {
		onlineUpdatable = false
	}
	if !onlineUpdatable {
		cleanDL = ""
		cleanCS = ""
	}

	var approxSize int64
	if res.ApproxSizeBytes > 0 {
		approxSize = res.ApproxSizeBytes
	}

	state := StateUpToDate
	if hasUpdate {
		state = StateUpdateAvailable
	}

	return UpdateStatus{
		State:              state,
		CurrentVersion:     currentVersion,
		LatestVersion:      latestClean,
		HasUpdate:          hasUpdate,
		OnlineUpdatable:    onlineUpdatable,
		CompatibleWithHost: compat,
		MinHostVersion:     minHost,
		DownloadURL:        cleanDL,
		ChecksumURL:        cleanCS,
		SHA256:             cleanSHA,
		ReleaseURL:         cleanRel,
		Changelog:          SanitizePlainChangelog(res.Changelog),
		PublishedAt:        truncateUTF8(strings.TrimSpace(res.PublishedAt), 64),
		ApproxSizeBytes:    approxSize,
		CheckedAt:          checkedAtMs,
	}, nil
}

// SanitizePlainChangelog strips control characters and bounds changelog text to <= 2048 runes.
func SanitizePlainChangelog(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(minInt(len(s), maxChangelogChars))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\t' || (c >= 0x20 && c != 0x7f) {
			b.WriteByte(c)
		}
	}
	return truncateUTF8(strings.TrimSpace(b.String()), maxChangelogChars)
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ResolveLocalizedAny extracts a string for locale from either a plain string or a map[string]any / map[string]string.
func ResolveLocalizedAny(v any, locale string) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]string:
		return resolveLocalizedMap(x, locale)
	case map[string]any:
		m := make(map[string]string, len(x))
		for k, val := range x {
			if s, ok := val.(string); ok {
				m[k] = s
			}
		}
		return resolveLocalizedMap(m, locale)
	default:
		return ""
	}
}

func resolveLocalizedMap(m map[string]string, locale string) string {
	if len(m) == 0 {
		return ""
	}
	norm := strings.ReplaceAll(strings.TrimSpace(locale), "_", "-")
	if norm != "" {
		if v := strings.TrimSpace(m[norm]); v != "" {
			return v
		}
		for k, v := range m {
			if strings.EqualFold(strings.ReplaceAll(k, "_", "-"), norm) && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		base := strings.ToLower(strings.Split(norm, "-")[0])
		for k, v := range m {
			if strings.ToLower(strings.Split(strings.ReplaceAll(k, "_", "-"), "-")[0]) == base && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	for _, fb := range []string{"zh-CN", "en-US", "en", "zh-TW"} {
		if v := strings.TrimSpace(m[fb]); v != "" {
			return v
		}
	}
	for _, v := range m {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

type cachedURLBody struct {
	etag         string
	lastModified string
	body         []byte
	fetchedAt    time.Time
}

type urlCall struct {
	done chan struct{}
	body []byte
	err  error
}

// MetadataFetcher executes bounded, SSRF-safe declarative update metadata requests with
// URL-level singleflight coalescing and ETag / Last-Modified conditional 304 caching.
type MetadataFetcher struct {
	mu       sync.Mutex
	cache    map[string]cachedURLBody
	order    []string
	inflight map[string]*urlCall
}

// NewMetadataFetcher creates a bounded MetadataFetcher.
func NewMetadataFetcher() *MetadataFetcher {
	return &MetadataFetcher{
		cache:    make(map[string]cachedURLBody),
		inflight: make(map[string]*urlCall),
	}
}

// FetchRaw retrieves a remote JSON or checksum document (<= 128 KiB), coalescing concurrent identical URL reads
// with an isolated leader context so an impatient caller canceling ctx never aborts concurrent callers.
func (f *MetadataFetcher) FetchRaw(ctx context.Context, baseDialer *security.OutboundSafeDialer, rawURL string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dialer := PublicCloneDialer(baseDialer)
	u, err := ValidatePublicHTTPURL(rawURL, dialer.AllowLoopbackForTests)
	if err != nil {
		return nil, err
	}
	canonURL := u.String()

	f.mu.Lock()
	call, active := f.inflight[canonURL]
	if !active {
		prev, hasCached := f.cache[canonURL]
		call = &urlCall{done: make(chan struct{})}
		f.inflight[canonURL] = call
		go func(prev cachedURLBody, hasCached bool) {
			leaderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaderFetchTimeout)
			defer cancel()

			body, fetchErr := f.doFetch(leaderCtx, dialer, canonURL, prev, hasCached)

			f.mu.Lock()
			call.body = body
			call.err = fetchErr
			delete(f.inflight, canonURL)
			close(call.done)
			f.mu.Unlock()
		}(prev, hasCached)
	}
	f.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		return call.body, call.err
	}
}

func (f *MetadataFetcher) doFetch(
	ctx context.Context,
	dialer *security.OutboundSafeDialer,
	canonURL string,
	prev cachedURLBody,
	hasCached bool,
) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, leaderFetchTimeout)
	defer cancel()

	client := dialer.NewHTTPClient(leaderFetchTimeout)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, canonURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultDownloadUserAgt)
	req.Header.Set("Accept", "application/vnd.github+json, application/json, text/plain;q=0.8, */*;q=0.5")
	if hasCached {
		if prev.etag != "" {
			req.Header.Set("If-None-Match", prev.etag)
		}
		if prev.lastModified != "" {
			req.Header.Set("If-Modified-Since", prev.lastModified)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && hasCached && len(prev.body) > 0 {
		f.mu.Lock()
		prev.fetchedAt = time.Now()
		f.cache[canonURL] = prev
		f.mu.Unlock()
		return prev.body, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("metadata endpoint returned HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxMetadataBodyBytes {
		f.evictCache(canonURL)
		return nil, fmt.Errorf("metadata response exceeds %d KiB limit", maxMetadataBodyBytes>>10)
	}

	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	lastMod := strings.TrimSpace(resp.Header.Get("Last-Modified"))
	if (etag != "" || lastMod != "") && len(raw) <= 64<<10 {
		f.storeCache(canonURL, cachedURLBody{
			etag:         etag,
			lastModified: lastMod,
			body:         raw,
			fetchedAt:    time.Now(),
		})
	} else {
		f.evictCache(canonURL)
	}
	return raw, nil
}

func (f *MetadataFetcher) storeCache(urlKey string, entry cachedURLBody) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.cache[urlKey]; !exists {
		if len(f.order) >= maxCachedURLResources {
			oldest := f.order[0]
			f.order = f.order[1:]
			delete(f.cache, oldest)
		}
		f.order = append(f.order, urlKey)
	}
	f.cache[urlKey] = entry
}

func (f *MetadataFetcher) evictCache(urlKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.cache[urlKey]; !exists {
		return
	}
	delete(f.cache, urlKey)
	for i, k := range f.order {
		if k == urlKey {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
}

// ResolveCatalogChecksum fetches a standard SHA256SUMS.txt manifest via fetcher.FetchRaw,
// parses `<64-hex>  [* ]<path/or/filename>` lines, matches candidateNames (or URL path basenames),
// validates the 64-character hex digest, and returns the lowercase SHA-256 string.
func ResolveCatalogChecksum(
	ctx context.Context,
	fetcher *MetadataFetcher,
	dialer *security.OutboundSafeDialer,
	checksumURL string,
	candidateNames ...string,
) (string, error) {
	cleanURL := SanitizeHTTPURL(checksumURL)
	if cleanURL == "" {
		return "", errors.New("extpkg: empty or invalid checksum_url")
	}
	if fetcher == nil {
		fetcher = NewMetadataFetcher()
	}

	var candidates []string
	seenCand := make(map[string]struct{}, len(candidateNames))
	for _, rawCand := range candidateNames {
		c := strings.TrimSpace(rawCand)
		if c == "" {
			continue
		}
		if strings.Contains(c, "://") {
			if u, uErr := url.Parse(c); uErr == nil && u.Path != "" {
				c = u.Path
			}
		}
		c = strings.ReplaceAll(c, "\\", "/")
		base := strings.TrimSpace(path.Base(c))
		if base == "" || base == "." || base == "/" {
			continue
		}
		low := strings.ToLower(base)
		if _, dup := seenCand[low]; !dup {
			seenCand[low] = struct{}{}
			candidates = append(candidates, base)
		}
	}
	if len(candidates) == 0 {
		return "", errors.New("extpkg: no candidate asset names provided for checksum lookup")
	}

	raw, err := fetcher.FetchRaw(ctx, dialer, cleanURL)
	if err != nil {
		return "", fmt.Errorf("extpkg: fetch checksum file: %w", err)
	}

	exactMap := make(map[string]string)
	lowerMap := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || len(line) < 66 {
			continue
		}
		sep := strings.IndexAny(line, " \t")
		if sep != 64 {
			continue
		}
		digest := line[:64]
		if !hexSHA256Re.MatchString(digest) {
			continue
		}
		rest := strings.TrimLeft(line[64:], " \t")
		if strings.HasPrefix(rest, "*") {
			rest = rest[1:]
		}
		rest = strings.TrimSpace(strings.ReplaceAll(rest, "\\", "/"))
		if rest == "" {
			continue
		}
		base := path.Base(rest)
		if base == "" || base == "." || base == "/" {
			continue
		}
		lowDigest := strings.ToLower(digest)
		if _, ok := exactMap[base]; !ok {
			exactMap[base] = lowDigest
		}
		lowBase := strings.ToLower(base)
		if _, ok := lowerMap[lowBase]; !ok {
			lowerMap[lowBase] = lowDigest
		}
	}

	for _, cand := range candidates {
		if sum, ok := exactMap[cand]; ok {
			return sum, nil
		}
	}
	for _, cand := range candidates {
		if sum, ok := lowerMap[strings.ToLower(cand)]; ok {
			return sum, nil
		}
	}
	return "", fmt.Errorf("extpkg: no sha256 entry matching %v in checksum manifest", candidates)
}

// CheckDeclarative evaluates a declarative UpdateConfig (volo-catalog | manifest-json | github-release)
// entirely in native Go without allocating any JavaScript VM.
func (f *MetadataFetcher) CheckDeclarative(
	ctx context.Context,
	baseDialer *security.OutboundSafeDialer,
	extID string,
	cfg UpdateConfig,
	locale string,
) (UpdateCheckResult, error) {
	raw, err := f.FetchRaw(ctx, baseDialer, cfg.URL)
	if err != nil {
		return UpdateCheckResult{}, err
	}
	return EvaluateDeclarativeUpdate(raw, extID, cfg, locale)
}

// EvaluateDeclarativeUpdate parses declarative update metadata bytes for the configured strategy
// and sanitizes returned HTTP(S) URLs.
func EvaluateDeclarativeUpdate(raw []byte, extID string, cfg UpdateConfig, locale string) (UpdateCheckResult, error) {
	var (
		res UpdateCheckResult
		err error
	)
	switch cfg.Strategy {
	case StrategyVoloCatalog:
		res, err = parseCatalogResult(raw, extID, locale)
	case StrategyManifestJSON:
		res, err = parseManifestJSONResult(raw, extID, locale)
	case StrategyGitHubRelease:
		res, err = parseGitHubReleaseResult(raw, extID, cfg, locale)
	default:
		return UpdateCheckResult{}, fmt.Errorf("unsupported declarative strategy %q", cfg.Strategy)
	}
	if err != nil {
		return UpdateCheckResult{}, err
	}
	res.DownloadURL = SanitizeHTTPURL(res.DownloadURL)
	res.ChecksumURL = SanitizeHTTPURL(res.ChecksumURL)
	res.ReleaseURL = SanitizeHTTPURL(res.ReleaseURL)
	return res, nil
}

// CatalogItem represents an official or third-party catalog entry in plugins/catalog.json, themes/catalog.json, or locales/catalog.json.
type CatalogItem struct {
	ID                   string  `json:"id"`
	Version              string  `json:"version"`
	MinHostVersion       string  `json:"min_host_version,omitempty"`
	Author               string  `json:"author,omitempty"`
	Runtime              string  `json:"runtime,omitempty"`
	Distribution         string  `json:"distribution,omitempty"`
	ReleaseTag           string  `json:"release_tag,omitempty"`
	ReleaseAsset         string  `json:"release_asset,omitempty"`
	ReleaseAssetAlias    string  `json:"release_asset_alias,omitempty"`
	DownloadURL          string  `json:"download_url,omitempty"`
	ChecksumURL          string  `json:"checksum_url,omitempty"`
	SHA256               string  `json:"sha256,omitempty"`
	ReleaseURL           string  `json:"release_url,omitempty"`
	Homepage             string  `json:"homepage,omitempty"`
	OnlineUpdatable      *bool   `json:"online_updatable,omitempty"`
	Name                 any     `json:"name,omitempty"`
	Summary              any     `json:"summary,omitempty"`
	Description          any     `json:"description,omitempty"`
	Changelog            any     `json:"changelog,omitempty"`
	PublishedAt          string  `json:"published_at,omitempty"`
	ApproxZipMB          float64 `json:"approx_zip_mb,omitempty"`
	ApproxSizeBytes      int64   `json:"approx_size_bytes,omitempty"`
	BundledInCoreArchive bool    `json:"bundled_in_core_archive,omitempty"`
	BundledInDockerImage bool    `json:"bundled_in_docker_image,omitempty"`
}

// ParseCatalogItems parses items from { "plugins": [...], "themes": [...], "items": [...], "repository": "..." }.
func ParseCatalogItems(raw []byte) (repo string, items []CatalogItem, err error) {
	var doc struct {
		Repository string        `json:"repository"`
		Plugins    []CatalogItem `json:"plugins"`
		Themes     []CatalogItem `json:"themes"`
		Locales    []CatalogItem `json:"locales"`
		Items      []CatalogItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", nil, fmt.Errorf("invalid catalog json: %w", err)
	}
	out := make([]CatalogItem, 0, len(doc.Plugins)+len(doc.Themes)+len(doc.Locales)+len(doc.Items))
	out = append(out, doc.Plugins...)
	out = append(out, doc.Themes...)
	out = append(out, doc.Locales...)
	out = append(out, doc.Items...)
	for i := range out {
		out[i].DownloadURL = SanitizeHTTPURL(out[i].DownloadURL)
		out[i].ChecksumURL = SanitizeHTTPURL(out[i].ChecksumURL)
		out[i].ReleaseURL = SanitizeHTTPURL(out[i].ReleaseURL)
		out[i].Homepage = SanitizeHTTPURL(out[i].Homepage)
	}
	return strings.TrimSpace(doc.Repository), out, nil
}

// ResolveCatalogReleaseURL resolves the canonical human-readable release page URL for a catalog entry.
func ResolveCatalogReleaseURL(repo string, it CatalogItem) string {
	relURL := SanitizeHTTPURL(it.ReleaseURL)
	if relURL == "" && repo != "" && strings.TrimSpace(it.ReleaseTag) != "" {
		cleanRepo := strings.TrimRight(strings.TrimSpace(repo), "/")
		low := strings.ToLower(cleanRepo)
		var built string
		if strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://") {
			built = cleanRepo + "/releases/tag/" + strings.TrimSpace(it.ReleaseTag)
		} else {
			built = fmt.Sprintf("https://github.com/%s/releases/tag/%s", strings.Trim(cleanRepo, "/"), strings.TrimSpace(it.ReleaseTag))
		}
		relURL = SanitizeHTTPURL(built)
	}
	if relURL == "" {
		relURL = SanitizeHTTPURL(it.Homepage)
	}
	return relURL
}

func parseCatalogResult(raw []byte, extID string, locale string) (UpdateCheckResult, error) {
	repo, items, err := ParseCatalogItems(raw)
	if err != nil {
		return UpdateCheckResult{}, err
	}
	for _, it := range items {
		if strings.TrimSpace(it.ID) != extID {
			continue
		}
		relURL := ResolveCatalogReleaseURL(repo, it)
		return UpdateCheckResult{
			LatestVersion:   strings.TrimSpace(it.Version),
			OnlineUpdatable: it.OnlineUpdatable,
			MinHostVersion:  strings.TrimSpace(it.MinHostVersion),
			DownloadURL:     SanitizeHTTPURL(it.DownloadURL),
			ChecksumURL:     SanitizeHTTPURL(it.ChecksumURL),
			SHA256:          strings.TrimSpace(it.SHA256),
			ReleaseURL:      relURL,
			Changelog:       ResolveLocalizedAny(it.Changelog, locale),
			PublishedAt:     strings.TrimSpace(it.PublishedAt),
			ApproxSizeBytes: it.ApproxSizeBytes,
		}, nil
	}
	return UpdateCheckResult{}, fmt.Errorf("id %q not found in remote catalog", extID)
}

func parseManifestJSONResult(raw []byte, extID string, locale string) (UpdateCheckResult, error) {
	var doc struct {
		ID              string `json:"id"`
		Version         string `json:"version"`
		LatestVersion   string `json:"latest_version"`
		MinHostVersion  string `json:"min_host_version"`
		DownloadURL     string `json:"download_url"`
		ChecksumURL     string `json:"checksum_url"`
		SHA256          string `json:"sha256"`
		ReleaseURL      string `json:"release_url"`
		Homepage        string `json:"homepage"`
		OnlineUpdatable *bool  `json:"online_updatable"`
		Changelog       any    `json:"changelog"`
		PublishedAt     string `json:"published_at"`
		ApproxSizeBytes int64  `json:"approx_size_bytes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return UpdateCheckResult{}, fmt.Errorf("invalid remote manifest json: %w", err)
	}
	if docID := strings.TrimSpace(doc.ID); docID != "" && docID != extID {
		return UpdateCheckResult{}, fmt.Errorf("remote manifest id %q does not match installed %q", docID, extID)
	}
	ver := strings.TrimSpace(doc.LatestVersion)
	if ver == "" {
		ver = strings.TrimSpace(doc.Version)
	}
	relURL := SanitizeHTTPURL(doc.ReleaseURL)
	if relURL == "" {
		relURL = SanitizeHTTPURL(doc.Homepage)
	}
	return UpdateCheckResult{
		LatestVersion:   ver,
		OnlineUpdatable: doc.OnlineUpdatable,
		MinHostVersion:  strings.TrimSpace(doc.MinHostVersion),
		DownloadURL:     SanitizeHTTPURL(doc.DownloadURL),
		ChecksumURL:     SanitizeHTTPURL(doc.ChecksumURL),
		SHA256:          strings.TrimSpace(doc.SHA256),
		ReleaseURL:      relURL,
		Changelog:       ResolveLocalizedAny(doc.Changelog, locale),
		PublishedAt:     strings.TrimSpace(doc.PublishedAt),
		ApproxSizeBytes: doc.ApproxSizeBytes,
	}, nil
}

type ghReleaseObj struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Digest             string `json:"digest"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

func parseGitHubReleaseResult(raw []byte, extID string, cfg UpdateConfig, locale string) (UpdateCheckResult, error) {
	trimmed := strings.TrimSpace(string(raw))
	var chosen *ghReleaseObj
	if strings.HasPrefix(trimmed, "[") {
		var list []ghReleaseObj
		if err := json.Unmarshal(raw, &list); err != nil {
			return UpdateCheckResult{}, fmt.Errorf("invalid github releases list: %w", err)
		}
		for i := range list {
			r := &list[i]
			if r.Draft || r.Prerelease {
				continue
			}
			if cfg.TagPrefix != "" && !strings.HasPrefix(r.TagName, cfg.TagPrefix) {
				continue
			}
			chosen = r
			break
		}
	} else {
		var single ghReleaseObj
		if err := json.Unmarshal(raw, &single); err != nil {
			return UpdateCheckResult{}, fmt.Errorf("invalid github release json: %w", err)
		}
		chosen = &single
	}
	if chosen == nil || strings.TrimSpace(chosen.TagName) == "" {
		return UpdateCheckResult{}, errors.New("no matching github release found")
	}

	ver := strings.TrimSpace(chosen.TagName)
	if cfg.TagPrefix != "" && strings.HasPrefix(ver, cfg.TagPrefix) {
		ver = strings.TrimPrefix(ver, cfg.TagPrefix)
	}
	ver = NormalizeVersion(ver)

	pat := strings.TrimSpace(cfg.AssetPattern)
	if pat != "" {
		pat = strings.ReplaceAll(pat, "{id}", extID)
		pat = strings.ReplaceAll(pat, "{version}", ver)
	}

	var dlURL, csURL, sha string
	var approxSize int64
	for _, a := range chosen.Assets {
		name := strings.TrimSpace(a.Name)
		lowName := strings.ToLower(name)
		if lowName == "sha256sums.txt" || lowName == "sha256sums" {
			csURL = SanitizeHTTPURL(a.BrowserDownloadURL)
			continue
		}
		if dlURL != "" {
			continue
		}
		if !strings.HasSuffix(lowName, ".zip") {
			continue
		}
		if pat != "" && !strings.EqualFold(name, pat) {
			continue
		}
		dlURL = SanitizeHTTPURL(a.BrowserDownloadURL)
		approxSize = a.Size
		if sum, err := NormalizeSHA256(a.Digest); err == nil {
			sha = sum
		}
	}

	return UpdateCheckResult{
		LatestVersion:   ver,
		DownloadURL:     dlURL,
		ChecksumURL:     csURL,
		SHA256:          sha,
		ReleaseURL:      SanitizeHTTPURL(chosen.HTMLURL),
		Changelog:       chosen.Body,
		PublishedAt:     strings.TrimSpace(chosen.PublishedAt),
		ApproxSizeBytes: approxSize,
	}, nil
}
