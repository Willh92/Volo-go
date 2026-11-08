package plugin

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/pkg/id"
)

const (
	MaxZipArchiveBytes = 32 << 20 // 32 MiB max ZIP archive or total extracted size
	MaxZipSingleBytes  = 8 << 20  // 8 MiB max single extracted file
	MaxZipEntries      = 256

	DefaultHookTimeout  = 500 * time.Millisecond
	DefaultRouteTimeout = 3 * time.Second
	DefaultCronTimeout  = 15 * time.Second
	DefaultMaxFailures  = 5
)

// HookRejectedError signals an intentional business-rule rejection returned by a pre-action hook (e.g. comment.before_create).
type HookRejectedError struct {
	PluginID string
	Reason   string
}

func (e *HookRejectedError) Error() string {
	if strings.TrimSpace(e.Reason) != "" {
		return e.Reason
	}
	return "rejected by plugin " + e.PluginID
}

// APIRequest represents a sanitized HTTP request routed to /plugins/<id>/api/*.
type APIRequest struct {
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Query          map[string]string `json:"query"`
	Headers        map[string]string `json:"headers"`
	Body           string            `json:"body"`
	Locale         string            `json:"locale"`
	ClientIPMasked string            `json:"client_ip_masked"`
	IsAdmin        bool              `json:"is_admin"`
}

// APIResponse represents a plugin's HTTP response from /plugins/<id>/api/*.
type APIResponse struct {
	Status      int               `json:"status"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body"`
}

// SlotRenderContext carries sanitized page metadata into UI slot renderers.
type SlotRenderContext struct {
	Locale      string `json:"locale"`
	Path        string `json:"path"`
	ArticleID   string `json:"article_id,omitempty"`
	ArticleSlug string `json:"article_slug,omitempty"`
	ThemeAccent string `json:"theme_accent,omitempty"`
}

// RenderedSlot represents a single rendered UI injection fragment.
type RenderedSlot struct {
	PluginID string `json:"plugin_id"`
	Slot     string `json:"slot"`
	Persist  bool   `json:"persist"`
	HTML     string `json:"html"`
}

// ArticleHookPayload carries article event data to plugin hooks.
// Untrusted/external plugins ALWAYS receive a redacted copy via RedactArticleForPlugin.
type ArticleHookPayload struct {
	ID                  string `json:"id"`
	Title               string `json:"title"`
	Slug                string `json:"slug"`
	Lang                string `json:"lang"`
	Status              string `json:"status,omitempty"`
	SummaryMD           string `json:"summary_md"`
	ContentMD           string `json:"content_md"`
	HTML                string `json:"html,omitempty"`
	CoverURL            string `json:"cover_url,omitempty"`
	AudioURL            string `json:"audio_url,omitempty"`
	IsPasswordProtected bool   `json:"is_password_protected"`
	HasContentLock      bool   `json:"has_content_lock"`
	OmittedProtected    bool   `json:"omitted_protected,omitempty"`
}

// PageHookPayload carries standalone page lifecycle event data to plugin hooks.
type PageHookPayload struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Slug     string `json:"slug"`
	Template string `json:"template,omitempty"`
	Status   string `json:"status,omitempty"`
}

// CommentModeratedHookPayload carries comment moderation status transitions to plugin hooks.
type CommentModeratedHookPayload struct {
	ID         string `json:"id"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Status     string `json:"status"`
}

// BuiltinPlugin is implemented by compiled-in Go plugins (zero goja overhead).
type BuiltinPlugin interface {
	Manifest() Manifest
	OnEnable(ctx context.Context, host *HostContext) error
	OnDisable(ctx context.Context) error
	OnHook(ctx context.Context, hookName string, payload any) (any, error)
	RenderSlot(ctx context.Context, slotName string, slotCtx SlotRenderContext) (string, error)
	ServeAPI(ctx context.Context, req APIRequest) (APIResponse, error)
	RunCron(ctx context.Context, handler string) error
	AllowProtectedArticles() bool
}

// BaseBuiltinPlugin provides no-op defaults so compiled Go plugins only implement what they need.
type BaseBuiltinPlugin struct{}

func (BaseBuiltinPlugin) OnEnable(context.Context, *HostContext) error { return nil }
func (BaseBuiltinPlugin) OnDisable(context.Context) error              { return nil }
func (BaseBuiltinPlugin) OnHook(_ context.Context, _ string, payload any) (any, error) {
	return payload, nil
}
func (BaseBuiltinPlugin) RenderSlot(context.Context, string, SlotRenderContext) (string, error) {
	return "", nil
}
func (BaseBuiltinPlugin) ServeAPI(context.Context, APIRequest) (APIResponse, error) {
	return APIResponse{Status: http.StatusNotFound, ContentType: "application/json", Body: `{"error":"not found"}`}, nil
}
func (BaseBuiltinPlugin) RunCron(context.Context, string) error { return nil }
func (BaseBuiltinPlugin) AllowProtectedArticles() bool          { return false }

// HostContext exposes capability-scoped host services to a BuiltinPlugin.
type HostContext struct {
	PluginID  string
	KV        *KVStore
	Providers *ProviderRegistry
	logger    *PluginLogger
	hostI18n  *i18n.Catalog
	getConfig func() map[string]any
	dialer    *security.OutboundSafeDialer
}

func (h *HostContext) Config() map[string]any {
	cfg := h.getConfig()
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out
}
func (h *HostContext) LogInfo(event, msg string)  { h.logger.Write(h.PluginID, "info", event, msg) }
func (h *HostContext) LogWarn(event, msg string)  { h.logger.Write(h.PluginID, "warn", event, msg) }
func (h *HostContext) LogError(event, msg string) { h.logger.Write(h.PluginID, "error", event, msg) }
func (h *HostContext) T(locale, key string, vars ...map[string]any) string {
	if h.hostI18n == nil {
		return key
	}
	return h.hostI18n.T(locale, key, vars...)
}
func (h *HostContext) NewHTTPClient(timeout time.Duration) *http.Client {
	return h.dialer.NewHTTPClient(timeout)
}

// PluginSummary is the safe, secret-masked view returned to Admin REST APIs.
type PluginSummary struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind"` // builtin | external
	Runtime       string          `json:"runtime"`
	Version       string          `json:"version"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Author        string          `json:"author,omitempty"`
	Homepage      string          `json:"homepage,omitempty"`
	IsEnabled     bool            `json:"is_enabled"`
	CircuitState  string          `json:"circuit_state"`
	FailureCount  int             `json:"failure_count"`
	LastError     string          `json:"last_error,omitempty"`
	Permissions   []string        `json:"permissions"`
	AllowedHosts  []string        `json:"allowed_hosts"`
	ConfigSchema  []ConfigField   `json:"config_schema"`
	PublicConfig  map[string]any  `json:"public_config"`
	SecretKeysSet map[string]bool `json:"secret_keys_set"`
	InstalledAt   int64           `json:"installed_at"`
	UpdatedAt     int64           `json:"updated_at"`
}

// EditorQuickInsertView describes a localized quick-insert button for CodeMirror 6.
type EditorQuickInsertView struct {
	PluginID string `json:"plugin_id"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	Icon     string `json:"icon,omitempty"`
	Template string `json:"template"`
}

// ManagerOptions configures the Plugin Manager.
type ManagerOptions struct {
	Store                 *sqlx.Store
	Keys                  *config.KeyRing
	PluginsDir            string
	DataDir               string
	Catalog               *i18n.Catalog
	Providers             *ProviderRegistry
	HookTimeout           time.Duration
	RouteTimeout          time.Duration
	CronTimeout           time.Duration
	MaxFailures           int
	CustomResolver        security.ResolverFunc
	AllowLoopbackForTests bool
}

type managedPlugin struct {
	mu           sync.Mutex
	manifest     Manifest
	kind         string // "builtin" | "external"
	pluginDir    string
	builtin      BuiltinPlugin
	gojaRT       *gojaPluginRuntime
	kv           *KVStore
	enabled      bool
	circuitState string
	failureCount int
	lastError    string
	installedAt  int64
	updatedAt    int64

	publicConfig  map[string]any
	secretConfig  map[string]string
	mergedCfgSnap atomic.Pointer[map[string]any]

	cronCancel context.CancelFunc
	cronWG     sync.WaitGroup
}

func (mp *managedPlugin) rebuildConfigSnapshot() {
	combined := make(map[string]any, len(mp.publicConfig)+len(mp.secretConfig))
	for k, v := range mp.publicConfig {
		combined[k] = v
	}
	for k, v := range mp.secretConfig {
		combined[k] = v
	}
	mp.mergedCfgSnap.Store(&combined)
}

