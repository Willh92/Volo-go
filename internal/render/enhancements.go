package render

import (
	"fmt"
	"html"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	anyHeadingTagRe       = regexp.MustCompile(`(?is)<h([1-6])(\b[^>]*)>(.*?)</h[1-6]>`)
	existingIDAttrRe      = regexp.MustCompile(`(?i)\s*\bid="([^"]*)"`)
	genericHeadingRe      = regexp.MustCompile(`(?i)^heading(?:-\d+)?$`)
	headingSlugClean      = regexp.MustCompile(`[^\p{L}\p{M}\p{N}_-]+`)
	tableBlockRe          = regexp.MustCompile(`(?is)<table\b[^>]*>.*?</table>`)
	trBlockRe             = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	thCellRe              = regexp.MustCompile(`(?is)<th\b[^>]*>(.*?)</th>`)
	tdFullRe              = regexp.MustCompile(`(?is)<td(\b[^>]*)>(.*?)</td>`)
	anchorTagOpenRe       = regexp.MustCompile(`(?i)<a\b([^>]*)>`)
	ghStatsImgPlainRe     = regexp.MustCompile(`(?is)(<p>\s*)(<img\b[^>]*\bsrc="([^"]*/github-stats/([a-zA-Z0-9-]+)\.svg)"[^>]*>)(\s*</p>)`)
	ghAlertMarkerRe       = regexp.MustCompile(`(?is)^\s*<p>\s*\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\](?:[ \t]*</p>\s*|(?:[ \t]*<br\s*/?>\s*|\s*\n|[ \t]+))`)
	standaloneAnchorParRe = regexp.MustCompile(`(?is)<p>\s*<a\b([^>]*?\bhref="([^"]+)"[^>]*)>(.*?)</a>\s*</p>`)
	bilibiliBVIDRe        = regexp.MustCompile(`^/video/(BV[0-9A-Za-z]{10})(?:[/?#]|$)`)
	youtubeIDStrictRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	youtubePathIDRe       = regexp.MustCompile(`^/(?:shorts|embed|v)/([A-Za-z0-9_-]{11})(?:[/?#]|$)`)
)

// assignReadableHeadingIDs ensures every <h1>..<h6> carries a unique, readable, CJK-friendly `id`
// so TOC links (`#中文标题` or `#architecture-overview`) scroll smoothly without generic `heading-1` collisions.
func assignReadableHeadingIDs(safeHTML string) string {
	if !strings.Contains(safeHTML, "<h") && !strings.Contains(safeHTML, "<H") {
		return safeHTML
	}
	used := make(map[string]int)
	seq := 0
	return anyHeadingTagRe.ReplaceAllStringFunc(safeHTML, func(full string) string {
		m := anyHeadingTagRe.FindStringSubmatch(full)
		if len(m) < 4 {
			return full
		}
		lvl := m[1]
		attrs := m[2]
		inner := m[3]
		plain := strings.TrimSpace(html.UnescapeString(stripAllTagsRe.ReplaceAllString(inner, "")))

		existingID := ""
		if im := existingIDAttrRe.FindStringSubmatch(attrs); len(im) >= 2 {
			existingID = strings.TrimSpace(im[1])
		}

		baseID := ""
		if existingID != "" &&
			!genericHeadingRe.MatchString(existingID) &&
			!strings.Contains(strings.ToLower(existingID), "voloplaceholder") &&
			isCleanHeadingID(existingID) &&
			!(hasNonASCIILetterOrMark(plain) && !hasNonASCIILetterOrMark(existingID) && strings.EqualFold(existingID, goldmarkASCIIAutoID(plain))) {
			baseID = strings.ToLower(existingID)
		} else {
			baseID = slugifyHeadingText(plain)
		}
		if baseID == "" {
			seq++
			baseID = fmt.Sprintf("section-%d", seq)
		}

		finalID := baseID
		if count := used[baseID]; count > 0 {
			for k := count + 1; ; k++ {
				cand := fmt.Sprintf("%s-%d", baseID, k)
				if used[cand] == 0 {
					finalID = cand
					used[baseID] = k
					break
				}
			}
		} else {
			used[baseID] = 1
		}
		used[finalID] = 1

		cleanAttrs := existingIDAttrRe.ReplaceAllString(attrs, "")
		return fmt.Sprintf(`<h%s id="%s"%s>%s</h%s>`, lvl, html.EscapeString(finalID), cleanAttrs, inner, lvl)
	})
}

func hasNonASCIILetterOrMark(s string) bool {
	for _, r := range s {
		if r >= 0x80 && (unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r)) {
			return true
		}
	}
	return false
}

