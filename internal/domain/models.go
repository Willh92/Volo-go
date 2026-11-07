package domain

// User roles (Sovereign Identity & RBAC).
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleReader = "reader"

	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

// Article statuses.
const (
	ArticleStatusPublished = "published"
	ArticleStatusDraft     = "draft"
	ArticleStatusScheduled = "scheduled"
)

// Comment statuses & access/moderation modes.
const (
	CommentStatusApproved = "approved"
	CommentStatusPending  = "pending"
	CommentStatusRejected = "rejected"

	CommentAccessAll        = "all"
	CommentAccessRegistered = "registered"
	CommentAccessOff        = "off"

	CommentModerationOff   = "off"
	CommentModerationSmart = "smart"
	CommentModerationAll   = "all"

	TargetTypeArticle = "article"
	TargetTypePage    = "page"
)

// Domain-scoped atomic settings keys.
const (
	SettingsDomainSite          = "site"
	SettingsDomainReading       = "reading"
	SettingsDomainFeed          = "feed"
	SettingsDomainSEO           = "seo"
	SettingsDomainStorage       = "storage"
	SettingsDomainNotifications = "notifications"
	SettingsDomainSecurity      = "security"
	SettingsDomainAppearance    = "appearance"
	SettingsDomainBackup        = "backup"
)

type SettingRecord struct {
	Domain           string `db:"domain" json:"domain"`
	ValueJSON        string `db:"value_json" json:"value_json"`
	SecretCiphertext string `db:"secret_ciphertext" json:"secret_ciphertext,omitempty"`
	Version          int64  `db:"version" json:"version"`
	UpdatedAt        int64  `db:"updated_at" json:"updated_at"`
}

type User struct {
	ID              string `db:"id" json:"id"`
	Username        string `db:"username" json:"username"`
	Email           string `db:"email" json:"email"`
	DisplayName     string `db:"display_name" json:"display_name"`
	AvatarURL       string `db:"avatar_url" json:"avatar_url"`
	Bio             string `db:"bio" json:"bio"`
	WebsiteURL      string `db:"website_url" json:"website_url"`
	Role            string `db:"role" json:"role"`
	PasswordHash    string `db:"password_hash" json:"password_hash,omitempty"`
	LegacyHash      string `db:"legacy_hash" json:"legacy_hash,omitempty"`
	Status          string `db:"status" json:"status"`
	EmailVerifiedAt int64  `db:"email_verified_at" json:"email_verified_at"`
	PreferredLocale string `db:"preferred_locale" json:"preferred_locale"`
	CreatedAt       int64  `db:"created_at" json:"created_at"`
	UpdatedAt       int64  `db:"updated_at" json:"updated_at"`
}

type UserCredential struct {
	ID               string `db:"id" json:"id"`
	UserID           string `db:"user_id" json:"user_id"`
	Kind             string `db:"kind" json:"kind"` // passkey | totp | recovery_codes
	Name             string `db:"name" json:"name"`
	CredentialID     string `db:"credential_id" json:"credential_id"`
	PublicKeySPKI    string `db:"public_key_spki" json:"public_key_spki,omitempty"`
	COSEAlg          int    `db:"cose_alg" json:"cose_alg"`
	SignCount        int64  `db:"sign_count" json:"sign_count"`
	SecretCiphertext string `db:"secret_ciphertext" json:"secret_ciphertext,omitempty"`
	MetaJSON         string `db:"meta_json" json:"meta_json"`
	LastUsedAt       int64  `db:"last_used_at" json:"last_used_at"`
	CreatedAt        int64  `db:"created_at" json:"created_at"`
}

type UserSession struct {
	ID                string `db:"id" json:"id"`
	UserID            string `db:"user_id" json:"user_id"`
	TokenHash         string `db:"token_hash" json:"token_hash,omitempty"`
	IPMasked          string `db:"ip_masked" json:"ip_masked"`
	GeoRegion         string `db:"geo_region" json:"geo_region"`
	UserAgent         string `db:"user_agent" json:"user_agent"`
	TOTPVerifiedUntil int64  `db:"totp_verified_until" json:"totp_verified_until"`
	ExpiresAt         int64  `db:"expires_at" json:"expires_at"`
	LastSeenAt        int64  `db:"last_seen_at" json:"last_seen_at"`
	CreatedAt         int64  `db:"created_at" json:"created_at"`
}