func (mp *managedPlugin) getConfigMap() map[string]any {
	ptr := mp.mergedCfgSnap.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// Manager orchestrates built-in and external goja plugins, persistent plugin_kv, capability sandboxes,
// automatic circuit breaking, .zip hot installation, cron scheduling, and /plugins/<id>/* HTTP serving.
type Manager struct {
	mu                    sync.RWMutex
	store                 *sqlx.Store
	keys                  *config.KeyRing
	pluginsDir            string
	logger                *PluginLogger
	catalog               *i18n.Catalog
	providers             *ProviderRegistry
	hookTimeout           time.Duration
	routeTimeout          time.Duration
	cronTimeout           time.Duration
	cronBaseUnit          time.Duration
	maxFailures           int
	customResolver        security.ResolverFunc
	allowLoopbackForTests bool

	siteInfoFn   atomic.Pointer[func() map[string]any]
	reqInspector atomic.Pointer[func(*http.Request) (bool, string)]

	plugins map[string]*managedPlugin
	closed  bool
}

// NewManager constructs a Plugin Manager.
func NewManager(opts ManagerOptions) *Manager {
	providers := opts.Providers
	if providers == nil {
		providers = NewProviderRegistry(nil, nil)
	}
	hookTimeout := opts.HookTimeout
	if hookTimeout <= 0 {
		hookTimeout = DefaultHookTimeout
	}
	routeTimeout := opts.RouteTimeout
	if routeTimeout <= 0 {
		routeTimeout = DefaultRouteTimeout
	}
	cronTimeout := opts.CronTimeout
	if cronTimeout <= 0 {
		cronTimeout = DefaultCronTimeout
	}
	maxFailures := opts.MaxFailures
	if maxFailures <= 0 {
		maxFailures = DefaultMaxFailures
	}
	if opts.PluginsDir != "" {
		_ = os.MkdirAll(opts.PluginsDir, 0o750)
	}
	return &Manager{
		store:                 opts.Store,
		keys:                  opts.Keys,
		pluginsDir:            opts.PluginsDir,
		logger:                NewPluginLogger(opts.DataDir),
		catalog:               opts.Catalog,
		providers:             providers,
		hookTimeout:           hookTimeout,
		routeTimeout:          routeTimeout,
		cronTimeout:           cronTimeout,
		cronBaseUnit:          time.Second,
		maxFailures:           maxFailures,
		customResolver:        opts.CustomResolver,
		allowLoopbackForTests: opts.AllowLoopbackForTests,
		plugins:               make(map[string]*managedPlugin),
	}
}

func (m *Manager) Providers() *ProviderRegistry { return m.providers }
func (m *Manager) Logger() *PluginLogger        { return m.logger }

// SetSiteInfoFunc registers a host callback returning sanitized site metadata for volo.site.info().
func (m *Manager) SetSiteInfoFunc(fn func() map[string]any) {
	if fn == nil {
		m.siteInfoFn.Store(nil)
		return
	}
	m.siteInfoFn.Store(&fn)
}

func (m *Manager) getSiteInfo() map[string]any {
	ptr := m.siteInfoFn.Load()
	if ptr == nil || *ptr == nil {
		return map[string]any{}
	}
	return (*ptr)()
}

// SetRequestInspector registers a host callback resolving (isAdmin, clientIPMasked) on /plugins/<id>/api/* requests.
func (m *Manager) SetRequestInspector(fn func(r *http.Request) (isAdmin bool, clientIPMasked string)) {
	if fn == nil {
		m.reqInspector.Store(nil)
		return
	}
	m.reqInspector.Store(&fn)
}

func (m *Manager) inspectRequest(r *http.Request) (bool, string) {
	if ptr := m.reqInspector.Load(); ptr != nil && *ptr != nil {
		return (*ptr)(r)
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if addr, pErr := netip.ParseAddr(host); pErr == nil {
		return false, security.MaskIP(addr)
	}
	return false, ""
}

// RegisterBuiltin registers a compiled-in Go plugin prior to or after DiscoverAndStart.
func (m *Manager) RegisterBuiltin(ctx context.Context, bp BuiltinPlugin) error {
	manifest := bp.Manifest()
	manifest.Runtime = RuntimeBuiltin
	if err := manifest.Validate(); err != nil {
		return err
	}
	mp, err := m.syncRecordWithDB(ctx, manifest, "builtin", "", bp)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.plugins[manifest.ID] = mp
	m.mu.Unlock()

	if mp.enabled && mp.circuitState == CircuitClosed {
		return m.activateRuntime(ctx, mp)
	}
	return nil
}

// DiscoverAndStart scans pluginsDir for external plugins (<pluginsDir>/<id>/manifest.json),
// reconciles state with the `plugins` table, and activates newly discovered enabled plugins.
// Calling DiscoverAndStart multiple times is idempotent and never overwrites active runtimes or cron goroutines.
func (m *Manager) DiscoverAndStart(ctx context.Context) error {
	if strings.TrimSpace(m.pluginsDir) == "" {
		return nil
	}
	entries, err := os.ReadDir(m.pluginsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("plugin: read plugins dir %q: %w", m.pluginsDir, err)
	}

	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		if _, alreadyLoaded := m.getPlugin(ent.Name()); alreadyLoaded {
			continue
		}
		dir := filepath.Join(m.pluginsDir, ent.Name())
		manifestBytes, rErr := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if rErr != nil {
			continue
		}
		manifest, pErr := ParseManifest(manifestBytes)
		if pErr != nil {
			m.logger.Write(ent.Name(), "error", "discover", pErr.Error())
			continue
		}
		if manifest.ID != ent.Name() {
			m.logger.Write(ent.Name(), "error", "discover", "directory name does not match manifest.id")
			continue
		}
		if _, alreadyLoaded := m.getPlugin(manifest.ID); alreadyLoaded {
			continue
		}
		mp, sErr := m.syncRecordWithDB(ctx, manifest, "external", dir, nil)
		if sErr != nil {
			m.logger.Write(manifest.ID, "error", "discover", sErr.Error())
			continue
		}
		m.mu.Lock()
		if _, exists := m.plugins[manifest.ID]; exists {
			m.mu.Unlock()
			continue
		}
		m.plugins[manifest.ID] = mp
		m.mu.Unlock()

		if mp.enabled && mp.circuitState == CircuitClosed {
			if aErr := m.activateRuntime(ctx, mp); aErr != nil {
				m.recordFailure(ctx, mp, "activate", aErr)
			}
		}
	}
	return nil
}

// ReloadAll synchronizes every already-registered plugin with the database (e.g. after full-site DB restore),
// stops runtimes that became disabled, re-activates enabled plugins with updated config, and discovers any new plugins on disk.
func (m *Manager) ReloadAll(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.plugins))
	for id := range m.plugins {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sort.Strings(ids)

	for _, pid := range ids {
		existing, ok := m.getPlugin(pid)
		if !ok {
			continue
		}
		m.deactivateRuntime(ctx, existing)
		synced, err := m.syncRecordWithDB(ctx, existing.manifest, existing.kind, existing.pluginDir, existing.builtin)
		if err != nil {
			m.logger.Write(pid, "error", "reload_all", err.Error())
			continue
		}
		existing.mu.Lock()
		existing.enabled = synced.enabled
		existing.circuitState = synced.circuitState
		existing.failureCount = synced.failureCount
		existing.lastError = synced.lastError
		existing.installedAt = synced.installedAt
		existing.updatedAt = synced.updatedAt
		existing.publicConfig = synced.publicConfig
		existing.secretConfig = synced.secretConfig
		existing.rebuildConfigSnapshot()
		shouldActivate := existing.enabled && existing.circuitState == CircuitClosed
		existing.mu.Unlock()
		if shouldActivate {
			if aErr := m.activateRuntime(ctx, existing); aErr != nil {
				m.recordFailure(ctx, existing, "reload_all", aErr)
			}
		}
	}
	return m.DiscoverAndStart(ctx)
}

// ClearPluginKV deletes all key-value entries stored by pluginID in plugin_kv without bumping content dataRev.
func (m *Manager) ClearPluginKV(ctx context.Context, pluginID string) (int64, error) {
	if m == nil || m.store == nil {
		return 0, errors.New("plugin: store unavailable")
	}
	idClean := strings.TrimSpace(pluginID)
	if !validPluginIDRe.MatchString(idClean) {
		return 0, fmt.Errorf("plugin: invalid plugin id %q", pluginID)
	}
	if _, ok := m.getPlugin(idClean); !ok {
		return 0, fmt.Errorf("plugin %q not found", idClean)
	}
	res, err := m.store.ExecBgContext(ctx, `DELETE FROM plugin_kv WHERE plugin_id = ?`, idClean)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// HasPermission reports whether pluginID is registered and declares perm in its manifest.
func (m *Manager) HasPermission(pluginID string, perm string) bool {
	if m == nil {
		return false
	}
	mp, ok := m.getPlugin(strings.TrimSpace(pluginID))
	if !ok {
		return false
	}
	return mp.manifest.HasPermission(perm)
}

// ReloadPlugin cleanly stops an existing plugin runtime (including cron tickers and providers),
// re-reads its manifest and JS source from disk (for external plugins), and reactivates it if enabled.
func (m *Manager) ReloadPlugin(ctx context.Context, pluginID string, locale string) (PluginSummary, error) {
	idClean := strings.TrimSpace(pluginID)
	if !validPluginIDRe.MatchString(idClean) {
		return PluginSummary{}, fmt.Errorf("plugin: invalid plugin id %q", idClean)
	}

	existing, exists := m.getPlugin(idClean)
	if exists && existing.kind == "builtin" {
		m.deactivateRuntime(ctx, existing)
		existing.mu.Lock()
		shouldActivate := existing.enabled && existing.circuitState == CircuitClosed
		existing.mu.Unlock()
		if shouldActivate {
			if err := m.activateRuntime(ctx, existing); err != nil {
				m.recordFailure(ctx, existing, "reload", err)
				return m.toSummary(existing, locale), err
			}
		}
		return m.toSummary(existing, locale), nil
	}

	if strings.TrimSpace(m.pluginsDir) == "" {
		return PluginSummary{}, fmt.Errorf("plugin %q not found", idClean)
	}
	dir := filepath.Join(m.pluginsDir, idClean)
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return PluginSummary{}, fmt.Errorf("plugin %q: read manifest.json: %w", idClean, err)
	}
	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return PluginSummary{}, err
	}
	if manifest.ID != idClean {
		return PluginSummary{}, fmt.Errorf("plugin %q: directory name does not match manifest.id %q", idClean, manifest.ID)
	}

	if exists {
		m.deactivateRuntime(ctx, existing)
	}

	mp, err := m.syncRecordWithDB(ctx, manifest, "external", dir, nil)
	if err != nil {
		return PluginSummary{}, err
	}
	now := time.Now().Unix()
	mp.updatedAt = now
	_, _ = m.store.ExecContext(ctx, `UPDATE plugins SET version = ?, updated_at = ? WHERE id = ?`, manifest.Version, now, manifest.ID)

	m.mu.Lock()
	m.plugins[manifest.ID] = mp
	m.mu.Unlock()

	mp.mu.Lock()
	shouldActivate := mp.enabled && mp.circuitState == CircuitClosed
	mp.mu.Unlock()

	if shouldActivate {
		if aErr := m.activateRuntime(ctx, mp); aErr != nil {
			m.recordFailure(ctx, mp, "reload", aErr)
			return m.toSummary(mp, locale), aErr
		}
	}
	return m.toSummary(mp, locale), nil
}

