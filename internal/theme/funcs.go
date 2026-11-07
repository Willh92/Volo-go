package theme

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Willh92/Volo-go/internal/render"
)

func (e *Engine) templateFuncMap(themeID string, assets map[string]cachedAsset, fallbackAssets map[string]cachedAsset, themeLocales map[string]map[string]string) template.FuncMap {
	translate := func(locale string, key string, vars ...map[string]any) string {
		if msg, ok := lookupThemeLocaleMessage(themeLocales, locale, key); ok {
			if len(vars) > 0 && vars[0] != nil {
				for k, v := range vars[0] {
					s := fmt.Sprint(v)
					msg = strings.ReplaceAll(msg, "{{"+k+"}}", s)
					msg = strings.ReplaceAll(msg, "{"+k+"}", s)
				}
			}
			return msg
		}
		if e.catalog == nil {
			return key
		}
		return e.catalog.T(locale, key, vars...)
	}

	themeBoolFn := func(cfg map[string]any, key string, fallback bool) bool {
		if cfg == nil {
			return fallback
		}
		v, ok := cfg[key]
		if !ok || v == nil {
			return fallback
		}
		switch b := v.(type) {
		case bool:
			return b
		case string:
			switch strings.ToLower(strings.TrimSpace(b)) {
			case "true", "1", "yes", "on":
				return true
			case "false", "0", "no", "off":
				return false
			}
		case float64:
			return b != 0
		case int:
			return b != 0
		}
		return fallback
	}

	themeIntFn := func(cfg map[string]any, key string, fallback int) int {
		if cfg == nil {
			return fallback
		}
		v, ok := cfg[key]
		if !ok || v == nil {
			return fallback
		}
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		case string:
			if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				return parsed
			}
		}
		return fallback
	}

	return template.FuncMap{
		"t": translate,
		"tt": func(args ...any) string {
			// Supports:
			//   {{tt .ThemeID .Locale "key" (dict ...)}}
			//   {{tt .Locale "key" (dict ...)}}
			//   {{tt "key" (dict ...)}}
			var strs []string
			var vars []map[string]any
			for _, a := range args {
				switch v := a.(type) {
				case string:
					strs = append(strs, v)
				case map[string]any:
					vars = append(vars, v)
				}
			}
			switch {
			case len(strs) >= 3:
				return translate(strs[1], strs[2], vars...)
			case len(strs) == 2:
				return translate(strs[0], strs[1], vars...)
			case len(strs) == 1:
				return translate("zh-CN", strs[0], vars...)
			default:
				return ""
			}
		},
		"assetURL": func(tid string, rel string) string {
			if tid == "" {
				tid = themeID
			}
			clean := strings.TrimPrefix(rel, "/")
			a, ok := assets[clean]
			if !ok && fallbackAssets != nil {
				a, ok = fallbackAssets[clean]
			}
			prefix := e.assetCDNPrefix()
			if ok && a.shortHash != "" {
				return fmt.Sprintf("%s/themes/%s/assets/%s?v=%s", prefix, tid, clean, a.shortHash)
			}
			return fmt.Sprintf("%s/themes/%s/assets/%s", prefix, tid, clean)
		},
		"vendorURL": func(rel string) string {
			clean := strings.TrimPrefix(strings.TrimSpace(rel), "/")
			if clean == "" {
				return ""
			}
			return fmt.Sprintf("%s/%s", e.vendorCDNPrefix(), clean)
		},
		"themeCfg": func(cfg map[string]any, key string, fallback ...any) any {
			var def any
			if len(fallback) > 0 {
				def = fallback[0]
			}
			if cfg == nil {
				return def
			}
			v, ok := cfg[key]
			if !ok || v == nil {
				return def
			}
			if s, isStr := v.(string); isStr && s == "" && def != nil {
				if _, defBool := def.(bool); !defBool {
					return def
				}
			}
			return v
		},
		"themeBool":    themeBoolFn,
		"themeCfgBool": themeBoolFn,
		"themeInt":     themeIntFn,
		"themeCfgInt":  themeIntFn,
		"customVar": func(vars map[string]string, key string, fallback ...string) string {
			def := ""
			if len(fallback) > 0 {
				def = fallback[0]
			}
			if vars == nil {
				return def
			}
			if v, ok := vars[key]; ok && v != "" {
				return v
			}
			return def
		},
		"pageURL": func(rawPathAndQuery string, page int) string {
			return buildThemePageURL(rawPathAndQuery, page)
		},
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s)
		},
		"renderNotice": func(raw string) template.HTML {
			clean := strings.TrimSpace(raw)
			if clean == "" {
				return ""
			}
			return template.HTML(render.Default().Render(clean, render.Options{Mode: render.RenderModeWeb}).HTML)
		},
		"linkifyLicense": func(licenseText string, canonicalURL string) template.HTML {
			clean := strings.TrimSpace(licenseText)
			if clean == "" {
				return ""
			}
			esc := html.EscapeString(clean)
			esc = strings.ReplaceAll(esc, "\r\n", "\n")
			esc = strings.ReplaceAll(esc, "\n", "<br>")
			if u, ok := sanitizeThemeURL(canonicalURL, false); ok {
				escURL := html.EscapeString(u)
				link := `<a class="volo-license-link" href="` + escURL + `">` + escURL + `</a>`
				if strings.Contains(esc, escURL) {
					esc = strings.ReplaceAll(esc, escURL, link)
				} else {
					esc += `<div class="volo-license-url">` + link + `</div>`
				}
			}
			return template.HTML(esc)
		},
		"safeCSS": func(s string) template.CSS {
			return template.CSS(sanitizeStyleContent(s))
		},
		"safeJS": func(s string) template.JS {
			return template.JS(s)
		},
		"safeURL": func(s string) template.URL {
			if u, ok := sanitizeThemeURL(s, true); ok {
				return template.URL(u)
			}
			return template.URL("#")
		},
		"cssURL": func(s string) template.CSS {
			u, ok := sanitizeThemeURL(s, false)
			if !ok {
				return template.CSS("none")
			}
			replacer := strings.NewReplacer(
				`\`, `\\`,
				`"`, `\"`,
				`'`, `\'`,
				`(`, `\(`,
				`)`, `\)`,
				`<`, `\3c `,
				`>`, `\3e `,
				`;`, `\3b `,
			)
			return template.CSS(`url("` + replacer.Replace(u) + `")`)
		},
		"fmtDate": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).Format("2006-01-02")
		},
		"formatDate": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).Format("2006-01-02")
		},
		"formatDotDate": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).Format("2006.01.02")
		},
		"formatMonthDay": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).Format("01-02")
		},
		"fmtDateTime": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).Format("2006-01-02 15:04")
		},
		"formatDateTime": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).Format("2006-01-02 15:04")
		},
		"fmtTime": func(ms int64, layout string) string {
			if ms <= 0 {
				return ""
			}
			goLayout := normalizeDateLayout(layout)
			if goLayout == "" {
				goLayout = "2006-01-02"
			}
			return time.UnixMilli(ms).Format(goLayout)
		},
		"formatTime": func(ms int64, layout string) string {
			if ms <= 0 {
				return ""
			}
			goLayout := normalizeDateLayout(layout)
			if goLayout == "" {
				goLayout = "2006-01-02"
			}
			return time.UnixMilli(ms).Format(goLayout)
		},
		"fmtISO": func(ms int64) string {
			if ms <= 0 {
				return ""
			}
			return time.UnixMilli(ms).UTC().Format(time.RFC3339)
		},
		"default": func(fallback any, val any) any {
			if isEmptyTemplateValue(val) {
				return fallback
			}
			return val
		},
		"coalesce": func(vals ...any) any {
			for _, v := range vals {
				if !isEmptyTemplateValue(v) {
					return v
				}
			}
			return ""
		},
		"truncate": func(s string, maxRunes int) string {
			if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
				return s
			}
			runes := []rune(s)
			return strings.TrimSpace(string(runes[:maxRunes])) + "…"
		},
		"urlEncode": url.QueryEscape,
		"hasPrefix": strings.HasPrefix,
		"hasSuffix": strings.HasSuffix,
		"contains":  strings.Contains,
		"replace":   strings.ReplaceAll,
		"split":     strings.Split,
		"join":      strings.Join,
		"trim":      strings.TrimSpace,
		"lower":     strings.ToLower,
		"upper":     strings.ToUpper,
		"add":       func(a, b int) int { return a + b },
		"sub":       func(a, b int) int { return a - b },
		"mul":       func(a, b int) int { return a * b },
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mod": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a % b
		},
		"seq": func(start, end int) []int {
			if end < start {
				return nil
			}
			const maxSeq = 500
			count := end - start + 1
			if count > maxSeq {
				count = maxSeq
			}
			out := make([]int, count)
			for i := 0; i < count; i++ {
				out[i] = start + i
			}
			return out
		},
		"dict": func(values ...any) map[string]any {
			m := make(map[string]any, len(values)/2)
			for i := 0; i+1 < len(values); i += 2 {
				if k, ok := values[i].(string); ok {
					m[k] = values[i+1]
				}
			}
			return m
		},
		"list": func(values ...any) []any {
			return values
		},
		"slice": func(values ...any) []any {
			return values
		},
		"first": func(limit int, items any) any {
			if limit <= 0 || items == nil {
				return nil
			}
			rv := reflect.ValueOf(items)
			if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
				return items
			}
			if rv.Len() <= limit {
				return items
			}
			return rv.Slice(0, limit).Interface()
		},
		"json": func(v any) template.JS {
			b, err := jsonMarshalSafe(v)
			if err != nil {
				return template.JS("null")
			}
			return template.JS(b)
		},
		"socialIconSVG": renderSocialIconSVG,
		"navIconHTML":   renderNavIconHTML,
		"coverSVG":      renderDeterministicCoverSVG,
	}
}

