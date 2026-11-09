package render

import (
	"net/url"
	"regexp"
	"strings"

	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	safeCustomAppSchemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]{1,31}:[^\s"'<>\x00-\x1f\x7f]+$`)
	safeEditorB64AttrRe   = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)
	safeDimensionAttrRe   = regexp.MustCompile(`^\d{1,5}%?$`)
	safeIntAttrRe         = regexp.MustCompile(`^-?\d{1,9}$`)
	safeSpanCountAttrRe   = regexp.MustCompile(`^\d{1,4}$`)
	safeTabIndexAttrRe    = regexp.MustCompile(`^-?\d{1,4}$`)
	safeRoleAttrRe        = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	safeSourceMimeRe      = regexp.MustCompile(`^[a-zA-Z0-9.+/-]{1,64}(?:;\s*codecs="[a-zA-Z0-9., _-]{1,64}")?$`)
)

// htmlSanitizer implements a deterministic single-pass HTML5 fragment sanitizer
// directly over golang.org/x/net/html without regex tag rewriting.
type htmlSanitizer struct {
	commentMode           bool
	allowCustomAppSchemes bool
	allowEditorDirectives bool
}

func newArticleSanitizer(allowEditorDirectives bool) *htmlSanitizer {
	return &htmlSanitizer{
		commentMode:           false,
		allowCustomAppSchemes: true,
		allowEditorDirectives: allowEditorDirectives,
	}
}

func newCommentSanitizer() *htmlSanitizer {
	return &htmlSanitizer{
		commentMode:           true,
		allowCustomAppSchemes: false,
		allowEditorDirectives: false,
	}
}

// Sanitize parses rawHTML using the HTML5 fragment parser in a <body> context and
// serializes only allowlisted elements and validated attributes in a single tree pass.
func (s *htmlSanitizer) Sanitize(rawHTML string) string {
	if rawHTML == "" {
		return ""
	}
	if !strings.ContainsAny(rawHTML, "<&\x00") {
		return rawHTML
	}
	if strings.Contains(rawHTML, "\x00") {
		rawHTML = strings.ReplaceAll(rawHTML, "\x00", "")
	}

	ctxNode := &nethtml.Node{
		Type:     nethtml.ElementNode,
		DataAtom: atom.Body,
		Data:     "body",
	}
	nodes, err := nethtml.ParseFragment(strings.NewReader(rawHTML), ctxNode)
	if err != nil || len(nodes) == 0 {
		return ""
	}

	var out strings.Builder
	out.Grow(len(rawHTML))
	for _, n := range nodes {
		s.writeSanitizedNode(&out, n)
	}
	return out.String()
}

func (s *htmlSanitizer) writeSanitizedNode(out *strings.Builder, n *nethtml.Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case nethtml.TextNode:
		writeEscapedHTMLText(out, n.Data)
		return
	case nethtml.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			s.writeSanitizedNode(out, c)
		}
		return
	case nethtml.ElementNode:
		// Foreign namespaces (SVG, MathML, etc.) are completely stripped with all descendants.
		if n.Namespace != "" {
			return
		}
		tag := strings.ToLower(n.Data)
		if isDroppedSubtreeElement(tag) {
			return
		}
		if s.commentMode {
			s.writeCommentElement(out, n, tag)
		} else {
			s.writeArticleElement(out, n, tag)
		}
	default:
		// CommentNode, DoctypeNode, RawNode are omitted.
		return
	}
}

func (s *htmlSanitizer) writeChildren(out *strings.Builder, n *nethtml.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		s.writeSanitizedNode(out, c)
	}
}

