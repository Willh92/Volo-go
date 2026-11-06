package notify

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

// Dispatcher coordinates SMTP email notifications, multi-platform Webhooks, and NotifierProvider plugins.
type Dispatcher struct {
	store     *sqlx.Store
	keys      *config.KeyRing
	settings  *repo.SettingsManager
	catalog   *i18n.Catalog
	plugins   *plugin.Manager
	mailer    *Mailer
	webhooks  *WebhookSender
	publicURL string
}

// DispatcherOptions configures a unified Notification Dispatcher.
type DispatcherOptions struct {
	Store          *sqlx.Store
	Keys           *config.KeyRing
	Settings       *repo.SettingsManager
	Catalog        *i18n.Catalog
	Plugins        *plugin.Manager
	OutboundDialer *security.OutboundSafeDialer
	PublicURL      string
}

// NewDispatcher constructs a notification Dispatcher.
func NewDispatcher(opts DispatcherOptions) *Dispatcher {
	return &Dispatcher{
		store:     opts.Store,
		keys:      opts.Keys,
		settings:  opts.Settings,
		catalog:   opts.Catalog,
		plugins:   opts.Plugins,
		mailer:    NewMailer(),
		webhooks:  NewWebhookSender(opts.OutboundDialer),
		publicURL: strings.TrimRight(strings.TrimSpace(opts.PublicURL), "/"),
	}
}

func (d *Dispatcher) Mailer() *Mailer           { return d.mailer }
func (d *Dispatcher) Webhooks() *WebhookSender  { return d.webhooks }

// SendVerificationCode emails a 6-digit reader activation code via the configured SMTP channel.
func (d *Dispatcher) SendVerificationCode(ctx context.Context, toEmail string, code string, locale string) error {
	snap := d.settings.Snapshot()
	sec, err := d.settings.GetNotificationsSecrets(ctx)
	if err != nil {
		return err
	}
	vars := map[string]any{
		"site": snap.Site.Title,
		"code": code,
	}
	subj := d.catalog.T(locale, "notify.verify_code.subject", vars)
	body := d.catalog.T(locale, "notify.verify_code.body", vars)
	return d.mailer.Send(ctx, snap.Notifications, sec.SMTPPassword, MailMessage{
		To:       toEmail,
		Subject:  subj,
		TextBody: body,
	})
}

// SendTestSMTP sends a live connectivity verification email using either saved settings or transient overrides.
func (d *Dispatcher) SendTestSMTP(ctx context.Context, cfg domain.NotificationsSettings, overridePassword string, toEmail string, locale string) error {
	pwd := overridePassword
	if pwd == "" {
		sec, _ := d.settings.GetNotificationsSecrets(ctx)
		pwd = sec.SMTPPassword
	}
	cfg.SMTPEnabled = true
	vars := map[string]any{"site": d.settings.Snapshot().Site.Title}
	return d.mailer.Send(ctx, cfg, pwd, MailMessage{
		To:       toEmail,
		Subject:  d.catalog.T(locale, "notify.test_smtp.subject", vars),
		TextBody: d.catalog.T(locale, "notify.test_smtp.body", vars),
	})
}

// TriggerTestWebhook dispatches a live connectivity test event to a Webhook and updates its status in DB.
func (d *Dispatcher) TriggerTestWebhook(ctx context.Context, wh domain.Webhook, locale string) (int, error) {
	plainSecret := ""
	if wh.SecretCiphertext != "" && d.keys != nil {
		plainSecret, _ = d.keys.OpenString(wh.SecretCiphertext, "webhook:"+wh.ID)
	}
	ev := WebhookEvent{
		Event:     "webhook.test",
		SiteTitle: d.settings.Snapshot().Site.Title,
		Title:     d.catalog.T(locale, "notify.webhook.test_title"),
		Body:      "Volo-go notification webhook is online.",
		TargetURL: d.publicURL,
		Timestamp: time.Now().Unix(),
	}
	code, err := d.webhooks.Deliver(ctx, wh, plainSecret, ev)
	d.recordWebhookOutcome(ctx, wh.ID, code, err)
	return code, err
}

// NotifyCommentCreated handles asynchronous email + webhook + plugin notifications on new comments.
// SECURITY INVARIANTS:
// - `comments.notify_email_ciphertext` is decrypted ONLY in memory for reply delivery and NEVER logged or returned!
// - Pending/rejected spam comments do NOT trigger reply emails to third parties!
func (d *Dispatcher) NotifyCommentCreated(ctx context.Context, view content.PublicCommentView, in content.CreateCommentInput) {
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d.dispatchCommentNotifications(bgCtx, view, in)
	}()
}

