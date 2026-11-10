package plugin

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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

// SecretSealer seals and opens sensitive plugin configuration fields (e.g. backed by config.SecretBox).
type SecretSealer interface {
	Seal(plaintext string) (string, error)
	Open(ciphertext string) (string, error)
}

const reservedPluginPriorityKey = "__volo_priority__"

// HookRejectedError signals an intentional business-rule rejection returned by a pre-action hook (`before_*`).
type HookRejectedError struct {
	PluginID string
	Hook     string
	Reason   string
	Code     string
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

// ConfigChangeListener is an optional interface that BuiltinPlugin implementations may implement
// to react immediately when plugin configuration is updated at runtime (e.g. rescheduling cron tasks).
type ConfigChangeListener interface {
	OnConfigChange(ctx context.Context, cfg map[string]any) error
}

// UninstallListener is an optional interface for BuiltinPlugin implementations mirroring JS onUninstall().
type UninstallListener interface {
	OnUninstall(ctx context.Context) error
}

func safeCallBuiltin(pluginID string, stage string, fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("plugin %q: panic in %s: %v", pluginID, stage, rec)
		}
	}()
	return fn()
}

// HostContext exposes capability-scoped host services to a BuiltinPlugin.
type HostContext struct {
	PluginID     string
	KV           *KVStore
	Providers    *ProviderRegistry
	cron         *CronRegistry
	hasCronPerm  bool
	logger       *PluginLogger
	hostI18n     *i18n.Catalog
	getConfig    func() map[string]any
	unsealSecret func(string) string
	dialer       *security.OutboundSafeDialer
}

func (h *HostContext) Config() map[string]any {
	cfg := h.getConfig()
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "v1:") && h.unsealSecret != nil {
			out[k] = h.unsealSecret(s)
		} else {
			out[k] = v
		}
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
func (h *HostContext) RegisterCron(spec CronSpec, immediate bool) error {
	if !h.hasCronPerm || h.cron == nil {
		return errors.New("plugin: cron permission is not declared")
	}
	return h.cron.RegisterJob(h.PluginID, spec, CronSourceDynamic, immediate)
}
func (h *HostContext) UnregisterCron(name string) bool {
	if !h.hasCronPerm || h.cron == nil {
		return false
	}
	return h.cron.UnregisterJob(h.PluginID, name)
}
func (h *HostContext) TriggerCron(name string) int {
	if !h.hasCronPerm || h.cron == nil {
		return 0
	}
	return h.cron.Trigger(h.PluginID, name)
}
func (h *HostContext) CronJobs() []CronJobSnapshot {
	if h.cron == nil {
		return []CronJobSnapshot{}
	}
	return h.cron.ListJobs(h.PluginID)
}

