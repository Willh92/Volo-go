package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

const (
	maxAsyncNotifyWorkers     = 8
	asyncNotifyQueueSize      = 64
	maxWebhookLastErrLen      = 240
	maxWebhookPayloadBytes    = 4096
	maxWebhookRespBytes       = 2048
	maxWebhookDeliveryHistory = 100
)

type asyncNotifyJob struct {
	timeout time.Duration
	fn      func(context.Context)
}

// Dispatcher coordinates SMTP email notifications, multi-platform Webhooks, and NotifierProvider plugins.
type Dispatcher struct {
	store    *sqlx.Store
	keys     *config.KeyRing
	settings *repo.SettingsManager
	catalog  *i18n.Catalog
	plugins  *plugin.Manager
	mailer   *Mailer
	webhooks *WebhookSender

	mu        sync.RWMutex
	publicURL string
	closed    bool

	queue    chan asyncNotifyJob
	wg       sync.WaitGroup
	bgCtx    context.Context
	bgCancel context.CancelFunc
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
	bgCtx, bgCancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		store:     opts.Store,
		keys:      opts.Keys,
		settings:  opts.Settings,
		catalog:   opts.Catalog,
		plugins:   opts.Plugins,
		mailer:    NewMailer(),
		webhooks:  NewWebhookSender(opts.OutboundDialer),
		publicURL: strings.TrimRight(strings.TrimSpace(opts.PublicURL), "/"),
		queue:     make(chan asyncNotifyJob, asyncNotifyQueueSize),
		bgCtx:     bgCtx,
		bgCancel:  bgCancel,
	}
	for i := 0; i < maxAsyncNotifyWorkers; i++ {
		d.wg.Add(1)
		go d.runAsyncWorker()
	}
	return d
}

func (d *Dispatcher) runAsyncWorker() {
	defer d.wg.Done()
	for job := range d.queue {
		if d.bgCtx.Err() != nil || job.fn == nil {
			continue
		}
		runCtx, cancel := context.WithTimeout(d.bgCtx, job.timeout)
		job.fn(runCtx)
		cancel()
	}
}

func (d *Dispatcher) Mailer() *Mailer          { return d.mailer }
func (d *Dispatcher) Webhooks() *WebhookSender { return d.webhooks }

// SetPublicURL updates the canonical site base URL used in notification links dynamically.
func (d *Dispatcher) SetPublicURL(u string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.publicURL = strings.TrimRight(strings.TrimSpace(u), "/")
	d.mu.Unlock()
}

func (d *Dispatcher) getPublicURL() string {
	if d == nil {
		return ""
	}
	d.mu.RLock()
	u := d.publicURL
	d.mu.RUnlock()
	if u != "" || d.settings == nil {
		return u
	}
	snap := d.settings.Snapshot()
	if len(snap.Site.CustomVars) > 0 {
		for _, key := range []string{"public_url", "site_url", "canonical_url"} {
			if cand := strings.TrimRight(strings.TrimSpace(snap.Site.CustomVars[key]), "/"); cand != "" {
				return cand
			}
		}
	}
	return ""
}

// Close stops accepting new background notifications and waits for in-flight deliveries to drain
// (or aborts them if ctx expires) so callers can safely close the database store afterward.
func (d *Dispatcher) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		if d.bgCancel != nil {
			d.bgCancel()
		}
		return nil
	case <-ctx.Done():
		if d.bgCancel != nil {
			d.bgCancel()
		}
		return ctx.Err()
	}
}

func (d *Dispatcher) enqueueAsync(timeout time.Duration, fn func(context.Context)) {
	if d == nil || fn == nil {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return
	}
	select {
	case d.queue <- asyncNotifyJob{timeout: timeout, fn: fn}:
	default:
		slog.Warn("notify async worker queue saturated; dropping background notification",
			"component", "notify", "queue_cap", asyncNotifyQueueSize)
	}
}