func goldmarkASCIIAutoID(plain string) string {
	var b strings.Builder
	for i := 0; i < len(plain); i++ {
		c := plain[i]
		if c >= 'A' && c <= 'Z' {
			b.WriteByte(c + ('a' - 'A'))
		} else if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		} else if c == ' ' || c == '\t' {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func isCleanHeadingID(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return s != ""
}

func slugifyHeadingText(plain string) string {
	s := strings.ToLower(strings.TrimSpace(plain))
	s = headingSlugClean.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if utf8.RuneCountInString(s) > 64 {
		runes := []rune(s)
		end := 64
		for end < len(runes) && unicode.IsMark(runes[end]) {
			end++
		}
		s = strings.Trim(string(runes[:end]), "-")
	}
	return s
}

// wrapTablesInScroll wraps standalone <table>...</table> blocks in <div class="volo-table-scroll">...</div>
// so comment, summary, basic, and notice-board tables never overflow narrow 320px viewports, while skipping
// code/script/style blocks and tables already wrapped in .volo-table-scroll.
func wrapTablesInScroll(htmlSrc string) string {
	if !strings.Contains(htmlSrc, "<table") && !strings.Contains(htmlSrc, "<TABLE") {
		return htmlSrc
	}
	var out strings.Builder
	out.Grow(len(htmlSrc) + 64)

	skipCloseTag := ""
	scrollDivDepth := 0

	for i := 0; i < len(htmlSrc); {
		if htmlSrc[i] != '<' {
			next := strings.IndexByte(htmlSrc[i:], '<')
			if next < 0 {
				out.WriteString(htmlSrc[i:])
				break
			}
			out.WriteString(htmlSrc[i : i+next])
			i += next
			continue
		}

		end := strings.IndexByte(htmlSrc[i:], '>')
		if end < 0 {
			out.WriteString(htmlSrc[i:])
			break
		}
		tag := htmlSrc[i : i+end+1]
		lowerTag := strings.ToLower(tag)

		if skipCloseTag != "" {
			if strings.HasPrefix(lowerTag, skipCloseTag) {
				skipCloseTag = ""
			}
			out.WriteString(tag)
			i += end + 1
			continue
		}

		for _, rawName := range []string{"pre", "code", "script", "style", "textarea"} {
			if hasHTMLTagOpen(lowerTag, rawName) && !strings.HasSuffix(lowerTag, "/>") {
				skipCloseTag = "</" + rawName
				break
			}
		}
		if skipCloseTag != "" {
			out.WriteString(tag)
			i += end + 1
			continue
		}

		if scrollDivDepth > 0 {
			if hasHTMLTagOpen(lowerTag, "div") && !strings.HasSuffix(lowerTag, "/>") {
				scrollDivDepth++
			} else if strings.HasPrefix(lowerTag, "</div") {
				scrollDivDepth--
			}
			out.WriteString(tag)
			i += end + 1
			continue
		}

		if hasHTMLTagOpen(lowerTag, "div") && !strings.HasSuffix(lowerTag, "/>") &&
			(strings.Contains(lowerTag, "volo-table-scroll") || strings.Contains(lowerTag, "volo-github-repos-wrap")) {
			scrollDivDepth = 1
			out.WriteString(tag)
			i += end + 1
			continue
		}

		if hasHTMLTagOpen(lowerTag, "table") && !strings.HasSuffix(lowerTag, "/>") {
			// Find matching </table> while handling nested tables.
			tableDepth := 1
			scan := i + end + 1
			tableEnd := -1
			for scan < len(htmlSrc) {
				nextLt := strings.IndexByte(htmlSrc[scan:], '<')
				if nextLt < 0 {
					break
				}
				tPos := scan + nextLt
				tClose := strings.IndexByte(htmlSrc[tPos:], '>')
				if tClose < 0 {
					break
				}
				innerLower := strings.ToLower(htmlSrc[tPos : tPos+tClose+1])
				if hasHTMLTagOpen(innerLower, "table") && !strings.HasSuffix(innerLower, "/>") {
					tableDepth++
				} else if strings.HasPrefix(innerLower, "</table") {
					tableDepth--
					if tableDepth == 0 {
						tableEnd = tPos + tClose + 1
						break
					}
				}
				scan = tPos + tClose + 1
			}
			if tableEnd > i {
				out.WriteString(`<div class="volo-table-scroll">`)
				out.WriteString(htmlSrc[i:tableEnd])
				out.WriteString(`</div>`)
				i = tableEnd
				continue
			}
		}

		out.WriteString(tag)
		i += end + 1
	}

	return out.String()
}

func hasHTMLTagOpen(lowerTag string, name string) bool {
	if !strings.HasPrefix(lowerTag, "<"+name) {
		return false
	}
	if len(lowerTag) <= 1+len(name) {
		return false
	}
	ch := lowerTag[1+len(name)]
	return ch == '>' || ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '/'
}

// enhanceTablesAndGitHubShowcase ports Volo-solo's GitHub showcase image/table enhancements (2f50960)
// and wraps regular prose tables in `.volo-table-scroll` to prevent horizontal overflow and mid-word breaks.
func enhanceTablesAndGitHubShowcase(rendered string) string {
	out := rendered

	// 1. Link standalone `/github-stats/{username}.svg` images to the author's GitHub profile
	if strings.Contains(out, "/github-stats/") {
		out = ghStatsImgPlainRe.ReplaceAllStringFunc(out, func(match string) string {
			sub := ghStatsImgPlainRe.FindStringSubmatch(match)
			if len(sub) < 5 {
				return match
			}
			username := strings.TrimSpace(sub[4])
			if username == "" {
				return match
			}
			profileURL := "https://github.com/" + html.EscapeString(username)
			return fmt.Sprintf(`%s<a href="%s" target="_blank" rel="noopener noreferrer" data-github-profile="1">%s</a>%s`,
				sub[1], profileURL, sub[2], sub[5])
		})
	}

	if !strings.Contains(out, "<table") && !strings.Contains(out, "<TABLE") {
		return out
	}

	// 2. Transform GitHub showcase 5-column tables (`仓库 / 项目简介 / Stars / Forks / 编程语言`)
	//    and wrap all other prose tables in `<div class="volo-table-scroll">`.
	out = tableBlockRe.ReplaceAllStringFunc(out, func(tbl string) string {
		if isGitHubReposTable(tbl) {
			return transformGitHubReposTable(tbl)
		}
		return `<div class="volo-table-scroll">` + tbl + `</div>`
	})
	return out
}

func isGitHubReposTable(tbl string) bool {
	firstTR := trBlockRe.FindStringSubmatch(tbl)
	if len(firstTR) < 2 {
		return false
	}
	ths := thCellRe.FindAllStringSubmatch(firstTR[1], -1)
	if len(ths) == 5 {
		h0 := cleanCellText(ths[0][1])
		h1 := cleanCellText(ths[1][1])
		h2 := strings.ToLower(cleanCellText(ths[2][1]))
		h3 := strings.ToLower(cleanCellText(ths[3][1]))
		h4 := cleanCellText(ths[4][1])
		if (h0 == "仓库" || strings.EqualFold(h0, "repository") || h0 == "项目") &&
			(h1 == "项目简介" || h1 == "简介" || strings.EqualFold(h1, "description")) &&
			h2 == "stars" && h3 == "forks" &&
			(h4 == "编程语言" || h4 == "主要语言" || strings.EqualFold(h4, "language")) {
			return true
		}
	}
	return false
}

func cleanCellText(rawHTML string) string {
	return strings.TrimSpace(html.UnescapeString(stripAllTagsRe.ReplaceAllString(rawHTML, "")))
}

func transformGitHubReposTable(tbl string) string {
	// Add `class="volo-github-repos-table"` to opening `<table...>`
	openEnd := strings.IndexByte(tbl, '>')
	if openEnd <= 0 {
		return tbl
	}
	openTag := tbl[:openEnd+1]
	bodyAndClose := tbl[openEnd+1:]
	if strings.Contains(openTag, `class="`) {
		openTag = strings.Replace(openTag, `class="`, `class="volo-github-repos-table `, 1)
	} else {
		openTag = openTag[:len(openTag)-1] + ` class="volo-github-repos-table">`
	}

	// Transform tbody rows: col 0 external link attrs, col 1 `.volo-github-repo-desc-cell > .volo-github-repo-desc[title]`
	tbodyIdx := strings.Index(strings.ToLower(bodyAndClose), "<tbody")
	prefix := ""
	targetRows := bodyAndClose
	if tbodyIdx >= 0 {
		prefix = bodyAndClose[:tbodyIdx]
		targetRows = bodyAndClose[tbodyIdx:]
	}

	transformedRows := trBlockRe.ReplaceAllStringFunc(targetRows, func(rowHTML string) string {
		if strings.Contains(strings.ToLower(rowHTML), "<th") {
			return rowHTML
		}
		colIdx := 0
		return tdFullRe.ReplaceAllStringFunc(rowHTML, func(cellHTML string) string {
			m := tdFullRe.FindStringSubmatch(cellHTML)
			if len(m) < 3 {
				colIdx++
				return cellHTML
			}
			tdAttrs := m[1]
			tdInner := m[2]
			cur := colIdx
			colIdx++

			if cur == 0 {
				// Ensure repo links open in a new tab safely
				fixedInner := anchorTagOpenRe.ReplaceAllStringFunc(tdInner, func(aOpen string) string {
					res := aOpen
					if !strings.Contains(strings.ToLower(res), "target=") {
						res = res[:len(res)-1] + ` target="_blank" rel="noopener noreferrer">`
					}
					return res
				})
				return `<td` + tdAttrs + `>` + fixedInner + `</td>`
			}
			if cur == 1 {
				plainDesc := cleanCellText(tdInner)
				newAttrs := tdAttrs
				if strings.Contains(newAttrs, `class="`) {
					newAttrs = strings.Replace(newAttrs, `class="`, `class="volo-github-repo-desc-cell `, 1)
				} else {
					newAttrs += ` class="volo-github-repo-desc-cell"`
				}
				if plainDesc != "" && !strings.Contains(tdInner, "volo-github-repo-desc") {
					return fmt.Sprintf(`<td%s><div class="volo-github-repo-desc" title="%s">%s</div></td>`,
						newAttrs, html.EscapeString(plainDesc), tdInner)
				}
				return `<td` + newAttrs + `>` + tdInner + `</td>`
			}
			return cellHTML
		})
	})

	return `<div class="volo-github-repos-wrap">` + openTag + prefix + transformedRows + `</div>`
}

func alertDefaultTitle(kindLower string, locale string) string {
	isZH := strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "zh")
	switch kindLower {
	case "note":
		if isZH {
			return "说明"
		}
		return "Note"
	case "tip":
		if isZH {
			return "提示"
		}
		return "Tip"
	case "important":
		if isZH {
			return "重要"
		}
		return "Important"
	case "warning":
		if isZH {
			return "警告"
		}
		return "Warning"
	case "caution":
		if isZH {
			return "注意"
		}
		return "Caution"
	default:
		return "Note"
	}
}

// transformGitHubAlerts converts GitHub-flavored alert blockquotes (`> [!NOTE]`, `> [!TIP]`,
// `> [!IMPORTANT]`, `> [!WARNING]`, `> [!CAUTION]`) into semantic `.volo-callout` blocks,
// safely handling nested blockquotes from innermost to outermost.
func transformGitHubAlerts(rendered string, locale string) string {
	if !strings.Contains(rendered, "[!") || !strings.Contains(rendered, "<blockquote") {
		return rendered
	}
	out := rendered
	for pass := 0; pass < 8; pass++ {
		changed := false
		var b strings.Builder
		pos := 0
		for pos < len(out) {
			openRel := strings.Index(out[pos:], "<blockquote>")
			if openRel < 0 {
				b.WriteString(out[pos:])
				break
			}
			openIdx := pos + openRel
			innerStart := openIdx + len("<blockquote>")
			// Find the matching </blockquote> for this <blockquote>
			depth := 1
			scan := innerStart
			closeIdx := -1
			for scan < len(out) {
				nextOpen := strings.Index(out[scan:], "<blockquote>")
				nextClose := strings.Index(out[scan:], "</blockquote>")
				if nextClose < 0 {
					break
				}
				if nextOpen >= 0 && nextOpen < nextClose {
					depth++
					scan += nextOpen + len("<blockquote>")
				} else {
					depth--
					if depth == 0 {
						closeIdx = scan + nextClose
						break
					}
					scan += nextClose + len("</blockquote>")
				}
			}
			if closeIdx < 0 {
				b.WriteString(out[pos:])
				break
			}
			innerHTML := out[innerStart:closeIdx]
			if m := ghAlertMarkerRe.FindStringSubmatchIndex(innerHTML); m != nil {
				kindLower := strings.ToLower(innerHTML[m[2]:m[3]])
				matchedPrefix := innerHTML[m[0]:m[1]]
				rest := innerHTML[m[1]:]
				var bodyHTML string
				if strings.HasSuffix(strings.TrimSpace(matchedPrefix), "</p>") {
					bodyHTML = strings.TrimSpace(rest)
				} else {
					bodyHTML = "<p>" + strings.TrimLeft(rest, " \t\r\n")
				}
				role := "note"
				if kindLower == "warning" || kindLower == "caution" {
					role = "alert"
				}
				title := alertDefaultTitle(kindLower, locale)
				b.WriteString(out[pos:openIdx])
				b.WriteString(`<aside class="volo-callout volo-callout--` + kindLower + `" data-callout="` + kindLower + `" role="` + role + `">`)
				b.WriteString(`<p class="volo-callout__title">` + html.EscapeString(title) + `</p>`)
				if bodyHTML != "" && bodyHTML != "<p></p>" {
					b.WriteString(`<div class="volo-callout__body">` + bodyHTML + `</div>`)
				}
				b.WriteString(`</aside>`)
				pos = closeIdx + len("</blockquote>")
				changed = true
				continue
			}
			b.WriteString(out[pos : openIdx+len("<blockquote>")])
			pos = innerStart
		}
		out = b.String()
		if !changed || !strings.Contains(out, "[!") {
			break
		}
	}
	return out
}

func isASCIIWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func isBlockBoundaryTag(lowerTag string) bool {
	trimmed := strings.TrimPrefix(lowerTag, "</")
	if trimmed == lowerTag {
		trimmed = strings.TrimPrefix(lowerTag, "<")
	}
	end := 0
	for end < len(trimmed) {
		ch := trimmed[end]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			end++
			continue
		}
		break
	}
	switch trimmed[:end] {
	case "p", "div", "li", "ul", "ol", "dl", "dt", "dd",
		"table", "thead", "tbody", "tfoot", "tr", "th", "td",
		"blockquote", "pre", "hr", "br",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"section", "aside", "figure", "figcaption", "details", "summary", "nav":
		return true
	}
	return false
}

func isURLPrevTokenByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
		b == '_' || b == ')' || b == ']' || b == '}'
}

func isPathSegChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
		b == '_' || b == '-' || b == '.' || b == '~' || b == '%'
}

func scanURLBodyEnd(s string, start int) int {
	parenDepth := 0
	bracketDepth := 0
	q := start
	for q < len(s) {
		b := s[q]
		if isASCIIWhitespace(b) || b <= 0x1f || b == 0x7f || b == '<' || b == '>' || b == '"' || b == '\'' || b == '`' || b == '\\' {
			break
		}
		if b >= 0x80 {
			r, w := utf8.DecodeRuneInString(s[q:])
			if unicode.IsSpace(r) || strings.ContainsRune("，。；：？！、（）【】「」『』《》", r) {
				break
			}
			q += w
			continue
		}
		if b == '(' {
			parenDepth++
		} else if b == ')' {
			if parenDepth == 0 {
				break
			}
			parenDepth--
		} else if b == '[' {
			bracketDepth++
		} else if b == ']' {
			if bracketDepth == 0 {
				break
			}
			bracketDepth--
		}
		q++
	}
	// Trim trailing ASCII sentence punctuation that isn't part of the URL itself.
	for q > start {
		last := s[q-1]
		if last == '.' || last == ',' || last == ';' || last == '!' || last == '?' || last == ':' {
			q--
			continue
		}
		break
	}
	return q
}

