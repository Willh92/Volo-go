package domain

import "strings"

// SocialLink represents an item in the dynamic social profile links list (replacing legacy fixed 21 usite* fields).
type SocialLink struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
	URL   string `json:"url"`
}

// SiteSettings ("site" domain).
type SiteSettings struct {
	Initialized         bool              `json:"initialized"`
	Title               string            `json:"title"`
	Subtitle            string            `json:"subtitle"`
	Description         string            `json:"description"`
	LogoURL             string            `json:"logo_url"`
	FaviconURL          string            `json:"favicon_url"`
	DefaultLocale       string            `json:"default_locale"`
	AllowLocaleSwitch   bool              `json:"allow_locale_switch"`
	TimeZone            string            `json:"timezone"`
	NoticeBoard         string            `json:"notice_board"`
	SocialLinks         []SocialLink      `json:"social_links"`
	LicenseTemplate     string            `json:"license_template"`
	EnableLicenseNotice bool              `json:"enable_license_notice"`
	FooterText          string            `json:"footer_text"`
	EnableFooterHTML    bool              `json:"enable_footer_html"`
	CustomHeadMeta      string            `json:"custom_head_meta"`
	CustomVars          map[string]string `json:"custom_vars"`
	ShowAdminLoginEntry bool              `json:"show_admin_login_entry"`
}

// ReadingSettings ("reading" domain).
type ReadingSettings struct {
	ListStyle             string   `json:"list_style"` // summary | title_only | full
	PostsPerPage          int      `json:"posts_per_page"`
	RecentPostsCount      int      `json:"recent_posts_count"`
	RecentCommentsCount   int      `json:"recent_comments_count"`
	PopularTagsCount      int      `json:"popular_tags_count"`
	RelatedPostsCount     int      `json:"related_posts_count"`
	EnableReadingStats    bool     `json:"enable_reading_stats"`
	EnableOutdatedNotice  bool     `json:"enable_outdated_notice"`
	OutdatedNoticeDays    int      `json:"outdated_notice_days"` // 0 = disabled
	EnableInstantSearch   bool     `json:"enable_instant_search"`
	ShowCodeLineNumbers   bool     `json:"show_code_line_numbers"`
	AllowReaderRegister   bool     `json:"allow_reader_register"`
	CommentAccessMode     string   `json:"comment_access_mode"`     // all | registered | off
	CommentModerationMode string   `json:"comment_moderation_mode"` // off | smart | all
	BuiltinSpamFilter     bool     `json:"builtin_spam_filter"`
	BlockedKeywords       []string `json:"blocked_keywords"`
	AllowedKeywords       []string `json:"allowed_keywords"`
	PoWDifficultyBits     int      `json:"pow_difficulty_bits"`
	AssetCDNPrefix        string   `json:"asset_cdn_prefix"`
}

// PodcastChannelConfig holds full Apple/Spotify iTunes Podcast metadata inside "feed" domain.
type PodcastChannelConfig struct {
	Enabled       bool   `json:"enabled"`
	Title         string `json:"title"`
	Subtitle      string `json:"subtitle"`
	Summary       string `json:"summary"`
	Author        string `json:"author"`
	OwnerName     string `json:"owner_name"`
	OwnerEmail    string `json:"owner_email"`
	CoverImageURL string `json:"cover_image_url"`
	Language      string `json:"language"`
	Category      string `json:"category"`
	SubCategory   string `json:"sub_category"`
	Explicit      bool   `json:"explicit"`
	Copyright     string `json:"copyright"`
}

// FeedSettings ("feed" domain).
type FeedSettings struct {
	OutputMode            string               `json:"output_mode"` // summary | full
	ItemCount             int                  `json:"item_count"`
	EnableLLMSTxt         bool                 `json:"enable_llms_txt"`
	EnableRawMarkdown     bool                 `json:"enable_raw_markdown"`
	Podcast               PodcastChannelConfig `json:"podcast"`
}

