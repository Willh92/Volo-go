package render

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

type chromaRule struct {
	re  *regexp.Regexp
	cls string // Chroma CSS class ("k", "kt", "kc", "s", "s1", "s2", "sb", "sd", "c1", "cm", "cp", "m", "mi", "mf", "mh", "mo", "nf", "na", "nt", "nv", "nd", "ni", "o", "p", "gh", "gi", "gd", "gu", "gs")
}

type identPrefixMode uint8

const (
	prefixNone identPrefixMode = iota
	prefixPython
	prefixRust
	prefixNginx
)

type codeLexer struct {
	customScan       func(w *codeHTMLWriter, src string)
	skipWS           bool
	hasFastIdent     bool
	identAllowDollar bool
	dollarVarCls     string
	identFoldCase    bool
	rustMacroBang    bool
	hasSlashComments bool
	hasDashComment   bool
	detectFuncCall   bool
	detectTypeIdent  bool
	stmtDirective    string
	prefixMode       identPrefixMode
	defaultIdentCls  string
	identMap         map[string]string
	byteRules        [256][]chromaRule
	nonASCIIRules    []chromaRule
	opBytes          [256]bool
	punctCls         [256]string
}

var (
	codeLexerOnce sync.Once
	codeLexers    map[string]*codeLexer
	isASCIISpace  = [256]bool{
		' ':  true,
		'\t': true,
		'\n': true,
		'\r': true,
		'\v': true,
		'\f': true,
	}
)

// codeHTMLWriter streams highlighted Chroma spans and optional per-line wrappers directly
// into a single strings.Builder without intermediate full-buffer copies.
type codeHTMLWriter struct {
	out       *strings.Builder
	withLines bool
	lineNo    int
	numBuf    [20]byte
}

func (w *codeHTMLWriter) startNumberedLines() {
	w.lineNo = 1
	w.out.WriteString(`<span class="line"><span class="ln">1</span><span class="cl">`)
}

func (w *codeHTMLWriter) finishNumberedLines() {
	w.out.WriteString(`</span></span>`)
}

func (w *codeHTMLWriter) emit(cls string, seg string) {
	if len(seg) == 0 {
		return
	}
	if !w.withLines {
		writeSpanOrPlain(w.out, cls, seg)
		return
	}
	nl := strings.IndexByte(seg, '\n')
	if nl < 0 {
		writeSpanOrPlain(w.out, cls, seg)
		return
	}
	rem := seg
	for {
		part := rem[:nl]
		if len(part) > 0 {
			writeSpanOrPlain(w.out, cls, part)
		}
		w.out.WriteString("\n</span></span><span class=\"line\"><span class=\"ln\">")
		w.lineNo++
		w.out.Write(strconv.AppendInt(w.numBuf[:0], int64(w.lineNo), 10))
		w.out.WriteString(`</span><span class="cl">`)
		rem = rem[nl+1:]
		nl = strings.IndexByte(rem, '\n')
		if nl < 0 {
			if len(rem) > 0 {
				writeSpanOrPlain(w.out, cls, rem)
			}
			return
		}
	}
}

func writeSpanOrPlain(out *strings.Builder, cls string, seg string) {
	if cls == "" {
		writeEscapedCode(out, seg)
		return
	}
	out.WriteString(`<span class="`)
	out.WriteString(cls)
	out.WriteString(`">`)
	writeEscapedCode(out, seg)
	out.WriteString(`</span>`)
}

func writeEscapedCode(out *strings.Builder, s string) {
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
		case '"':
			esc = "&#34;"
		case '\'':
			esc = "&#39;"
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

// HighlightCodeHTML renders source code into Volo's canonical Chroma-compatible HTML block.
func HighlightCodeHTML(code string, lang string, withLineNumbers bool) string {
	return highlightCodeChroma(code, lang, withLineNumbers)
}

func isPlainCodeLang(key string) bool {
	switch key {
	case "text", "plaintext", "txt", "plain", "none", "raw", "ascii", "output", "log", "tex", "latex", "math", "mermaid":
		return true
	default:
		return false
	}
}

func normalizeCodeLangKey(lang string) string {
	key := strings.ToLower(strings.TrimSpace(lang))
	if fields := strings.Fields(key); len(fields) > 0 {
		key = fields[0]
	}
	key = strings.TrimPrefix(key, "language-")
	key = strings.TrimPrefix(key, "lang-")
	return strings.Trim(key, ",;:")
}

func resolveCodeLexer(lang string) *codeLexer {
	codeLexerOnce.Do(initCodeLexers)
	key := normalizeCodeLangKey(lang)
	if key == "" || isPlainCodeLang(key) {
		return nil
	}
	return codeLexers[key]
}

func resolveCodeLexerForCode(lang string, code string) (*codeLexer, string) {
	codeLexerOnce.Do(initCodeLexers)
	key := normalizeCodeLangKey(lang)
	if isPlainCodeLang(key) {
		return nil, ""
	}
	if key != "" {
		if lx := codeLexers[key]; lx != nil {
			return lx, ""
		}
	}
	lx, detectedLang := detectCodeLexerWithLang(code)
	if key != "" {
		return lx, ""
	}
	return lx, detectedLang
}

func highlightCodeChroma(code string, lang string, withLineNumbers bool) string {
	lx, detectedLang := resolveCodeLexerForCode(lang, code)

	var out strings.Builder
	grow := len(code)*2 + 96
	if withLineNumbers {
		grow += 64 * (1 + strings.Count(code, "\n"))
	}
	out.Grow(grow)

	out.WriteString(`<figure class="volo-codeblock"`)
	if lang != "" {
		out.WriteString(` data-lang="`)
		writeEscapedCode(&out, lang)
		out.WriteByte('"')
	} else if detectedLang != "" {
		out.WriteString(` data-detected-lang="`)
		writeEscapedCode(&out, detectedLang)
		out.WriteByte('"')
	}
	out.WriteString(`><pre class="chroma"><code>`)

	if withLineNumbers {
		trimmed := strings.TrimSuffix(code, "\n")
		w := codeHTMLWriter{out: &out, withLines: true}
		w.startNumberedLines()
		tokenizeWithLexer(&w, trimmed, lx)
		w.finishNumberedLines()
	} else {
		w := codeHTMLWriter{out: &out}
		tokenizeWithLexer(&w, code, lx)
	}

	out.WriteString(`</code></pre></figure>`)
	return out.String()
}

func highlightCommentCodeChroma(code string, lang string) string {
	lx, _ := resolveCodeLexerForCode(lang, code)

	var out strings.Builder
	out.Grow(len(code)*2 + 48)
	out.WriteString(`<pre class="chroma"><code>`)
	w := codeHTMLWriter{out: &out}
	tokenizeWithLexer(&w, code, lx)
	out.WriteString(`</code></pre>`)
	return out.String()
}

func (lx *codeLexer) isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || (lx.identAllowDollar && b == '$')
}

func (lx *codeLexer) isIdentCont(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || (lx.identAllowDollar && b == '$')
}

func isPyStringPrefixByte(b byte) bool {
	switch b {
	case 'f', 'F', 'r', 'R', 'b', 'B', 'u', 'U':
		return true
	default:
		return false
	}
}

func (lx *codeLexer) hasPrefixLiteralAt(src string, i int) bool {
	switch lx.prefixMode {
	case prefixPython:
		if !isPyStringPrefixByte(src[i]) || i+1 >= len(src) {
			return false
		}
		n1 := src[i+1]
		if n1 == '"' || n1 == '\'' {
			return true
		}
		if isPyStringPrefixByte(n1) && i+2 < len(src) {
			n2 := src[i+2]
			return n2 == '"' || n2 == '\''
		}
		return false

	case prefixRust:
		b0 := src[i]
		if b0 == 'b' && i+1 < len(src) {
			n1 := src[i+1]
			if n1 == '"' || n1 == '\'' {
				return true
			}
			if n1 == 'r' && i+2 < len(src) && (src[i+2] == '"' || src[i+2] == '#') {
				return true
			}
		} else if b0 == 'r' && i+1 < len(src) {
			return src[i+1] == '"' || src[i+1] == '#'
		}
		return false

	case prefixNginx:
		rem := src[i:]
		for _, pfx := range []string{"http://", "https://", "ws://", "wss://", "grpc://", "grpcs://", "fastcgi://", "uwsgi://", "scgi://", "unix:"} {
			if len(rem) > len(pfx) && strings.EqualFold(rem[:len(pfx)], pfx) {
				return true
			}
		}
		return false

	default:
		return false
	}
}

