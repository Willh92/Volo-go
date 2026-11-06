package i18n

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var placeholderRe = regexp.MustCompile(`\{[a-zA-Z0-9_]+\}`)

func TestBuiltinLocalesKeyAndPlaceholderParity(t *testing.T) {
	cat, err := NewCatalog("zh-CN", "")
	if err != nil {
		t.Fatalf("NewCatalog failed: %v", err)
	}
	zh := cat.Map("zh-CN")
	en := cat.Map("en-US")

	if len(zh) != len(en) {
		t.Fatalf("locale key count mismatch: zh-CN=%d vs en-US=%d", len(zh), len(en))
	}
	for k, zhVal := range zh {
		enVal, ok := en[k]
		if !ok {
			t.Fatalf("key %q present in zh-CN but missing in en-US", k)
		}
		zhVars := placeholderRe.FindAllString(zhVal, -1)
		enVars := placeholderRe.FindAllString(enVal, -1)
		if len(zhVars) != len(enVars) {
			t.Fatalf("placeholder mismatch on key %q: zh=%v vs en=%v", k, zhVars, enVars)
		}
	}

	msg := cat.T("en_US", "article.reading_stats", map[string]any{"words": 1200, "minutes": 4})
	if msg != "1200 words · 4 min read" {
		t.Fatalf("unexpected formatted translation: %q", msg)
	}
}

func TestExternalLocaleOverlayAndRequestNegotiation(t *testing.T) {
	extDir := t.TempDir()
	jaPack := `{"meta.name":"日本語","nav.home":"ホーム"}`
	if err := os.WriteFile(filepath.Join(extDir, "ja-JP.json"), []byte(jaPack), 0o644); err != nil {
		t.Fatal(err)
	}

	cat, err := NewCatalog("zh-CN", extDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cat.T("ja", "nav.home"); got != "ホーム" {
		t.Fatalf("expected external ja-JP translation ホーム, got %q", got)
	}
	// Missing key in ja-JP falls back cleanly to defaultLocale (zh-CN)
	if got := cat.T("ja-JP", "nav.archives"); got != "归档" {
		t.Fatalf("expected fallback to zh-CN 归档, got %q", got)
	}

	req := httptest.NewRequest("GET", "https://blog.example.com/?lang=en", nil)
	if loc := cat.ResolveRequest(req, "zh-CN", true); loc != "en-US" {
		t.Fatalf("expected ?lang=en -> en-US, got %q", loc)
	}
	// When allowSwitch=false, ?lang= and Accept-Language are ignored in favor of siteDefault
	if loc := cat.ResolveRequest(req, "zh-CN", false); loc != "zh-CN" {
		t.Fatalf("expected siteDefault zh-CN when allowSwitch=false, got %q", loc)
	}
}
