package security

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

// GeoLocation represents the offline-resolved country ISO code and human-readable region label.
type GeoLocation struct {
	CountryISO string // e.g. "CN", "US"
	Region     string // e.g. "广东省·深圳市" or "California"
}

// LookupFunc allows injecting deterministic GeoIP lookups in unit tests without shipping 60MB DBs in git.
type LookupFunc func(addr netip.Addr) (GeoLocation, bool)

// GeoEngine provides zero-external-API offline IP geolocation using MaxMind MMDB + ip2region XDB cascade,
// ordered allow/deny rule evaluation, self-lockout protection, and emergency circuit-breaker support.
type GeoEngine struct {
	mu          sync.RWMutex
	mmdbReader  *maxminddb.Reader
	xdbSearcher *xdb.Searcher
	rules       []domain.SecurityGeoRule
	settingsMgr *repo.SettingsManager
	overrideFn  LookupFunc
}

func NewGeoEngine(geoIPDir string, settingsMgr *repo.SettingsManager) *GeoEngine {
	g := &GeoEngine{
		settingsMgr: settingsMgr,
	}
	if strings.TrimSpace(geoIPDir) != "" {
		_ = g.LoadDatabases(geoIPDir)
	}
	return g
}

// SetLookupOverride sets a custom lookup function (used in unit tests).
func (g *GeoEngine) SetLookupOverride(fn LookupFunc) {
	g.mu.Lock()
	g.overrideFn = fn
	g.mu.Unlock()
}

// LoadDatabases scans geoIPDir for any *.mmdb and *.xdb files and loads them into memory safely.
func (g *GeoEngine) LoadDatabases(geoIPDir string) error {
	entries, err := os.ReadDir(geoIPDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var newMMDB *maxminddb.Reader
	var newXDB *xdb.Searcher

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fullPath := filepath.Join(geoIPDir, e.Name())
		low := strings.ToLower(e.Name())
		if strings.HasSuffix(low, ".mmdb") && newMMDB == nil {
			if r, oErr := maxminddb.Open(fullPath); oErr == nil {
				newMMDB = r
			}
		} else if strings.HasSuffix(low, ".xdb") && newXDB == nil {
			if buf, rErr := xdb.LoadContentFromFile(fullPath); rErr == nil {
				if s, sErr := xdb.NewWithBuffer(xdb.IPv4, buf); sErr == nil {
					newXDB = s
				}
			}
		}
	}

	g.mu.Lock()
	oldMMDB := g.mmdbReader
	oldXDB := g.xdbSearcher
	g.mmdbReader = newMMDB
	g.xdbSearcher = newXDB
	g.mu.Unlock()

	if oldMMDB != nil {
		_ = oldMMDB.Close()
	}
	if oldXDB != nil {
		oldXDB.Close()
	}
	return nil
}

func (g *GeoEngine) Close() {
	g.mu.Lock()
	oldMMDB := g.mmdbReader
	oldXDB := g.xdbSearcher
	g.mmdbReader = nil
	g.xdbSearcher = nil
	g.mu.Unlock()
	if oldMMDB != nil {
		_ = oldMMDB.Close()
	}
	if oldXDB != nil {
		oldXDB.Close()
	}
}