func tokenizeWithLexer(w *codeHTMLWriter, src string, lx *codeLexer) {
	if len(src) == 0 {
		return
	}
	if lx == nil {
		w.emit("", src)
		return
	}
	if lx.customScan != nil {
		lx.customScan(w, src)
		return
	}

	var foldBuf [64]byte
	atStmtStart := true
	n := len(src)
	for i := 0; i < n; {
		b := src[i]

		if lx.skipWS && isASCIISpace[b] {
			j := i + 1
			hasNL := b == '\n'
			for j < n && isASCIISpace[src[j]] {
				if src[j] == '\n' {
					hasNL = true
				}
				j++
			}
			w.emit("", src[i:j])
			if hasNL {
				atStmtStart = true
			}
			i = j
			continue
		}

		if b >= 0x80 {
			matched := false
			for _, r := range lx.nonASCIIRules {
				loc := r.re.FindStringIndex(src[i:])
				if loc != nil && loc[0] == 0 && loc[1] > 0 {
					w.emit(r.cls, src[i:i+loc[1]])
					i += loc[1]
					matched = true
					break
				}
			}
			if matched {
				atStmtStart = false
				continue
			}
			j := i
			for j < n && src[j] >= 0x80 {
				r, width := utf8.DecodeRuneInString(src[j:])
				if r == utf8.RuneError && width <= 1 {
					if j == i {
						j++
					}
					break
				}
				j += width
			}
			w.emit("", src[i:j])
			atStmtStart = false
			i = j
			continue
		}

		if lx.hasFastIdent && lx.isIdentStart(b) && !lx.hasPrefixLiteralAt(src, i) {
			j := i + 1
			for j < n && lx.isIdentCont(src[j]) {
				j++
			}
			if lx.rustMacroBang && j < n && src[j] == '!' && (j+1 == n || src[j+1] != '=') {
				w.emit("nd", src[i:j+1])
				atStmtStart = false
				i = j + 1
				continue
			}
			ident := src[i:j]
			cls := lx.defaultIdentCls
			matchedMap := false
			if lx.dollarVarCls != "" && ident[0] == '$' && len(ident) > 1 {
				cls = lx.dollarVarCls
				matchedMap = true
			} else if lx.identMap != nil {
				lookupKey := ident
				if lx.identFoldCase {
					if len(ident) <= len(foldBuf) {
						for k := 0; k < len(ident); k++ {
							c := ident[k]
							if c >= 'A' && c <= 'Z' {
								c += 'a' - 'A'
							}
							foldBuf[k] = c
						}
						lookupKey = string(foldBuf[:len(ident)])
					} else {
						lookupKey = strings.ToLower(ident)
					}
				}
				if mapped, ok := lx.identMap[lookupKey]; ok {
					cls = mapped
					matchedMap = true
				}
			}
			if !matchedMap {
				isCall := false
				if lx.detectFuncCall {
					k := j
					for k < n && (src[k] == ' ' || src[k] == '\t') {
						k++
					}
					if k < n && src[k] == '(' {
						cls = "nf"
						isCall = true
					}
				}
				if !isCall && atStmtStart && lx.stmtDirective != "" {
					cls = lx.stmtDirective
				} else if !isCall && lx.detectTypeIdent {
					if tc := classifyUpperOrPascalIdent(ident); tc != "" {
						cls = tc
					}
				}
			}
			w.emit(cls, ident)
			atStmtStart = false
			i = j
			continue
		}

		if rules := lx.byteRules[b]; len(rules) > 0 {
			rem := src[i:]
			matched := false
			for _, r := range rules {
				loc := r.re.FindStringIndex(rem)
				if loc != nil && loc[0] == 0 && loc[1] > 0 {
					w.emit(r.cls, rem[:loc[1]])
					if r.cls == "" || r.cls[0] != 'c' {
						atStmtStart = false
					}
					i += loc[1]
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}

		if lx.opBytes[b] {
			j := i + 1
			hasStmtDelim := b == ';' || b == '{' || b == '}'
			for j < n && lx.opBytes[src[j]] {
				if lx.hasSlashComments && src[j] == '/' && j+1 < n && (src[j+1] == '/' || src[j+1] == '*') {
					break
				}
				if lx.hasDashComment && src[j] == '-' && j+1 < n && src[j+1] == '-' {
					break
				}
				if src[j] == ';' || src[j] == '{' || src[j] == '}' {
					hasStmtDelim = true
				}
				j++
			}
			w.emit("o", src[i:j])
			atStmtStart = hasStmtDelim
			i = j
			continue
		}

		if pCls := lx.punctCls[b]; pCls != "" {
			w.emit(pCls, src[i:i+1])
			atStmtStart = b == ';' || b == '{' || b == '}'
			i++
			continue
		}

		w.emit("", src[i:i+1])
		if b == '\n' || b == ';' || b == '{' || b == '}' {
			atStmtStart = true
		} else if !isASCIISpace[b] {
			atStmtStart = false
		}
		i++
	}
}

func classifyUpperOrPascalIdent(ident string) string {
	n := len(ident)
	if n == 0 {
		return ""
	}
	first := ident[0]
	if n >= 2 && first >= 'A' && first <= 'Z' {
		allCaps := true
		hasLower := false
		for i := 0; i < n; i++ {
			c := ident[i]
			if c >= 'a' && c <= 'z' {
				allCaps = false
				hasLower = true
			} else if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				allCaps = false
			}
		}
		if allCaps {
			return "kc"
		}
		if hasLower {
			return "nc"
		}
	}
	return ""
}

func isHTMLTagNameStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isHTMLTagNameCont(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
		b == '-' || b == ':' || b == '.' || b == '_'
}

func isHTMLAttrNameStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		b == '_' || b == ':' || b == '@' || b == '#' || b == '?'
}

func isHTMLAttrNameCont(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
		b == '_' || b == '-' || b == ':' || b == '.' || b == '@'
}

func findCaseInsensitiveCloseTag(src string, rawTagLower string) int {
	needle := "</" + rawTagLower
	for off := 0; off+len(needle) < len(src); {
		idx := strings.IndexByte(src[off:], '<')
		if idx < 0 {
			return -1
		}
		pos := off + idx
		if pos+len(needle) < len(src) && src[pos+1] == '/' && strings.EqualFold(src[pos:pos+len(needle)], needle) {
			next := src[pos+len(needle)]
			if next == '>' || isASCIISpace[next] || next == '/' {
				return pos
			}
		}
		off = pos + 1
	}
	return -1
}

// tokenizeHTML implements a tag-state-aware scanner for HTML, XML, SVG, Vue, and Svelte
// so prose outside `<...>` tags is emitted as plain escaped text without mis-coloring words
// as attribute names (`na`) or apostrophes (`don't`) as strings (`s`).
func tokenizeHTML(w *codeHTMLWriter, src string) {
	n := len(src)
	i := 0
	for i < n {
		if src[i] != '<' && src[i] != '&' {
			j := i + 1
			for j < n && src[j] != '<' && src[j] != '&' {
				j++
			}
			w.emit("", src[i:j])
			i = j
			continue
		}

		if src[i] == '&' {
			k := i + 1
			if k < n && src[k] == '#' {
				k++
			}
			entStart := k
			for k < n && ((src[k] >= 'a' && src[k] <= 'z') || (src[k] >= 'A' && src[k] <= 'Z') || (src[k] >= '0' && src[k] <= '9')) {
				k++
			}
			if k > entStart && k < n && src[k] == ';' {
				w.emit("ni", src[i:k+1])
				i = k + 1
				continue
			}
			w.emit("", "&")
			i++
			continue
		}

		// src[i] == '<'
		if strings.HasPrefix(src[i:], "<!--") {
			if end := strings.Index(src[i+4:], "-->"); end >= 0 {
				stop := i + 4 + end + 3
				w.emit("cm", src[i:stop])
				i = stop
			} else {
				w.emit("cm", src[i:])
				i = n
			}
			continue
		}

		if strings.HasPrefix(src[i:], "<![CDATA[") {
			if end := strings.Index(src[i+9:], "]]>"); end >= 0 {
				stop := i + 9 + end + 3
				w.emit("cp", src[i:stop])
				i = stop
			} else {
				w.emit("cp", src[i:])
				i = n
			}
			continue
		}

		if i+1 < n && (src[i+1] == '!' || src[i+1] == '?') {
			if gt := strings.IndexByte(src[i+2:], '>'); gt >= 0 {
				stop := i + 2 + gt + 1
				w.emit("cp", src[i:stop])
				i = stop
			} else {
				w.emit("cp", src[i:])
				i = n
			}
			continue
		}

		isClose := false
		nameStart := i + 1
		if nameStart < n && src[nameStart] == '/' {
			isClose = true
			nameStart++
		}
		if nameStart >= n || !isHTMLTagNameStart(src[nameStart]) {
			w.emit("", "<")
			i++
			continue
		}

		nameEnd := nameStart + 1
		for nameEnd < n && isHTMLTagNameCont(src[nameEnd]) {
			nameEnd++
		}
		tagNameLower := strings.ToLower(src[nameStart:nameEnd])

		// Fast path for `</tag>` or `<tag>` with zero attributes/whitespace before `>`.
		if nameEnd < n && src[nameEnd] == '>' {
			w.emit("nt", src[i:nameEnd+1])
			i = nameEnd + 1
			if !isClose && (tagNameLower == "script" || tagNameLower == "style") {
				if closePos := findCaseInsensitiveCloseTag(src[i:], tagNameLower); closePos >= 0 {
					subLang := "ts"
					if tagNameLower == "style" {
						subLang = "css"
					}
					tokenizeWithLexer(w, src[i:i+closePos], codeLexers[subLang])
					i += closePos
				}
			}
			continue
		}

		w.emit("nt", src[i:nameEnd])
		i = nameEnd
		selfClosing := false

		// Inside `<tag ...>` attribute state until closing `>` or `/>`.
		for i < n {
			b := src[i]
			if isASCIISpace[b] {
				j := i + 1
				for j < n && isASCIISpace[src[j]] {
					j++
				}
				w.emit("", src[i:j])
				i = j
				continue
			}
			if b == '/' && i+1 < n && src[i+1] == '>' {
				w.emit("nt", "/>")
				i += 2
				selfClosing = true
				break
			}
			if b == '>' {
				w.emit("nt", ">")
				i++
				break
			}
			if b == '"' || b == '\'' {
				quote := b
				j := i + 1
				for j < n {
					if src[j] == '\\' && j+1 < n {
						j += 2
						continue
					}
					if src[j] == quote {
						j++
						break
					}
					j++
				}
				w.emit("s", src[i:j])
				i = j
				continue
			}
			if isHTMLAttrNameStart(b) {
				j := i + 1
				for j < n && isHTMLAttrNameCont(src[j]) {
					j++
				}
				w.emit("na", src[i:j])
				i = j
				continue
			}
			if b >= 0x80 {
				_, width := utf8.DecodeRuneInString(src[i:])
				if width <= 0 {
					width = 1
				}
				w.emit("", src[i:i+width])
				i += width
				continue
			}
			w.emit("", src[i:i+1])
			i++
		}

		if !isClose && !selfClosing && (tagNameLower == "script" || tagNameLower == "style") {
			if closePos := findCaseInsensitiveCloseTag(src[i:], tagNameLower); closePos >= 0 {
				subLang := "ts"
				if tagNameLower == "style" {
					subLang = "css"
				}
				tokenizeWithLexer(w, src[i:i+closePos], codeLexers[subLang])
				i += closePos
			}
		}
	}
}

type rawByteRule struct {
	leads   string // empty means register across all ASCII + non-ASCII fallback
	pattern string
	cls     string
}

func addLexerRules(lx *codeLexer, rules []rawByteRule) {
	for _, rr := range rules {
		cr := chromaRule{
			re:  regexp.MustCompile("^(?:" + rr.pattern + ")"),
			cls: rr.cls,
		}
		if rr.leads == "" {
			for b := 0; b < 128; b++ {
				lx.byteRules[b] = append(lx.byteRules[b], cr)
			}
			lx.nonASCIIRules = append(lx.nonASCIIRules, cr)
			continue
		}
		seen := [256]bool{}
		for i := 0; i < len(rr.leads); i++ {
			b := rr.leads[i]
			if !seen[b] {
				seen[b] = true
				lx.byteRules[b] = append(lx.byteRules[b], cr)
			}
		}
	}
}

func setWordMap(m map[string]string, pipeList string, cls string, foldLower bool) {
	if pipeList == "" {
		return
	}
	for _, w := range strings.Split(pipeList, "|") {
		if w == "" {
			continue
		}
		if foldLower {
			w = strings.ToLower(w)
		}
		if _, exists := m[w]; !exists {
			m[w] = cls
		}
	}
}

func setOpBytes(lx *codeLexer, chars string) {
	for i := 0; i < len(chars); i++ {
		lx.opBytes[chars[i]] = true
	}
}

func setPunctBytes(lx *codeLexer, chars string, cls string) {
	for i := 0; i < len(chars); i++ {
		lx.punctCls[chars[i]] = cls
	}
}