func (m *Manager) syncRecordWithDB(
	ctx context.Context,
	manifest Manifest,
	kind string,
	pluginDir string,
	builtin BuiltinPlugin,
) (*managedPlugin, error) {
	now := time.Now().Unix()
	existing, err := sqlx.Get[domain.PluginRecord](
		ctx,
		m.store,
		`SELECT id, kind, version, is_enabled, config_json, secret_ciphertext, circuit_state, failure_count, last_error, installed_at, updated_at
		 FROM plugins WHERE id = ?`,
		manifest.ID,
	)

	var pubCfg map[string]any
	secretCfg := make(map[string]string)

	if errors.Is(err, sql.ErrNoRows) {
		defPub, defSec, _ := manifest.ValidateAndMergeConfig(nil, nil, nil)
		pubCfg = defPub
		secretCfg = defSec
		cfgBytes, _ := json.Marshal(pubCfg)
		secCipher, sErr := m.sealSecrets(manifest.ID, secretCfg)
		if sErr != nil {
			return nil, sErr
		}
		existing = domain.PluginRecord{
			ID:               manifest.ID,
			Kind:             kind,
			Version:          manifest.Version,
			IsEnabled:        false,
			ConfigJSON:       string(cfgBytes),
			SecretCiphertext: secCipher,
			CircuitState:     CircuitClosed,
			FailureCount:     0,
			LastError:        "",
			InstalledAt:      now,
			UpdatedAt:        now,
		}
		upsert := m.store.Dialect().UpsertSQL(
			"plugins",
			[]string{"id", "kind", "version", "is_enabled", "config_json", "secret_ciphertext", "circuit_state", "failure_count", "last_error", "installed_at", "updated_at"},
			[]string{"id"},
			[]string{"kind", "version", "updated_at"},
		)
		if _, eErr := m.store.ExecContext(
			ctx, upsert,
			existing.ID, existing.Kind, existing.Version, existing.IsEnabled,
			existing.ConfigJSON, existing.SecretCiphertext, existing.CircuitState,
			existing.FailureCount, existing.LastError, existing.InstalledAt, existing.UpdatedAt,
		); eErr != nil {
			return nil, eErr
		}
	} else if err != nil {
		return nil, err
	} else {
		var rawPub map[string]any
		_ = json.Unmarshal([]byte(existing.ConfigJSON), &rawPub)
		openedSec := m.openSecretsLenient(manifest.ID, existing.SecretCiphertext)
		defPub, defSec, vErr := manifest.ValidateAndMergeConfig(nil, rawPub, openedSec)
		if vErr == nil {
			pubCfg = defPub
			secretCfg = defSec
		} else {
			pubCfg = rawPub
			if pubCfg == nil {
				pubCfg = make(map[string]any)
			}
			secretCfg = openedSec
		}
		if existing.Version != manifest.Version {
			_, _ = m.store.ExecContext(
				ctx,
				`UPDATE plugins SET version = ?, kind = ?, updated_at = ? WHERE id = ?`,
				manifest.Version, kind, now, manifest.ID,
			)
			existing.Version = manifest.Version
		}
	}

	if existing.CircuitState == "" {
		existing.CircuitState = CircuitClosed
	}

	mp := &managedPlugin{
		manifest:     manifest,
		kind:         kind,
		pluginDir:    pluginDir,
		builtin:      builtin,
		kv:           NewKVStore(m.store, manifest.ID),
		enabled:      existing.IsEnabled,
		circuitState: existing.CircuitState,
		failureCount: existing.FailureCount,
		lastError:    existing.LastError,
		installedAt:  existing.InstalledAt,
		updatedAt:    existing.UpdatedAt,
		publicConfig: pubCfg,
		secretConfig: secretCfg,
	}
	mp.rebuildConfigSnapshot()
	return mp, nil
}

func (m *Manager) sealSecrets(pluginID string, secrets map[string]string) (string, error) {
	if len(secrets) == 0 || m.keys == nil {
		return "", nil
	}
	raw, err := json.Marshal(secrets)
	if err != nil {
		return "", err
	}
	return m.keys.SealString(string(raw), "plugins.secret_ciphertext:"+pluginID)
}

func (m *Manager) openSecretsLenient(pluginID string, ciphertext string) map[string]string {
	out := make(map[string]string)
	if ciphertext == "" || m.keys == nil {
		return out
	}
	plain, err := m.keys.OpenString(ciphertext, "plugins.secret_ciphertext:"+pluginID)
	if err != nil {
		m.logger.Write(pluginID, "warn", "secrets", "encrypted plugin secrets could not be opened (key changed); re-enter in admin console")
		return out
	}
	_ = json.Unmarshal([]byte(plain), &out)
	return out
}

func (m *Manager) activateRuntime(ctx context.Context, mp *managedPlugin) error {
	mp.mu.Lock()
	prevCancel := mp.cronCancel
	mp.cronCancel = nil
	mp.mu.Unlock()

	if prevCancel != nil {
		prevCancel()
	}
	mp.cronWG.Wait()

	mp.mu.Lock()
	defer mp.mu.Unlock()

	if mp.cronCancel != nil {
		mp.cronCancel()
		mp.cronCancel = nil
	}
	m.providers.DetachPlugin(mp.manifest.ID)

	if mp.manifest.Runtime == RuntimeBuiltin {
		if mp.builtin == nil {
			return fmt.Errorf("plugin %q: missing builtin implementation", mp.manifest.ID)
		}
		hostCtx := &HostContext{
			PluginID:  mp.manifest.ID,
			KV:        mp.kv,
			Providers: m.providers,
			logger:    m.logger,
			hostI18n:  m.catalog,
			getConfig: mp.getConfigMap,
			dialer: &security.OutboundSafeDialer{
				AllowedHosts:          mp.manifest.EffectiveAllowedHosts(),
				Timeout:               5 * time.Second,
				Resolver:              m.customResolver,
				AllowLoopbackForTests: m.allowLoopbackForTests,
			},
		}
		if err := mp.builtin.OnEnable(ctx, hostCtx); err != nil {
			m.providers.DetachPlugin(mp.manifest.ID)
			return err
		}
	} else {
		rt, err := newGojaPluginRuntime(gojaRuntimeOptions{
			manifest:              mp.manifest,
			pluginDir:             mp.pluginDir,
			kv:                    mp.kv,
			logger:                m.logger,
			hostI18n:              m.catalog,
			getConfig:             mp.getConfigMap,
			getSiteInfo:           m.getSiteInfo,
			customResolver:        m.customResolver,
			allowLoopbackForTests: m.allowLoopbackForTests,
		})
		if err != nil {
			return err
		}
		mp.gojaRT = rt

		// Optional JS lifecycle hook onEnable().
		if _, _, err := rt.invoke(ctx, m.hookTimeout, "onEnable"); err != nil {
			mp.gojaRT = nil
			return err
		}

		// Register JS providers if declared in permissions.
		if mp.manifest.HasPermission("provider:antispam") {
			m.providers.RegisterAntiSpam(mp.manifest.ID, &gojaAntiSpamAdapter{
				manager: m,
				mp:      mp,
			})
		}
		if mp.manifest.HasPermission("provider:notifier") {
			m.providers.RegisterNotifier(mp.manifest.ID, &gojaNotifierAdapter{
				manager: m,
				mp:      mp,
			})
		}
		if mp.manifest.HasPermission("provider:storage") {
			m.providers.RegisterStorage(mp.manifest.ID, &gojaStorageAdapter{
				manager: m,
				mp:      mp,
			})
		}
		if mp.manifest.HasPermission("provider:oidc") && rt.hasCallable("oidcAuthURL") && rt.hasCallable("oidcExchange") {
			adapter := newGojaOIDCAdapter(ctx, m, mp, rt)
			m.providers.RegisterOIDC(mp.manifest.ID, adapter)
		}
	}

	m.startCronLocked(mp)
	m.logger.Write(mp.manifest.ID, "info", "lifecycle", "plugin enabled v"+mp.manifest.Version)
	return nil
}

func (m *Manager) deactivateRuntime(ctx context.Context, mp *managedPlugin) {
	m.deactivateRuntimeOpts(ctx, mp, true)
}

func (m *Manager) deactivateRuntimeOpts(ctx context.Context, mp *managedPlugin, waitCron bool) {
	mp.mu.Lock()
	cancel := mp.cronCancel
	mp.cronCancel = nil
	mp.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// Never call mp.cronWG.Wait() while holding mp.mu.Lock() or from inside a cron goroutine itself!
	if waitCron {
		mp.cronWG.Wait()
	}

	mp.mu.Lock()
	m.providers.DetachPlugin(mp.manifest.ID)
	runtimeKind := mp.manifest.Runtime
	builtin := mp.builtin
	rt := mp.gojaRT
	mp.gojaRT = nil
	mp.mu.Unlock()

	if runtimeKind == RuntimeBuiltin && builtin != nil {
		_ = builtin.OnDisable(ctx)
	} else if rt != nil {
		_, _, _ = rt.invoke(ctx, m.hookTimeout, "onDisable")
	}
	m.logger.Write(mp.manifest.ID, "info", "lifecycle", "plugin disabled")
}

func (m *Manager) startCronLocked(mp *managedPlugin) {
	if len(mp.manifest.Cron) == 0 || !mp.manifest.HasPermission("cron") {
		return
	}
	cronCtx, cancel := context.WithCancel(context.Background())
	mp.cronCancel = cancel
	unit := m.cronBaseUnit
	if unit <= 0 {
		unit = time.Second
	}

	for _, spec := range mp.manifest.Cron {
		job := spec
		interval := time.Duration(job.IntervalSeconds) * unit
		if interval <= 0 {
			interval = time.Second
		}
		mp.cronWG.Add(1)
		go func() {
			defer mp.cronWG.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-cronCtx.Done():
					return
				case <-ticker.C:
					runCtx, runCancel := context.WithTimeout(cronCtx, m.cronTimeout)
					err := m.runSingleCron(runCtx, mp, job.Handler)
					runCancel()
					if err != nil && !errors.Is(err, context.Canceled) {
						m.recordFailureWithOrigin(context.Background(), mp, "cron:"+job.Name, err, true)
					} else if err == nil {
						m.recordSuccess(context.Background(), mp)
					}
				}
			}
		}()
	}
}

