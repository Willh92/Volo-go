package wordfilter

import (
	"html"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

type maskScratch struct {
	scanScratch
	uStart     []int32
	uEnd       []int32
	matchSpans []runeSpan
	localSpans []runeSpan
}

var maskScratchPool = sync.Pool{
	New: func() any {
		return &maskScratch{
			scanScratch: scanScratch{
				norm:      make([]uint16, 0, 512),
				visNorm:   make([]uint16, 0, 256),
				compacted: make([]uint16, 0, 512),
				origStart: make([]int32, 0, 512),
				origEnd:   make([]int32, 0, 512),
				spans:     make([]runeSpan, 0, 8),
			},
			uStart:     make([]int32, 0, 512),
			uEnd:       make([]int32, 0, 512),
			matchSpans: make([]runeSpan, 0, 8),
			localSpans: make([]runeSpan, 0, 4),
		}
	},
}

func acquireMaskScratch() *maskScratch {
	ms := maskScratchPool.Get().(*maskScratch)
	ms.norm = ms.norm[:0]
	ms.visNorm = ms.visNorm[:0]
	ms.compacted = ms.compacted[:0]
	ms.origStart = ms.origStart[:0]
	ms.origEnd = ms.origEnd[:0]
	ms.spans = ms.spans[:0]
	ms.uStart = ms.uStart[:0]
	ms.uEnd = ms.uEnd[:0]
	ms.matchSpans = ms.matchSpans[:0]
	ms.localSpans = ms.localSpans[:0]
	return ms
}

func releaseMaskScratch(ms *maskScratch) {
	if cap(ms.norm) > maxPooledScratchCap ||
		cap(ms.compacted) > maxPooledScratchCap ||
		cap(ms.origStart) > maxPooledScratchCap ||
		cap(ms.origEnd) > maxPooledScratchCap ||
		cap(ms.uStart) > maxPooledScratchCap ||
		cap(ms.uEnd) > maxPooledScratchCap ||
		cap(ms.matchSpans) > maxPooledScratchCap {
		return
	}
	maskScratchPool.Put(ms)
}

// MaskText replaces all non-allowlisted sensitive word matches in plain UTF-8 text with '*'
// while preserving valid UTF-8 rune boundaries, surrounding emoji/combining sequences, whitespace,
// allowlisted phrases, and ASCII word boundaries. Returns text unchanged on the fast path.
func (m *Matcher) MaskText(text string) string {
	if m == nil || m.wordCount == 0 || m.deny == nil || strings.TrimSpace(text) == "" {
		return text
	}
	ms := acquireMaskScratch()
	defer releaseMaskScratch(ms)

	spans := m.collectMatchByteSpans(text, ms)
	if len(spans) == 0 {
		return text
	}
	return applyByteSpansMask(text, spans)
}

type htmlMaskSegment struct {
	rawStart  int
	rawEnd    int
	unescaped string
	virtStart int
	virtEnd   int
	isText    bool
}

// MaskHTML replaces all non-allowlisted sensitive word matches across visible text nodes of htmlStr
// with '*' while leaving all HTML tags, tag attributes (href, src, class, etc.), HTML comments,
// script/style blocks, and untouched HTML entities completely intact. Words split across inline
// HTML tags (e.g. 傻<strong>逼</strong>) are joined in a virtual visible-text pass and masked safely.
func (m *Matcher) MaskHTML(htmlStr string) string {
	if m == nil || m.wordCount == 0 || m.deny == nil || strings.TrimSpace(htmlStr) == "" {
		return htmlStr
	}
	if !strings.ContainsAny(htmlStr, "<&") {
		return m.MaskText(htmlStr)
	}

	ms := acquireMaskScratch()
	defer releaseMaskScratch(ms)

	segments, virtText := tokenizeHTMLForMask(htmlStr)
	if virtText == "" {
		return htmlStr
	}
	virtSpans := m.collectMatchByteSpans(virtText, ms)
	if len(virtSpans) == 0 {
		return htmlStr
	}

	var out strings.Builder
	out.Grow(len(htmlStr))
	spIdx := 0

	for _, seg := range segments {
		if !seg.isText || seg.unescaped == "" {
			out.WriteString(htmlStr[seg.rawStart:seg.rawEnd])
			continue
		}
		for spIdx < len(virtSpans) && virtSpans[spIdx].end <= seg.virtStart {
			spIdx++
		}
		if spIdx >= len(virtSpans) || virtSpans[spIdx].start >= seg.virtEnd {
			out.WriteString(htmlStr[seg.rawStart:seg.rawEnd])
			continue
		}

		local := ms.localSpans[:0]
		for k := spIdx; k < len(virtSpans) && virtSpans[k].start < seg.virtEnd; k++ {
			ls := virtSpans[k].start - seg.virtStart
			if ls < 0 {
				ls = 0
			}
			le := virtSpans[k].end - seg.virtStart
			if le > len(seg.unescaped) {
				le = len(seg.unescaped)
			}
			if ls < le {
				local = append(local, runeSpan{start: ls, end: le})
			}
		}
		ms.localSpans = local
		if len(local) == 0 {
			out.WriteString(htmlStr[seg.rawStart:seg.rawEnd])
			continue
		}

		masked := applyByteSpansMask(seg.unescaped, local)
		writeEscapedHTMLText(&out, masked)
	}
	return out.String()
}

func (m *Matcher) collectMatchByteSpans(s string, ms *maskScratch) []runeSpan {
	units := ms.norm[:0]
	uStart := ms.uStart[:0]
	uEnd := ms.uEnd[:0]

	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if width <= 0 {
			width = 1
		}
		nextI := i + width
		nr, keep := foldRune(r)
		if !keep {
			if len(uEnd) > 0 {
				prevStart := uStart[len(uStart)-1]
				for k := len(uEnd) - 1; k >= 0 && uStart[k] == prevStart; k-- {
					uEnd[k] = int32(nextI)
				}
			}
			i = nextI
			continue
		}
		if nr <= 0xffff {
			units = append(units, uint16(nr))
			uStart = append(uStart, int32(i))
			uEnd = append(uEnd, int32(nextI))
		} else {
			r1, r2 := utf16.EncodeRune(nr)
			units = append(units, uint16(r1), uint16(r2))
			uStart = append(uStart, int32(i), int32(i))
			uEnd = append(uEnd, int32(nextI), int32(nextI))
		}
		i = nextI
	}

	ms.norm = units
	ms.uStart = uStart
	ms.uEnd = uEnd
	if len(units) == 0 {
		return nil
	}

	matched := ms.matchSpans[:0]

	// Pass 1: Direct normalized unit scan.
	matched = m.appendDirectMatchByteSpans(matched, units, uStart, uEnd, &ms.scanScratch)

	// Pass 2: CJK separator-piercing scan ("傻*逼", "加·微·信").
	if m.ignoreSeparators && m.hasCJKMulti && len(units) >= 2 {
		matched = m.appendCompactedCJKByteSpans(matched, units, uStart, uEnd, &ms.scanScratch)
	}

	// Pass 3: Consecutive CJK repeat-collapsing scan ("傻傻逼逼").
	if m.ignoreRepeat && m.hasCJKMulti && len(units) >= 2 {
		matched = m.appendCollapsedRepeatCJKByteSpans(matched, units, uStart, uEnd, &ms.scanScratch)
	}

	ms.matchSpans = matched
	if len(matched) == 0 {
		return nil
	}
	return mergeSpans(matched)
}

