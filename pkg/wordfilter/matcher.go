package wordfilter

import (
	"html"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
)

const (
	// MaxWordRunes caps the maximum UTF-16 unit / rune length of a single deny or allow keyword
	// so terminal metadata packs into 7 bits (1..127) inside each 16-byte acState.
	MaxWordRunes = 120
	// MaxTotalWords caps the total number of patterns compiled into a single Matcher.
	MaxTotalWords = 120_000

	maxCategories = 64
)

const (
	wordFlagLeadASCII  uint8 = 1 << 0
	wordFlagTrailASCII uint8 = 1 << 1
	wordFlagHasCJK     uint8 = 1 << 2
)

// RuleGroup associates a slice of deny keywords with a named category (e.g. "porn", "gambling").
type RuleGroup struct {
	Category string
	Words    []string
}

// Options configures compilation of an immutable Matcher.
type Options struct {
	Groups           []RuleGroup
	Deny             []string
	Allow            []string
	IgnoreSeparators bool
	IgnoreRepeat     bool
}

// BuilderOptions configures incremental streaming construction of a Matcher.
type BuilderOptions struct {
	IgnoreSeparators bool
	IgnoreRepeat     bool
}

// MatchResult describes a matched forbidden keyword.
type MatchResult struct {
	Matched  bool   `json:"matched"`
	Word     string `json:"word,omitempty"`
	Category string `json:"category,omitempty"`
}

// acState is a 16-byte pointer-free Aho-Corasick state in BFS-CSR layout (zero GC scan overhead).
// Children of state u occupy states[states[u].childStart : states[u+1].childStart], sorted by ch.
type acState struct {
	childStart uint32
	fail       uint32
	dictSuffix uint32
	ch         uint16
	meta       uint16 // 0 if non-terminal; bits[0:7]=unitLen(1..127), bits[7:10]=flags, bits[10:16]=catIdx(0..63)
}

func packStateMeta(unitLen uint8, flags uint8, catIdx uint8) uint16 {
	return uint16(unitLen&0x7f) | (uint16(flags&0x07) << 7) | (uint16(catIdx&0x3f) << 10)
}

type automaton struct {
	states      []acState
	rootASCII   [128]uint32
	rootBitmap  [1024]uint64
	rootBuckets [257]uint16
}

type runeSpan struct {
	start int
	end   int
}

// Matcher is an immutable, concurrency-safe, zero-GC-pointer BFS-CSR Aho-Corasick filter
// with Traditional-to-Simplified Hanzi folding, styled alphanumeric normalization, Markdown/HTML
// visible-text unwrapping, ASCII word-boundary protection, CJK separator-piercing, optional
// CJK repeat collapsing, and span-safe allowlists.
type Matcher struct {
	deny             *automaton
	allow            *automaton
	categories       []string
	wordCount        int
	allowCount       int
	hasCJKMulti      bool
	ignoreSeparators bool
	ignoreRepeat     bool
}

type patEntry struct {
	offset  uint32
	order   uint32
	unitLen uint8
	catIdx  uint8
	flags   uint8
}

// Builder accumulates normalized patterns into flat pointer-free buffers before compiling
// an immutable Matcher without allocating per-word string headers or hash maps.
type Builder struct {
	opts         BuilderOptions
	catMap       map[string]uint8
	categories   []string
	denyUnits    []uint16
	denyEntries  []patEntry
	allowUnits   []uint16
	allowEntries []patEntry
	scratchUnits []uint16
	hasCJKMulti  bool
}

// NewBuilder creates a streaming Matcher builder.
func NewBuilder(opts BuilderOptions) *Builder {
	return &Builder{
		opts:         opts,
		catMap:       make(map[string]uint8, 8),
		scratchUnits: make([]uint16, 0, MaxWordRunes),
	}
}

// Reserve pre-sizes flat pattern arenas when the approximate dictionary size is known
// ahead of streaming, avoiding repeated slice-doubling allocations during compilation.
func (b *Builder) Reserve(denyWords int, allowWords int) {
	if denyWords > MaxTotalWords {
		denyWords = MaxTotalWords
	}
	if allowWords > MaxTotalWords {
		allowWords = MaxTotalWords
	}
	if denyWords > cap(b.denyEntries) {
		nextEntries := make([]patEntry, len(b.denyEntries), denyWords)
		copy(nextEntries, b.denyEntries)
		b.denyEntries = nextEntries
		unitCap := denyWords * 4
		if unitCap > cap(b.denyUnits) {
			nextUnits := make([]uint16, len(b.denyUnits), unitCap)
			copy(nextUnits, b.denyUnits)
			b.denyUnits = nextUnits
		}
	}
	if allowWords > cap(b.allowEntries) {
		nextEntries := make([]patEntry, len(b.allowEntries), allowWords)
		copy(nextEntries, b.allowEntries)
		b.allowEntries = nextEntries
		unitCap := allowWords * 4
		if unitCap > cap(b.allowUnits) {
			nextUnits := make([]uint16, len(b.allowUnits), unitCap)
			copy(nextUnits, b.allowUnits)
			b.allowUnits = nextUnits
		}
	}
}

func (b *Builder) categoryIndex(cat string) uint8 {
	c := strings.TrimSpace(cat)
	if idx, ok := b.catMap[c]; ok {
		return idx
	}
	if len(b.categories) >= maxCategories {
		return uint8(maxCategories - 1)
	}
	idx := uint8(len(b.categories))
	b.categories = append(b.categories, c)
	b.catMap[c] = idx
	return idx
}