func scanPathTokenEnd(s string, start int) int {
	q := start
	for q < len(s) {
		b := s[q]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			b == '/' || b == '_' || b == '-' || b == '.' || b == '~' || b == '^' ||
			b == '+' || b == '%' || b == '@' || b == '=' || b == '&' || b == '?' || b == '#' {
			q++
			continue
		}
		break
	}
	for q > start && (s[q-1] == '.' || s[q-1] == ',') {
		q--
	}
	return q
}

// skipURLOrPathToken returns the end index > p if s[p:] begins a URL (`scheme://...`, `mailto:...`, `www....`)
// or file/relative path (`~/...`, `./...`, `../...`, `/~...`, `/dir/...`), or -1 otherwise.
func skipURLOrPathToken(s string, p int) int {
	if p >= len(s) {
		return -1
	}
	if s[p] == '/' && p+1 < len(s) && (s[p+1] == '~' || s[p+1] == '^') && (p == 0 || (s[p-1] != '<' && s[p-1] != '/')) {
		return scanPathTokenEnd(s, p+1)
	}
	if p > 0 && isURLPrevTokenByte(s[p-1]) {
		return -1
	}

	b0 := s[p]
	if (b0 >= 'a' && b0 <= 'z') || (b0 >= 'A' && b0 <= 'Z') {
		if len(s)-p >= 5 && strings.EqualFold(s[p:p+4], "www.") && isPathSegChar(s[p+4]) {
			if end := scanURLBodyEnd(s, p+4); end > p+4 {
				return end
			}
		}
		k := p + 1
		for k < len(s) && k-p < 32 {
			ch := s[k]
			if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
				ch == '+' || ch == '-' || ch == '.' {
				k++
				continue
			}
			break
		}
		if k > p {
			if strings.HasPrefix(s[k:], "://") && k+3 < len(s) {
				if end := scanURLBodyEnd(s, k+3); end > k+3 {
					return end
				}
			}
			if k < len(s) && s[k] == ':' && strings.EqualFold(s[p:k], "mailto") && k+1 < len(s) {
				if end := scanURLBodyEnd(s, k+1); end > k+1 {
					return end
				}
			}
		}
	}

	if b0 == '~' && p+2 < len(s) && s[p+1] == '/' && isPathSegChar(s[p+2]) {
		return scanPathTokenEnd(s, p+2)
	}
	if b0 == '.' {
		if p+2 < len(s) && s[p+1] == '/' && isPathSegChar(s[p+2]) {
			return scanPathTokenEnd(s, p+2)
		}
		if p+3 < len(s) && s[p+1] == '.' && s[p+2] == '/' && isPathSegChar(s[p+3]) {
			return scanPathTokenEnd(s, p+3)
		}
	}
	if b0 == '/' && (p == 0 || (s[p-1] != '<' && s[p-1] != '/')) && p+1 < len(s) && isPathSegChar(s[p+1]) {
		end := scanPathTokenEnd(s, p+1)
		// Treat `/...` as a path when starting with `/~` or containing another `/` or `~` or `.`.
		seg := s[p:end]
		if s[p+1] == '~' || strings.IndexByte(seg[1:], '/') >= 0 || strings.IndexByte(seg[1:], '~') >= 0 || strings.IndexByte(seg[1:], '.') >= 0 {
			return end
		}
	}
	return -1
}