const asciiDigits = "0123456789"
const asciiAlpha = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func initCodeLexers() {
	buildIdentMap := func(keywords, types, consts string, fold bool) map[string]string {
		m := make(map[string]string, 64)
		setWordMap(m, keywords, "k", fold)
		setWordMap(m, types, "kt", fold)
		setWordMap(m, consts, "kc", fold)
		return m
	}

	// 1. Go
	goLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		hasSlashComments: true,
		detectFuncCall:   true,
		detectTypeIdent:  true,
		identMap: buildIdentMap(
			"break|case|chan|const|continue|default|defer|else|fallthrough|for|func|go|goto|if|import|interface|map|package|range|return|select|struct|switch|type|var",
			"any|bool|byte|comparable|complex64|complex128|error|float32|float64|int|int8|int16|int32|int64|rune|string|uint|uint8|uint16|uint32|uint64|uintptr",
			"true|false|nil|iota",
			false,
		),
	}
	addLexerRules(goLex, []rawByteRule{
		{leads: "/", pattern: `//[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: "`", pattern: "`[^`]*`", cls: "sb"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\\n])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\.|[^'\\\n])*'`, cls: "s1"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F](?:_?[0-9a-fA-F])*`, cls: "mh"},
		{leads: "0", pattern: `0[bB][01](?:_?[01])*`, cls: "mi"},
		{leads: "0", pattern: `0[oO][0-7](?:_?[0-7])*`, cls: "mo"},
		{leads: asciiDigits, pattern: `\d(?:_?\d)*(?:\.\d(?:_?\d)*(?:[eE][+-]?\d(?:_?\d)*)?|[eE][+-]?\d(?:_?\d)*)`, cls: "mf"},
		{leads: asciiDigits, pattern: `\d(?:_?\d)*`, cls: "mi"},
	})
	setOpBytes(goLex, "+-*/%&|^!~<>?:=")
	setPunctBytes(goLex, "{}()[];,.", "p")
	setWordMap(goLex.identMap, "append|cap|clear|close|complex|copy|delete|imag|len|make|max|min|new|panic|print|println|real|recover", "nb", false)

	// 2. TypeScript / JavaScript / JSX / TSX
	tsLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		identAllowDollar: true,
		hasSlashComments: true,
		detectFuncCall:   true,
		detectTypeIdent:  true,
		identMap: buildIdentMap(
			"async|await|break|case|catch|class|const|continue|debugger|default|delete|do|else|enum|export|extends|finally|for|from|function|if|implements|import|in|instanceof|interface|let|new|of|package|private|protected|public|readonly|return|static|super|switch|this|throw|try|type|typeof|var|void|while|with|yield",
			"any|bigint|boolean|never|number|object|string|symbol|unknown|Promise|Record|Partial|Array|Map|Set",
			"true|false|null|undefined|NaN|Infinity",
			false,
		),
	}
	setWordMap(tsLex.identMap, "console|window|document|globalThis|JSON|Math|Object|Reflect|Proxy|Intl|Date|RegExp|Error|URL|URLSearchParams|fetch|setTimeout|clearTimeout|setInterval|clearInterval|queueMicrotask|structuredClone|parseInt|parseFloat|isNaN|isFinite", "nb", false)
	addLexerRules(tsLex, []rawByteRule{
		{leads: "/", pattern: `//[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: "`", pattern: "`(?:\\\\[\\s\\S]|[^`\\\\])*`", cls: "sb"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\\n])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\.|[^'\\\n])*'`, cls: "s1"},
		{leads: "@", pattern: `@[A-Za-z_$][A-Za-z0-9_$]*`, cls: "nd"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F](?:_?[0-9a-fA-F])*n?`, cls: "mh"},
		{leads: "0", pattern: `0[bB][01](?:_?[01])*n?`, cls: "mi"},
		{leads: "0", pattern: `0[oO][0-7](?:_?[0-7])*n?`, cls: "mo"},
		{leads: asciiDigits, pattern: `\d(?:_?\d)*(?:\.\d(?:_?\d)*(?:[eE][+-]?\d(?:_?\d)*)?|[eE][+-]?\d(?:_?\d)*)`, cls: "mf"},
		{leads: asciiDigits, pattern: `\d(?:_?\d)*n?`, cls: "mi"},
	})
	setOpBytes(tsLex, "+-*/%&|^!~<>?:=")
	setPunctBytes(tsLex, "{}()[];,.", "p")

	// 3. Rust (distinguishing single-char literals `'x'`/`'\n'` from lifetime parameters `'a`, `'static`)
	rsLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		rustMacroBang:    true,
		hasSlashComments: true,
		detectFuncCall:   true,
		detectTypeIdent:  true,
		prefixMode:       prefixRust,
		identMap: buildIdentMap(
			"as|async|await|break|const|continue|crate|dyn|else|enum|extern|fn|for|if|impl|in|let|loop|match|mod|move|mut|pub|ref|return|self|Self|static|struct|super|trait|type|unsafe|use|where|while",
			"bool|char|f32|f64|i8|i16|i32|i64|i128|isize|str|String|u8|u16|u32|u64|u128|usize|Vec|Option|Result|Box",
			"true|false|None|Some|Ok|Err",
			false,
		),
	}
	addLexerRules(rsLex, []rawByteRule{
		{leads: "/", pattern: `//[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: "#", pattern: `#!?\[[^\]]*\]`, cls: "nd"},
		{leads: `"br`, pattern: `b?r##"[\s\S]*?"##|b?r#"[\s\S]*?"#|b?r"[^"]*"|b?"(?:\\[\s\S]|[^"\\])*"`, cls: "s2"},
		{leads: "'b", pattern: `b?'(?:\\(?:x[0-9a-fA-F]{2}|u\{[0-9a-fA-F_]{1,6}\}|.)|[^'\\\n])'`, cls: "s1"},
		{leads: "'", pattern: `'[A-Za-z_][A-Za-z0-9_]*\b`, cls: "kt"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F_]+(?:[iu](?:8|16|32|64|128|size))?`, cls: "mh"},
		{leads: "0", pattern: `0[bB][01_]+(?:[iu](?:8|16|32|64|128|size))?`, cls: "mi"},
		{leads: "0", pattern: `0[oO][0-7_]+(?:[iu](?:8|16|32|64|128|size))?`, cls: "mo"},
		{leads: asciiDigits, pattern: `\d[\d_]*(?:\.\d[\d_]*(?:[eE][+-]?\d[\d_]*)?|[eE][+-]?\d[\d_]*)(?:f32|f64)?`, cls: "mf"},
		{leads: asciiDigits, pattern: `\d[\d_]*(?:[iu](?:8|16|32|64|128|size)|f32|f64)?`, cls: "mi"},
	})
	setOpBytes(rsLex, "+-*/%&|^!~<>?:=")
	setPunctBytes(rsLex, "{}()[];,.", "p")

	// 4. C / C++ / Java / C# / Kotlin / Swift (supporting C++14 digit separators `1'000'000` / `0xFF'FF` and no JS backticks)
	cFamLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		hasSlashComments: true,
		detectFuncCall:   true,
		detectTypeIdent:  true,
		identMap: buildIdentMap(
			"abstract|assert|async|await|break|case|catch|class|concept|const|consteval|constexpr|constinit|continue|co_await|co_return|co_yield|decltype|default|delete|do|else|enum|explicit|export|extends|final|finally|for|friend|goto|guard|if|implements|import|in|inline|interface|internal|let|mutable|namespace|new|noexcept|open|operator|override|package|private|protected|public|requires|return|sealed|sizeof|static|static_assert|struct|super|switch|synchronized|template|this|throw|throws|try|typedef|typename|using|val|var|virtual|volatile|when|where|while",
			"auto|bool|boolean|byte|char|char8_t|char16_t|char32_t|decimal|double|float|int|int8_t|int16_t|int32_t|int64_t|long|ptrdiff_t|short|signed|size_t|string|String|uint8_t|uint16_t|uint32_t|uint64_t|uintptr_t|unsigned|void|wchar_t",
			"true|false|null|nullptr|nil",
			false,
		),
	}
	addLexerRules(cFamLex, []rawByteRule{
		{leads: "/", pattern: `//[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: "#", pattern: `#[ \t]*(?:include|define|ifdef|ifndef|endif|elif|else|pragma|undef|error|warning|if|line|region|endregion)\b[^\n]*`, cls: "cp"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\\n])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\.|[^'\\\n])+'`, cls: "s1"},
		{leads: "@", pattern: `@"(?:""|[^"])*"`, cls: "s2"},
		{leads: "@", pattern: `@[A-Za-z_][A-Za-z0-9_]*`, cls: "nd"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F](?:['_]?[0-9a-fA-F])*(?:\.[0-9a-fA-F](?:['_]?[0-9a-fA-F])*[pP][+-]?\d(?:['_]?\d)*)?[uUlLfFdD]*`, cls: "mh"},
		{leads: "0", pattern: `0[bB][01](?:['_]?[01])*[uUlL]*`, cls: "mi"},
		{leads: asciiDigits, pattern: `\d(?:['_]?\d)*(?:\.\d(?:['_]?\d)*(?:[eE][+-]?\d(?:['_]?\d)*)?|[eE][+-]?\d(?:['_]?\d)*)[fFdDmM]?`, cls: "mf"},
		{leads: asciiDigits, pattern: `\d(?:['_]?\d)*[uUlL]*`, cls: "mi"},
	})
	setOpBytes(cFamLex, "+-*/%&|^!~<>?:=")
	setPunctBytes(cFamLex, "{}()[];,.", "p")

	// 5. Python (supporting multi-line triple-quoted prefixed f/r/b/u strings)
	pyLex := &codeLexer{
		skipWS:          true,
		hasFastIdent:    true,
		detectFuncCall:  true,
		detectTypeIdent: true,
		prefixMode:      prefixPython,
		identMap: buildIdentMap(
			"and|as|assert|async|await|break|case|class|continue|def|del|elif|else|except|finally|for|from|global|if|import|in|is|lambda|match|nonlocal|not|or|pass|raise|return|try|type|while|with|yield",
			"int|float|str|bytes|bool|list|dict|set|tuple|Any|Optional|Callable|Iterable|Sequence",
			"True|False|None|self|cls",
			false,
		),
	}
	addLexerRules(pyLex, []rawByteRule{
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: `"'`, pattern: `"""[\s\S]*?"""|'''[\s\S]*?'''`, cls: "sd"},
		{leads: `"'fFrRbBuU`, pattern: `(?i)[frbu]{0,2}(?:"""[\s\S]*?"""|'''[\s\S]*?''')`, cls: "s"},
		{leads: `"'fFrRbBuU`, pattern: `(?i)[frbu]{0,2}"(?:\\.|[^"\\\n])*"|(?i)[frbu]{0,2}'(?:\\.|[^'\\\n])*'`, cls: "s"},
		{leads: "@", pattern: `@[A-Za-z_][A-Za-z0-9_.]*`, cls: "nd"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F_]+`, cls: "mh"},
		{leads: "0", pattern: `0[bB][01_]+`, cls: "mi"},
		{leads: "0", pattern: `0[oO][0-7_]+`, cls: "mo"},
		{leads: asciiDigits, pattern: `\d[\d_]*(?:\.\d[\d_]*(?:[eE][+-]?\d[\d_]*)?|[eE][+-]?\d[\d_]*)[jJ]?`, cls: "mf"},
		{leads: asciiDigits, pattern: `\d[\d_]*[jJ]?`, cls: "mi"},
	})
	setOpBytes(pyLex, "+-*/%&|^~<>!=:@")
	setPunctBytes(pyLex, "{}()[];,.", "p")
	setWordMap(pyLex.identMap, "abs|all|any|ascii|bin|chr|dir|divmod|enumerate|eval|exec|filter|format|getattr|globals|hasattr|hash|hex|id|input|isinstance|issubclass|iter|len|locals|map|max|min|next|oct|open|ord|pow|print|property|range|repr|reversed|round|setattr|slice|sorted|staticmethod|classmethod|sum|super|vars|zip", "nb", false)

	// 6. Shell / Bash / Dockerfile
	shLex := &codeLexer{
		customScan: tokenizeShell,
	}

	// 7. SQL
	sqlLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		identAllowDollar: true,
		identFoldCase:    true,
		hasSlashComments: true,
		hasDashComment:   true,
		detectFuncCall:   true,
		identMap: buildIdentMap(
			"select|from|where|insert|into|values|update|set|delete|create|alter|drop|table|index|unique|primary|key|foreign|references|join|left|right|inner|outer|on|as|and|or|not|null|is|in|exists|between|like|order|by|group|having|limit|offset|union|all|distinct|case|when|then|else|end|begin|commit|rollback|transaction|pragma|with|returning|conflict|do|nothing|default|cascade|if",
			"integer|int|bigint|smallint|text|varchar|char|boolean|bool|blob|bytea|real|double|numeric|decimal|timestamp|timestamptz|date|time|uuid|json|jsonb",
			"true|false",
			true,
		),
	}
	addLexerRules(sqlLex, []rawByteRule{
		{leads: "-", pattern: `--[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: "'", pattern: `'(?:''|[^'])*'`, cls: "s1"},
		{leads: "\"`", pattern: `"(?:""|[^"])*"|` + "`[^`]*`", cls: ""},
		{leads: asciiDigits, pattern: `\d+(?:\.\d+)?`, cls: "m"},
	})
	setOpBytes(sqlLex, "=<>!+-*/%|")
	setPunctBytes(sqlLex, "(),;.", "p")

	// 8. Structured Config Data: YAML / TOML / INI / ENV + dedicated JSON / JSONC
	jsonLex := &codeLexer{
		customScan: tokenizeJSON,
	}
	dataLex := &codeLexer{
		customScan: tokenizeConfigData,
	}

	// 9. HTML / XML / SVG / Vue / Svelte (tag-state-aware scanner)
	htmlLex := &codeLexer{
		customScan: tokenizeHTML,
	}

	// 10. CSS / SCSS / LESS (property-vs-selector-vs-value aware scanner)
	cssLex := &codeLexer{
		customScan: tokenizeCSS,
	}

	// 11. Nginx / Caddy / Apache / Reverse-Proxy Config
	nginxLex := &codeLexer{
		skipWS:        true,
		hasFastIdent:  true,
		prefixMode:    prefixNginx,
		stmtDirective: "na",
		identMap: buildIdentMap(
			"http|server|location|upstream|events|stream|mail|map|geo|types|limit_except|if|else|return|rewrite|break|set|include|handle|route|match|reverse_proxy|file_server|respond|redir|tls|VirtualHost|Directory|Location|IfModule",
			"",
			"on|off|yes|no|true|false|default_server|ssl|http2|http3|permanent|redirect|last|any|all|none|localhost",
			false,
		),
	}
	addLexerRules(nginxLex, []rawByteRule{
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: "hHwWgGfFuUsS", pattern: `(?i)(?:https?|wss?|grpcs?|fastcgi|uwsgi|scgi)://[^\s;'"<>]+|(?i)unix:[^\s;'"<>]+`, cls: "s2"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\\n])*"`, cls: "s2"},
		{leads: "'", pattern: `'[^'\n]*'`, cls: "s1"},
		{leads: "$", pattern: `\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`, cls: "nv"},
		{leads: "@", pattern: `@[A-Za-z_][A-Za-z0-9_-]*`, cls: "nd"},
		{leads: asciiDigits, pattern: `\d+\.\d+\.\d+\.\d+(?::\d+)?(?:/\d+)?`, cls: "m"},
		{leads: asciiDigits, pattern: `\d+(?:\.\d+)?(?:ms|[kKmMgGsSmMhHdD])?\b`, cls: "m"},
	})
	setOpBytes(nginxLex, "=<>!~*&|^")
	setPunctBytes(nginxLex, "{}()[];,:.", "p")

	// 12. Diff / Patch
	diffLex := &codeLexer{}
	addLexerRules(diffLex, []rawByteRule{
		{leads: "+", pattern: `\+[^\n]*`, cls: "gi"},
		{leads: "-", pattern: `-[^\n]*`, cls: "gd"},
		{leads: "@", pattern: `@@[^\n]*`, cls: "gu"},
		{leads: "#", pattern: `#{1,6}\s[^\n]*`, cls: "gh"},
		{leads: "", pattern: `[^\n]+|\n`, cls: ""},
	})

	// 13. Markdown
	mdLex := &codeLexer{}
	addLexerRules(mdLex, []rawByteRule{
		{leads: "#", pattern: `#{1,6}\s[^\n]*`, cls: "gh"},
		{leads: "`", pattern: "```[^\\n]*|`[^`\\n]+`", cls: "sb"},
		{leads: "*_", pattern: `\*\*[^*\n]+\*\*|__[^_\n]+__`, cls: "gs"},
		{leads: "![", pattern: `!?\[[^\]\n]*\]\([^)\n]*\)`, cls: "nt"},
		{leads: ">", pattern: `>\s?`, cls: "k"},
		{leads: "-*+" + asciiDigits, pattern: `(?:[-*+]|\d+\.)\s`, cls: "k"},
		{leads: "", pattern: "[^\\n`*_\\[\\]#>+-]+|.|\\n", cls: ""},
	})

	// 14. PHP (split from cFamLex so `$class` / `$var` never splits `$` + keyword, with `#`, `//`, and `/* */` comments)
	phpLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		identAllowDollar: true,
		dollarVarCls:     "nv",
		hasSlashComments: true,
		detectFuncCall:   true,
		detectTypeIdent:  true,
		identMap: buildIdentMap(
			"abstract|and|array|as|break|callable|case|catch|class|clone|const|continue|declare|default|die|do|echo|else|elseif|empty|enddeclare|endfor|endforeach|endif|endswitch|endwhile|eval|exit|extends|final|finally|fn|for|foreach|function|global|goto|if|implements|include|include_once|instanceof|insteadof|interface|isset|list|match|namespace|new|or|print|private|protected|public|readonly|require|require_once|return|static|switch|throw|trait|try|unset|use|var|while|xor|yield",
			"bool|float|int|iterable|mixed|never|numeric|object|parent|resource|self|string|void",
			"true|false|null|TRUE|FALSE|NULL|__CLASS__|__DIR__|__FILE__|__FUNCTION__|__LINE__|__METHOD__|__NAMESPACE__|__TRAIT__",
			false,
		),
	}
	addLexerRules(phpLex, []rawByteRule{
		{leads: "<", pattern: `<\?(?:php|=)?`, cls: "cp"},
		{leads: "?", pattern: `\?>`, cls: "cp"},
		{leads: "/", pattern: `//[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: "#", pattern: `#\[[^\]]*\]`, cls: "nd"},
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: `"`, pattern: `"(?:\\[\s\S]|[^"\\])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\[\s\S]|[^'\\])*'`, cls: "s1"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F_]+`, cls: "mh"},
		{leads: "0", pattern: `0[bB][01_]+`, cls: "mi"},
		{leads: "0", pattern: `0[oO][0-7_]+`, cls: "mo"},
		{leads: asciiDigits, pattern: `\d[\d_]*(?:\.\d[\d_]*(?:[eE][+-]?\d[\d_]*)?|[eE][+-]?\d[\d_]*)`, cls: "mf"},
		{leads: asciiDigits, pattern: `\d[\d_]*`, cls: "mi"},
	})
	setOpBytes(phpLex, "+-*/%&|^!~<>?:=.")
	setPunctBytes(phpLex, "{}()[];,", "p")

	// 15. Ruby (`#` comments, `:symbol`, `@ivar`/`@@cvar`/`$gvar`, strings, keywords)
	rubyLex := &codeLexer{
		skipWS:          true,
		hasFastIdent:    true,
		detectFuncCall:  true,
		detectTypeIdent: true,
		identMap: buildIdentMap(
			"alias|and|attr_accessor|attr_reader|attr_writer|begin|break|case|class|def|defined|do|else|elsif|end|ensure|extend|for|if|in|include|module|next|not|or|prepend|private|protected|public|raise|redo|require|require_relative|rescue|retry|return|super|then|undef|unless|until|when|while|yield",
			"",
			"nil|true|false|self|__FILE__|__LINE__|__ENCODING__",
			false,
		),
	}
	setWordMap(rubyLex.identMap, "puts|print|p|pp|lambda|proc|loop|open|sleep|rand|freeze|frozen", "nb", false)
	addLexerRules(rubyLex, []rawByteRule{
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: ":", pattern: `::`, cls: "o"},
		{leads: ":", pattern: `:[A-Za-z_][A-Za-z0-9_]*[?!]?`, cls: "ss"},
		{leads: "@", pattern: `@@?[A-Za-z_][A-Za-z0-9_]*`, cls: "nv"},
		{leads: "$", pattern: `\$[A-Za-z0-9_!@&+?~*$-]+`, cls: "nv"},
		{leads: `"`, pattern: `"(?:\\[\s\S]|[^"\\])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\[\s\S]|[^'\\])*'`, cls: "s1"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F_]+`, cls: "mh"},
		{leads: "0", pattern: `0[bB][01_]+`, cls: "mi"},
		{leads: "0", pattern: `0[oO][0-7_]+`, cls: "mo"},
		{leads: asciiDigits, pattern: `\d[\d_]*(?:\.\d[\d_]*(?:[eE][+-]?\d[\d_]*)?|[eE][+-]?\d[\d_]*)`, cls: "mf"},
		{leads: asciiDigits, pattern: `\d[\d_]*`, cls: "mi"},
	})
	setOpBytes(rubyLex, "+-*/%&|^!~<>?=")
	setPunctBytes(rubyLex, "{}()[];,.:", "p")

	// 16. Lua (`--[[...]]` & `--` comments, strings, keywords)
	luaLex := &codeLexer{
		skipWS:         true,
		hasFastIdent:   true,
		hasDashComment: true,
		detectFuncCall: true,
		identMap: buildIdentMap(
			"and|break|do|else|elseif|end|for|function|goto|if|in|local|not|or|repeat|return|then|until|while",
			"",
			"nil|true|false",
			false,
		),
	}
	setWordMap(luaLex.identMap, "assert|collectgarbage|dofile|error|getmetatable|ipairs|load|loadfile|next|pairs|pcall|print|rawequal|rawget|rawlen|rawset|require|select|setmetatable|tonumber|tostring|type|unpack|xpcall|coroutine|io|math|os|package|string|table|utf8", "nb", false)
	addLexerRules(luaLex, []rawByteRule{
		{leads: "-", pattern: `--\[\[[\s\S]*?\]\]|--\[=\[[\s\S]*?\]=\]|--\[==\[[\s\S]*?\]==\]`, cls: "cm"},
		{leads: "-", pattern: `--[^\n]*`, cls: "c1"},
		{leads: "[", pattern: `\[\[[\s\S]*?\]\]|\[=\[[\s\S]*?\]=\]|\[==\[[\s\S]*?\]==\]`, cls: "s"},
		{leads: `"`, pattern: `"(?:\\[\s\S]|[^"\\\n])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\[\s\S]|[^'\\\n])*'`, cls: "s1"},
		{leads: ".", pattern: `\.\.\.?`, cls: "o"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F]+(?:\.[0-9a-fA-F]+)?(?:[pP][+-]?\d+)?`, cls: "mh"},
		{leads: asciiDigits, pattern: `\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`, cls: "m"},
	})
	setOpBytes(luaLex, "+-*/%^#=~<>")
	setPunctBytes(luaLex, "{}()[];,:.", "p")

	// 17. GraphQL (`#` comments, `@directive`, `$var`, keywords, builtin scalars)
	graphqlLex := &codeLexer{
		skipWS:          true,
		hasFastIdent:    true,
		detectTypeIdent: true,
		identMap: buildIdentMap(
			"directive|enum|extend|fragment|implements|input|interface|mutation|on|query|repeatable|scalar|schema|subscription|type|union",
			"Boolean|Float|ID|Int|String",
			"true|false|null",
			false,
		),
	}
	addLexerRules(graphqlLex, []rawByteRule{
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: `"`, pattern: `"""[\s\S]*?"""`, cls: "sd"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\\n])*"`, cls: "s2"},
		{leads: "@", pattern: `@[A-Za-z_][A-Za-z0-9_]*`, cls: "nd"},
		{leads: "$", pattern: `\$[A-Za-z_][A-Za-z0-9_]*`, cls: "nv"},
		{leads: ".", pattern: `\.\.\.`, cls: "o"},
		{leads: "-" + asciiDigits, pattern: `-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`, cls: "m"},
	})
	setOpBytes(graphqlLex, "=!|&")
	setPunctBytes(graphqlLex, "{}()[]:,", "p")

	// 18. Protocol Buffers (`//` & `/* */` comments, keywords, scalar types)
	protoLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		hasSlashComments: true,
		detectFuncCall:   true,
		detectTypeIdent:  true,
		identMap: buildIdentMap(
			"edition|enum|extend|extensions|import|map|max|message|oneof|option|optional|package|public|repeated|required|reserved|returns|rpc|service|stream|syntax|to|weak",
			"bool|bytes|double|fixed32|fixed64|float|int32|int64|sfixed32|sfixed64|sint32|sint64|string|uint32|uint64",
			"true|false",
			false,
		),
	}
	addLexerRules(protoLex, []rawByteRule{
		{leads: "/", pattern: `//[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\\n])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\.|[^'\\\n])*'`, cls: "s1"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F]+`, cls: "mh"},
		{leads: asciiDigits, pattern: `\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`, cls: "m"},
	})
	setOpBytes(protoLex, "=<>+-")
	setPunctBytes(protoLex, "{}()[];,.", "p")

	// 19. HCL / Terraform (`#`, `//`, `/* */` comments, blocks, types)
	hclLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		hasSlashComments: true,
		detectFuncCall:   true,
		identMap: buildIdentMap(
			"backend|count|data|dynamic|for|for_each|if|in|lifecycle|locals|module|moved|output|provider|provisioner|removed|resource|terraform|variable",
			"any|bool|list|map|number|object|set|string|tuple",
			"true|false|null",
			false,
		),
	}
	addLexerRules(hclLex, []rawByteRule{
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `//[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\\n])*"`, cls: "s2"},
		{leads: "$", pattern: `\$\{[^}\n]*\}`, cls: "nv"},
		{leads: asciiDigits, pattern: `\d+(?:\.\d+)?`, cls: "m"},
	})
	setOpBytes(hclLex, "=+-*/%&|!<>?:")
	setPunctBytes(hclLex, "{}()[],.", "p")

	// 20. Hash-comment scripting languages: Elixir / R / Perl (`#` comments only, never `--` or `//`)
	hashLangLex := &codeLexer{
		skipWS:          true,
		hasFastIdent:    true,
		detectFuncCall:  true,
		detectTypeIdent: true,
		identMap: buildIdentMap(
			"after|alias|and|case|catch|cond|def|defexception|defimpl|defmacro|defmacrop|defmodule|defoverridable|defp|defprotocol|defstruct|do|else|elsif|end|fn|for|function|if|import|in|last|library|local|my|next|not|or|our|package|quote|raise|receive|repeat|require|rescue|return|sub|try|unless|until|use|when|while|with",
			"",
			"true|false|nil|TRUE|FALSE|NULL|NA|NaN|Inf",
			false,
		),
	}
	addLexerRules(hashLangLex, []rawByteRule{
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: ":", pattern: `::`, cls: "o"},
		{leads: ":", pattern: `:[A-Za-z_][A-Za-z0-9_]*[?!]?`, cls: "ss"},
		{leads: "@", pattern: `@[A-Za-z_][A-Za-z0-9_]*`, cls: "nd"},
		{leads: "$%", pattern: `[\$@%][A-Za-z_][A-Za-z0-9_]*`, cls: "nv"},
		{leads: `"`, pattern: `"""[\s\S]*?"""|"(?:\\[\s\S]|[^"\\])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\[\s\S]|[^'\\])*'`, cls: "s1"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F_]+`, cls: "mh"},
		{leads: asciiDigits, pattern: `\d[\d_]*(?:\.\d[\d_]*)?`, cls: "m"},
	})
	setOpBytes(hashLangLex, "+-*/%&|^!~<>?=")
	setPunctBytes(hashLangLex, "{}()[];,.:", "p")

	// 21. Generic fallback for code-like unlabeled or rare-language blocks
	genericLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		identAllowDollar: true,
		hasSlashComments: true,
		hasDashComment:   true,
		detectFuncCall:   true,
		detectTypeIdent:  true,
		stmtDirective:    "na",
		identMap: buildIdentMap(
			"if|else|elif|for|while|do|switch|case|default|break|continue|return|func|function|fn|def|class|struct|interface|enum|trait|impl|type|const|let|var|val|import|from|export|package|pub|async|await|try|catch|finally|throw|new|in|of|with|as|is|match|yield|defer|go|select|server|location|upstream|http|events|listen|proxy_pass",
			"int|int64|uint|float|float64|bool|boolean|string|str|byte|void|any|error|null|auto|char|double",
			"true|false|null|nil|undefined|none|None|True|False|on|off|yes|no|NaN|Infinity",
			false,
		),
	}
	addLexerRules(genericLex, []rawByteRule{
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `//[^\n]*`, cls: "c1"},
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: "-", pattern: `--[^\n]*`, cls: "c1"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\\n])*"`, cls: "s2"},
		{leads: "'", pattern: `'(?:\\.|[^'\\\n])*'`, cls: "s1"},
		{leads: "`", pattern: "`[^`\\n]*`", cls: "sb"},
		{leads: "$", pattern: `\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`, cls: "nv"},
		{leads: "0", pattern: `0[xX][0-9a-fA-F_]+`, cls: "mh"},
		{leads: asciiDigits, pattern: `\d+(?:\.\d+)?`, cls: "m"},
	})
	setOpBytes(genericLex, "+-*/%&|^!~<>?:=")
	setPunctBytes(genericLex, "{}()[];,.", "p")
	genericCodeLexer = genericLex

	codeLexers = make(map[string]*codeLexer, 128)
	register := func(lx *codeLexer, aliases ...string) {
		for _, a := range aliases {
			codeLexers[a] = lx
		}
	}
	register(goLex, "go", "golang")
	register(tsLex, "ts", "typescript", "tsx", "js", "javascript", "jsx", "mjs", "cjs", "mts", "cts")
	register(rsLex, "rs", "rust")
	register(cFamLex, "c", "h", "cpp", "c++", "cc", "hpp", "cxx", "java", "kotlin", "kt", "csharp", "cs", "swift", "dart", "scala", "zig", "prisma")
	register(phpLex, "php", "php8", "phtml")
	register(pyLex, "py", "python", "python3")
	register(rubyLex, "ruby", "rb")
	register(luaLex, "lua")
	register(graphqlLex, "graphql", "gql")
	register(protoLex, "proto", "protobuf")
	register(hclLex, "hcl", "tf", "terraform")
	register(hashLangLex, "elixir", "ex", "exs", "r", "perl", "pl")
	register(shLex, "sh", "bash", "zsh", "fish", "ash", "shell", "console", "shellsession", "sh-session", "terminal", "dockerfile", "docker", "powershell", "ps1", "pwsh", "make", "makefile")
	register(sqlLex, "sql", "sqlite", "postgres", "postgresql", "pgsql", "mysql")
	register(jsonLex, "json", "jsonc", "json5", "geojson")
	register(dataLex, "yaml", "yml", "toml", "ini", "env", "dotenv", "properties", "systemd", "service", "editorconfig")
	register(nginxLex, "nginx", "conf", "caddy", "caddyfile", "apache", "apacheconf", "haproxy", "vhost")
	register(htmlLex, "html", "htm", "xml", "svg", "vue", "svelte")
	register(cssLex, "css", "scss", "sass", "less")
	register(diffLex, "diff", "patch")
	register(mdLex, "md", "markdown")
	register(genericLex, "haskell", "hs", "ada", "vhdl")
}

