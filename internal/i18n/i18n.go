package i18n

import (
	"archive/zip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

//go:embed builtin/*.json
var builtinFS embed.FS

var validLocaleCodeRe = regexp.MustCompile(`^[a-zA-Z]{2,3}(-[a-zA-Z0-9]{2,8}){0,2}$`)

// LocaleInfo describes a registered UI locale for theme/admin language switchers.
type LocaleInfo struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// LocalePackSummary describes an installed built-in or external language pack for admin management.
type LocalePackSummary struct {
	Code           string `json:"code"`
	Name           string `json:"name"`
	Dir            string `json:"dir"`
	Author         string `json:"author,omitempty"`
	Version        string `json:"version,omitempty"`
	Builtin        bool   `json:"builtin"`
	HasOverride    bool   `json:"has_override"`
	KeyCount       int    `json:"key_count"`
	CustomKeyCount int    `json:"custom_key_count"`
	CompletionPct  int    `json:"completion_pct"`
}

// Catalog manages embedded built-in locales (zh-CN, en-US) layered with optional external
// JSON language packs/overrides from VOLO_LOCALES_DIR (e.g., zh-TW.json, ja-JP.json).
type Catalog struct {
	mu            sync.RWMutex
	defaultLocale string
	externalDir   string
	builtin       map[string]map[string]string
	custom        map[string]map[string]string
	messages      map[string]map[string]string
	order         []string
}

// NewCatalog loads built-in embedded dictionaries and optionally overlays externalDir (*.json).
func NewCatalog(defaultLocale string, externalDir string) (*Catalog, error) {
	c := &Catalog{
		defaultLocale: NormalizeLocale(defaultLocale),
		externalDir:   strings.TrimSpace(externalDir),
		builtin:       make(map[string]map[string]string),
		custom:        make(map[string]map[string]string),
		messages:      make(map[string]map[string]string),
	}
	if c.defaultLocale == "" {
		c.defaultLocale = "zh-CN"
	}
	if err := c.Reload(c.externalDir); err != nil {
		return nil, err
	}
	return c, nil
}

// ExternalDir returns the configured external language pack directory.
func (c *Catalog) ExternalDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.externalDir
}

// ReloadDir reloads using the catalog's configured external directory.
func (c *Catalog) ReloadDir() error {
	return c.Reload(c.ExternalDir())
}

// Reload reloads built-in locales and overlays any *.json files found in externalDir.
func (c *Catalog) Reload(externalDir string) error {
	builtinMap := make(map[string]map[string]string)
	customMap := make(map[string]map[string]string)
	merged := make(map[string]map[string]string)

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
		if err := mergeLocaleBytes(builtinMap, code, raw); err != nil {
			return fmt.Errorf("i18n: parse builtin %s: %w", ent.Name(), err)
		}
		_ = mergeLocaleBytes(merged, code, raw)
	}

	extDir := strings.TrimSpace(externalDir)
	if extDir != "" {
		extEntries, err := os.ReadDir(extDir)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("i18n: read external locales dir %q: %w", extDir, err)
		}
		for _, ent := range extEntries {
			if ent.IsDir() || !strings.HasSuffix(strings.ToLower(ent.Name()), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(extDir, ent.Name()))
			if err != nil {
				return fmt.Errorf("i18n: read external locale %s: %w", ent.Name(), err)
			}
			baseCode := strings.TrimSuffix(ent.Name(), filepath.Ext(ent.Name()))
			code := NormalizeLocale(baseCode)
			if code == "" {
				continue
			}
			if err := mergeLocaleBytes(customMap, code, raw); err != nil {
				return fmt.Errorf("i18n: parse external locale %s: %w", ent.Name(), err)
			}
			_ = mergeLocaleBytes(merged, code, raw)
		}
	}

	codes := make([]string, 0, len(merged))
	for k := range merged {
		codes = append(codes, k)
	}
	sort.Strings(codes)

	c.mu.Lock()
	c.externalDir = extDir
	c.builtin = builtinMap
	c.custom = customMap
	c.messages = merged
	c.order = codes
	c.mu.Unlock()
	return nil
}