func (d *Dispatcher) dispatchCommentNotifications(ctx context.Context, view content.PublicCommentView, in content.CreateCommentInput) {
	snap := d.settings.Snapshot()
	title, targetPath := d.resolveTargetTitleAndPath(ctx, in.TargetType, in.TargetID)
	fullURL := d.publicURL + targetPath + "#comment-" + view.ID

	// 1. Dispatch Webhooks & Plugin Notifiers
	d.BroadcastWebhookEvent(ctx, WebhookEvent{
		Event:     "comment.created",
		SiteTitle: snap.Site.Title,
		Title: d.catalog.T(snap.Site.DefaultLocale, "notify.new_comment.subject", map[string]any{
			"site":  snap.Site.Title,
			"title": title,
		}),
		Body:      view.AuthorName + ": " + truncatePlain(in.ContentMD, 200),
		TargetURL: fullURL,
		Timestamp: time.Now().Unix(),
		Extra: map[string]any{
			"comment_id":  view.ID,
			"target_type": in.TargetType,
			"target_id":   in.TargetID,
			"status":      view.Status,
		},
	})

	if !snap.Notifications.SMTPEnabled {
		return
	}
	sec, err := d.settings.GetNotificationsSecrets(ctx)
	if err != nil {
		return
	}

	// 2. Notify site admin on new comment if enabled
	if snap.Notifications.NotifyAdminOnComment {
		adminEmail := d.lookupPrimaryAdminEmail(ctx)
		if adminEmail != "" {
			vars := map[string]any{
				"site":    snap.Site.Title,
				"title":   title,
				"author":  view.AuthorName,
				"content": truncatePlain(in.ContentMD, 400),
				"url":     fullURL,
			}
			_ = d.mailer.Send(ctx, snap.Notifications, sec.SMTPPassword, MailMessage{
				To:       adminEmail,
				Subject:  d.catalog.T(snap.Site.DefaultLocale, "notify.new_comment.subject", vars),
				TextBody: d.catalog.T(snap.Site.DefaultLocale, "notify.new_comment.body", vars),
			})
		}
	}

	// 3. Notify parent commenter if comment is approved and parent opted into encrypted email alerts
	if snap.Notifications.NotifyReplyByEmail && view.Status == domain.CommentStatusApproved && in.ParentID != "" {
		parent, pErr := sqlx.Get[domain.Comment](ctx, d.store,
			`SELECT id, user_id, notify_email_ciphertext, notify_locale, is_deleted FROM comments WHERE id = ?`, in.ParentID)
		if pErr == nil && !parent.IsDeleted {
			recipientEmail := d.decryptCommentNotifyEmail(ctx, parent)
			if recipientEmail != "" {
				loc := parent.NotifyLocale
				if loc == "" {
					loc = snap.Site.DefaultLocale
				}
				vars := map[string]any{
					"site":    snap.Site.Title,
					"title":   title,
					"author":  view.AuthorName,
					"content": truncatePlain(in.ContentMD, 400),
					"url":     fullURL,
				}
				_ = d.mailer.Send(ctx, snap.Notifications, sec.SMTPPassword, MailMessage{
					To:       recipientEmail,
					Subject:  d.catalog.T(loc, "notify.reply_comment.subject", vars),
					TextBody: d.catalog.T(loc, "notify.reply_comment.body", vars),
				})
			}
		}
	}
}

// BroadcastArticlePublished dispatches `article.published` to Webhooks with strict zero-leak scrubbing!
func (d *Dispatcher) BroadcastArticlePublished(ctx context.Context, raw domain.RawArticle, safeSummary string) {
	snap := d.settings.Snapshot()
	body := safeSummary
	if strings.TrimSpace(raw.PasswordCiphertext) != "" {
		body = d.catalog.T(snap.Site.DefaultLocale, "article.protected_excerpt")
	} else if body == "" {
		body = truncatePlain(plugin.StripLockedMarkdownBlocks(raw.ContentMD), 180)
	}
	fullURL := d.publicURL + visibility.ArticlePath(raw.Slug)

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		d.BroadcastWebhookEvent(bgCtx, WebhookEvent{
			Event:     "article.published",
			SiteTitle: snap.Site.Title,
			Title:     raw.Title,
			Body:      body,
			TargetURL: fullURL,
			Timestamp: time.Now().Unix(),
			Extra: map[string]any{
				"article_id": raw.ID,
				"slug":       raw.Slug,
			},
		})
	}()
}

