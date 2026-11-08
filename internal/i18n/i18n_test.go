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

func TestPacksCompletionPctAgainstDefaultLocale(t *testing.T) {
	// 1. Real catalog: both symmetric built-in packs reach 100% naturally, while a tiny external pack reflects real coverage.
	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, "fr-FR.json"), []byte(`{"meta.name":"Français","nav.home":"Accueil"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := NewCatalog("zh-CN", extDir)
	if err != nil {
		t.Fatal(err)
	}
	byCode := make(map[string]LocalePackSummary)
	for _, p := range cat.Packs() {
		byCode[p.Code] = p
	}
	if byCode["zh-CN"].CompletionPct != 100 || byCode["en-US"].CompletionPct != 100 {
		t.Fatalf("expected symmetric built-in packs at 100%%, got zh-CN=%d%% en-US=%d%%",
			byCode["zh-CN"].CompletionPct, byCode["en-US"].CompletionPct)
	}
	if byCode["fr-FR"].CompletionPct <= 0 || byCode["fr-FR"].CompletionPct >= 20 {
		t.Fatalf("expected partial external fr-FR CompletionPct in (0,20), got %d%%", byCode["fr-FR"].CompletionPct)
	}

	// 2. Synthetic catalog: an incomplete built-in pack MUST NOT be artificially clamped to 100%,
	// and an external override supplying missing keys raises CompletionPct accurately against defaultLocale!
	syn := &Catalog{
		defaultLocale: "zh-CN",
		builtin: map[string]map[string]string{
			"zh-CN": {
				"meta.code": "zh-CN",
				"meta.name": "简体中文",
				"meta.dir":  "ltr",
				"k1":        "一",
				"k2":        "二",
				"k3":        "三",
				"k4":        "四",
				"k5":        "五",
				"k6":        "六",
				"k7":        "七",
			},
			"en-US": {
				"meta.code": "en-US",
				"meta.name": "English",
				"meta.dir":  "ltr",
				"k1":        "One",
				"k2":        "Two",
				"k3":        "   ", // blank should not count as translated
			},
		},
		custom: map[string]map[string]string{
			"ja-JP": {
				"meta.code": "ja-JP",
				"meta.name": "日本語",
				"meta.dir":  "ltr",
				"k1":        "いち",
				"k2":        "に",
			},
		},
		order: []string{"en-US", "ja-JP", "zh-CN"},
	}
	synByCode := make(map[string]LocalePackSummary)
	for _, p := range syn.Packs() {
		synByCode[p.Code] = p
	}
	// Reference defaultLocale zh-CN has 10 keys.
	// en-US covers 5 non-blank keys (meta.code, meta.name, meta.dir, k1, k2) -> 50% (NOT hardcoded 100%!).
	if synByCode["zh-CN"].CompletionPct != 100 {
		t.Fatalf("expected defaultLocale zh-CN CompletionPct=100, got %d", synByCode["zh-CN"].CompletionPct)
	}
	if synByCode["en-US"].CompletionPct != 50 {
		t.Fatalf("expected incomplete built-in en-US CompletionPct=50, got %d", synByCode["en-US"].CompletionPct)
	}
	if synByCode["ja-JP"].CompletionPct != 50 {
		t.Fatalf("expected partial external ja-JP CompletionPct=50, got %d", synByCode["ja-JP"].CompletionPct)
	}
}