func mergeLocaleBytes(dst map[string]map[string]string, code string, raw []byte) error {
	var parsed map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	if codeVal := strings.TrimSpace(parsed["meta.code"]); codeVal != "" && code == "" {
		code = NormalizeLocale(codeVal)
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

// ValidateLocaleCode validates and normalizes a BCP-47 locale tag for safe filesystem storage.
func ValidateLocaleCode(raw string) (string, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(raw, "_", "-"))
	if trimmed == "" || strings.ContainsAny(trimmed, `/\.`) {
		return "", errors.New("invalid locale code")
	}
	if !validLocaleCodeRe.MatchString(trimmed) {
		return "", fmt.Errorf("invalid BCP-47 locale code %q", raw)
	}
	norm := NormalizeLocale(trimmed)
	if norm == "" || !validLocaleCodeRe.MatchString(norm) {
		return "", fmt.Errorf("invalid normalized locale code %q", norm)
	}
	return norm, nil
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

// Packs returns detailed summaries of all built-in and external language packs.
func (c *Catalog) Packs() []LocalePackSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()

	refCount := len(c.builtin["zh-CN"])
	if len(c.builtin["en-US"]) > refCount {
		refCount = len(c.builtin["en-US"])
	}
	if refCount == 0 {
		refCount = 1
	}

	out := make([]LocalePackSummary, 0, len(c.order))
	for _, code := range c.order {
		tbl := c.messages[code]
		_, isBuiltin := c.builtin[code]
		custTbl, hasOverride := c.custom[code]

		covered := 0
		for k := range c.builtin["en-US"] {
			if strings.TrimSpace(tbl[k]) != "" {
				covered++
			}
		}
		pct := (covered * 100) / refCount
		if isBuiltin || pct > 100 {
			pct = 100
		}

		out = append(out, LocalePackSummary{
			Code:           code,
			Name:           tbl["meta.name"],
			Dir:            tbl["meta.dir"],
			Author:         tbl["meta.author"],
			Version:        tbl["meta.version"],
			Builtin:        isBuiltin,
			HasOverride:    hasOverride,
			KeyCount:       len(tbl),
			CustomKeyCount: len(custTbl),
			CompletionPct:  pct,
		})
	}
	return out
}

// CustomMap returns a shallow copy of ONLY the external language pack keys for locale (if any).
func (c *Catalog) CustomMap(locale string) map[string]string {
	resolved := c.Match(locale, "")
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]string)
	for k, v := range c.custom[resolved] {
		out[k] = v
	}
	return out
}

// InstallPackJSON validates and writes a single JSON language pack into externalDir and hot-reloads.
func (c *Catalog) InstallPackJSON(codeHint string, raw []byte, overwrite bool) (LocalePackSummary, error) {
	extDir := c.ExternalDir()
	if extDir == "" {
		return LocalePackSummary{}, errors.New("external locales directory is not configured")
	}
	if len(raw) == 0 || len(raw) > 4<<20 {
		return LocalePackSummary{}, errors.New("language pack JSON must be between 1B and 4MB")
	}

	var parsed map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return LocalePackSummary{}, fmt.Errorf("invalid language pack JSON: %w", err)
	}
	if len(parsed) == 0 {
		return LocalePackSummary{}, errors.New("language pack JSON cannot be empty")
	}

	candidate := strings.TrimSpace(codeHint)
	if candidate == "" {
		candidate = strings.TrimSpace(parsed["meta.code"])
	}
	code, err := ValidateLocaleCode(candidate)
	if err != nil {
		return LocalePackSummary{}, err
	}
	parsed["meta.code"] = code
	if strings.TrimSpace(parsed["meta.name"]) == "" {
		parsed["meta.name"] = code
	}
	dir := strings.ToLower(strings.TrimSpace(parsed["meta.dir"]))
	if dir != "rtl" {
		dir = "ltr"
	}
	parsed["meta.dir"] = dir

	if err := os.MkdirAll(extDir, 0o755); err != nil {
		return LocalePackSummary{}, fmt.Errorf("create locales dir: %w", err)
	}
	dstPath := filepath.Join(extDir, code+".json")
	if !overwrite {
		if _, statErr := os.Stat(dstPath); statErr == nil {
			return LocalePackSummary{}, fmt.Errorf("language pack %s already exists", code)
		}
	}

	formatted, err := marshalSortedMap(parsed)
	if err != nil {
		return LocalePackSummary{}, err
	}
	tmpPath := dstPath + ".tmp"
	if err := os.WriteFile(tmpPath, formatted, 0o644); err != nil {
		return LocalePackSummary{}, fmt.Errorf("write locale pack: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return LocalePackSummary{}, fmt.Errorf("commit locale pack: %w", err)
	}

	if err := c.Reload(extDir); err != nil {
		return LocalePackSummary{}, err
	}
	for _, p := range c.Packs() {
		if p.Code == code {
			return p, nil
		}
	}
	return LocalePackSummary{Code: code, Name: parsed["meta.name"], Dir: dir}, nil
}