type APIToken struct {
	ID          string `db:"id" json:"id"`
	UserID      string `db:"user_id" json:"user_id"`
	Name        string `db:"name" json:"name"`
	TokenPrefix string `db:"token_prefix" json:"token_prefix"`
	TokenHash   string `db:"token_hash" json:"token_hash,omitempty"`
	ScopesJSON  string `db:"scopes_json" json:"scopes_json"`
	LastUsedAt  int64  `db:"last_used_at" json:"last_used_at"`
	ExpiresAt   int64  `db:"expires_at" json:"expires_at"`
	CreatedAt   int64  `db:"created_at" json:"created_at"`
}

type Category struct {
	ID          string `db:"id" json:"id"`
	Name        string `db:"name" json:"name"`
	Slug        string `db:"slug" json:"slug"`
	Description string `db:"description" json:"description"`
	Icon        string `db:"icon" json:"icon"`
	SortOrder   int    `db:"sort_order" json:"sort_order"`
	CreatedAt   int64  `db:"created_at" json:"created_at"`
	UpdatedAt   int64  `db:"updated_at" json:"updated_at"`
}

type Tag struct {
	ID        string `db:"id" json:"id"`
	Name      string `db:"name" json:"name"`
	Slug      string `db:"slug" json:"slug"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
}

type ArticleTag struct {
	ArticleID string `db:"article_id" json:"article_id"`
	TagID     string `db:"tag_id" json:"tag_id"`
}

// RawArticle represents the full database record before visibility redaction.
// Public endpoints MUST convert RawArticle through visibility.Guard into PublicArticle.
type RawArticle struct {
	ID                 string `db:"id" json:"id"`
	AuthorID           string `db:"author_id" json:"author_id"`
	CategoryID         string `db:"category_id" json:"category_id"`
	Title              string `db:"title" json:"title"`
	Slug               string `db:"slug" json:"slug"`
	LegacyPermalink    string `db:"legacy_permalink" json:"legacy_permalink"`
	SummaryMD          string `db:"summary_md" json:"summary_md"`
	ContentMD          string `db:"content_md" json:"content_md"`
	CoverURL           string `db:"cover_url" json:"cover_url"`
	Status             string `db:"status" json:"status"`
	PublishedAt        int64  `db:"published_at" json:"published_at"`
	ScheduledAt        int64  `db:"scheduled_at" json:"scheduled_at"`
	IsPinned           bool   `db:"is_pinned" json:"is_pinned"`
	AllowComment       bool   `db:"allow_comment" json:"allow_comment"`
	PasswordCiphertext string `db:"password_ciphertext" json:"password_ciphertext,omitempty"`
	HasContentLock     bool   `db:"has_content_lock" json:"has_content_lock"`
	AudioURL           string `db:"audio_url" json:"audio_url"`
	AudioDurationSec   int    `db:"audio_duration_sec" json:"audio_duration_sec"`
	AudioSizeBytes     int64  `db:"audio_size_bytes" json:"audio_size_bytes"`
	AudioMIME          string `db:"audio_mime" json:"audio_mime"`
	Lang               string `db:"lang" json:"lang"`
	TranslationGroup   string `db:"translation_group" json:"translation_group"`
	LicenseOverride    string `db:"license_override" json:"license_override"`
	WordCount          int    `db:"word_count" json:"word_count"`
	ReadingMinutes     int    `db:"reading_minutes" json:"reading_minutes"`
	ViewCount          int64  `db:"view_count" json:"view_count"`
	CommentCount       int    `db:"comment_count" json:"comment_count"`
	CreatedAt          int64  `db:"created_at" json:"created_at"`
	UpdatedAt          int64  `db:"updated_at" json:"updated_at"`
}

type ArticleRevision struct {
	ID         string `db:"id" json:"id"`
	ArticleID  string `db:"article_id" json:"article_id"`
	Title      string `db:"title" json:"title"`
	SummaryMD  string `db:"summary_md" json:"summary_md"`
	ContentMD  string `db:"content_md" json:"content_md"`
	AuthorName string `db:"author_name" json:"author_name"`
	Note       string `db:"note" json:"note"`
	CreatedAt  int64  `db:"created_at" json:"created_at"`
}

type ArticleBacklink struct {
	SourceArticleID string `db:"source_article_id" json:"source_article_id"`
	TargetArticleID string `db:"target_article_id" json:"target_article_id"`
	UpdatedAt       int64  `db:"updated_at" json:"updated_at"`
}

type Page struct {
	ID               string `db:"id" json:"id"`
	AuthorID         string `db:"author_id" json:"author_id"`
	Title            string `db:"title" json:"title"`
	Slug             string `db:"slug" json:"slug"`
	ContentMD        string `db:"content_md" json:"content_md"`
	IsPublished      bool   `db:"is_published" json:"is_published"`
	AllowComment     bool   `db:"allow_comment" json:"allow_comment"`
	ShowTOC          bool   `db:"show_toc" json:"show_toc"`
	Lang             string `db:"lang" json:"lang"`
	TranslationGroup string `db:"translation_group" json:"translation_group"`
	ViewCount        int64  `db:"view_count" json:"view_count"`
	CommentCount     int    `db:"comment_count" json:"comment_count"`
	CreatedAt        int64  `db:"created_at" json:"created_at"`
	UpdatedAt        int64  `db:"updated_at" json:"updated_at"`
}

type NavItem struct {
	ID             string `db:"id" json:"id"`
	ParentID       string `db:"parent_id" json:"parent_id"`
	Title          string `db:"title" json:"title"`
	I18nTitlesJSON string `db:"i18n_titles_json" json:"i18n_titles_json"`
	URL            string `db:"url" json:"url"`
	OpenInNewTab   bool   `db:"open_in_new_tab" json:"open_in_new_tab"`
	Icon           string `db:"icon" json:"icon"`
	SortOrder      int    `db:"sort_order" json:"sort_order"`
	IsEnabled      bool   `db:"is_enabled" json:"is_enabled"`
	CreatedAt      int64  `db:"created_at" json:"created_at"`
	UpdatedAt      int64  `db:"updated_at" json:"updated_at"`
}

type Series struct {
	ID          string `db:"id" json:"id"`
	Title       string `db:"title" json:"title"`
	Slug        string `db:"slug" json:"slug"`
	Description string `db:"description" json:"description"`
	CoverURL    string `db:"cover_url" json:"cover_url"`
	SortOrder   int    `db:"sort_order" json:"sort_order"`
	CreatedAt   int64  `db:"created_at" json:"created_at"`
	UpdatedAt   int64  `db:"updated_at" json:"updated_at"`
}

type SeriesArticle struct {
	SeriesID  string `db:"series_id" json:"series_id"`
	ArticleID string `db:"article_id" json:"article_id"`
	SortOrder int    `db:"sort_order" json:"sort_order"`
}

type Book struct {
	ID           string `db:"id" json:"id"`
	Title        string `db:"title" json:"title"`
	Slug         string `db:"slug" json:"slug"`
	Description  string `db:"description" json:"description"`
	CoverURL     string `db:"cover_url" json:"cover_url"`
	SourceType   string `db:"source_type" json:"source_type"` // auto | gitbook | mdbook | vuepress | series | manual
	SourceURL    string `db:"source_url" json:"source_url"`
	SeriesID     string `db:"series_id" json:"series_id"`
	EmbedMode    string `db:"embed_mode" json:"embed_mode"` // iframe | link
	ChapterCount int    `db:"chapter_count" json:"chapter_count"`
	SyncedAt     int64  `db:"synced_at" json:"synced_at"`
	SortOrder    int    `db:"sort_order" json:"sort_order"`
	CreatedAt    int64  `db:"created_at" json:"created_at"`
	UpdatedAt    int64  `db:"updated_at" json:"updated_at"`
}

type BookChapter struct {
	ID        string `db:"id" json:"id"`
	BookID    string `db:"book_id" json:"book_id"`
	ParentID  string `db:"parent_id" json:"parent_id"`
	Title     string `db:"title" json:"title"`
	URL       string `db:"url" json:"url"`
	Depth     int    `db:"depth" json:"depth"`
	SortOrder int    `db:"sort_order" json:"sort_order"`
}

type Comment struct {
	ID                    string `db:"id" json:"id"`
	TargetType            string `db:"target_type" json:"target_type"` // article | page
	TargetID              string `db:"target_id" json:"target_id"`
	ParentID              string `db:"parent_id" json:"parent_id"`
	ReplyToName           string `db:"reply_to_name" json:"reply_to_name"`
	UserID                string `db:"user_id" json:"user_id"`
	GuestIDHash           string `db:"guest_id_hash" json:"guest_id_hash,omitempty"`
	EnvFPHash             string `db:"env_fp_hash" json:"env_fp_hash,omitempty"`
	AuthorName            string `db:"author_name" json:"author_name"`
	AuthorURL             string `db:"author_url" json:"author_url"`
	AvatarURL             string `db:"avatar_url" json:"avatar_url"`
	NotifyEmailCiphertext string `db:"notify_email_ciphertext" json:"notify_email_ciphertext,omitempty"`
	NotifyLocale          string `db:"notify_locale" json:"notify_locale"`
	ClientIP              string `db:"client_ip" json:"client_ip,omitempty"`
	IPMasked              string `db:"ip_masked" json:"ip_masked"`
	GeoRegion             string `db:"geo_region" json:"geo_region"`
	ContentMD             string `db:"content_md" json:"content_md"`
	Status                string `db:"status" json:"status"`
	IsDeleted             bool   `db:"is_deleted" json:"is_deleted"`
	CreatedAt             int64  `db:"created_at" json:"created_at"`
	UpdatedAt             int64  `db:"updated_at" json:"updated_at"`
}

type Reaction struct {
	TargetType string `db:"target_type" json:"target_type"`
	TargetID   string `db:"target_id" json:"target_id"`
	ActorHash  string `db:"actor_hash" json:"actor_hash"`
	Kind       string `db:"kind" json:"kind"`
	CreatedAt  int64  `db:"created_at" json:"created_at"`
}

type Link struct {
	ID            string `db:"id" json:"id"`
	Title         string `db:"title" json:"title"`
	URL           string `db:"url" json:"url"`
	Description   string `db:"description" json:"description"`
	IconURL       string `db:"icon_url" json:"icon_url"`
	CategoryName  string `db:"category_name" json:"category_name"`
	SortOrder     int    `db:"sort_order" json:"sort_order"`
	HealthStatus  string `db:"health_status" json:"health_status"` // unknown | ok | redirect | broken | blocked
	HTTPCode      int    `db:"http_code" json:"http_code"`
	LatencyMS     int    `db:"latency_ms" json:"latency_ms"`
	HealthError   string `db:"health_error" json:"health_error"`
	LastCheckedAt int64  `db:"last_checked_at" json:"last_checked_at"`
	CreatedAt     int64  `db:"created_at" json:"created_at"`
	UpdatedAt     int64  `db:"updated_at" json:"updated_at"`
}

type Follow struct {
	ID           string `db:"id" json:"id"`
	Title        string `db:"title" json:"title"`
	SiteURL      string `db:"site_url" json:"site_url"`
	FeedURL      string `db:"feed_url" json:"feed_url"`
	Description  string `db:"description" json:"description"`
	IconURL      string `db:"icon_url" json:"icon_url"`
	SortOrder    int    `db:"sort_order" json:"sort_order"`
	LastSyncedAt int64  `db:"last_synced_at" json:"last_synced_at"`
	SyncError    string `db:"sync_error" json:"sync_error"`
	CreatedAt    int64  `db:"created_at" json:"created_at"`
	UpdatedAt    int64  `db:"updated_at" json:"updated_at"`
}

type FollowItem struct {
	ID          string `db:"id" json:"id"`
	FollowID    string `db:"follow_id" json:"follow_id"`
	GUIDHash    string `db:"guid_hash" json:"guid_hash"`
	Title       string `db:"title" json:"title"`
	URL         string `db:"url" json:"url"`
	Summary     string `db:"summary" json:"summary"`
	AuthorName  string `db:"author_name" json:"author_name"`
	PublishedAt int64  `db:"published_at" json:"published_at"`
}

type MediaAsset struct {
	ID            string `db:"id" json:"id"`
	StorageDriver string `db:"storage_driver" json:"storage_driver"` // local | s3
	ObjectKey     string `db:"object_key" json:"object_key"`
	PublicURL     string `db:"public_url" json:"public_url"`
	OriginalName  string `db:"original_name" json:"original_name"`
	AltText       string `db:"alt_text" json:"alt_text"`
	MIMEType      string `db:"mime_type" json:"mime_type"`
	SizeBytes     int64  `db:"size_bytes" json:"size_bytes"`
	Width         int    `db:"width" json:"width"`
	Height        int    `db:"height" json:"height"`
	SHA256        string `db:"sha256" json:"sha256"`
	EXIFStripped  bool   `db:"exif_stripped" json:"exif_stripped"`
	UploaderID    string `db:"uploader_id" json:"uploader_id"`
	CreatedAt     int64  `db:"created_at" json:"created_at"`
}

type RedirectRule struct {
	ID             string `db:"id" json:"id"`
	SourcePath     string `db:"source_path" json:"source_path"`
	TargetPath     string `db:"target_path" json:"target_path"`
	StatusCode     int    `db:"status_code" json:"status_code"` // 301 | 302
	IsSystemLegacy bool   `db:"is_system_legacy" json:"is_system_legacy"`
	HitCount       int64  `db:"hit_count" json:"hit_count"`
	CreatedAt      int64  `db:"created_at" json:"created_at"`
}

type Webhook struct {
	ID               string `db:"id" json:"id"`
	Name             string `db:"name" json:"name"`
	Provider         string `db:"provider" json:"provider"` // feishu | wecom | dingtalk | bark | telegram | custom
	URL              string `db:"url" json:"url"`
	SecretCiphertext string `db:"secret_ciphertext" json:"secret_ciphertext,omitempty"`
	ChatID           string `db:"chat_id" json:"chat_id"`
	EventsJSON       string `db:"events_json" json:"events_json"`
	IsEnabled        bool   `db:"is_enabled" json:"is_enabled"`
	LastStatusCode   int    `db:"last_status_code" json:"last_status_code"`
	LastError        string `db:"last_error" json:"last_error"`
	LastTriggeredAt  int64  `db:"last_triggered_at" json:"last_triggered_at"`
	CreatedAt        int64  `db:"created_at" json:"created_at"`
	UpdatedAt        int64  `db:"updated_at" json:"updated_at"`
}

type AnalyticsDaily struct {
	StatDate  string `db:"stat_date" json:"stat_date"` // YYYY-MM-DD
	PV        int64  `db:"pv" json:"pv"`
	UV        int64  `db:"uv" json:"uv"`
	UpdatedAt int64  `db:"updated_at" json:"updated_at"`
}

type AnalyticsTopItem struct {
	StatDate string `db:"stat_date" json:"stat_date"`
	Kind     string `db:"kind" json:"kind"` // article | referer
	ItemKey  string `db:"item_key" json:"item_key"`
	Hits     int64  `db:"hits" json:"hits"`
}

type SecurityIPBan struct {
	ID          string `db:"id" json:"id"`
	CIDR        string `db:"cidr" json:"cidr"`
	Source      string `db:"source" json:"source"` // manual | auto
	ReasonCode  string `db:"reason_code" json:"reason_code"`
	Reason      string `db:"reason" json:"reason"`
	Note        string `db:"note" json:"note"`
	Occurrences int    `db:"occurrences" json:"occurrences"`
	ExpiresAt   int64  `db:"expires_at" json:"expires_at"` // 0 = permanent
	CreatedAt   int64  `db:"created_at" json:"created_at"`
	UpdatedAt   int64  `db:"updated_at" json:"updated_at"`
}

type SecurityGeoRule struct {
	ID        string `db:"id" json:"id"`
	Name      string `db:"name" json:"name"`
	IsEnabled bool   `db:"is_enabled" json:"is_enabled"`
	Action    string `db:"action" json:"action"`   // allow | deny
	OnMiss    string `db:"on_miss" json:"on_miss"` // next | allow | deny
	Countries string `db:"countries" json:"countries"`
	Regions   string `db:"regions" json:"regions"`
	SortOrder int    `db:"sort_order" json:"sort_order"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
	UpdatedAt int64  `db:"updated_at" json:"updated_at"`
}

