package content

import (
	"fmt"
	"html"
	"strings"

	nethtml "golang.org/x/net/html"

	"github.com/Willh92/Volo-go/internal/render"
)

// normalizeBookCodeBlocks converts third-party documentation code block DOM structures
// (VuePress 1/2, VitePress, Shiki, PrismJS, Highlight.js, Hexo/Rouge/Hugo gutter tables,
// GitBook/mdBook toolbars) into Volo's canonical Chroma-compatible markup so line numbers,
// syntax tokens, horizontal scroll gutters, and clean copy buttons align 1:1.
func normalizeBookCodeBlocks(root *nethtml.Node) {
	if root == nil {
		return
	}
	unwrapBookCodeGutterTables(root, root)
	normalizeBookPreContainers(root, root)
}

func unwrapBookCodeGutterTables(n *nethtml.Node, root *nethtml.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == nethtml.ElementNode && strings.EqualFold(c.Data, "table") {
			if codeRoot, rowCells, ok := matchCodeGutterTable(c); ok {
				target := outermostSingleCodeTableWrapper(c, root)
				lang := detectCodeBlockLang(target, c, codeRoot)
				var lines []codeLineFrag
				var hasSyntax bool
				if len(rowCells) > 0 {
					lines = make([]codeLineFrag, 0, len(rowCells))
					for _, cell := range rowCells {
						if hasSyntaxTokenElements(cell) {
							hasSyntax = true
						}
						cellLines := splitInlineCodeDOMIntoLines(cell)
						if len(cellLines) == 0 {
							lines = append(lines, codeLineFrag{})
						} else {
							lines = append(lines, codeLineFrag{
								html:        strings.Join(cellLines, " "),
								highlighted: hasAnyClassToken(getNodeAttr(cell, "class"), "highlighted", "hl", "marked"),
							})
						}
					}
				} else if codeRoot != nil {
					hasSyntax = hasSyntaxTokenElements(codeRoot)
					lines = extractCodeLinesFromNode(codeRoot, 0, nil)
				}
				var canon string
				if !hasSyntax {
					plain := extractPlainLinesFromFrags(lines)
					canon = render.HighlightCodeHTML(plain, lang, true)
				} else {
					canon = buildCanonicalCodeBlockHTML(lines, lang, true)
				}
				replaceNodeWithFragment(target, canon)
				c = next
				continue
			}
		}
		unwrapBookCodeGutterTables(c, root)
		c = next
	}
}

func matchCodeGutterTable(tbl *nethtml.Node) (codeRoot *nethtml.Node, multiRowCodeCells []*nethtml.Node, ok bool) {
	var rows []*nethtml.Node
	var collectRows func(cur *nethtml.Node)
	collectRows = func(cur *nethtml.Node) {
		for ch := cur.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != nethtml.ElementNode {
				continue
			}
			switch strings.ToLower(ch.Data) {
			case "tr":
				rows = append(rows, ch)
			case "tbody", "thead":
				collectRows(ch)
			}
		}
	}
	collectRows(tbl)
	if len(rows) == 0 {
		return nil, nil, false
	}

	rowTDs := func(tr *nethtml.Node) []*nethtml.Node {
		var tds []*nethtml.Node
		for ch := tr.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type == nethtml.ElementNode && strings.EqualFold(ch.Data, "td") {
				tds = append(tds, ch)
			}
		}
		return tds
	}

	if len(rows) == 1 {
		tds := rowTDs(rows[0])
		if len(tds) != 2 {
			return nil, nil, false
		}
		cls0 := getNodeAttr(tds[0], "class")
		cls1 := getNodeAttr(tds[1], "class")
		isGutter := hasAnyClassToken(cls0, "gutter", "rouge-gutter", "lntd", "lineno", "linenos", "line-numbers") ||
			findFirstDescendantWithClass(tds[0], "lineno", "lnt", "ln", "line-number") != nil
		if !isGutter && !hasAnyClassToken(cls1, "code", "rouge-code") {
			return nil, nil, false
		}
		pre1 := findFirstElementByTag(tds[1], "pre")
		if pre1 == nil {
			code1 := findFirstElementByTag(tds[1], "code")
			if code1 == nil {
				return nil, nil, false
			}
			return code1, nil, true
		}
		if code1 := findFirstDirectChildByTag(pre1, "code"); code1 != nil {
			return code1, nil, true
		}
		return pre1, nil, true
	}

	// Multi-row code table (e.g. highlightjs-line-numbers `td.hljs-ln-numbers` + `td.hljs-ln-code` or GitHub `td.blob-num` + `td.blob-code`)
	cells := make([]*nethtml.Node, 0, len(rows))
	for _, tr := range rows {
		tds := rowTDs(tr)
		if len(tds) != 2 {
			return nil, nil, false
		}
		cls0 := getNodeAttr(tds[0], "class")
		cls1 := getNodeAttr(tds[1], "class")
		if !hasAnyClassToken(cls0, "hljs-ln-numbers", "blob-num", "line-num", "gutter") &&
			!hasAnyClassToken(cls1, "hljs-ln-code", "blob-code", "blob-code-inner", "line-code") {
			return nil, nil, false
		}
		cells = append(cells, tds[1])
	}
	return nil, cells, len(cells) > 0
}

