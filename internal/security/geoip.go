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

// GeoDBStatus describes the currently mounted offline GeoIP databases and any skipped files.
type GeoDBStatus struct {
	Path         string   `json:"path"`
	ActiveFiles  []string `json:"active_files"`
	SkippedFiles []string `json:"skipped_files,omitempty"`
}

// GeoEngine provides zero-external-API offline IP geolocation using MaxMind MMDB + ip2region XDB (IPv4 & IPv6) cascade,
// ordered allow/deny rule evaluation, self-lockout protection, and atomic hot-reload support.
type GeoEngine struct {
	mu          sync.RWMutex
	mmdbReader  *mmdbReader
	xdbV4       *xdbReader
	xdbV6       *xdbReader
	status      GeoDBStatus
	rules       []domain.SecurityGeoRule
	settingsMgr *repo.SettingsManager
	overrideFn  LookupFunc
}

func NewGeoEngine(geoIPDir string, settingsMgr *repo.SettingsManager) *GeoEngine {
	g := &GeoEngine{
		settingsMgr: settingsMgr,
		status:      GeoDBStatus{ActiveFiles: []string{}},
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

// Status returns a snapshot of the currently mounted offline GeoIP databases and skipped candidates.
func (g *GeoEngine) Status() GeoDBStatus {
	if g == nil {
		return GeoDBStatus{ActiveFiles: []string{}}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	st := GeoDBStatus{
		Path:        g.status.Path,
		ActiveFiles: make([]string, len(g.status.ActiveFiles)),
	}
	copy(st.ActiveFiles, g.status.ActiveFiles)
	if len(g.status.SkippedFiles) > 0 {
		st.SkippedFiles = make([]string, len(g.status.SkippedFiles))
		copy(st.SkippedFiles, g.status.SkippedFiles)
	}
	return st
}

// InspectGeoIPPath probes a directory or .mmdb/.xdb file path without keeping file descriptors open,
// returning the exact active and skipped database summary used by both hot-reload and volo doctor.
func InspectGeoIPPath(geoIPPath string) (GeoDBStatus, error) {
	tmp := &GeoEngine{status: GeoDBStatus{ActiveFiles: []string{}}}
	err := tmp.LoadDatabases(geoIPPath)
	st := tmp.Status()
	tmp.Close()
	return st, err
}

type mmdbCandidate struct {
	name       string
	reader     *mmdbReader
	tierRank   int
	ipVersion  uint16
	buildEpoch uint64
}

type xdbCandidate struct {
	name      string
	reader    *xdbReader
	createdAt uint32
}

// mmdbTierRank classifies MMDB metadata database_type (with filename fallback when database_type is generic).
// Returns 0 for non-geographic MMDBs (e.g. GeoLite2-ASN, ISP, Domain, Anonymous-IP) so they never shadow City/Country DBs.
func mmdbTierRank(dbType, fileName string) int {
	lowType := strings.ToLower(strings.TrimSpace(dbType))
	lowName := strings.ToLower(strings.TrimSpace(fileName))
	hasGeoType := strings.Contains(lowType, "city") || strings.Contains(lowType, "country") || strings.Contains(lowType, "enterprise")
	if !hasGeoType {
		for _, nonGeo := range []string{"asn", "isp", "domain", "connection-type", "connection_type", "anonymous"} {
			if strings.Contains(lowType, nonGeo) || (lowType == "" && strings.Contains(lowName, nonGeo)) {
				return 0
			}
		}
	}
	switch {
	case strings.Contains(lowType, "enterprise") || (strings.Contains(lowType, "geoip2") && strings.Contains(lowType, "city")):
		return 40
	case strings.Contains(lowType, "city") || (lowType == "" && strings.Contains(lowName, "city")):
		return 30
	case strings.Contains(lowType, "geoip2") && strings.Contains(lowType, "country"):
		return 20
	case strings.Contains(lowType, "country") || (lowType == "" && strings.Contains(lowName, "country")):
		return 15
	default:
		return 10
	}
}

func isBetterMMDB(cand, cur *mmdbCandidate) bool {
	if cur == nil {
		return true
	}
	if cand.tierRank != cur.tierRank {
		return cand.tierRank > cur.tierRank
	}
	if cand.ipVersion != cur.ipVersion {
		return cand.ipVersion > cur.ipVersion
	}
	return cand.buildEpoch > cur.buildEpoch
}

func formatActiveMMDBLabel(name, dbType string) string {
	t := strings.TrimSpace(dbType)
	if t == "" || strings.Contains(strings.ToLower(name), strings.ToLower(t)) {
		return name
	}
	return fmt.Sprintf("%s [%s]", name, t)
}

func formatActiveXDBLabel(name, verLabel string) string {
	low := strings.ToLower(name)
	if (verLabel == "IPv4" && (strings.Contains(low, "v4") || strings.Contains(low, "ipv4"))) ||
		(verLabel == "IPv6" && (strings.Contains(low, "v6") || strings.Contains(low, "ipv6"))) {
		return name
	}
	return fmt.Sprintf("%s [%s]", name, verLabel)
}

// LoadDatabases scans geoIPPath (either a directory or a direct *.mmdb / *.xdb file) and atomically hot-swaps readers:
//   - MMDB: ignores non-geographic DBs (such as ASN) and selects the highest-precision / newest DB (GeoIP2-City > GeoLite2-City > Country).
//   - ip2region XDB: mounts independent IPv4 and IPv6 readers simultaneously (caching only ~512 KiB vector index per file).
func (g *GeoEngine) LoadDatabases(geoIPPath string) error {
	cleanPath := strings.TrimSpace(geoIPPath)
	if cleanPath == "" {
		g.swapDatabases("", nil, nil, nil, nil, nil)
		return nil
	}

	info, stErr := os.Stat(cleanPath)
	if stErr != nil {
		if os.IsNotExist(stErr) {
			g.swapDatabases(cleanPath, nil, nil, nil, nil, nil)
			return nil
		}
		return stErr
	}

	type fileTarget struct {
		name string
		path string
	}
	var targets []fileTarget
	if !info.IsDir() {
		targets = append(targets, fileTarget{name: filepath.Base(cleanPath), path: cleanPath})
	} else {
		entries, err := os.ReadDir(cleanPath)
		if err != nil {
			if os.IsNotExist(err) {
				g.swapDatabases(cleanPath, nil, nil, nil, nil, nil)
				return nil
			}
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			full := filepath.Join(cleanPath, e.Name())
			if st, sErr := os.Stat(full); sErr != nil || st.IsDir() {
				continue
			}
			targets = append(targets, fileTarget{
				name: e.Name(),
				path: full,
			})
		}
	}

	var bestMMDB *mmdbCandidate
	var bestXDB4 *xdbCandidate
	var bestXDB6 *xdbCandidate
	var skipped []string

	for _, t := range targets {
		low := strings.ToLower(t.name)
		switch {
		case strings.HasSuffix(low, ".mmdb"):
			r, oErr := openMMDBFile(t.path)
			if oErr != nil {
				skipped = append(skipped, fmt.Sprintf("%s (invalid)", t.name))
				continue
			}
			rank := mmdbTierRank(r.databaseType, t.name)
			if rank <= 0 {
				reason := strings.TrimSpace(r.databaseType)
				if reason == "" {
					reason = "non-geo"
				}
				_ = r.Close()
				skipped = append(skipped, fmt.Sprintf("%s (%s)", t.name, reason))
				continue
			}
			cand := &mmdbCandidate{
				name:       t.name,
				reader:     r,
				tierRank:   rank,
				ipVersion:  r.ipVersion,
				buildEpoch: r.buildEpoch,
			}
			if isBetterMMDB(cand, bestMMDB) {
				if bestMMDB != nil {
					_ = bestMMDB.reader.Close()
					skipped = append(skipped, fmt.Sprintf("%s (superseded)", bestMMDB.name))
				}
				bestMMDB = cand
			} else {
				_ = r.Close()
				skipped = append(skipped, fmt.Sprintf("%s (superseded)", t.name))
			}

		case strings.HasSuffix(low, ".xdb"):
			xr, xErr := openXDBFile(t.path)
			if xErr != nil {
				skipped = append(skipped, fmt.Sprintf("%s (invalid)", t.name))
				continue
			}
			cand := &xdbCandidate{
				name:      t.name,
				reader:    xr,
				createdAt: xr.createdAt,
			}
			if xr.ipVersion == xdbIPv4Ver {
				if bestXDB4 == nil || cand.createdAt > bestXDB4.createdAt {
					if bestXDB4 != nil {
						_ = bestXDB4.reader.Close()
						skipped = append(skipped, fmt.Sprintf("%s (superseded)", bestXDB4.name))
					}
					bestXDB4 = cand
				} else {
					_ = xr.Close()
					skipped = append(skipped, fmt.Sprintf("%s (superseded)", t.name))
				}
			} else {
				if bestXDB6 == nil || cand.createdAt > bestXDB6.createdAt {
					if bestXDB6 != nil {
						_ = bestXDB6.reader.Close()
						skipped = append(skipped, fmt.Sprintf("%s (superseded)", bestXDB6.name))
					}
					bestXDB6 = cand
				} else {
					_ = xr.Close()
					skipped = append(skipped, fmt.Sprintf("%s (superseded)", t.name))
				}
			}
		}
	}

	active := make([]string, 0, 3)
	var newMMDB *mmdbReader
	var newXDB4 *xdbReader
	var newXDB6 *xdbReader
	if bestMMDB != nil {
		newMMDB = bestMMDB.reader
		active = append(active, formatActiveMMDBLabel(bestMMDB.name, bestMMDB.reader.databaseType))
	}
	if bestXDB4 != nil {
		newXDB4 = bestXDB4.reader
		active = append(active, formatActiveXDBLabel(bestXDB4.name, "IPv4"))
	}
	if bestXDB6 != nil {
		newXDB6 = bestXDB6.reader
		active = append(active, formatActiveXDBLabel(bestXDB6.name, "IPv6"))
	}

	g.swapDatabases(cleanPath, newMMDB, newXDB4, newXDB6, active, skipped)
	return nil
}

func (g *GeoEngine) swapDatabases(path string, newMMDB *mmdbReader, newXDB4, newXDB6 *xdbReader, active, skipped []string) {
	if active == nil {
		active = []string{}
	}
	g.mu.Lock()
	oldMMDB := g.mmdbReader
	oldXDB4 := g.xdbV4
	oldXDB6 := g.xdbV6
	g.mmdbReader = newMMDB
	g.xdbV4 = newXDB4
	g.xdbV6 = newXDB6
	g.status = GeoDBStatus{
		Path:         path,
		ActiveFiles:  active,
		SkippedFiles: skipped,
	}
	g.mu.Unlock()

	if oldMMDB != nil {
		_ = oldMMDB.Close()
	}
	if oldXDB4 != nil {
		_ = oldXDB4.Close()
	}
	if oldXDB6 != nil {
		_ = oldXDB6.Close()
	}
}

func (g *GeoEngine) Close() {
	if g == nil {
		return
	}
	g.swapDatabases("", nil, nil, nil, nil, nil)
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

// Lookup resolves CountryISO + Region for addr using overrideFn -> MMDB -> ip2region XDB (IPv4 / IPv6) cascade.
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

	// Cascade into ip2region XDB (IPv4 or IPv6 slot) for fine-grained province/city precision.
	// xdbReader.lookup uses stateless io.ReaderAt (pread), so RLock allows full concurrency.
	var raw string
	var xErr error
	if u.Is4() && g.xdbV4 != nil {
		raw, xErr = g.xdbV4.lookup(u)
	} else if u.Is6() && g.xdbV6 != nil {
		raw, xErr = g.xdbV6.lookup(u)
	}
	g.mu.RUnlock()

	if xErr == nil && raw != "" {
		// Standard 5-segment format: Country|Region|Province|City|ISP
		segs := strings.Split(raw, "|")
		if len(segs) >= 4 {
			country := cleanXDBSeg(segs[0])
			prov := cleanXDBSeg(segs[2])
			if prov == "" {
				prov = cleanXDBSeg(segs[1])
			}
			city := cleanXDBSeg(segs[3])
			if out.CountryISO == "" {
				if country == "中国" || strings.EqualFold(country, "China") {
					out.CountryISO = "CN"
				} else if len(country) == 2 && isASCIIAlpha(country[0]) && isASCIIAlpha(country[1]) {
					out.CountryISO = strings.ToUpper(country)
				}
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
	return s
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