func (m *Manager) runSingleCron(ctx context.Context, mp *managedPlugin, handler string) error {
	mp.mu.Lock()
	enabled := mp.enabled && mp.circuitState == CircuitClosed
	rt := mp.gojaRT
	bp := mp.builtin
	runtimeKind := mp.manifest.Runtime
	mp.mu.Unlock()

	if !enabled {
		return nil
	}
	if runtimeKind == RuntimeBuiltin && bp != nil {
		return bp.RunCron(ctx, handler)
	}
	if rt != nil {
		_, _, err := rt.invoke(ctx, m.cronTimeout, handler)
		return err
	}
	return nil
}

// Enable enables a plugin (fail-closed if circuit is open until ResetCircuit is called, or auto-resets when requested).
func (m *Manager) Enable(ctx context.Context, pluginID string) error {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return fmt.Errorf("plugin: %q not found", pluginID)
	}

	mp.mu.Lock()
	if mp.circuitState == CircuitOpen {
		mp.mu.Unlock()
		return fmt.Errorf("plugin %q circuit breaker is OPEN (%s); reset circuit before enabling", pluginID, mp.lastError)
	}
	mp.enabled = true
	mp.updatedAt = time.Now().Unix()
	mp.mu.Unlock()

	if err := m.activateRuntime(ctx, mp); err != nil {
		mp.mu.Lock()
		mp.enabled = false
		mp.lastError = err.Error()
		mp.mu.Unlock()
		_, _ = m.store.ExecContext(
			ctx,
			`UPDATE plugins SET is_enabled = ?, last_error = ?, updated_at = ? WHERE id = ?`,
			false, err.Error(), time.Now().Unix(), pluginID,
		)
		return err
	}

	_, err := m.store.ExecContext(
		ctx,
		`UPDATE plugins SET is_enabled = ?, last_error = '', updated_at = ? WHERE id = ?`,
		true, time.Now().Unix(), pluginID,
	)
	return err
}

// Disable hot-stops a plugin immediately without restarting Volo-go.
func (m *Manager) Disable(ctx context.Context, pluginID string) error {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return fmt.Errorf("plugin: %q not found", pluginID)
	}
	mp.mu.Lock()
	mp.enabled = false
	mp.updatedAt = time.Now().Unix()
	mp.mu.Unlock()

	m.deactivateRuntime(ctx, mp)

	_, err := m.store.ExecContext(
		ctx,
		`UPDATE plugins SET is_enabled = ?, updated_at = ? WHERE id = ?`,
		false, time.Now().Unix(), pluginID,
	)
	return err
}

// ResetCircuit clears CIRCUIT_OPEN state and failure counters so an administrator can re-enable a repaired plugin.
func (m *Manager) ResetCircuit(ctx context.Context, pluginID string) error {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return fmt.Errorf("plugin: %q not found", pluginID)
	}
	now := time.Now().Unix()
	mp.mu.Lock()
	mp.circuitState = CircuitClosed
	mp.failureCount = 0
	mp.lastError = ""
	mp.updatedAt = now
	mp.mu.Unlock()

	m.logger.Write(pluginID, "info", "circuit", "circuit breaker manually reset to CLOSED")
	_, err := m.store.ExecContext(
		ctx,
		`UPDATE plugins SET circuit_state = ?, failure_count = 0, last_error = '', updated_at = ? WHERE id = ?`,
		CircuitClosed, now, pluginID,
	)
	return err
}

// UpdateConfig validates a partial/full config patch against ConfigSchema, seals secret fields with AES-256-GCM,
// persists to DB, and atomically refreshes the live in-memory config snapshot without restarting VMs.
func (m *Manager) UpdateConfig(ctx context.Context, pluginID string, patch map[string]any, locale string) (PluginSummary, error) {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return PluginSummary{}, fmt.Errorf("plugin: %q not found", pluginID)
	}

	mp.mu.Lock()
	nextPub, nextSec, err := mp.manifest.ValidateAndMergeConfig(patch, mp.publicConfig, mp.secretConfig)
	if err != nil {
		mp.mu.Unlock()
		return PluginSummary{}, err
	}
	cfgBytes, err := json.Marshal(nextPub)
	if err != nil {
		mp.mu.Unlock()
		return PluginSummary{}, err
	}
	secCipher, err := m.sealSecrets(pluginID, nextSec)
	if err != nil {
		mp.mu.Unlock()
		return PluginSummary{}, err
	}
	now := time.Now().Unix()
	mp.publicConfig = nextPub
	mp.secretConfig = nextSec
	mp.updatedAt = now
	mp.rebuildConfigSnapshot()
	mp.mu.Unlock()

	if _, err := m.store.ExecContext(
		ctx,
		`UPDATE plugins SET config_json = ?, secret_ciphertext = ?, updated_at = ? WHERE id = ?`,
		string(cfgBytes), secCipher, now, pluginID,
	); err != nil {
		return PluginSummary{}, err
	}

	m.logger.Write(pluginID, "info", "config", "configuration hot-reloaded")
	return m.toSummary(mp, locale), nil
}

// InstallFromZip safely validates and extracts an external plugin .zip package with strict anti-Zip-Slip,
// symlink rejection, anti-Zip-Bomb byte limits, manifest validation, and goja dry-run compilation.
func (m *Manager) InstallFromZip(ctx context.Context, readerAt io.ReaderAt, size int64, overwrite bool, locale string) (PluginSummary, error) {
	if strings.TrimSpace(m.pluginsDir) == "" {
		return PluginSummary{}, errors.New("plugin: external plugins directory is not configured")
	}
	if size <= 0 || size > MaxZipArchiveBytes {
		return PluginSummary{}, fmt.Errorf("plugin: zip archive size %d is out of bounds (1..%d bytes)", size, MaxZipArchiveBytes)
	}
	zr, err := zip.NewReader(readerAt, size)
	if err != nil {
		return PluginSummary{}, fmt.Errorf("plugin: invalid zip archive: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > MaxZipEntries {
		return PluginSummary{}, fmt.Errorf("plugin: zip entry count %d out of bounds (1..%d)", len(zr.File), MaxZipEntries)
	}

	if err := os.MkdirAll(m.pluginsDir, 0o750); err != nil {
		return PluginSummary{}, err
	}
	stagingDir := filepath.Join(m.pluginsDir, ".staging-"+id.Next())
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return PluginSummary{}, err
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	realStaging, err := filepath.EvalSymlinks(stagingDir)
	if err != nil {
		return PluginSummary{}, err
	}

	var totalExtracted int64
	for _, zf := range zr.File {
		mode := zf.Mode()
		if mode&os.ModeSymlink != 0 || (!zf.FileInfo().IsDir() && !mode.IsRegular()) {
			return PluginSummary{}, fmt.Errorf("plugin: disallowed non-regular or symlink entry %q in zip", zf.Name)
		}
		cleanRel, cErr := sanitizeZipEntryPath(zf.Name)
		if cErr != nil {
			return PluginSummary{}, fmt.Errorf("plugin: unsafe zip entry %q: %w", zf.Name, cErr)
		}
		destPath := filepath.Join(realStaging, filepath.FromSlash(cleanRel))
		if !isSubPath(realStaging, destPath) {
			return PluginSummary{}, fmt.Errorf("plugin: zip slip detected on %q", zf.Name)
		}

		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0o750); err != nil {
				return PluginSummary{}, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
			return PluginSummary{}, err
		}

		rc, oErr := zf.Open()
		if oErr != nil {
			return PluginSummary{}, oErr
		}
		outFile, fErr := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if fErr != nil {
			_ = rc.Close()
			return PluginSummary{}, fErr
		}

		written, copyErr := io.Copy(outFile, io.LimitReader(rc, MaxZipSingleBytes+1))
		_ = outFile.Close()
		_ = rc.Close()
		if copyErr != nil {
			return PluginSummary{}, copyErr
		}
		if written > MaxZipSingleBytes {
			return PluginSummary{}, fmt.Errorf("plugin: file %q exceeds %d MiB limit", cleanRel, MaxZipSingleBytes>>20)
		}
		totalExtracted += written
		if totalExtracted > MaxZipArchiveBytes {
			return PluginSummary{}, fmt.Errorf("plugin: decompressed archive exceeds %d MiB limit", MaxZipArchiveBytes>>20)
		}
	}

	// Locate manifest.json either at root or inside a single top-level folder.
	contentRoot, err := locateManifestRoot(realStaging)
	if err != nil {
		return PluginSummary{}, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(contentRoot, "manifest.json"))
	if err != nil {
		return PluginSummary{}, fmt.Errorf("plugin: read manifest.json: %w", err)
	}
	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return PluginSummary{}, err
	}
	if manifest.Runtime != RuntimeGoja {
		return PluginSummary{}, fmt.Errorf("plugin %q: external zip plugins must use runtime \"goja\"", manifest.ID)
	}

	// Dry-run compile entry JS before touching existing installation.
	if _, err := newGojaPluginRuntime(gojaRuntimeOptions{
		manifest:  manifest,
		pluginDir: contentRoot,
		logger:    m.logger,
		hostI18n:  m.catalog,
		getConfig: func() map[string]any { return nil },
	}); err != nil {
		return PluginSummary{}, err
	}

	// Check existing plugin conflict.
	targetDir := filepath.Join(m.pluginsDir, manifest.ID)
	existingMP, exists := m.getPlugin(manifest.ID)
	if exists {
		if existingMP.kind == "builtin" {
			return PluginSummary{}, fmt.Errorf("plugin %q conflicts with a built-in plugin", manifest.ID)
		}
		if !overwrite {
			return PluginSummary{}, fmt.Errorf("plugin %q is already installed (set overwrite=true to upgrade)", manifest.ID)
		}
		_ = m.Disable(ctx, manifest.ID)
	} else if _, statErr := os.Stat(targetDir); statErr == nil && !overwrite {
		return PluginSummary{}, fmt.Errorf("plugin directory %q already exists", manifest.ID)
	}

	backupDir := filepath.Join(m.pluginsDir, ".backup-"+manifest.ID+"-"+id.Next())
	hadOldDir := false
	if _, statErr := os.Stat(targetDir); statErr == nil {
		if err := os.Rename(targetDir, backupDir); err != nil {
			return PluginSummary{}, fmt.Errorf("plugin: backup existing dir: %w", err)
		}
		hadOldDir = true
	}
	if err := os.Rename(contentRoot, targetDir); err != nil {
		if hadOldDir {
			_ = os.Rename(backupDir, targetDir)
		}
		return PluginSummary{}, fmt.Errorf("plugin: move staged plugin into place: %w", err)
	}
	if hadOldDir {
		_ = os.RemoveAll(backupDir)
	}

	mp, err := m.syncRecordWithDB(ctx, manifest, "external", targetDir, nil)
	if err != nil {
		return PluginSummary{}, err
	}
	m.mu.Lock()
	m.plugins[manifest.ID] = mp
	m.mu.Unlock()

	m.logger.Write(manifest.ID, "info", "install", "installed external plugin v"+manifest.Version)
	return m.toSummary(mp, locale), nil
}

