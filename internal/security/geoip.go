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
	xdbMu       sync.Mutex
	mmdbReader  *mmdbReader
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

// LoadDatabases scans geoIPDir for any *.mmdb and *.xdb files and loads them safely.
// For ip2region (*.xdb), only the ~512 KiB vector index is cached in RAM rather than buffering the full ~11.5 MiB file.
func (g *GeoEngine) LoadDatabases(geoIPDir string) error {
	entries, err := os.ReadDir(geoIPDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var newMMDB *mmdbReader
	var newXDB *xdb.Searcher

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fullPath := filepath.Join(geoIPDir, e.Name())
		low := strings.ToLower(e.Name())
		if strings.HasSuffix(low, ".mmdb") && newMMDB == nil {
			if r, oErr := openMMDBFile(fullPath); oErr == nil {
				newMMDB = r
			}
		} else if strings.HasSuffix(low, ".xdb") && newXDB == nil {
			if vIndex, rErr := xdb.LoadVectorIndexFromFile(fullPath); rErr == nil {
				if s, sErr := xdb.NewWithVectorIndex(xdb.IPv4, fullPath, vIndex); sErr == nil {
					newXDB = s
				}
			}
		}
	}

	g.mu.Lock()
	oldMMDB := g.mmdbReader
	g.mmdbReader = newMMDB
	g.mu.Unlock()

	g.xdbMu.Lock()
	oldXDB := g.xdbSearcher
	g.xdbSearcher = newXDB
	g.xdbMu.Unlock()

	if oldMMDB != nil {
		_ = oldMMDB.Close()
	}
	if oldXDB != nil {
		oldXDB.Close()
	}
	return nil
}

func (g *GeoEngine) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	oldMMDB := g.mmdbReader
	g.mmdbReader = nil
	g.mu.Unlock()

	g.xdbMu.Lock()
	oldXDB := g.xdbSearcher
	g.xdbSearcher = nil
	g.xdbMu.Unlock()

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

// LookupDisplay returns a concise human-readable geolocation label (Region or CountryISO), or "" if unknown.
func (g *GeoEngine) LookupDisplay(addr netip.Addr) string {
	loc, ok := g.Lookup(addr)
	if !ok {
		return ""
	}
	if loc.Region != "" {
		return loc.Region
	}
	return loc.CountryISO
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
	if override != nil {
		g.mu.RUnlock()
		return override(u)
	}

	var out GeoLocation
	var found bool

	if g.mmdbReader != nil {
		if rec, ok, err := g.mmdbReader.lookupGeo(u); err == nil && ok {
			iso := strings.TrimSpace(rec.CountryISO)
			if iso == "" {
				iso = strings.TrimSpace(rec.RegCountryISO)
			}
			if iso != "" {
				out.CountryISO = strings.ToUpper(iso)
				var parts []string
				if n := firstGeoStr(rec.SubdivisionZH, rec.SubdivisionEN); n != "" {
					parts = append(parts, n)
				}
				if cName := firstGeoStr(rec.CityZH, rec.CityEN); cName != "" {
					parts = append(parts, cName)
				}
				if len(parts) == 0 {
					if cn := firstGeoStr(rec.CountryZH, rec.CountryEN, rec.RegCountryZH, rec.RegCountryEN); cn != "" {
						parts = append(parts, cn)
					}
				}
				out.Region = strings.Join(parts, "·")
				found = true
			}
		}
	}
	g.mu.RUnlock()

	// Cascade into ip2region XDB for IPv4 precision (especially Chinese provinces/cities).
	// Searcher.Search mutates internal ioCount and seeks its underlying file descriptor, so serialize via xdbMu.
	if u.Is4() {
		var raw string
		var xErr error
		g.xdbMu.Lock()
		if g.xdbSearcher != nil {
			raw, xErr = g.xdbSearcher.Search(u.String())
		}
		g.xdbMu.Unlock()

		if xErr == nil && raw != "" {
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

func firstGeoStr(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
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