func lookupThemeLocaleMessage(themeLocales map[string]map[string]string, locale string, key string) (string, bool) {
	if len(themeLocales) == 0 {
		return "", false
	}
	norm := strings.TrimSpace(locale)
	if m, ok := themeLocales[norm]; ok {
		if val, exists := m[key]; exists && val != "" {
			return val, true
		}
	}
	base := strings.ToLower(norm)
	if idx := strings.IndexAny(base, "-_"); idx > 0 {
		base = base[:idx]
	}
	for code, m := range themeLocales {
		codeLower := strings.ToLower(code)
		if codeLower == strings.ToLower(norm) || strings.HasPrefix(codeLower, base+"-") || codeLower == base {
			if val, exists := m[key]; exists && val != "" {
				return val, true
			}
		}
	}
	if m, ok := themeLocales["en-US"]; ok {
		if val, exists := m[key]; exists && val != "" {
			return val, true
		}
	}
	return "", false
}

// normalizeDateLayout accepts either native Go reference time layouts ("2006-01-02")
// or traditional pattern tokens ("yyyy-MM-dd HH:mm:ss") used in classic skin templates.
func normalizeDateLayout(layout string) string {
	l := strings.TrimSpace(layout)
	if l == "" || strings.Contains(l, "2006") || strings.Contains(l, "15:04") || strings.Contains(l, "Jan") {
		return l
	}
	replacer := strings.NewReplacer(
		"yyyy", "2006",
		"YYYY", "2006",
		"yy", "06",
		"MM", "01",
		"dd", "02",
		"DD", "02",
		"HH", "15",
		"hh", "03",
		"mm", "04",
		"ss", "05",
	)
	return replacer.Replace(l)
}