// Uninstall disables an external plugin, deletes its directory, KV keys, DB row, and logs.
func (m *Manager) Uninstall(ctx context.Context, pluginID string) error {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return fmt.Errorf("plugin: %q not found", pluginID)
	}
	if mp.kind == "builtin" {
		return fmt.Errorf("plugin %q is built-in and cannot be uninstalled (use Disable instead)", pluginID)
	}
	_ = m.Disable(ctx, pluginID)

	m.mu.Lock()
	delete(m.plugins, pluginID)
	m.mu.Unlock()

	if mp.pluginDir != "" && m.pluginsDir != "" {
		realPluginsRoot, err1 := filepath.EvalSymlinks(m.pluginsDir)
		realTarget, err2 := filepath.EvalSymlinks(mp.pluginDir)
		if err1 == nil && err2 == nil && realTarget != realPluginsRoot && isSubPath(realPluginsRoot, realTarget) {
			_ = os.RemoveAll(realTarget)
		}
	}

	_ = m.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_kv WHERE plugin_id = ?`, pluginID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM plugins WHERE id = ?`, pluginID)
		return err
	})
	m.logger.RemoveLogs(pluginID)
	return nil
}

// List returns secret-masked summaries of all registered plugins in deterministic ID order.
func (m *Manager) List(locale string) []PluginSummary {
	m.mu.RLock()
	ids := make([]string, 0, len(m.plugins))
	for k := range m.plugins {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	mps := make([]*managedPlugin, 0, len(ids))
	for _, k := range ids {
		mps = append(mps, m.plugins[k])
	}
	m.mu.RUnlock()

	out := make([]PluginSummary, 0, len(mps))
	for _, mp := range mps {
		out = append(out, m.toSummary(mp, locale))
	}
	return out
}

// Get returns the summary of a single plugin.
func (m *Manager) Get(pluginID string, locale string) (PluginSummary, bool) {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return PluginSummary{}, false
	}
	return m.toSummary(mp, locale), true
}

func (m *Manager) toSummary(mp *managedPlugin, locale string) PluginSummary {
	mp.mu.Lock()
	defer mp.mu.Unlock()

	pubCopy := make(map[string]any, len(mp.publicConfig))
	for k, v := range mp.publicConfig {
		pubCopy[k] = v
	}
	secSet := make(map[string]bool)
	for _, f := range mp.manifest.ConfigSchema {
		if f.Secret {
			secSet[f.Key] = mp.secretConfig[f.Key] != ""
		}
	}
	return PluginSummary{
		ID:            mp.manifest.ID,
		Kind:          mp.kind,
		Runtime:       mp.manifest.Runtime,
		Version:       mp.manifest.Version,
		Name:          ResolveLocalized(mp.manifest.Name, locale),
		Description:   ResolveLocalized(mp.manifest.Description, locale),
		Author:        mp.manifest.Author,
		Homepage:      mp.manifest.Homepage,
		IsEnabled:     mp.enabled,
		CircuitState:  mp.circuitState,
		FailureCount:  mp.failureCount,
		LastError:     mp.lastError,
		Permissions:   append([]string(nil), mp.manifest.Permissions...),
		AllowedHosts:  mp.manifest.EffectiveAllowedHosts(),
		ConfigSchema:  mp.manifest.ConfigSchema,
		PublicConfig:  pubCopy,
		SecretKeysSet: secSet,
		InstalledAt:   mp.installedAt,
		UpdatedAt:     mp.updatedAt,
	}
}

// DispatchArticleHook safely dispatches an article lifecycle/render hook across enabled plugins,
// strictly redacting password-protected articles and stripping `:::lock` blocks (10th Privacy Surface).
func (m *Manager) DispatchArticleHook(ctx context.Context, hookName string, raw ArticleHookPayload) ArticleHookPayload {
	active := m.snapshotEnabledPlugins()
	cur := raw

	for _, mp := range active {
		if !mp.manifest.CanUseHook(hookName) {
			continue
		}
		trustedProtected := mp.kind == "builtin" && mp.builtin != nil &&
			mp.builtin.AllowProtectedArticles() && mp.manifest.HasPermission("articles:read_protected")

		// 10th Privacy Surface Redaction: untrusted / external plugins NEVER receive protected plaintext!
		if !trustedProtected && cur.IsPasswordProtected && (hookName == HookRenderPostHTML || hookName == HookRenderPreMarkdown) {
			continue
		}
		safePayload := RedactArticleForPlugin(cur, trustedProtected)

		res, invoked, err := m.invokeHook(ctx, mp, hookName, safePayload)
		if err != nil {
			m.recordFailure(ctx, mp, "hook:"+hookName, err)
			continue
		}
		if invoked {
			m.recordSuccess(ctx, mp)
		}
		if hookName == HookRenderPreMarkdown && !cur.IsPasswordProtected && !cur.HasContentLock {
			switch out := res.(type) {
			case map[string]any:
				if nextMD, sOk := out["content_md"].(string); sOk && nextMD != "" {
					cur.ContentMD = nextMD
				}
			case ArticleHookPayload:
				if out.ContentMD != "" {
					cur.ContentMD = out.ContentMD
				}
			case string:
				if out != "" {
					cur.ContentMD = out
				}
			}
		}
		if hookName == HookRenderPostHTML && !cur.IsPasswordProtected && !cur.HasContentLock {
			if outMap, ok := res.(map[string]any); ok {
				if nextHTML, sOk := outMap["html"].(string); sOk && nextHTML != "" {
					cur.HTML = nextHTML
				}
			} else if outStruct, ok := res.(ArticleHookPayload); ok && outStruct.HTML != "" {
				cur.HTML = outStruct.HTML
			}
		}
	}
	return cur
}

// DispatchHook dispatches a generic domain hook (e.g., comment.before_create, comment.created).
func (m *Manager) DispatchHook(ctx context.Context, hookName string, payload any) (any, error) {
	active := m.snapshotEnabledPlugins()
	cur := payload
	for _, mp := range active {
		if !mp.manifest.CanUseHook(hookName) {
			continue
		}
		res, invoked, err := m.invokeHook(ctx, mp, hookName, cur)
		if err != nil {
			var rej *HookRejectedError
			if errors.As(err, &rej) {
				m.recordSuccess(ctx, mp)
				return cur, rej
			}
			m.recordFailure(ctx, mp, "hook:"+hookName, err)
			continue
		}
		if invoked {
			m.recordSuccess(ctx, mp)
			if hookName == HookCommentBeforeCreate {
				if allowVal, isBool := res.(bool); isBool && !allowVal {
					return cur, &HookRejectedError{PluginID: mp.manifest.ID, Reason: "comment rejected by plugin"}
				}
				if resMap, isMap := res.(map[string]any); isMap {
					if allowVal, hasAllow := resMap["allow"].(bool); hasAllow && !allowVal {
						reason, _ := resMap["reason"].(string)
						return cur, &HookRejectedError{PluginID: mp.manifest.ID, Reason: reason}
					}
					if rejVal, hasRej := resMap["reject"].(bool); hasRej && rejVal {
						reason, _ := resMap["reason"].(string)
						return cur, &HookRejectedError{PluginID: mp.manifest.ID, Reason: reason}
					}
				}
			}
			if res != nil {
				cur = res
			}
		}
	}
	return cur, nil
}

func (m *Manager) invokeHook(ctx context.Context, mp *managedPlugin, hookName string, payload any) (any, bool, error) {
	mp.mu.Lock()
	enabled := mp.enabled && mp.circuitState == CircuitClosed
	rt := mp.gojaRT
	bp := mp.builtin
	runtimeKind := mp.manifest.Runtime
	mp.mu.Unlock()

	if !enabled {
		return nil, false, nil
	}
	hookCtx, cancel := context.WithTimeout(ctx, m.hookTimeout)
	defer cancel()

	if runtimeKind == RuntimeBuiltin && bp != nil {
		out, err := bp.OnHook(hookCtx, hookName, payload)
		return out, true, err
	}
	if rt != nil {
		return rt.invoke(hookCtx, m.hookTimeout, "onHook", hookName, payload)
	}
	return nil, false, nil
}

// RenderDashboardCards renders and strictly sanitizes `admin.dashboard_card` slots across enabled plugins.
func (m *Manager) RenderDashboardCards(ctx context.Context, slotCtx SlotRenderContext) []RenderedSlot {
	return m.RenderSlots(ctx, SlotAdminDashboardCard, slotCtx)
}