// GitHubRepoItem represents a cached public GitHub repository for showcase & /github-stats/{user}.svg.
type GitHubRepoItem struct {
	Name        string `json:"name"`
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	HTMLURL     string `json:"html_url"`
	Homepage    string `json:"homepage"`
	Language    string `json:"language"`
	Stars       int    `json:"stars"`
	Forks       int    `json:"forks"`
	IsFork      bool   `json:"is_fork"`
	Hidden      bool   `json:"hidden"`
	UpdatedAt   string `json:"updated_at"`
}

// SEOSettings ("seo" domain).
type SEOSettings struct {
	MetaKeywords       string           `json:"meta_keywords"`
	MetaDescription    string           `json:"meta_description"`
	DefaultOGImageURL  string           `json:"default_og_image_url"`
	PostPermalinkStyle string           `json:"post_permalink_style"` // /posts/{slug}
	IndexNowEnabled    bool             `json:"indexnow_enabled"`
	IndexNowKey        string           `json:"indexnow_key"`
	GitHubUsername     string           `json:"github_username"`
	GitHubIncludeForks bool             `json:"github_include_forks"`
	GitHubRepos        []GitHubRepoItem `json:"github_repos"`
	GitHubSyncedAt     int64            `json:"github_synced_at"`
}

// StorageSettings ("storage" domain). Secrets are stored exclusively in SecretCiphertext!
type StorageSettings struct {
	ActiveDriver       string `json:"active_driver"` // local | s3
	MaxUploadMB        int    `json:"max_upload_mb"`
	StripEXIF          bool   `json:"strip_exif"`
	AdaptiveWebP       bool   `json:"adaptive_webp"`
	ImageQuality       int    `json:"image_quality"`
	S3Endpoint         string `json:"s3_endpoint"`
	S3Region           string `json:"s3_region"`
	S3Bucket           string `json:"s3_bucket"`
	S3AccessKey        string `json:"s3_access_key"`
	S3PublicBaseURL    string `json:"s3_public_base_url"`
	S3KeyPrefix        string `json:"s3_key_prefix"`
	S3PathStyle        bool   `json:"s3_path_style"`
	S3SecretKeySet     bool   `json:"s3_secret_key_set"`
}

// StorageSecrets is encrypted via AES-256-GCM into settings.secret_ciphertext for domain="storage".
type StorageSecrets struct {
	S3SecretKey string `json:"s3_secret_key"`
}

// NotificationsSettings ("notifications" domain). Secrets are stored in SecretCiphertext!
type NotificationsSettings struct {
	SMTPEnabled          bool   `json:"smtp_enabled"`
	SMTPHost             string `json:"smtp_host"`
	SMTPPort             int    `json:"smtp_port"`
	SMTPSecurity         string `json:"smtp_security"` // tls | starttls | plain_internal
	SMTPAuth             bool   `json:"smtp_auth"`
	SMTPUsername         string `json:"smtp_username"`
	SMTPFromEmail        string `json:"smtp_from_email"`
	SMTPFromName         string `json:"smtp_from_name"`
	SMTPPasswordSet      bool   `json:"smtp_password_set"`
	NotifyAdminOnComment bool   `json:"notify_admin_on_comment"`
	NotifyReplyByEmail   bool   `json:"notify_reply_by_email"`
}

// NotificationsSecrets is encrypted via AES-256-GCM into settings.secret_ciphertext for domain="notifications".
type NotificationsSecrets struct {
	SMTPPassword string `json:"smtp_password"`
}