// PluginSummary is the safe, secret-masked view returned to Admin REST APIs.
type PluginSummary struct {
	ID            string            `json:"id"`
	Kind          string            `json:"kind"` // builtin | external
	Runtime       string            `json:"runtime"`
	Version       string            `json:"version"`
	Priority      int               `json:"priority"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Author        string            `json:"author,omitempty"`
	Homepage      string            `json:"homepage,omitempty"`
	IsEnabled     bool              `json:"is_enabled"`
	CircuitState  string            `json:"circuit_state"`
	FailureCount  int               `json:"failure_count"`
	LastError     string            `json:"last_error,omitempty"`
	Permissions   []string          `json:"permissions"`
	Hooks         []string          `json:"hooks,omitempty"`
	AllowedHosts  []string          `json:"allowed_hosts"`
	ConfigSchema  []ConfigField     `json:"config_schema"`
	PublicConfig  map[string]any    `json:"public_config"`
	Config        map[string]any    `json:"config,omitempty"`
	SecretKeysSet map[string]bool   `json:"secret_keys_set"`
	CronJobs      []CronJobSnapshot `json:"cron_jobs,omitempty"`
	InstalledAt   int64             `json:"installed_at"`
	UpdatedAt     int64             `json:"updated_at"`
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
	SecretSealer          SecretSealer
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
	mu             sync.Mutex
	manifest       Manifest
	kind           string // "builtin" | "external"
	pluginDir      string
	builtin        BuiltinPlugin
	gojaRT         *gojaPluginRuntime
	kv             *KVStore
	enabled        bool
	runtimeActive  bool
	priority       int
	customPriority bool
	prioritySnap   atomic.Int64
	circuitState   string
	failureCount   int
	lastError      string
	installedAt    int64
	updatedAt      int64

	publicConfig  map[string]any
	secretConfig  map[string]string
	mergedCfgSnap atomic.Pointer[map[string]any]
	cfgRev        atomic.Uint64
}

func (mp *managedPlugin) setPriorityLocked(p int, custom bool) {
	clamped := ClampPluginPriority(p)
	mp.priority = clamped
	mp.customPriority = custom
	mp.prioritySnap.Store(int64(clamped))
}

func (mp *managedPlugin) getPriority() int {
	return int(mp.prioritySnap.Load())
}

func parsePriorityInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case int:
		return ClampPluginPriority(v), true
	case int64:
		return ClampPluginPriority(int(v)), true
	case float64:
		return ClampPluginPriority(int(v)), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return ClampPluginPriority(int(n)), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return ClampPluginPriority(n), true
		}
	}
	return 0, false
}

func extractPluginPriorityFromMap(rawMap map[string]any, defPriority int) (int, bool) {
	if rawMap == nil {
		return ClampPluginPriority(defPriority), false
	}
	raw, exists := rawMap[reservedPluginPriorityKey]
	delete(rawMap, reservedPluginPriorityKey)
	if !exists {
		return ClampPluginPriority(defPriority), false
	}
	if p, ok := parsePriorityInt(raw); ok {
		return p, true
	}
	return ClampPluginPriority(defPriority), false
}

func marshalPluginConfigJSON(pubCfg map[string]any, priority int, custom bool) ([]byte, error) {
	if !custom {
		return json.Marshal(pubCfg)
	}
	withMeta := make(map[string]any, len(pubCfg)+1)
	for k, v := range pubCfg {
		withMeta[k] = v
	}
	withMeta[reservedPluginPriorityKey] = ClampPluginPriority(priority)
	return json.Marshal(withMeta)
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
	mp.cfgRev.Add(1)
}

func (mp *managedPlugin) getConfigMap() map[string]any {
	ptr := mp.mergedCfgSnap.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// Manager orchestrates built-in and external goja plugins, persistent plugin_kv, capability sandboxes,
// automatic circuit breaking, .zip hot installation, unified cron scheduling, and /plugins/<id>/* HTTP serving.
type Manager struct {
	mu                    sync.RWMutex
	store                 *sqlx.Store
	keys                  *config.KeyRing
	sealerMu              sync.RWMutex
	sealer                SecretSealer
	pluginsDir            string
	logger                *PluginLogger
	catalog               *i18n.Catalog
	providers             *ProviderRegistry
	cron                  *CronRegistry
	hookTimeout           time.Duration
	routeTimeout          time.Duration
	cronTimeout           time.Duration
	cronBaseUnit          time.Duration
	maxFailures           int
	customResolver        security.ResolverFunc
	allowLoopbackForTests bool

	siteInfoFn   atomic.Pointer[func() map[string]any]
	reqInspector atomic.Pointer[func(*http.Request) (bool, string)]

	fedWG   sync.WaitGroup
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
	sealer := normalizeSealer(opts.SecretSealer)
	if sealer == nil && opts.Keys != nil {
		sealer = normalizeSealer(opts.Keys.SecretBox())
	}
	mgr := &Manager{
		store:                 opts.Store,
		keys:                  opts.Keys,
		sealer:                sealer,
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
	mgr.cron = newCronRegistry(defaultMaxCronConcurrency, mgr.effectiveCronUnit, mgr.executeScheduledCron)
	return mgr
}

func (m *Manager) effectiveCronUnit() time.Duration {
	if m != nil && m.cronBaseUnit > 0 {
		return m.cronBaseUnit
	}
	return time.Second
}

func normalizeSealer(s SecretSealer) SecretSealer {
	if s == nil {
		return nil
	}
	rv := reflect.ValueOf(s)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		return nil
	}
	return s
}

// SetSecretSealer configures or replaces the SecretSealer used for sealing/opening "secret" schema fields.
func (m *Manager) SetSecretSealer(s SecretSealer) {
	if m == nil {
		return
	}
	m.sealerMu.Lock()
	m.sealer = normalizeSealer(s)
	m.sealerMu.Unlock()
}

func (m *Manager) getSealer() SecretSealer {
	if m == nil {
		return nil
	}
	m.sealerMu.RLock()
	s := m.sealer
	m.sealerMu.RUnlock()
	return s
}

func (m *Manager) unsealSecretValue(val string) string {
	if !strings.HasPrefix(val, "v1:") {
		return val
	}
	sealer := m.getSealer()
	if sealer == nil {
		return val
	}
	plain, err := sealer.Open(val)
	if err != nil {
		return ""
	}
	return plain
}

func (m *Manager) Providers() *ProviderRegistry { return m.providers }
func (m *Manager) Logger() *PluginLogger        { return m.logger }

// CronJobs returns snapshots of all active scheduled plugin tasks in the unified CronRegistry.
func (m *Manager) CronJobs() []CronJobSnapshot {
	if m == nil || m.cron == nil {
		return []CronJobSnapshot{}
	}
	return m.cron.ListJobs("")
}

// PluginCronJobs returns snapshots of scheduled tasks registered by pluginID.
func (m *Manager) PluginCronJobs(pluginID string) []CronJobSnapshot {
	if m == nil || m.cron == nil {
		return []CronJobSnapshot{}
	}
	return m.cron.ListJobs(pluginID)
}

// TriggerCronJob schedules one job (or all jobs of pluginID when name == "") for immediate execution.
func (m *Manager) TriggerCronJob(pluginID string, name string) int {
	if m == nil || m.cron == nil {
		return 0
	}
	return m.cron.Trigger(pluginID, name)
}

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
	mp, ok := m.getPlugin(idClean)
	if !ok {
		return 0, fmt.Errorf("plugin %q not found", idClean)
	}
	if hub := lookupKVHub(m.store); hub != nil {
		hub.flushMu.Lock()
		hub.mu.Lock()
		hub.gen++
		for pk := range hub.entries {
			if pk.pluginID == idClean {
				delete(hub.dirty, pk)
				delete(hub.entries, pk)
			}
		}
		hub.mu.Unlock()
		defer hub.flushMu.Unlock()
	}
	res, err := m.store.ExecBgContext(ctx, `DELETE FROM plugin_kv WHERE plugin_id = ?`, idClean)
	if err != nil {
		return 0, err
	}
	mp.mu.Lock()
	rt := mp.gojaRT
	mp.mu.Unlock()
	if rt != nil {
		rt.clearWordMatchers()
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
	var existing domain.PluginRecord
	err := sqlx.GetInto(
		ctx,
		m.store,
		&existing,
		`SELECT id, kind, version, is_enabled, config_json, secret_ciphertext, circuit_state, failure_count, last_error, installed_at, updated_at
		 FROM plugins WHERE id = ?`,
		manifest.ID,
	)

	var pubCfg map[string]any
	var secretCfg map[string]string
	effPriority := manifest.DefaultPriority()
	hasCustomPriority := false

	if errors.Is(err, sql.ErrNoRows) {
		defPub, defSec, _ := manifest.ValidateAndMergeConfig(nil, nil, nil)
		if sealer := m.getSealer(); sealer != nil {
			for k, v := range defSec {
				if v != "" && !strings.HasPrefix(v, "v1:") {
					if sealed, sErr := sealer.Seal(v); sErr == nil {
						defSec[k] = sealed
					}
				}
			}
		}
		pubCfg = defPub
		secretCfg = defSec
		cfgBytes, _ := marshalPluginConfigJSON(pubCfg, effPriority, false)
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
		effPriority, hasCustomPriority = extractPluginPriorityFromMap(rawPub, manifest.DefaultPriority())
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
		if sealer := m.getSealer(); sealer != nil {
			for k, v := range secretCfg {
				if v != "" && !strings.HasPrefix(v, "v1:") {
					if sealed, sErr := sealer.Seal(v); sErr == nil {
						secretCfg[k] = sealed
					}
				}
			}
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
	mp.setPriorityLocked(effPriority, hasCustomPriority)
	mp.rebuildConfigSnapshot()
	if m.providers != nil {
		m.providers.SetPluginPriority(manifest.ID, effPriority)
	}
	return mp, nil
}

func (m *Manager) sealSecrets(pluginID string, secrets map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(secrets)
	if err != nil {
		return "", err
	}
	if m.keys != nil {
		return m.keys.SealString(string(raw), "plugins.secret_ciphertext:"+pluginID)
	}
	if m.getSealer() != nil {
		return string(raw), nil
	}
	return "", nil
}

func (m *Manager) openSecretsLenient(pluginID string, ciphertext string) map[string]string {
	out := make(map[string]string)
	if ciphertext == "" {
		return out
	}
	if m.keys != nil && strings.HasPrefix(ciphertext, "enc:v1:") {
		plain, err := m.keys.OpenString(ciphertext, "plugins.secret_ciphertext:"+pluginID)
		if err != nil {
			m.logger.Write(pluginID, "warn", "secrets", "encrypted plugin secrets could not be opened (key changed); re-enter in admin console")
			return out
		}
		_ = json.Unmarshal([]byte(plain), &out)
		return out
	}
	if strings.HasPrefix(strings.TrimSpace(ciphertext), "{") {
		_ = json.Unmarshal([]byte(ciphertext), &out)
		return out
	}
	if m.keys != nil {
		plain, err := m.keys.OpenString(ciphertext, "plugins.secret_ciphertext:"+pluginID)
		if err != nil {
			m.logger.Write(pluginID, "warn", "secrets", "encrypted plugin secrets could not be opened (key changed); re-enter in admin console")
			return out
		}
		_ = json.Unmarshal([]byte(plain), &out)
	}
	return out
}

func (m *Manager) activateRuntime(ctx context.Context, mp *managedPlugin) error {
	if m.cron != nil {
		m.cron.UnregisterPlugin(mp.manifest.ID, true)
	}

	mp.mu.Lock()
	defer mp.mu.Unlock()

	m.providers.DetachPlugin(mp.manifest.ID)
	hasCronPerm := mp.manifest.HasPermission("cron")
	if hasCronPerm && m.cron != nil {
		m.cron.BeginPlugin(mp.manifest.ID)
	}

	if mp.manifest.Runtime == RuntimeBuiltin {
		if mp.builtin == nil {
			if hasCronPerm && m.cron != nil {
				m.cron.UnregisterPlugin(mp.manifest.ID, true)
			}
			return fmt.Errorf("plugin %q: missing builtin implementation", mp.manifest.ID)
		}
		hostCtx := &HostContext{
			PluginID:     mp.manifest.ID,
			KV:           mp.kv,
			Providers:    m.providers,
			cron:         m.cron,
			hasCronPerm:  hasCronPerm,
			logger:       m.logger,
			hostI18n:     m.catalog,
			getConfig:    mp.getConfigMap,
			unsealSecret: m.unsealSecretValue,
			dialer: &security.OutboundSafeDialer{
				AllowedHosts:          mp.manifest.EffectiveAllowedHosts(),
				Timeout:               5 * time.Second,
				Resolver:              m.customResolver,
				AllowLoopbackForTests: m.allowLoopbackForTests,
			},
		}
		if err := safeCallBuiltin(mp.manifest.ID, "OnEnable", func() error {
			return mp.builtin.OnEnable(ctx, hostCtx)
		}); err != nil {
			m.providers.DetachPlugin(mp.manifest.ID)
			if hasCronPerm && m.cron != nil {
				m.cron.UnregisterPlugin(mp.manifest.ID, true)
			}
			return err
		}
	} else {
		rt, err := newGojaPluginRuntime(gojaRuntimeOptions{
			manifest:              mp.manifest,
			pluginDir:             mp.pluginDir,
			kv:                    mp.kv,
			cron:                  m.cron,
			logger:                m.logger,
			hostI18n:              m.catalog,
			getConfig:             mp.getConfigMap,
			unsealSecret:          m.unsealSecretValue,
			getSiteInfo:           m.getSiteInfo,
			customResolver:        m.customResolver,
			allowLoopbackForTests: m.allowLoopbackForTests,
		})
		if err != nil {
			if hasCronPerm && m.cron != nil {
				m.cron.UnregisterPlugin(mp.manifest.ID, true)
			}
			return err
		}
		mp.gojaRT = rt

		// Optional JS lifecycle hook onEnable().
		if _, _, err := rt.invoke(ctx, m.hookTimeout, "onEnable"); err != nil {
			mp.gojaRT = nil
			rt.closeRuntime()
			if hasCronPerm && m.cron != nil {
				m.cron.UnregisterPlugin(mp.manifest.ID, true)
			}
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
		if mp.manifest.HasOIDCCapability() && rt.hasCallable("oidcAuthURL") && rt.hasCallable("oidcExchange") {
			adapter := newGojaOIDCAdapter(ctx, m, mp, rt)
			m.providers.RegisterOIDC(mp.manifest.ID, adapter)
		}
		if mp.manifest.HasFederationCapability() {
			m.providers.RegisterFederation(mp.manifest.ID, &gojaFederationAdapter{
				manager: m,
				mp:      mp,
			})
		}
	}

	if hasCronPerm && m.cron != nil {
		for _, spec := range mp.manifest.Cron {
			if !m.cron.HasJob(mp.manifest.ID, spec.Name) {
				_ = m.cron.RegisterJob(mp.manifest.ID, spec, CronSourceManifest, false)
			}
		}
	}
	mp.runtimeActive = true
	m.logger.Write(mp.manifest.ID, "info", "lifecycle", "plugin enabled v"+mp.manifest.Version)
	return nil
}

func (m *Manager) deactivateRuntime(ctx context.Context, mp *managedPlugin) {
	m.deactivateRuntimeOpts(ctx, mp, true)
}

func (m *Manager) deactivateRuntimeOpts(ctx context.Context, mp *managedPlugin, waitCron bool) {
	// Never pass wait=true from inside a cron worker goroutine of the same plugin!
	if m.cron != nil {
		m.cron.UnregisterPlugin(mp.manifest.ID, waitCron)
	}

	mp.mu.Lock()
	m.providers.DetachPlugin(mp.manifest.ID)
	runtimeKind := mp.manifest.Runtime
	builtin := mp.builtin
	rt := mp.gojaRT
	mp.gojaRT = nil
	wasRuntimeActive := mp.runtimeActive
	mp.runtimeActive = false
	mp.mu.Unlock()

	if runtimeKind == RuntimeBuiltin && builtin != nil && wasRuntimeActive {
		_ = safeCallBuiltin(mp.manifest.ID, "OnDisable", func() error {
			return builtin.OnDisable(ctx)
		})
	} else if rt != nil {
		_, _, _ = rt.invoke(ctx, m.hookTimeout, "onDisable")
		rt.closeRuntime()
	}

	if m.store != nil {
		flushCtx := ctx
		var cancel context.CancelFunc
		if flushCtx == nil || flushCtx.Err() != nil {
			flushCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		}
		if fErr := FlushPluginKV(flushCtx, m.store, mp.manifest.ID); fErr != nil {
			slog.Warn("plugin: failed to flush buffered kv on deactivate", "plugin_id", mp.manifest.ID, "err", fErr)
		}
		if cancel != nil {
			cancel()
		}
	}
	m.logger.Write(mp.manifest.ID, "info", "lifecycle", "plugin disabled")
}

func (m *Manager) executeScheduledCron(parentCtx context.Context, pluginID string, jobName string, handler string) error {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return nil
	}
	runCtx, runCancel := context.WithTimeout(parentCtx, m.cronTimeout)
	err := m.runSingleCron(runCtx, mp, handler)
	runCancel()
	if err != nil && !errors.Is(err, context.Canceled) {
		m.recordFailureWithOrigin(context.Background(), mp, "cron:"+jobName, err, true)
	} else if err == nil {
		m.recordSuccess(context.Background(), mp)
	}
	return err
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
		return safeCallBuiltin(mp.manifest.ID, "RunCron:"+handler, func() error {
			return bp.RunCron(ctx, handler)
		})
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
	if mp.enabled && mp.runtimeActive {
		mp.mu.Unlock()
		return nil
	}
	mp.enabled = true
	mp.updatedAt = time.Now().Unix()
	mp.mu.Unlock()

	if err := m.activateRuntime(ctx, mp); err != nil {
		mp.mu.Lock()
		mp.enabled = false
		mp.runtimeActive = false
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

func invalidateOIDCDiscoveryKV(ctx context.Context, kv *KVStore) {
	if kv == nil {
		return
	}
	items, err := kv.ListPrefix(ctx, "", 256, "")
	if err != nil {
		return
	}
	for _, it := range items {
		lk := strings.ToLower(it.Key)
		if strings.Contains(lk, "discovery") || strings.Contains(lk, "jwks") || strings.Contains(lk, "openid") {
			_ = kv.Delete(ctx, it.Key)
		}
	}
}

// UpdateConfig validates a partial/full config patch against ConfigSchema, seals secret fields with AES-256-GCM,
// persists to DB, atomically refreshes the live in-memory config snapshot without restarting VMs, and notifies
// optional onConfigChange(cfg) hooks (e.g. to reschedule dynamic cron jobs).
func (m *Manager) UpdateConfig(ctx context.Context, pluginID string, patch map[string]any, locale string) (PluginSummary, error) {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return PluginSummary{}, fmt.Errorf("plugin: %q not found", pluginID)
	}

	cleanPatch := make(map[string]any, len(patch))
	var patchPriority int
	hasPatchPriority := false
	for k, v := range patch {
		if k == reservedPluginPriorityKey {
			if p, pOk := parsePriorityInt(v); pOk {
				patchPriority = p
				hasPatchPriority = true
			}
			continue
		}
		cleanPatch[k] = v
	}

	mp.mu.Lock()
	nextPub, nextSec, err := mp.manifest.ValidateAndMergeConfig(cleanPatch, mp.publicConfig, mp.secretConfig)
	if err != nil {
		mp.mu.Unlock()
		return PluginSummary{}, err
	}
	if sealer := m.getSealer(); sealer != nil {
		for k, v := range nextSec {
			if v == "" {
				continue
			}
			rawPatch, inPatch := cleanPatch[k]
			rawStr, isStr := rawPatch.(string)
			needsSeal := false
			if inPatch && isStr && rawStr != SecretMaskSentinel && rawStr != mp.secretConfig[k] {
				needsSeal = true
			} else if !strings.HasPrefix(v, "v1:") {
				needsSeal = true
			}
			if needsSeal {
				sealed, sErr := sealer.Seal(v)
				if sErr != nil {
					mp.mu.Unlock()
					return PluginSummary{}, sErr
				}
				nextSec[k] = sealed
			}
		}
	}
	nextPriority := mp.priority
	nextCustomPrio := mp.customPriority
	if hasPatchPriority {
		nextPriority = patchPriority
		nextCustomPrio = true
	}
	cfgBytes, err := marshalPluginConfigJSON(nextPub, nextPriority, nextCustomPrio)
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
	mp.setPriorityLocked(nextPriority, nextCustomPrio)
	mp.updatedAt = now
	mp.rebuildConfigSnapshot()
	isActive := mp.enabled && mp.circuitState == CircuitClosed
	runtimeKind := mp.manifest.Runtime
	hasOIDCCap := mp.manifest.HasOIDCCapability()
	bp := mp.builtin
	rt := mp.gojaRT
	cfgSnap := mp.getConfigMap()
	mp.mu.Unlock()

	if hasPatchPriority && m.providers != nil {
		m.providers.SetPluginPriority(pluginID, nextPriority)
	}

	if _, err := m.store.ExecContext(
		ctx,
		`UPDATE plugins SET config_json = ?, secret_ciphertext = ?, updated_at = ? WHERE id = ?`,
		string(cfgBytes), secCipher, now, pluginID,
	); err != nil {
		return PluginSummary{}, err
	}

	if rt != nil {
		rt.resetHTTPClient()
	}

	var activeOIDCProvs []OIDCProvider
	if m.providers != nil {
		activeOIDCProvs = m.providers.OIDCProvidersForPlugin(pluginID)
	}
	isOIDCPlugin := hasOIDCCap || len(activeOIDCProvs) > 0
	if isOIDCPlugin {
		invalidateOIDCDiscoveryKV(ctx, mp.kv)
		if bp != nil {
			if inv, ok := bp.(OIDCDiscoveryInvalidator); ok {
				_ = safeCallBuiltin(pluginID, "InvalidateOIDCDiscovery", func() error {
					inv.InvalidateOIDCDiscovery()
					return nil
				})
			}
		}
		for _, prov := range activeOIDCProvs {
			if inv, ok := prov.(OIDCDiscoveryInvalidator); ok {
				inv.InvalidateOIDCDiscovery()
			}
		}
		if rt != nil {
			rt.resetPool()
		}
	}

	if isActive {
		cfgCopy := make(map[string]any, len(cfgSnap))
		for k, v := range cfgSnap {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "v1:") {
				cfgCopy[k] = m.unsealSecretValue(s)
			} else {
				cfgCopy[k] = v
			}
		}
		if runtimeKind == RuntimeBuiltin && bp != nil {
			_, hasListener := bp.(ConfigChangeListener)
			_, hasInvalidator := bp.(OIDCDiscoveryInvalidator)
			if hasListener {
				if cErr := safeCallBuiltin(pluginID, "OnConfigChange", func() error {
					return bp.(ConfigChangeListener).OnConfigChange(ctx, cfgCopy)
				}); cErr != nil {
					m.recordFailure(ctx, mp, "config_change", cErr)
				} else {
					m.recordSuccess(ctx, mp)
				}
			} else if isOIDCPlugin && !hasInvalidator {
				m.deactivateRuntime(ctx, mp)
				if aErr := m.activateRuntime(ctx, mp); aErr != nil {
					m.recordFailure(ctx, mp, "config_change", aErr)
				}
			}
		} else if rt != nil && rt.hasCallable("onConfigChange") {
			if _, _, cErr := rt.invoke(ctx, m.hookTimeout, "onConfigChange", cfgCopy); cErr != nil {
				m.recordFailure(ctx, mp, "config_change", cErr)
			} else {
				m.recordSuccess(ctx, mp)
			}
		}
		if isOIDCPlugin && rt != nil && m.providers != nil && rt.hasCallable("oidcAuthURL") && rt.hasCallable("oidcExchange") {
			m.providers.DetachOIDCPlugin(pluginID)
			adapter := newGojaOIDCAdapter(ctx, m, mp, rt)
			m.providers.RegisterOIDC(pluginID, adapter)
		}
	}

	m.logger.Write(pluginID, "info", "config", "configuration hot-reloaded")
	return m.toSummary(mp, locale), nil
}

// SaveConfig validates, seals, and persists plugin configuration (preserving masked "********" secret sentinels).
func (m *Manager) SaveConfig(ctx context.Context, pluginID string, patch map[string]any, locale string) (PluginSummary, error) {
	return m.UpdateConfig(ctx, pluginID, patch, locale)
}

// GetConfigRedacted returns a copy of pluginID's configuration suitable for the Admin UI,
// masking any non-empty "secret" schema field to "********".
func (m *Manager) GetConfigRedacted(pluginID string) (map[string]any, bool) {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return nil, false
	}
	mp.mu.Lock()
	defer mp.mu.Unlock()
	return buildRedactedConfigLocked(mp), true
}

// LocalizedPlugins returns secret-masked localized plugin summaries for the Admin UI.
func (m *Manager) LocalizedPlugins(locale string) []PluginSummary {
	return m.List(locale)
}

func buildRedactedConfigLocked(mp *managedPlugin) map[string]any {
	out := make(map[string]any, len(mp.publicConfig)+len(mp.secretConfig))
	for k, v := range mp.publicConfig {
		out[k] = v
	}
	for _, f := range mp.manifest.ConfigSchema {
		if f.IsSecret() {
			if mp.secretConfig[f.Key] != "" {
				out[f.Key] = SecretMaskSentinel
			} else {
				out[f.Key] = ""
			}
		}
	}
	return out
}

// ExportZip safely packages `./plugins/<id>` into a `.zip` archive written to w,
// validating a safe plugin ID, rejecting symlinks, and stripping raw unmasked secrets from local state files.
func (m *Manager) ExportZip(pluginID string, w io.Writer) error {
	if m == nil {
		return errors.New("plugin: manager is nil")
	}
	if w == nil {
		return errors.New("plugin: export writer is nil")
	}
	idClean := strings.TrimSpace(pluginID)
	if !validPluginIDRe.MatchString(idClean) {
		return fmt.Errorf("plugin: invalid plugin id %q", pluginID)
	}
	if strings.TrimSpace(m.pluginsDir) == "" {
		return errors.New("plugin: external plugins directory is not configured")
	}

	targetDir := filepath.Join(m.pluginsDir, idClean)
	targetLstat, err := os.Lstat(targetDir)
	if err != nil {
		return fmt.Errorf("plugin %q: stat plugin directory: %w", idClean, err)
	}
	if targetLstat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("plugin %q: symlink plugin directory rejected", idClean)
	}
	if !targetLstat.IsDir() {
		return fmt.Errorf("plugin %q: path is not a directory", idClean)
	}

	realPluginsRoot, err := filepath.EvalSymlinks(m.pluginsDir)
	if err != nil {
		return fmt.Errorf("plugin %q: resolve plugins root: %w", idClean, err)
	}
	realPluginDir, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		return fmt.Errorf("plugin %q: resolve plugin dir: %w", idClean, err)
	}
	if realPluginDir == realPluginsRoot || !isSubPath(realPluginsRoot, realPluginDir) {
		return fmt.Errorf("plugin %q: plugin directory escapes plugins root", idClean)
	}

	secretFieldKeys := make(map[string]bool)
	knownRawSecrets := make(map[string]bool)
	if mp, ok := m.getPlugin(idClean); ok {
		for _, f := range mp.manifest.ConfigSchema {
			if f.IsSecret() {
				secretFieldKeys[f.Key] = true
			}
		}
		mp.mu.Lock()
		for _, val := range mp.secretConfig {
			if val != "" {
				knownRawSecrets[val] = true
				if plain := m.unsealSecretValue(val); plain != "" {
					knownRawSecrets[plain] = true
				}
			}
		}
		mp.mu.Unlock()
	}

	manifestPath := filepath.Join(realPluginDir, "manifest.json")
	if mfInfo, mErr := os.Lstat(manifestPath); mErr == nil {
		if mfInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin %q: symlink manifest.json rejected", idClean)
		}
		if mfBytes, rErr := os.ReadFile(manifestPath); rErr == nil {
			if parsed, pErr := ParseManifest(mfBytes); pErr == nil {
				for _, f := range parsed.ConfigSchema {
					if f.IsSecret() {
						secretFieldKeys[f.Key] = true
					}
				}
			}
		}
	}

	type exportEntry struct {
		relSlash string
		absPath  string
		modTime  time.Time
	}
	var entries []exportEntry
	var totalBytes int64

	walkErr := filepath.WalkDir(realPluginDir, func(curPath string, _ os.DirEntry, walkInErr error) error {
		if walkInErr != nil {
			return walkInErr
		}
		linfo, sErr := os.Lstat(curPath)
		if sErr != nil {
			return sErr
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin %q: symlink entry %q rejected during export", idClean, curPath)
		}
		if curPath == realPluginDir {
			return nil
		}
		rel, rErr := filepath.Rel(realPluginDir, curPath)
		if rErr != nil {
			return rErr
		}
		relSlash := filepath.ToSlash(rel)
		baseLower := strings.ToLower(filepath.Base(curPath))

		if shouldSkipPluginExportFile(baseLower, linfo.IsDir()) {
			if linfo.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if linfo.IsDir() {
			return nil
		}
		if !linfo.Mode().IsRegular() {
			return fmt.Errorf("plugin %q: irregular file %q rejected during export", idClean, relSlash)
		}
		if linfo.Size() > MaxZipSingleBytes {
			return fmt.Errorf("plugin %q: file %q exceeds %d MiB export limit", idClean, relSlash, MaxZipSingleBytes>>20)
		}
		totalBytes += linfo.Size()
		if totalBytes > MaxZipArchiveBytes {
			return fmt.Errorf("plugin %q: archive exceeds %d MiB export limit", idClean, MaxZipArchiveBytes>>20)
		}
		entries = append(entries, exportEntry{
			relSlash: relSlash,
			absPath:  curPath,
			modTime:  linfo.ModTime().UTC(),
		})
		if len(entries) > MaxZipEntries {
			return fmt.Errorf("plugin %q: file count exceeds %d export limit", idClean, MaxZipEntries)
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relSlash < entries[j].relSlash
	})

	zw := zip.NewWriter(w)
	for _, ent := range entries {
		rawBytes, rErr := os.ReadFile(ent.absPath)
		if rErr != nil {
			_ = zw.Close()
			return rErr
		}
		payloadBytes := sanitizeExportedPluginFileBytes(ent.relSlash, rawBytes, secretFieldKeys, knownRawSecrets)

		hdr := &zip.FileHeader{
			Name:     ent.relSlash,
			Method:   zip.Deflate,
			Modified: ent.modTime,
		}
		hdr.SetMode(0o644)
		fw, cErr := zw.CreateHeader(hdr)
		if cErr != nil {
			_ = zw.Close()
			return cErr
		}
		if _, wErr := fw.Write(payloadBytes); wErr != nil {
			_ = zw.Close()
			return wErr
		}
	}
	return zw.Close()
}

func shouldSkipPluginExportFile(baseLower string, isDir bool) bool {
	if baseLower == ".git" || baseLower == ".ds_store" ||
		strings.HasPrefix(baseLower, ".tmp") || strings.HasPrefix(baseLower, ".bak") ||
		strings.HasSuffix(baseLower, ".tmp") || strings.HasSuffix(baseLower, ".bak") {
		return true
	}
	if isDir {
		return baseLower == ".secrets" || baseLower == ".private"
	}
	if baseLower == ".env" || strings.HasPrefix(baseLower, ".env.") ||
		strings.HasPrefix(baseLower, ".secret") ||
		baseLower == "secrets.json" || baseLower == "secret.json" ||
		strings.HasSuffix(baseLower, ".key") || strings.HasSuffix(baseLower, ".pem") {
		return true
	}
	return false
}

func sanitizeExportedPluginFileBytes(
	relSlash string,
	raw []byte,
	secretFieldKeys map[string]bool,
	knownRawSecrets map[string]bool,
) []byte {
	lowerRel := strings.ToLower(relSlash)
	if !strings.HasSuffix(lowerRel, ".json") {
		return raw
	}
	if lowerRel == "manifest.json" {
		var mfMap map[string]any
		if err := json.Unmarshal(raw, &mfMap); err != nil {
			return raw
		}
		changed := false
		if schemaRaw, ok := mfMap["config_schema"].([]any); ok {
			for _, item := range schemaRaw {
				fieldMap, mOk := item.(map[string]any)
				if !mOk {
					continue
				}
				isSec, _ := fieldMap["secret"].(bool)
				fType, _ := fieldMap["type"].(string)
				if isSec || strings.EqualFold(strings.TrimSpace(fType), "secret") {
					if defVal, hasDef := fieldMap["default"]; hasDef && defVal != nil && defVal != "" {
						delete(fieldMap, "default")
						changed = true
					}
				}
			}
		}
		if !changed {
			return raw
		}
		if out, mErr := json.MarshalIndent(mfMap, "", "  "); mErr == nil {
			return out
		}
		return raw
	}

	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	cleaned, changed := stripSecretsFromJSONNode(doc, secretFieldKeys, knownRawSecrets)
	if !changed {
		return raw
	}
	if out, mErr := json.MarshalIndent(cleaned, "", "  "); mErr == nil {
		return out
	}
	return raw
}

func stripSecretsFromJSONNode(
	node any,
	secretFieldKeys map[string]bool,
	knownRawSecrets map[string]bool,
) (any, bool) {
	switch v := node.(type) {
	case map[string]any:
		changed := false
		out := make(map[string]any, len(v))
		for k, val := range v {
			if secretFieldKeys[k] || isLikelySecretStateKey(k) {
				if s, isStr := val.(string); !isStr || (s != "" && s != SecretMaskSentinel) {
					changed = true
					continue
				}
			}
			if s, isStr := val.(string); isStr && s != "" && knownRawSecrets[s] {
				changed = true
				continue
			}
			nextVal, subChanged := stripSecretsFromJSONNode(val, secretFieldKeys, knownRawSecrets)
			if subChanged {
				changed = true
			}
			out[k] = nextVal
		}
		return out, changed
	case []any:
		changed := false
		out := make([]any, 0, len(v))
		for _, elem := range v {
			if s, isStr := elem.(string); isStr && s != "" && knownRawSecrets[s] {
				changed = true
				continue
			}
			nextElem, subChanged := stripSecretsFromJSONNode(elem, secretFieldKeys, knownRawSecrets)
			if subChanged {
				changed = true
			}
			out = append(out, nextElem)
		}
		return out, changed
	default:
		return node, false
	}
}

func isLikelySecretStateKey(k string) bool {
	lk := strings.ToLower(strings.TrimSpace(k))
	switch lk {
	case "secret", "secrets", "api_key", "apikey", "api_token", "apitoken",
		"token", "access_token", "refresh_token", "private_key", "password",
		"passwd", "client_secret", "webhook_secret", "signing_secret", "secret_ciphertext":
		return true
	}
	return strings.HasSuffix(lk, "_secret") || strings.HasSuffix(lk, "_api_key") || strings.HasSuffix(lk, "_token")
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

	// Check existing plugin conflict and capture previous version + enabled state for upgrade lifecycle.
	targetDir := filepath.Join(m.pluginsDir, manifest.ID)
	existingMP, exists := m.getPlugin(manifest.ID)
	prevVersion := ""
	isUpgrade := false
	wasEnabled := false
	if exists {
		if existingMP.kind == "builtin" {
			return PluginSummary{}, fmt.Errorf("plugin %q conflicts with a built-in plugin", manifest.ID)
		}
		if !overwrite {
			return PluginSummary{}, fmt.Errorf("plugin %q is already installed (set overwrite=true to upgrade)", manifest.ID)
		}
		existingMP.mu.Lock()
		wasEnabled = existingMP.enabled
		existingMP.mu.Unlock()
		prevVersion = existingMP.manifest.Version
		isUpgrade = true
		if dErr := m.Disable(ctx, manifest.ID); dErr != nil {
			slog.Warn("plugin: disable before upgrade failed", "plugin_id", manifest.ID, "err", dErr)
		}
	} else {
		if _, statErr := os.Stat(targetDir); statErr == nil && !overwrite {
			return PluginSummary{}, fmt.Errorf("plugin directory %q already exists", manifest.ID)
		}
		var dbRec domain.PluginRecord
		if dbErr := sqlx.GetInto(ctx, m.store, &dbRec, `SELECT version, is_enabled FROM plugins WHERE id = ?`, manifest.ID); dbErr == nil {
			if dbRec.Version != "" {
				prevVersion = dbRec.Version
				isUpgrade = true
			}
			if overwrite && dbRec.IsEnabled {
				wasEnabled = true
			}
		}
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

	// Optional bounded onInstall({ previous_version, version, is_upgrade }) lifecycle hook on fresh install or version upgrade.
	if !isUpgrade || prevVersion != manifest.Version {
		installRT, rtErr := newGojaPluginRuntime(gojaRuntimeOptions{
			manifest:              mp.manifest,
			pluginDir:             mp.pluginDir,
			kv:                    mp.kv,
			cron:                  m.cron,
			logger:                m.logger,
			hostI18n:              m.catalog,
			getConfig:             mp.getConfigMap,
			unsealSecret:          m.unsealSecretValue,
			getSiteInfo:           m.getSiteInfo,
			customResolver:        m.customResolver,
			allowLoopbackForTests: m.allowLoopbackForTests,
		})
		if rtErr != nil {
			// Error details may echo plugin-controlled HTTP data; they go to the plugin's own log only.
			slog.Warn("plugin: failed to initialize runtime for onInstall hook (see plugin log)", "plugin_id", manifest.ID, "version", manifest.Version)
			m.logger.Write(manifest.ID, "warn", "install", "onInstall init error: "+rtErr.Error())
		} else {
			if installRT.hasCallable("onInstall") {
				if _, _, hErr := installRT.invoke(ctx, m.hookTimeout, "onInstall", map[string]any{
					"previous_version": prevVersion,
					"version":          manifest.Version,
					"is_upgrade":       isUpgrade,
				}); hErr != nil {
					slog.Warn("plugin: onInstall hook failed", "plugin_id", manifest.ID, "version", manifest.Version, "is_upgrade", isUpgrade, "err", hErr)
					m.logger.Write(manifest.ID, "warn", "install", "onInstall error: "+hErr.Error())
				}
			}
			installRT.closeRuntime()
		}
	}

	m.logger.Write(manifest.ID, "info", "install", "installed external plugin v"+manifest.Version)
	if wasEnabled {
		if eErr := m.Enable(ctx, manifest.ID); eErr != nil {
			slog.Warn("plugin: failed to re-enable upgraded plugin (see plugin log)", "plugin_id", manifest.ID, "version", manifest.Version)
			m.logger.Write(manifest.ID, "warn", "install", "re-enable after upgrade failed: "+eErr.Error())
			return m.toSummary(mp, locale), fmt.Errorf("plugin %q upgraded to v%s but failed to re-enable: %w", manifest.ID, manifest.Version, eErr)
		}
	}
	return m.toSummary(mp, locale), nil
}

// Uninstall disables an external plugin, invokes its optional onUninstall() hook, and deletes its directory, KV keys, DB row, and logs.
func (m *Manager) Uninstall(ctx context.Context, pluginID string) error {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return fmt.Errorf("plugin: %q not found", pluginID)
	}
	if mp.kind == "builtin" {
		return fmt.Errorf("plugin %q is built-in and cannot be uninstalled (use Disable instead)", pluginID)
	}
	_ = m.Disable(ctx, pluginID)

	// Best-effort bounded onUninstall() lifecycle hook before deleting plugin_kv, config, and directory.
	if mp.manifest.Runtime == RuntimeBuiltin && mp.builtin != nil {
		if listener, lOk := mp.builtin.(UninstallListener); lOk {
			_ = safeCallBuiltin(pluginID, "OnUninstall", func() error {
				return listener.OnUninstall(ctx)
			})
		}
	} else if mp.manifest.Runtime != RuntimeBuiltin && mp.pluginDir != "" {
		if tempRT, rtErr := newGojaPluginRuntime(gojaRuntimeOptions{
			manifest:              mp.manifest,
			pluginDir:             mp.pluginDir,
			kv:                    mp.kv,
			cron:                  m.cron,
			logger:                m.logger,
			hostI18n:              m.catalog,
			getConfig:             mp.getConfigMap,
			unsealSecret:          m.unsealSecretValue,
			getSiteInfo:           m.getSiteInfo,
			customResolver:        m.customResolver,
			allowLoopbackForTests: m.allowLoopbackForTests,
		}); rtErr == nil {
			_, _, _ = tempRT.invoke(ctx, m.hookTimeout, "onUninstall")
			tempRT.closeRuntime()
		}
	}
	if m.cron != nil {
		m.cron.UnregisterPlugin(pluginID, true)
	}

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

	if hub := lookupKVHub(m.store); hub != nil {
		hub.flushMu.Lock()
		hub.mu.Lock()
		hub.gen++
		for pk := range hub.entries {
			if pk.pluginID == pluginID {
				delete(hub.dirty, pk)
				delete(hub.entries, pk)
			}
		}
		hub.mu.Unlock()
		defer hub.flushMu.Unlock()
	}

	if err := m.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_kv WHERE plugin_id = ?`, pluginID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM plugins WHERE id = ?`, pluginID)
		return err
	}); err != nil {
		return fmt.Errorf("plugin %q: database cleanup failed: %w", pluginID, err)
	}
	m.logger.RemoveLogs(pluginID)
	return nil
}

// SetPriority updates a single plugin's execution priority (smaller value runs earlier in chains),
// immediately re-sorting all active hook/slot/provider chains and persisting the order.
func (m *Manager) SetPriority(ctx context.Context, pluginID string, priority int, locale string) (PluginSummary, error) {
	mp, ok := m.getPlugin(pluginID)
	if !ok {
		return PluginSummary{}, fmt.Errorf("plugin: %q not found", pluginID)
	}
	clamped := ClampPluginPriority(priority)
	now := time.Now().Unix()

	mp.mu.Lock()
	cfgBytes, err := marshalPluginConfigJSON(mp.publicConfig, clamped, true)
	if err != nil {
		mp.mu.Unlock()
		return PluginSummary{}, err
	}
	mp.setPriorityLocked(clamped, true)
	mp.updatedAt = now
	mp.mu.Unlock()

	if m.providers != nil {
		m.providers.SetPluginPriority(pluginID, clamped)
	}
	if _, err := m.store.ExecContext(
		ctx,
		`UPDATE plugins SET config_json = ?, updated_at = ? WHERE id = ?`,
		string(cfgBytes), now, pluginID,
	); err != nil {
		return PluginSummary{}, err
	}
	return m.toSummary(mp, locale), nil
}

// Reorder assigns spaced execution priorities (10, 20, 30, ...) according to orderedIDs,
// preserving relative order for any registered plugins omitted from orderedIDs.
func (m *Manager) Reorder(ctx context.Context, orderedIDs []string, locale string) ([]PluginSummary, error) {
	m.mu.RLock()
	existing := make([]*managedPlugin, 0, len(m.plugins))
	byID := make(map[string]*managedPlugin, len(m.plugins))
	for id, mp := range m.plugins {
		existing = append(existing, mp)
		byID[id] = mp
	}
	m.mu.RUnlock()

	sortManagedPluginsByPriority(existing)

	seen := make(map[string]bool, len(existing))
	finalOrder := make([]*managedPlugin, 0, len(existing))
	for _, rawID := range orderedIDs {
		id := strings.TrimSpace(rawID)
		if id == "" || seen[id] {
			continue
		}
		mp, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("plugin: %q not found", id)
		}
		seen[id] = true
		finalOrder = append(finalOrder, mp)
	}
	for _, mp := range existing {
		if !seen[mp.manifest.ID] {
			finalOrder = append(finalOrder, mp)
		}
	}

	type pendingWrite struct {
		id       string
		mp       *managedPlugin
		priority int
		cfgJSON  string
	}
	now := time.Now().Unix()
	writes := make([]pendingWrite, 0, len(finalOrder))
	for idx, mp := range finalOrder {
		prio := ClampPluginPriority((idx + 1) * 10)
		mp.mu.Lock()
		cfgBytes, err := marshalPluginConfigJSON(mp.publicConfig, prio, true)
		mp.mu.Unlock()
		if err != nil {
			return nil, err
		}
		writes = append(writes, pendingWrite{
			id:       mp.manifest.ID,
			mp:       mp,
			priority: prio,
			cfgJSON:  string(cfgBytes),
		})
	}

	if err := m.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		for _, w := range writes {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE plugins SET config_json = ?, updated_at = ? WHERE id = ?`,
				w.cfgJSON, now, w.id,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	for _, w := range writes {
		w.mp.mu.Lock()
		w.mp.setPriorityLocked(w.priority, true)
		w.mp.updatedAt = now
		w.mp.mu.Unlock()
		if m.providers != nil {
			m.providers.SetPluginPriority(w.id, w.priority)
		}
	}
	return m.List(locale), nil
}