// scanHTMLTagOrCommentEnd returns the index immediately after a raw HTML tag `<...>` (honoring quoted attributes
// that contain `>`) or HTML comment `<!-- ... -->`, or -1 if s[start:] is not a complete tag/comment.
func scanHTMLTagOrCommentEnd(s string, start int) int {
	if start+1 >= len(s) || s[start] != '<' {
		return -1
	}
	if strings.HasPrefix(s[start:], "<!--") {
		if rel := strings.Index(s[start+4:], "-->"); rel >= 0 {
			return start + 4 + rel + 3
		}
		return -1
	}
	c1 := s[start+1]
	if !((c1 >= 'a' && c1 <= 'z') || (c1 >= 'A' && c1 <= 'Z') || c1 == '/' || c1 == '!' || c1 == '?') {
		return -1
	}
	var quote byte
	for k := start + 2; k < len(s); k++ {
		ch := s[k]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			// Enter quoted attribute mode when preceded by `=` (ignoring spaces/tabs).
			prev := k - 1
			for prev > start && (s[prev] == ' ' || s[prev] == '\t' || s[prev] == '\n' || s[prev] == '\r') {
				prev--
			}
			if s[prev] == '=' {
				quote = ch
			}
			continue
		}
		if ch == '>' {
			return k + 1
		}
		if ch == '<' {
			return -1
		}
	}
	return -1
}

func rewriteSupSubText(text string) string {
	if !strings.Contains(text, "^") && !strings.Contains(text, "~") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + 16)
	for p := 0; p < len(text); {
		if end := skipURLOrPathToken(text, p); end > p {
			b.WriteString(text[p:end])
			p = end
			continue
		}
		if p+2 < len(text) && text[p] == '^' &&
			(p == 0 || (text[p-1] != '^' && text[p-1] != '\\' && text[p-1] != '/')) &&
			text[p+1] != '^' && text[p+1] != '/' && !isASCIIWhitespace(text[p+1]) {
			limit := len(text)
			if p+48 < limit {
				limit = p + 48
			}
			closed := -1
			for q := p + 1; q < limit; q++ {
				if isASCIIWhitespace(text[q]) || text[q] == '>' || text[q] == '/' {
					break
				}
				if text[q] == '^' {
					if q > p+1 && text[q-1] != '\\' && (q+1 == len(text) || text[q+1] != '^') {
						closed = q
					}
					break
				}
			}
			if closed > p+1 {
				b.WriteString("<sup>")
				b.WriteString(text[p+1 : closed])
				b.WriteString("</sup>")
				p = closed + 1
				continue
			}
		}
		if p+2 < len(text) && text[p] == '~' &&
			(p == 0 || (text[p-1] != '~' && text[p-1] != '\\' && text[p-1] != '/')) &&
			text[p+1] != '~' && text[p+1] != '/' && !isASCIIWhitespace(text[p+1]) {
			limit := len(text)
			if p+48 < limit {
				limit = p + 48
			}
			closed := -1
			for q := p + 1; q < limit; q++ {
				if isASCIIWhitespace(text[q]) || text[q] == '>' || text[q] == '/' {
					break
				}
				if text[q] == '~' {
					if q > p+1 && text[q-1] != '\\' && (q+1 == len(text) || text[q+1] != '~') {
						closed = q
					}
					break
				}
			}
			if closed > p+1 {
				b.WriteString("<sub>")
				b.WriteString(text[p+1 : closed])
				b.WriteString("</sub>")
				p = closed + 1
				continue
			}
		}
		b.WriteByte(text[p])
		p++
	}
	return b.String()
}