// SecuritySettings ("security" domain).
type SecuritySettings struct {
	RiskControlEnabled   bool   `json:"risk_control_enabled"`
	WAFEnabled           bool   `json:"waf_enabled"`
	WAFWindowSeconds     int    `json:"waf_window_seconds"`
	WAFMaxRequests       int    `json:"waf_max_requests"`
	GeoPolicyActive      bool   `json:"geo_policy_active"`
	GeoDefaultAction     string `json:"geo_default_action"` // allow | deny
	GeoUnknownAction     string `json:"geo_unknown_action"` // allow | deny
	TOTPRequiredScope    string `json:"totp_required_scope"` // off | admin | staff
	TOTPRememberDays     int    `json:"totp_remember_days"`
	TOTPWindowSteps      int    `json:"totp_window_steps"`
	DynamicLoginPath     string `json:"dynamic_login_path"`
}

// SecuritySecrets is encrypted via AES-256-GCM into settings.secret_ciphertext for domain="security".
type SecuritySecrets struct {
	ShareTokenSecret string `json:"share_token_secret"`
}

// AppearanceSettings ("appearance" domain).
type AppearanceSettings struct {
	ActiveTheme  string                    `json:"active_theme"`            // volo-studio or external theme id
	MobileTheme  string                    `json:"mobile_theme,omitempty"`  // optional separate theme id for mobile devices
	Accent       string                    `json:"accent"`                  // forest | ocean | teal | amber | plum | rose
	DefaultMode  string                    `json:"default_mode"`            // auto | system | light | dark
	CustomCSS    string                    `json:"custom_css,omitempty"`    // optional site-wide custom CSS
	ThemeConfigs map[string]map[string]any `json:"theme_configs,omitempty"` // per-theme declarative config values keyed by theme_id
}

// BackupSettings ("backup" domain).
type BackupSettings struct {
	LocalAutoEnabled  bool   `json:"local_auto_enabled"`
	LocalIntervalHrs  int    `json:"local_interval_hours"`
	LocalKeepCount    int    `json:"local_keep_count"`
	IncludeMediaInAuto bool  `json:"include_media_in_auto"`
	RemoteEnabled     bool   `json:"remote_enabled"`
	RemoteProtocol    string `json:"remote_protocol"` // s3 | webdav
	WebDAVURL         string `json:"webdav_url"`
	WebDAVUser        string `json:"webdav_user"`
	WebDAVDir         string `json:"webdav_dir"`
	WebDAVPasswordSet bool   `json:"webdav_password_set"`
	S3Endpoint        string `json:"s3_endpoint"`
	S3Region          string `json:"s3_region"`
	S3Bucket          string `json:"s3_bucket"`
	S3AccessKey       string `json:"s3_access_key"`
	S3Prefix          string `json:"s3_prefix"`
	S3PathStyle       bool   `json:"s3_path_style"`
	S3SecretKeySet    bool   `json:"s3_secret_key_set"`
}

// BackupSecrets is encrypted via AES-256-GCM into settings.secret_ciphertext for domain="backup".
type BackupSecrets struct {
	WebDAVPassword string `json:"webdav_password"`
	S3SecretKey    string `json:"s3_secret_key"`
}

// SettingsSnapshot is an immutable hot-reloaded in-memory snapshot of all domain settings.
type SettingsSnapshot struct {
	Site          SiteSettings          `json:"site"`
	Reading       ReadingSettings       `json:"reading"`
	Feed          FeedSettings          `json:"feed"`
	SEO           SEOSettings           `json:"seo"`
	Storage       StorageSettings       `json:"storage"`
	Notifications NotificationsSettings `json:"notifications"`
	Security      SecuritySettings      `json:"security"`
	Appearance    AppearanceSettings    `json:"appearance"`
	Backup        BackupSettings        `json:"backup"`
}