func (d *Dispatcher) decryptWebhookSecret(wh domain.Webhook) (string, error) {
	if strings.TrimSpace(wh.SecretCiphertext) == "" {
		return "", nil
	}
	if d.keys == nil {
		return "", errors.New("notify: keyring unavailable to decrypt webhook secret")
	}
	return d.keys.OpenStringFallback(
		wh.SecretCiphertext,
		config.WebhookSecretAAD(wh.ID),
		"webhooks:secret:"+wh.ID,
	)
}

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
	ev := WebhookEvent{
		Event:     "webhook.test",
		SiteTitle: d.siteTitleFallback(),
		Title:     d.catalogTextFallback(locale, "notify.webhook.test_title", "Webhook Connectivity Test"),
		Body:      "Volo-go notification webhook is online.",
		TargetURL: d.getPublicURL(),
		Timestamp: time.Now().Unix(),
	}
	plainSecret, decErr := d.decryptWebhookSecret(wh)
	if decErr != nil {
		slog.Error("webhook secret decryption failed; refusing unsigned test webhook delivery",
			"component", "notify", "webhook_id", wh.ID, "err", decErr)
		d.recordWebhookDelivery(ctx, wh.ID, ev.Event, WebhookDeliveryOutcome{}, decErr)
		return 0, decErr
	}
	out, err := d.webhooks.DeliverDetailed(ctx, wh, plainSecret, ev)
	d.recordWebhookDelivery(ctx, wh.ID, ev.Event, out, err)
	return out.StatusCode, err
}

// TriggerTestNotifiers dispatches a connectivity test event to all registered plugin NotifierProviders.
func (d *Dispatcher) TriggerTestNotifiers(ctx context.Context, locale string) (int, error) {
	if d == nil || d.plugins == nil {
		return 0, nil
	}
	notifiers := d.plugins.Providers().Notifiers()
	if len(notifiers) == 0 {
		return 0, nil
	}
	siteTitle := d.siteTitleFallback()
	if strings.TrimSpace(locale) == "" && d.settings != nil {
		locale = d.settings.Snapshot().Site.DefaultLocale
	}
	if strings.TrimSpace(locale) == "" {
		locale = "zh-CN"
	}
	ev := plugin.NotificationEvent{
		Event:   "webhook.test",
		Locale:  locale,
		Title:   d.catalogTextFallback(locale, "notify.webhook.test_title", "Webhook Connectivity Test"),
		Summary: "Volo-go notification provider is online.",
		URL:     d.getPublicURL(),
		Meta: map[string]string{
			"site_title": siteTitle,
		},
	}
	var errs []error
	sent := 0
	for _, np := range notifiers {
		if err := np.Send(ctx, ev); err != nil {
			errs = append(errs, err)
			continue
		}
		sent++
	}
	return sent, errors.Join(errs...)
}

func (d *Dispatcher) siteTitleFallback() string {
	if d != nil && d.settings != nil {
		if t := strings.TrimSpace(d.settings.Snapshot().Site.Title); t != "" {
			return t
		}
	}
	return "Volo"
}

func (d *Dispatcher) catalogTextFallback(locale, key, fallback string) string {
	if d != nil && d.catalog != nil {
		if v := strings.TrimSpace(d.catalog.T(locale, key)); v != "" && v != key {
			return v
		}
	}
	return fallback
}

// NotifyCommentCreated handles asynchronous email + webhook + plugin notifications on new comments.
// SECURITY INVARIANTS:
// - `comments.notify_email_ciphertext` is decrypted ONLY in memory for reply delivery and NEVER logged or returned!
// - Pending/rejected spam comments do NOT trigger reply emails to third parties!
func (d *Dispatcher) NotifyCommentCreated(_ context.Context, view content.PublicCommentView, in content.CreateCommentInput) {
	d.enqueueAsync(15*time.Second, func(bgCtx context.Context) {
		d.dispatchCommentNotifications(bgCtx, view, in)
	})
}