func rewriteExtendedTextSegment(text string) string {
	if !strings.Contains(text, "==") {
		return rewriteSupSubText(text)
	}
	var b strings.Builder
	b.Grow(len(text) + 24)
	lastFlush := 0
	for p := 0; p < len(text); {
		if end := skipURLOrPathToken(text, p); end > p {
			p = end
			continue
		}
		if p+3 < len(text) && text[p] == '=' && text[p+1] == '=' &&
			(p == 0 || text[p-1] != '=') &&
			text[p+2] != '=' && !isASCIIWhitespace(text[p+2]) {
			closed := -1
			for q := p + 2; q+1 < len(text); q++ {
				if text[q] == '\n' || text[q] == '\r' {
					break
				}
				if text[q] == '=' && text[q+1] == '=' &&
					q > p+2 && text[q-1] != '=' && !isASCIIWhitespace(text[q-1]) &&
					(q+2 == len(text) || text[q+2] != '=') {
					closed = q
					break
				}
			}
			if closed > p+2 {
				if p > lastFlush {
					b.WriteString(rewriteSupSubText(text[lastFlush:p]))
				}
				b.WriteString("<mark>")
				b.WriteString(rewriteSupSubText(text[p+2 : closed]))
				b.WriteString("</mark>")
				p = closed + 2
				lastFlush = p
				continue
			}
		}
		p++
	}
	if lastFlush < len(text) {
		b.WriteString(rewriteSupSubText(text[lastFlush:]))
	}
	return b.String()
}

// enhanceExtendedInlineFormatting converts `==highlight==`, `^sup^`, and `~sub~` into `<mark>`, `<sup>`, and `<sub>`
// outside `<pre>`, `<code>`, `<kbd>`, math blocks/spans, HTML tag attributes, and URLs/paths.
func enhanceExtendedInlineFormatting(rendered string) string {
	if !strings.Contains(rendered, "==") && !strings.Contains(rendered, "^") && !strings.Contains(rendered, "~") {
		return rendered
	}
	var out strings.Builder
	out.Grow(len(rendered) + 64)

	skipCloseTag := ""
	anchorDepth := 0

	for i := 0; i < len(rendered); {
		if rendered[i] != '<' {
			next := strings.IndexByte(rendered[i:], '<')
			var text string
			if next < 0 {
				text = rendered[i:]
				i = len(rendered)
			} else {
				text = rendered[i : i+next]
				i += next
			}
			if skipCloseTag != "" {
				out.WriteString(text)
				continue
			}
			if anchorDepth > 0 {
				trimmed := strings.TrimSpace(text)
				if strings.Contains(trimmed, "://") ||
					strings.HasPrefix(strings.ToLower(trimmed), "www.") ||
					strings.HasPrefix(strings.ToLower(trimmed), "mailto:") ||
					strings.HasPrefix(trimmed, "/") ||
					strings.HasPrefix(trimmed, "~/") ||
					strings.HasPrefix(trimmed, "./") ||
					strings.HasPrefix(trimmed, "../") {
					out.WriteString(text)
					continue
				}
			}
			out.WriteString(rewriteExtendedTextSegment(text))
			continue
		}

		tagEnd := scanHTMLTagOrCommentEnd(rendered, i)
		if tagEnd < 0 {
			if gt := strings.IndexByte(rendered[i:], '>'); gt >= 0 {
				tagEnd = i + gt + 1
			} else {
				out.WriteString(rendered[i:])
				break
			}
		}
		tag := rendered[i:tagEnd]
		lowerTag := strings.ToLower(tag)
		i = tagEnd

		if skipCloseTag != "" {
			if strings.HasPrefix(lowerTag, skipCloseTag) {
				skipCloseTag = ""
			}
			out.WriteString(tag)
			continue
		}

		for _, rawName := range []string{"pre", "code", "script", "style", "textarea", "kbd"} {
			if hasHTMLTagOpen(lowerTag, rawName) && !strings.HasSuffix(lowerTag, "/>") {
				skipCloseTag = "</" + rawName
				break
			}
		}
		if skipCloseTag == "" && !strings.HasSuffix(lowerTag, "/>") {
			if hasHTMLTagOpen(lowerTag, "span") && strings.Contains(lowerTag, "volo-math-inline") {
				skipCloseTag = "</span"
			} else if hasHTMLTagOpen(lowerTag, "div") && strings.Contains(lowerTag, "volo-math-block") {
				skipCloseTag = "</div"
			}
		}
		if skipCloseTag == "" {
			if hasHTMLTagOpen(lowerTag, "a") && !strings.HasSuffix(lowerTag, "/>") {
				anchorDepth++
			} else if strings.HasPrefix(lowerTag, "</a") && anchorDepth > 0 {
				anchorDepth--
			}
		}
		out.WriteString(tag)
	}

	pass1 := out.String()
	if !strings.Contains(pass1, "==") {
		return pass1
	}
	return rewriteCrossTagHighlights(pass1)
}