// AddDeny normalizes and appends a single deny pattern under category.
func (b *Builder) AddDeny(rawWord string, category string) bool {
	if len(b.denyEntries) >= MaxTotalWords {
		return false
	}
	disp := strings.TrimSpace(rawWord)
	if disp == "" {
		return false
	}
	units := b.normalizeIntoScratch(disp)
	if len(units) == 0 || len(units) > MaxWordRunes {
		return false
	}

	var flags uint8
	if isASCIIAlphaNum16(units[0]) {
		flags |= wordFlagLeadASCII
	}
	if isASCIIAlphaNum16(units[len(units)-1]) {
		flags |= wordFlagTrailASCII
	}
	cjkRunes := 0
	for i := 0; i < len(units); i++ {
		u := units[i]
		var r rune
		if u >= 0xd800 && u <= 0xdbff && i+1 < len(units) && units[i+1] >= 0xdc00 && units[i+1] <= 0xdfff {
			r = utf16.DecodeRune(rune(u), rune(units[i+1]))
			i++
		} else {
			r = rune(u)
		}
		if isCJKIdeograph(r) {
			flags |= wordFlagHasCJK
			cjkRunes++
		}
	}
	if (flags&wordFlagHasCJK) != 0 && len(units) >= 2 && (cjkRunes >= 1) {
		b.hasCJKMulti = true
	}

	off := uint32(len(b.denyUnits))
	b.denyUnits = append(b.denyUnits, units...)
	b.denyEntries = append(b.denyEntries, patEntry{
		offset:  off,
		order:   uint32(len(b.denyEntries)),
		unitLen: uint8(len(units)),
		catIdx:  b.categoryIndex(category),
		flags:   flags,
	})
	return true
}

// AddAllow normalizes and appends a single allowlist exemption phrase.
func (b *Builder) AddAllow(rawAllow string) bool {
	if len(b.allowEntries) >= MaxTotalWords {
		return false
	}
	disp := strings.TrimSpace(rawAllow)
	if disp == "" {
		return false
	}
	units := b.normalizeIntoScratch(disp)
	if len(units) == 0 || len(units) > MaxWordRunes {
		return false
	}
	off := uint32(len(b.allowUnits))
	b.allowUnits = append(b.allowUnits, units...)
	b.allowEntries = append(b.allowEntries, patEntry{
		offset:  off,
		order:   uint32(len(b.allowEntries)),
		unitLen: uint8(len(units)),
	})
	return true
}

// AddWordLines parses newline/comma/semicolon delimited word lines (supporting '!' and '@allow:'
// prefixes) directly into the builder without intermediate slice allocations per line.
func (b *Builder) AddWordLines(raw string, category string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	rem := raw
	for len(rem) > 0 {
		var line string
		if nl := strings.IndexByte(rem, '\n'); nl >= 0 {
			line = rem[:nl]
			rem = rem[nl+1:]
		} else {
			line = rem
			rem = ""
		}
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		trimmed = strings.TrimPrefix(trimmed, "\ufeff")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		b.addTokenizedLine(trimmed, category)
	}
}

func (b *Builder) addTokenizedLine(trimmedLine string, category string) {
	if !strings.ContainsAny(trimmedLine, ",，;；") {
		b.addSingleToken(trimmedLine, category)
		return
	}
	start := 0
	for idx, r := range trimmedLine {
		if r == ',' || r == '，' || r == ';' || r == '；' {
			b.addSingleToken(trimmedLine[start:idx], category)
			start = idx + len(string(r))
		}
	}
	if start < len(trimmedLine) {
		b.addSingleToken(trimmedLine[start:], category)
	}
}

func (b *Builder) addSingleToken(tok string, category string) {
	t := strings.TrimSpace(tok)
	if t == "" {
		return
	}
	isAllow := false
	if strings.HasPrefix(t, "!") && len(t) > 1 {
		isAllow = true
		t = strings.TrimSpace(t[1:])
	} else if len(t) > len("@allow:") && strings.EqualFold(t[:len("@allow:")], "@allow:") {
		isAllow = true
		t = strings.TrimSpace(t[len("@allow:"):])
	}
	if t == "" {
		return
	}
	if isAllow {
		b.AddAllow(t)
	} else {
		b.AddDeny(t, category)
	}
}

func (b *Builder) normalizeIntoScratch(s string) []uint16 {
	u := unescapeHTMLBounded(s)
	b.scratchUnits = appendNormalizedUnits(b.scratchUnits[:0], u, MaxWordRunes+1)
	return b.scratchUnits
}

// Build finalizes and returns an immutable zero-GC-pointer Matcher.
func (b *Builder) Build() *Matcher {
	m := &Matcher{
		categories:       append([]string(nil), b.categories...),
		hasCJKMulti:      b.hasCJKMulti,
		ignoreSeparators: b.opts.IgnoreSeparators,
		ignoreRepeat:     b.opts.IgnoreRepeat,
	}

	if len(b.denyEntries) > 0 {
		dedupDeny, exactDenyStates := sortAndDeduplicateEntries(b.denyEntries, b.denyUnits)
		m.deny = buildCSRAutomaton(dedupDeny, b.denyUnits, exactDenyStates)
		m.wordCount = len(dedupDeny)
	}
	if len(b.allowEntries) > 0 {
		dedupAllow, exactAllowStates := sortAndDeduplicateEntries(b.allowEntries, b.allowUnits)
		m.allow = buildCSRAutomaton(dedupAllow, b.allowUnits, exactAllowStates)
		m.allowCount = len(dedupAllow)
	}
	return m
}

// ParseWordLines splits newline, comma, Chinese comma, or semicolon delimited word lists,
// ignoring blank lines and '#' / '//' comments. Entries prefixed with '!' or '@allow:' are
// returned in the allow slice; all other entries are returned in deny.
func ParseWordLines(raw string) (deny []string, allow []string) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	for _, line := range strings.Split(raw, "\n") {
		trimmedLine := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		trimmedLine = strings.TrimPrefix(trimmedLine, "\ufeff")
		if trimmedLine == "" || strings.HasPrefix(trimmedLine, "#") || strings.HasPrefix(trimmedLine, "//") {
			continue
		}
		tokens := splitTokens(trimmedLine)
		for _, tok := range tokens {
			t := strings.TrimSpace(tok)
			if t == "" {
				continue
			}
			isAllow := false
			if strings.HasPrefix(t, "!") && len(t) > 1 {
				isAllow = true
				t = strings.TrimSpace(t[1:])
			} else if strings.HasPrefix(strings.ToLower(t), "@allow:") {
				isAllow = true
				t = strings.TrimSpace(t[len("@allow:"):])
			}
			if t == "" || runeCountBounded(t, MaxWordRunes+1) > MaxWordRunes {
				continue
			}
			if isAllow {
				allow = append(allow, t)
			} else {
				deny = append(deny, t)
			}
		}
	}
	return deny, allow
}

func splitTokens(line string) []string {
	if !strings.ContainsAny(line, ",，;；") {
		return []string{line}
	}
	return strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；'
	})
}