// BroadcastWebhookEvent delivers ev to all enabled Webhooks subscribed to ev.Event (plus NotifierProvider plugins).
func (d *Dispatcher) BroadcastWebhookEvent(ctx context.Context, ev WebhookEvent) {
	if d.store == nil {
		return
	}
	if d.plugins != nil {
		for _, np := range d.plugins.Providers().Notifiers() {
			_ = np.Send(ctx, plugin.NotificationEvent{
				Event:   ev.Event,
				Locale:  d.settings.Snapshot().Site.DefaultLocale,
				Title:   ev.Title,
				Summary: ev.Body,
				URL:     ev.TargetURL,
			})
		}
	}

	hooks, err := sqlx.Select[domain.Webhook](ctx, d.store, `SELECT * FROM webhooks WHERE is_enabled = 1`)
	if err != nil {
		return
	}
	for _, wh := range hooks {
		if !webhookSubscribes(wh.EventsJSON, ev.Event) {
			continue
		}
		plainSecret := ""
		if wh.SecretCiphertext != "" && d.keys != nil {
			plainSecret, _ = d.keys.OpenString(wh.SecretCiphertext, "webhook:"+wh.ID)
		}
		code, dErr := d.webhooks.Deliver(ctx, wh, plainSecret, ev)
		d.recordWebhookOutcome(ctx, wh.ID, code, dErr)
	}
}

func (d *Dispatcher) decryptCommentNotifyEmail(ctx context.Context, c domain.Comment) string {
	if c.NotifyEmailCiphertext != "" && d.keys != nil {
		if plain, err := d.keys.OpenString(c.NotifyEmailCiphertext, "comment:email"); err == nil && strings.Contains(plain, "@") {
			return strings.TrimSpace(plain)
		}
	}
	if c.UserID != "" {
		if u, err := sqlx.Get[domain.User](ctx, d.store, `SELECT email FROM users WHERE id = ? AND status = ?`, c.UserID, domain.UserStatusActive); err == nil {
			return strings.TrimSpace(u.Email)
		}
	}
	return ""
}

func (d *Dispatcher) lookupPrimaryAdminEmail(ctx context.Context) string {
	u, err := sqlx.Get[domain.User](ctx, d.store,
		`SELECT email FROM users WHERE role = ? AND status = ? ORDER BY created_at ASC LIMIT 1`,
		domain.RoleAdmin, domain.UserStatusActive)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Email)
}

func (d *Dispatcher) resolveTargetTitleAndPath(ctx context.Context, targetType, targetID string) (string, string) {
	if targetType == domain.TargetTypePage {
		pg, err := sqlx.Get[domain.Page](ctx, d.store, `SELECT title, slug FROM pages WHERE id = ?`, targetID)
		if err == nil {
			return pg.Title, "/" + pg.Slug
		}
		return "Page", "/"
	}
	art, err := sqlx.Get[domain.RawArticle](ctx, d.store, `SELECT title, slug FROM articles WHERE id = ?`, targetID)
	if err == nil {
		return art.Title, visibility.ArticlePath(art.Slug)
	}
	return "Post", "/"
}

func (d *Dispatcher) recordWebhookOutcome(ctx context.Context, webhookID string, code int, err error) {
	if d.store == nil || webhookID == "" {
		return
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
		if len(errMsg) > 400 {
			errMsg = errMsg[:400]
		}
	}
	now := time.Now().UnixMilli()
	_, _ = d.store.ExecContext(ctx,
		`UPDATE webhooks SET last_status_code = ?, last_error = ?, last_triggered_at = ?, updated_at = ? WHERE id = ?`,
		code, errMsg, now, now, webhookID)
}

func webhookSubscribes(eventsJSON string, event string) bool {
	if strings.TrimSpace(eventsJSON) == "" {
		return true
	}
	var list []string
	if json.Unmarshal([]byte(eventsJSON), &list) != nil {
		return false
	}
	for _, item := range list {
		if item == "*" || item == event {
			return true
		}
	}
	return false
}

func truncatePlain(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
