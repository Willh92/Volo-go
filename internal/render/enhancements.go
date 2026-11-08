package render

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	anyHeadingTagRe   = regexp.MustCompile(`(?is)<h([1-6])(\b[^>]*)>(.*?)</h[1-6]>`)
	existingIDAttrRe  = regexp.MustCompile(`(?i)\s*\bid="([^"]*)"`)
	genericHeadingRe  = regexp.MustCompile(`(?i)^heading(?:-\d+)?$`)
	headingSlugClean  = regexp.MustCompile(`[^\p{L}\p{N}_-]+`)
	tableBlockRe      = regexp.MustCompile(`(?is)<table\b[^>]*>.*?</table>`)
	trBlockRe         = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	thCellRe          = regexp.MustCompile(`(?is)<th\b[^>]*>(.*?)</th>`)
	tdFullRe          = regexp.MustCompile(`(?is)<td(\b[^>]*)>(.*?)</td>`)
	anchorTagOpenRe   = regexp.MustCompile(`(?i)<a\b([^>]*)>`)
	ghStatsImgPlainRe = regexp.MustCompile(`(?is)(<p>\s*)(<img\b[^>]*\bsrc="([^"]*/github-stats/([a-zA-Z0-9-]+)\.svg)"[^>]*>)(\s*</p>)`)
	ghAlertMarkerRe   = regexp.MustCompile(`(?is)^\s*<p>\s*\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\](?:[ \t]*</p>\s*|(?:[ \t]*<br\s*/?>\s*|\s*\n|[ \t]+))`)
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
		if existingID != "" && !genericHeadingRe.MatchString(existingID) && isCleanHeadingID(existingID) {
			baseID = existingID
		} else {
			baseID = slugifyHeadingText(plain)
		}
		if baseID == "" {
			seq++
			baseID = fmt.Sprintf("section-%d", seq)
		}

		finalID := baseID
		if count := used[strings.ToLower(baseID)]; count > 0 {
			for k := count + 1; ; k++ {
				cand := fmt.Sprintf("%s-%d", baseID, k)
				if used[strings.ToLower(cand)] == 0 {
					finalID = cand
					used[strings.ToLower(baseID)] = k
					break
				}
			}
		} else {
			used[strings.ToLower(baseID)] = 1
		}
		used[strings.ToLower(finalID)] = 1

		cleanAttrs := existingIDAttrRe.ReplaceAllString(attrs, "")
		return fmt.Sprintf(`<h%s id="%s"%s>%s</h%s>`, lvl, html.EscapeString(finalID), cleanAttrs, inner, lvl)
	})
}

func isCleanHeadingID(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
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
		s = strings.Trim(string(runes[:64]), "-")
	}
	return s
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