func (s *htmlSanitizer) writeCommentElement(out *strings.Builder, n *nethtml.Node, tag string) {
	if !isAllowedCommentElement(tag) {
		// Non-dangerous disallowed wrapper tags (e.g., <button>, <audio>, <font>) drop the tag itself.
		if !isVoidHTMLElement(tag) {
			s.writeChildren(out, n)
		}
		return
	}

	if tag == "a" {
		rawHref, hasHref := findAttrVal(n.Attr, "href")
		cleanHref, isExtHTTP, ok := "", false, false
		if hasHref {
			cleanHref, isExtHTTP, ok = sanitizeAnchorHref(rawHref, false)
		}
		if !ok {
			s.writeChildren(out, n)
			return
		}
		out.WriteString("<a href=\"")
		writeEscapedAttrVal(out, cleanHref)
		out.WriteByte('"')
		if title, hasTitle := findAttrVal(n.Attr, "title"); hasTitle && !containsControlChars(title) {
			out.WriteString(" title=\"")
			writeEscapedAttrVal(out, title)
			out.WriteByte('"')
		}
		if isExtHTTP {
			out.WriteString(` target="_blank" rel="nofollow noopener noreferrer"`)
		}
		out.WriteByte('>')
		s.writeChildren(out, n)
		out.WriteString("</a>")
		return
	}

	if tag == "img" {
		rawSrc, hasSrc := findAttrVal(n.Attr, "src")
		if !hasSrc {
			return
		}
		cleanSrc, ok := sanitizeImageSrc(rawSrc)
		if !ok {
			return
		}
		out.WriteString("<img src=\"")
		writeEscapedAttrVal(out, cleanSrc)
		out.WriteByte('"')
		for _, a := range n.Attr {
			if a.Namespace != "" {
				continue
			}
			k := strings.ToLower(a.Key)
			switch k {
			case "alt", "title":
				if !containsControlChars(a.Val) {
					writeAttrKV(out, k, a.Val)
				}
			case "width", "height":
				v := strings.TrimSpace(a.Val)
				if safeDimensionAttrRe.MatchString(v) {
					writeAttrKV(out, k, v)
				}
			}
		}
		out.WriteByte('>')
		return
	}

	out.WriteByte('<')
	out.WriteString(tag)
	for _, a := range n.Attr {
		if a.Namespace != "" {
			continue
		}
		k := strings.ToLower(a.Key)
		v := strings.TrimSpace(a.Val)
		switch k {
		case "title":
			if tag == "abbr" && !containsControlChars(a.Val) {
				writeAttrKV(out, k, a.Val)
			}
		case "start":
			if tag == "ol" && commentListStartRe.MatchString(v) {
				writeAttrKV(out, k, v)
			}
		case "align":
			if (tag == "th" || tag == "td") && commentTableAlignRe.MatchString(v) {
				writeAttrKV(out, k, v)
			}
		case "class":
			switch tag {
			case "pre", "code", "span", "table", "tbody", "tr", "td":
				if chromaCommentClsRe.MatchString(v) {
					writeAttrKV(out, k, v)
				}
			case "div":
				if commentTableScrollClsRe.MatchString(v) {
					writeAttrKV(out, k, v)
				}
			}
		}
	}
	out.WriteByte('>')
	if !isVoidHTMLElement(tag) {
		s.writeChildren(out, n)
		out.WriteString("</")
		out.WriteString(tag)
		out.WriteByte('>')
	}
}

