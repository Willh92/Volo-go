package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/security"
)

// WebhookEvent represents a normalized event delivered to multi-platform Webhook channels.
type WebhookEvent struct {
	_ [0]func()

	Event     string         `json:"event"` // comment.created | article.published | webhook.test
	SiteTitle string         `json:"site_title"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	TargetURL string         `json:"target_url"`
	Timestamp int64          `json:"timestamp"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// WebhookSender delivers signed payloads to Feishu, WeCom, DingTalk, Bark, Telegram, and Custom Webhooks
// strictly through OutboundSafeDialer (preventing SSRF against loopback/LAN/metadata targets).
type WebhookSender struct {
	httpClient *http.Client
}

// NewWebhookSender constructs an SSRF-safe WebhookSender.
func NewWebhookSender(dialer *security.OutboundSafeDialer) *WebhookSender {
	if dialer == nil {
		dialer = &security.OutboundSafeDialer{Timeout: 8 * time.Second}
	}
	return &WebhookSender{
		httpClient: dialer.NewHTTPClient(8 * time.Second),
	}
}

// WebhookDeliveryOutcome captures full telemetry from a single outbound webhook HTTP attempt.
type WebhookDeliveryOutcome struct {
	_ [0]func()

	StatusCode   int
	DurationMS   int
	PayloadJSON  string
	ResponseBody string
}

// Deliver sends ev to a single Webhook definition using its decrypted secret (if any) and returns the HTTP status code.
func (s *WebhookSender) Deliver(ctx context.Context, wh domain.Webhook, plainSecret string, ev WebhookEvent) (int, error) {
	out, err := s.DeliverDetailed(ctx, wh, plainSecret, ev)
	return out.StatusCode, err
}

// DeliverDetailed sends ev to a single Webhook definition and returns detailed telemetry for webhook_deliveries.
func (s *WebhookSender) DeliverDetailed(ctx context.Context, wh domain.Webhook, plainSecret string, ev WebhookEvent) (WebhookDeliveryOutcome, error) {
	var out WebhookDeliveryOutcome
	rawURL := strings.TrimSpace(wh.URL)
	if rawURL == "" {
		return out, errors.New("webhook: url is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return out, errors.New("webhook: url must use https or http")
	}
	if ev.Timestamp <= 0 {
		ev.Timestamp = time.Now().Unix()
	}

	targetURL, bodyBytes, headers, err := BuildProviderPayload(wh, plainSecret, ev)
	if err != nil {
		return out, err
	}
	out.PayloadJSON = sanitizeStoredWebhookPayload(wh.Provider, bodyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "Volo-go-Webhook/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	started := time.Now()
	resp, err := s.httpClient.Do(req)
	if err != nil {
		out.DurationMS = int(time.Since(started).Milliseconds())
		return out, err
	}
	defer resp.Body.Close()
	respSnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	out.DurationMS = int(time.Since(started).Milliseconds())
	out.StatusCode = resp.StatusCode
	out.ResponseBody = truncateUTF8Bytes(strings.TrimSpace(string(respSnippet)), maxWebhookRespBytes)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("webhook HTTP %d: %s", resp.StatusCode, out.ResponseBody)
	}
	return out, nil
}

func sanitizeStoredWebhookPayload(provider string, raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(provider), "feishu") {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			if _, ok := m["sign"]; ok {
				m["sign"] = "[REDACTED]"
				if scrubbed, err := json.Marshal(m); err == nil {
					raw = scrubbed
				}
			}
		}
	}
	return truncateUTF8Bytes(string(raw), maxWebhookPayloadBytes)
}