type AuditLog struct {
	ID           string `db:"id" json:"id"`
	ActorID      string `db:"actor_id" json:"actor_id"`
	ActorName    string `db:"actor_name" json:"actor_name"`
	Action       string `db:"action" json:"action"`
	ResourceType string `db:"resource_type" json:"resource_type"`
	ResourceID   string `db:"resource_id" json:"resource_id"`
	ClientIP     string `db:"client_ip" json:"client_ip"`
	DetailJSON   string `db:"detail_json" json:"detail_json"`
	CreatedAt    int64  `db:"created_at" json:"created_at"`
}

type PluginRecord struct {
	ID               string `db:"id" json:"id"`
	Kind             string `db:"kind" json:"kind"` // builtin | external
	Version          string `db:"version" json:"version"`
	IsEnabled        bool   `db:"is_enabled" json:"is_enabled"`
	ConfigJSON       string `db:"config_json" json:"config_json"`
	SecretCiphertext string `db:"secret_ciphertext" json:"secret_ciphertext,omitempty"`
	CircuitState     string `db:"circuit_state" json:"circuit_state"` // closed | open
	FailureCount     int    `db:"failure_count" json:"failure_count"`
	LastError        string `db:"last_error" json:"last_error"`
	InstalledAt      int64  `db:"installed_at" json:"installed_at"`
	UpdatedAt        int64  `db:"updated_at" json:"updated_at"`
}

type PluginKV struct {
	PluginID  string `db:"plugin_id" json:"plugin_id"`
	Key       string `db:"k" json:"k"`
	ValueText string `db:"value_text" json:"value_text"`
	ValueInt  int64  `db:"value_int" json:"value_int"`
	ExpiresAt int64  `db:"expires_at" json:"expires_at"` // 0 = no expiry
	UpdatedAt int64  `db:"updated_at" json:"updated_at"`
}