func outermostSingleCodeTableWrapper(tbl *nethtml.Node, stopRoot *nethtml.Node) *nethtml.Node {
	cur := tbl
	for cur.Parent != nil && cur.Parent != stopRoot {
		p := cur.Parent
		if p.Type != nethtml.ElementNode {
			break
		}
		tag := strings.ToLower(p.Data)
		if tag != "figure" && tag != "div" {
			break
		}
		if countElementsByTag(p, "table") != 1 || hasProseBlockOutsideCode(p, tbl) {
			break
		}
		cur = p
	}
	return cur
}

func normalizeBookPreContainers(n *nethtml.Node, root *nethtml.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == nethtml.ElementNode {
			if preNode, isWrapper := matchSingleCodeBlockTarget(c, root); preNode != nil {
				target := c
				if !isWrapper {
					target = preNode
				}
				normalizeSingleCodeTarget(target, preNode)
				c = next
				continue
			}
		}
		normalizeBookPreContainers(c, root)
		c = next
	}
}

func matchSingleCodeBlockTarget(n *nethtml.Node, root *nethtml.Node) (preNode *nethtml.Node, isWrapper bool) {
	if n == nil || n == root || n.Type != nethtml.ElementNode {
		return nil, false
	}
	tag := strings.ToLower(n.Data)
	if tag == "pre" {
		return n, false
	}
	if tag != "div" && tag != "figure" {
		return nil, false
	}
	// Already canonical Volo figure?
	if tag == "figure" && hasAnyClassToken(getNodeAttr(n, "class"), "volo-codeblock") {
		if pre := findFirstDirectChildByTag(n, "pre"); pre != nil && isAlreadyCanonicalChroma(pre) {
			return nil, false
		}
	}
	if countElementsByTag(n, "pre") != 1 {
		return nil, false
	}
	pre := findFirstElementByTag(n, "pre")
	if pre == nil || hasProseBlockOutsideCode(n, pre) {
		return nil, false
	}
	cls := getNodeAttr(n, "class")
	hasCodeWrapperSignal := hasCodeWrapperClass(cls) ||
		getNodeAttr(n, "data-lang") != "" ||
		getNodeAttr(n, "data-ext") != "" ||
		getNodeAttr(n, "data-language") != "" ||
		findFirstDescendantWithClass(n, "line-numbers-wrapper", "line-numbers", "highlight-lines", "line-numbers-rows", "lang", "copy") != nil
	if !hasCodeWrapperSignal {
		return nil, false
	}
	return pre, true
}

func hasCodeWrapperClass(cls string) bool {
	for _, tok := range strings.Fields(strings.ToLower(cls)) {
		if strings.HasPrefix(tok, "language-") || strings.HasPrefix(tok, "lang-") || strings.HasPrefix(tok, "ext-") ||
			strings.HasPrefix(tok, "code-") || strings.HasPrefix(tok, "vp-code") || strings.HasPrefix(tok, "prism") ||
			strings.HasPrefix(tok, "shiki") {
			return true
		}
		switch tok {
		case "line-numbers-mode", "line-numbers", "highlight", "highlighter-rouge", "codeblock", "code":
			return true
		}
	}
	return false
}