func sortManagedPluginsByPriority(mps []*managedPlugin) {
	sort.Slice(mps, func(i, j int) bool {
		pi := mps[i].getPriority()
		pj := mps[j].getPriority()
		if pi != pj {
			return pi < pj
		}
		return mps[i].manifest.ID < mps[j].manifest.ID
	})
}

// List returns secret-masked summaries of all registered plugins sorted by execution priority (then ID).
func (m *Manager) List(locale string) []PluginSummary {
	m.mu.RLock()
	mps := make([]*managedPlugin, 0, len(m.plugins))
	for _, mp := range m.plugins {
		mps = append(mps, mp)
	}
	m.mu.RUnlock()

	sortManagedPluginsByPriority(mps)

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
	cronJobs := m.PluginCronJobs(mp.manifest.ID)
	if len(cronJobs) == 0 {
		cronJobs = nil
	}

	mp.mu.Lock()
	defer mp.mu.Unlock()

	pubCopy := make(map[string]any, len(mp.publicConfig))
	for k, v := range mp.publicConfig {
		pubCopy[k] = v
	}
	secSet := make(map[string]bool)
	for _, f := range mp.manifest.ConfigSchema {
		if f.IsSecret() {
			secSet[f.Key] = mp.secretConfig[f.Key] != ""
		}
	}
	redactedCfg := buildRedactedConfigLocked(mp)
	effectiveHosts := computeEffectiveOutboundHosts(mp.manifest, mp.getConfigMap(), func(v any) any {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "v1:") {
			return m.unsealSecretValue(s)
		}
		return v
	}, m.allowLoopbackForTests)
	return PluginSummary{
		ID:            mp.manifest.ID,
		Kind:          mp.kind,
		Runtime:       mp.manifest.Runtime,
		Version:       mp.manifest.Version,
		Priority:      mp.priority,
		Name:          ResolveLocalized(mp.manifest.Name, locale),
		Description:   ResolveLocalized(mp.manifest.Description, locale),
		Author:        mp.manifest.Author,
		Homepage:      mp.manifest.Homepage,
		IsEnabled:     mp.enabled,
		CircuitState:  mp.circuitState,
		FailureCount:  mp.failureCount,
		LastError:     mp.lastError,
		Permissions:   append([]string(nil), mp.manifest.Permissions...),
		Hooks:         append([]string(nil), mp.manifest.Hooks...),
		AllowedHosts:  effectiveHosts,
		ConfigSchema:  mp.manifest.ConfigSchema,
		PublicConfig:  pubCopy,
		Config:        redactedCfg,
		SecretKeysSet: secSet,
		CronJobs:      cronJobs,
		InstalledAt:   mp.installedAt,
		UpdatedAt:     mp.updatedAt,
	}
}