func (s *htmlSanitizer) writeArticleElement(out *strings.Builder, n *nethtml.Node, tag string) {
	if !isAllowedArticleElement(tag) {
		if !isVoidHTMLElement(tag) {
			s.writeChildren(out, n)
		}
		return
	}

	switch tag {
	case "iframe":
		rawSrc, hasSrc := findAttrVal(n.Attr, "src")
		if !hasSrc {
			return
		}
		cleanSrc := stripControlAndTrimSpace(rawSrc)
		if !safeEmbedIframeRe.MatchString(cleanSrc) {
			return
		}
		out.WriteString("<iframe")
		writeAttrKV(out, "src", cleanSrc)
		for _, a := range n.Attr {
			if a.Namespace != "" {
				continue
			}
			k := strings.ToLower(a.Key)
			if k == "src" || k == "sandbox" {
				continue
			}
			v := strings.TrimSpace(a.Val)
			switch k {
			case "loading":
				if strings.EqualFold(v, "lazy") || strings.EqualFold(v, "eager") {
					writeAttrKV(out, k, strings.ToLower(v))
				}
			case "allowfullscreen":
				writeBooleanOrEmptyAttr(out, k, v)
			case "referrerpolicy":
				if safeIframeRefPolRe.MatchString(v) {
					writeAttrKV(out, k, v)
				}
			case "width", "height":
				if safeDimensionAttrRe.MatchString(v) {
					writeAttrKV(out, k, v)
				}
			default:
				s.writeAllowedGlobalArticleAttr(out, tag, k, a.Val)
			}
		}
		writeAttrKV(out, "sandbox", "allow-scripts allow-same-origin allow-presentation")
		out.WriteString("></iframe>")
		return

	case "input":
		rawType, hasType := findAttrVal(n.Attr, "type")
		if !hasType || !inputCheckboxTypeRe.MatchString(strings.ToLower(strings.TrimSpace(rawType))) {
			return
		}
		out.WriteString("<input")
		for _, a := range n.Attr {
			if a.Namespace != "" {
				continue
			}
			k := strings.ToLower(a.Key)
			switch k {
			case "type":
				writeAttrKV(out, "type", "checkbox")
			case "checked", "disabled":
				writeBooleanOrEmptyAttr(out, k, a.Val)
			default:
				s.writeAllowedGlobalArticleAttr(out, tag, k, a.Val)
			}
		}
		out.WriteByte('>')
		return

	case "source":
		rawSrc, hasSrc := findAttrVal(n.Attr, "src")
		if !hasSrc {
			return
		}
		cleanSrc := stripControlAndTrimSpace(rawSrc)
		if !safeMediaSrcAttrRe.MatchString(cleanSrc) {
			return
		}
		out.WriteString("<source")
		writeAttrKV(out, "src", cleanSrc)
		for _, a := range n.Attr {
			if a.Namespace != "" {
				continue
			}
			k := strings.ToLower(a.Key)
			if k == "src" {
				continue
			}
			v := strings.TrimSpace(a.Val)
			switch k {
			case "type":
				if safeSourceMimeRe.MatchString(v) {
					writeAttrKV(out, k, v)
				}
			case "controls":
				writeBooleanOrEmptyAttr(out, k, a.Val)
			case "preload":
				if isSafePreloadAttr(v) {
					writeAttrKV(out, k, strings.ToLower(v))
				}
			default:
				s.writeAllowedGlobalArticleAttr(out, tag, k, a.Val)
			}
		}
		out.WriteByte('>')
		return

	case "img":
		rawSrc, hasSrc := findAttrVal(n.Attr, "src")
		if !hasSrc {
			return
		}
		cleanSrc, ok := sanitizeImageSrc(rawSrc)
		if !ok {
			return
		}
		out.WriteString("<img")
		for _, a := range n.Attr {
			if a.Namespace != "" {
				continue
			}
			k := strings.ToLower(a.Key)
			v := strings.TrimSpace(a.Val)
			switch k {
			case "src":
				writeAttrKV(out, "src", cleanSrc)
			case "alt":
				if !containsControlChars(a.Val) {
					writeAttrKV(out, "alt", a.Val)
				}
			case "width", "height":
				if safeDimensionAttrRe.MatchString(v) {
					writeAttrKV(out, k, v)
				}
			case "loading":
				if strings.EqualFold(v, "lazy") || strings.EqualFold(v, "eager") {
					writeAttrKV(out, k, strings.ToLower(v))
				}
			case "decoding":
				if strings.EqualFold(v, "async") || strings.EqualFold(v, "sync") || strings.EqualFold(v, "auto") {
					writeAttrKV(out, k, strings.ToLower(v))
				}
			default:
				s.writeAllowedGlobalArticleAttr(out, tag, k, a.Val)
			}
		}
		out.WriteByte('>')
		return

	case "a":
		rawHref, hasHref := findAttrVal(n.Attr, "href")
		cleanHref, isExtHTTP, hrefOK := "", false, false
		if hasHref {
			cleanHref, isExtHTTP, hrefOK = sanitizeAnchorHref(rawHref, s.allowCustomAppSchemes)
		}
		rawID, hasID := findAttrVal(n.Attr, "id")
		idOK := hasID && isSafeIDAttr(rawID)
		if !hrefOK && !idOK {
			s.writeChildren(out, n)
			return
		}

		out.WriteString("<a")
		if idOK {
			writeAttrKV(out, "id", strings.TrimSpace(rawID))
		}
		if rawClass, hasClass := findAttrVal(n.Attr, "class"); hasClass && !containsControlChars(rawClass) {
			if trimmedClass := strings.TrimSpace(rawClass); trimmedClass != "" {
				writeAttrKV(out, "class", trimmedClass)
			}
		}
		if hrefOK {
			writeAttrKV(out, "href", cleanHref)
		}
		wroteRel := false
		for _, a := range n.Attr {
			if a.Namespace != "" {
				continue
			}
			k := strings.ToLower(a.Key)
			if k == "id" || k == "class" || k == "href" {
				continue
			}
			v := strings.TrimSpace(a.Val)
			switch k {
			case "target":
				if strings.EqualFold(v, "_blank") || strings.EqualFold(v, "_self") {
					writeAttrKV(out, "target", strings.ToLower(v))
				}
			case "rel":
				if isExtHTTP {
					writeAttrKV(out, "rel", "nofollow noopener noreferrer")
					wroteRel = true
				} else if cleanRel := sanitizeRelTokens(v); cleanRel != "" {
					writeAttrKV(out, "rel", cleanRel)
					wroteRel = true
				}
			case "data-no-pjax":
				if !containsControlChars(a.Val) {
					writeAttrKV(out, "data-no-pjax", a.Val)
				}
			default:
				s.writeAllowedGlobalArticleAttr(out, tag, k, a.Val)
			}
		}
		if isExtHTTP && !wroteRel {
			writeAttrKV(out, "rel", "nofollow noopener noreferrer")
		}
		out.WriteByte('>')
		s.writeChildren(out, n)
		out.WriteString("</a>")
		return
	}

	out.WriteByte('<')
	out.WriteString(tag)
	for _, a := range n.Attr {
		if a.Namespace != "" {
			continue
		}
		k := strings.ToLower(a.Key)
		v := strings.TrimSpace(a.Val)

		if s.writeElementSpecificArticleAttr(out, tag, k, v, a.Val) {
			continue
		}
		s.writeAllowedGlobalArticleAttr(out, tag, k, a.Val)
	}
	out.WriteByte('>')
	if !isVoidHTMLElement(tag) {
		s.writeChildren(out, n)
		out.WriteString("</")
		out.WriteString(tag)
		out.WriteByte('>')
	}
}