// RenderSlots collects rendered HTML/asset fragments for slotName across all enabled plugins.
// Persistent slots (Persist=true) are automatically wrapped with `data-volo-persist="<plugin_id>"`
// for zero-coupled PJAX survival in volo-studio.
func (m *Manager) RenderSlots(ctx context.Context, slotName string, slotCtx SlotRenderContext) []RenderedSlot {
	active := m.snapshotEnabledPlugins()
	var out []RenderedSlot
	isAdminCard := slotName == SlotAdminDashboardCard

	for _, mp := range active {
		for _, spec := range mp.manifest.UISlots {
			if spec.Slot != slotName {
				continue
			}
			var buf strings.Builder
			verEsc := html.EscapeString(mp.manifest.Version)
			idEsc := html.EscapeString(mp.manifest.ID)

			if spec.StyleAsset != "" && !isAdminCard {
				fmt.Fprintf(&buf, `<link rel="stylesheet" href="/plugins/%s/assets/%s?v=%s" data-volo-plugin="%s">`,
					idEsc, html.EscapeString(spec.StyleAsset), verEsc, idEsc)
			}
			var bodyBuf strings.Builder
			if spec.HTML != "" {
				bodyBuf.WriteString(spec.HTML)
			}
			if spec.Dynamic {
				dynHTML, invoked, err := m.invokeSlot(ctx, mp, slotName, slotCtx)
				if err != nil {
					m.recordFailure(ctx, mp, "slot:"+slotName, err)
					continue
				}
				if invoked {
					m.recordSuccess(ctx, mp)
				}
				bodyBuf.WriteString(dynHTML)
			}
			if isAdminCard {
				buf.WriteString(render.Default().SanitizeHTML(bodyBuf.String()))
			} else {
				buf.WriteString(bodyBuf.String())
				if spec.ScriptAsset != "" {
					fmt.Fprintf(&buf, `<script defer src="/plugins/%s/assets/%s?v=%s" data-volo-plugin="%s"></script>`,
						idEsc, html.EscapeString(spec.ScriptAsset), verEsc, idEsc)
				}
			}

			fragment := buf.String()
			if strings.TrimSpace(fragment) == "" {
				continue
			}
			if spec.Persist && !isAdminCard {
				fragment = fmt.Sprintf(`<div data-volo-persist="%s" data-volo-plugin="%s">%s</div>`, idEsc, idEsc, fragment)
			}
			out = append(out, RenderedSlot{
				PluginID: mp.manifest.ID,
				Slot:     slotName,
				Persist:  spec.Persist && !isAdminCard,
				HTML:     fragment,
			})
		}
	}
	return out
}

func (m *Manager) invokeSlot(ctx context.Context, mp *managedPlugin, slotName string, slotCtx SlotRenderContext) (string, bool, error) {
	mp.mu.Lock()
	enabled := mp.enabled && mp.circuitState == CircuitClosed
	rt := mp.gojaRT
	bp := mp.builtin
	runtimeKind := mp.manifest.Runtime
	mp.mu.Unlock()

	if !enabled {
		return "", false, nil
	}
	sCtx, cancel := context.WithTimeout(ctx, m.hookTimeout)
	defer cancel()

	if runtimeKind == RuntimeBuiltin && bp != nil {
		s, err := bp.RenderSlot(sCtx, slotName, slotCtx)
		return s, true, err
	}
	if rt != nil {
		res, found, err := rt.invoke(sCtx, m.hookTimeout, "renderSlot", slotName, slotCtx)
		if err != nil || !found || res == nil {
			return "", found, err
		}
		return fmt.Sprint(res), true, nil
	}
	return "", false, nil
}

// EditorQuickInserts returns localized toolbar actions contributed by enabled plugins for CodeMirror 6.
func (m *Manager) EditorQuickInserts(locale string) []EditorQuickInsertView {
	active := m.snapshotEnabledPlugins()
	var out []EditorQuickInsertView
	for _, mp := range active {
		for _, spec := range mp.manifest.UISlots {
			if spec.Slot != SlotEditorQuickInsert {
				continue
			}
			if spec.QuickInsert != nil {
				out = append(out, EditorQuickInsertView{
					PluginID: mp.manifest.ID,
					ID:       spec.QuickInsert.ID,
					Label:    ResolveLocalized(spec.QuickInsert.Label, locale),
					Icon:     spec.QuickInsert.Icon,
					Template: spec.QuickInsert.Template,
				})
			}
			if spec.Dynamic {
				out = append(out, m.invokeDynamicQuickInserts(mp, locale)...)
			}
		}
	}
	return out
}

func (m *Manager) invokeDynamicQuickInserts(mp *managedPlugin, locale string) []EditorQuickInsertView {
	mp.mu.Lock()
	enabled := mp.enabled && mp.circuitState == CircuitClosed
	rt := mp.gojaRT
	bp := mp.builtin
	runtimeKind := mp.manifest.Runtime
	mp.mu.Unlock()
	if !enabled {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.hookTimeout)
	defer cancel()

	var (
		res   any
		found bool
		err   error
	)
	if runtimeKind == RuntimeBuiltin && bp != nil {
		rawStr, bErr := bp.RenderSlot(ctx, SlotEditorQuickInsert, SlotRenderContext{Locale: locale})
		err = bErr
		if err == nil && strings.TrimSpace(rawStr) != "" {
			res = rawStr
			found = true
		}
	} else if rt != nil {
		res, found, err = rt.invoke(ctx, m.hookTimeout, "editorQuickInserts", locale)
		if !found && err == nil {
			res, found, err = rt.invoke(ctx, m.hookTimeout, "renderSlot", SlotEditorQuickInsert, SlotRenderContext{Locale: locale})
		}
	} else {
		return nil
	}
	if err != nil {
		m.recordFailure(ctx, mp, "slot:"+SlotEditorQuickInsert, err)
		return nil
	}
	if !found || res == nil {
		return nil
	}
	m.recordSuccess(ctx, mp)

	var items []any
	switch raw := res.(type) {
	case []any:
		items = raw
	case string:
		_ = json.Unmarshal([]byte(raw), &items)
	}
	var out []EditorQuickInsertView
	for i, it := range items {
		itemMap, ok := it.(map[string]any)
		if !ok {
			continue
		}
		tmpl, _ := itemMap["template"].(string)
		if strings.TrimSpace(tmpl) == "" {
			continue
		}
		itemID, _ := itemMap["id"].(string)
		if strings.TrimSpace(itemID) == "" {
			itemID = fmt.Sprintf("dyn-%d", i+1)
		}
		var label string
		switch lv := itemMap["label"].(type) {
		case string:
			label = lv
		case map[string]any:
			lblMap := make(map[string]string, len(lv))
			for k, v := range lv {
				lblMap[k] = fmt.Sprint(v)
			}
			label = ResolveLocalized(lblMap, locale)
		}
		if strings.TrimSpace(label) == "" {
			label = itemID
		}
		icon, _ := itemMap["icon"].(string)
		out = append(out, EditorQuickInsertView{
			PluginID: mp.manifest.ID,
			ID:       itemID,
			Label:    label,
			Icon:     icon,
			Template: tmpl,
		})
	}
	return out
}

// Handler returns an http.Handler serving `/plugins/{id}/api/*` and `/plugins/{id}/assets/*`.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/plugins/")
		parts := strings.SplitN(trimmed, "/", 3)
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		pluginID, namespace := parts[0], parts[1]
		subPath := "/"
		if len(parts) == 3 && parts[2] != "" {
			subPath = "/" + parts[2]
		}

		mp, ok := m.getPlugin(pluginID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		mp.mu.Lock()
		isActive := mp.enabled && mp.circuitState == CircuitClosed
		mp.mu.Unlock()
		if !isActive {
			http.NotFound(w, r)
			return
		}

		switch namespace {
		case "api":
			m.servePluginAPI(w, r, mp, subPath)
		case "assets":
			m.servePluginAsset(w, r, mp, strings.TrimPrefix(subPath, "/"))
		default:
			http.NotFound(w, r)
		}
	})
}