func hasProseBlockOutsideCode(container *nethtml.Node, allowedSubtree *nethtml.Node) bool {
	found := false
	var walk func(cur *nethtml.Node)
	walk = func(cur *nethtml.Node) {
		if found || cur == nil || cur == allowedSubtree {
			return
		}
		if cur != container && cur.Type == nethtml.ElementNode {
			switch strings.ToLower(cur.Data) {
			case "p", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "dl", "blockquote", "table", "section", "article", "details":
				found = true
				return
			}
		}
		for ch := cur.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(container)
	return found
}

func isAlreadyCanonicalChroma(pre *nethtml.Node) bool {
	if !hasAnyClassToken(getNodeAttr(pre, "class"), "chroma") {
		return false
	}
	code := findFirstDirectChildByTag(pre, "code")
	if code == nil {
		return false
	}
	return findFirstDescendantWithClass(code, "cl", "ln") != nil
}

func normalizeSingleCodeTarget(target *nethtml.Node, preNode *nethtml.Node) {
	if isAlreadyCanonicalChroma(preNode) && target == preNode {
		return
	}
	codeNode := findFirstDirectChildByTag(preNode, "code")
	codeRoot := preNode
	if codeNode != nil {
		codeRoot = codeNode
	}

	lang := detectCodeBlockLang(target, preNode, codeNode)
	if strings.EqualFold(lang, "mermaid") || hasAnyClassToken(getNodeAttr(preNode, "class"), "mermaid") {
		raw := strings.TrimSuffix(strings.ReplaceAll(extractNodeText(codeRoot), "\r\n", "\n"), "\n")
		replaceNodeWithFragment(target, `<pre class="mermaid">`+html.EscapeString(raw)+`</pre>`)
		return
	}

	hasLineNumbers, expectedLines := detectCodeLineNumbers(target, preNode, codeNode)
	hlSet := detectWrapperHighlightedLines(target)

	// Remove embedded toolbar/line-number overlay nodes inside pre/code before reading code tokens.
	pruneEmbeddedCodeNoise(preNode)

	if !hasSyntaxTokenElements(codeRoot) && len(hlSet) == 0 {
		raw := strings.ReplaceAll(extractNodeText(codeRoot), "\r\n", "\n")
		raw = strings.ReplaceAll(raw, "\r", "\n")
		raw = strings.TrimSuffix(raw, "\n")
		replaceNodeWithFragment(target, render.HighlightCodeHTML(raw, lang, hasLineNumbers))
		return
	}

	lines := extractCodeLinesFromNode(codeRoot, expectedLines, hlSet)
	canon := buildCanonicalCodeBlockHTML(lines, lang, hasLineNumbers)
	replaceNodeWithFragment(target, canon)
}

type codeLineFrag struct {
	html        string
	highlighted bool
}

func detectCodeLineNumbers(target, preNode, codeNode *nethtml.Node) (bool, int) {
	hasLN := false
	for _, nd := range []*nethtml.Node{target, preNode, codeNode} {
		if nd == nil {
			continue
		}
		if hasAnyClassToken(getNodeAttr(nd, "class"), "line-numbers-mode", "line-numbers", "linenos", "has-line-numbers", "show-line-numbers") {
			hasLN = true
		}
	}
	expected := 0
	if wrap := findLineNumberGutterNode(target); wrap != nil {
		hasLN = true
		for ch := wrap.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type == nethtml.ElementNode && (hasAnyClassToken(getNodeAttr(ch, "class"), "line-number") || strings.EqualFold(ch.Data, "span") || strings.EqualFold(ch.Data, "div")) {
				expected++
			}
		}
	}
	return hasLN, expected
}

