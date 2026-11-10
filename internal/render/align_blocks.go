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
// not turn into indented code blocks. Fenced code is already replaced by placeholders at this point.
func normalizeAlignmentWrappers(md string) string {
	if !strings.Contains(md, "<") || !strings.Contains(md, "\n") {
		return md
	}
	lowMD := strings.ToLower(md)
	if !strings.Contains(lowMD, "align") && !strings.Contains(lowMD, "<center") {
		return md
	}
	lines := strings.Split(md, "\n")
	openTag := make(map[int]string)
	closeTag := make(map[int]string)

	for i := 0; i < len(lines); i++ {
		if len(lines[i]) > 512 {
			continue
		}
		m := alignWrapperOpenLineRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		tag := strings.ToLower(m[1])
		if tag != "center" && !alignWrapperAttrRe.MatchString(m[2]) && !alignWrapperStyleRe.MatchString(m[2]) {
			continue
		}
		depth := 1
		end := -1
		limit := i + 2000
		if limit > len(lines)-1 {
			limit = len(lines) - 1
		}
		for k := i + 1; k <= limit; k++ {
			low := strings.ToLower(lines[k])
			if !strings.Contains(low, tag) {
				continue
			}
			depth += countHTMLOpenTag(low, tag)
			depth -= strings.Count(low, "</"+tag+">")
			if depth <= 0 {
				end = k
				break
			}
		}
		if end < 0 || strings.TrimSpace(strings.ToLower(lines[end])) != "</"+tag+">" {
			continue
		}
		indented := false
		for k := i + 1; k < end; k++ {
			if strings.HasPrefix(lines[k], "    ") || strings.HasPrefix(lines[k], "\t") {
				indented = true
				break
			}
		}
		if indented {
			continue
		}
		openTag[i] = tag
		closeTag[end] = tag
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

// countHTMLOpenTag counts `<tag>` / `<tag …>` occurrences in an already lower-cased line.
func countHTMLOpenTag(low, tag string) int {
	n := 0
	needle := "<" + tag
	for idx := 0; ; {
		j := strings.Index(low[idx:], needle)
		if j < 0 {
			return n
		}
		pos := idx + j + len(needle)
		if pos >= len(low) || low[pos] == '>' || low[pos] == ' ' || low[pos] == '\t' || low[pos] == '/' {
			n++
		}
		idx = pos
	}
}