func isEmptyTemplateValue(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) == ""
	case bool:
		return !x
	case int:
		return x == 0
	case int64:
		return x == 0
	case float64:
		return x == 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	case reflect.Ptr, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func stripThemeASCIIControls(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x1F || c == 0x7F {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func hasPrefixFoldASCIITheme(s, prefixLower string) bool {
	if len(s) < len(prefixLower) {
		return false
	}
	for i := 0; i < len(prefixLower); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefixLower[i] {
			return false
		}
	}
	return true
}

func indexFoldASCIITheme(s, needleLower string) int {
	n := len(needleLower)
	if n == 0 || len(s) < n {
		return -1
	}
	limit := len(s) - n
	for i := 0; i <= limit; i++ {
		if hasPrefixFoldASCIITheme(s[i:], needleLower) {
			return i
		}
	}
	return -1
}

func sanitizeThemeURL(raw string, allowMailTelAnchor bool) (string, bool) {
	cleaned := strings.TrimSpace(stripThemeASCIIControls(raw))
	if cleaned == "" {
		return "", false
	}
	if strings.HasPrefix(cleaned, "/") {
		if strings.HasPrefix(cleaned, "//") || strings.HasPrefix(cleaned, `/\`) {
			return "", false
		}
		return cleaned, true
	}
	if hasPrefixFoldASCIITheme(cleaned, "http://") && len(cleaned) > len("http://") {
		return cleaned, true
	}
	if hasPrefixFoldASCIITheme(cleaned, "https://") && len(cleaned) > len("https://") {
		return cleaned, true
	}
	if allowMailTelAnchor {
		if strings.HasPrefix(cleaned, "#") {
			return cleaned, true
		}
		if hasPrefixFoldASCIITheme(cleaned, "mailto:") && len(cleaned) > len("mailto:") {
			return cleaned, true
		}
		if hasPrefixFoldASCIITheme(cleaned, "tel:") && len(cleaned) > len("tel:") {
			return cleaned, true
		}
	}
	return "", false
}

func buildThemePageURL(rawPathAndQuery string, page int) string {
	cleaned := strings.TrimSpace(stripThemeASCIIControls(rawPathAndQuery))
	if cleaned == "" {
		cleaned = "/"
	}
	u, err := url.Parse(cleaned)
	if err != nil {
		if page <= 1 {
			return "/"
		}
		return "/?page=" + strconv.Itoa(page)
	}
	basePath := u.Path
	if basePath == "" {
		basePath = "/"
	}
	q := u.Query()
	if page <= 1 {
		q.Del("page")
	} else {
		q.Set("page", strconv.Itoa(page))
	}
	out := basePath
	if enc := q.Encode(); enc != "" {
		out += "?" + enc
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

func sanitizeStyleContent(css string) string {
	if css == "" {
		return ""
	}
	clean := strings.ReplaceAll(css, "\x00", "")
	for {
		changed := false
		for _, bad := range []string{"javascript:", "expression("} {
			for {
				idx := indexFoldASCIITheme(clean, bad)
				if idx < 0 {
					break
				}
				clean = clean[:idx] + clean[idx+len(bad):]
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	clean = strings.ReplaceAll(clean, "</", `<\/`)
	return clean
}

func renderSocialIconSVG(icon string) template.HTML {
	key := strings.ToLower(strings.TrimSpace(icon))
	var inner string
	switch key {
	case "github":
		inner = `<path d="M8 1.2a6.8 6.8 0 0 0-2.15 13.25c.34.06.46-.15.46-.33v-1.15c-1.89.41-2.29-.91-2.29-.91-.31-.78-.75-.99-.75-.99-.62-.42.05-.41.05-.41.68.05 1.04.7 1.04.7.61 1.04 1.59.74 1.98.57.06-.44.24-.74.43-.91-1.51-.17-3.1-.76-3.1-3.36 0-.74.26-1.35.7-1.82-.07-.17-.3-.86.07-1.8 0 0 .57-.18 1.87.7a6.5 6.5 0 0 1 3.4 0c1.3-.88 1.87-.7 1.87-.7.37.94.14 1.63.07 1.8.44.47.7 1.08.7 1.82 0 2.61-1.59 3.19-3.11 3.36.25.21.46.63.46 1.27v1.88c0 .18.12.39.47.33A6.8 6.8 0 0 0 8 1.2Z" fill="currentColor"/>`
	case "x", "twitter":
		inner = `<path d="M9.3 6.8 14.2 1.5h-1.2L8.7 6.1 5.3 1.5H1.4l5.1 7.2-5.1 5.8h1.2l4.5-5.1 3.6 5.1h3.9L9.3 6.8Zm-1.6 1.8-.5-.7L3.1 2.4h1.8l3.3 4.7.5.7 4.3 6.1h-1.8L7.7 8.6Z" fill="currentColor"/>`
	case "rss", "feed":
		inner = `<circle cx="3.8" cy="12.2" r="1.6" fill="currentColor"/><path d="M2.2 7.4a6.4 6.4 0 0 1 6.4 6.4M2.2 2.8a11 11 0 0 1 11 11" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" fill="none"/>`
	case "email", "mail":
		inner = `<rect x="1.8" y="3.2" width="12.4" height="9.6" rx="1.8" stroke="currentColor" stroke-width="1.4" fill="none"/><path d="m2.4 4.2 5.6 4.4 5.6-4.4" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" fill="none"/>`
	case "telegram":
		inner = `<path d="M14.3 2.4 1.8 7.2c-.6.24-.59.82-.05 1l3.1 1 7.2-4.5c.34-.21.65-.1.4.13L6.6 10.1l-.22 3.2c.32 0 .46-.15.64-.32l1.55-1.5 3.2 2.36c.59.32 1.01.16 1.16-.54l2.05-9.7c.2-.78-.3-1.14-.68-1.2Z" fill="currentColor"/>`
	case "bilibili", "video", "youtube":
		inner = `<rect x="1.8" y="3.8" width="12.4" height="9.2" rx="2.2" stroke="currentColor" stroke-width="1.4" fill="none"/><path d="m6.6 6.4 3.6 2-3.6 2V6.4Z" fill="currentColor"/>`
	default:
		inner = `<circle cx="8" cy="8" r="6.2" stroke="currentColor" stroke-width="1.4" fill="none"/><path d="M2 8h12M8 1.8c1.8 1.8 2.8 4 2.8 6.2S9.8 12.4 8 14.2C6.2 12.4 5.2 10.2 5.2 8S6.2 3.6 8 1.8Z" stroke="currentColor" stroke-width="1.3" fill="none"/>`
	}
	return template.HTML(`<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" style="vertical-align:-2px">` + inner + `</svg>`)
}

func renderNavIconHTML(icon string) template.HTML {
	trimmed := strings.TrimSpace(stripThemeASCIIControls(icon))
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	switch {
	case lower == "github" || strings.Contains(lower, "github-icon"):
		return template.HTML(`<svg class="studio-nav-icon" viewBox="0 0 16 16" width="15" height="15" aria-hidden="true"><path d="M8 1.2a6.8 6.8 0 0 0-2.15 13.25c.34.06.46-.15.46-.33v-1.15c-1.89.41-2.29-.91-2.29-.91-.31-.78-.75-.99-.75-.99-.62-.42.05-.41.05-.41.68.05 1.04.7 1.04.7.61 1.04 1.59.74 1.98.57.06-.44.24-.74.43-.91-1.51-.17-3.1-.76-3.1-3.36 0-.74.26-1.35.7-1.82-.07-.17-.3-.86.07-1.8 0 0 .57-.18 1.87.7a6.5 6.5 0 0 1 3.4 0c1.3-.88 1.87-.7 1.87-.7.37.94.14 1.63.07 1.8.44.47.7 1.08.7 1.82 0 2.61-1.59 3.19-3.11 3.36.25.21.46.63.46 1.27v1.88c0 .18.12.39.47.33A6.8 6.8 0 0 0 8 1.2Z" fill="currentColor"/></svg>`)
	case lower == "panel" || lower == "admin" || lower == "console" || strings.Contains(lower, "admin-panel"):
		return template.HTML(`<svg class="studio-nav-icon" viewBox="0 0 16 16" width="15" height="15" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><rect x="2.2" y="2.2" width="4.8" height="4.8" rx="1"/><rect x="9" y="2.2" width="4.8" height="4.8" rx="1"/><rect x="2.2" y="9" width="4.8" height="4.8" rx="1"/><rect x="9" y="9" width="4.8" height="4.8" rx="1"/></svg>`)
	case lower == "logout" || strings.Contains(lower, "admin-logout"):
		return template.HTML(`<svg class="studio-nav-icon" viewBox="0 0 16 16" width="15" height="15" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M6.2 2.5H3.6A1.4 1.4 0 0 0 2.2 3.9v8.2a1.4 1.4 0 0 0 1.4 1.4h2.6"/><path d="M10.5 11.2 13.8 8l-3.3-3.2"/><path d="M13.8 8H6.2"/></svg>`)
	case lower == "book" || lower == "books":
		return template.HTML(`<svg class="studio-nav-icon" viewBox="0 0 16 16" width="15" height="15" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M2.5 3.2A1.7 1.7 0 0 1 4.2 1.5h8.8v11.6H4.2a1.7 1.7 0 0 0-1.7 1.4V3.2Z"/><path d="M4.2 11.6h8.8"/></svg>`)
	case lower == "external" || lower == "link":
		return template.HTML(`<svg class="studio-nav-icon" viewBox="0 0 16 16" width="15" height="15" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M9.5 2.5h4v4"/><path d="M13.5 2.5 7.2 8.8"/><path d="M12.5 9.2v3.3a1 1 0 0 1-1 1h-8a1 1 0 0 1-1-1v-8a1 1 0 0 1 1-1h3.3"/></svg>`)
	}
	safeSrc, ok := sanitizeThemeURL(trimmed, false)
	if !ok {
		return ""
	}
	return template.HTML(`<img class="studio-nav-icon" src="` + html.EscapeString(safeSrc) + `" alt="" loading="lazy" decoding="async" data-hide-on-error="1" />`)
}

func jsonMarshalSafe(v any) ([]byte, error) {
	return json.Marshal(v)
}