func (d *Dispatcher) dispatchCommentNotifications(ctx context.Context, view content.PublicCommentView, in content.CreateCommentInput) {
	snap := d.settings.Snapshot()
	title, targetPath := d.resolveTargetTitleAndPath(ctx, in.TargetType, in.TargetID)
	fullURL := d.getPublicURL() + targetPath + "#comment-" + view.ID

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
		var parent domain.Comment
		pErr := sqlx.GetInto(ctx, d.store, &parent,
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

// NotifyCommentReplyApproved asynchronously notifies the parent commenter when a pending reply comment
// is moderated to approved, matching step 3 of dispatchCommentNotifications.
func (d *Dispatcher) NotifyCommentReplyApproved(_ context.Context, cmt domain.Comment) {
	if d == nil || d.settings == nil || d.store == nil || d.mailer == nil || strings.TrimSpace(cmt.ParentID) == "" {
		return
	}
	d.enqueueAsync(15*time.Second, func(bgCtx context.Context) {
		snap := d.settings.Snapshot()
		if !snap.Notifications.SMTPEnabled || !snap.Notifications.NotifyReplyByEmail || cmt.ParentID == "" {
			return
		}
		sec, err := d.settings.GetNotificationsSecrets(bgCtx)
		if err != nil {
			return
		}
		var parent domain.Comment
		pErr := sqlx.GetInto(bgCtx, d.store, &parent,
			`SELECT id, user_id, notify_email_ciphertext, notify_locale, is_deleted FROM comments WHERE id = ?`, cmt.ParentID)
		if pErr != nil || parent.IsDeleted {
			return
		}
		recipientEmail := d.decryptCommentNotifyEmail(bgCtx, parent)
		if recipientEmail == "" {
			return
		}
		title, targetPath := d.resolveTargetTitleAndPath(bgCtx, cmt.TargetType, cmt.TargetID)
		fullURL := d.getPublicURL() + targetPath + "#comment-" + cmt.ID
		loc := parent.NotifyLocale
		if loc == "" {
			loc = snap.Site.DefaultLocale
		}
		vars := map[string]any{
			"site":    snap.Site.Title,
			"title":   title,
			"author":  cmt.AuthorName,
			"content": truncatePlain(cmt.ContentMD, 400),
			"url":     fullURL,
		}
		subj := "New reply"
		body := truncatePlain(cmt.ContentMD, 400)
		if d.catalog != nil {
			subj = d.catalog.T(loc, "notify.reply_comment.subject", vars)
			body = d.catalog.T(loc, "notify.reply_comment.body", vars)
		}
		_ = d.mailer.Send(bgCtx, snap.Notifications, sec.SMTPPassword, MailMessage{
			To:       recipientEmail,
			Subject:  subj,
			TextBody: body,
		})
	})
}

// BroadcastArticlePublished dispatches `article.published` to Webhooks with strict zero-leak scrubbing!
func (d *Dispatcher) BroadcastArticlePublished(_ context.Context, raw domain.RawArticle, safeSummary string) {
	snap := d.settings.Snapshot()
	body := safeSummary
	if strings.TrimSpace(raw.PasswordCiphertext) != "" {
		body = d.catalog.T(snap.Site.DefaultLocale, "article.protected_excerpt")
	} else if body == "" {
		body = truncatePlain(plugin.StripLockedMarkdownBlocks(raw.ContentMD), 180)
	}
	pubAt := raw.PublishedAt
	if pubAt <= 0 {
		pubAt = raw.CreatedAt
	}
	fullURL := d.getPublicURL() + visibility.FormatArticlePath(snap.SEO.PostPermalinkStyle, raw.Slug, pubAt, snap.Site.TimeZone)

	d.enqueueAsync(12*time.Second, func(bgCtx context.Context) {
		d.BroadcastWebhookEvent(bgCtx, WebhookEvent{
			Event:     "article.published",
			SiteTitle: snap.Site.Title,
			Body:      body,
			Title:     raw.Title,
			TargetURL: fullURL,
			Timestamp: time.Now().Unix(),
			Extra: map[string]any{
				"article_id": raw.ID,
				"slug":       raw.Slug,
			},
		})
	})
}

// BroadcastWebhookEvent delivers ev to all enabled Webhooks subscribed to ev.Event (plus NotifierProvider plugins).
func (d *Dispatcher) BroadcastWebhookEvent(ctx context.Context, ev WebhookEvent) {
	if d.plugins != nil {
		defaultLoc := "zh-CN"
		if d.settings != nil {
			defaultLoc = d.settings.Snapshot().Site.DefaultLocale
		}
		meta := buildNotificationMeta(ev)
		for _, np := range d.plugins.Providers().Notifiers() {
			_ = np.Send(ctx, plugin.NotificationEvent{
				Event:   ev.Event,
				Locale:  defaultLoc,
				Title:   ev.Title,
				Summary: ev.Body,
				URL:     ev.TargetURL,
				Meta:    meta,
			})
		}
	}
	if d.store == nil {
		return
	}

	var hooks []domain.Webhook
	err := sqlx.SelectInto(ctx, d.store, &hooks, `SELECT * FROM webhooks WHERE is_enabled = ?`, true)
	if err != nil {
		return
	}
	for _, wh := range hooks {
		if !webhookSubscribes(wh.EventsJSON, ev.Event) {
			continue
		}
		plainSecret, decErr := d.decryptWebhookSecret(wh)
		if decErr != nil {
			slog.Error("webhook secret decryption failed; skipping unsigned webhook delivery",
				"component", "notify", "webhook_id", wh.ID, "err", decErr)
			d.recordWebhookDelivery(ctx, wh.ID, ev.Event, WebhookDeliveryOutcome{}, decErr)
			continue
		}
		out, dErr := d.webhooks.DeliverDetailed(ctx, wh, plainSecret, ev)
		d.recordWebhookDelivery(ctx, wh.ID, ev.Event, out, dErr)
	}
}

func buildNotificationMeta(ev WebhookEvent) map[string]string {
	meta := make(map[string]string, len(ev.Extra)+1)
	if strings.TrimSpace(ev.SiteTitle) != "" {
		meta["site_title"] = ev.SiteTitle
	}
	for k, v := range ev.Extra {
		if k == "" || v == nil {
			continue
		}
		meta[k] = fmt.Sprint(v)
	}
	return meta
}

func (d *Dispatcher) decryptCommentNotifyEmail(ctx context.Context, c domain.Comment) string {
	if c.NotifyEmailCiphertext != "" && d.keys != nil {
		plain, err := d.keys.OpenStringFallback(
			c.NotifyEmailCiphertext,
			config.CommentNotifyEmailAAD(c.ID),
			"comments:email:"+c.ID,
			"comment:email",
		)
		if err == nil && strings.Contains(plain, "@") {
			return strings.TrimSpace(plain)
		}
	}
	if c.UserID != "" {
		var u domain.User
		if err := sqlx.GetInto(ctx, d.store, &u, `SELECT email FROM users WHERE id = ? AND status = ?`, c.UserID, domain.UserStatusActive); err == nil {
			return strings.TrimSpace(u.Email)
		}
	}
	return ""
}

func (d *Dispatcher) lookupPrimaryAdminEmail(ctx context.Context) string {
	var u domain.User
	err := sqlx.GetInto(ctx, d.store, &u,
		`SELECT email FROM users WHERE role = ? AND status = ? ORDER BY created_at ASC LIMIT 1`,
		domain.RoleAdmin, domain.UserStatusActive)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Email)
}

func (d *Dispatcher) resolveTargetTitleAndPath(ctx context.Context, targetType, targetID string) (string, string) {
	if targetType == domain.TargetTypePage {
		var pg domain.Page
		err := sqlx.GetInto(ctx, d.store, &pg, `SELECT title, slug FROM pages WHERE id = ?`, targetID)
		if err == nil {
			return pg.Title, "/" + pg.Slug
		}
		return "Page", "/"
	}
	var art domain.RawArticle
	err := sqlx.GetInto(ctx, d.store, &art, `SELECT title, slug, published_at, created_at FROM articles WHERE id = ?`, targetID)
	if err == nil {
		pubAt := art.PublishedAt
		if pubAt <= 0 {
			pubAt = art.CreatedAt
		}
		var style, tz string
		if d.settings != nil {
			snap := d.settings.Snapshot()
			style = snap.SEO.PostPermalinkStyle
			tz = snap.Site.TimeZone
		}
		return art.Title, visibility.FormatArticlePath(style, art.Slug, pubAt, tz)
	}
	return "Post", "/"
}

func (d *Dispatcher) recordWebhookDelivery(ctx context.Context, webhookID string, event string, out WebhookDeliveryOutcome, err error) {
	if d.store == nil || webhookID == "" {
		return
	}
	errMsg := ""
	if err != nil {
		errMsg = truncateUTF8Bytes(err.Error(), maxWebhookLastErrLen)
	}
	if strings.TrimSpace(event) == "" {
		event = "webhook.unknown"
	}
	event = truncateUTF8Bytes(strings.TrimSpace(event), 64)
	payloadJSON := truncateUTF8Bytes(out.PayloadJSON, maxWebhookPayloadBytes)
	respBody := truncateUTF8Bytes(out.ResponseBody, maxWebhookRespBytes)
	durMS := out.DurationMS
	if durMS < 0 {
		durMS = 0
	}
	success := err == nil && out.StatusCode >= 200 && out.StatusCode < 300
	now := time.Now().UnixMilli()

	// Use ExecBgContext so webhook telemetry never bumps global content dataRev or flushes article caches.
	_, _ = d.store.ExecBgContext(ctx,
		`UPDATE webhooks SET last_status_code = ?, last_error = ?, last_triggered_at = ?, updated_at = ? WHERE id = ?`,
		out.StatusCode, errMsg, now, now, webhookID)

	_, _ = d.store.ExecBgContext(ctx,
		`INSERT INTO webhook_deliveries (id, webhook_id, event, status_code, duration_ms, success, payload_json, response_body, error_text, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.Next(), webhookID, event, out.StatusCode, durMS, success, payloadJSON, respBody, errMsg, now)

	_, _ = d.store.ExecBgContext(ctx,
		`DELETE FROM webhook_deliveries WHERE webhook_id = ? AND id NOT IN (SELECT id FROM (SELECT id FROM webhook_deliveries WHERE webhook_id = ? ORDER BY created_at DESC, id DESC LIMIT ?) AS keep_rows)`,
		webhookID, webhookID, maxWebhookDeliveryHistory)
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 3 {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		return s[:cut]
	}
	cut := maxBytes - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
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