func rewriteCrossTagHighlights(htmlSrc string) string {
	var finalOut strings.Builder
	finalOut.Grow(len(htmlSrc) + 32)

	var markBuf strings.Builder
	inOpenMark := false
	lastMarkByteGt := false
	inlineDepth := 0
	skipCloseTag := ""

	abortOpenMark := func() {
		if inOpenMark {
			finalOut.WriteString("==")
			finalOut.WriteString(markBuf.String())
			markBuf.Reset()
			inOpenMark = false
			lastMarkByteGt = false
			inlineDepth = 0
		}
	}

	writeTargetByte := func(ch byte) {
		if inOpenMark {
			markBuf.WriteByte(ch)
			lastMarkByteGt = (ch == '>')
		} else {
			finalOut.WriteByte(ch)
		}
	}

	writeTargetString := func(s string) {
		if len(s) == 0 {
			return
		}
		if inOpenMark {
			markBuf.WriteString(s)
			lastMarkByteGt = (s[len(s)-1] == '>')
		} else {
			finalOut.WriteString(s)
		}
	}

	for i := 0; i < len(htmlSrc); {
		if htmlSrc[i] != '<' {
			next := strings.IndexByte(htmlSrc[i:], '<')
			var text string
			if next < 0 {
				text = htmlSrc[i:]
				i = len(htmlSrc)
			} else {
				text = htmlSrc[i : i+next]
				i += next
			}
			if skipCloseTag != "" {
				writeTargetString(text)
				continue
			}
			for p := 0; p < len(text); {
				if text[p] == '\n' {
					abortOpenMark()
					finalOut.WriteByte('\n')
					p++
					continue
				}
				if p+1 < len(text) && text[p] == '=' && text[p+1] == '=' &&
					(p == 0 || text[p-1] != '=') && (p+2 == len(text) || text[p+2] != '=') {
					if inOpenMark && inlineDepth == 0 {
						prevOk := (p > 0 && !isASCIIWhitespace(text[p-1])) || (p == 0 && markBuf.Len() > 0 && lastMarkByteGt)
						if prevOk {
							finalOut.WriteString("<mark>")
							finalOut.WriteString(markBuf.String())
							finalOut.WriteString("</mark>")
							markBuf.Reset()
							inOpenMark = false
							lastMarkByteGt = false
							inlineDepth = 0
							p += 2
							continue
						}
					} else if !inOpenMark {
						nextOk := (p+2 < len(text) && !isASCIIWhitespace(text[p+2])) || (p+2 == len(text) && i < len(htmlSrc) && htmlSrc[i] == '<')
						if nextOk {
							inOpenMark = true
							lastMarkByteGt = false
							inlineDepth = 0
							markBuf.Reset()
							p += 2
							continue
						}
					}
				}
				writeTargetByte(text[p])
				p++
			}
			continue
		}

		tagEnd := scanHTMLTagOrCommentEnd(htmlSrc, i)
		if tagEnd < 0 {
			if gt := strings.IndexByte(htmlSrc[i:], '>'); gt >= 0 {
				tagEnd = i + gt + 1
			} else {
				writeTargetString(htmlSrc[i:])
				break
			}
		}
		tag := htmlSrc[i:tagEnd]
		lowerTag := strings.ToLower(tag)
		i = tagEnd

		if skipCloseTag != "" {
			if strings.HasPrefix(lowerTag, skipCloseTag) {
				skipCloseTag = ""
			}
			writeTargetString(tag)
			continue
		}

		if isBlockBoundaryTag(lowerTag) ||
			(hasHTMLTagOpen(lowerTag, "div") && strings.Contains(lowerTag, "volo-math-block")) {
			abortOpenMark()
			finalOut.WriteString(tag)
			for _, blockSkip := range []string{"pre", "script", "style", "textarea"} {
				if hasHTMLTagOpen(lowerTag, blockSkip) && !strings.HasSuffix(lowerTag, "/>") {
					skipCloseTag = "</" + blockSkip
					break
				}
			}
			continue
		}

		for _, inlineSkip := range []string{"code", "kbd"} {
			if hasHTMLTagOpen(lowerTag, inlineSkip) && !strings.HasSuffix(lowerTag, "/>") {
				skipCloseTag = "</" + inlineSkip
				break
			}
		}
		if skipCloseTag == "" && hasHTMLTagOpen(lowerTag, "span") && strings.Contains(lowerTag, "volo-math-inline") && !strings.HasSuffix(lowerTag, "/>") {
			skipCloseTag = "</span"
		}

		if skipCloseTag == "" && inOpenMark && !strings.HasPrefix(lowerTag, "<!") && !strings.HasSuffix(lowerTag, "/>") {
			if strings.HasPrefix(lowerTag, "</") {
				inlineDepth--
				if inlineDepth < 0 {
					abortOpenMark()
					finalOut.WriteString(tag)
					continue
				}
			} else {
				inlineDepth++
			}
		}
		writeTargetString(tag)
	}
	abortOpenMark()
	return finalOut.String()
}