var genericCodeLexer *codeLexer

var (
	detectNginxBlockRe = regexp.MustCompile(`(?m)^\s*(?:server|http|events|stream|upstream|location|map|geo|limit_except)\b[^\n{]*\{`)
	detectNginxStmtRe  = regexp.MustCompile(`(?m)^\s*(?:listen|server_name|root|index|proxy_pass|proxy_set_header|absolute_redirect|add_header|rewrite|return|include|error_page|access_log|error_log|ssl_certificate|client_max_body_size|worker_processes|try_files)\b[^\n]*;`)
	detectCSSRuleRe    = regexp.MustCompile(`(?s)(?:@[a-z-]+\b|\{[^{}]*[a-z-]+\s*:\s*[^;{}\n]+;?[^{}]*\})`)
	detectSQLStmtRe    = regexp.MustCompile(`(?i)(?:\bselect\b[\s\S]+\bfrom\b|\binsert\s+into\b|\bupdate\b[\s\S]+\bset\b|\bdelete\s+from\b|\bcreate\s+(?:table|index|view)\b|\balter\s+table\b|\bdrop\s+(?:table|index)\b)`)
	detectYAMLKeyRe    = regexp.MustCompile(`(?m)^[ \t]*(?:-[ \t]+)?[A-Za-z0-9_.-]+:[ \t]*(?:\S.*)?$`)
	detectTOMLIniRe    = regexp.MustCompile(`(?m)^(?:\[[A-Za-z0-9_.:-]+\]|\[?\[[A-Za-z0-9_.:-]+\]\]|[A-Za-z0-9_.-]+\s*=\s*\S)`)
	detectEnvLineRe    = regexp.MustCompile(`(?m)^[A-Z][A-Z0-9_]*=`)
	detectFuncCallRe   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*\s*\(`)
	detectRubyDefRe    = regexp.MustCompile(`(?m)^\s*def\s+[A-Za-z_][A-Za-z0-9_?!.]*(?:\([^)\n]*\))?[ \t]*$`)
	detectRubyEndRe    = regexp.MustCompile(`(?m)^\s*end\b`)
	detectLuaFuncRe    = regexp.MustCompile(`(?s)\blocal\s+function\s+[A-Za-z_][A-Za-z0-9_.:]*\s*\([^)]*\).*?\bend\b`)
)

func detectCodeLexerWithLang(code string) (*codeLexer, string) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return nil, ""
	}
	sample := trimmed
	if len(sample) > 4096 {
		sample = sample[:4096]
	}

	// 1. Shebang
	if strings.HasPrefix(sample, "#!") {
		firstLine := sample
		if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
			firstLine = firstLine[:idx]
		}
		lowerFirst := strings.ToLower(firstLine)
		if strings.Contains(lowerFirst, "python") {
			return codeLexers["py"], "python"
		}
		if strings.Contains(lowerFirst, "node") || strings.Contains(lowerFirst, "deno") || strings.Contains(lowerFirst, "bun") {
			return codeLexers["ts"], "javascript"
		}
		return codeLexers["sh"], "bash"
	}

	// 2. Diff / Patch
	if strings.HasPrefix(sample, "diff --git ") || strings.Contains(sample, "\n@@ -") || strings.HasPrefix(sample, "@@ -") ||
		(strings.Contains(sample, "\n--- ") && strings.Contains(sample, "\n+++ ")) {
		return codeLexers["diff"], "diff"
	}

	// 3. HTML / XML
	lowerSample := strings.ToLower(sample)
	if strings.HasPrefix(lowerSample, "<!doctype") || strings.HasPrefix(lowerSample, "<html") ||
		strings.HasPrefix(lowerSample, "<svg") || strings.HasPrefix(lowerSample, "<?xml") ||
		(strings.Contains(lowerSample, "<div") && strings.Contains(lowerSample, "</div>")) ||
		(strings.Contains(lowerSample, "<script") && strings.Contains(lowerSample, "</script>")) {
		if strings.HasPrefix(lowerSample, "<?xml") {
			return codeLexers["xml"], "xml"
		}
		return codeLexers["html"], "html"
	}

	// 4. JSON / JSONC (avoid mistaking INI/TOML [section] headers)
	if strings.HasPrefix(sample, "{") && (strings.Contains(sample, "\":") || strings.HasSuffix(trimmed, "}")) {
		return codeLexers["json"], "json"
	}
	if strings.HasPrefix(sample, "[") && strings.HasSuffix(trimmed, "]") &&
		!strings.Contains(sample, "]\n") && !strings.Contains(sample, "]\r\n") &&
		(strings.Contains(sample, "{") || strings.Contains(sample, `"`) || strings.Contains(sample, ",")) {
		return codeLexers["json"], "json"
	}

	// 5. Nginx / Caddy / Reverse-Proxy Config
	if detectNginxBlockRe.MatchString(sample) || detectNginxStmtRe.MatchString(sample) {
		return codeLexers["nginx"], "nginx"
	}

	// 6. CSS / SCSS / LESS
	if strings.Contains(sample, "{") && strings.Contains(sample, "}") && detectCSSRuleRe.MatchString(sample) &&
		(strings.Contains(sample, "px") || strings.Contains(sample, "rem") || strings.Contains(sample, "--") ||
			strings.Contains(sample, "@media") || strings.Contains(sample, "@keyframes") ||
			strings.Contains(sample, "display:") || strings.Contains(sample, "color:") || strings.Contains(sample, "background")) {
		return codeLexers["css"], "css"
	}

	// 7. SQL
	if detectSQLStmtRe.MatchString(sample) {
		return codeLexers["sql"], "sql"
	}

	// 8. Go / Rust / Python / JS-TS / C-family
	if strings.Contains(sample, "package main") || strings.Contains(sample, "func ") && strings.Contains(sample, "{") {
		return codeLexers["go"], "go"
	}
	if strings.Contains(sample, "fn main(") || strings.Contains(sample, "pub fn ") || strings.Contains(sample, "let mut ") {
		return codeLexers["rs"], "rust"
	}
	if strings.Contains(sample, "#include <") || strings.Contains(sample, `#include "`) || strings.Contains(sample, "int main(") {
		return codeLexers["cpp"], "cpp"
	}
	if strings.Contains(sample, "<?php") {
		return codeLexers["php"], "php"
	}
	if detectLuaFuncRe.MatchString(sample) {
		return codeLexers["lua"], "lua"
	}
	if (strings.Contains(sample, "def ") && strings.Contains(sample, "):")) ||
		(strings.Contains(sample, "import ") && !strings.Contains(sample, ";") && strings.Contains(sample, "print(")) {
		return codeLexers["py"], "python"
	}
	if detectRubyDefRe.MatchString(sample) && detectRubyEndRe.MatchString(sample) {
		return codeLexers["ruby"], "ruby"
	}
	if strings.Contains(sample, "export default ") || strings.Contains(sample, "=> {") ||
		strings.Contains(sample, "console.log(") || (strings.Contains(sample, "const ") && strings.Contains(sample, ";")) {
		return codeLexers["ts"], "javascript"
	}

	// 9. Shell / CLI commands
	lines := strings.Split(sample, "\n")
	shellHits := 0
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "$ ") || strings.HasPrefix(t, "curl ") || strings.HasPrefix(t, "sudo ") ||
			strings.HasPrefix(t, "docker ") || strings.HasPrefix(t, "kubectl ") || strings.HasPrefix(t, "systemctl ") ||
			strings.HasPrefix(t, "git ") || strings.HasPrefix(t, "npm ") || strings.HasPrefix(t, "pnpm ") ||
			strings.HasPrefix(t, "go ") || strings.HasPrefix(t, "apt ") || strings.HasPrefix(t, "apt-get ") ||
			strings.HasPrefix(t, "export ") || strings.HasPrefix(t, "chmod ") || strings.HasPrefix(t, "mkdir ") {
			shellHits++
		}
	}
	if shellHits >= 1 && !strings.Contains(sample, "{") {
		return codeLexers["sh"], "bash"
	}

	// 10. Config data (TOML / INI / .env / YAML)
	if len(detectEnvLineRe.FindAllStringIndex(sample, 3)) >= 2 {
		return codeLexers["env"], "env"
	}
	if len(detectTOMLIniRe.FindAllStringIndex(sample, 3)) >= 2 {
		return codeLexers["toml"], "toml"
	}
	if (strings.HasPrefix(sample, "---\n") || len(detectYAMLKeyRe.FindAllStringIndex(sample, 3)) >= 2) &&
		!strings.Contains(sample, ";") && !strings.Contains(sample, "{") {
		return codeLexers["yaml"], "yaml"
	}

	// 11. Generic fallback ONLY if content clearly looks like code
	codeScore := 0
	if strings.Contains(sample, "{") && strings.Contains(sample, "}") {
		codeScore++
	}
	if strings.Contains(sample, ";\n") || strings.HasSuffix(trimmed, ";") {
		codeScore++
	}
	if strings.Contains(sample, "//") || strings.Contains(sample, "/*") || strings.Contains(sample, "# ") || strings.Contains(sample, "-- ") {
		codeScore++
	}
	if strings.Contains(sample, "=>") || strings.Contains(sample, ":=") || strings.Contains(sample, "==") || strings.Contains(sample, "!=") {
		codeScore++
	}
	if detectFuncCallRe.MatchString(sample) {
		codeScore++
	}
	if codeScore >= 2 {
		return genericCodeLexer, ""
	}
	return nil, ""
}