// RenderHooksStateHash computes a fast SHA-256 hex digest over all currently enabled plugins
// with render capability (priority order, ID, version, circuit state, and config hash).
// Returns "" when zero render plugins are enabled so callers can bypass cache key expansion.
func (m *Manager) RenderHooksStateHash() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	candidates := make([]*managedPlugin, 0, len(m.plugins))
	for _, mp := range m.plugins {
		if mp.manifest.HasRenderCapability() {
			candidates = append(candidates, mp)
		}
	}
	m.mu.RUnlock()

	if len(candidates) == 0 {
		return ""
	}
	sortManagedPluginsByPriority(candidates)

	h := sha256.New()
	enabledCount := 0
	for _, mp := range candidates {
		mp.mu.Lock()
		if !mp.enabled || mp.circuitState != CircuitClosed {
			mp.mu.Unlock()
			continue
		}
		enabledCount++
		prio := mp.priority
		pubBytes, _ := json.Marshal(mp.publicConfig)
		secKeys := make([]string, 0, len(mp.secretConfig))
		for k := range mp.secretConfig {
			secKeys = append(secKeys, k)
		}
		sort.Strings(secKeys)
		cfgDigest := sha256.New()
		_, _ = cfgDigest.Write(pubBytes)
		for _, k := range secKeys {
			_, _ = cfgDigest.Write([]byte{0})
			_, _ = cfgDigest.Write([]byte(k))
			_, _ = cfgDigest.Write([]byte{'='})
			_, _ = cfgDigest.Write([]byte(mp.secretConfig[k]))
		}
		circuit := mp.circuitState
		mp.mu.Unlock()

		_, _ = h.Write([]byte(strconv.Itoa(prio)))
		_, _ = h.Write([]byte{'|'})
		_, _ = h.Write([]byte(mp.manifest.ID))
		_, _ = h.Write([]byte{'|'})
		_, _ = h.Write([]byte(mp.manifest.Version))
		_, _ = h.Write([]byte{'|'})
		_, _ = h.Write([]byte(circuit))
		_, _ = h.Write([]byte{'|'})
		_, _ = h.Write(cfgDigest.Sum(nil))
		_, _ = h.Write([]byte{'\n'})
	}
	if enabledCount == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DispatchRenderPreMarkdown dispatches render.pre_markdown across enabled render plugins and returns updated content_md.
