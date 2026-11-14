package render

import (
	"regexp"
	"strings"
)

var (
	alignWrapperOpenLineRe = regexp.MustCompile(`(?i)^ {0,3}<(div|p|center|section)\b([^<>]*)>\s*$`)
	alignWrapperAttrRe     = regexp.MustCompile(`(?i)\balign\s*=\s*["']?(?:left|center|right|justify)\b`)
	alignWrapperStyleRe    = regexp.MustCompile(`(?i)\bstyle\s*=\s*["'][^"']*\btext-align\s*:\s*(?:left|center|right|justify)\b`)
)

type alignStackEntry struct {
	lineIdx   int
	isWrapper bool
}

func isAlignWrapperTagName(t string) bool {
	switch t {
	case "div", "p", "center", "section":
		return true
	default:
		return false
	}
}

// normalizeAlignmentWrappers lets Markdown render inside standalone alignment wrappers such as
//
//	<div align=center>
//	[![Repo](/images/repo.jpg)](https://github.com/x)
//	</div>
//
// CommonMark treats everything up to the next blank line after `<div …>` as a raw HTML block, so the
// inner Markdown would otherwise be printed literally (Solo/Lute and GitHub READMEs commonly omit the
// blank lines). Blank lines are inserted after the opening and around the closing wrapper line, and a
// `<p align=…>` wrapper is promoted to `<div align=…>` because a `<p>` cannot contain the generated
// paragraphs. Blocks whose inner lines are indented by 4+ spaces / tabs are left untouched so they do
// not turn into indented code blocks. Raw `<pre>...</pre>`, HTML comments `<!-- ... -->`, and fenced
// code blocks are tracked in a single O(N) pass so inner tags are never mutated and unclosed wrappers
// never trigger quadratic rescans.
func normalizeAlignmentWrappers(md string) string {
	if !strings.Contains(md, "<") || !strings.Contains(md, "\n") {
		return md
	}
	lowMD := strings.ToLower(md)
	if !strings.Contains(lowMD, "align") && !strings.Contains(lowMD, "<center") {
		return md
	}
	lines := strings.Split(md, "\n")

	lastCommentCloseLine := -1
	lastPreCloseLine := -1
	for idx := len(lines) - 1; idx >= 0 && (lastCommentCloseLine < 0 || lastPreCloseLine < 0); idx-- {
		if lastCommentCloseLine < 0 && strings.Contains(lines[idx], "-->") {
			lastCommentCloseLine = idx
		}
		if lastPreCloseLine < 0 && findCaseInsensitivePreCloseEnd(lines[idx]) >= 0 {
			lastPreCloseLine = idx
		}
	}

	openTag := make(map[int]string)
	closeTag := make(map[int]string)
	stacks := make(map[string][]alignStackEntry, 4)
	lastIndentedLine := -1

	var tracker blockLineTracker
	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceIndent := 0
	fenceQuoteDepth := 0
	inComment := false
	inPre := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if inFence {
			cl := parseChildBlockLine(line, fenceQuoteDepth, fenceIndent, -1)
			if cl.quoteBroken {
				inFence = false
			} else {
				if cl.quoteOK && cl.indentOK {
					if ch, n, fOk := parseFenceStart(cl.trimmed); fOk && ch == fenceChar && n >= fenceLen && strings.TrimSpace(cl.trimmed[n:]) == "" {
						inFence = false
					}
				}
				continue
			}
		}

		if inComment {
			if closeIdx := strings.Index(line, "-->"); closeIdx >= 0 {
				inComment = false
				rem := line[closeIdx+3:]
				if strings.Contains(rem, "<!--") && !strings.Contains(rem[strings.Index(rem, "<!--")+4:], "-->") && i < lastCommentCloseLine {
					inComment = true
				} else if strings.Contains(strings.ToLower(rem), "<pre") {
					lowRem := strings.ToLower(rem)
					if idx := strings.Index(lowRem, "<pre"); idx >= 0 && hasHTMLTagOpen(lowRem[idx:], "pre") && findCaseInsensitivePreCloseEnd(rem[idx:]) < 0 && i < lastPreCloseLine {
						inPre = true
					}
				}
			}
			continue
		}

		if inPre {
			if closeEnd := findCaseInsensitivePreCloseEnd(line); closeEnd >= 0 {
				inPre = false
				rem := line[closeEnd:]
				if strings.Contains(rem, "<!--") && !strings.Contains(rem[strings.Index(rem, "<!--")+4:], "-->") && i < lastCommentCloseLine {
					inComment = true
				} else if strings.Contains(strings.ToLower(rem), "<pre") {
					lowRem := strings.ToLower(rem)
					if idx := strings.Index(lowRem, "<pre"); idx >= 0 && hasHTMLTagOpen(lowRem[idx:], "pre") && findCaseInsensitivePreCloseEnd(rem[idx:]) < 0 && i < lastPreCloseLine {
						inPre = true
					}
				}
			}
			continue
		}

		info := tracker.next(line)
		if info.ok {
			if ch, n, ok := parseFenceStart(info.rest); ok {
				inFence = true
				fenceChar = ch
				fenceLen = n
				fenceIndent = info.activeIndent
				fenceQuoteDepth = info.quoteDepth
				continue
			}
			if strings.HasPrefix(info.rest, "<!--") {
				openPos := strings.Index(line, "<!--")
				if strings.Index(line[openPos+4:], "-->") < 0 {
					if i < lastCommentCloseLine || info.quoteDepth == 0 {
						inComment = true
					}
					continue
				}
			}
			if hasHTMLTagOpen(strings.ToLower(info.rest), "pre") {
				openPos := strings.IndexByte(line, '<')
				tagEnd := scanHTMLTagOrCommentEnd(line, openPos)
				if tagEnd > openPos && !strings.HasSuffix(strings.TrimSpace(line[openPos:tagEnd]), "/>") {
					if findCaseInsensitivePreCloseEnd(line[tagEnd:]) < 0 {
						if i < lastPreCloseLine {
							inPre = true
						}
						continue
					}
				}
			}
		}

		isIndented := strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
		if len(line) <= 512 && !isIndented {
			if m := alignWrapperOpenLineRe.FindStringSubmatch(line); m != nil && !strings.HasSuffix(strings.TrimSpace(m[2]), "/") {
				tag := strings.ToLower(m[1])
				if tag == "center" || alignWrapperAttrRe.MatchString(m[2]) || alignWrapperStyleRe.MatchString(m[2]) {
					stacks[tag] = append(stacks[tag], alignStackEntry{lineIdx: i, isWrapper: true})
					continue
				}
			}
			trimmedLow := strings.ToLower(strings.TrimSpace(line))
			if strings.HasPrefix(trimmedLow, "</") && strings.HasSuffix(trimmedLow, ">") {
				candTag := trimmedLow[2 : len(trimmedLow)-1]
				if isAlignWrapperTagName(candTag) {
					if stk := stacks[candTag]; len(stk) > 0 {
						top := stk[len(stk)-1]
						stacks[candTag] = stk[:len(stk)-1]
						if top.isWrapper && lastIndentedLine <= top.lineIdx && i-top.lineIdx <= 2000 {
							openTag[top.lineIdx] = candTag
							closeTag[i] = candTag
						}
					}
					continue
				}
			}
		}

		if isIndented {
			lastIndentedLine = i
		}

		if strings.IndexByte(line, '<') < 0 {
			continue
		}

		low := strings.ToLower(line)
		for p := 0; p < len(line); {
			lt := strings.IndexByte(line[p:], '<')
			if lt < 0 {
				break
			}
			p += lt
			if strings.HasPrefix(line[p:], "<!--") {
				if rel := strings.Index(line[p+4:], "-->"); rel >= 0 {
					p += 4 + rel + 3
					continue
				}
				if i < lastCommentCloseLine {
					inComment = true
				}
				break
			}
			if hasHTMLTagOpen(low[p:], "pre") {
				tagEnd := scanHTMLTagOrCommentEnd(line, p)
				if tagEnd > p && !strings.HasSuffix(strings.TrimSpace(line[p:tagEnd]), "/>") {
					if closeRel := findCaseInsensitivePreCloseEnd(line[tagEnd:]); closeRel >= 0 {
						p = tagEnd + closeRel
						continue
					}
					if i < lastPreCloseLine {
						inPre = true
					}
					break
				}
			}
			matchedTag := false
			for _, tName := range []string{"div", "p", "center", "section"} {
				if hasHTMLTagOpen(low[p:], tName) {
					tagEnd := scanHTMLTagOrCommentEnd(line, p)
					if tagEnd > p {
						if !strings.HasSuffix(strings.TrimSpace(line[p:tagEnd]), "/>") {
							stacks[tName] = append(stacks[tName], alignStackEntry{lineIdx: i, isWrapper: false})
						}
						p = tagEnd
					} else {
						stacks[tName] = append(stacks[tName], alignStackEntry{lineIdx: i, isWrapper: false})
						p += 1 + len(tName)
					}
					matchedTag = true
					break
				}
				closeLiteral := "</" + tName + ">"
				if strings.HasPrefix(low[p:], closeLiteral) {
					if stk := stacks[tName]; len(stk) > 0 {
						stacks[tName] = stk[:len(stk)-1]
					}
					p += len(closeLiteral)
					matchedTag = true
					break
				}
			}
			if !matchedTag {
				p++
			}
		}
	}

	if len(openTag) == 0 {
		return md
	}

	isBlank := func(s string) bool { return strings.TrimSpace(s) == "" }
	out := make([]string, 0, len(lines)+len(openTag)*4)
	for i, line := range lines {
		if tag, ok := closeTag[i]; ok {
			if len(out) > 0 && !isBlank(out[len(out)-1]) {
				out = append(out, "")
			}
			if tag == "p" {
				line = strings.Replace(strings.ToLower(line), "</p>", "</div>", 1)
			}
			out = append(out, line)
			if i+1 < len(lines) && !isBlank(lines[i+1]) {
				out = append(out, "")
			}
			continue
		}
		if tag, ok := openTag[i]; ok {
			if tag == "p" {
				idx := strings.Index(strings.ToLower(line), "<p")
				line = line[:idx] + "<div" + line[idx+2:]
			}
			if len(out) > 0 && !isBlank(out[len(out)-1]) {
				out = append(out, "")
			}
			out = append(out, line)
			if i+1 < len(lines) && !isBlank(lines[i+1]) {
				out = append(out, "")
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