// DefaultSettingsSnapshot returns safe, privacy-first defaults out-of-the-box.
func DefaultSettingsSnapshot(defaultLocale string) SettingsSnapshot {
	if defaultLocale == "" {
		defaultLocale = "zh-CN"
	}
	isZH := strings.HasPrefix(strings.ToLower(strings.TrimSpace(defaultLocale)), "zh")
	defaultSubtitle := "Independent thoughts & craft"
	defaultDesc := "A fast, sovereign personal blog powered by Volo-go."
	if isZH {
		defaultSubtitle = "记录 · 思考 · 分享"
		defaultDesc = "由 Volo-go 驱动的独立个人博客。"
	}
	return SettingsSnapshot{
		Site: SiteSettings{
			Initialized:         false,
			Title:               "Volo Blog",
			Subtitle:            defaultSubtitle,
			Description:         defaultDesc,
			LogoURL:             "",
			FaviconURL:          "/favicon.svg",
			DefaultLocale:       defaultLocale,
			AllowLocaleSwitch:   true,
			TimeZone:            "Asia/Shanghai",
			SocialLinks:         []SocialLink{},
			LicenseTemplate:     "本文由 {author} 原创发布于 {site}，转载请注明原文链接：{url}",
			EnableLicenseNotice: true,
			EnableFooterHTML:    true,
			CustomVars:          map[string]string{},
			ShowAdminLoginEntry: false,
		},
		Reading: ReadingSettings{
			ListStyle:             "summary",
			PostsPerPage:          15,
			RecentPostsCount:      8,
			RecentCommentsCount:   8,
			PopularTagsCount:      20,
			RelatedPostsCount:     5,
			EnableReadingStats:    true,
			EnableOutdatedNotice:  true,
			OutdatedNoticeDays:    365,
			EnableInstantSearch:   true,
			ShowCodeLineNumbers:   false,
			AllowReaderRegister:   false,
			CommentAccessMode:     CommentAccessAll,
			CommentModerationMode: CommentModerationSmart,
			BuiltinSpamFilter:     true,
			BlockedKeywords:       []string{},
			AllowedKeywords:       []string{},
			PoWDifficultyBits:     16,
		},
		Feed: FeedSettings{
			OutputMode:        "summary",
			ItemCount:         20,
			EnableLLMSTxt:     true,
			EnableRawMarkdown: true,
			Podcast: PodcastChannelConfig{
				Enabled:  true,
				Language: defaultLocale,
				Category: "Technology",
				Explicit: false,
			},
		},
		SEO: SEOSettings{
			MetaKeywords:       "Volo,Go,Blog",
			MetaDescription:    "Personal independent blog",
			PostPermalinkStyle: "/posts/{slug}",
			IndexNowEnabled:    false,
			GitHubIncludeForks: false,
			GitHubRepos:        []GitHubRepoItem{},
		},
		Storage: StorageSettings{
			ActiveDriver: "local",
			MaxUploadMB:  50,
			StripEXIF:    true,
			AdaptiveWebP: true,
			ImageQuality: 85,
			S3Region:     "auto",
		},
		Notifications: NotificationsSettings{
			SMTPEnabled:          false,
			SMTPPort:             465,
			SMTPSecurity:         "tls",
			SMTPAuth:             true,
			NotifyAdminOnComment: true,
			NotifyReplyByEmail:   true,
		},
		Security: SecuritySettings{
			RiskControlEnabled: true,
			WAFEnabled:         true,
			WAFWindowSeconds:   180,
			WAFMaxRequests:     180,
			GeoPolicyActive:    false,
			GeoDefaultAction:   "allow",
			GeoUnknownAction:   "allow",
			TOTPRequiredScope:  "off",
			TOTPRememberDays:   14,
			TOTPWindowSteps:    1,
		},
		Appearance: AppearanceSettings{
			ActiveTheme:  "volo-studio",
			Accent:       "forest",
			DefaultMode:  "auto",
			ThemeConfigs: map[string]map[string]any{},
		},
		Backup: BackupSettings{
			LocalAutoEnabled:   false,
			LocalIntervalHrs:   24,
			LocalKeepCount:     7,
			IncludeMediaInAuto: false,
			RemoteEnabled:      false,
			RemoteProtocol:     "s3",
			WebDAVDir:          "volo-backups",
			S3Region:           "auto",
			S3Prefix:           "volo-backups",
		},
	}
}