func (m *Manager) DispatchRenderPreMarkdown(ctx context.Context, raw ArticleHookPayload) string {
	if m == nil {
		return raw.ContentMD
	}
	return m.DispatchArticleHook(ctx, HookRenderPreMarkdown, raw).ContentMD
}

// DispatchRenderPreMarkdownFull dispatches render.pre_markdown and returns both updated content_md and summary_md.
func (m *Manager) DispatchRenderPreMarkdownFull(ctx context.Context, raw ArticleHookPayload) (contentMD, summaryMD string) {
	if m == nil {
		return raw.ContentMD, raw.SummaryMD
	}
	out := m.DispatchArticleHook(ctx, HookRenderPreMarkdown, raw)
	return out.ContentMD, out.SummaryMD
}

// DispatchRenderPostHTML dispatches render.post_html across enabled render plugins and returns updated HTML.
func (m *Manager) DispatchRenderPostHTML(ctx context.Context, raw ArticleHookPayload) string {
	if m == nil {
		return raw.HTML
	}
	return m.DispatchArticleHook(ctx, HookRenderPostHTML, raw).HTML
}

// DispatchRenderDirective invokes custom markdown directive handlers registered via volo.registerDirective
// or hooks["render.directive"] with full timeout and circuit-breaker protection.
func (m *Manager) DispatchRenderDirective(ctx context.Context, name string, attrs map[string]string, body string) (string, bool) {
	if m == nil {
		return "", false
	}
	normName := strings.ToLower(strings.TrimSpace(name))
	if normName == "" || normName == "lock" {
		return "", false
	}

	active := m.snapshotEnabledPlugins()
	for _, mp := range active {
		if !mp.manifest.HasRenderCapability() {
			continue
		}

		mp.mu.Lock()
		enabled := mp.enabled && mp.circuitState == CircuitClosed
		rt := mp.gojaRT
		bp := mp.builtin
		runtimeKind := mp.manifest.Runtime
		mp.mu.Unlock()
		if !enabled {
			continue
		}

		if runtimeKind == RuntimeBuiltin && bp != nil {
			dCtx, cancel := context.WithTimeout(ctx, m.hookTimeout)
			attrsCopy := make(map[string]string, len(attrs))
			for k, v := range attrs {
				attrsCopy[k] = v
			}
			res, err := bp.OnHook(dCtx, HookRenderDirective, map[string]any{
				"name":  normName,
				"attrs": attrsCopy,
				"body":  body,
			})
			cancel()
			if err != nil {
				m.recordFailure(ctx, mp, "directive:"+normName, err)
				continue
			}
			switch v := res.(type) {
			case string:
				if v != "" {
					m.recordSuccess(ctx, mp)
					return v, true
				}
			case map[string]any:
				if h, ok := v["html"].(string); ok && h != "" {
					m.recordSuccess(ctx, mp)
					return h, true
				}
			}
			continue
		}

		if rt != nil {
			htmlOut, handled, err := rt.invokeDirective(ctx, m.hookTimeout, normName, attrs, body)
			if err != nil {
				m.recordFailure(ctx, mp, "directive:"+normName, err)
				continue
			}
			if handled {
				m.recordSuccess(ctx, mp)
				return htmlOut, true
			}
		}
	}
	return "", false
}