func tokenizeJSON(w *codeHTMLWriter, src string) {
	n := len(src)
	for i := 0; i < n; {
		b := src[i]
		if isASCIISpace[b] {
			j := i + 1
			for j < n && isASCIISpace[src[j]] {
				j++
			}
			w.emit("", src[i:j])
			i = j
			continue
		}
		if b == '/' && i+1 < n {
			if src[i+1] == '/' {
				j := i + 2
				for j < n && src[j] != '\n' {
					j++
				}
				w.emit("c1", src[i:j])
				i = j
				continue
			}
			if src[i+1] == '*' {
				j := i + 2
				for j+1 < n && !(src[j] == '*' && src[j+1] == '/') {
					j++
				}
				if j+1 < n {
					j += 2
				} else {
					j = n
				}
				w.emit("cm", src[i:j])
				i = j
				continue
			}
		}
		if b == '"' {
			j := i + 1
			for j < n {
				if src[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				if src[j] == '"' {
					j++
					break
				}
				j++
			}
			k := j
			for k < n && (src[k] == ' ' || src[k] == '\t' || src[k] == '\r' || src[k] == '\n') {
				k++
			}
			cls := "s2"
			if k < n && src[k] == ':' {
				cls = "nt"
			}
			w.emit(cls, src[i:j])
			i = j
			continue
		}
		if (b >= '0' && b <= '9') || (b == '-' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9') {
			j := i + 1
			isFloat := false
			for j < n {
				c := src[j]
				if c >= '0' && c <= '9' {
					j++
				} else if c == '.' || c == 'e' || c == 'E' {
					isFloat = true
					j++
				} else if (c == '+' || c == '-') && (src[j-1] == 'e' || src[j-1] == 'E') {
					j++
				} else {
					break
				}
			}
			cls := "mi"
			if isFloat {
				cls = "mf"
			}
			w.emit(cls, src[i:j])
			i = j
			continue
		}
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == '$' {
			j := i + 1
			for j < n {
				c := src[j]
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
					j++
				} else {
					break
				}
			}
			word := src[i:j]
			cls := ""
			if word == "true" || word == "false" || word == "null" {
				cls = "kc"
			} else {
				k := j
				for k < n && (src[k] == ' ' || src[k] == '\t') {
					k++
				}
				if k < n && src[k] == ':' {
					cls = "nt"
				}
			}
			w.emit(cls, word)
			i = j
			continue
		}
		if b == '{' || b == '}' || b == '[' || b == ']' || b == ':' || b == ',' {
			w.emit("p", src[i:i+1])
			i++
			continue
		}
		if b >= 0x80 {
			_, width := utf8.DecodeRuneInString(src[i:])
			if width <= 0 {
				width = 1
			}
			w.emit("", src[i:i+width])
			i += width
			continue
		}
		w.emit("", src[i:i+1])
		i++
	}
}