func findLineNumberGutterNode(root *nethtml.Node) *nethtml.Node {
	var found *nethtml.Node
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if found != nil || n == nil {
			return
		}
		if n != root && n.Type == nethtml.ElementNode {
			tag := strings.ToLower(n.Data)
			cls := getNodeAttr(n, "class")
			if hasAnyClassToken(cls, "line-numbers-wrapper", "line-numbers-rows") ||
				(tag == "div" && hasAnyClassToken(cls, "line-numbers") && countElementsByTag(n, "pre") == 0) {
				found = n
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}

func detectWrapperHighlightedLines(target *nethtml.Node) map[int]bool {
	hlWrap := findFirstDescendantWithClass(target, "highlight-lines")
	if hlWrap == nil {
		return nil
	}
	out := make(map[int]bool)
	lineIdx := 0
	for ch := hlWrap.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != nethtml.ElementNode {
			continue
		}
		tag := strings.ToLower(ch.Data)
		if tag == "div" || tag == "span" {
			lineIdx++
			if hasAnyClassToken(getNodeAttr(ch, "class"), "highlighted", "hl", "active") {
				out[lineIdx] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func detectCodeBlockLang(nodes ...*nethtml.Node) string {
	for _, nd := range nodes {
		if nd == nil {
			continue
		}
		for _, attr := range []string{"data-lang", "data-language", "data-ext"} {
			if v := cleanLangIdentifier(getNodeAttr(nd, attr)); v != "" {
				return v
			}
		}
	}
	// Check VitePress / VuePress `<span class="lang">dart</span>`
	if len(nodes) > 0 && nodes[0] != nil {
		if langNode := findFirstDescendantWithClass(nodes[0], "lang"); langNode != nil && strings.EqualFold(langNode.Data, "span") {
			if v := cleanLangIdentifier(extractNodeText(langNode)); v != "" {
				return v
			}
		}
	}
	// Check `language-*`, `lang-*`, `ext-*` class tokens (innermost code -> pre -> wrapper)
	for i := len(nodes) - 1; i >= 0; i-- {
		nd := nodes[i]
		if nd == nil {
			continue
		}
		for _, tok := range strings.Fields(getNodeAttr(nd, "class")) {
			low := strings.ToLower(tok)
			for _, pfx := range []string{"language-", "lang-", "ext-"} {
				if strings.HasPrefix(low, pfx) {
					if v := cleanLangIdentifier(low[len(pfx):]); v != "" {
						return v
					}
				}
			}
		}
	}
	// Check Hexo `figure.highlight dart`
	for _, nd := range nodes {
		if nd == nil {
			continue
		}
		fields := strings.Fields(strings.ToLower(getNodeAttr(nd, "class")))
		hasHighlight := false
		for _, f := range fields {
			if f == "highlight" {
				hasHighlight = true
				break
			}
		}
		if hasHighlight {
			for _, f := range fields {
				if f != "highlight" && f != "plaintext" && !strings.Contains(f, "line") {
					if v := cleanLangIdentifier(f); v != "" {
						return v
					}
				}
			}
		}
	}
	return ""
}

func cleanLangIdentifier(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == "none" || s == "plain" || s == "plaintext" || s == "text" || s == "default" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '#' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			break
		}
	}
	return b.String()
}

func pruneEmbeddedCodeNoise(root *nethtml.Node) {
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == nethtml.ElementNode {
				tag := strings.ToLower(c.Data)
				cls := getNodeAttr(c, "class")
				if tag == "button" ||
					hasAnyClassToken(cls, "line-numbers-rows", "line-numbers-wrapper", "line-numbers", "highlight-lines", "buttons", "copy", "copy-code-button", "clip-button", "code-copy") {
					n.RemoveChild(c)
					c = next
					continue
				}
			}
			walk(c)
			c = next
		}
	}
	walk(root)
}

func hasSyntaxTokenElements(codeRoot *nethtml.Node) bool {
	found := false
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if found || n == nil {
			return
		}
		if n != codeRoot && n.Type == nethtml.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "span" && !hasAnyClassToken(getNodeAttr(n, "class"), "line", "ln", "lnt", "cl", "line-number", "line-numbers-rows") {
				found = true
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(codeRoot)
	return found
}

func extractCodeLinesFromNode(codeRoot *nethtml.Node, expectedLines int, hlSet map[int]bool) []codeLineFrag {
	// Case 1: Direct `.line` element children (Shiki, VitePress, Astro, Hexo `td.code pre > span.line`)
	if lineNodes, ok := directLineElementChildren(codeRoot); ok {
		out := make([]codeLineFrag, 0, len(lineNodes))
		for idx, lnNode := range lineNodes {
			cls := getNodeAttr(lnNode, "class")
			isHL := hlSet[idx+1] || hasAnyClassToken(cls, "highlighted", "hl", "diff", "add", "remove", "focused")
			innerParts := splitInlineCodeDOMIntoLines(lnNode)
			lineHTML := ""
			if len(innerParts) > 0 {
				// Strip single trailing empty segment if `.line` ended with a trailing `\n`
				if len(innerParts) > 1 && isVisuallyEmptyCodeHTML(innerParts[len(innerParts)-1]) {
					innerParts = innerParts[:len(innerParts)-1]
				}
				lineHTML = strings.Join(innerParts, "")
			}
			out = append(out, codeLineFrag{html: lineHTML, highlighted: isHL})
		}
		return out
	}

	// Case 2: Inline token stream separated by `\n` or `<br>` (PrismJS / VuePress / Highlight.js / Rouge / GitBook)
	rawLines := splitInlineCodeDOMIntoLines(codeRoot)
	if len(rawLines) > 1 && isVisuallyEmptyCodeHTML(rawLines[len(rawLines)-1]) {
		rawLines = rawLines[:len(rawLines)-1]
	}
	if expectedLines > 0 && len(rawLines) == expectedLines+1 && isVisuallyEmptyCodeHTML(rawLines[0]) {
		rawLines = rawLines[1:]
	}
	if len(rawLines) == 0 {
		rawLines = []string{""}
	}
	out := make([]codeLineFrag, len(rawLines))
	for i, s := range rawLines {
		out[i] = codeLineFrag{
			html:        s,
			highlighted: hlSet[i+1],
		}
	}
	return out
}

func directLineElementChildren(codeRoot *nethtml.Node) ([]*nethtml.Node, bool) {
	var lines []*nethtml.Node
	for c := codeRoot.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case nethtml.TextNode:
			if strings.TrimSpace(c.Data) != "" {
				return nil, false
			}
		case nethtml.ElementNode:
			tag := strings.ToLower(c.Data)
			if tag == "br" {
				continue
			}
			if (tag == "span" || tag == "div") && hasAnyClassToken(getNodeAttr(c, "class"), "line", "code-line") {
				lines = append(lines, c)
				continue
			}
			return nil, false
		}
	}
	return lines, len(lines) > 0
}