func (m *Manager) servePluginAPI(w http.ResponseWriter, r *http.Request, mp *managedPlugin, subPath string) {
	if !mp.manifest.HasPermission("http:routes") {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":"request body too large"}`))
			return
		}
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	queryMap := make(map[string]string, len(r.URL.Query()))
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			queryMap[k] = vs[0]
		}
	}
	hdrMap := make(map[string]string)
	for _, allowedHdr := range []string{"Accept", "Content-Type", "Accept-Language", "User-Agent"} {
		if v := r.Header.Get(allowedHdr); v != "" {
			hdrMap[strings.ToLower(allowedHdr)] = v
		}
	}
	if mp.manifest.HasPermission("http:raw_headers") || mp.manifest.HasPermission("raw_headers") {
		for _, sigHdr := range []string{
			"X-Hub-Signature-256",
			"X-Hub-Signature",
			"X-Signature",
			"X-Webhook-Signature",
			"Stripe-Signature",
			"Authorization",
			"User-Agent",
			"X-GitHub-Event",
			"X-GitHub-Delivery",
		} {
			if v := r.Header.Get(sigHdr); v != "" {
				hdrMap[strings.ToLower(sigHdr)] = v
			}
		}
	}
	locale := "zh-CN"
	if m.catalog != nil {
		locale = m.catalog.ResolveRequest(r, "zh-CN", true)
	}
	isAdmin, ipMasked := m.inspectRequest(r)
	apiReq := APIRequest{
		Method:         r.Method,
		Path:           subPath,
		Query:          queryMap,
		Headers:        hdrMap,
		Body:           string(bodyBytes),
		Locale:         locale,
		ClientIPMasked: ipMasked,
		IsAdmin:        isAdmin,
	}

	mp.mu.Lock()
	enabled := mp.enabled && mp.circuitState == CircuitClosed
	runtimeKind := mp.manifest.Runtime
	bp := mp.builtin
	rt := mp.gojaRT
	mp.mu.Unlock()

	if !enabled {
		http.NotFound(w, r)
		return
	}

	reqCtx, cancel := context.WithTimeout(r.Context(), m.routeTimeout)
	defer cancel()

	var resp APIResponse
	var invokeErr error
	if runtimeKind == RuntimeBuiltin && bp != nil {
		resp, invokeErr = bp.ServeAPI(reqCtx, apiReq)
	} else if rt != nil {
		raw, found, iErr := rt.invoke(reqCtx, m.routeTimeout, "onRequest", apiReq)
		if iErr != nil {
			invokeErr = iErr
		} else if !found {
			http.NotFound(w, r)
			return
		} else {
			resp = coerceAPIResponse(raw)
		}
	} else {
		http.NotFound(w, r)
		return
	}

	if invokeErr != nil {
		m.recordFailure(r.Context(), mp, "route:"+subPath, invokeErr)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"plugin execution failed"}`))
		return
	}
	m.recordSuccess(r.Context(), mp)

	if resp.Status < 100 || resp.Status > 599 {
		resp.Status = http.StatusOK
	}
	ct := strings.TrimSpace(resp.ContentType)
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	// Prevent arbitrary top-level HTML script execution on /plugins/<id>/api/*.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	for k, v := range resp.Headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "cache-control" || strings.HasPrefix(lk, "x-plugin-") {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.Status)
	_, _ = io.WriteString(w, resp.Body)
}

func coerceAPIResponse(raw any) APIResponse {
	if raw == nil {
		return APIResponse{Status: http.StatusNoContent}
	}
	switch v := raw.(type) {
	case APIResponse:
		return v
	case string:
		return APIResponse{Status: http.StatusOK, ContentType: "text/plain; charset=utf-8", Body: v}
	case map[string]any:
		out := APIResponse{Status: http.StatusOK, ContentType: "application/json; charset=utf-8"}
		if st, ok := v["status"]; ok {
			switch n := st.(type) {
			case int64:
				out.Status = int(n)
			case float64:
				out.Status = int(n)
			case int:
				out.Status = n
			}
		}
		if ct, ok := v["content_type"].(string); ok && ct != "" {
			out.ContentType = ct
		} else if ct2, ok := v["contentType"].(string); ok && ct2 != "" {
			out.ContentType = ct2
		}
		if hdrs, ok := v["headers"].(map[string]any); ok {
			out.Headers = make(map[string]string, len(hdrs))
			for hk, hv := range hdrs {
				out.Headers[hk] = fmt.Sprint(hv)
			}
		}
		if bodyStr, ok := v["body"].(string); ok {
			out.Body = bodyStr
		} else if jsonData, exists := v["json"]; exists {
			b, _ := json.Marshal(jsonData)
			out.Body = string(b)
		} else {
			b, _ := json.Marshal(v)
			out.Body = string(b)
		}
		return out
	default:
		b, _ := json.Marshal(v)
		return APIResponse{Status: http.StatusOK, ContentType: "application/json; charset=utf-8", Body: string(b)}
	}
}

func (m *Manager) servePluginAsset(w http.ResponseWriter, r *http.Request, mp *managedPlugin, relAsset string) {
	if mp.pluginDir == "" {
		http.NotFound(w, r)
		return
	}
	cleanRel, err := sanitizeRelativeAssetPath(relAsset)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	assetsRoot := filepath.Join(mp.pluginDir, "assets")
	realAssetsRoot, err := filepath.EvalSymlinks(assetsRoot)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	candidate := filepath.Join(realAssetsRoot, filepath.FromSlash(cleanRel))
	realTarget, err := filepath.EvalSymlinks(candidate)
	if err != nil || realTarget == realAssetsRoot || !isSubPath(realAssetsRoot, realTarget) {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(realTarget)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	ext := strings.ToLower(filepath.Ext(realTarget))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if ext == ".svg" || ext == ".html" || ext == ".htm" || ext == ".xml" {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; sandbox")
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

func (m *Manager) recordFailure(ctx context.Context, mp *managedPlugin, stage string, err error) {
	m.recordFailureWithOrigin(ctx, mp, stage, err, false)
}

func (m *Manager) recordFailureWithOrigin(ctx context.Context, mp *managedPlugin, stage string, err error, fromCron bool) {
	now := time.Now().Unix()
	errMsg := err.Error()

	mp.mu.Lock()
	mp.failureCount++
	count := mp.failureCount
	mp.lastError = errMsg
	mp.updatedAt = now
	tripped := false
	if mp.circuitState != CircuitOpen && count >= m.maxFailures {
		mp.circuitState = CircuitOpen
		mp.enabled = false
		tripped = true
	}
	state := mp.circuitState
	enabled := mp.enabled
	mp.mu.Unlock()

	m.logger.Write(mp.manifest.ID, "error", stage, fmt.Sprintf("failure #%d: %s", count, errMsg))

	if tripped {
		m.deactivateRuntimeOpts(ctx, mp, !fromCron)
		m.logger.Write(mp.manifest.ID, "error", "circuit", fmt.Sprintf("CIRCUIT_OPEN after %d consecutive failures; plugin auto-disabled", count))
	}

	_, _ = m.store.ExecBgContext(
		ctx,
		`UPDATE plugins SET is_enabled = ?, circuit_state = ?, failure_count = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		enabled, state, count, errMsg, now, mp.manifest.ID,
	)
}

func (m *Manager) recordSuccess(ctx context.Context, mp *managedPlugin) {
	mp.mu.Lock()
	if mp.failureCount == 0 {
		mp.mu.Unlock()
		return
	}
	mp.failureCount = 0
	mp.lastError = ""
	mp.updatedAt = time.Now().Unix()
	now := mp.updatedAt
	mp.mu.Unlock()

	_, _ = m.store.ExecBgContext(
		ctx,
		`UPDATE plugins SET failure_count = 0, last_error = '', updated_at = ? WHERE id = ?`,
		now, mp.manifest.ID,
	)
}

// PurgeExpiredKV deletes expired plugin_kv rows across all plugins.
func (m *Manager) PurgeExpiredKV(ctx context.Context) (int64, error) {
	if m == nil || m.store == nil {
		return 0, nil
	}
	return PurgeExpiredKV(ctx, m.store, time.Now().Unix())
}

// Close gracefully stops all plugin background cron jobs and detaches runtimes.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	list := make([]*managedPlugin, 0, len(m.plugins))
	for _, mp := range m.plugins {
		list = append(list, mp)
	}
	m.mu.Unlock()

	for _, mp := range list {
		m.deactivateRuntime(ctx, mp)
	}
	return nil
}

func (m *Manager) getPlugin(pluginID string) (*managedPlugin, bool) {
	m.mu.RLock()
	mp, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	return mp, ok
}

func (m *Manager) snapshotEnabledPlugins() []*managedPlugin {
	m.mu.RLock()
	candidates := make([]*managedPlugin, 0, len(m.plugins))
	for _, mp := range m.plugins {
		candidates = append(candidates, mp)
	}
	m.mu.RUnlock()

	out := make([]*managedPlugin, 0, len(candidates))
	for _, mp := range candidates {
		mp.mu.Lock()
		active := mp.enabled && mp.circuitState == CircuitClosed
		mp.mu.Unlock()
		if active {
			out = append(out, mp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].manifest.ID < out[j].manifest.ID
	})
	return out
}

type gojaOIDCAdapter struct {
	manager      *Manager
	mp           *managedPlugin
	provName     string
	displayNames map[string]string
}

func newGojaOIDCAdapter(ctx context.Context, m *Manager, mp *managedPlugin, rt *gojaPluginRuntime) *gojaOIDCAdapter {
	provName := mp.manifest.ID
	displayNames := make(map[string]string, len(mp.manifest.Name))
	for k, v := range mp.manifest.Name {
		displayNames[k] = v
	}
	if rt != nil && rt.hasCallable("oidcProviderInfo") {
		if raw, found, err := rt.invoke(ctx, m.hookTimeout, "oidcProviderInfo"); err == nil && found && raw != nil {
			if info, ok := raw.(map[string]any); ok {
				for _, key := range []string{"id", "provider", "name"} {
					if s, sOk := info[key].(string); sOk {
						cand := strings.ToLower(strings.TrimSpace(s))
						if validPluginIDRe.MatchString(cand) {
							provName = cand
							break
						}
					}
				}
				dnRaw := info["display_name"]
				if dnRaw == nil {
					dnRaw = info["label"]
				}
				if dnRaw == nil && (info["id"] != nil || info["provider"] != nil) {
					dnRaw = info["name"]
				}
				switch dn := dnRaw.(type) {
				case string:
					if strings.TrimSpace(dn) != "" {
						displayNames = map[string]string{"en-US": strings.TrimSpace(dn), "zh-CN": strings.TrimSpace(dn)}
					}
				case map[string]any:
					custom := make(map[string]string, len(dn))
					for k, v := range dn {
						if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
							custom[k] = s
						}
					}
					if len(custom) > 0 {
						displayNames = custom
					}
				}
			}
		}
	}
	return &gojaOIDCAdapter{
		manager:      m,
		mp:           mp,
		provName:     provName,
		displayNames: displayNames,
	}
}

func (a *gojaOIDCAdapter) Name() string { return a.provName }

func (a *gojaOIDCAdapter) DisplayName(locale string) string {
	if lbl := ResolveLocalized(a.displayNames, locale); lbl != "" {
		return lbl
	}
	if lbl := ResolveLocalized(a.mp.manifest.Name, locale); lbl != "" {
		return lbl
	}
	return a.provName
}

func (a *gojaOIDCAdapter) AuthCodeURL(state string, redirectURI string) (string, error) {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return "", errors.New("plugin oidc provider inactive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.manager.routeTimeout)
	defer cancel()

	res, found, err := rt.invoke(ctx, a.manager.routeTimeout, "oidcAuthURL", state, redirectURI)
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:oidc:auth_url", err)
		return "", err
	}
	if !found {
		return "", errors.New("oidcAuthURL function not exported")
	}
	a.manager.recordSuccess(ctx, a.mp)

	rawURL := ""
	switch v := res.(type) {
	case string:
		rawURL = strings.TrimSpace(v)
	case map[string]any:
		if u, ok := v["url"].(string); ok {
			rawURL = strings.TrimSpace(u)
		}
	}
	uParsed, uErr := url.Parse(rawURL)
	if rawURL == "" || uErr != nil || (uParsed.Scheme != "https" && uParsed.Scheme != "http") || uParsed.Host == "" {
		return "", errors.New("oidcAuthURL must return a valid http/https authorization URL")
	}
	return rawURL, nil
}