func runeCountBounded(s string, limit int) int {
	n := 0
	for range s {
		n++
		if n >= limit {
			return n
		}
	}
	return n
}

// Compile builds an immutable Matcher from opts.
func Compile(opts Options) *Matcher {
	b := NewBuilder(BuilderOptions{
		IgnoreSeparators: opts.IgnoreSeparators,
		IgnoreRepeat:     opts.IgnoreRepeat,
	})
	for _, grp := range opts.Groups {
		for _, w := range grp.Words {
			b.AddDeny(w, grp.Category)
		}
	}
	for _, w := range opts.Deny {
		b.AddDeny(w, "custom")
	}
	for _, a := range opts.Allow {
		b.AddAllow(a)
	}
	return b.Build()
}

// WordCount returns the number of active deduplicated deny patterns.
func (m *Matcher) WordCount() int {
	if m == nil {
		return 0
	}
	return m.wordCount
}

// AllowCount returns the number of active deduplicated allowlist patterns.
func (m *Matcher) AllowCount() int {
	if m == nil {
		return 0
	}
	return m.allowCount
}

// Contains reports whether text matches any deny pattern (after allowlist exemptions).
func (m *Matcher) Contains(text string) bool {
	return m.Match(text).Matched
}

type scanScratch struct {
	norm      []uint16
	visNorm   []uint16
	compacted []uint16
	origStart []int32
	origEnd   []int32
	spans     []runeSpan
}

const maxPooledScratchCap = 8192

var scratchPool = sync.Pool{
	New: func() any {
		return &scanScratch{
			norm:      make([]uint16, 0, 512),
			visNorm:   make([]uint16, 0, 512),
			compacted: make([]uint16, 0, 512),
			origStart: make([]int32, 0, 512),
			origEnd:   make([]int32, 0, 512),
			spans:     make([]runeSpan, 0, 8),
		}
	},
}

func acquireScratch() *scanScratch {
	sc := scratchPool.Get().(*scanScratch)
	sc.norm = sc.norm[:0]
	sc.visNorm = sc.visNorm[:0]
	sc.compacted = sc.compacted[:0]
	sc.origStart = sc.origStart[:0]
	sc.origEnd = sc.origEnd[:0]
	sc.spans = sc.spans[:0]
	return sc
}

func releaseScratch(sc *scanScratch) {
	if cap(sc.norm) > maxPooledScratchCap || cap(sc.visNorm) > maxPooledScratchCap ||
		cap(sc.compacted) > maxPooledScratchCap || cap(sc.origStart) > maxPooledScratchCap ||
		cap(sc.origEnd) > maxPooledScratchCap {
		return
	}
	scratchPool.Put(sc)
}

// Match evaluates text against the compiled automaton across normalized source text,
// unwrapped visible Markdown/HTML text, CJK separator-piercing text, and optional CJK repeat collapsing.
func (m *Matcher) Match(text string) MatchResult {
	if m == nil || m.wordCount == 0 || m.deny == nil || strings.TrimSpace(text) == "" {
		return MatchResult{}
	}

	sc := acquireScratch()
	defer releaseScratch(sc)

	unescaped := unescapeHTMLBounded(text)

	// Pass 1: Normalized source text (catches direct matches, Traditional Hanzi, full-width/styled/zero-width obfuscation).
	sc.norm = appendNormalizedUnits(sc.norm[:0], unescaped, 0)
	if len(sc.norm) > 0 {
		if word, cIdx, ok := m.scanNormalized(sc.norm, sc); ok {
			return m.makeResult(word, cIdx)
		}
	}

	// Pass 2: Visible Markdown/HTML text (catches words split by **bold**, `code`, [link](url), or inline HTML tags).
	if hasMarkdownOrHTMLMarkup(unescaped) {
		if vis, changed := extractVisibleMarkdown(unescaped); changed {
			sc.visNorm = appendNormalizedUnits(sc.visNorm[:0], vis, 0)
			if len(sc.visNorm) > 0 {
				if word, cIdx, ok := m.scanNormalized(sc.visNorm, sc); ok {
					return m.makeResult(word, cIdx)
				}
				if m.ignoreSeparators && m.hasCJKMulti {
					if word, cIdx, ok := m.scanCompactedCJK(sc.visNorm, sc); ok {
						return m.makeResult(word, cIdx)
					}
				}
				if m.ignoreRepeat && m.hasCJKMulti {
					if word, cIdx, ok := m.scanCollapsedRepeatCJK(sc.visNorm, sc); ok {
						return m.makeResult(word, cIdx)
					}
				}
			}
		}
	}

	// Pass 3: CJK separator-piercing scan (catches "傻*逼", "加.微.信", "赌 博", etc.).
	if m.ignoreSeparators && m.hasCJKMulti && len(sc.norm) >= 2 {
		if word, cIdx, ok := m.scanCompactedCJK(sc.norm, sc); ok {
			return m.makeResult(word, cIdx)
		}
	}

	// Pass 4: Optional CJK consecutive-repeat collapsing (catches "傻傻傻逼逼" -> "傻逼" without mangling English/numbers).
	if m.ignoreRepeat && m.hasCJKMulti && len(sc.norm) >= 2 {
		if word, cIdx, ok := m.scanCollapsedRepeatCJK(sc.norm, sc); ok {
			return m.makeResult(word, cIdx)
		}
	}

	return MatchResult{}
}

func (m *Matcher) makeResult(word string, catIdx uint8) MatchResult {
	res := MatchResult{
		Matched: true,
		Word:    word,
	}
	if int(catIdx) < len(m.categories) {
		res.Category = m.categories[catIdx]
	}
	return res
}

