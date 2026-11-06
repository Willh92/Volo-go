package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed builtin/*.json
var builtinFS embed.FS

// LocaleInfo describes a registered UI locale for theme/admin language switchers.
type LocaleInfo struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// Catalog manages embedded built-in locales (zh-CN, en-US) layered with optional external
// JSON overrides/extensions from VOLO_LOCALES_DIR (e.g., zh-TW.json, ja-JP.json).
type Catalog struct {
	mu            sync.RWMutex
	defaultLocale string
	messages      map[string]map[string]string
	order         []string
}

// NewCatalog loads built-in embedded dictionaries and optionally overlays externalDir (*.json).
func NewCatalog(defaultLocale string, externalDir string) (*Catalog, error) {
	c := &Catalog{
		defaultLocale: NormalizeLocale(defaultLocale),
		messages:      make(map[string]map[string]string),
	}
	if c.defaultLocale == "" {
		c.defaultLocale = "zh-CN"
	}
	if err := c.Reload(externalDir); err != nil {
		return nil, err
	}
	return c, nil
}

// Reload reloads built-in locales and overlays any *.json files found in externalDir.
func (c *Catalog) Reload(externalDir string) error {
	next := make(map[string]map[string]string)

	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return fmt.Errorf("i18n: read builtin dir: %w", err)
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		raw, err := builtinFS.ReadFile("builtin/" + ent.Name())
		if err != nil {
			return fmt.Errorf("i18n: read builtin %s: %w", ent.Name(), err)
		}
		code := NormalizeLocale(strings.TrimSuffix(ent.Name(), ".json"))
		if err := mergeLocaleBytes(next, code, raw); err != nil {
			return fmt.Errorf("i18n: parse builtin %s: %w", ent.Name(), err)
		}
	}

	if strings.TrimSpace(externalDir) != "" {
		extEntries, err := os.ReadDir(externalDir)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("i18n: read external locales dir %q: %w", externalDir, err)
		}
		for _, ent := range extEntries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(externalDir, ent.Name()))
			if err != nil {
				return fmt.Errorf("i18n: read external locale %s: %w", ent.Name(), err)
			}
			code := NormalizeLocale(strings.TrimSuffix(ent.Name(), ".json"))
			if err := mergeLocaleBytes(next, code, raw); err != nil {
				return fmt.Errorf("i18n: parse external locale %s: %w", ent.Name(), err)
			}
		}
	}

	codes := make([]string, 0, len(next))
	for k := range next {
		codes = append(codes, k)
	}
	sort.Strings(codes)

	c.mu.Lock()
	c.messages = next
	c.order = codes
	c.mu.Unlock()
	return nil
}

func mergeLocaleBytes(dst map[string]map[string]string, code string, raw []byte) error {
	var parsed map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	table, ok := dst[code]
	if !ok {
		table = make(map[string]string, len(parsed))
		dst[code] = table
	}
	for k, v := range parsed {
		table[k] = v
	}
	if table["meta.code"] == "" {
		table["meta.code"] = code
	}
	if table["meta.name"] == "" {
		table["meta.name"] = code
	}
	if table["meta.dir"] == "" {
		table["meta.dir"] = "ltr"
	}
	return nil
}

// Locales returns metadata for all registered locales.
func (c *Catalog) Locales() []LocaleInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]LocaleInfo, 0, len(c.order))
	for _, code := range c.order {
		tbl := c.messages[code]
		out = append(out, LocaleInfo{
			Code: code,
			Name: tbl["meta.name"],
			Dir:  tbl["meta.dir"],
		})
	}
	return out
}

// Has reports whether locale (after normalization) is registered.
func (c *Catalog) Has(locale string) bool {
	norm := NormalizeLocale(locale)
	c.mu.RLock()
	_, ok := c.messages[norm]
	c.mu.RUnlock()
	return ok
}

// Map returns a shallow copy of the full translation dictionary for the resolved locale
// (falling back to defaultLocale and en-US for any missing custom locale keys).
func (c *Catalog) Map(locale string) map[string]string {
	resolved := c.Match(locale, "")
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]string)
	for k, v := range c.messages["en-US"] {
		out[k] = v
	}
	if defTbl, ok := c.messages[c.defaultLocale]; ok {
		for k, v := range defTbl {
			out[k] = v
		}
	}
	if tbl, ok := c.messages[resolved]; ok {
		for k, v := range tbl {
			out[k] = v
		}
	}
	return out
}

// T translates key in locale with optional named placeholder substitutions ({name} -> val).
func (c *Catalog) T(locale string, key string, vars ...map[string]any) string {
	norm := c.Match(locale, "")
	c.mu.RLock()
	val, ok := c.messages[norm][key]
	if !ok {
		val, ok = c.messages[c.defaultLocale][key]
	}
	if !ok {
		val, ok = c.messages["en-US"][key]
	}
	c.mu.RUnlock()
	if !ok {
		return key
	}
	if len(vars) == 0 || len(vars[0]) == 0 {
		return val
	}
	pairs := make([]string, 0, len(vars[0])*2)
	for k, v := range vars[0] {
		pairs = append(pairs, "{"+k+"}", fmt.Sprint(v))
	}
	return strings.NewReplacer(pairs...).Replace(val)
}

// Match resolves a requested locale tag (e.g. "zh_CN", "zh-Hans", "en") to an available catalog code,
// falling back to siteDefault (if provided) and c.defaultLocale.
func (c *Catalog) Match(requested string, siteDefault string) string {
	norm := NormalizeLocale(requested)
	c.mu.RLock()
	defer c.mu.RUnlock()

	if norm != "" {
		if _, ok := c.messages[norm]; ok {
			return norm
		}
		langPrefix := strings.ToLower(strings.Split(norm, "-")[0])
		for _, code := range c.order {
			if strings.ToLower(strings.Split(code, "-")[0]) == langPrefix {
				return code
			}
		}
	}

	sd := NormalizeLocale(siteDefault)
	if sd != "" {
		if _, ok := c.messages[sd]; ok {
			return sd
		}
	}
	if _, ok := c.messages[c.defaultLocale]; ok {
		return c.defaultLocale
	}
	return "zh-CN"
}

// ResolveRequest resolves the active UI locale for a public HTTP request:
// ?lang= query -> Cookie "volo_lang" -> Accept-Language (when allowSwitch=true) -> siteDefault.
func (c *Catalog) ResolveRequest(r *http.Request, siteDefault string, allowSwitch bool) string {
	if r == nil {
		return c.Match("", siteDefault)
	}
	if allowSwitch {
		if q := strings.TrimSpace(r.URL.Query().Get("lang")); q != "" && c.HasLanguageMatch(q) {
			return c.Match(q, siteDefault)
		}
		if ck, err := r.Cookie("volo_lang"); err == nil && ck.Value != "" && c.HasLanguageMatch(ck.Value) {
			return c.Match(ck.Value, siteDefault)
		}
		if al := r.Header.Get("Accept-Language"); al != "" {
			for _, part := range strings.Split(al, ",") {
				tag := strings.TrimSpace(strings.Split(part, ";")[0])
				if tag != "" && c.HasLanguageMatch(tag) {
					return c.Match(tag, siteDefault)
				}
			}
		}
	}
	return c.Match("", siteDefault)
}

// HasLanguageMatch checks if requested matches an exact or language-prefix registered locale.
func (c *Catalog) HasLanguageMatch(requested string) bool {
	norm := NormalizeLocale(requested)
	if norm == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, ok := c.messages[norm]; ok {
		return true
	}
	prefix := strings.ToLower(strings.Split(norm, "-")[0])
	for _, code := range c.order {
		if strings.ToLower(strings.Split(code, "-")[0]) == prefix {
			return true
		}
	}
	return false
}

// NormalizeLocale converts legacy Java-style "zh_CN"/"en_US" or lowercase tags into canonical BCP-47 ("zh-CN", "en-US").
func NormalizeLocale(raw string) string {
	s := strings.TrimSpace(strings.ReplaceAll(raw, "_", "-"))
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "-")
	switch strings.ToLower(parts[0]) {
	case "zh":
		if len(parts) >= 2 && (strings.EqualFold(parts[1], "tw") || strings.EqualFold(parts[1], "hk") || strings.EqualFold(parts[1], "hant")) {
			return "zh-TW"
		}
		return "zh-CN"
	case "en":
		return "en-US"
	default:
		if len(parts) == 1 {
			return strings.ToLower(parts[0])
		}
		return strings.ToLower(parts[0]) + "-" + strings.ToUpper(parts[1])
	}
}