func (a *gojaOIDCAdapter) Exchange(ctx context.Context, code string, redirectURI string) (OIDCIdentity, error) {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return OIDCIdentity{}, errors.New("plugin oidc provider inactive")
	}
	timeout := a.manager.routeTimeout
	if timeout < 8*time.Second {
		timeout = 8 * time.Second
	}
	res, found, err := rt.invoke(ctx, timeout, "oidcExchange", code, redirectURI)
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:oidc:exchange", err)
		return OIDCIdentity{}, err
	}
	if !found {
		return OIDCIdentity{}, errors.New("oidcExchange function not exported")
	}
	a.manager.recordSuccess(ctx, a.mp)

	m, ok := res.(map[string]any)
	if !ok {
		return OIDCIdentity{}, errors.New("oidcExchange must return an identity object")
	}
	sub := ""
	for _, k := range []string{"subject", "sub", "id"} {
		if s, sOk := m[k].(string); sOk && strings.TrimSpace(s) != "" {
			sub = strings.TrimSpace(s)
			break
		}
	}
	if sub == "" {
		return OIDCIdentity{}, errors.New("oidcExchange returned empty subject")
	}
	prov := a.Name()
	if pStr, sOk := m["provider"].(string); sOk && strings.TrimSpace(pStr) != "" {
		prov = strings.TrimSpace(pStr)
	}
	email, _ := m["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))

	disp := ""
	for _, k := range []string{"display_name", "name", "preferred_username"} {
		if s, sOk := m[k].(string); sOk && strings.TrimSpace(s) != "" {
			disp = strings.TrimSpace(s)
			break
		}
	}
	avatar := ""
	for _, k := range []string{"avatar_url", "picture"} {
		if s, sOk := m[k].(string); sOk && strings.TrimSpace(s) != "" {
			cand := strings.TrimSpace(stripASCIIControlsString(s))
			if strings.HasPrefix(cand, "/") && !strings.HasPrefix(cand, "//") && !strings.HasPrefix(cand, "/\\") {
				avatar = cand
				break
			}
			if u, uErr := url.Parse(cand); uErr == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
				avatar = cand
				break
			}
		}
	}
	return OIDCIdentity{
		Provider:    prov,
		Subject:     sub,
		Email:       email,
		DisplayName: disp,
		AvatarURL:   avatar,
	}, nil
}

type gojaAntiSpamAdapter struct {
	manager *Manager
	mp      *managedPlugin
}

func (a *gojaAntiSpamAdapter) Name() string { return "plugin:" + a.mp.manifest.ID }

func (a *gojaAntiSpamAdapter) CheckComment(ctx context.Context, input CommentCheckInput) (SpamVerdict, error) {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return SpamVerdict{}, errors.New("plugin antispam inactive")
	}
	res, found, err := rt.invoke(ctx, a.manager.hookTimeout, "checkSpam", input)
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:antispam", err)
		return SpamVerdict{}, err
	}
	if !found {
		return SpamVerdict{}, errors.New("checkSpam function not exported")
	}
	a.manager.recordSuccess(ctx, a.mp)

	if m, ok := res.(map[string]any); ok {
		verdict := SpamVerdict{Action: SpamActionAllow}
		if act, sOk := m["action"].(string); sOk {
			verdict.Action = strings.ToLower(strings.TrimSpace(act))
		}
		if reason, sOk := m["reason"].(string); sOk {
			verdict.Reason = reason
		}
		switch sc := m["score"].(type) {
		case float64:
			verdict.Score = sc
		case int64:
			verdict.Score = float64(sc)
		}
		return verdict, nil
	}
	return SpamVerdict{Action: SpamActionAllow}, nil
}

type gojaNotifierAdapter struct {
	manager *Manager
	mp      *managedPlugin
}

func (a *gojaNotifierAdapter) Name() string { return "plugin:" + a.mp.manifest.ID }

func (a *gojaNotifierAdapter) Send(ctx context.Context, event NotificationEvent) error {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return errors.New("plugin notifier inactive")
	}
	timeout := a.manager.routeTimeout
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	_, found, err := rt.invoke(ctx, timeout, "sendNotification", event)
	if !found && err == nil {
		_, found, err = rt.invoke(ctx, timeout, "onNotify", event)
	}
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:notifier", err)
		return err
	}
	if !found {
		return errors.New("sendNotification function not exported")
	}
	a.manager.recordSuccess(ctx, a.mp)
	return nil
}

type gojaStorageAdapter struct {
	manager *Manager
	mp      *managedPlugin
}

func (a *gojaStorageAdapter) Name() string { return a.mp.manifest.ID }

func (a *gojaStorageAdapter) Put(ctx context.Context, objectKey string, r io.Reader, size int64, contentType string) (string, error) {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return "", errors.New("plugin storage inactive")
	}
	const maxPluginStorageBytes = 64 << 20
	if size > maxPluginStorageBytes {
		return "", errors.New("plugin storage: payload exceeds 64 MiB limit")
	}
	rawBytes, err := io.ReadAll(io.LimitReader(r, maxPluginStorageBytes+1))
	if err != nil {
		return "", err
	}
	if len(rawBytes) > maxPluginStorageBytes {
		return "", errors.New("plugin storage: payload exceeds 64 MiB limit")
	}
	b64 := base64.StdEncoding.EncodeToString(rawBytes)
	timeout := a.manager.cronTimeout
	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	}
	res, found, err := rt.invoke(ctx, timeout, "storagePut", objectKey, b64, contentType)
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:storage:put", err)
		return "", err
	}
	if !found {
		return "", errors.New("storagePut function not exported")
	}
	a.manager.recordSuccess(ctx, a.mp)

	switch v := res.(type) {
	case string:
		u := strings.TrimSpace(v)
		if u == "" {
			return "", errors.New("storagePut returned empty URL")
		}
		return u, nil
	case map[string]any:
		if u, ok := v["url"].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u), nil
		}
		if u, ok := v["public_url"].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u), nil
		}
	}
	return "", errors.New("storagePut must return a non-empty URL string")
}

func (a *gojaStorageAdapter) Delete(ctx context.Context, objectKey string) error {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return errors.New("plugin storage inactive")
	}
	_, found, err := rt.invoke(ctx, a.manager.routeTimeout, "storageDelete", objectKey)
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:storage:delete", err)
		return err
	}
	if !found {
		return nil
	}
	a.manager.recordSuccess(ctx, a.mp)
	return nil
}

func (a *gojaStorageAdapter) HealthCheck(ctx context.Context) error {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return errors.New("plugin storage inactive")
	}
	res, found, err := rt.invoke(ctx, a.manager.routeTimeout, "storageHealthCheck")
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:storage:health", err)
		return err
	}
	if found {
		a.manager.recordSuccess(ctx, a.mp)
		if okVal, isBool := res.(bool); isBool && !okVal {
			return errors.New("plugin storage health check reported unhealthy")
		}
	}
	return nil
}

// RedactArticleForPlugin enforces the 10th Privacy Surface:
// 1. Password-protected articles omit SummaryMD, ContentMD, HTML, CoverURL, and AudioURL completely.
// 2. Partial `:::lock` blocks (including unclosed `:::lock` to EOF outside code fences) are physically stripped.
func RedactArticleForPlugin(in ArticleHookPayload, trustedProtected bool) ArticleHookPayload {
	if trustedProtected {
		return in
	}
	out := in
	if in.IsPasswordProtected {
		out.SummaryMD = ""
		out.ContentMD = ""
		out.HTML = ""
		out.CoverURL = ""
		out.AudioURL = ""
		out.OmittedProtected = true
		return out
	}
	if in.HasContentLock || strings.Contains(in.ContentMD, ":::lock") || strings.Contains(in.SummaryMD, ":::lock") || strings.Contains(in.HTML, ":::lock") {
		out.SummaryMD = StripLockedMarkdownBlocks(in.SummaryMD)
		out.ContentMD = StripLockedMarkdownBlocks(in.ContentMD)
		out.HTML = ""
	}
	return out
}

// StripLockedMarkdownBlocks removes all `:::lock ... :::` blocks outside fenced code blocks (` ``` ` / `~~~`).
// If an opening `:::lock` has no matching closing `:::`, it FAILS CLOSED to EOF so locked content never leaks!
func StripLockedMarkdownBlocks(src string) string {
	if !strings.Contains(src, ":::lock") {
		return src
	}
	normalized := strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	out := make([]string, 0, len(lines))

	inFence := false
	var fenceChar byte
	fenceLen := 0
	inLock := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track fenced code blocks so literal `:::lock` examples inside code blocks are never misinterpreted.
		if ch, n, ok := parseCodeFenceMarker(trimmed); ok {
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = n
			} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				inFence = false
				fenceChar = 0
				fenceLen = 0
			}
			if !inLock {
				out = append(out, line)
			}
			continue
		}

		if !inFence {
			if !inLock && isLockOpenDirective(trimmed) {
				inLock = true
				continue
			}
			if inLock && trimmed == ":::" {
				inLock = false
				continue
			}
		}

		if !inLock {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func parseCodeFenceMarker(trimmed string) (byte, int, bool) {
	if len(trimmed) < 3 {
		return 0, 0, false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	return ch, n, true
}

func isLockOpenDirective(trimmed string) bool {
	if !strings.HasPrefix(trimmed, ":::lock") {
		return false
	}
	rest := trimmed[len(":::lock"):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '['
}

func sanitizeZipEntryPath(raw string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", errors.New("empty zip entry name")
	}
	if strings.Contains(raw, "\\") || strings.Contains(raw, "\x00") || strings.HasPrefix(raw, "/") || strings.Contains(raw, ":") {
		return "", errors.New("forbidden path characters")
	}
	return sanitizeRelativeAssetPath(trimmed)
}

func locateManifestRoot(stagingRoot string) (string, error) {
	if _, err := os.Stat(filepath.Join(stagingRoot, "manifest.json")); err == nil {
		return stagingRoot, nil
	}
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), "__MACOSX") || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		if ent.IsDir() {
			dirs = append(dirs, filepath.Join(stagingRoot, ent.Name()))
		}
	}
	if len(dirs) == 1 {
		if _, err := os.Stat(filepath.Join(dirs[0], "manifest.json")); err == nil {
			return dirs[0], nil
		}
	}
	return "", errors.New("plugin: manifest.json not found at archive root or single top-level directory")
}