type openInlineElem struct {
	tag   string
	attrs []nethtml.Attribute
}

func splitInlineCodeDOMIntoLines(codeRoot *nethtml.Node) []string {
	var lines []string
	var buf strings.Builder
	var stack []openInlineElem

	writeOpen := func(el openInlineElem) {
		buf.WriteByte('<')
		buf.WriteString(el.tag)
		for _, a := range el.attrs {
			buf.WriteByte(' ')
			buf.WriteString(a.Key)
			buf.WriteString(`="`)
			buf.WriteString(html.EscapeString(a.Val))
			buf.WriteByte('"')
		}
		buf.WriteByte('>')
	}
	writeClose := func(el openInlineElem) {
		buf.WriteString("</")
		buf.WriteString(el.tag)
		buf.WriteByte('>')
	}
	flushLine := func() {
		for i := len(stack) - 1; i >= 0; i-- {
			writeClose(stack[i])
		}
		lines = append(lines, buf.String())
		buf.Reset()
		for i := 0; i < len(stack); i++ {
			writeOpen(stack[i])
		}
	}

	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case nethtml.TextNode:
				norm := strings.ReplaceAll(c.Data, "\r\n", "\n")
				norm = strings.ReplaceAll(norm, "\r", "\n")
				parts := strings.Split(norm, "\n")
				for idx, part := range parts {
					if idx > 0 {
						flushLine()
					}
					if part != "" {
						buf.WriteString(html.EscapeString(part))
					}
				}
			case nethtml.ElementNode:
				tag := strings.ToLower(c.Data)
				if tag == "br" {
					flushLine()
					continue
				}
				cls := getNodeAttr(c, "class")
				if hasAnyClassToken(cls, "ln", "lnt", "line-number", "line-numbers-rows") {
					continue
				}
				if isSafeCodeInlineTag(tag) {
					el := openInlineElem{
						tag:   tag,
						attrs: filterSafeCodeInlineAttrs(tag, c.Attr),
					}
					stack = append(stack, el)
					writeOpen(el)
					walk(c)
					stack = stack[:len(stack)-1]
					writeClose(el)
				} else {
					walk(c)
				}
			}
		}
	}
	walk(codeRoot)
	for i := len(stack) - 1; i >= 0; i-- {
		writeClose(stack[i])
	}
	lines = append(lines, buf.String())
	return lines
}

func isSafeCodeInlineTag(tag string) bool {
	switch tag {
	case "span", "strong", "b", "em", "i", "u", "s", "del", "ins", "mark", "kbd", "sub", "sup", "var", "samp", "a":
		return true
	}
	return false
}

