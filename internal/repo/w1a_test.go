package repo

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
)

func TestValidateAndNormalizeDomainJSON_PreservesDefaultsAndClampsFields(t *testing.T) {
	def := domain.DefaultSettingsSnapshot("zh-CN")

	// 1. Partial `{}` JSON must preserve defaults across domains (never zeroing LicenseTemplate, PostPermalinkStyle, etc.)
	siteOut, err := validateAndNormalizeDomainJSON(domain.SettingsDomainSite, []byte(`{"title":"My Blog"}`))
	if err != nil {
		t.Fatalf("site partial JSON failed: %v", err)
	}
	var siteVal domain.SiteSettings
	if err := json.Unmarshal(siteOut, &siteVal); err != nil {
		t.Fatal(err)
	}
	if siteVal.TimeZone != def.Site.TimeZone || siteVal.LicenseTemplate == "" {
		t.Fatalf("expected Site defaults to be preserved on partial JSON, got: %+v", siteVal)
	}

	seoPartialOut, err := validateAndNormalizeDomainJSON(domain.SettingsDomainSEO, []byte(`{}`))
	if err != nil {
		t.Fatalf("seo partial JSON failed: %v", err)
	}
	var seoVal domain.SEOSettings
	_ = json.Unmarshal(seoPartialOut, &seoVal)
	if seoVal.PostPermalinkStyle == "" || seoVal.PostPermalinkStyle != def.SEO.PostPermalinkStyle {
		t.Fatalf("expected SEO.PostPermalinkStyle default %q to be preserved on partial JSON, got %q", def.SEO.PostPermalinkStyle, seoVal.PostPermalinkStyle)
	}

	// 2. Reading clamping & ListStyle enum fallback
	readOut, err := validateAndNormalizeDomainJSON(domain.SettingsDomainReading, []byte(`{
		"list_style": "unknown_mode",
		"recent_posts_count": 999,
		"related_posts_count": -5,
		"popular_tags_count": 250,
		"recent_comments_count": -1
	}`))
	if err != nil {
		t.Fatalf("reading validation failed: %v", err)
	}
	var readVal domain.ReadingSettings
	_ = json.Unmarshal(readOut, &readVal)
	if readVal.ListStyle != "summary" || readVal.RecentPostsCount != 100 || readVal.RelatedPostsCount != 0 || readVal.PopularTagsCount != 100 || readVal.RecentCommentsCount != 0 {
		t.Fatalf("unexpected clamped reading values: %+v", readVal)
	}

	// 3. Storage clamping (MaxUploadMB 1..2048, ImageQuality 1..100) + default StripEXIF preserved
	storOut, err := validateAndNormalizeDomainJSON(domain.SettingsDomainStorage, []byte(`{"max_upload_mb": 5000, "image_quality": 0}`))
	if err != nil {
		t.Fatalf("storage validation failed: %v", err)
	}
	var storVal domain.StorageSettings
	_ = json.Unmarshal(storOut, &storVal)
	if storVal.MaxUploadMB != 2048 || storVal.ImageQuality != 1 || !storVal.StripEXIF {
		t.Fatalf("unexpected storage normalization: %+v", storVal)
	}

	// 4. Appearance DefaultMode validation (`auto|system|light|dark`)
	for _, mode := range []string{"auto", "system", "light", "dark"} {
		if _, err := validateAndNormalizeDomainJSON(domain.SettingsDomainAppearance, []byte(`{"default_mode":"`+mode+`"}`)); err != nil {
			t.Fatalf("expected appearance.default_mode=%q to be valid: %v", mode, err)
		}
	}
	if _, err := validateAndNormalizeDomainJSON(domain.SettingsDomainAppearance, []byte(`{"default_mode":"neon"}`)); err == nil {
		t.Fatalf("expected invalid appearance.default_mode to error")
	}

	// 5. Security GeoDefaultAction, GeoUnknownAction, TOTPRequiredScope, TOTPRememberDays, TOTPWindowSteps
	secOut, err := validateAndNormalizeDomainJSON(domain.SettingsDomainSecurity, []byte(`{
		"geo_default_action": "deny",
		"geo_unknown_action": "deny",
		"totp_required_scope": "staff",
		"totp_remember_days": 999,
		"totp_window_steps": 42
	}`))
	if err != nil {
		t.Fatalf("security validation failed: %v", err)
	}
	var secVal domain.SecuritySettings
	_ = json.Unmarshal(secOut, &secVal)
	if secVal.GeoDefaultAction != "deny" || secVal.GeoUnknownAction != "deny" || secVal.TOTPRequiredScope != "staff" || secVal.TOTPRememberDays != 365 || secVal.TOTPWindowSteps != 5 {
		t.Fatalf("unexpected security normalization: %+v", secVal)
	}
	if _, err := validateAndNormalizeDomainJSON(domain.SettingsDomainSecurity, []byte(`{"geo_unknown_action":"maybe"}`)); err == nil {
		t.Fatalf("expected invalid geo_unknown_action to be rejected")
	}
	if _, err := validateAndNormalizeDomainJSON(domain.SettingsDomainSecurity, []byte(`{"totp_required_scope":"everyone"}`)); err == nil {
		t.Fatalf("expected invalid totp_required_scope to be rejected")
	}

	// 6. SEO IndexNowKey validation (`^[a-zA-Z0-9-]{8,128}$`)
	validSEO, _ := json.Marshal(domain.SEOSettings{IndexNowKey: "validKey-12345678"})
	if _, err := validateAndNormalizeDomainJSON(domain.SettingsDomainSEO, validSEO); err != nil {
		t.Fatalf("expected valid IndexNowKey to pass: %v", err)
	}
	shortSEO, _ := json.Marshal(domain.SEOSettings{IndexNowKey: "short"})
	if _, err := validateAndNormalizeDomainJSON(domain.SettingsDomainSEO, shortSEO); err == nil {
		t.Fatalf("expected short IndexNowKey to be rejected")
	}
	badSEO, _ := json.Marshal(domain.SEOSettings{IndexNowKey: "bad/path/../key1234"})
	if _, err := validateAndNormalizeDomainJSON(domain.SettingsDomainSEO, badSEO); err == nil {
		t.Fatalf("expected path-traversal IndexNowKey to be rejected")
	}
}

func TestNormalizeSiteInjectionHTML_StripsSVGMathBaseMetaRefreshAndXlinkHref(t *testing.T) {
	payload := strings.Join([]string{
		`<meta name="google-site-verification" content="ok123">`,
		`<svg onload="alert(1)"><animate xlink:href="#x" attributeName="href" values="javascript:alert(1)"/></svg>`,
		`<math><mtext><annotation-xml encoding="text/html"><img src=x onerror=alert(1)></annotation-xml></mtext></math>`,
		`<base href="https://evil.example.com/">`,
		`<meta http-equiv="refresh" content="0;url=https://evil.example.com">`,
		`<a xlink:href="javascript:alert(2)" href="https://safe.example.com">Link</a>`,
	}, "\n")

	clean := NormalizeSiteInjectionHTML(payload)
	for _, forbidden := range []string{"<svg", "</svg>", "<math", "</math>", "<base", "http-equiv", "xlink:href", "onerror", "onload", "evil.example.com"} {
		if strings.Contains(strings.ToLower(clean), forbidden) {
			t.Fatalf("expected %q to be stripped from site injection HTML, got: %s", forbidden, clean)
		}
	}
	if !strings.Contains(clean, `google-site-verification`) || !strings.Contains(clean, `https://safe.example.com`) {
		t.Fatalf("expected safe meta and link to remain intact, got: %s", clean)
	}
}