// ReloadRules loads ordered security_geo_rules from the database.
func (g *GeoEngine) ReloadRules(ctx context.Context, store *sqlx.Store) error {
	if store == nil {
		return nil
	}
	rows, err := sqlx.Select[domain.SecurityGeoRule](ctx, store,
		`SELECT * FROM security_geo_rules ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return err
	}
	g.SetRules(rows)
	return nil
}

// SetRules updates the in-memory rule chain directly.
func (g *GeoEngine) SetRules(rules []domain.SecurityGeoRule) {
	cp := make([]domain.SecurityGeoRule, len(rules))
	copy(cp, rules)
	g.mu.Lock()
	g.rules = cp
	g.mu.Unlock()
}

// Lookup resolves CountryISO + Region for addr using overrideFn -> MMDB -> ip2region XDB cascade.
func (g *GeoEngine) Lookup(addr netip.Addr) (GeoLocation, bool) {
	if !addr.IsValid() {
		return GeoLocation{}, false
	}
	u := addr.Unmap()
	if u.IsLoopback() || u.IsPrivate() {
		return GeoLocation{CountryISO: "LOCAL", Region: "Local Network"}, true
	}

	g.mu.RLock()
	override := g.overrideFn
	mmdbR := g.mmdbReader
	xdbS := g.xdbSearcher
	g.mu.RUnlock()

	if override != nil {
		return override(u)
	}

	var out GeoLocation
	var found bool

	if mmdbR != nil {
		var rec struct {
			Country struct {
				ISOCode string            `maxminddb:"iso_code"`
				Names   map[string]string `maxminddb:"names"`
			} `maxminddb:"country"`
			Subdivisions []struct {
				Names map[string]string `maxminddb:"names"`
			} `maxminddb:"subdivisions"`
			City struct {
				Names map[string]string `maxminddb:"names"`
			} `maxminddb:"city"`
		}
		if err := mmdbR.Lookup(u).Decode(&rec); err == nil && rec.Country.ISOCode != "" {
			out.CountryISO = strings.ToUpper(rec.Country.ISOCode)
			var parts []string
			if len(rec.Subdivisions) > 0 {
				if n := firstMapVal(rec.Subdivisions[0].Names, "zh-CN", "en"); n != "" {
					parts = append(parts, n)
				}
			}
			if cName := firstMapVal(rec.City.Names, "zh-CN", "en"); cName != "" {
				parts = append(parts, cName)
			}
			if len(parts) == 0 {
				if cn := firstMapVal(rec.Country.Names, "zh-CN", "en"); cn != "" {
					parts = append(parts, cn)
				}
			}
			out.Region = strings.Join(parts, "·")
			found = true
		}
	}

	// Cascade into ip2region XDB for IPv4 precision (especially Chinese provinces/cities)
	if xdbS != nil && u.Is4() {
		if raw, err := xdbS.Search(u.String()); err == nil && raw != "" {
			// Format: Country|0|Province|City|ISP
			segs := strings.Split(raw, "|")
			if len(segs) >= 4 {
				country := cleanXDBSeg(segs[0])
				prov := cleanXDBSeg(segs[2])
				city := cleanXDBSeg(segs[3])
				if out.CountryISO == "" && (country == "中国" || strings.EqualFold(country, "China")) {
					out.CountryISO = "CN"
				}
				var rParts []string
				if prov != "" {
					rParts = append(rParts, prov)
				}
				if city != "" && city != prov {
					rParts = append(rParts, city)
				}
				if len(rParts) > 0 {
					out.Region = strings.Join(rParts, "·")
					found = true
				} else if country != "" && !found {
					out.Region = country
					found = true
				}
			}
		}
	}

	return out, found
}

// Allows evaluates whether addr is permitted by the active GeoIP ruleset.
// Private/loopback addresses are always allowed.
func (g *GeoEngine) Allows(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	u := addr.Unmap()
	if u.IsLoopback() || u.IsPrivate() {
		return true
	}

	defaultPolicy := "allow"
	if g.settingsMgr != nil {
		sec := g.settingsMgr.Snapshot().Security
		if !sec.GeoPolicyActive {
			return true
		}
		if strings.EqualFold(sec.GeoDefaultAction, "deny") {
			defaultPolicy = "deny"
		}
	}

	g.mu.RLock()
	rules := g.rules
	g.mu.RUnlock()

	return g.evaluateRules(u, rules, defaultPolicy)
}

// ValidateRulesAgainstAdminIP prevents an administrator from saving a GeoIP configuration
// that would immediately lock out their own current IP address!
func (g *GeoEngine) ValidateRulesAgainstAdminIP(rules []domain.SecurityGeoRule, defaultPolicy string, adminAddr netip.Addr) error {
	if !adminAddr.IsValid() || adminAddr.Unmap().IsLoopback() || adminAddr.Unmap().IsPrivate() {
		return nil
	}
	if !g.evaluateRules(adminAddr.Unmap(), rules, defaultPolicy) {
		return errors.New("security: refusing GeoIP rule update because it would lock out your current administrator IP")
	}
	return nil
}

func (g *GeoEngine) evaluateRules(addr netip.Addr, rules []domain.SecurityGeoRule, defaultPolicy string) bool {
	enabledCount := 0
	for _, r := range rules {
		if r.IsEnabled {
			enabledCount++
		}
	}
	if enabledCount == 0 {
		return defaultPolicy != "deny"
	}

	loc, ok := g.Lookup(addr)
	if !ok {
		return defaultPolicy != "deny"
	}

	for _, rule := range rules {
		if !rule.IsEnabled {
			continue
		}
		if matchesGeoRule(loc, rule) {
			return !strings.EqualFold(rule.Action, "deny")
		}
		switch strings.ToLower(strings.TrimSpace(rule.OnMiss)) {
		case "allow":
			return true
		case "deny":
			return false
		default:
			// "next" -> continue evaluating chain
		}
	}
	return defaultPolicy != "deny"
}

func matchesGeoRule(loc GeoLocation, rule domain.SecurityGeoRule) bool {
	countryMatched := false
	hasCountryCriteria := strings.TrimSpace(rule.Countries) != ""
	if hasCountryCriteria {
		for _, c := range strings.Split(rule.Countries, ",") {
			if strings.EqualFold(strings.TrimSpace(c), loc.CountryISO) && loc.CountryISO != "" {
				countryMatched = true
				break
			}
		}
	}

	regionMatched := false
	hasRegionCriteria := strings.TrimSpace(rule.Regions) != ""
	if hasRegionCriteria {
		lowReg := strings.ToLower(loc.Region)
		for _, sub := range strings.Split(rule.Regions, ",") {
			s := strings.ToLower(strings.TrimSpace(sub))
			if s != "" && strings.Contains(lowReg, s) {
				regionMatched = true
				break
			}
		}
	}

	if hasCountryCriteria && hasRegionCriteria {
		return countryMatched && regionMatched
	}
	if hasCountryCriteria {
		return countryMatched
	}
	if hasRegionCriteria {
		return regionMatched
	}
	return false
}

func firstMapVal(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}

func cleanXDBSeg(s string) string {
	s = strings.TrimSpace(s)
	if s == "0" {
		return ""
	}
	return fmt.Sprint(s)
}