func tokenizeConfigData(w *codeHTMLWriter, src string) {
	n := len(src)
	atLineStart := true
	afterDelim := false
	for i := 0; i < n; {
		b := src[i]
		if isASCIISpace[b] {
			j := i + 1
			hasNL := b == '\n'
			for j < n && isASCIISpace[src[j]] {
				if src[j] == '\n' {
					hasNL = true
				}
				j++
			}
			w.emit("", src[i:j])
			if hasNL {
				atLineStart = true
				afterDelim = false
			}
			i = j
			continue
		}
		if b == '#' && (atLineStart || (i > 0 && isASCIISpace[src[i-1]])) {
			j := i + 1
			for j < n && src[j] != '\n' {
				j++
			}
			w.emit("c1", src[i:j])
			i = j
			continue
		}
		if b == ';' && atLineStart {
			j := i + 1
			for j < n && src[j] != '\n' {
				j++
			}
			w.emit("c1", src[i:j])
			i = j
			continue
		}
		if b == '/' && i+1 < n && src[i+1] == '/' {
			j := i + 2
			for j < n && src[j] != '\n' {
				j++
			}
			w.emit("c1", src[i:j])
			i = j
			continue
		}
		if atLineStart && (strings.HasPrefix(src[i:], "---") || strings.HasPrefix(src[i:], "...")) {
			end := i + 3
			if end == n || isASCIISpace[src[end]] {
				w.emit("k", src[i:end])
				atLineStart = false
				i = end
				continue
			}
		}
		if atLineStart && b == '-' && (i+1 == n || src[i+1] == ' ' || src[i+1] == '\t' || src[i+1] == '\n' || src[i+1] == '\r') {
			w.emit("k", "-")
			i++
			continue
		}
		if atLineStart && !afterDelim && b == '[' {
			end := i + 1
			for end < n && src[end] != ']' && src[end] != '\n' {
				end++
			}
			if end < n && src[end] == ']' {
				openB := "["
				innerStart := i + 1
				closeB := "]"
				innerEnd := end
				if i+1 < n && src[i+1] == '[' && end+1 < n && src[end+1] == ']' {
					openB = "[["
					innerStart = i + 2
					closeB = "]]"
					end++
				}
				w.emit("p", openB)
				if innerEnd > innerStart {
					w.emit("nc", src[innerStart:innerEnd])
				}
				w.emit("p", closeB)
				atLineStart = false
				i = end + 1
				continue
			}
		}
		if b == '"' || b == '\'' {
			q := b
			j := i + 1
			for j < n {
				if q == '"' && src[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				if src[j] == q {
					j++
					break
				}
				if src[j] == '\n' {
					break
				}
				j++
			}
			cls := "s2"
			if q == '\'' {
				cls = "s1"
			}
			if !afterDelim {
				k := j
				for k < n && (src[k] == ' ' || src[k] == '\t') {
					k++
				}
				if k < n && (src[k] == '=' || (src[k] == ':' && (k+1 == n || isASCIISpace[src[k+1]]))) {
					cls = "nt"
				}
			}
			w.emit(cls, src[i:j])
			atLineStart = false
			i = j
			continue
		}
		if b == '$' && i+1 < n && (src[i+1] == '{' || (src[i+1] >= 'A' && src[i+1] <= 'Z') || (src[i+1] >= 'a' && src[i+1] <= 'z') || src[i+1] == '_') {
			j := i + 1
			if src[j] == '{' {
				j++
				for j < n && src[j] != '}' && src[j] != '\n' {
					j++
				}
				if j < n && src[j] == '}' {
					j++
				}
			} else {
				for j < n && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
					j++
				}
			}
			w.emit("nv", src[i:j])
			atLineStart = false
			i = j
			continue
		}
		if ((b >= '0' && b <= '9') || (b == '-' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9')) && afterDelim {
			j := i + 1
			isFloat := false
			isPureNum := true
			for j < n {
				c := src[j]
				if c >= '0' && c <= '9' {
					j++
				} else if c == '.' && j+1 < n && src[j+1] >= '0' && src[j+1] <= '9' {
					isFloat = true
					j++
				} else if isASCIISpace[c] || c == ',' || c == ']' || c == '}' || c == '#' {
					break
				} else {
					isPureNum = false
					break
				}
			}
			if isPureNum {
				cls := "mi"
				if isFloat {
					cls = "mf"
				}
				w.emit(cls, src[i:j])
				atLineStart = false
				i = j
				continue
			}
		}
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-' {
			j := i + 1
			for j < n {
				c := src[j]
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || (c == '.' && !afterDelim) {
					j++
				} else {
					break
				}
			}
			word := src[i:j]
			cls := ""
			if !afterDelim {
				k := j
				for k < n && (src[k] == ' ' || src[k] == '\t') {
					k++
				}
				if k < n && (src[k] == '=' || (src[k] == ':' && (k+1 == n || isASCIISpace[src[k+1]]))) {
					cls = "nt"
				}
			}
			if cls == "" {
				switch strings.ToLower(word) {
				case "true", "false", "null", "yes", "no", "on", "off", "nil", "none":
					cls = "kc"
				}
			}
			w.emit(cls, word)
			atLineStart = false
			i = j
			continue
		}
		if b == ':' || b == '=' {
			if b == ':' && (i+1 == n || isASCIISpace[src[i+1]]) {
				afterDelim = true
				w.emit("p", ":")
			} else if b == '=' {
				afterDelim = true
				w.emit("o", "=")
			} else {
				w.emit("p", src[i:i+1])
			}
			atLineStart = false
			i++
			continue
		}
		if b == '{' || b == '}' || b == '[' || b == ']' || b == ',' {
			w.emit("p", src[i:i+1])
			atLineStart = false
			i++
			continue
		}
		if b >= 0x80 {
			_, width := utf8.DecodeRuneInString(src[i:])
			if width <= 0 {
				width = 1
			}
			w.emit("", src[i:i+width])
			atLineStart = false
			i += width
			continue
		}
		w.emit("", src[i:i+1])
		atLineStart = false
		i++
	}
}

func isCSSIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

func isCSSPropAhead(src string, afterWord int, inParens bool) bool {
	n := len(src)
	k := afterWord
	for k < n && (src[k] == ' ' || src[k] == '\t') {
		k++
	}
	if k >= n || src[k] != ':' || (k+1 < n && src[k+1] == ':') {
		return false
	}
	if inParens {
		return true
	}
	limit := k + 400
	if limit > n {
		limit = n
	}
	for m := k + 1; m < limit; m++ {
		c := src[m]
		if c == '{' {
			return false
		}
		if c == ';' || c == '}' {
			return true
		}
	}
	return true
}

func isCSSValueKeyword(lowerWord string) bool {
	switch lowerWord {
	case "none", "auto", "inherit", "initial", "unset", "revert", "revert-layer", "block", "inline", "inline-block",
		"flex", "inline-flex", "grid", "inline-grid", "subgrid", "flow-root", "table", "contents", "hidden", "visible",
		"scroll", "clip", "static", "relative", "absolute", "fixed", "sticky", "solid", "dashed",
		"dotted", "double", "bold", "normal", "italic", "center", "left", "right", "top", "bottom",
		"middle", "pointer", "default", "transparent", "currentcolor", "cover", "contain", "nowrap",
		"wrap", "column", "row", "space-between", "space-around", "space-evenly", "start", "end",
		"stretch", "baseline", "all", "and", "or", "not", "only", "screen", "print", "from", "to",
		"ease", "ease-in", "ease-out", "ease-in-out", "linear", "infinite", "alternate", "forwards",
		"backwards", "both", "paused", "running", "border-box", "content-box", "padding-box",
		"isolate", "balance", "pretty", "stable", "smooth", "pixelated", "safe", "unsafe":
		return true
	default:
		return false
	}
}