func (s *htmlSanitizer) writeElementSpecificArticleAttr(out *strings.Builder, tag, k, trimmedVal, rawVal string) bool {
	switch tag {
	case "video":
		switch k {
		case "src", "poster":
			clean := stripControlAndTrimSpace(rawVal)
			if safeMediaSrcAttrRe.MatchString(clean) {
				writeAttrKV(out, k, clean)
			}
			return true
		case "controls", "playsinline", "loop", "muted":
			writeBooleanOrEmptyAttr(out, k, rawVal)
			return true
		case "preload":
			if isSafePreloadAttr(trimmedVal) {
				writeAttrKV(out, k, strings.ToLower(trimmedVal))
			}
			return true
		case "width", "height":
			if safeDimensionAttrRe.MatchString(trimmedVal) {
				writeAttrKV(out, k, trimmedVal)
			}
			return true
		}
	case "audio":
		switch k {
		case "src":
			clean := stripControlAndTrimSpace(rawVal)
			if safeMediaSrcAttrRe.MatchString(clean) {
				writeAttrKV(out, k, clean)
			}
			return true
		case "controls", "loop", "muted":
			writeBooleanOrEmptyAttr(out, k, rawVal)
			return true
		case "preload":
			if isSafePreloadAttr(trimmedVal) {
				writeAttrKV(out, k, strings.ToLower(trimmedVal))
			}
			return true
		}
	case "button":
		switch k {
		case "type":
			if strings.EqualFold(trimmedVal, "button") {
				writeAttrKV(out, "type", "button")
			}
			return true
		case "disabled":
			writeBooleanOrEmptyAttr(out, "disabled", rawVal)
			return true
		}
	case "details":
		if k == "open" {
			writeBooleanOrEmptyAttr(out, "open", rawVal)
			return true
		}
	case "font":
		switch k {
		case "color":
			if safeFontColorRe.MatchString(trimmedVal) {
				writeAttrKV(out, "color", trimmedVal)
			}
			return true
		case "size":
			if safeFontSizeRe.MatchString(trimmedVal) {
				writeAttrKV(out, "size", trimmedVal)
			}
			return true
		}
	case "ol":
		switch k {
		case "start":
			if safeIntAttrRe.MatchString(trimmedVal) {
				writeAttrKV(out, "start", trimmedVal)
			}
			return true
		case "reversed":
			writeBooleanOrEmptyAttr(out, "reversed", rawVal)
			return true
		case "type":
			if len(trimmedVal) == 1 && strings.ContainsRune("1aAiI", rune(trimmedVal[0])) {
				writeAttrKV(out, "type", trimmedVal)
			}
			return true
		}
	case "li":
		if k == "value" {
			if safeIntAttrRe.MatchString(trimmedVal) {
				writeAttrKV(out, "value", trimmedVal)
			}
			return true
		}
	case "th", "td":
		switch k {
		case "align":
			if commentTableAlignRe.MatchString(trimmedVal) {
				writeAttrKV(out, "align", strings.ToLower(trimmedVal))
			}
			return true
		case "colspan", "rowspan":
			if safeSpanCountAttrRe.MatchString(trimmedVal) {
				writeAttrKV(out, k, trimmedVal)
			}
			return true
		case "scope":
			if tag == "th" {
				low := strings.ToLower(trimmedVal)
				if low == "row" || low == "col" || low == "rowgroup" || low == "colgroup" {
					writeAttrKV(out, "scope", low)
				}
			}
			return true
		}
	case "col", "colgroup":
		if k == "span" {
			if safeSpanCountAttrRe.MatchString(trimmedVal) {
				writeAttrKV(out, "span", trimmedVal)
			}
			return true
		}
	case "blockquote", "q", "del", "ins":
		if k == "cite" {
			if cleanCite, _, ok := sanitizeAnchorHref(rawVal, false); ok {
				writeAttrKV(out, "cite", cleanCite)
			}
			return true
		}
		if (tag == "del" || tag == "ins") && k == "datetime" {
			if len(trimmedVal) > 0 && len(trimmedVal) <= 64 && !containsControlChars(trimmedVal) {
				writeAttrKV(out, "datetime", trimmedVal)
			}
			return true
		}
	case "time":
		if k == "datetime" {
			if len(trimmedVal) > 0 && len(trimmedVal) <= 64 && !containsControlChars(trimmedVal) {
				writeAttrKV(out, "datetime", trimmedVal)
			}
			return true
		}
	}

	if k == "style" {
		if isStyleAllowedElement(tag) && safeInlineStyleRe.MatchString(trimmedVal) {
			writeAttrKV(out, "style", trimmedVal)
		}
		return true
	}

	return false
}