func (m *Matcher) scanNormalized(units []uint16, sc *scanScratch) (string, uint8, bool) {
	var allowSpans []runeSpan
	allowComputed := false

	states := m.deny.states
	var state uint32
	for i, ch := range units {
		state = m.deny.step(state, ch)
		if state == 0 {
			continue
		}
		endPos := i + 1
		for out := state; out != 0; out = states[out].dictSuffix {
			meta := states[out].meta
			if meta == 0 {
				continue
			}
			uLen := int(meta & 0x7f)
			startPos := endPos - uLen
			if startPos < 0 {
				continue
			}
			flags := uint8((meta >> 7) & 0x07)
			if (flags&wordFlagLeadASCII) != 0 && startPos > 0 && isASCIIAlphaNum16(units[startPos-1]) {
				continue
			}
			if (flags&wordFlagTrailASCII) != 0 && endPos < len(units) && isASCIIAlphaNum16(units[endPos]) {
				continue
			}
			if m.allowCount > 0 {
				if !allowComputed {
					sc.spans = m.appendAllowSpans(sc.spans[:0], units)
					allowSpans = mergeSpans(sc.spans)
					allowComputed = true
				}
				if isCoveredByAllow(allowSpans, startPos, endPos) {
					continue
				}
			}
			return decodeUTF16Units(units[startPos:endPos]), uint8(meta >> 10), true
		}
	}
	return "", 0, false
}

func (m *Matcher) scanCompactedCJK(normUnits []uint16, sc *scanScratch) (string, uint8, bool) {
	compacted := sc.compacted[:0]
	origStart := sc.origStart[:0]
	origEnd := sc.origEnd[:0]
	strippedAny := false

	for idx := 0; idx < len(normUnits); idx++ {
		u := normUnits[idx]
		if u >= 0xd800 && u <= 0xdbff && idx+1 < len(normUnits) && normUnits[idx+1] >= 0xdc00 && normUnits[idx+1] <= 0xdfff {
			u2 := normUnits[idx+1]
			r := utf16.DecodeRune(rune(u), rune(u2))
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				compacted = append(compacted, u, u2)
				origStart = append(origStart, int32(idx), int32(idx))
				origEnd = append(origEnd, int32(idx+2), int32(idx+2))
			} else {
				strippedAny = true
			}
			idx++
			continue
		}
		r := rune(u)
		if isASCIIAlphaNum16(u) || (r > 0x7f && (unicode.IsLetter(r) || unicode.IsNumber(r))) {
			compacted = append(compacted, u)
			origStart = append(origStart, int32(idx))
			origEnd = append(origEnd, int32(idx+1))
		} else {
			strippedAny = true
		}
	}

	sc.compacted = compacted
	sc.origStart = origStart
	sc.origEnd = origEnd
	if !strippedAny || len(compacted) < 2 {
		return "", 0, false
	}
	return m.scanMappedCJK(normUnits, compacted, origStart, origEnd, sc)
}

func (m *Matcher) scanCollapsedRepeatCJK(normUnits []uint16, sc *scanScratch) (string, uint8, bool) {
	compacted := sc.compacted[:0]
	origStart := sc.origStart[:0]
	origEnd := sc.origEnd[:0]
	repeatedCJK := false

	var prevCJK uint16
	hasPrevCJK := false

	for idx := 0; idx < len(normUnits); idx++ {
		u := normUnits[idx]
		if u >= 0xd800 && u <= 0xdbff && idx+1 < len(normUnits) && normUnits[idx+1] >= 0xdc00 && normUnits[idx+1] <= 0xdfff {
			u2 := normUnits[idx+1]
			compacted = append(compacted, u, u2)
			origStart = append(origStart, int32(idx), int32(idx))
			origEnd = append(origEnd, int32(idx+2), int32(idx+2))
			hasPrevCJK = false
			idx++
			continue
		}
		if isCJKIdeograph(rune(u)) {
			if hasPrevCJK && u == prevCJK {
				origEnd[len(origEnd)-1] = int32(idx + 1)
				repeatedCJK = true
				continue
			}
			prevCJK = u
			hasPrevCJK = true
		} else if isASCIIAlphaNum16(u) || unicode.IsLetter(rune(u)) || unicode.IsNumber(rune(u)) {
			hasPrevCJK = false
		} else {
			// Also pierce separators between repeated CJK characters when IgnoreSeparators is active (e.g. "傻*傻*逼*逼").
			if m.ignoreSeparators {
				continue
			}
			hasPrevCJK = false
		}
		compacted = append(compacted, u)
		origStart = append(origStart, int32(idx))
		origEnd = append(origEnd, int32(idx+1))
	}

	sc.compacted = compacted
	sc.origStart = origStart
	sc.origEnd = origEnd
	if !repeatedCJK || len(compacted) < 2 {
		return "", 0, false
	}
	return m.scanMappedCJK(normUnits, compacted, origStart, origEnd, sc)
}

func (m *Matcher) scanMappedCJK(
	normUnits []uint16,
	compacted []uint16,
	origStart []int32,
	origEnd []int32,
	sc *scanScratch,
) (string, uint8, bool) {
	var allowSpans []runeSpan
	allowComputed := false

	states := m.deny.states
	var state uint32
	for i, ch := range compacted {
		state = m.deny.step(state, ch)
		if state == 0 {
			continue
		}
		endPos := i + 1
		for out := state; out != 0; out = states[out].dictSuffix {
			meta := states[out].meta
			if meta == 0 {
				continue
			}
			uLen := int(meta & 0x7f)
			flags := uint8((meta >> 7) & 0x07)
			if (flags&wordFlagHasCJK) == 0 || uLen < 2 {
				continue
			}
			startPos := endPos - uLen
			if startPos < 0 {
				continue
			}
			if (flags&wordFlagLeadASCII) != 0 && startPos > 0 && isASCIIAlphaNum16(compacted[startPos-1]) {
				continue
			}
			if (flags&wordFlagTrailASCII) != 0 && endPos < len(compacted) && isASCIIAlphaNum16(compacted[endPos]) {
				continue
			}
			if m.allowCount > 0 {
				if !allowComputed {
					allowSpans = m.collectAllowSpansWithMapped(sc, normUnits, compacted, origStart, origEnd)
					allowComputed = true
				}
				rawStart := int(origStart[startPos])
				rawEnd := int(origEnd[endPos-1])
				if isCoveredByAllow(allowSpans, rawStart, rawEnd) {
					continue
				}
			}
			return decodeUTF16Units(compacted[startPos:endPos]), uint8(meta >> 10), true
		}
	}
	return "", 0, false
}