// DispatchFederationArticle asynchronously invokes all enabled plugins with "federation" capability,
// calling their publish / deliver handler with timeout, circuit-breaker protection, and plugin log entries.
func (m *Manager) DispatchFederationArticle(ctx context.Context, action string, articlePayload map[string]any) {
	if m == nil {
		return
	}
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return
	}

	normAction := strings.ToLower(strings.TrimSpace(action))
	if normAction == "" {
		normAction = "publish"
	}
	safePayload := sanitizeFederationPayloadMap(articlePayload)
	providers := m.providers.Federations()
	if len(providers) == 0 {
		return
	}

	var baseCtx context.Context
	var callerDeadline time.Time
	var hasDeadline bool
	if ctx != nil {
		baseCtx = context.WithoutCancel(ctx)
		callerDeadline, hasDeadline = ctx.Deadline()
	} else {
		baseCtx = context.Background()
	}

	for _, prov := range providers {
		prov := prov
		m.fedWG.Add(1)
		go func() {
			defer m.fedWG.Done()
			defer func() {
				if rec := recover(); rec != nil {
					m.logger.Write(prov.Name(), "error", "federation:"+normAction, fmt.Sprintf("recovered panic: %v", rec))
				}
			}()

			timeout := m.routeTimeout
			if hasDeadline {
				rem := time.Until(callerDeadline)
				if rem <= 0 {
					timeout = 1 * time.Millisecond
				} else if rem < timeout {
					timeout = rem
				}
			}
			fCtx, cancel := context.WithTimeout(baseCtx, timeout)
			defer cancel()

			payloadCopy := cloneArbitraryMap(safePayload)
			if ga, isGoja := prov.(*gojaFederationAdapter); isGoja {
				_ = ga.Publish(fCtx, normAction, payloadCopy)
				return
			}

			mp, hasMP := m.getPlugin(prov.Name())
			if err := prov.Publish(fCtx, normAction, payloadCopy); err != nil {
				if hasMP {
					m.recordFailure(fCtx, mp, "federation:"+normAction, err)
				} else {
					m.logger.Write(prov.Name(), "error", "federation:"+normAction, err.Error())
				}
				return
			}
			if hasMP {
				m.recordSuccess(fCtx, mp)
			}
			m.logger.Write(prov.Name(), "info", "federation:"+normAction, "federation "+normAction+" completed")
		}()
	}
}

// DispatchArticleHook safely dispatches an article lifecycle/render hook across enabled plugins,
// strictly redacting password-protected articles and stripping `:::lock` blocks (10th Privacy Surface).
func (m *Manager) DispatchArticleHook(ctx context.Context, hookName string, raw ArticleHookPayload) ArticleHookPayload {
	out, _ := m.DispatchArticleHookWithError(ctx, hookName, raw)
	return out
}

// DispatchArticleHookWithError dispatches an article hook in priority order, enforcing 10th Privacy Surface
// redaction and supporting chained short-circuit interception (`*HookRejectedError`) on `before_*` hooks
// such as `article.before_save`.
func (m *Manager) DispatchArticleHookWithError(ctx context.Context, hookName string, raw ArticleHookPayload) (ArticleHookPayload, error) {
	active := m.snapshotEnabledPlugins()
	cur := raw
	interceptable := IsInterceptableHook(hookName)

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
			var rej *HookRejectedError
			if interceptable && errors.As(err, &rej) {
				if rej.PluginID == "" {
					rej.PluginID = mp.manifest.ID
				}
				if rej.Hook == "" {
					rej.Hook = hookName
				}
				m.recordSuccess(ctx, mp)
				return cur, rej
			}
			m.recordFailure(ctx, mp, "hook:"+hookName, err)
			continue
		}
		if invoked {
			m.recordSuccess(ctx, mp)
			if interceptable {
				if rej, rejected := extractHookRejection(mp.manifest.ID, hookName, res); rejected {
					return cur, rej
				}
			}
		}
		hasLock := cur.HasContentLock || strings.Contains(cur.ContentMD, ":::lock") || strings.Contains(cur.SummaryMD, ":::lock")
		allowPlaintextMut := (!cur.IsPasswordProtected && !hasLock) || trustedProtected
		if (hookName == HookRenderPreMarkdown || hookName == HookArticleBeforeSave) && allowPlaintextMut {
			switch out := res.(type) {
			case map[string]any:
				if hookName == HookArticleBeforeSave {
					if nextTitle, sOk := out["title"].(string); sOk && strings.TrimSpace(nextTitle) != "" {
						cur.Title = nextTitle
					}
				}
				if nextMD, sOk := out["content_md"].(string); sOk && nextMD != "" {
					cur.ContentMD = nextMD
				}
				if nextSummary, sOk := out["summary_md"].(string); sOk && nextSummary != "" {
					cur.SummaryMD = nextSummary
				}
			case ArticleHookPayload:
				if hookName == HookArticleBeforeSave && strings.TrimSpace(out.Title) != "" {
					cur.Title = out.Title
				}
				if out.ContentMD != "" {
					cur.ContentMD = out.ContentMD
				}
				if out.SummaryMD != "" {
					cur.SummaryMD = out.SummaryMD
				}
			case *ArticleHookPayload:
				if out != nil {
					if hookName == HookArticleBeforeSave && strings.TrimSpace(out.Title) != "" {
						cur.Title = out.Title
					}
					if out.ContentMD != "" {
						cur.ContentMD = out.ContentMD
					}
					if out.SummaryMD != "" {
						cur.SummaryMD = out.SummaryMD
					}
				}
			case string:
				if out != "" && hookName == HookRenderPreMarkdown {
					cur.ContentMD = out
				}
			}
		}
		if hookName == HookRenderPostHTML && !cur.IsPasswordProtected && !hasLock {
			switch out := res.(type) {
			case string:
				if out != "" {
					cur.HTML = out
				}
			case map[string]any:
				if nextHTML, sOk := out["html"].(string); sOk && nextHTML != "" {
					cur.HTML = nextHTML
				}
			case ArticleHookPayload:
				if out.HTML != "" {
					cur.HTML = out.HTML
				}
			case *ArticleHookPayload:
				if out != nil && out.HTML != "" {
					cur.HTML = out.HTML
				}
			}
		}
	}
	return cur, nil
}

// DispatchHook dispatches a generic domain hook in plugin priority order.
// Pre-action hooks (`IsInterceptableHook(hookName) == true`, e.g. `comment.before_create`,
// `comment.before_update`, `user.before_register`, `user.before_update_profile`, `article.before_save`,
// `page.before_save`, `media.before_upload`) uniformly support chained short-circuit interception
// (`volo.reject`, `{allow:false}`, `{reject:true}`, `{action:"reject"}`, `return false`) and
// non-destructive partial map payload chaining across multiple plugins.
func (m *Manager) DispatchHook(ctx context.Context, hookName string, payload any) (any, error) {
	switch v := payload.(type) {
	case ArticleHookPayload:
		out, err := m.DispatchArticleHookWithError(ctx, hookName, v)
		if IsInterceptableHook(hookName) {
			return out, err
		}
		return sanitizeArticleHookPayload(out), err
	case *ArticleHookPayload:
		if v == nil {
			return (*ArticleHookPayload)(nil), nil
		}
		out, err := m.DispatchArticleHookWithError(ctx, hookName, *v)
		if IsInterceptableHook(hookName) {
			return &out, err
		}
		sanitized := sanitizeArticleHookPayload(out)
		return &sanitized, err
	}

	interceptable := IsInterceptableHook(hookName)
	active := m.snapshotEnabledPlugins()
	cur := payload
	for _, mp := range active {
		if !mp.manifest.CanUseHook(hookName) {
			continue
		}
		callPayload := cloneHookPayloadInput(cur)
		res, invoked, err := m.invokeHook(ctx, mp, hookName, callPayload)
		if err != nil {
			var rej *HookRejectedError
			if interceptable && errors.As(err, &rej) {
				if rej.PluginID == "" {
					rej.PluginID = mp.manifest.ID
				}
				if rej.Hook == "" {
					rej.Hook = hookName
				}
				m.recordSuccess(ctx, mp)
				return cur, rej
			}
			m.recordFailure(ctx, mp, "hook:"+hookName, err)
			continue
		}
		if invoked {
			m.recordSuccess(ctx, mp)
			if interceptable {
				if rej, rejected := extractHookRejection(mp.manifest.ID, hookName, res); rejected {
					return cur, rej
				}
			}
			cur = mergeHookPayloadResult(cur, res)
		}
	}
	return cur, nil
}