// BuildProviderPayload formats and cryptographically signs the outbound request for each provider.
func BuildProviderPayload(wh domain.Webhook, secret string, ev WebhookEvent) (targetURL string, payload []byte, headers map[string]string, err error) {
	headers = make(map[string]string)
	targetURL = strings.TrimSpace(wh.URL)
	provider := strings.ToLower(strings.TrimSpace(wh.Provider))
	if provider == "" {
		provider = "custom"
	}

	summaryLine := ev.Body
	if ev.TargetURL != "" {
		summaryLine = strings.TrimSpace(ev.Body + "\n" + ev.TargetURL)
	}

	switch provider {
	case "feishu":
		tsStr := strconv.FormatInt(ev.Timestamp, 10)
		msg := map[string]any{
			"msg_type": "text",
			"content": map[string]string{
				"text": fmt.Sprintf("[%s] %s\n%s", ev.SiteTitle, ev.Title, summaryLine),
			},
		}
		if secret != "" {
			// Feishu Bot V2 signature: key = timestamp + "\n" + secret, message = ""
			stringToSign := tsStr + "\n" + secret
			mac := hmac.New(sha256.New, []byte(stringToSign))
			msg["timestamp"] = tsStr
			msg["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
		payload, err = json.Marshal(msg)
		return targetURL, payload, headers, err

	case "dingtalk":
		if secret != "" {
			tsMS := strconv.FormatInt(ev.Timestamp*1000, 10)
			stringToSign := tsMS + "\n" + secret
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(stringToSign))
			sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			u, uErr := url.Parse(targetURL)
			if uErr != nil {
				return "", nil, nil, uErr
			}
			q := u.Query()
			q.Set("timestamp", tsMS)
			q.Set("sign", strings.NewReplacer("%25", "%").Replace(sign))
			// Ensure standard DingTalk query encoding via url.Values
			q.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			u.RawQuery = q.Encode()
			targetURL = u.String()
		}
		mdText := fmt.Sprintf("### %s\n\n%s", ev.Title, ev.Body)
		if ev.TargetURL != "" {
			mdText += fmt.Sprintf("\n\n[Open](%s)", ev.TargetURL)
		}
		payload, err = json.Marshal(map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]string{
				"title": ev.Title,
				"text":  mdText,
			},
		})
		return targetURL, payload, headers, err

	case "wecom":
		mdText := fmt.Sprintf("**%s**\n>%s", ev.Title, strings.ReplaceAll(ev.Body, "\n", "\n>"))
		if ev.TargetURL != "" {
			mdText += fmt.Sprintf("\n[Open](%s)", ev.TargetURL)
		}
		payload, err = json.Marshal(map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]string{
				"content": mdText,
			},
		})
		return targetURL, payload, headers, err

	case "bark":
		barkBody := map[string]any{
			"title": ev.Title,
			"body":  ev.Body,
			"group": ev.SiteTitle,
		}
		if ev.TargetURL != "" {
			barkBody["url"] = ev.TargetURL
		}
		payload, err = json.Marshal(barkBody)
		return targetURL, payload, headers, err

	case "telegram":
		chatID := strings.TrimSpace(wh.ChatID)
		if chatID == "" {
			return "", nil, nil, errors.New("webhook: telegram requires chat_id")
		}
		tgHTML := fmt.Sprintf("<b>%s</b>\n%s", html.EscapeString(ev.Title), html.EscapeString(ev.Body))
		if ev.TargetURL != "" {
			tgHTML += fmt.Sprintf("\n<a href=\"%s\">Open</a>", html.EscapeString(ev.TargetURL))
		}
		payload, err = json.Marshal(map[string]any{
			"chat_id":                  chatID,
			"text":                     tgHTML,
			"parse_mode":               "HTML",
			"disable_web_page_preview": true,
		})
		return targetURL, payload, headers, err

	case "custom":
		envelope := map[string]any{
			"event":      ev.Event,
			"site_title": ev.SiteTitle,
			"title":      ev.Title,
			"body":       ev.Body,
			"url":        ev.TargetURL,
			"timestamp":  ev.Timestamp,
			"data":       ev.Extra,
		}
		payload, err = json.Marshal(envelope)
		if err != nil {
			return "", nil, nil, err
		}
		tsStr := strconv.FormatInt(ev.Timestamp, 10)
		headers["X-Volo-Event"] = ev.Event
		headers["X-Volo-Timestamp"] = tsStr
		if secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(tsStr + "."))
			mac.Write(payload)
			headers["X-Volo-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		return targetURL, payload, headers, nil

	default:
		return "", nil, nil, fmt.Errorf("webhook: unsupported provider %q", provider)
	}
}