func (m *Matcher) appendAllowSpans(dst []runeSpan, units []uint16) []runeSpan {
	if m.allowCount == 0 || m.allow == nil || len(m.allow.states) == 0 {
		return dst
	}
	states := m.allow.states
	var state uint32
	for i, ch := range units {
		state = m.allow.step(state, ch)
		if state == 0 {
			continue
		}
		endPos := i + 1
		for out := state; out != 0; out = states[out].dictSuffix {
			meta := states[out].meta
			if meta == 0 {
				continue
			}
			uLen := int(meta & 0x7f)
			if endPos >= uLen {
				dst = append(dst, runeSpan{start: endPos - uLen, end: endPos})
			}
		}
	}
	return dst
}

func (m *Matcher) collectAllowSpansWithMapped(
	sc *scanScratch,
	normUnits []uint16,
	compacted []uint16,
	origStart []int32,
	origEnd []int32,
) []runeSpan {
	spans := m.appendAllowSpans(sc.spans[:0], normUnits)
	if m.allow != nil && len(m.allow.states) > 0 {
		states := m.allow.states
		var state uint32
		for i, ch := range compacted {
			state = m.allow.step(state, ch)
			if state == 0 {
				continue
			}
			endPos := i + 1
			for out := state; out != 0; out = states[out].dictSuffix {
				meta := states[out].meta
				if meta == 0 {
					continue
				}
				uLen := int(meta & 0x7f)
				if endPos >= uLen {
					cs := endPos - uLen
					spans = append(spans, runeSpan{start: int(origStart[cs]), end: int(origEnd[endPos-1])})
				}
			}
		}
	}
	sc.spans = spans
	return mergeSpans(spans)
}

func mergeSpans(spans []runeSpan) []runeSpan {
	if len(spans) <= 1 {
		return spans
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	out := spans[:1]
	for _, sp := range spans[1:] {
		last := &out[len(out)-1]
		if sp.start <= last.end {
			if sp.end > last.end {
				last.end = sp.end
			}
		} else {
			out = append(out, sp)
		}
	}
	return out
}

func isCoveredByAllow(spans []runeSpan, start, end int) bool {
	for _, sp := range spans {
		if sp.start > start {
			return false
		}
		if sp.start <= start && end <= sp.end {
			return true
		}
	}
	return false
}

func compareUnitsWithLCP(a, b []uint16) (cmp int, lcp int) {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	for i := 0; i < minLen; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1, i
			}
			return 1, i
		}
	}
	if len(a) < len(b) {
		return -1, minLen
	}
	if len(a) > len(b) {
		return 1, minLen
	}
	return 0, minLen
}

func lessPatEntry(ei, ej patEntry, arena []uint16) bool {
	ui := arena[ei.offset : ei.offset+uint32(ei.unitLen)]
	uj := arena[ej.offset : ej.offset+uint32(ej.unitLen)]
	if cmp, _ := compareUnitsWithLCP(ui, uj); cmp != 0 {
		return cmp < 0
	}
	return ei.order < ej.order
}