func cloneHookPayloadInput(in any) any {
	m, ok := in.(map[string]any)
	if !ok || m == nil {
		return in
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func extractHookRejection(pluginID string, hookName string, res any) (*HookRejectedError, bool) {
	if res == nil {
		return nil, false
	}
	if allowVal, isBool := res.(bool); isBool {
		if !allowVal {
			return &HookRejectedError{
				PluginID: pluginID,
				Hook:     hookName,
				Reason:   defaultHookRejectReason(hookName),
			}, true
		}
		return nil, false
	}
	resMap, isMap := res.(map[string]any)
	if !isMap {
		return nil, false
	}
	reason, _ := resMap["reason"].(string)
	reason = strings.TrimSpace(reason)
	code, _ := resMap["code"].(string)
	code = strings.TrimSpace(code)

	if v, ok := resMap["__volo_rejected__"].(bool); ok && v {
		return &HookRejectedError{PluginID: pluginID, Hook: hookName, Reason: reason, Code: code}, true
	}
	if allowVal, hasAllow := resMap["allow"].(bool); hasAllow && !allowVal {
		return &HookRejectedError{PluginID: pluginID, Hook: hookName, Reason: reason, Code: code}, true
	}
	if rejVal, hasRej := resMap["reject"].(bool); hasRej && rejVal {
		return &HookRejectedError{PluginID: pluginID, Hook: hookName, Reason: reason, Code: code}, true
	}
	if actStr, hasAct := resMap["action"].(string); hasAct && strings.EqualFold(strings.TrimSpace(actStr), "reject") {
		return &HookRejectedError{PluginID: pluginID, Hook: hookName, Reason: reason, Code: code}, true
	}
	return nil, false
}

func defaultHookRejectReason(hookName string) string {
	if strings.HasPrefix(hookName, "comment.") {
		return "comment rejected by plugin"
	}
	return "action rejected by plugin"
}

func mergeHookPayloadResult(cur any, res any) any {
	if res == nil {
		return cur
	}
	if b, isBool := res.(bool); isBool && b {
		return cur
	}
	curMap, curIsMap := cur.(map[string]any)
	resMap, resIsMap := res.(map[string]any)
	if !curIsMap || !resIsMap {
		return res
	}
	next := make(map[string]any, len(curMap)+len(resMap))
	pendingRatcheted := false
	for k, v := range curMap {
		next[k] = v
		if k == "status" {
			if s, ok := v.(string); ok && strings.EqualFold(strings.TrimSpace(s), "pending") {
				pendingRatcheted = true
			}
		}
	}
	if actStr, ok := resMap["action"].(string); ok && strings.EqualFold(strings.TrimSpace(actStr), "moderate") {
		pendingRatcheted = true
	}
	if modVal, ok := resMap["moderate"].(bool); ok && modVal {
		pendingRatcheted = true
	}
	for k, v := range resMap {
		switch k {
		case "allow", "reject", "action", "moderate", "reason", "code", "__volo_rejected__":
			continue
		case "status":
			if pendingRatcheted {
				next["status"] = "pending"
				continue
			}
		}
		next[k] = v
	}
	if pendingRatcheted {
		next["status"] = "pending"
	}
	return next
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

func annotateHeadPersistAttrs(raw string, idEsc string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	headTags := []string{"<link", "<style", "<script", "<meta"}
	var out strings.Builder
	out.Grow(len(raw) + 64)
	i := 0
	for i < len(raw) {
		matchedTag := ""
		if raw[i] == '<' {
			remLower := strings.ToLower(raw[i:])
			for _, ht := range headTags {
				if strings.HasPrefix(remLower, ht) && len(remLower) > len(ht) {
					nextCh := remLower[len(ht)]
					if nextCh == ' ' || nextCh == '\t' || nextCh == '\n' || nextCh == '\r' || nextCh == '>' || nextCh == '/' {
						matchedTag = raw[i : i+len(ht)]
						break
					}
				}
			}
		}
		if matchedTag == "" {
			out.WriteByte(raw[i])
			i++
			continue
		}
		// Scan to closing '>' of opening tag (respecting quotes).
		j := i + len(matchedTag)
		inSingle := false
		inDouble := false
		for j < len(raw) {
			ch := raw[j]
			if ch == '\'' && !inDouble {
				inSingle = !inSingle
			} else if ch == '"' && !inSingle {
				inDouble = !inDouble
			} else if ch == '>' && !inSingle && !inDouble {
				break
			}
			j++
		}
		if j >= len(raw) {
			out.WriteString(raw[i:])
			break
		}
		tagBody := raw[i:j]
		tagLower := strings.ToLower(tagBody)
		selfClose := strings.HasSuffix(strings.TrimSpace(tagBody), "/")
		if selfClose {
			slashIdx := strings.LastIndexByte(tagBody, '/')
			out.WriteString(tagBody[:slashIdx])
			if !strings.Contains(tagLower, "data-volo-persist") {
				fmt.Fprintf(&out, ` data-volo-persist="%s"`, idEsc)
			}
			if !strings.Contains(tagLower, "data-volo-plugin") {
				fmt.Fprintf(&out, ` data-volo-plugin="%s"`, idEsc)
			}
			out.WriteString(tagBody[slashIdx:])
		} else {
			out.WriteString(tagBody)
			if !strings.Contains(tagLower, "data-volo-persist") {
				fmt.Fprintf(&out, ` data-volo-persist="%s"`, idEsc)
			}
			if !strings.Contains(tagLower, "data-volo-plugin") {
				fmt.Fprintf(&out, ` data-volo-plugin="%s"`, idEsc)
			}
		}
		out.WriteByte('>')
		i = j + 1
	}
	return out.String()
}

// RenderSlots collects rendered HTML/asset fragments for slotName across all enabled plugins.
// Persistent slots (Persist=true) are automatically annotated or wrapped with `data-volo-persist="<plugin_id>"`
// for zero-coupled PJAX survival in volo-studio.
func (m *Manager) RenderSlots(ctx context.Context, slotName string, slotCtx SlotRenderContext) []RenderedSlot {
	active := m.snapshotEnabledPlugins()
	var out []RenderedSlot
	isAdminCard := slotName == SlotAdminDashboardCard
	isHeadSlot := slotName == SlotPublicHead

	for _, mp := range active {
		for _, spec := range mp.manifest.UISlots {
			if spec.Slot != slotName {
				continue
			}
			var buf strings.Builder
			verEsc := html.EscapeString(mp.manifest.Version)
			idEsc := html.EscapeString(mp.manifest.ID)
			persistHeadAttr := ""
			if spec.Persist && isHeadSlot {
				persistHeadAttr = fmt.Sprintf(` data-volo-persist="%s"`, idEsc)
			}

			if spec.StyleAsset != "" && !isAdminCard {
				fmt.Fprintf(&buf, `<link rel="stylesheet" href="/plugins/%s/assets/%s?v=%s"%s data-volo-plugin="%s">`,
					idEsc, html.EscapeString(spec.StyleAsset), verEsc, persistHeadAttr, idEsc)
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
				rawBody := bodyBuf.String()
				if spec.Persist && isHeadSlot && rawBody != "" {
					rawBody = annotateHeadPersistAttrs(rawBody, idEsc)
				}
				buf.WriteString(rawBody)
				if spec.ScriptAsset != "" {
					fmt.Fprintf(&buf, `<script defer src="/plugins/%s/assets/%s?v=%s"%s data-volo-plugin="%s"></script>`,
						idEsc, html.EscapeString(spec.ScriptAsset), verEsc, persistHeadAttr, idEsc)
				}
			}

			fragment := buf.String()
			if strings.TrimSpace(fragment) == "" {
				continue
			}
			if spec.Persist && !isAdminCard && !isHeadSlot {
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
	if isSandboxedMarkupAssetExt(ext) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	}
	ct := mime.TypeByExtension(ext)
	if ct == "" || isNonStandardExecutableOrMarkupAssetExt(ext) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	} else {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

func isSandboxedMarkupAssetExt(ext string) bool {
	switch ext {
	case ".xhtml", ".shtml", ".svg", ".html", ".htm", ".xml":
		return true
	default:
		return false
	}
}

func isNonStandardExecutableOrMarkupAssetExt(ext string) bool {
	switch ext {
	case "", ".exe", ".dll", ".msi", ".com", ".bat", ".cmd", ".ps1", ".vbs", ".wsf",
		".sh", ".bash", ".zsh", ".py", ".rb", ".pl", ".php", ".phtml", ".jsp", ".asp", ".aspx", ".cgi",
		".jar", ".war", ".swf", ".hta", ".htc", ".mht", ".mhtml", ".svgz", ".xsl", ".xslt":
		return true
	default:
		return false
	}
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

// WaitFederation waits for any in-flight asynchronous DispatchFederationArticle deliveries to finish.
func (m *Manager) WaitFederation() {
	if m == nil {
		return
	}
	m.fedWG.Wait()
}

// Close gracefully stops all plugin background cron jobs, waits for in-flight federation tasks, and detaches runtimes.
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

	m.fedWG.Wait()
	for _, mp := range list {
		m.deactivateRuntime(ctx, mp)
	}
	if m.cron != nil {
		m.cron.Close()
	}
	if m.store != nil {
		flushCtx := ctx
		var cancel context.CancelFunc
		if flushCtx == nil || flushCtx.Err() != nil {
			flushCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		}
		if fErr := CloseKVBuffer(flushCtx, m.store); fErr != nil {
			slog.Warn("plugin: failed to flush buffered kv on manager close", "err", fErr)
		}
		if cancel != nil {
			cancel()
		}
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
	sortManagedPluginsByPriority(out)
	return out
}

var _ OIDCAdvancedProvider = (*gojaOIDCAdapter)(nil)

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
	return a.AuthCodeURLWithParams(OIDCAuthParams{
		State:       state,
		RedirectURI: redirectURI,
	})
}

func (a *gojaOIDCAdapter) AuthCodeURLWithParams(params OIDCAuthParams) (string, error) {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return "", errors.New("plugin oidc provider inactive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.manager.routeTimeout)
	defer cancel()

	opts := make(map[string]any, 3)
	if params.Nonce != "" {
		opts["nonce"] = params.Nonce
	}
	if params.CodeChallenge != "" {
		opts["code_challenge"] = params.CodeChallenge
	}
	if params.CodeChallengeMethod != "" {
		opts["code_challenge_method"] = params.CodeChallengeMethod
	}

	res, found, err := rt.invoke(ctx, a.manager.routeTimeout, "oidcAuthURL", params.State, params.RedirectURI, opts)
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

	q := uParsed.Query()
	mutated := false
	if params.Nonce != "" && !q.Has("nonce") {
		q.Set("nonce", params.Nonce)
		mutated = true
	}
	if params.CodeChallenge != "" {
		if !q.Has("code_challenge") {
			q.Set("code_challenge", params.CodeChallenge)
			mutated = true
		}
		if params.CodeChallengeMethod != "" && !q.Has("code_challenge_method") {
			q.Set("code_challenge_method", params.CodeChallengeMethod)
			mutated = true
		}
	}
	if mutated {
		uParsed.RawQuery = q.Encode()
		return uParsed.String(), nil
	}
	return rawURL, nil
}

func (a *gojaOIDCAdapter) Exchange(ctx context.Context, code string, redirectURI string) (OIDCIdentity, error) {
	return a.ExchangeWithParams(ctx, OIDCExchangeParams{
		Code:        code,
		RedirectURI: redirectURI,
	})
}

func (a *gojaOIDCAdapter) ExchangeWithParams(ctx context.Context, params OIDCExchangeParams) (OIDCIdentity, error) {
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

	opts := make(map[string]any, 2)
	if params.Nonce != "" {
		opts["nonce"] = params.Nonce
	}
	if params.CodeVerifier != "" {
		opts["code_verifier"] = params.CodeVerifier
	}

	res, found, err := rt.invoke(ctx, timeout, "oidcExchange", params.Code, params.RedirectURI, opts)
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

	emailVerified := false
	if rawVer, hasVer := m["email_verified"]; hasVer && rawVer != nil {
		emailVerified = parseOIDCBool(rawVer)
	} else if rawVer, hasVer := m["verified"]; hasVer && rawVer != nil {
		emailVerified = parseOIDCBool(rawVer)
	}

	nonce := ""
	if nStr, sOk := m["nonce"].(string); sOk {
		nonce = strings.TrimSpace(nStr)
	}

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
		Provider:      prov,
		Subject:       sub,
		Email:         email,
		EmailVerified: emailVerified,
		Nonce:         nonce,
		DisplayName:   disp,
		AvatarURL:     avatar,
	}, nil
}

func parseOIDCBool(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return strings.EqualFold(strings.TrimSpace(val), "true")
	case int:
		return val == 1
	case int64:
		return val == 1
	case float64:
		return val == 1
	case json.Number:
		return val.String() == "1"
	default:
		return false
	}
}

var (
	_ NicknameAntiSpamProvider = (*gojaAntiSpamAdapter)(nil)
	_ ContentSanitizerProvider = (*gojaAntiSpamAdapter)(nil)
)

type gojaAntiSpamAdapter struct {
	manager *Manager
	mp      *managedPlugin
}

func (a *gojaAntiSpamAdapter) Name() string { return "plugin:" + a.mp.manifest.ID }

func (a *gojaAntiSpamAdapter) activeRuntime() *gojaPluginRuntime {
	if a == nil || a.mp == nil {
		return nil
	}
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()
	if !enabled {
		return nil
	}
	return rt
}

func (a *gojaAntiSpamAdapter) SanitizerStateToken() string {
	rt := a.activeRuntime()
	if rt == nil {
		return ""
	}
	return strconv.FormatUint(a.mp.cfgRev.Load(), 36) + ":" + strconv.FormatInt(rt.wordRev.Load(), 36)
}

func (a *gojaAntiSpamAdapter) MaskHTML(ctx context.Context, htmlStr string) string {
	rt := a.activeRuntime()
	if rt == nil {
		return htmlStr
	}
	return rt.maskSanitizerHTML(ctx, htmlStr)
}

func (a *gojaAntiSpamAdapter) MaskText(ctx context.Context, text string) string {
	rt := a.activeRuntime()
	if rt == nil {
		return text
	}
	return rt.maskSanitizerText(ctx, text)
}

func (a *gojaAntiSpamAdapter) MaskNickname(ctx context.Context, nickname string, isStaffOrAuthor bool) string {
	rt := a.activeRuntime()
	if rt == nil {
		return nickname
	}
	return rt.maskSanitizerNickname(ctx, nickname, isStaffOrAuthor)
}

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
	return parseSpamVerdictResult(res), nil
}

func (a *gojaAntiSpamAdapter) CheckNickname(ctx context.Context, input NicknameCheckInput) (SpamVerdict, error) {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return SpamVerdict{}, errors.New("plugin antispam inactive")
	}
	res, found, err := rt.invoke(ctx, a.manager.hookTimeout, "checkNickname", input)
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:antispam", err)
		return SpamVerdict{}, err
	}
	if found {
		a.manager.recordSuccess(ctx, a.mp)
		return parseSpamVerdictResult(res), nil
	}
	// Seamless fallback for AntiSpamProvider plugins that only export checkSpam(input).
	return a.CheckComment(ctx, CommentCheckInput{
		TargetType: "nickname",
		TargetID:   input.Source,
		AuthorName: input.Nickname,
		ContentMD:  input.Nickname,
		IPMasked:   input.IPMasked,
		GeoRegion:  input.GeoRegion,
		Locale:     input.Locale,
		IsGuest:    true,
	})
}

func parseSpamVerdictResult(res any) SpamVerdict {
	if m, ok := res.(map[string]any); ok {
		verdict := SpamVerdict{Action: SpamActionPass}
		if act, sOk := m["action"].(string); sOk && strings.TrimSpace(act) != "" {
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
		return verdict
	}
	return SpamVerdict{Action: SpamActionPass}
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
		_, found, err = rt.invoke(ctx, timeout, "notifySend", event)
	}
	if !found && err == nil {
		_, found, err = rt.invoke(ctx, timeout, "onNotify", event)
	}
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "provider:notifier", err)
		return err
	}
	if !found {
		return errors.New("sendNotification or notifySend function not exported")
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

type gojaFederationAdapter struct {
	manager *Manager
	mp      *managedPlugin
}

func (a *gojaFederationAdapter) Name() string { return a.mp.manifest.ID }

func (a *gojaFederationAdapter) Publish(ctx context.Context, action string, articlePayload map[string]any) error {
	a.mp.mu.Lock()
	rt := a.mp.gojaRT
	enabled := a.mp.enabled && a.mp.circuitState == CircuitClosed
	a.mp.mu.Unlock()

	if !enabled || rt == nil {
		return errors.New("plugin federation inactive")
	}

	normAction := strings.ToLower(strings.TrimSpace(action))
	if normAction == "" {
		normAction = "publish"
	}
	safePayload := sanitizeFederationPayloadMap(articlePayload)

	_, found, err := rt.invokeFederation(ctx, a.manager.routeTimeout, normAction, safePayload)
	if err != nil {
		a.manager.recordFailure(ctx, a.mp, "federation:"+normAction, err)
		return err
	}
	if !found {
		return nil
	}
	a.manager.recordSuccess(ctx, a.mp)
	a.manager.logger.Write(a.mp.manifest.ID, "info", "federation:"+normAction, "federation "+normAction+" completed")
	return nil
}

func sanitizeFederationPayloadMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := cloneArbitraryMap(in)
	isProtected, _ := out["is_password_protected"].(bool)
	if !isProtected {
		if v, ok := out["IsPasswordProtected"].(bool); ok && v {
			isProtected = true
		}
	}
	if isProtected {
		out["summary_md"] = ""
		out["content_md"] = ""
		out["html"] = ""
		out["cover_url"] = ""
		out["audio_url"] = ""
		out["omitted_protected"] = true
		delete(out, "SummaryMD")
		delete(out, "ContentMD")
		delete(out, "HTML")
		delete(out, "CoverURL")
		delete(out, "AudioURL")
		return out
	}

	summaryMD, _ := out["summary_md"].(string)
	contentMD, _ := out["content_md"].(string)
	htmlBody, _ := out["html"].(string)
	hasLock, _ := out["has_content_lock"].(bool)
	if hasLock || strings.Contains(summaryMD, ":::lock") || strings.Contains(contentMD, ":::lock") || strings.Contains(htmlBody, ":::lock") {
		out["has_content_lock"] = true
		if summaryMD != "" {
			out["summary_md"] = stripAllLockBlocks(summaryMD)
		}
		if contentMD != "" {
			out["content_md"] = stripAllLockBlocks(contentMD)
		}
		if _, exists := out["html"]; exists {
			out["html"] = ""
		}
	}
	return out
}

func cloneArbitraryMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch tv := v.(type) {
		case map[string]any:
			out[k] = cloneArbitraryMap(tv)
		case []any:
			cp := make([]any, len(tv))
			copy(cp, tv)
			out[k] = cp
		case []string:
			cp := make([]string, len(tv))
			copy(cp, tv)
			out[k] = cp
		default:
			out[k] = v
		}
	}
	return out
}

