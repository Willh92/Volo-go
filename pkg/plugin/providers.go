package plugin

import (
	"context"
	"io"
	"sort"
	"sync"
)

// Anti-spam verdict actions.
const (
	SpamActionAllow    = "allow"
	SpamActionModerate = "moderate"
	SpamActionReject   = "reject"
)

// StorageProvider allows trusted plugins to extend or replace media/object storage backends.
type StorageProvider interface {
	Name() string
	Put(ctx context.Context, objectKey string, r io.Reader, size int64, contentType string) (publicURL string, err error)
	Delete(ctx context.Context, objectKey string) error
	HealthCheck(ctx context.Context) error
}

// NotificationEvent represents an outbound event dispatched through NotifierProvider implementations.
type NotificationEvent struct {
	Event   string            `json:"event"`
	Locale  string            `json:"locale"`
	Title   string            `json:"title"`
	Summary string            `json:"summary"`
	URL     string            `json:"url,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// NotifierProvider allows plugins to provide additional push notification channels.
type NotifierProvider interface {
	Name() string
	Send(ctx context.Context, event NotificationEvent) error
}

// OIDCIdentity holds a verified external identity returned by an OIDCProvider.
type OIDCIdentity struct {
	Provider    string `json:"provider"`
	Subject     string `json:"subject"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
}

// OIDCProvider allows trusted plugins to register external single-sign-on providers.
type OIDCProvider interface {
	Name() string
	DisplayName(locale string) string
	AuthCodeURL(state string, redirectURI string) (string, error)
	Exchange(ctx context.Context, code string, redirectURI string) (OIDCIdentity, error)
}

// CommentCheckInput contains sanitized comment attributes evaluated by AntiSpamProvider.
type CommentCheckInput struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	AuthorName string `json:"author_name"`
	AuthorURL  string `json:"author_url"`
	ContentMD  string `json:"content_md"`
	IPMasked   string `json:"ip_masked"`
	GeoRegion  string `json:"geo_region"`
	Locale     string `json:"locale"`
	IsGuest    bool   `json:"is_guest"`
}

// SpamVerdict is returned by AntiSpamProvider implementations.
type SpamVerdict struct {
	Action string  `json:"action"` // allow | moderate | reject
	Score  float64 `json:"score"`
	Reason string  `json:"reason,omitempty"`
}

// AntiSpamProvider evaluates incoming comments and allows seamless hot-swapping with fallback
// to the host's built-in offline DFA + PoW anti-spam pipeline.
type AntiSpamProvider interface {
	Name() string
	CheckComment(ctx context.Context, input CommentCheckInput) (SpamVerdict, error)
}

type providerEntry[T any] struct {
	pluginID string
	provider T
}

// ProviderRegistry coordinates hot-swappable plugin providers with automatic fallback to built-in
// host providers whenever a plugin is disabled, uninstalled, or circuit-broken.
type ProviderRegistry struct {
	mu sync.RWMutex

	defaultStorage  StorageProvider
	defaultAntiSpam AntiSpamProvider

	storageList  []providerEntry[StorageProvider]
	notifierList []providerEntry[NotifierProvider]
	oidcMap      map[string]providerEntry[OIDCProvider]
	antiSpamList []providerEntry[AntiSpamProvider]
}

// NewProviderRegistry creates a thread-safe ProviderRegistry with optional built-in defaults.
func NewProviderRegistry(defaultStorage StorageProvider, defaultAntiSpam AntiSpamProvider) *ProviderRegistry {
	if defaultAntiSpam == nil {
		defaultAntiSpam = noopAntiSpamProvider{}
	}
	return &ProviderRegistry{
		defaultStorage:  defaultStorage,
		defaultAntiSpam: defaultAntiSpam,
		oidcMap:         make(map[string]providerEntry[OIDCProvider]),
	}
}

// SetDefaults updates the host's built-in fallback StorageProvider and AntiSpamProvider.
func (r *ProviderRegistry) SetDefaults(storage StorageProvider, antiSpam AntiSpamProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if storage != nil {
		r.defaultStorage = storage
	}
	if antiSpam != nil {
		r.defaultAntiSpam = antiSpam
	}
}

// RegisterStorage binds a StorageProvider owned by pluginID.
func (r *ProviderRegistry) RegisterStorage(pluginID string, p StorageProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storageList = removeEntries(r.storageList, pluginID)
	r.storageList = append(r.storageList, providerEntry[StorageProvider]{pluginID: pluginID, provider: p})
}

// RegisterNotifier binds a NotifierProvider owned by pluginID.
func (r *ProviderRegistry) RegisterNotifier(pluginID string, p NotifierProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifierList = removeEntries(r.notifierList, pluginID)
	r.notifierList = append(r.notifierList, providerEntry[NotifierProvider]{pluginID: pluginID, provider: p})
}