// enhanceStandaloneMediaLinks upgrades standalone single-anchor paragraphs pointing to audio/video files
// or Bilibili/YouTube URLs into native HTML5 media players or strictly sandboxed allowlisted embeds.
func enhanceStandaloneMediaLinks(rendered string) string {
	if !strings.Contains(rendered, "<a ") && !strings.Contains(rendered, "<A ") {
		return rendered
	}
	return standaloneAnchorParRe.ReplaceAllStringFunc(rendered, func(match string) string {
		m := standaloneAnchorParRe.FindStringSubmatch(match)
		if len(m) < 4 {
			return match
		}
		innerHTML := m[3]
		lowerInner := strings.ToLower(innerHTML)
		if strings.Contains(lowerInner, "<img") || strings.Contains(lowerInner, "<svg") ||
			strings.Contains(lowerInner, "<video") || strings.Contains(lowerInner, "<audio") {
			return match
		}
		rawHref := strings.TrimSpace(html.UnescapeString(m[2]))
		if rawHref == "" || !safeMediaSrcAttrRe.MatchString(rawHref) {
			return match
		}
		u, err := url.Parse(rawHref)
		if err != nil {
			return match
		}
		label := cleanCellText(innerHTML)
		if label == "" {
			label = rawHref
		}
		escHref := html.EscapeString(rawHref)
		escLabel := html.EscapeString(label)

		ext := strings.ToLower(path.Ext(u.Path))
		switch ext {
		case ".mp4", ".webm", ".m4v", ".mov", ".ogv":
			return fmt.Sprintf(`<figure class="volo-media-figure" data-md-media-href="%s" data-md-media-label="%s"><video controls preload="metadata" playsinline src="%s"></video></figure>`,
				escHref, escLabel, escHref)
		case ".mp3", ".wav", ".ogg", ".m4a", ".weba", ".flac", ".aac", ".opus":
			return fmt.Sprintf(`<figure class="volo-media-figure volo-media-figure--audio" data-md-media-href="%s" data-md-media-label="%s"><audio controls preload="metadata" src="%s"></audio></figure>`,
				escHref, escLabel, escHref)
		}

		if u.Scheme == "https" || u.Scheme == "http" {
			host := strings.ToLower(u.Hostname())
			if host == "bilibili.com" || host == "www.bilibili.com" || host == "m.bilibili.com" {
				if bm := bilibiliBVIDRe.FindStringSubmatch(u.Path); len(bm) >= 2 {
					bvid := bm[1]
					embedSrc := "https://player.bilibili.com/player.html?bvid=" + bvid + "&amp;autoplay=0"
					return fmt.Sprintf(`<figure class="volo-media-embed" data-md-media-href="%s" data-md-media-label="%s"><iframe src="%s" loading="lazy" allowfullscreen referrerpolicy="no-referrer" sandbox="allow-scripts allow-same-origin allow-presentation" title="%s"></iframe></figure>`,
						escHref, escLabel, embedSrc, escLabel)
				}
			}
			if host == "youtube.com" || host == "www.youtube.com" || host == "m.youtube.com" || host == "youtu.be" {
				vid := ""
				if host == "youtu.be" {
					cand := strings.Trim(u.Path, "/")
					if idx := strings.IndexByte(cand, '/'); idx >= 0 {
						cand = cand[:idx]
					}
					if youtubeIDStrictRe.MatchString(cand) {
						vid = cand
					}
				} else if u.Path == "/watch" {
					cand := u.Query().Get("v")
					if youtubeIDStrictRe.MatchString(cand) {
						vid = cand
					}
				} else if ym := youtubePathIDRe.FindStringSubmatch(u.Path); len(ym) >= 2 {
					vid = ym[1]
				}
				if vid != "" {
					embedSrc := "https://www.youtube-nocookie.com/embed/" + vid
					return fmt.Sprintf(`<figure class="volo-media-embed" data-md-media-href="%s" data-md-media-label="%s"><iframe src="%s" loading="lazy" allowfullscreen referrerpolicy="strict-origin-when-cross-origin" sandbox="allow-scripts allow-same-origin allow-presentation" title="%s"></iframe></figure>`,
						escHref, escLabel, embedSrc, escLabel)
				}
			}
		}
		return match
	})
}

func renderInlineTOC(toc []TOCItem, locale string) string {
	return renderInlineTOCWithDirective(toc, locale, "")
}

func renderInlineTOCWithDirective(toc []TOCItem, locale string, directiveB64 string) string {
	if len(toc) == 0 {
		if directiveB64 != "" {
			return `<nav class="volo-inline-toc" data-md-toc="1" contenteditable="false" data-volo-md-b64="` + directiveB64 + `">TOC</nav>`
		}
		return ""
	}
	title := "Contents"
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "zh") {
		title = "目录"
	}
	minLevel := 6
	for _, item := range toc {
		if item.Level > 0 && item.Level < minLevel {
			minLevel = item.Level
		}
	}
	var b strings.Builder
	b.WriteString(`<nav class="volo-inline-toc" data-md-toc="1"`)
	if directiveB64 != "" {
		b.WriteString(` contenteditable="false" data-volo-md-b64="` + directiveB64 + `"`)
	}
	b.WriteString(` aria-label="` + html.EscapeString(title) + `">`)
	b.WriteString(`<p class="volo-inline-toc__title">` + html.EscapeString(title) + `</p>`)
	b.WriteString(`<ul class="volo-inline-toc__list">`)
	for _, item := range toc {
		indent := item.Level - minLevel
		if indent < 0 {
			indent = 0
		}
		if indent > 4 {
			indent = 4
		}
		b.WriteString(fmt.Sprintf(`<li class="volo-inline-toc__item volo-inline-toc__item--d%d"><a href="#%s">%s</a></li>`,
			indent, html.EscapeString(item.ID), html.EscapeString(item.Text)))
	}
	b.WriteString(`</ul></nav>`)
	return b.String()
}