func tokenizeCSS(w *codeHTMLWriter, src string) {
	n := len(src)
	inDeclValue := false
	pendingPropColon := false
	afterPseudoColon := false
	parenDepth := 0
	braceDepth := 0
	for i := 0; i < n; {
		b := src[i]
		if isASCIISpace[b] {
			afterPseudoColon = false
			j := i + 1
			for j < n && isASCIISpace[src[j]] {
				j++
			}
			w.emit("", src[i:j])
			i = j
			continue
		}
		if b == '/' && i+1 < n {
			if src[i+1] == '*' {
				j := i + 2
				for j+1 < n && !(src[j] == '*' && src[j+1] == '/') {
					j++
				}
				if j+1 < n {
					j += 2
				} else {
					j = n
				}
				w.emit("cm", src[i:j])
				i = j
				continue
			}
			if src[i+1] == '/' {
				j := i + 2
				for j < n && src[j] != '\n' {
					j++
				}
				w.emit("c1", src[i:j])
				i = j
				continue
			}
		}
		if b == '"' || b == '\'' {
			q := b
			j := i + 1
			for j < n {
				if src[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				if src[j] == q {
					j++
					break
				}
				j++
			}
			cls := "s2"
			if q == '\'' {
				cls = "s1"
			}
			w.emit(cls, src[i:j])
			i = j
			continue
		}
		if b == '@' && i+1 < n && isCSSIdentChar(src[i+1]) {
			j := i + 2
			for j < n && isCSSIdentChar(src[j]) {
				j++
			}
			w.emit("k", src[i:j])
			inDeclValue = false
			i = j
			continue
		}
		if b == '!' && strings.HasPrefix(strings.ToLower(src[i:]), "!important") {
			w.emit("kc", src[i:i+10])
			i += 10
			continue
		}
		if b == '-' && i+1 < n && src[i+1] == '-' {
			j := i + 2
			for j < n && isCSSIdentChar(src[j]) {
				j++
			}
			w.emit("nv", src[i:j])
			i = j
			continue
		}
		if b == '$' && i+1 < n && isCSSIdentChar(src[i+1]) {
			j := i + 2
			for j < n && isCSSIdentChar(src[j]) {
				j++
			}
			w.emit("nv", src[i:j])
			i = j
			continue
		}
		if b == '#' && i+1 < n {
			j := i + 1
			isHex := true
			for j < n && isCSSIdentChar(src[j]) {
				c := src[j]
				if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
					isHex = false
				}
				j++
			}
			hexLen := j - (i + 1)
			if isHex && (hexLen == 3 || hexLen == 4 || hexLen == 6 || hexLen == 8) {
				w.emit("mh", src[i:j])
				i = j
				continue
			}
			if j > i+1 {
				w.emit("nf", src[i:j])
				i = j
				continue
			}
		}
		if b == '.' && i+1 < n && ((src[i+1] >= 'a' && src[i+1] <= 'z') || (src[i+1] >= 'A' && src[i+1] <= 'Z') || src[i+1] == '_' || src[i+1] == '-') {
			j := i + 2
			for j < n && isCSSIdentChar(src[j]) {
				j++
			}
			w.emit("nc", src[i:j])
			i = j
			continue
		}
		if (b >= '0' && b <= '9') || (b == '-' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9') || (b == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9') {
			j := i + 1
			isFloat := b == '.'
			for j < n && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				if src[j] == '.' {
					isFloat = true
				}
				j++
			}
			numCls := "mi"
			if isFloat {
				numCls = "mf"
			}
			w.emit(numCls, src[i:j])
			u := j
			if u < n && src[u] == '%' {
				u++
			} else {
				for u < n && ((src[u] >= 'a' && src[u] <= 'z') || (src[u] >= 'A' && src[u] <= 'Z')) {
					u++
				}
			}
			if u > j {
				w.emit("kt", src[j:u])
			}
			i = u
			continue
		}
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || (b == '-' && i+1 < n && ((src[i+1] >= 'a' && src[i+1] <= 'z') || (src[i+1] >= 'A' && src[i+1] <= 'Z') || src[i+1] == '_')) {
			j := i + 1
			for j < n && isCSSIdentChar(src[j]) {
				j++
			}
			word := src[i:j]
			cls := "nt"
			k := j
			for k < n && (src[k] == ' ' || src[k] == '\t') {
				k++
			}
			if afterPseudoColon {
				cls = "nd"
			} else if k < n && src[k] == '(' {
				cls = "nf"
			} else if !inDeclValue && isCSSPropAhead(src, j, parenDepth > 0) {
				cls = "na"
				pendingPropColon = true
			} else if inDeclValue {
				if isCSSValueKeyword(strings.ToLower(word)) {
					cls = "kc"
				} else {
					cls = ""
				}
			} else if braceDepth == 0 && (word == "and" || word == "or" || word == "not" || word == "only") {
				cls = "k"
			}
			afterPseudoColon = false
			w.emit(cls, word)
			i = j
			continue
		}
		if b == ';' || b == '{' || b == '}' {
			if b == '{' {
				braceDepth++
			} else if b == '}' && braceDepth > 0 {
				braceDepth--
			}
			inDeclValue = false
			pendingPropColon = false
			afterPseudoColon = false
			w.emit("p", src[i:i+1])
			i++
			continue
		}
		if b == ':' {
			if i+1 < n && src[i+1] == ':' {
				pendingPropColon = false
				afterPseudoColon = true
				w.emit("p", "::")
				i += 2
				continue
			}
			if pendingPropColon {
				inDeclValue = true
				pendingPropColon = false
				afterPseudoColon = false
			} else {
				afterPseudoColon = true
			}
			w.emit("p", ":")
			i++
			continue
		}
		if b == ',' || b == '(' || b == ')' || b == '[' || b == ']' {
			afterPseudoColon = false
			if b == '(' {
				parenDepth++
			} else if b == ')' {
				if parenDepth > 0 {
					parenDepth--
				}
				if braceDepth == 0 && parenDepth == 0 {
					inDeclValue = false
					pendingPropColon = false
				}
			}
			w.emit("p", src[i:i+1])
			i++
			continue
		}
		if b == '>' || b == '+' || b == '~' || b == '*' || b == '/' || b == '=' {
			w.emit("o", src[i:i+1])
			i++
			continue
		}
		if b >= 0x80 {
			_, width := utf8.DecodeRuneInString(src[i:])
			if width <= 0 {
				width = 1
			}
			w.emit("", src[i:i+width])
			i += width
			continue
		}
		w.emit("", src[i:i+1])
		i++
	}
}

func isShellKeyword(w string) (cls string, keepCmdStart bool) {
	switch w {
	case "if", "then", "else", "elif", "do", "case", "while", "until", "select", "time":
		return "k", true
	case "fi", "for", "in", "done", "esac", "function", "return", "exit", "export", "local", "readonly", "set", "unset", "source", ".",
		"FROM", "RUN", "CMD", "ENTRYPOINT", "COPY", "ADD", "ENV", "ARG", "WORKDIR", "EXPOSE", "VOLUME", "USER", "HEALTHCHECK", "LABEL", "SHELL":
		return "k", false
	case "sudo", "env", "nohup", "xargs", "command", "exec", "builtin":
		return "nf", true
	default:
		return "", false
	}
}

func isShellStopByte(b byte) bool {
	return isASCIISpace[b] || b == '|' || b == '&' || b == ';' || b == '<' || b == '>' || b == '(' || b == ')' || b == '"' || b == '\'' || b == '`' || b == '='
}

func tokenizeShell(w *codeHTMLWriter, src string) {
	n := len(src)
	atCmdStart := true
	afterAssign := false
	for i := 0; i < n; {
		b := src[i]
		if isASCIISpace[b] {
			j := i + 1
			hasNL := b == '\n'
			for j < n && isASCIISpace[src[j]] {
				if src[j] == '\n' {
					hasNL = true
				}
				j++
			}
			w.emit("", src[i:j])
			if hasNL {
				atCmdStart = true
				afterAssign = false
			}
			i = j
			continue
		}
		if b == '\\' && i+1 < n {
			if src[i+1] == '\n' {
				w.emit("o", "\\")
				w.emit("", "\n")
				i += 2
				continue
			}
			if src[i+1] == '\r' && i+2 < n && src[i+2] == '\n' {
				w.emit("o", "\\")
				w.emit("", "\r\n")
				i += 3
				continue
			}
		}
		if b == '#' && (atCmdStart || (i > 0 && (isASCIISpace[src[i-1]] || src[i-1] == ';' || src[i-1] == '&' || src[i-1] == '|' || src[i-1] == '('))) {
			j := i + 1
			for j < n && src[j] != '\n' {
				j++
			}
			w.emit("c1", src[i:j])
			i = j
			continue
		}
		if b == '\'' {
			j := i + 1
			for j < n && src[j] != '\'' {
				j++
			}
			if j < n {
				j++
			}
			w.emit("s1", src[i:j])
			atCmdStart = afterAssign
			afterAssign = false
			i = j
			continue
		}
		if b == '"' {
			j := i + 1
			for j < n {
				if src[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				if src[j] == '"' {
					j++
					break
				}
				j++
			}
			w.emit("s2", src[i:j])
			atCmdStart = afterAssign
			afterAssign = false
			i = j
			continue
		}
		if b == '`' {
			j := i + 1
			for j < n && src[j] != '`' {
				if src[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				j++
			}
			if j < n {
				j++
			}
			w.emit("sb", src[i:j])
			atCmdStart = afterAssign
			afterAssign = false
			i = j
			continue
		}
		if b == '$' && i+1 < n {
			if src[i+1] == '(' {
				w.emit("o", "$(")
				atCmdStart = true
				afterAssign = false
				i += 2
				continue
			}
			if src[i+1] == '{' {
				j := i + 2
				for j < n && src[j] != '}' && src[j] != '\n' {
					j++
				}
				if j < n && src[j] == '}' {
					j++
				}
				w.emit("nv", src[i:j])
				atCmdStart = afterAssign
				afterAssign = false
				i = j
				continue
			}
			if (src[i+1] >= 'A' && src[i+1] <= 'Z') || (src[i+1] >= 'a' && src[i+1] <= 'z') || src[i+1] == '_' {
				j := i + 2
				for j < n && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
					j++
				}
				w.emit("nv", src[i:j])
				atCmdStart = afterAssign
				afterAssign = false
				i = j
				continue
			}
			if strings.IndexByte("@*#?$!0123456789-", src[i+1]) >= 0 {
				w.emit("nv", src[i:i+2])
				atCmdStart = afterAssign
				afterAssign = false
				i += 2
				continue
			}
		}
		if b == '|' || b == '&' || b == ';' || b == '<' || b == '>' || b == '=' {
			j := i + 1
			hasCmdSep := b == '|' || b == '&' || b == ';'
			for j < n && (src[j] == '|' || src[j] == '&' || src[j] == ';' || src[j] == '<' || src[j] == '>' || src[j] == '=') {
				if src[j] == '|' || src[j] == '&' || src[j] == ';' {
					hasCmdSep = true
				}
				j++
			}
			w.emit("o", src[i:j])
			if b == '=' && j == i+1 {
				afterAssign = true
				atCmdStart = false
			} else if hasCmdSep {
				atCmdStart = true
				afterAssign = false
			} else {
				atCmdStart = false
			}
			i = j
			continue
		}
		if b == '(' || b == ')' || b == '{' || b == '}' || b == '[' || b == ']' {
			w.emit("p", src[i:i+1])
			atCmdStart = b == '(' || b == '{'
			afterAssign = false
			i++
			continue
		}
		if b == '-' && (i == 0 || isASCIISpace[src[i-1]]) {
			j := i + 1
			for j < n && !isShellStopByte(src[j]) && src[j] < 0x80 {
				j++
			}
			if j > i+1 {
				w.emit("na", src[i:j])
				atCmdStart = false
				i = j
				continue
			}
		}
		if b >= 0x80 {
			_, width := utf8.DecodeRuneInString(src[i:])
			if width <= 0 {
				width = 1
			}
			w.emit("", src[i:i+width])
			atCmdStart = afterAssign
			afterAssign = false
			i += width
			continue
		}
		j := i + 1
		for j < n && !isShellStopByte(src[j]) && src[j] < 0x80 {
			j++
		}
		word := src[i:j]
		if atCmdStart && j < n && src[j] == '=' {
			w.emit("nv", word)
			w.emit("o", "=")
			atCmdStart = false
			afterAssign = true
			i = j + 1
			continue
		}
		cls := ""
		nextCmdStart := afterAssign
		afterAssign = false
		if atCmdStart {
			if kwCls, keep := isShellKeyword(word); kwCls != "" {
				cls = kwCls
				nextCmdStart = keep
			} else {
				cls = "nf"
				nextCmdStart = false
			}
		} else {
			if word == "true" || word == "false" {
				cls = "kc"
			} else {
				isNum := true
				dots := 0
				for k := 0; k < len(word); k++ {
					c := word[k]
					if c >= '0' && c <= '9' {
						continue
					}
					if c == '.' && dots == 0 && k > 0 && k+1 < len(word) {
						dots++
						continue
					}
					isNum = false
					break
				}
				if isNum && len(word) > 0 {
					cls = "m"
				}
			}
		}
		w.emit(cls, word)
		atCmdStart = nextCmdStart
		i = j
	}
}