// RegisterOIDC binds an OIDCProvider owned by pluginID.
func (r *ProviderRegistry) RegisterOIDC(pluginID string, p OIDCProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.oidcMap[p.Name()] = providerEntry[OIDCProvider]{pluginID: pluginID, provider: p}
}

// RegisterAntiSpam binds an AntiSpamProvider owned by pluginID.
func (r *ProviderRegistry) RegisterAntiSpam(pluginID string, p AntiSpamProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.antiSpamList = removeEntries(r.antiSpamList, pluginID)
	r.antiSpamList = append(r.antiSpamList, providerEntry[AntiSpamProvider]{pluginID: pluginID, provider: p})
}

// DetachPlugin removes all providers registered by pluginID (invoked on disable, uninstall, or circuit trip).
func (r *ProviderRegistry) DetachPlugin(pluginID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storageList = removeEntries(r.storageList, pluginID)
	r.notifierList = removeEntries(r.notifierList, pluginID)
	r.antiSpamList = removeEntries(r.antiSpamList, pluginID)
	for name, ent := range r.oidcMap {
		if ent.pluginID == pluginID {
			delete(r.oidcMap, name)
		}
	}
}

// ActiveStorage returns the most recently activated plugin StorageProvider, or defaultStorage.
func (r *ProviderRegistry) ActiveStorage() StorageProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n := len(r.storageList); n > 0 {
		return r.storageList[n-1].provider
	}
	return r.defaultStorage
}

// StorageByID returns the StorageProvider registered by pluginID if active.
func (r *ProviderRegistry) StorageByID(pluginID string) (StorageProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := len(r.storageList) - 1; i >= 0; i-- {
		if r.storageList[i].pluginID == pluginID {
			return r.storageList[i].provider, true
		}
	}
	return nil, false
}

// StorageProviderIDs returns sorted IDs of all active plugin StorageProviders.
func (r *ProviderRegistry) StorageProviderIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.storageList))
	for _, ent := range r.storageList {
		ids = append(ids, ent.pluginID)
	}
	sort.Strings(ids)
	return ids
}

// Notifiers returns a snapshot of all active plugin NotifierProviders.
func (r *ProviderRegistry) Notifiers() []NotifierProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NotifierProvider, len(r.notifierList))
	for i, ent := range r.notifierList {
		out[i] = ent.provider
	}
	return out
}

// OIDC returns a named OIDCProvider if active.
func (r *ProviderRegistry) OIDC(name string) (OIDCProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ent, ok := r.oidcMap[name]
	return ent.provider, ok
}

// OIDCProviders returns all active OIDCProviders sorted deterministically by provider name.
func (r *ProviderRegistry) OIDCProviders() []OIDCProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.oidcMap))
	for name := range r.oidcMap {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]OIDCProvider, 0, len(names))
	for _, name := range names {
		out = append(out, r.oidcMap[name].provider)
	}
	return out
}

// EvaluateComment runs the active plugin AntiSpamProvider (if any); if none is active or if the
// plugin provider errors, it seamlessly falls back to defaultAntiSpam so comments never break.
func (r *ProviderRegistry) EvaluateComment(ctx context.Context, input CommentCheckInput) SpamVerdict {
	r.mu.RLock()
	var active AntiSpamProvider
	if n := len(r.antiSpamList); n > 0 {
		active = r.antiSpamList[n-1].provider
	}
	fallback := r.defaultAntiSpam
	r.mu.RUnlock()

	if active != nil {
		verdict, err := active.CheckComment(ctx, input)
		if err == nil && (verdict.Action == SpamActionAllow || verdict.Action == SpamActionModerate || verdict.Action == SpamActionReject) {
			return verdict
		}
	}
	if fallback != nil {
		verdict, err := fallback.CheckComment(ctx, input)
		if err == nil {
			return verdict
		}
	}
	return SpamVerdict{Action: SpamActionAllow}
}

func removeEntries[T any](in []providerEntry[T], pluginID string) []providerEntry[T] {
	if len(in) == 0 {
		return in
	}
	out := in[:0]
	for _, ent := range in {
		if ent.pluginID != pluginID {
			out = append(out, ent)
		}
	}
	clear(in[len(out):])
	return out
}

type noopAntiSpamProvider struct{}

func (noopAntiSpamProvider) Name() string { return "builtin-default" }
func (noopAntiSpamProvider) CheckComment(_ context.Context, _ CommentCheckInput) (SpamVerdict, error) {
	return SpamVerdict{Action: SpamActionAllow}, nil
}