func sortPatEntries(a []patEntry, arena []uint16) {
	for len(a) > 16 {
		mid := len(a) >> 1
		last := len(a) - 1
		if lessPatEntry(a[mid], a[0], arena) {
			a[0], a[mid] = a[mid], a[0]
		}
		if lessPatEntry(a[last], a[0], arena) {
			a[0], a[last] = a[last], a[0]
		}
		if lessPatEntry(a[last], a[mid], arena) {
			a[mid], a[last] = a[last], a[mid]
		}
		pivot := a[mid]
		i, j := 0, last
		for i <= j {
			for lessPatEntry(a[i], pivot, arena) {
				i++
			}
			for lessPatEntry(pivot, a[j], arena) {
				j--
			}
			if i <= j {
				a[i], a[j] = a[j], a[i]
				i++
				j--
			}
		}
		if j < len(a)-i {
			if j > 0 {
				sortPatEntries(a[:j+1], arena)
			}
			a = a[i:]
		} else {
			if i < len(a)-1 {
				sortPatEntries(a[i:], arena)
			}
			a = a[:j+1]
		}
	}
	for i := 1; i < len(a); i++ {
		cur := a[i]
		j := i - 1
		for j >= 0 && lessPatEntry(cur, a[j], arena) {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = cur
	}
}

// sortAndDeduplicateEntries sorts patterns lexicographically, deduplicates identical patterns
// (preserving the earliest registration order), and computes the exact CSR state count
// (root + trie nodes + trailing sentinel = 2 + |P_0| + sum(|P_i| - LCP(P_{i-1}, P_i)))
// in a single linear pass with zero extra allocations.
func sortAndDeduplicateEntries(entries []patEntry, arena []uint16) ([]patEntry, int) {
	if len(entries) == 0 {
		return nil, 2
	}
	if len(entries) == 1 {
		return entries, 2 + int(entries[0].unitLen)
	}
	sortPatEntries(entries, arena)

	w := 1
	prev := entries[0]
	prevSlice := arena[prev.offset : prev.offset+uint32(prev.unitLen)]
	exactStates := 2 + len(prevSlice)
	for i := 1; i < len(entries); i++ {
		cur := entries[i]
		curSlice := arena[cur.offset : cur.offset+uint32(cur.unitLen)]
		cmp, lcp := compareUnitsWithLCP(prevSlice, curSlice)
		if cmp == 0 {
			continue
		}
		exactStates += len(curSlice) - lcp
		entries[w] = cur
		prevSlice = curSlice
		w++
	}
	return entries[:w], exactStates
}

// buildCSRAutomaton builds a pointer-free 16-byte/state BFS-CSR Aho-Corasick automaton
// directly from sorted, deduplicated UTF-16 patterns with exact zero-slack state capacity
// and ping-pong depth frontiers.
func buildCSRAutomaton(entries []patEntry, arena []uint16, exactStates int) *automaton {
	type bfsInterval struct {
		lo uint32
		hi uint32
	}

	if exactStates < 2 {
		exactStates = 2
	}
	states := make([]acState, 1, exactStates)

	frontierCap := len(entries)
	if frontierCap > 4096 {
		frontierCap = 4096
	} else if frontierCap < 4 {
		frontierCap = 4
	}
	curLevel := make([]bfsInterval, 1, frontierCap)
	nextLevel := make([]bfsInterval, 0, frontierCap)
	curLevel[0] = bfsInterval{lo: 0, hi: uint32(len(entries))}

	var levelStateStart uint32
	var depth uint8
	for len(curLevel) > 0 {
		for k := 0; k < len(curLevel); k++ {
			u := levelStateStart + uint32(k)
			iv := curLevel[k]
			states[u].childStart = uint32(len(states))

			lo := iv.lo
			hi := iv.hi

			if lo < hi {
				first := entries[lo]
				if first.unitLen == depth {
					states[u].meta = packStateMeta(first.unitLen, first.flags, first.catIdx)
					lo++
				}
			}

			for lo < hi {
				e := entries[lo]
				ch := arena[e.offset+uint32(depth)]
				runEnd := lo + 1
				for runEnd < hi {
					en := entries[runEnd]
					if arena[en.offset+uint32(depth)] != ch {
						break
					}
					runEnd++
				}
				states = append(states, acState{ch: ch})
				nextLevel = append(nextLevel, bfsInterval{lo: lo, hi: runEnd})
				lo = runEnd
			}
		}
		levelStateStart += uint32(len(curLevel))
		curLevel, nextLevel = nextLevel, curLevel[:0]
		depth++
	}

	nodeCount := uint32(len(states))
	// Sentinel state at states[nodeCount] eliminates bounds checks when reading child end offset.
	states = append(states, acState{childStart: nodeCount})

	auto := &automaton{
		states: states,
	}

	// Populate O(1) root ASCII lookup table, 8 KiB root BMP bitmap, and 514-byte hi-byte bucket index.
	rootStart := states[0].childStart
	rootEnd := states[1].childStart
	var rootDirect *[65536]uint32
	if rootEnd-rootStart > 64 {
		rootDirect = new([65536]uint32)
	}
	curHi := 0
	for v := rootStart; v < rootEnd; v++ {
		ch := states[v].ch
		hi := int(ch >> 8)
		for curHi <= hi {
			auto.rootBuckets[curHi] = uint16(v - rootStart)
			curHi++
		}
		if ch < 128 {
			auto.rootASCII[ch] = v
		}
		auto.rootBitmap[ch>>6] |= uint64(1) << (ch & 63)
		if rootDirect != nil {
			rootDirect[ch] = v
		}
	}
	totalRoot := uint16(rootEnd - rootStart)
	for curHi <= 256 {
		auto.rootBuckets[curHi] = totalRoot
		curHi++
	}

	// States 1..nodeCount-1 are already in exact BFS depth order, so failure & dictionary-suffix
	// links resolve in a single linear pass without an extra queue allocation.
	for u := uint32(1); u < nodeCount; u++ {
		uFail := states[u].fail
		cStart := states[u].childStart
		cEnd := states[u+1].childStart
		for v := cStart; v < cEnd; v++ {
			ch := states[v].ch
			f := uFail
			var target uint32
			for {
				if f == 0 && rootDirect != nil {
					target = rootDirect[ch]
					break
				}
				if nxt, ok := auto.findChild(f, ch); ok {
					target = nxt
					break
				}
				if f == 0 {
					break
				}
				f = states[f].fail
			}
			states[v].fail = target
			if states[target].meta != 0 {
				states[v].dictSuffix = target
			} else {
				states[v].dictSuffix = states[target].dictSuffix
			}
		}
	}

	return auto
}

func (a *automaton) findChild(state uint32, ch uint16) (uint32, bool) {
	var start, end int
	if state == 0 {
		if ch < 128 {
			nxt := a.rootASCII[ch]
			return nxt, nxt != 0
		}
		if (a.rootBitmap[ch>>6] & (uint64(1) << (ch & 63))) == 0 {
			return 0, false
		}
		rootBase := int(a.states[0].childStart)
		hi := ch >> 8
		start = rootBase + int(a.rootBuckets[hi])
		end = rootBase + int(a.rootBuckets[hi+1])
	} else {
		start = int(a.states[state].childStart)
		end = int(a.states[state+1].childStart)
	}
	cnt := end - start
	if cnt <= 0 {
		return 0, false
	}
	if cnt <= 8 {
		for i := start; i < end; i++ {
			c := a.states[i].ch
			if c == ch {
				return uint32(i), true
			}
			if c > ch {
				return 0, false
			}
		}
		return 0, false
	}
	lo, hi := start, end-1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		c := a.states[mid].ch
		if c == ch {
			return uint32(mid), true
		}
		if c < ch {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return 0, false
}

func (a *automaton) step(state uint32, ch uint16) uint32 {
	if state == 0 {
		if ch < 128 {
			return a.rootASCII[ch]
		}
		if (a.rootBitmap[ch>>6] & (uint64(1) << (ch & 63))) == 0 {
			return 0
		}
	}
	for {
		if nxt, ok := a.findChild(state, ch); ok {
			return nxt
		}
		if state == 0 {
			return 0
		}
		state = a.states[state].fail
		if state == 0 {
			if ch < 128 {
				return a.rootASCII[ch]
			}
			if (a.rootBitmap[ch>>6] & (uint64(1) << (ch & 63))) == 0 {
				return 0
			}
		}
	}
}

func decodeUTF16Units(units []uint16) string {
	for _, u := range units {
		if u >= 0x80 {
			return string(utf16.Decode(units))
		}
	}
	buf := make([]byte, len(units))
	for i, u := range units {
		buf[i] = byte(u)
	}
	return string(buf)
}

func unescapeHTMLBounded(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	u1 := html.UnescapeString(s)
	if u1 != s && strings.ContainsRune(u1, '&') {
		return html.UnescapeString(u1)
	}
	return u1
}

// appendNormalizedUnits normalizes s into UTF-16 code units. When maxUnits > 0, stops once
// len(dst) reaches maxUnits so oversize patterns fail fast without allocating further.
func appendNormalizedUnits(dst []uint16, s string, maxUnits int) []uint16 {
	for _, r := range s {
		nr, keep := foldRune(r)
		if !keep {
			continue
		}
		if nr <= 0xffff {
			dst = append(dst, uint16(nr))
		} else {
			r1, r2 := utf16.EncodeRune(nr)
			dst = append(dst, uint16(r1), uint16(r2))
		}
		if maxUnits > 0 && len(dst) >= maxUnits {
			return dst
		}
	}
	return dst
}

func foldRune(r rune) (rune, bool) {
	// Fast path for printable ASCII (covers English text, URLs, Markdown, and digits).
	if r >= 0x20 && r <= 0x7e {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A'), true
		}
		return r, true
	}

	if r < 0x20 {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' ', true
		}
		return 0, false
	}

	// Strip DEL, zero-width, soft-hyphen, Zalgo/strikethrough combining overlays, variation selectors, and Cf format chars.
	switch {
	case r == 0x7f || r == '\u00ad' || r == '\u034f' || r == '\u061c' || r == '\u115f' || r == '\u1160' || r == '\u180e' || r == '\u3164' || r == '\ufeff' || r == '\uffa0':
		return 0, false
	case (r >= '\u0300' && r <= '\u036f') || (r >= '\u0483' && r <= '\u0489') ||
		(r >= '\u1ab0' && r <= '\u1aff') || (r >= '\u1dc0' && r <= '\u1dff') ||
		(r >= '\u20d0' && r <= '\u20ff') || (r >= '\ufe20' && r <= '\ufe2f'):
		return 0, false
	case r >= '\u200b' && r <= '\u200f':
		return 0, false
	case r >= '\u202a' && r <= '\u202f':
		return 0, false
	case r >= '\u2060' && r <= '\u206f':
		return 0, false
	case r >= '\ufe00' && r <= '\ufe0f':
		return 0, false
	case r >= 0xe0100 && r <= 0xe01ef:
		return 0, false
	case r == '\u3000': // Full-width space
		return ' ', true
	case r >= '\uff01' && r <= '\uff5e': // Full-width ASCII
		r -= 0xfee0
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A'), true
		}
		return r, true
	}

	// Fold styled numerals & letters (BMP enclosed, Mathematical Alphanumeric Symbols, Enclosed Supplement).
	if folded, ok := foldStyledAlphaNum(r); ok {
		return folded, true
	}

	// Fold Traditional Chinese BMP ideographs to Simplified Chinese (houbb ZhSlimUtil parity).
	if r >= zhTradMinRune && r <= zhTradMaxRune {
		return foldTraditionalHanzi(r), true
	}

	if unicode.Is(unicode.Cf, r) {
		return 0, false
	}
	return unicode.ToLower(r), true
}