func (m *Matcher) appendDirectMatchByteSpans(
	dst []runeSpan,
	units []uint16,
	uStart []int32,
	uEnd []int32,
	sc *scanScratch,
) []runeSpan {
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
			bStart := int(uStart[startPos])
			bEnd := int(uEnd[endPos-1])
			if bStart < bEnd {
				dst = append(dst, runeSpan{start: bStart, end: bEnd})
			}
		}
	}
	return dst
}

func (m *Matcher) appendCompactedCJKByteSpans(
	dst []runeSpan,
	normUnits []uint16,
	uStart []int32,
	uEnd []int32,
	sc *scanScratch,
) []runeSpan {
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
		return dst
	}
	return m.appendMappedCJKByteSpans(dst, normUnits, uStart, uEnd, compacted, origStart, origEnd, sc)
}

func (m *Matcher) appendCollapsedRepeatCJKByteSpans(
	dst []runeSpan,
	normUnits []uint16,
	uStart []int32,
	uEnd []int32,
	sc *scanScratch,
) []runeSpan {
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
		return dst
	}
	return m.appendMappedCJKByteSpans(dst, normUnits, uStart, uEnd, compacted, origStart, origEnd, sc)
}

func (m *Matcher) appendMappedCJKByteSpans(
	dst []runeSpan,
	normUnits []uint16,
	uStart []int32,
	uEnd []int32,
	compacted []uint16,
	origStart []int32,
	origEnd []int32,
	sc *scanScratch,
) []runeSpan {
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
			rawStart := int(origStart[startPos])
			rawEnd := int(origEnd[endPos-1])
			if rawStart < 0 || rawEnd <= rawStart || rawEnd > len(normUnits) {
				continue
			}
			if m.allowCount > 0 {
				if !allowComputed {
					allowSpans = m.collectAllowSpansWithMapped(sc, normUnits, compacted, origStart, origEnd)
					allowComputed = true
				}
				if isCoveredByAllow(allowSpans, rawStart, rawEnd) {
					continue
				}
			}
			bStart := int(uStart[rawStart])
			bEnd := int(uEnd[rawEnd-1])
			if bStart < bEnd {
				dst = append(dst, runeSpan{start: bStart, end: bEnd})
			}
		}
	}
	return dst
}