func (s *htmlSanitizer) writeAllowedGlobalArticleAttr(out *strings.Builder, tag, k, rawVal string) {
	if containsControlChars(rawVal) {
		return
	}
	trimmed := strings.TrimSpace(rawVal)
	switch k {
	case "id":
		if isSafeIDAttr(trimmed) {
			writeAttrKV(out, "id", trimmed)
		}
	case "class":
		if trimmed != "" {
			writeAttrKV(out, "class", trimmed)
		}
	case "title":
		writeAttrKV(out, "title", rawVal)
	case "tabindex":
		if safeTabIndexAttrRe.MatchString(trimmed) {
			writeAttrKV(out, "tabindex", trimmed)
		}
	case "role":
		if safeRoleAttrRe.MatchString(trimmed) {
			writeAttrKV(out, "role", trimmed)
		}
	case "lang":
		if len(trimmed) > 0 && len(trimmed) <= 32 && !strings.ContainsAny(trimmed, " \t\"'<>") {
			writeAttrKV(out, "lang", trimmed)
		}
	case "dir":
		low := strings.ToLower(trimmed)
		if low == "ltr" || low == "rtl" || low == "auto" {
			writeAttrKV(out, "dir", low)
		}
	case "aria-label", "aria-labelledby", "aria-describedby", "aria-controls":
		if trimmed != "" {
			writeAttrKV(out, k, rawVal)
		}
	case "aria-hidden", "aria-expanded":
		low := strings.ToLower(trimmed)
		if low == "true" || low == "false" {
			writeAttrKV(out, k, low)
		}
	case "data-lang", "data-lock-type", "data-article-id", "data-book-slug",
		"data-github-profile", "data-callout", "data-md-toc", "data-md-media-label",
		"data-math-src", "data-mermaid-src", "data-footnote-ref", "data-footnote-backref":
		writeAttrKV(out, k, rawVal)
	case "data-audio-seek":
		if commentListStartRe.MatchString(trimmed) {
			writeAttrKV(out, k, trimmed)
		}
	case "data-math-display":
		if trimmed == "0" || trimmed == "1" {
			writeAttrKV(out, k, trimmed)
		}
	case "data-md-media-href":
		clean := stripControlAndTrimSpace(rawVal)
		if safeMediaSrcAttrRe.MatchString(clean) {
			writeAttrKV(out, k, clean)
		}
	case "data-volo-md-b64":
		if s.allowEditorDirectives && isEditorDirectiveContainer(tag) && safeEditorB64AttrRe.MatchString(trimmed) {
			writeAttrKV(out, "data-volo-md-b64", trimmed)
		}
	case "contenteditable":
		if s.allowEditorDirectives && isEditorDirectiveContainer(tag) && strings.EqualFold(trimmed, "false") {
			writeAttrKV(out, "contenteditable", "false")
		}
	}
}