// RedactArticleForPlugin enforces the 10th Privacy Surface:
// 1. Password-protected articles omit SummaryMD, ContentMD, HTML, CoverURL, and AudioURL completely.
// 2. Partial `:::lock` blocks (including unclosed `:::lock` to EOF outside code fences) are physically stripped.
func RedactArticleForPlugin(in ArticleHookPayload, trustedProtected bool) ArticleHookPayload {
	if trustedProtected {
		return in
	}
	return sanitizeArticleHookPayload(in)
}

func sanitizeArticleHookPayload(in ArticleHookPayload) ArticleHookPayload {
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
		out.HasContentLock = true
		out.SummaryMD = stripAllLockBlocks(in.SummaryMD)
		out.ContentMD = stripAllLockBlocks(in.ContentMD)
		out.HTML = ""
	}
	return out
}

// StripLockedMarkdownBlocks removes all `:::lock ... :::` blocks outside fenced code blocks (` ``` ` / `~~~`).
// If an opening `:::lock` has no matching closing `:::`, it FAILS CLOSED to EOF so locked content never leaks!
func StripLockedMarkdownBlocks(src string) string {
	return stripAllLockBlocks(src)
}

func stripAllLockBlocks(src string) string {
	if !strings.Contains(src, ":::lock") {
		return src
	}
	normalized := strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	out := make([]string, 0, len(lines))

	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceQuoteDepth := 0
	lockNestDepth := 0

	for _, line := range lines {
		rest, quoteDepth, ok := stripBlockquoteAndIndent(line)
		if inFence && quoteDepth < fenceQuoteDepth {
			inFence = false
			fenceChar = 0
			fenceLen = 0
			fenceQuoteDepth = 0
		}

		if ok {
			if ch, n, fenceOK := parseCodeFenceMarker(rest); fenceOK {
				if !inFence {
					if ch != '`' || !strings.ContainsRune(rest[n:], '`') {
						inFence = true
						fenceChar = ch
						fenceLen = n
						fenceQuoteDepth = quoteDepth
						if lockNestDepth == 0 {
							out = append(out, line)
						}
						continue
					}
				} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
					inFence = false
					fenceChar = 0
					fenceLen = 0
					fenceQuoteDepth = 0
					if lockNestDepth == 0 {
						out = append(out, line)
					}
					continue
				}
			}

			if !inFence {
				if lockNestDepth == 0 {
					if isLockOpenDirective(rest) {
						lockNestDepth = 1
						continue
					}
				} else {
					if rest == ":::" {
						lockNestDepth--
						continue
					}
					if isInnerDirectiveOpen(rest) {
						lockNestDepth++
						continue
					}
				}
			}
		}

		if lockNestDepth == 0 {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// stripBlockquoteAndIndent strips zero or more CommonMark `>` blockquote prefixes
// (each with `<= 3` leading spaces and an optional space after `>`) and then checks
// that the remaining block indentation is at most 3 spaces (`<= 3`, no leading tab).
func stripBlockquoteAndIndent(line string) (rest string, quoteDepth int, ok bool) {
	s := line
	for {
		spaces := 0
		for spaces < len(s) && s[spaces] == ' ' {
			spaces++
		}
		if spaces <= 3 && spaces < len(s) && s[spaces] == '>' {
			quoteDepth++
			s = s[spaces+1:]
			if len(s) > 0 && s[0] == ' ' {
				s = s[1:]
			}
			continue
		}
		break
	}
	indent := 0
	for indent < len(s) && s[indent] == ' ' {
		indent++
	}
	if indent > 3 || (indent < len(s) && s[indent] == '\t') {
		return "", quoteDepth, false
	}
	return strings.TrimRight(s[indent:], " \t\r"), quoteDepth, true
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
	if rest == "" || rest == ":::" {
		return true
	}
	return rest[0] == ' ' || rest[0] == '\t' || rest[0] == '[' || rest[0] == '{'
}

func isInnerDirectiveOpen(trimmed string) bool {
	if !strings.HasPrefix(trimmed, ":::") || len(trimmed) <= 3 {
		return false
	}
	c := trimmed[3]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
		return false
	}
	end := 3
	for end < len(trimmed) {
		ch := trimmed[end]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			end++
		} else {
			break
		}
	}
	name := trimmed[3:end]
	tail := trimmed[end:]
	if tail != "" && tail != ":::" && tail[0] != ' ' && tail[0] != '\t' && tail[0] != '[' && tail[0] != '{' {
		return false
	}
	if name != "lock" && strings.HasSuffix(tail, ":::") {
		return false
	}
	return true
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