func applyByteSpansMask(s string, spans []runeSpan) string {
	var b strings.Builder
	b.Grow(len(s))
	cursor := 0
	for _, sp := range spans {
		start := sp.start
		end := sp.end
		if start < cursor {
			start = cursor
		}
		if end > len(s) {
			end = len(s)
		}
		if start >= end {
			continue
		}
		if start > cursor {
			b.WriteString(s[cursor:start])
		}
		emitted := false
		for j := start; j < end; {
			r, w := utf8.DecodeRuneInString(s[j:end])
			if w <= 0 {
				w = 1
			}
			j += w
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				b.WriteByte(byte(r))
				continue
			}
			if _, keep := foldRune(r); keep {
				b.WriteByte('*')
				emitted = true
			}
		}
		if !emitted {
			b.WriteByte('*')
		}
		cursor = end
	}
	if cursor < len(s) {
		b.WriteString(s[cursor:])
	}
	return b.String()
}

func tokenizeHTMLForMask(htmlStr string) ([]htmlMaskSegment, string) {
	n := len(htmlStr)
	segments := make([]htmlMaskSegment, 0, 8)
	var virtBuf strings.Builder
	virtBuf.Grow(n)
	pendingBlockBreak := false

	appendTextSeg := func(start, end int) {
		if start >= end {
			return
		}
		rawSeg := htmlStr[start:end]
		unesc := rawSeg
		if strings.IndexByte(rawSeg, '&') >= 0 {
			unesc = html.UnescapeString(rawSeg)
		}
		if unesc == "" {
			segments = append(segments, htmlMaskSegment{
				rawStart: start,
				rawEnd:   end,
				isText:   false,
			})
			return
		}
		if pendingBlockBreak && virtBuf.Len() > 0 {
			virtBuf.WriteByte('\n')
		}
		pendingBlockBreak = false
		vStart := virtBuf.Len()
		virtBuf.WriteString(unesc)
		vEnd := virtBuf.Len()
		segments = append(segments, htmlMaskSegment{
			rawStart:  start,
			rawEnd:    end,
			unescaped: unesc,
			virtStart: vStart,
			virtEnd:   vEnd,
			isText:    true,
		})
	}

	textStart := 0
	for i := 0; i < n; {
		rel := strings.IndexByte(htmlStr[i:], '<')
		if rel < 0 {
			break
		}
		lt := i + rel

		if strings.HasPrefix(htmlStr[lt:], "<!--") {
			appendTextSeg(textStart, lt)
			endRel := strings.Index(htmlStr[lt+4:], "-->")
			commentEnd := n
			if endRel >= 0 {
				commentEnd = lt + 4 + endRel + 3
			}
			segments = append(segments, htmlMaskSegment{rawStart: lt, rawEnd: commentEnd})
			i = commentEnd
			textStart = i
			continue
		}

		tagEnd, tagName, isClosing, isSelfClosing, ok := scanHTMLTag(htmlStr, lt)
		if !ok {
			i = lt + 1
			continue
		}

		appendTextSeg(textStart, lt)
		rawEnd := tagEnd
		if !isClosing && !isSelfClosing && (tagName == "script" || tagName == "style") {
			rawEnd = skipRawTextElement(htmlStr, tagEnd, tagName)
		}
		segments = append(segments, htmlMaskSegment{rawStart: lt, rawEnd: rawEnd})
		if isHTMLBlockTag(tagName) {
			pendingBlockBreak = true
		}
		i = rawEnd
		textStart = i
	}

	appendTextSeg(textStart, n)
	return segments, virtBuf.String()
}