func isEditorDirectiveContainer(tag string) bool {
	switch tag {
	case "div", "section", "nav", "aside":
		return true
	default:
		return false
	}
}

func isStyleAllowedElement(tag string) bool {
	switch tag {
	case "span", "font", "p", "div", "mark", "strong", "em", "b", "i", "u", "s",
		"td", "th", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "center":
		return true
	default:
		return false
	}
}

func isSafePreloadAttr(v string) bool {
	low := strings.ToLower(v)
	return low == "" || low == "none" || low == "metadata" || low == "auto"
}

func isSafeIDAttr(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b <= 0x20 || b == 0x7f || b == '"' || b == '\'' || b == '<' || b == '>' {
			return false
		}
	}
	return true
}

func sanitizeRelTokens(raw string) string {
	if raw == "" {
		return ""
	}
	var kept []string
	for _, tok := range strings.Fields(strings.ToLower(raw)) {
		switch tok {
		case "nofollow", "noopener", "noreferrer", "ugc", "sponsored",
			"tag", "bookmark", "author", "license", "me", "prev", "next", "footnote":
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, " ")
}

func isBlockedScheme(scheme string) bool {
	switch scheme {
	case "javascript", "vbscript", "data", "blob", "file", "about",
		"chrome", "chrome-extension", "moz-extension", "edge", "ms-browser-extension",
		"jar", "view-source", "filesystem", "ws", "wss",
		"livescript", "jscript", "ecmascript", "safari-extension",
		"ms-its", "mhtml", "wyciwyg", "res", "ieframe":
		return true
	default:
		return false
	}
}

func sanitizeAnchorHref(rawVal string, allowCustomApp bool) (clean string, isExternalHTTP bool, ok bool) {
	cleaned := stripControlAndTrimSpace(rawVal)
	if cleaned == "" {
		return "", false, false
	}
	if strings.HasPrefix(cleaned, "//") || strings.HasPrefix(cleaned, `/\`) || strings.HasPrefix(cleaned, `\\`) {
		return "", false, false
	}

	// Same-origin path, fragment, or query URLs.
	if cleaned[0] == '/' || cleaned[0] == '#' || cleaned[0] == '?' {
		if strings.ContainsAny(cleaned, " \t\"'<>") {
			return "", false, false
		}
		return cleaned, false, true
	}

	// Relative paths without any scheme colon.
	colonIdx := strings.IndexByte(cleaned, ':')
	if colonIdx < 0 {
		if strings.ContainsAny(cleaned, " \t\"'<>") {
			return "", false, false
		}
		return cleaned, false, true
	}
	if colonIdx == 0 {
		return "", false, false
	}

	scheme := strings.ToLower(cleaned[:colonIdx])
	if isBlockedScheme(scheme) {
		return "", false, false
	}

	switch scheme {
	case "http", "https":
		rest := cleaned[colonIdx+1:]
		if !strings.HasPrefix(rest, "//") || strings.ContainsAny(cleaned, " \t\"'<>") {
			return "", false, false
		}
		normalized := scheme + ":" + rest
		u, err := url.Parse(normalized)
		if err != nil || u.Host == "" {
			return "", false, false
		}
		return normalized, true, true

	case "mailto":
		rest := cleaned[colonIdx+1:]
		if rest == "" || strings.HasPrefix(rest, "//") || strings.ContainsAny(cleaned, " \t\"'<>") {
			return "", false, false
		}
		return "mailto:" + rest, false, true

	default:
		if !allowCustomApp {
			return "", false, false
		}
		normalized := scheme + cleaned[colonIdx:]
		if !safeCustomAppSchemeRe.MatchString(normalized) {
			return "", false, false
		}
		return normalized, false, true
	}
}

func sanitizeImageSrc(rawVal string) (string, bool) {
	cleaned := stripControlAndTrimSpace(rawVal)
	if cleaned == "" {
		return "", false
	}
	if strings.HasPrefix(cleaned, "//") || strings.HasPrefix(cleaned, `/\`) || strings.HasPrefix(cleaned, `\\`) {
		return "", false
	}
	if strings.ContainsAny(cleaned, " \t\"'<>") {
		return "", false
	}
	if cleaned[0] == '/' {
		return cleaned, true
	}
	colonIdx := strings.IndexByte(cleaned, ':')
	if colonIdx < 0 {
		return cleaned, true
	}
	scheme := strings.ToLower(cleaned[:colonIdx])
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	rest := cleaned[colonIdx+1:]
	if !strings.HasPrefix(rest, "//") {
		return "", false
	}
	normalized := scheme + ":" + rest
	u, err := url.Parse(normalized)
	if err != nil || u.Host == "" {
		return "", false
	}
	return normalized, true
}

func stripControlAndTrimSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x1f || c == 0x7f {
			continue
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}

func containsControlChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t' && c != '\n' && c != '\r') || c == 0x7f {
			return true
		}
	}
	return false
}

func findAttrVal(attrs []nethtml.Attribute, key string) (string, bool) {
	for _, a := range attrs {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}

func writeAttrKV(out *strings.Builder, key, val string) {
	out.WriteByte(' ')
	out.WriteString(key)
	out.WriteString(`="`)
	writeEscapedAttrVal(out, val)
	out.WriteByte('"')
}

func writeBooleanOrEmptyAttr(out *strings.Builder, key, val string) {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" || strings.EqualFold(trimmed, key) {
		out.WriteByte(' ')
		out.WriteString(key)
		out.WriteString(`=""`)
	}
}

func writeEscapedHTMLText(out *strings.Builder, s string) {
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
		case '\x00':
			esc = ""
		default:
			continue
		}
		if i > last {
			out.WriteString(s[last:i])
		}
		out.WriteString(esc)
		last = i + 1
	}
	if last < len(s) {
		out.WriteString(s[last:])
	}
}

