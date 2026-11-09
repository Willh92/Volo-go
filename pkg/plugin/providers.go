package plugin

import (
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Anti-spam verdict actions.
const (
	// SpamActionPass indicates no verdict override from the provider (defer to host moderation/keyword rules).
	SpamActionPass     = "pass"
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
	Provider      string `json:"provider"`
	Subject       string `json:"subject"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Nonce         string `json:"nonce,omitempty"`
	DisplayName   string `json:"display_name"`
	AvatarURL     string `json:"avatar_url"`
}

// OIDCAuthParams carries authorization URL parameters including OIDC nonce and PKCE S256 challenge.
type OIDCAuthParams struct {
	State               string `json:"state"`
	RedirectURI         string `json:"redirect_uri"`
	Nonce               string `json:"nonce,omitempty"`
	CodeChallenge       string `json:"code_challenge,omitempty"`
	CodeChallengeMethod string `json:"code_challenge_method,omitempty"`
}

// OIDCExchangeParams carries authorization code exchange parameters including OIDC nonce and PKCE verifier.
type OIDCExchangeParams struct {
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	Nonce        string `json:"nonce,omitempty"`
	CodeVerifier string `json:"code_verifier,omitempty"`
}

// OIDCProvider allows trusted plugins to register external single-sign-on providers.
type OIDCProvider interface {
	Name() string
	DisplayName(locale string) string
	AuthCodeURL(state string, redirectURI string) (string, error)
	Exchange(ctx context.Context, code string, redirectURI string) (OIDCIdentity, error)
}

// OIDCAdvancedProvider extends OIDCProvider with OIDC nonce and PKCE S256 parameter forwarding.
type OIDCAdvancedProvider interface {
	OIDCProvider
	AuthCodeURLWithParams(params OIDCAuthParams) (string, error)
	ExchangeWithParams(ctx context.Context, params OIDCExchangeParams) (OIDCIdentity, error)
}

// OIDCDiscoveryInvalidator may be implemented by OIDCProvider or BuiltinPlugin implementations
// that cache OIDC discovery (.well-known/openid-configuration) or JWKS metadata in memory.
type OIDCDiscoveryInvalidator interface {
	InvalidateOIDCDiscovery()
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

// NicknameCheckInput contains candidate account/profile nickname attributes evaluated during registration or OIDC provisioning.
type NicknameCheckInput struct {
	Nickname  string `json:"nickname"`
	Email     string `json:"email,omitempty"`
	IPMasked  string `json:"ip_masked,omitempty"`
	GeoRegion string `json:"geo_region,omitempty"`
	Locale    string `json:"locale"`
	Source    string `json:"source"`
}

// SpamVerdict is returned by AntiSpamProvider implementations.
type SpamVerdict struct {
	Action string  `json:"action"` // pass | allow | moderate | reject
	Score  float64 `json:"score"`
	Reason string  `json:"reason,omitempty"`
}

// AntiSpamProvider evaluates incoming comments and allows seamless hot-swapping with fallback
// to the host's built-in offline DFA + PoW anti-spam pipeline.
type AntiSpamProvider interface {
	Name() string
	CheckComment(ctx context.Context, input CommentCheckInput) (SpamVerdict, error)
}

// NicknameAntiSpamProvider is an optional extension implemented by AntiSpamProvider plugins
// that screen candidate registration/profile nicknames (`checkNickname(input)` in JS).
type NicknameAntiSpamProvider interface {
	AntiSpamProvider
	CheckNickname(ctx context.Context, input NicknameCheckInput) (SpamVerdict, error)
}

// ContentSanitizerProvider is an optional extension implemented by AntiSpamProvider plugins
// to non-destructively mask sensitive words in existing/displayed HTML, plain text, and nicknames
// with '*' without mutating persisted database rows.
type ContentSanitizerProvider interface {
	AntiSpamProvider
	MaskHTML(ctx context.Context, htmlStr string) string
	MaskText(ctx context.Context, text string) string
	MaskNickname(ctx context.Context, nickname string, isStaffOrAuthor bool) string
	SanitizerStateToken() string
}

// FederationProvider allows plugins with federation capability to publish/deliver article lifecycle events.
type FederationProvider interface {
	Name() string
	Publish(ctx context.Context, action string, articlePayload map[string]any) error
}

type providerEntry[T any] struct {
	pluginID string
	priority int
	provider T
}

// ProviderRegistry coordinates hot-swappable plugin providers with automatic fallback to built-in
// host providers whenever a plugin is disabled, uninstalled, or circuit-broken.
type ProviderRegistry struct {
	mu sync.RWMutex

	defaultStorage   StorageProvider
	defaultAntiSpam  AntiSpamProvider
	pluginPriorities map[string]int

	storageList    []providerEntry[StorageProvider]
	notifierList   []providerEntry[NotifierProvider]
	oidcMap        map[string]providerEntry[OIDCProvider]
	antiSpamList   []providerEntry[AntiSpamProvider]
	federationList []providerEntry[FederationProvider]
}

// NewProviderRegistry creates a thread-safe ProviderRegistry with optional built-in defaults.
func NewProviderRegistry(defaultStorage StorageProvider, defaultAntiSpam AntiSpamProvider) *ProviderRegistry {
	if defaultAntiSpam == nil {
		defaultAntiSpam = noopAntiSpamProvider{}
	}
	return &ProviderRegistry{
		defaultStorage:   defaultStorage,
		defaultAntiSpam:  defaultAntiSpam,
		pluginPriorities: make(map[string]int),
		oidcMap:          make(map[string]providerEntry[OIDCProvider]),
	}
}

func (r *ProviderRegistry) priorityForLocked(pluginID string) int {
	if r.pluginPriorities != nil {
		if p, ok := r.pluginPriorities[pluginID]; ok {
			return p
		}
	}
	return DefaultPluginPriority
}

func sortProviderChain[T any](list []providerEntry[T]) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].priority != list[j].priority {
			return list[i].priority < list[j].priority
		}
		return list[i].pluginID < list[j].pluginID
	})
}

func sortStorageEntries(list []providerEntry[StorageProvider]) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].priority != list[j].priority {
			return list[i].priority < list[j].priority
		}
		return list[i].pluginID < list[j].pluginID
	})
}

// SetPluginPriority updates a plugin's execution priority and re-sorts all active provider chains.
func (r *ProviderRegistry) SetPluginPriority(pluginID string, priority int) {
	p := ClampPluginPriority(priority)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pluginPriorities == nil {
		r.pluginPriorities = make(map[string]int)
	}
	r.pluginPriorities[pluginID] = p
	for i := range r.storageList {
		if r.storageList[i].pluginID == pluginID {
			r.storageList[i].priority = p
		}
	}
	sortStorageEntries(r.storageList)
	for i := range r.notifierList {
		if r.notifierList[i].pluginID == pluginID {
			r.notifierList[i].priority = p
		}
	}
	sortProviderChain(r.notifierList)
	for i := range r.antiSpamList {
		if r.antiSpamList[i].pluginID == pluginID {
			r.antiSpamList[i].priority = p
		}
	}
	sortProviderChain(r.antiSpamList)
	for i := range r.federationList {
		if r.federationList[i].pluginID == pluginID {
			r.federationList[i].priority = p
		}
	}
	sortProviderChain(r.federationList)
	for name, ent := range r.oidcMap {
		if ent.pluginID == pluginID {
			ent.priority = p
			r.oidcMap[name] = ent
		}
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
	r.storageList = append(r.storageList, providerEntry[StorageProvider]{
		pluginID: pluginID,
		priority: r.priorityForLocked(pluginID),
		provider: p,
	})
	sortStorageEntries(r.storageList)
}

// RegisterNotifier binds a NotifierProvider owned by pluginID.
func (r *ProviderRegistry) RegisterNotifier(pluginID string, p NotifierProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifierList = removeEntries(r.notifierList, pluginID)
	r.notifierList = append(r.notifierList, providerEntry[NotifierProvider]{
		pluginID: pluginID,
		priority: r.priorityForLocked(pluginID),
		provider: p,
	})
	sortProviderChain(r.notifierList)
}

// RegisterOIDC binds an OIDCProvider owned by pluginID.
func (r *ProviderRegistry) RegisterOIDC(pluginID string, p OIDCProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.oidcMap[p.Name()] = providerEntry[OIDCProvider]{
		pluginID: pluginID,
		priority: r.priorityForLocked(pluginID),
		provider: p,
	}
}

// RegisterAntiSpam binds an AntiSpamProvider owned by pluginID.
func (r *ProviderRegistry) RegisterAntiSpam(pluginID string, p AntiSpamProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.antiSpamList = removeEntries(r.antiSpamList, pluginID)
	r.antiSpamList = append(r.antiSpamList, providerEntry[AntiSpamProvider]{
		pluginID: pluginID,
		priority: r.priorityForLocked(pluginID),
		provider: p,
	})
	sortProviderChain(r.antiSpamList)
}

// RegisterFederation binds a FederationProvider owned by pluginID.
func (r *ProviderRegistry) RegisterFederation(pluginID string, p FederationProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.federationList = removeEntries(r.federationList, pluginID)
	r.federationList = append(r.federationList, providerEntry[FederationProvider]{
		pluginID: pluginID,
		priority: r.priorityForLocked(pluginID),
		provider: p,
	})
	sortProviderChain(r.federationList)
}

// DetachPlugin removes all providers registered by pluginID (invoked on disable, uninstall, or circuit trip).
func (r *ProviderRegistry) DetachPlugin(pluginID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storageList = removeEntries(r.storageList, pluginID)
	r.notifierList = removeEntries(r.notifierList, pluginID)
	r.antiSpamList = removeEntries(r.antiSpamList, pluginID)
	r.federationList = removeEntries(r.federationList, pluginID)
	for name, ent := range r.oidcMap {
		if ent.pluginID == pluginID {
			delete(r.oidcMap, name)
		}
	}
}

// DetachOIDCPlugin removes only OIDC providers registered by pluginID.
func (r *ProviderRegistry) DetachOIDCPlugin(pluginID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, ent := range r.oidcMap {
		if ent.pluginID == pluginID {
			delete(r.oidcMap, name)
		}
	}
}

// Federations returns a snapshot of all active plugin FederationProviders.
func (r *ProviderRegistry) Federations() []FederationProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]FederationProvider, len(r.federationList))
	for i, ent := range r.federationList {
		out[i] = ent.provider
	}
	return out
}

// ActiveStorage returns the highest-priority active plugin StorageProvider, or defaultStorage.
func (r *ProviderRegistry) ActiveStorage() StorageProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.storageList) > 0 {
		return r.storageList[0].provider
	}
	return r.defaultStorage
}

// StorageByID returns the StorageProvider registered by pluginID if active.
func (r *ProviderRegistry) StorageByID(pluginID string) (StorageProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.storageList {
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

// OIDCProvidersForPlugin returns all active OIDCProviders owned by pluginID.
func (r *ProviderRegistry) OIDCProvidersForPlugin(pluginID string) []OIDCProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []OIDCProvider
	for _, ent := range r.oidcMap {
		if ent.pluginID == pluginID && ent.provider != nil {
			out = append(out, ent.provider)
		}
	}
	return out
}

// OIDCProviders returns all active OIDCProviders sorted deterministically by plugin priority then provider name.
func (r *ProviderRegistry) OIDCProviders() []OIDCProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.oidcMap))
	for name := range r.oidcMap {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		pi := r.oidcMap[names[i]].priority
		pj := r.oidcMap[names[j]].priority
		if pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})
	out := make([]OIDCProvider, 0, len(names))
	for _, name := range names {
		out = append(out, r.oidcMap[name].provider)
	}
	return out
}

func antiSpamActionRank(action string) int {
	switch action {
	case SpamActionReject:
		return 3
	case SpamActionModerate:
		return 2
	case SpamActionAllow:
		return 1
	case SpamActionPass:
		return 0
	default:
		return -1
	}
}

func mergeSpamVerdict(cur SpamVerdict, hasCur bool, cand SpamVerdict) (SpamVerdict, bool) {
	rank := antiSpamActionRank(cand.Action)
	if rank < 0 {
		return cur, hasCur
	}
	if !hasCur || rank > antiSpamActionRank(cur.Action) {
		if cand.Score < cur.Score {
			cand.Score = cur.Score
		}
		if cand.Reason == "" {
			cand.Reason = cur.Reason
		}
		return cand, true
	}
	if cand.Score > cur.Score {
		cur.Score = cand.Score
	}
	if cur.Reason == "" && cand.Reason != "" && rank == antiSpamActionRank(cur.Action) {
		cur.Reason = cand.Reason
	}
	return cur, true
}

// EvaluateComment runs all active plugin AntiSpamProviders in registration order (short-circuiting on
// reject and ratcheting moderate > allow > pass); if none are active or all error, falls back to defaultAntiSpam.
func (r *ProviderRegistry) EvaluateComment(ctx context.Context, input CommentCheckInput) SpamVerdict {
	r.mu.RLock()
	active := make([]AntiSpamProvider, len(r.antiSpamList))
	for i, ent := range r.antiSpamList {
		active[i] = ent.provider
	}
	fallback := r.defaultAntiSpam
	r.mu.RUnlock()

	var merged SpamVerdict
	hasVerdict := false
	for _, p := range active {
		if p == nil {
			continue
		}
		verdict, err := p.CheckComment(ctx, input)
		if err != nil {
			continue
		}
		if verdict.Action == SpamActionReject {
			return verdict
		}
		merged, hasVerdict = mergeSpamVerdict(merged, hasVerdict, verdict)
	}
	if hasVerdict {
		return merged
	}
	if fallback != nil {
		verdict, err := fallback.CheckComment(ctx, input)
		if err == nil && antiSpamActionRank(verdict.Action) >= 0 {
			return verdict
		}
	}
	return SpamVerdict{Action: SpamActionPass}
}

// EvaluateNickname screens a candidate account/profile nickname across all active AntiSpamProviders
// (preferring NicknameAntiSpamProvider.CheckNickname when implemented, or falling back to CheckComment).
func (r *ProviderRegistry) EvaluateNickname(ctx context.Context, input NicknameCheckInput) SpamVerdict {
	r.mu.RLock()
	active := make([]AntiSpamProvider, len(r.antiSpamList))
	for i, ent := range r.antiSpamList {
		active[i] = ent.provider
	}
	fallback := r.defaultAntiSpam
	r.mu.RUnlock()

	evalOne := func(p AntiSpamProvider) (SpamVerdict, bool) {
		if p == nil {
			return SpamVerdict{}, false
		}
		var (
			verdict SpamVerdict
			err     error
		)
		if np, ok := p.(NicknameAntiSpamProvider); ok {
			verdict, err = np.CheckNickname(ctx, input)
		} else {
			verdict, err = p.CheckComment(ctx, CommentCheckInput{
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
		if err == nil && antiSpamActionRank(verdict.Action) >= 0 {
			return verdict, true
		}
		return SpamVerdict{}, false
	}

	var merged SpamVerdict
	hasVerdict := false
	for _, p := range active {
		v, ok := evalOne(p)
		if !ok {
			continue
		}
		if v.Action == SpamActionReject {
			return v
		}
		merged, hasVerdict = mergeSpamVerdict(merged, hasVerdict, v)
	}
	if hasVerdict {
		return merged
	}
	if v, ok := evalOne(fallback); ok {
		return v
	}
	return SpamVerdict{Action: SpamActionPass}
}

func (r *ProviderRegistry) activeSanitizers() []providerEntry[ContentSanitizerProvider] {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.antiSpamList) == 0 {
		return nil
	}
	var out []providerEntry[ContentSanitizerProvider]
	for _, ent := range r.antiSpamList {
		if sp, ok := ent.provider.(ContentSanitizerProvider); ok && sp != nil {
			out = append(out, providerEntry[ContentSanitizerProvider]{
				pluginID: ent.pluginID,
				priority: ent.priority,
				provider: sp,
			})
		}
	}
	return out
}

// SanitizerStateHash returns a fast cache-coherence token reflecting all active ContentSanitizerProviders,
// their execution priority order, and their dictionary/configuration revisions.
func (r *ProviderRegistry) SanitizerStateHash() string {
	list := r.activeSanitizers()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	for _, ent := range list {
		tok := safeSanitizerCall("", ent.provider.SanitizerStateToken)
		if tok == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('|')
		}
		b.WriteString(ent.pluginID)
		b.WriteByte('#')
		b.WriteString(strconv.Itoa(ent.priority))
		b.WriteByte(':')
		b.WriteString(tok)
	}
	return b.String()
}

// MaskCommentHTML runs all active ContentSanitizerProviders in priority order over rendered comment HTML.
func (r *ProviderRegistry) MaskCommentHTML(ctx context.Context, htmlStr string) string {
	if r == nil || strings.TrimSpace(htmlStr) == "" {
		return htmlStr
	}
	list := r.activeSanitizers()
	if len(list) == 0 {
		return htmlStr
	}
	cur := htmlStr
	for _, ent := range list {
		sp := ent.provider
		cur = safeSanitizerCall(cur, func() string {
			return sp.MaskHTML(ctx, cur)
		})
	}
	return cur
}

// MaskCommentText runs all active ContentSanitizerProviders in priority order over plain text.
func (r *ProviderRegistry) MaskCommentText(ctx context.Context, text string) string {
	if r == nil || strings.TrimSpace(text) == "" {
		return text
	}
	list := r.activeSanitizers()
	if len(list) == 0 {
		return text
	}
	cur := text
	for _, ent := range list {
		sp := ent.provider
		cur = safeSanitizerCall(cur, func() string {
			return sp.MaskText(ctx, cur)
		})
	}
	return cur
}

// MaskNickname runs all active ContentSanitizerProviders in priority order over a displayed nickname.
func (r *ProviderRegistry) MaskNickname(ctx context.Context, nickname string, isStaffOrAuthor bool) string {
	if r == nil || strings.TrimSpace(nickname) == "" {
		return nickname
	}
	list := r.activeSanitizers()
	if len(list) == 0 {
		return nickname
	}
	cur := nickname
	for _, ent := range list {
		sp := ent.provider
		cur = safeSanitizerCall(cur, func() string {
			return sp.MaskNickname(ctx, cur, isStaffOrAuthor)
		})
	}
	return cur
}

func safeSanitizerCall(fallback string, fn func() string) (out string) {
	out = fallback
	defer func() {
		if recover() != nil {
			out = fallback
		}
	}()
	return fn()
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
	return SpamVerdict{Action: SpamActionPass}, nil
}