func filterSafeCodeInlineAttrs(tag string, attrs []nethtml.Attribute) []nethtml.Attribute {
	var out []nethtml.Attribute
	for _, a := range attrs {
		k := strings.ToLower(strings.TrimSpace(a.Key))
		if k == "class" || k == "title" || (tag == "a" && k == "href") {
			if strings.TrimSpace(a.Val) != "" {
				out = append(out, nethtml.Attribute{Key: k, Val: a.Val})
			}
		}
	}
	return out
}

func isVisuallyEmptyCodeHTML(lineHTML string) bool {
	if lineHTML == "" {
		return true
	}
	// Strip inline tags and check if nothing (not even spaces) remains
	inTag := false
	for i := 0; i < len(lineHTML); i++ {
		if lineHTML[i] == '<' {
			inTag = true
			continue
		}
		if lineHTML[i] == '>' {
			inTag = false
			continue
		}
		if !inTag {
			return false
		}
	}
	return true
}

func extractPlainLinesFromFrags(lines []codeLineFrag) string {
	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		// Strip tags and unescape entities
		var raw strings.Builder
		inTag := false
		for j := 0; j < len(ln.html); j++ {
			if ln.html[j] == '<' {
				inTag = true
				continue
			}
			if ln.html[j] == '>' {
				inTag = false
				continue
			}
			if !inTag {
				raw.WriteByte(ln.html[j])
			}
		}
		b.WriteString(html.UnescapeString(raw.String()))
	}
	return b.String()
}

func buildCanonicalCodeBlockHTML(lines []codeLineFrag, lang string, withLineNumbers bool) string {
	hasAnyHL := false
	for _, l := range lines {
		if l.highlighted {
			hasAnyHL = true
			break
		}
	}

	var inner strings.Builder
	if withLineNumbers || hasAnyHL {
		for idx, ln := range lines {
			lineCls := "line"
			if ln.highlighted {
				lineCls = "line hl"
			}
			inner.WriteString(`<span class="` + lineCls + `">`)
			if withLineNumbers {
				fmt.Fprintf(&inner, `<span class="ln">%d</span>`, idx+1)
			}
			inner.WriteString(`<span class="cl">`)
			inner.WriteString(ln.html)
			if idx < len(lines)-1 {
				inner.WriteByte('\n')
			}
			inner.WriteString(`</span></span>`)
		}
	} else {
		for idx, ln := range lines {
			if idx > 0 {
				inner.WriteByte('\n')
			}
			inner.WriteString(ln.html)
		}
	}

	langAttr := ""
	if lang != "" {
		langAttr = ` data-lang="` + html.EscapeString(lang) + `"`
	}
	return `<figure class="volo-codeblock"` + langAttr + `><pre class="chroma"><code>` + inner.String() + `</code></pre></figure>`
}

func replaceNodeWithFragment(target *nethtml.Node, fragmentHTML string) {
	if target == nil || target.Parent == nil {
		return
	}
	parent := target.Parent
	ctxNode := parent
	if ctxNode.Type != nethtml.ElementNode {
		ctxNode = &nethtml.Node{Type: nethtml.ElementNode, Data: "div"}
	}
	nodes, err := nethtml.ParseFragment(strings.NewReader(fragmentHTML), ctxNode)
	if err != nil || len(nodes) == 0 {
		return
	}
	for _, fn := range nodes {
		parent.InsertBefore(fn, target)
	}
	parent.RemoveChild(target)
}

func countElementsByTag(root *nethtml.Node, wantTag string) int {
	cnt := 0
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n != root && n.Type == nethtml.ElementNode && strings.EqualFold(n.Data, wantTag) {
			cnt++
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return cnt
}

func findFirstElementByTag(root *nethtml.Node, wantTag string) *nethtml.Node {
	var found *nethtml.Node
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if found != nil || n == nil {
			return
		}
		if n != root && n.Type == nethtml.ElementNode && strings.EqualFold(n.Data, wantTag) {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}

func findFirstDirectChildByTag(parent *nethtml.Node, wantTag string) *nethtml.Node {
	if parent == nil {
		return nil
	}
	for c := parent.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == nethtml.ElementNode && strings.EqualFold(c.Data, wantTag) {
			return c
		}
	}
	return nil
}

func findFirstDescendantWithClass(root *nethtml.Node, classes ...string) *nethtml.Node {
	var found *nethtml.Node
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if found != nil || n == nil {
			return
		}
		if n != root && n.Type == nethtml.ElementNode && hasAnyClassToken(getNodeAttr(n, "class"), classes...) {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}
