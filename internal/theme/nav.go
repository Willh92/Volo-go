package theme

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultNavItemKeys defines the default ordered navigation slots for themes.
// Built-in routes come first and `custom` (user-defined `nav_items`) is placed last by default.
var DefaultNavItemKeys = []string{
	"home",
	"archives",
	"tags",
	"categories",
	"series",
	"books",
	"links",
	"follows",
	"dynamic",
	"custom",
}

// DefaultNavPrimaryCount is the default number of leading navigation links shown directly
// in the header bar before overflowing remaining items into the "More" menu.
const DefaultNavPrimaryCount = 5

// NavLink represents a single resolved navigation link ready for theme templates.
type NavLink struct {
	Key          string `json:"key"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	Icon         string `json:"icon,omitempty"`
	OpenInNewTab bool   `json:"open_in_new_tab,omitempty"`
	IsActive     bool   `json:"is_active"`
	IsCustom     bool   `json:"is_custom,omitempty"`
}

// NavMenu provides theme templates with ordered, visibility-filtered navigation links
// split into Primary (top-bar) and Overflow ("More" dropdown) slices, plus flat Items.
type NavMenu struct {
	Items          []NavLink `json:"items"`
	Primary        []NavLink `json:"primary"`
	Overflow       []NavLink `json:"overflow"`
	OverflowActive bool      `json:"overflow_active"`
}

type builtinNavSpec struct {
	url       string
	i18nKey   string
	fallback  string
	icon      string
	exactPath bool
}

var builtinNavSpecs = map[string]builtinNavSpec{
	"home":       {url: "/", i18nKey: "nav.home", fallback: "Home", exactPath: true},
	"archives":   {url: "/archives", i18nKey: "nav.archives", fallback: "Archives"},
	"tags":       {url: "/tags", i18nKey: "nav.tags", fallback: "Tags"},
	"categories": {url: "/categories", i18nKey: "nav.categories", fallback: "Categories"},
	"series":     {url: "/series", i18nKey: "nav.series", fallback: "Series"},
	"books":      {url: "/books", i18nKey: "nav.bookshelf", fallback: "Bookshelf", icon: "book"},
	"links":      {url: "/links", i18nKey: "nav.links", fallback: "Links"},
	"follows":    {url: "/follows", i18nKey: "nav.follows", fallback: "Following"},
	"dynamic":    {url: "/dynamic", i18nKey: "nav.dynamic", fallback: "Stream"},
}

func parseNavItemKeys(cfg map[string]any) []string {
	if cfg == nil {
		return DefaultNavItemKeys
	}
	raw, exists := cfg["header_nav_items"]
	if !exists || raw == nil {
		return DefaultNavItemKeys
	}

	var candidates []string
	switch v := raw.(type) {
	case []string:
		candidates = v
	case []any:
		candidates = make([]string, 0, len(v))
		for _, elem := range v {
			if elem == nil {
				continue
			}
			candidates = append(candidates, strings.TrimSpace(fmt.Sprint(elem)))
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil
		}
		candidates = strings.Split(trimmed, ",")
	default:
		return DefaultNavItemKeys
	}

	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		k := strings.ToLower(strings.TrimSpace(c))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func parseNavPrimaryCount(cfg map[string]any) int {
	if cfg == nil {
		return DefaultNavPrimaryCount
	}
	raw, exists := cfg["header_nav_primary_count"]
	if !exists || raw == nil {
		return DefaultNavPrimaryCount
	}
	var n int
	switch v := raw.(type) {
	case int:
		n = v
	case int64:
		n = int(v)
	case float64:
		n = int(v)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return DefaultNavPrimaryCount
		}
		n = parsed
	default:
		return DefaultNavPrimaryCount
	}
	if n < 0 {
		return 0
	}
	return n
}

// ResolveNavMenu constructs a resolved NavMenu for `data` based on the active theme's
// `header_nav_items` and `header_nav_primary_count` configuration and custom `NavItems`.
func (e *Engine) ResolveNavMenu(data PageContext) NavMenu {
	var themeLocales map[string]map[string]string
	e.mu.RLock()
	if th, ok := e.themes[data.ThemeID]; ok && th != nil {
		themeLocales = th.locales
	}
	e.mu.RUnlock()

	tr := func(key, fallback string) string {
		if msg, ok := lookupThemeLocaleMessage(themeLocales, data.Locale, key); ok && msg != "" {
			return msg
		}
		if e.catalog != nil {
			if msg := e.catalog.T(data.Locale, key); msg != "" && msg != key {
				return msg
			}
		}
		return fallback
	}

	curPath := data.CurrentPath
	if curPath == "" {
		curPath = "/"
	}

	keys := parseNavItemKeys(data.ThemeConfig)
	items := make([]NavLink, 0, len(keys)+len(data.NavItems))
	for _, key := range keys {
		if key == "custom" {
			for _, it := range data.NavItems {
				u := strings.TrimSpace(it.URL)
				if u == "" || u == data.AdminPath || u == "/logout" {
					continue
				}
				items = append(items, NavLink{
					Key:          "custom:" + it.ID,
					Title:        it.Title,
					URL:          u,
					Icon:         it.Icon,
					OpenInNewTab: it.OpenInNewTab,
					IsActive:     curPath == u,
					IsCustom:     true,
				})
			}
			continue
		}
		spec, ok := builtinNavSpecs[key]
		if !ok {
			continue
		}
		active := false
		if spec.exactPath {
			active = curPath == spec.url
		} else {
			active = curPath == spec.url || strings.HasPrefix(curPath, spec.url+"/") || strings.HasPrefix(curPath, spec.url)
		}
		items = append(items, NavLink{
			Key:      key,
			Title:    tr(spec.i18nKey, spec.fallback),
			URL:      spec.url,
			Icon:     spec.icon,
			IsActive: active,
		})
	}

	primaryCount := parseNavPrimaryCount(data.ThemeConfig)
	var primary, overflow []NavLink
	if primaryCount >= len(items) {
		primary = items
	} else {
		primary = items[:primaryCount]
		overflow = items[primaryCount:]
	}

	overflowActive := false
	for _, ov := range overflow {
		if ov.IsActive {
			overflowActive = true
			break
		}
	}

	return NavMenu{
		Items:          items,
		Primary:        primary,
		Overflow:       overflow,
		OverflowActive: overflowActive,
	}
}