func writeEscapedAttrVal(out *strings.Builder, s string) {
	last := 0
	for i := 0; i < len(s); i++ {
		var esc string
		switch s[i] {
		case '&':
			esc = "&amp;"
		case '"':
			esc = "&#34;"
		case '\'':
			esc = "&#39;"
		case '<':
			esc = "&lt;"
		case '>':
			esc = "&gt;"
		case '\x00':
			esc = ""
		default:
			continue
		}
		if i > last {
			out.WriteString(s[last:i])
		}
		out.WriteString(esc)
		last = i + 1
	}
	if last < len(s) {
		out.WriteString(s[last:])
	}
}

func isVoidHTMLElement(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func isDroppedSubtreeElement(tag string) bool {
	switch tag {
	case "script", "style", "object", "embed", "applet", "base", "meta", "link",
		"form", "textarea", "title", "noscript", "template", "svg", "math",
		"xmp", "noembed", "noframes", "plaintext", "frameset", "frame", "head",
		"select", "option", "optgroup", "datalist", "canvas", "dialog", "portal",
		"slot", "param", "track", "area", "map", "marquee", "bgsound", "keygen":
		return true
	default:
		return false
	}
}

func isAllowedArticleElement(tag string) bool {
	switch tag {
	case "a", "abbr", "acronym", "address", "article", "aside", "audio",
		"b", "bdi", "bdo", "blockquote", "br", "button",
		"caption", "center", "cite", "code", "col", "colgroup",
		"data", "dd", "del", "details", "dfn", "div", "dl", "dt",
		"em", "figcaption", "figure", "font", "footer",
		"h1", "h2", "h3", "h4", "h5", "h6", "header", "hgroup", "hr",
		"i", "iframe", "img", "input", "ins",
		"kbd", "li", "main", "mark", "nav",
		"ol", "p", "pre", "q", "rp", "rt", "rtc", "ruby",
		"s", "samp", "section", "small", "source", "span", "strike", "strong", "sub", "summary", "sup",
		"table", "tbody", "td", "tfoot", "th", "thead", "time", "tr", "tt", "u", "ul", "var", "video", "wbr":
		return true
	default:
		return false
	}
}

func isAllowedCommentElement(tag string) bool {
	switch tag {
	case "p", "br", "hr", "blockquote", "div",
		"ul", "ol", "li", "dl", "dt", "dd",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"strong", "b", "em", "i", "del", "s", "strike", "ins", "sub", "sup", "kbd", "q", "cite", "abbr",
		"pre", "code", "span",
		"table", "thead", "tbody", "tfoot", "tr", "th", "td",
		"a", "img":
		return true
	default:
		return false
	}
}