func foldStyledAlphaNum(r rune) (rune, bool) {
	switch {
	case r == '\u24ea' || r == '\u24ff' || r == '\u2070' || r == '\u2080' || r == 0x1f100 || r == 0x1f10b || r == 0x1f10c: // ⓪ ⓿ ⁰ ₀ 🄀 🄋 🄌
		return '0', true
	case r == '\u00b9': // ¹
		return '1', true
	case r == '\u00b2': // ²
		return '2', true
	case r == '\u00b3': // ³
		return '3', true
	case r >= '\u2074' && r <= '\u2079': // ⁴..⁹
		return '4' + (r - '\u2074'), true
	case r >= '\u2081' && r <= '\u2089': // ₁..₉
		return '1' + (r - '\u2081'), true
	case r >= '\u2460' && r <= '\u2468': // ①..⑨
		return '1' + (r - '\u2460'), true
	case r >= '\u2474' && r <= '\u247c': // ⑴..⑼
		return '1' + (r - '\u2474'), true
	case r >= '\u2488' && r <= '\u2490': // ⒈..⒐
		return '1' + (r - '\u2488'), true
	case r >= '\u24f5' && r <= '\u24fd': // ⓵..⓽
		return '1' + (r - '\u24f5'), true
	case r >= '\u2776' && r <= '\u277e': // ❶..❾
		return '1' + (r - '\u2776'), true
	case r >= '\u2780' && r <= '\u2788': // ➀..➈
		return '1' + (r - '\u2780'), true
	case r >= '\u278a' && r <= '\u2792': // ➊..➒
		return '1' + (r - '\u278a'), true
	case r >= 0x1f101 && r <= 0x1f10a: // 🄁..🄊 (0..9 comma)
		return '0' + (r - 0x1f101), true
	case r >= 0x1d7ce && r <= 0x1d7ff: // Mathematical Bold/Double-Struck/Sans/Mono digits (5 * 10)
		return '0' + ((r - 0x1d7ce) % 10), true
	case r >= '\u249c' && r <= '\u24b5': // ⒜..⒵
		return 'a' + (r - '\u249c'), true
	case r >= '\u24b6' && r <= '\u24cf': // Ⓐ..Ⓩ
		return 'a' + (r - '\u24b6'), true
	case r >= '\u24d0' && r <= '\u24e9': // ⓐ..ⓩ
		return 'a' + (r - '\u24d0'), true
	case r >= 0x1d400 && r <= 0x1d6a3: // Mathematical Bold/Italic/Script/Fraktur/Double-Struck/Sans/Mono Latin (13 * 52)
		return 'a' + (((r - 0x1d400) % 52) % 26), true
	case r >= 0x1f110 && r <= 0x1f129: // 🄐..🄩
		return 'a' + (r - 0x1f110), true
	case r >= 0x1f130 && r <= 0x1f149: // 🄰..🅉
		return 'a' + (r - 0x1f130), true
	case r >= 0x1f150 && r <= 0x1f169: // 🅐..🅩
		return 'a' + (r - 0x1f150), true
	case r >= 0x1f170 && r <= 0x1f189: // 🅰..🆉
		return 'a' + (r - 0x1f170), true
	case r >= 0x1f1e6 && r <= 0x1f1ff: // Regional Indicator 🇦..🇿
		return 'a' + (r - 0x1f1e6), true
	case r >= '\u2102' && r <= '\u2134': // BMP Letterlike Symbols holes for Mathematical Script/Fraktur/Double-Struck
		return foldBMPLetterlike(r)
	case (r >= 0x0370 && r <= 0x052f): // Greek & Cyrillic visual Latin homoglyphs (blocks mixed-script 'аdmin' spoofing)
		return foldHomoglyphAlpha(r)
	default:
		return 0, false
	}
}