func scanHTMLTag(s string, lt int) (tagEnd int, tagName string, isClosing bool, isSelfClosing bool, ok bool) {
	n := len(s)
	if lt+2 > n || s[lt] != '<' {
		return 0, "", false, false, false
	}
	pos := lt + 1
	b := s[pos]
	if b == '!' || b == '?' {
		closeRel := strings.IndexByte(s[pos+1:], '>')
		if closeRel < 0 {
			return 0, "", false, false, false
		}
		return pos + 1 + closeRel + 1, "", false, true, true
	}
	if b == '/' {
		isClosing = true
		pos++
		if pos >= n {
			return 0, "", false, false, false
		}
	}
	if !isASCIIAlpha(s[pos]) {
		return 0, "", false, false, false
	}
	nameStart := pos
	for pos < n {
		c := s[pos]
		if isASCIIAlpha(c) || (c >= '0' && c <= '9') || c == '-' || c == ':' {
			pos++
		} else {
			break
		}
	}
	tagName = asciiLower(s[nameStart:pos])
	if pos < n && s[pos] != '>' && s[pos] != '/' && s[pos] != ' ' && s[pos] != '\t' && s[pos] != '\n' && s[pos] != '\r' {
		return 0, "", false, false, false
	}

	for j := pos; j < n; j++ {
		c := s[j]
		if c == '"' || c == '\'' {
			if closeQ := findSafeAttrQuoteEnd(s, j+1, c); closeQ >= 0 {
				j = closeQ
				continue
			}
		}
		if c == '>' {
			prev := j - 1
			for prev >= pos && (s[prev] == ' ' || s[prev] == '\t' || s[prev] == '\n' || s[prev] == '\r') {
				prev--
			}
			if prev >= pos && s[prev] == '/' {
				isSelfClosing = true
			}
			return j + 1, tagName, isClosing, isSelfClosing, true
		}
	}
	return 0, "", false, false, false
}

func findSafeAttrQuoteEnd(s string, start int, quote byte) int {
	limit := len(s)
	if start+4096 < limit {
		limit = start + 4096
	}
	for i := start; i < limit; i++ {
		if s[i] == quote {
			return i
		}
		if s[i] == '<' {
			return -1
		}
	}
	return -1
}

func skipRawTextElement(s string, bodyStart int, lowerTag string) int {
	n := len(s)
	tLen := len(lowerTag)
	for i := bodyStart; i+2+tLen < n; i++ {
		if s[i] == '<' && s[i+1] == '/' && asciiEqualFold(s[i+2:i+2+tLen], lowerTag) {
			after := i + 2 + tLen
			for after < n && (s[after] == ' ' || s[after] == '\t' || s[after] == '\n' || s[after] == '\r') {
				after++
			}
			if after < n && s[after] == '>' {
				return after + 1
			}
		}
	}
	return n
}

func isHTMLBlockTag(lowerTag string) bool {
	switch lowerTag {
	case "p", "div", "br", "hr", "li", "ul", "ol", "blockquote", "pre",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"table", "thead", "tbody", "tfoot", "tr", "td", "th",
		"section", "article", "header", "footer", "aside", "nav",
		"dl", "dt", "dd", "figure", "figcaption", "details", "summary":
		return true
	default:
		return false
	}
}

func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			buf := []byte(s)
			for j := i; j < len(buf); j++ {
				if buf[j] >= 'A' && buf[j] <= 'Z' {
					buf[j] += 'a' - 'A'
				}
			}
			return string(buf)
		}
	}
	return s
}

func asciiEqualFold(a, lowerB string) bool {
	if len(a) != len(lowerB) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lowerB[i] {
			return false
		}
	}
	return true
}

func writeEscapedHTMLText(dst *strings.Builder, s string) {
	last := 0
	for i := 0; i < len(s); i++ {
		var esc string
		switch s[i] {
		case '&':
			esc = "&amp;"
		case '<':
			esc = "&lt;"
		case '>':
			esc = "&gt;"
		case 0:
			esc = "\ufffd"
		default:
			continue
		}
		if i > last {
			dst.WriteString(s[last:i])
		}
		dst.WriteString(esc)
		last = i + 1
	}
	if last < len(s) {
		dst.WriteString(s[last:])
	}
}
