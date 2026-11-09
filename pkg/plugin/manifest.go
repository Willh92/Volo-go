package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Supported runtimes and circuit states.
const (
	RuntimeBuiltin = "builtin"
	RuntimeGoja    = "goja"

	CircuitClosed = "closed"
	CircuitOpen   = "open"
)

// Standard domain hooks.
const (
	// Pre-action chained interception hooks (before_*): support short-circuit rejection and non-destructive payload chaining.
	HookArticleBeforeSave       = "article.before_save"
	HookPageBeforeSave          = "page.before_save"
	HookCommentBeforeCreate     = "comment.before_create"
	HookCommentBeforeUpdate     = "comment.before_update"
	HookUserBeforeRegister      = "user.before_register"
	HookUserBeforeUpdateProfile = "user.before_update_profile"
	HookMediaBeforeUpload       = "media.before_upload"

	// Post-action lifecycle notification hooks: broadcast after host commit.
	HookArticlePublished = "article.published"
	HookArticleUpdated   = "article.updated"
	HookArticleDeleted   = "article.deleted"
	HookPagePublished    = "page.published"
	HookPageUpdated      = "page.updated"
	HookPageDeleted      = "page.deleted"
	HookCommentCreated   = "comment.created"
	HookCommentModerated = "comment.moderated"

	// Render pipeline transformation hooks.
	HookRenderPreMarkdown = "render.pre_markdown"
	HookRenderPostHTML    = "render.post_html"
	HookRenderDirective   = "render.directive"

	// Plugin execution priority bounds (smaller priority value runs earlier in chains; ties break by plugin ID).
	DefaultPluginPriority = 100
	MinPluginPriority     = -10000
	MaxPluginPriority     = 10000
)

// ClampPluginPriority bounds p into [MinPluginPriority, MaxPluginPriority].
func ClampPluginPriority(p int) int {
	if p < MinPluginPriority {
		return MinPluginPriority
	}
	if p > MaxPluginPriority {
		return MaxPluginPriority
	}
	return p
}

// IsInterceptableHook reports whether hookName is a pre-action gatekeeping hook (`*.before_*` or `before.*`)
// eligible for unified chained short-circuit interception (`volo.reject`, `{allow:false}`, `{reject:true}`, `return false`).
func IsInterceptableHook(hookName string) bool {
	h := strings.ToLower(strings.TrimSpace(hookName))
	if h == "" {
		return false
	}
	return strings.Contains(h, ".before_") || strings.HasPrefix(h, "before.") || strings.HasPrefix(h, "before_")
}

// Standard UI slots.
const (
	SlotPublicHead           = "public.head"
	SlotPublicHeader         = "public.header"
	SlotPublicSidebar        = "public.sidebar"
	SlotArticleBeforeContent = "article.before_content"
	SlotArticleAfterContent  = "article.after_content"
	SlotCommentBefore        = "comment.before"
	SlotCommentAfter         = "comment.after"
	SlotPublicFooter         = "public.footer"
	SlotEditorQuickInsert    = "editor.quick_insert"
	SlotAdminDashboardCard   = "admin.dashboard_card"

	ClearSecretSentinel = "__VOLO_CLEAR_SECRET__"
	SecretMaskSentinel  = "********"
)

var validPluginIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)
var validSemverLooseRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([0-9A-Za-z.-]*)$`)
var validHookNameRe = regexp.MustCompile(`^[a-z0-9._-]{1,128}$`)
var validDNSLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var allowedSlotNames = map[string]bool{
	SlotPublicHead:           true,
	SlotPublicHeader:         true,
	SlotPublicSidebar:        true,
	SlotArticleBeforeContent: true,
	SlotArticleAfterContent:  true,
	SlotCommentBefore:        true,
	SlotCommentAfter:         true,
	SlotPublicFooter:         true,
	SlotEditorQuickInsert:    true,
	SlotAdminDashboardCard:   true,
}

// Manifest defines the declarative metadata, permission sandbox, UI slots, cron schedules,
// and localized configuration JSON Schema of a Volo-go plugin (manifest.json).
type Manifest struct {
	ID             string            `json:"id"`
	Version        string            `json:"version"`
	MinHostVersion string            `json:"min_host_version,omitempty"`
	Runtime        string            `json:"runtime"`
	Entry          string            `json:"entry,omitempty"`
	Priority       *int              `json:"priority,omitempty"`
	Author         string            `json:"author,omitempty"`
	Homepage       string            `json:"homepage,omitempty"`
	Name           map[string]string `json:"name"`
	Description    map[string]string `json:"description,omitempty"`
	Permissions    []string          `json:"permissions,omitempty"`
	AllowedHosts   []string          `json:"allowed_hosts,omitempty"`
	Hooks          []string          `json:"hooks,omitempty"`
	UISlots        []SlotSpec        `json:"ui_slots,omitempty"`
	Cron           []CronSpec        `json:"cron,omitempty"`
	ConfigSchema   []ConfigField     `json:"config_schema,omitempty"`
}

// DefaultPriority returns the manifest's declared priority clamped to bounds, or DefaultPluginPriority (100).
func (m *Manifest) DefaultPriority() int {
	if m == nil || m.Priority == nil {
		return DefaultPluginPriority
	}
	return ClampPluginPriority(*m.Priority)
}

// SlotSpec declares a UI injection slot registration.
type SlotSpec struct {
	Slot        string             `json:"slot"`
	Persist     bool               `json:"persist,omitempty"` // Survives PJAX navigation outside #pjax via data-volo-persist
	HTML        string             `json:"html,omitempty"`
	ScriptAsset string             `json:"script_asset,omitempty"`
	StyleAsset  string             `json:"style_asset,omitempty"`
	Dynamic     bool               `json:"dynamic,omitempty"` // Calls renderSlot(slot, ctx) in runtime
	QuickInsert *QuickInsertAction `json:"quick_insert,omitempty"`
}

// QuickInsertAction describes an extensible toolbar snippet in the CodeMirror 6 editor drawer.
type QuickInsertAction struct {
	ID       string            `json:"id"`
	Label    map[string]string `json:"label"`
	Icon     string            `json:"icon,omitempty"`
	Template string            `json:"template"`
}

// CronSpec declares a managed periodic background callback.
type CronSpec struct {
	Name            string `json:"name"`
	IntervalSeconds int    `json:"interval_seconds"`
	Handler         string `json:"handler"`
}

// ConfigField describes a single field in a plugin's declarative configuration form.
type ConfigField struct {
	Key         string            `json:"key"`
	Type        string            `json:"type"` // string | text | textarea | secret | number | boolean | select | url | color | image | sortable_list | tag_list
	Secret      bool              `json:"secret,omitempty"`
	Required    bool              `json:"required,omitempty"`
	Default     any               `json:"default,omitempty"`
	Min         *float64          `json:"min,omitempty"`
	Max         *float64          `json:"max,omitempty"`
	Step        *float64          `json:"step,omitempty"`
	Options     []SelectOption    `json:"options,omitempty"`
	Group       map[string]string `json:"group,omitempty"`
	Label       map[string]string `json:"label"`
	Placeholder map[string]string `json:"placeholder,omitempty"`
	Description map[string]string `json:"description,omitempty"`
	Help        map[string]string `json:"help,omitempty"`
}

// IsSecret reports whether this config field stores sensitive secret material.
func (f ConfigField) IsSecret() bool {
	return f.Secret || strings.EqualFold(strings.TrimSpace(f.Type), "secret")
}

// SelectOption represents an option for a "select" ConfigField.
type SelectOption struct {
	Value string            `json:"value"`
	Label map[string]string `json:"label"`
}

// ParseManifest decodes and strictly validates a manifest.json payload.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("plugin: invalid manifest.json: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate enforces strict structural and security invariants on Manifest.
func (m *Manifest) Validate() error {
	m.ID = strings.TrimSpace(m.ID)
	if !validPluginIDRe.MatchString(m.ID) {
		return fmt.Errorf("plugin: invalid plugin id %q (must match %s)", m.ID, validPluginIDRe.String())
	}
	m.Version = strings.TrimSpace(m.Version)
	if !validSemverLooseRe.MatchString(m.Version) {
		return fmt.Errorf("plugin %q: invalid version %q", m.ID, m.Version)
	}
	m.Runtime = strings.ToLower(strings.TrimSpace(m.Runtime))
	if m.Runtime == "" {
		m.Runtime = RuntimeGoja
	}
	if m.Runtime != RuntimeBuiltin && m.Runtime != RuntimeGoja {
		return fmt.Errorf("plugin %q: unsupported runtime %q", m.ID, m.Runtime)
	}
	if m.Runtime == RuntimeGoja {
		if strings.TrimSpace(m.Entry) == "" {
			m.Entry = "main.js"
		}
		cleanEntry, err := sanitizeRelativeAssetPath(m.Entry)
		if err != nil || !strings.HasSuffix(strings.ToLower(cleanEntry), ".js") {
			return fmt.Errorf("plugin %q: invalid goja entry path %q", m.ID, m.Entry)
		}
		m.Entry = cleanEntry
	}
	if len(m.Name) == 0 || ResolveLocalized(m.Name, "en-US") == "" {
		return fmt.Errorf("plugin %q: localized name map must contain at least one non-empty label", m.ID)
	}
	if m.Homepage != "" {
		u, err := url.Parse(strings.TrimSpace(m.Homepage))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("plugin %q: homepage must use http or https", m.ID)
		}
	}
	if m.Priority != nil {
		clamped := ClampPluginPriority(*m.Priority)
		m.Priority = &clamped
	}

	// Validate permissions syntax.
	for _, perm := range m.Permissions {
		if err := validatePermissionToken(perm); err != nil {
			return fmt.Errorf("plugin %q: %w", m.ID, err)
		}
	}

	// Validate declared hooks syntax.
	for i, rawHook := range m.Hooks {
		h := strings.TrimSpace(rawHook)
		if h == "" || !validHookNameRe.MatchString(h) {
			return fmt.Errorf("plugin %q: invalid hook name %q (must match %s)", m.ID, rawHook, validHookNameRe.String())
		}
		m.Hooks[i] = h
	}

	// Validate declared outbound allowed_hosts patterns.
	for i, rawHost := range m.AllowedHosts {
		cleanHost, err := validateAllowedHostPattern(rawHost)
		if err != nil {
			return fmt.Errorf("plugin %q: invalid allowed_hosts entry %q: %w", m.ID, rawHost, err)
		}
		m.AllowedHosts[i] = cleanHost
	}

	// Validate UI slots and ensure matching slot permissions are declared.
	for i := range m.UISlots {
		slot := &m.UISlots[i]
		slot.Slot = strings.TrimSpace(slot.Slot)
		if !allowedSlotNames[slot.Slot] {
			return fmt.Errorf("plugin %q: unsupported ui_slot %q", m.ID, slot.Slot)
		}
		if !m.CanUseSlot(slot.Slot) {
			return fmt.Errorf("plugin %q: ui_slot %q requires permission \"slots:%s\" or \"ui:slots\"", m.ID, slot.Slot, slot.Slot)
		}
		if slot.Persist && slot.Slot != SlotPublicFooter && slot.Slot != SlotPublicHeader {
			return fmt.Errorf("plugin %q: persist=true is only supported on public.footer or public.header", m.ID)
		}
		if slot.ScriptAsset != "" {
			clean, err := sanitizeRelativeAssetPath(slot.ScriptAsset)
			if err != nil {
				return fmt.Errorf("plugin %q: invalid script_asset %q: %w", m.ID, slot.ScriptAsset, err)
			}
			slot.ScriptAsset = strings.TrimPrefix(clean, "assets/")
		}
		if slot.StyleAsset != "" {
			clean, err := sanitizeRelativeAssetPath(slot.StyleAsset)
			if err != nil {
				return fmt.Errorf("plugin %q: invalid style_asset %q: %w", m.ID, slot.StyleAsset, err)
			}
			slot.StyleAsset = strings.TrimPrefix(clean, "assets/")
		}
		if slot.Slot == SlotEditorQuickInsert && slot.QuickInsert == nil && !slot.Dynamic {
			return fmt.Errorf("plugin %q: editor.quick_insert slot requires quick_insert metadata or dynamic=true", m.ID)
		}
	}

	// Validate cron definitions.
	seenCron := make(map[string]bool, len(m.Cron))
	for _, c := range m.Cron {
		if !m.HasPermission("cron") {
			return fmt.Errorf("plugin %q: cron job %q requires permission \"cron\"", m.ID, c.Name)
		}
		name := strings.TrimSpace(c.Name)
		if name == "" || seenCron[name] {
			return fmt.Errorf("plugin %q: duplicate or empty cron name %q", m.ID, c.Name)
		}
		seenCron[name] = true
		if c.IntervalSeconds < 5 {
			return fmt.Errorf("plugin %q: cron %q interval_seconds must be >= 5", m.ID, c.Name)
		}
		if strings.TrimSpace(c.Handler) == "" {
			return fmt.Errorf("plugin %q: cron %q requires handler function name", m.ID, c.Name)
		}
	}

	// Validate config schema.
	seenKeys := make(map[string]bool, len(m.ConfigSchema))
	for i := range m.ConfigSchema {
		f := &m.ConfigSchema[i]
		k := strings.TrimSpace(f.Key)
		if k == "" || strings.HasPrefix(k, "__") || seenKeys[k] {
			return fmt.Errorf("plugin %q: invalid or duplicate config_schema key %q", m.ID, f.Key)
		}
		f.Key = k
		seenKeys[k] = true

		f.Type = strings.ToLower(strings.TrimSpace(f.Type))
		switch f.Type {
		case "string", "text", "textarea", "secret", "number", "boolean", "select", "url", "color", "image", "sortable_list", "tag_list", "token_list":
			if f.Type == "secret" {
				f.Secret = true
			} else if f.Type == "token_list" {
				f.Type = "tag_list"
			}
		default:
			return fmt.Errorf("plugin %q: config field %q has unsupported type %q", m.ID, k, f.Type)
		}
		if f.Min != nil && (math.IsNaN(*f.Min) || math.IsInf(*f.Min, 0)) {
			return fmt.Errorf("plugin %q: config field %q has invalid min bound", m.ID, k)
		}
		if f.Max != nil && (math.IsNaN(*f.Max) || math.IsInf(*f.Max, 0)) {
			return fmt.Errorf("plugin %q: config field %q has invalid max bound", m.ID, k)
		}
		if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			return fmt.Errorf("plugin %q: config field %q min (%v) exceeds max (%v)", m.ID, k, *f.Min, *f.Max)
		}
		if f.Step != nil && (math.IsNaN(*f.Step) || math.IsInf(*f.Step, 0) || *f.Step <= 0) {
			return fmt.Errorf("plugin %q: config field %q step must be > 0", m.ID, k)
		}
		if f.Type == "select" && len(f.Options) == 0 {
			return fmt.Errorf("plugin %q: select field %q must declare at least one option", m.ID, k)
		}
		if len(f.Label) == 0 || ResolveLocalized(f.Label, "en-US") == "" {
			return fmt.Errorf("plugin %q: config field %q requires a non-empty localized label", m.ID, k)
		}
	}
	return nil
}

func validateAllowedHostPattern(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	if h == "" || len(h) > 253 {
		return "", errors.New("hostname pattern must be 1..253 characters")
	}
	if strings.Contains(h, "://") {
		return "", errors.New("hostname pattern must not contain a URL scheme")
	}
	if strings.ContainsAny(h, "/:?#@\\ \t\r\n") {
		return "", errors.New("hostname pattern must not contain path, port, query, or userinfo")
	}
	checkHost := h
	if strings.HasPrefix(h, "*.") {
		checkHost = strings.TrimPrefix(h, "*.")
		if strings.Contains(checkHost, "*") {
			return "", errors.New("wildcard '*' is only allowed as leading '*.domain.tld'")
		}
	} else if strings.Contains(h, "*") {
		return "", errors.New("wildcard '*' is only allowed as leading '*.domain.tld'")
	}

	if ip := net.ParseIP(strings.Trim(checkHost, "[]")); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return "", errors.New("loopback, private, or unspecified IP literals are forbidden")
		}
		return "", errors.New("IP literals are forbidden in allowed_hosts; specify a valid DNS hostname")
	}
	if checkHost == "localhost" || strings.HasSuffix(checkHost, ".localhost") || strings.HasSuffix(checkHost, ".local") {
		return "", errors.New("loopback or local hostnames are forbidden")
	}

	labels := strings.Split(checkHost, ".")
	if len(labels) < 2 {
		return "", errors.New("hostname must be a qualified domain (e.g. api.example.com or *.example.com)")
	}
	for _, lbl := range labels {
		if !validDNSLabelRe.MatchString(lbl) {
			return "", fmt.Errorf("invalid DNS label %q", lbl)
		}
	}
	tld := labels[len(labels)-1]
	allDigits := true
	for i := 0; i < len(tld); i++ {
		if tld[i] < '0' || tld[i] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits || len(tld) < 2 {
		return "", fmt.Errorf("invalid top-level domain %q", tld)
	}
	return h, nil
}

func validatePermissionToken(perm string) error {
	p := strings.TrimSpace(perm)
	switch {
	case p == "kv" || p == "kv:rw" || p == "kv:read":
		return nil
	case p == "ui:slots" || (strings.HasPrefix(p, "slots:") && allowedSlotNames[strings.TrimPrefix(p, "slots:")]):
		return nil
	case p == "hooks:*" || p == "hooks:render.*":
		return nil
	case strings.HasPrefix(p, "hooks:"):
		target := strings.TrimPrefix(p, "hooks:")
		if strings.HasSuffix(target, ".*") {
			prefix := strings.TrimSuffix(target, ".*")
			if prefix != "" && !strings.Contains(prefix, ".") && validHookNameRe.MatchString(prefix) {
				return nil
			}
		}
		if !validHookNameRe.MatchString(target) {
			return fmt.Errorf("invalid hooks permission target %q", target)
		}
		return nil
	case p == "render" || p == "render:*" || p == "render:directive" || p == "render:directives":
		return nil
	case p == "federation" || p == "federation:*" || p == "provider:federation":
		return nil
	case p == "http:routes" || p == "http:outbound" || p == "net:http" || p == "http:raw_headers" || p == "raw_headers":
		return nil
	case strings.HasPrefix(p, "http.outbound:"):
		host := strings.TrimSpace(strings.TrimPrefix(p, "http.outbound:"))
		if _, err := validateAllowedHostPattern(host); err != nil {
			return fmt.Errorf("invalid outbound host pattern %q: %w", host, err)
		}
		return nil
	case p == "cron" || p == "articles:read_protected" || p == "crypto" || p == "site:read" || p == "ratelimit":
		return nil
	case p == "provider:storage" || p == "provider:notifier" || p == "provider:oidc" || p == "provider:antispam":
		return nil
	default:
		return fmt.Errorf("unknown permission %q", perm)
	}
}

func sanitizeRelativeAssetPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || strings.Contains(trimmed, "\x00") {
		return "", errors.New("path must be relative and non-empty")
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == ".." || seg == "" || seg == "." {
			return "", errors.New("path segments cannot contain '.', '..', or empty components")
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "..") {
		return "", errors.New("path traversal detected")
	}
	return cleaned, nil
}

// HasPermission reports whether exact perm is declared in m.Permissions.
func (m *Manifest) HasPermission(perm string) bool {
	for _, p := range m.Permissions {
		if strings.TrimSpace(p) == perm {
			return true
		}
	}
	return false
}

// CanReadKV reports whether the plugin may read from plugin_kv.
func (m *Manifest) CanReadKV() bool {
	return m.HasPermission("kv") || m.HasPermission("kv:rw") || m.HasPermission("kv:read")
}

// CanWriteKV reports whether the plugin may mutate plugin_kv.
func (m *Manifest) CanWriteKV() bool {
	return m.HasPermission("kv") || m.HasPermission("kv:rw")
}

// CanUseHook reports whether the plugin is permitted to handle hookName AND (when m.Hooks is non-empty)
// explicitly subscribed to hookName in its manifest.
func (m *Manifest) CanUseHook(hookName string) bool {
	if len(m.Hooks) > 0 {
		subscribed := false
		for _, h := range m.Hooks {
			if strings.TrimSpace(h) == hookName {
				subscribed = true
				break
			}
		}
		if !subscribed {
			return false
		}
	}
	if m.HasPermission("hooks:*") || m.HasPermission("hooks:"+hookName) {
		return true
	}
	if dot := strings.IndexByte(hookName, '.'); dot > 0 {
		domainPrefix := hookName[:dot]
		if m.HasPermission("hooks:" + domainPrefix + ".*") {
			return true
		}
	}
	if strings.HasPrefix(hookName, "render.") {
		if m.HasPermission("render") || m.HasPermission("render:*") || m.HasPermission("hooks:render.*") {
			return true
		}
		if hookName == HookRenderDirective && (m.HasPermission("render:directive") || m.HasPermission("render:directives")) {
			return true
		}
	}
	return false
}

// HasRenderCapability reports whether the plugin declares markdown/HTML/directive render capabilities.
func (m *Manifest) HasRenderCapability() bool {
	if m.HasPermission("render") || m.HasPermission("render:*") ||
		m.HasPermission("render:directive") || m.HasPermission("render:directives") ||
		m.HasPermission("hooks:render.*") ||
		m.CanUseHook(HookRenderPreMarkdown) || m.CanUseHook(HookRenderPostHTML) || m.CanUseHook(HookRenderDirective) {
		return true
	}
	for _, h := range m.Hooks {
		if strings.HasPrefix(strings.TrimSpace(h), "render.") {
			return true
		}
	}
	return false
}

// HasFederationCapability reports whether the plugin declares federation delivery/publishing capabilities.
func (m *Manifest) HasFederationCapability() bool {
	if m.HasPermission("federation") || m.HasPermission("federation:*") || m.HasPermission("provider:federation") {
		return true
	}
	for _, h := range m.Hooks {
		if strings.HasPrefix(strings.TrimSpace(h), "federation.") {
			return true
		}
	}
	return false
}

// CanUseSlot reports whether the plugin may inject into slotName.
func (m *Manifest) CanUseSlot(slotName string) bool {
	return m.HasPermission("ui:slots") || m.HasPermission("slots:"+slotName)
}

// EffectiveAllowedHosts collects outbound HTTP host patterns from both permissions
// ("http.outbound:<host>") and allowed_hosts when "http:outbound"/"net:http" is declared.
// Sandboxed external plugins MUST have at least one explicit allowed host pattern to make HTTP calls.
func (m *Manifest) EffectiveAllowedHosts() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(h string) {
		clean := strings.ToLower(strings.TrimSpace(h))
		if clean != "" && !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	for _, p := range m.Permissions {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "http.outbound:") {
			add(strings.TrimPrefix(p, "http.outbound:"))
		}
	}
	if m.HasPermission("http:outbound") || m.HasPermission("net:http") || len(out) > 0 {
		for _, h := range m.AllowedHosts {
			add(h)
		}
	}
	sort.Strings(out)
	return out
}

// ResolveLocalized picks the best label from a localized map (exact -> language prefix -> zh-CN -> en-US -> first key).
func ResolveLocalized(m map[string]string, locale string) string {
	if len(m) == 0 {
		return ""
	}
	norm := strings.TrimSpace(strings.ReplaceAll(locale, "_", "-"))
	if norm != "" {
		if v := strings.TrimSpace(m[norm]); v != "" {
			return v
		}
		prefix := strings.ToLower(strings.Split(norm, "-")[0])
		for k, v := range m {
			if strings.ToLower(strings.Split(strings.ReplaceAll(k, "_", "-"), "-")[0]) == prefix && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	if v := strings.TrimSpace(m["zh-CN"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(m["en-US"]); v != "" {
		return v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}

// ValidateAndMergeConfig validates an incoming partial/full config patch against ConfigSchema,
// preserving existing public fields and existing secrets when secret inputs are omitted, empty,
// or equal to SecretMaskSentinel ("********"), unless listed in incoming["__clear_secrets"].
func (m *Manifest) ValidateAndMergeConfig(
	incoming map[string]any,
	existingPublic map[string]any,
	existingSecrets map[string]string,
) (mergedPublic map[string]any, mergedSecrets map[string]string, err error) {
	mergedPublic = make(map[string]any, len(m.ConfigSchema))
	mergedSecrets = make(map[string]string)

	for k, v := range existingSecrets {
		if v != "" {
			mergedSecrets[k] = v
		}
	}

	clearSet := make(map[string]bool)
	if rawClear, ok := incoming["__clear_secrets"]; ok {
		switch xs := rawClear.(type) {
		case []string:
			for _, s := range xs {
				clearSet[s] = true
			}
		case []any:
			for _, item := range xs {
				if s, ok := item.(string); ok {
					clearSet[s] = true
				}
			}
		}
	}

	schemaByKey := make(map[string]ConfigField, len(m.ConfigSchema))
	for _, f := range m.ConfigSchema {
		schemaByKey[f.Key] = f
		if f.IsSecret() {
			if clearSet[f.Key] {
				delete(mergedSecrets, f.Key)
			}
			continue
		}
		if existingPublic != nil {
			if prev, exists := existingPublic[f.Key]; exists {
				if cleanPrev, cErr := coerceConfigValue(f, prev); cErr == nil {
					mergedPublic[f.Key] = cleanPrev
				} else {
					mergedPublic[f.Key] = prev
				}
				continue
			}
		}
		if f.Default != nil {
			coerced, cErr := coerceConfigValue(f, f.Default)
			if cErr == nil {
				mergedPublic[f.Key] = coerced
			}
		}
	}

	for k, rawVal := range incoming {
		if k == "__clear_secrets" {
			continue
		}
		field, ok := schemaByKey[k]
		if !ok {
			return nil, nil, fmt.Errorf("plugin %q: unknown config key %q", m.ID, k)
		}
		if field.IsSecret() {
			strVal, sOk := rawVal.(string)
			if !sOk {
				return nil, nil, fmt.Errorf("plugin %q: secret field %q must be a string", m.ID, k)
			}
			trimmed := strings.TrimSpace(strVal)
			if clearSet[k] || trimmed == ClearSecretSentinel {
				delete(mergedSecrets, k)
			} else if strVal != SecretMaskSentinel && trimmed != SecretMaskSentinel && trimmed != "" {
				mergedSecrets[k] = strVal
			}
			continue
		}
		coerced, cErr := coerceConfigValue(field, rawVal)
		if cErr != nil {
			return nil, nil, fmt.Errorf("plugin %q: field %q: %w", m.ID, k, cErr)
		}
		mergedPublic[k] = coerced
	}

	// Final required check across all schema fields.
	for _, f := range m.ConfigSchema {
		if !f.Required {
			continue
		}
		if f.IsSecret() {
			if strings.TrimSpace(mergedSecrets[f.Key]) == "" {
				return nil, nil, fmt.Errorf("plugin %q: required secret %q is not set", m.ID, f.Key)
			}
			continue
		}
		val, exists := mergedPublic[f.Key]
		if !exists || val == nil {
			return nil, nil, fmt.Errorf("plugin %q: required field %q cannot be empty", m.ID, f.Key)
		}
		if f.Type == "sortable_list" || f.Type == "tag_list" {
			if xs, ok := val.([]string); !ok || len(xs) == 0 {
				return nil, nil, fmt.Errorf("plugin %q: required field %q cannot be empty", m.ID, f.Key)
			}
			continue
		}
		if f.Type != "boolean" && f.Type != "number" && strings.TrimSpace(fmt.Sprint(val)) == "" {
			return nil, nil, fmt.Errorf("plugin %q: required field %q cannot be empty", m.ID, f.Key)
		}
	}

	return mergedPublic, mergedSecrets, nil
}

func stripASCIIControlsString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func splitLegacyTagListString(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	lines := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" || strings.HasPrefix(trimmedLine, "#") || strings.HasPrefix(trimmedLine, "//") {
			continue
		}
		parts := strings.FieldsFunc(trimmedLine, func(r rune) bool {
			return r == ',' || r == '，' || r == ';' || r == '；'
		})
		for _, p := range parts {
			if tp := strings.TrimSpace(p); tp != "" {
				out = append(out, tp)
			}
		}
	}
	return out
}

func coerceConfigValue(field ConfigField, raw any) (any, error) {
	switch field.Type {
	case "string", "text", "textarea":
		s, ok := raw.(string)
		if !ok {
			return nil, errors.New("expected string")
		}
		if field.Min != nil && float64(len([]rune(s))) < *field.Min {
			return nil, fmt.Errorf("must be at least %.0f characters", *field.Min)
		}
		if field.Max != nil && float64(len([]rune(s))) > *field.Max {
			return nil, fmt.Errorf("exceeds max length %.0f", *field.Max)
		}
		return s, nil

	case "color":
		s, ok := raw.(string)
		if !ok {
			return nil, errors.New("expected color string")
		}
		s = strings.TrimSpace(stripASCIIControlsString(s))
		if len(s) > 64 || strings.ContainsAny(s, ";<>{}\"'\\") {
			return nil, errors.New("invalid CSS color value")
		}
		return s, nil

	case "url", "image":
		s, ok := raw.(string)
		if !ok {
			return nil, errors.New("expected URL string")
		}
		s = strings.TrimSpace(stripASCIIControlsString(s))
		if s == "" {
			return "", nil
		}
		// Allow single-slash site-relative "/..." or absolute http(s) URLs; strictly block //, /\, javascript:, data:
		if strings.HasPrefix(s, "/") {
			if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/\\") {
				return nil, errors.New("protocol-relative URLs are not allowed")
			}
			return s, nil
		}
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("must be a valid http/https URL or site-relative path")
		}
		return s, nil

	case "boolean":
		b, ok := raw.(bool)
		if !ok {
			return nil, errors.New("expected boolean")
		}
		return b, nil

	case "number":
		var num float64
		switch v := raw.(type) {
		case float64:
			num = v
		case int:
			num = float64(v)
		case int64:
			num = float64(v)
		case json.Number:
			parsed, err := v.Float64()
			if err != nil {
				return nil, errors.New("invalid number")
			}
			num = parsed
		default:
			return nil, errors.New("expected number")
		}
		if math.IsNaN(num) || math.IsInf(num, 0) {
			return nil, errors.New("invalid number")
		}
		if field.Min != nil && num < *field.Min {
			return nil, fmt.Errorf("must be >= %v", *field.Min)
		}
		if field.Max != nil && num > *field.Max {
			return nil, fmt.Errorf("must be <= %v", *field.Max)
		}
		return num, nil

	case "select":
		s, ok := raw.(string)
		if !ok {
			return nil, errors.New("expected option string")
		}
		for _, opt := range field.Options {
			if opt.Value == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("value %q is not in allowed options", s)

	case "sortable_list":
		var rawItems []string
		switch xs := raw.(type) {
		case []string:
			rawItems = append(rawItems, xs...)
		case []any:
			rawItems = make([]string, 0, len(xs))
			for _, item := range xs {
				s, ok := item.(string)
				if !ok {
					return nil, errors.New("sortable_list items must be strings")
				}
				rawItems = append(rawItems, s)
			}
		default:
			return nil, errors.New("expected array of strings for sortable_list")
		}
		if len(rawItems) > 128 {
			return nil, errors.New("sortable_list exceeds 128 items")
		}
		allowedOpts := make(map[string]bool, len(field.Options))
		for _, opt := range field.Options {
			allowedOpts[opt.Value] = true
		}
		seen := make(map[string]bool, len(rawItems))
		out := make([]string, 0, len(rawItems))
		for _, it := range rawItems {
			clean := strings.TrimSpace(stripASCIIControlsString(it))
			if clean == "" || seen[clean] {
				continue
			}
			if len(allowedOpts) > 0 && !allowedOpts[clean] {
				return nil, fmt.Errorf("value %q is not in allowed options", clean)
			}
			seen[clean] = true
			out = append(out, clean)
		}
		return out, nil

	case "tag_list":
		maxItems := 20000
		if field.Max != nil && *field.Max > 0 {
			maxItems = int(*field.Max)
		}
		var rawItems []string
		switch xs := raw.(type) {
		case []string:
			rawItems = xs
		case []any:
			rawItems = make([]string, 0, len(xs))
			for _, item := range xs {
				s, ok := item.(string)
				if !ok {
					return nil, errors.New("tag_list items must be strings")
				}
				rawItems = append(rawItems, s)
			}
		case string:
			rawItems = splitLegacyTagListString(xs)
		default:
			return nil, errors.New("expected array of strings or delimited text for tag_list")
		}
		allowedOpts := make(map[string]bool, len(field.Options))
		for _, opt := range field.Options {
			allowedOpts[opt.Value] = true
		}
		seen := make(map[string]struct{}, len(rawItems))
		out := make([]string, 0, len(rawItems))
		for _, it := range rawItems {
			clean := strings.TrimSpace(stripASCIIControlsString(it))
			if clean == "" {
				continue
			}
			if len([]rune(clean)) > 256 {
				return nil, errors.New("tag_list item exceeds 256 characters")
			}
			if _, dup := seen[clean]; dup {
				continue
			}
			if len(allowedOpts) > 0 && !allowedOpts[clean] {
				return nil, fmt.Errorf("value %q is not in allowed options", clean)
			}
			if len(out) >= maxItems {
				return nil, fmt.Errorf("tag_list exceeds %d items", maxItems)
			}
			seen[clean] = struct{}{}
			out = append(out, clean)
		}
		if field.Min != nil && float64(len(out)) < *field.Min {
			return nil, fmt.Errorf("must contain at least %.0f items", *field.Min)
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unsupported type %q", field.Type)
	}
}