func foldHomoglyphAlpha(r rune) (rune, bool) {
	switch r {
	case '\u0430', '\u0410', '\u03b1', '\u0391': // Cyrillic а/А, Greek α/Α
		return 'a', true
	case '\u0432', '\u0412', '\u03b2', '\u0392': // Cyrillic в/В, Greek β/Β
		return 'b', true
	case '\u0441', '\u0421': // Cyrillic с/С
		return 'c', true
	case '\u0435', '\u0415', '\u03b5', '\u0395': // Cyrillic е/Е, Greek ε/Ε
		return 'e', true
	case '\u04bb', '\u04ba', '\u0397': // Cyrillic һ/Һ, Greek Η
		return 'h', true
	case '\u0456', '\u0406', '\u03b9', '\u0399': // Cyrillic і/І, Greek ι/Ι
		return 'i', true
	case '\u0458', '\u0408': // Cyrillic ј/Ј
		return 'j', true
	case '\u043a', '\u041a', '\u03ba', '\u039a': // Cyrillic к/К, Greek κ/Κ
		return 'k', true
	case '\u043c', '\u041c', '\u039c': // Cyrillic м/М, Greek Μ
		return 'm', true
	case '\u03bd', '\u039d': // Greek ν/Ν
		return 'v', true
	case '\u043e', '\u041e', '\u03bf', '\u039f': // Cyrillic о/О, Greek ο/Ο
		return 'o', true
	case '\u0440', '\u0420', '\u03c1', '\u03a1': // Cyrillic р/Р, Greek ρ/Ρ
		return 'p', true
	case '\u0455', '\u0405': // Cyrillic ѕ/Ѕ
		return 's', true
	case '\u0442', '\u0422', '\u03c4', '\u03a4': // Cyrillic т/Т, Greek τ/Τ
		return 't', true
	case '\u0443', '\u0423', '\u03c5', '\u03a5': // Cyrillic у/У, Greek υ/Υ
		return 'y', true
	case '\u0445', '\u0425', '\u03c7', '\u03a7': // Cyrillic х/Х, Greek χ/Χ
		return 'x', true
	case '\u0396': // Greek Ζ
		return 'z', true
	default:
		return 0, false
	}
}

func foldBMPLetterlike(r rune) (rune, bool) {
	switch r {
	case '\u2102', '\u212d': // ℂ ℭ
		return 'c', true
	case '\u212f', '\u2130': // ℯ ℰ
		return 'e', true
	case '\u2131': // ℱ
		return 'f', true
	case '\u210a': // ℊ
		return 'g', true
	case '\u210b', '\u210c', '\u210d', '\u210e': // ℋ ℌ ℍ ℎ
		return 'h', true
	case '\u2110', '\u2111': // ℐ ℑ
		return 'i', true
	case '\u2112': // ℒ
		return 'l', true
	case '\u2133': // ℳ
		return 'm', true
	case '\u2115': // ℕ
		return 'n', true
	case '\u2134': // ℴ
		return 'o', true
	case '\u2119': // ℙ
		return 'p', true
	case '\u211a': // ℚ
		return 'q', true
	case '\u211b', '\u211c', '\u211d': // ℛ ℜ ℝ
		return 'r', true
	case '\u2124', '\u2128': // ℤ ℨ
		return 'z', true
	case '\u212c': // ℬ
		return 'b', true
	default:
		return 0, false
	}
}

func isASCIIAlphaNum16(u uint16) bool {
	return (u >= 'a' && u <= 'z') || (u >= 'A' && u <= 'Z') || (u >= '0' && u <= '9')
}

func isCJKIdeograph(r rune) bool {
	return unicode.Is(unicode.Han, r)
}

func hasMarkdownOrHTMLMarkup(s string) bool {
	return strings.ContainsAny(s, "*_~`[]<>")
}

// extractVisibleMarkdown strips inline Markdown formatting markers, HTML tags, and link URL targets
// so split words like "禁**词**", "禁`词`", "[禁](https://example.com)词", or "禁<span>词</span>"
// rejoin into visible text "禁词" just as a human reader sees them.
// Returns (s, false) with zero heap allocations when no markup was actually stripped.
func extractVisibleMarkdown(s string) (string, bool) {
	var b strings.Builder
	changed := false
	n := len(s)

	flushPrefix := func(upto int) {
		if !changed {
			b.Grow(n)
			if upto > 0 {
				b.WriteString(s[:upto])
			}
			changed = true
		}
	}

	for i := 0; i < n; {
		ch := s[i]
		// Strip inline HTML tags <...> (bounded to avoid swallowing non-tag '<').
		if ch == '<' {
			closeIdx := strings.IndexByte(s[i+1:], '>')
			if closeIdx >= 0 && closeIdx <= 256 {
				tagBody := s[i+1 : i+1+closeIdx]
				if len(tagBody) > 0 && (isASCIIAlpha(tagBody[0]) || tagBody[0] == '/' || tagBody[0] == '!') {
					flushPrefix(i)
					i += closeIdx + 2
					continue
				}
			}
		}
		// Unwrap Markdown links/images: ![alt](url) -> alt, [text](url) -> text.
		if ch == '!' && i+1 < n && s[i+1] == '[' {
			if nextI, label, ok := parseMarkdownLinkAt(s, i+1); ok {
				flushPrefix(i)
				b.WriteString(label)
				i = nextI
				continue
			}
		}
		if ch == '[' {
			if nextI, label, ok := parseMarkdownLinkAt(s, i); ok {
				flushPrefix(i)
				b.WriteString(label)
				i = nextI
				continue
			}
		}
		// Strip Markdown emphasis/strikethrough/code delimiters.
		if ch == '*' || ch == '_' || ch == '~' || ch == '`' {
			flushPrefix(i)
			i++
			continue
		}
		if changed {
			b.WriteByte(ch)
		}
		i++
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func parseMarkdownLinkAt(s string, openBracket int) (nextIdx int, label string, ok bool) {
	depth := 0
	closeBracket := -1
	for i := openBracket; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if s[i] == '[' {
			depth++
		} else if s[i] == ']' {
			depth--
			if depth == 0 {
				closeBracket = i
				break
			}
		}
	}
	if closeBracket < 0 || closeBracket+1 >= len(s) || s[closeBracket+1] != '(' {
		return 0, "", false
	}
	parenDepth := 0
	for j := closeBracket + 1; j < len(s); j++ {
		if s[j] == '\\' && j+1 < len(s) {
			j++
			continue
		}
		if s[j] == '(' {
			parenDepth++
		} else if s[j] == ')' {
			parenDepth--
			if parenDepth == 0 {
				return j + 1, s[openBracket+1 : closeBracket], true
			}
		}
	}
	return 0, "", false
}