// InstallPackZip extracts one or more *.json language packs from a .zip archive into externalDir.
func (c *Catalog) InstallPackZip(r io.ReaderAt, size int64, overwrite bool) ([]LocalePackSummary, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("invalid language pack zip: %w", err)
	}
	var installed []LocalePackSummary
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(f.Name)
		if strings.HasPrefix(base, ".") || !strings.HasSuffix(strings.ToLower(base), ".json") {
			continue
		}
		if f.UncompressedSize64 > 4<<20 {
			return nil, fmt.Errorf("file %s exceeds 4MB limit", base)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		raw, rErr := io.ReadAll(io.LimitReader(rc, 4<<20+1))
		_ = rc.Close()
		if rErr != nil {
			return nil, rErr
		}
		hint := strings.TrimSuffix(base, filepath.Ext(base))
		// Infer code from JSON meta.code first if filename is generic like template.json or locale.json
		var peek map[string]string
		if json.Unmarshal(raw, &peek) == nil && strings.TrimSpace(peek["meta.code"]) != "" {
			hint = peek["meta.code"]
		}
		sum, iErr := c.InstallPackJSON(hint, raw, overwrite)
		if iErr != nil {
			return nil, iErr
		}
		installed = append(installed, sum)
	}
	if len(installed) == 0 {
		return nil, errors.New("no valid *.json language pack found in zip archive")
	}
	return installed, nil
}

// DeleteExternalPack removes an external language pack or external override from disk and reloads.
func (c *Catalog) DeleteExternalPack(rawCode string) error {
	extDir := c.ExternalDir()
	if extDir == "" {
		return errors.New("external locales directory is not configured")
	}
	code, err := ValidateLocaleCode(rawCode)
	if err != nil {
		return err
	}
	target := filepath.Join(extDir, code+".json")
	if _, statErr := os.Stat(target); errors.Is(statErr, os.ErrNotExist) {
		// Also check case-insensitive / underscore variants in extDir
		found := ""
		if entries, dErr := os.ReadDir(extDir); dErr == nil {
			for _, ent := range entries {
				if !ent.IsDir() && strings.HasSuffix(strings.ToLower(ent.Name()), ".json") {
					stem := strings.TrimSuffix(ent.Name(), filepath.Ext(ent.Name()))
					if NormalizeLocale(stem) == code {
						found = filepath.Join(extDir, ent.Name())
						break
					}
				}
			}
		}
		if found == "" {
			return fmt.Errorf("external language pack %s not found", code)
		}
		target = found
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	return c.Reload(extDir)
}

// ExportPackJSON returns sorted, indented JSON bytes of the resolved locale dictionary.
func (c *Catalog) ExportPackJSON(locale string) ([]byte, error) {
	m := c.Map(locale)
	return marshalSortedMap(m)
}

func marshalSortedMap(m map[string]string) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Has reports whether locale (after normalization) is registered.
func (c *Catalog) Has(locale string) bool {
	norm := NormalizeLocale(locale)
	c.mu.RLock()
	_, ok := c.messages[norm]
	c.mu.RUnlock()
	return ok
}

// Dir returns the text direction ("ltr" or "rtl") for the resolved locale.
func (c *Catalog) Dir(locale string) string {
	resolved := c.Match(locale, "")
	c.mu.RLock()
	dir := c.messages[resolved]["meta.dir"]
	c.mu.RUnlock()
	if strings.EqualFold(strings.TrimSpace(dir), "rtl") {
		return "rtl"
	}
	return "ltr"
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

// NormalizeLocale converts legacy Java-style "zh_CN"/"en_US" or lowercase tags into canonical BCP-47 ("zh-CN", "en-US", "ja-JP", "en-GB").
func NormalizeLocale(raw string) string {
	s := strings.TrimSpace(strings.ReplaceAll(raw, "_", "-"))
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "-")
	lang := strings.ToLower(parts[0])
	switch lang {
	case "zh":
		if len(parts) >= 2 {
			sub := strings.ToLower(parts[1])
			switch sub {
			case "tw", "hant":
				return "zh-TW"
			case "hk", "mo":
				return "zh-" + strings.ToUpper(sub)
			case "cn", "hans", "sg":
				return "zh-CN"
			}
		}
		return "zh-CN"
	case "en":
		if len(parts) >= 2 && !strings.EqualFold(parts[1], "us") {
			return "en-" + strings.ToUpper(parts[1])
		}
		return "en-US"
	default:
		if len(parts) == 1 {
			return lang
		}
		if len(parts[1]) == 4 {
			script := strings.ToUpper(parts[1][:1]) + strings.ToLower(parts[1][1:])
			if len(parts) >= 3 {
				return lang + "-" + script + "-" + strings.ToUpper(parts[2])
			}
			return lang + "-" + script
		}
		return lang + "-" + strings.ToUpper(parts[1])
	}
}
