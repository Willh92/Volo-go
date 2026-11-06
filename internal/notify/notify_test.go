package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/security"
)

func TestWebhookSignaturesAndSSRFDefense(t *testing.T) {
	ctx := context.Background()
	ev := WebhookEvent{
		Event:     "comment.created",
		SiteTitle: "Volo Studio",
		Title:     "New Comment",
		Body:      "Clean Go engine!",
		TargetURL: "https://blog.example.com/posts/hello#comment-1",
		Timestamp: 1762450000,
	}

	// 1. Feishu HMAC-SHA256 signature check
	_, feishuBody, _, err := BuildProviderPayload(domain.Webhook{
		Provider: "feishu",
		URL:      "https://open.feishu.cn/open-apis/bot/v2/hook/abc",
	}, "sec_feishu_123", ev)
	if err != nil {
		t.Fatalf("Feishu payload error: %v", err)
	}
	var feishuMap map[string]any
	if err := json.Unmarshal(feishuBody, &feishuMap); err != nil {
		t.Fatal(err)
	}
	wantFeishuMac := hmac.New(sha256.New, []byte("1762450000\nsec_feishu_123"))
	wantFeishuSign := base64.StdEncoding.EncodeToString(wantFeishuMac.Sum(nil))
	if feishuMap["sign"] != wantFeishuSign {
		t.Fatalf("Feishu sign=%v, want %s", feishuMap["sign"], wantFeishuSign)
	}

	// 2. DingTalk query timestamp + HMAC-SHA256 signature check
	dtURL, _, _, err := BuildProviderPayload(domain.Webhook{
		Provider: "dingtalk",
		URL:      "https://oapi.dingtalk.com/robot/send?access_token=tok1",
	}, "SEC_ding_999", ev)
	if err != nil {
		t.Fatalf("DingTalk payload error: %v", err)
	}
	parsedDT, _ := url.Parse(dtURL)
	if parsedDT.Query().Get("timestamp") != "1762450000000" || parsedDT.Query().Get("sign") == "" {
		t.Fatalf("DingTalk URL missing signed query params: %s", dtURL)
	}

	// 3. Custom Webhook HMAC-SHA256 header check
	_, customBody, customHeaders, err := BuildProviderPayload(domain.Webhook{
		Provider: "custom",
		URL:      "https://hooks.example.com/volo",
	}, "whsec_custom", ev)
	if err != nil {
		t.Fatalf("Custom payload error: %v", err)
	}
	mac := hmac.New(sha256.New, []byte("whsec_custom"))
	mac.Write([]byte("1762450000."))
	mac.Write(customBody)
	wantCustomSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if customHeaders["X-Volo-Signature"] != wantCustomSig {
		t.Fatalf("Custom signature=%q, want %q", customHeaders["X-Volo-Signature"], wantCustomSig)
	}

	// 4. SSRF defense: loopback target rejected by default OutboundSafeDialer
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer localSrv.Close()

	strictSender := NewWebhookSender(&security.OutboundSafeDialer{Timeout: 2 * time.Second})
	if _, err := strictSender.Deliver(ctx, domain.Webhook{
		Provider: "custom",
		URL:      localSrv.URL,
	}, "", ev); err == nil {
		t.Fatalf("expected SSRF dialer to block loopback webhook target %s", localSrv.URL)
	}

	// With explicit AllowLoopbackForTests=true, delivery succeeds 200 OK
	testSender := NewWebhookSender(&security.OutboundSafeDialer{
		Timeout:               2 * time.Second,
		AllowLoopbackForTests: true,
	})
	code, err := testSender.Deliver(ctx, domain.Webhook{
		Provider: "custom",
		URL:      localSrv.URL,
	}, "whsec_custom", ev)
	if err != nil || code != http.StatusOK {
		t.Fatalf("expected 200 OK with test dialer, got code=%d err=%v", code, err)
	}
}

func TestMailerPlainInternalRejectsPublicIPAndHeaderInjectionScrub(t *testing.T) {
	m := NewMailer()
	m.resolver = func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}

	err := m.Send(context.Background(), domain.NotificationsSettings{
		SMTPEnabled:   true,
		SMTPHost:      "smtp.public.example",
		SMTPPort:      25,
		SMTPSecurity:  "plain_internal",
		SMTPFromEmail: "noreply@example.com",
	}, "", MailMessage{
		To:       "reader@example.com",
		Subject:  "Injected\r\nBcc: victim@example.com",
		TextBody: "Hello",
	})
	if err == nil || !strings.Contains(err.Error(), "refuses public IP") {
		t.Fatalf("expected plain_internal mode to reject public IP, got %v", err)
	}

	// Verify CRLF header injection is scrubbed in MIME builder
	from, _ := (&Mailer{}), sanitizeHeaderToken("Subject\r\nBcc: attacker@evil.test")
	_ = from
	if strings.ContainsAny(sanitizeHeaderToken("Subject\r\nBcc: attacker@evil.test"), "\r\n") {
		t.Fatalf("CRLF injection was not scrubbed")
	}
}
