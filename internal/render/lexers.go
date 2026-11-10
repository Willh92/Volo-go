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
)

type codeLexer struct {
	customScan       func(w *codeHTMLWriter, src string)
	skipWS           bool
	hasFastIdent     bool
	identAllowDollar bool
	identFoldCase    bool
	rustMacroBang    bool
	hasSlashComments bool
	hasDashComment   bool
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

func resolveCodeLexer(lang string) *codeLexer {
	codeLexerOnce.Do(initCodeLexers)
	key := strings.ToLower(strings.TrimSpace(lang))
	if fields := strings.Fields(key); len(fields) > 0 {
		key = fields[0]
	}
	return codeLexers[key]
}

func highlightCodeChroma(code string, lang string, withLineNumbers bool) string {
	lx := resolveCodeLexer(lang)

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
	lx := resolveCodeLexer(lang)

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
	n := len(src)
	for i := 0; i < n; {
		b := src[i]

		if lx.skipWS && isASCIISpace[b] {
			j := i + 1
			for j < n && isASCIISpace[src[j]] {
				j++
			}
			w.emit("", src[i:j])
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
				i = j + 1
				continue
			}
			ident := src[i:j]
			cls := lx.defaultIdentCls
			if lx.identMap != nil {
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
				}
			}
			w.emit(cls, ident)
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
			for j < n && lx.opBytes[src[j]] {
				if lx.hasSlashComments && src[j] == '/' && j+1 < n && (src[j+1] == '/' || src[j+1] == '*') {
					break
				}
				if lx.hasDashComment && src[j] == '-' && j+1 < n && src[j+1] == '-' {
					break
				}
				j++
			}
			w.emit("o", src[i:j])
			i = j
			continue
		}

		if pCls := lx.punctCls[b]; pCls != "" {
			w.emit(pCls, src[i:i+1])
			i++
			continue
		}

		w.emit("", src[i:i+1])
		i++
	}
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

	// 2. TypeScript / JavaScript / JSX / TSX
	tsLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		identAllowDollar: true,
		hasSlashComments: true,
		identMap: buildIdentMap(
			"async|await|break|case|catch|class|const|continue|debugger|default|delete|do|else|enum|export|extends|finally|for|from|function|if|implements|import|in|instanceof|interface|let|new|of|package|private|protected|public|readonly|return|static|super|switch|this|throw|try|type|typeof|var|void|while|with|yield",
			"any|bigint|boolean|never|number|object|string|symbol|unknown|Promise|Record|Partial|Array|Map|Set",
			"true|false|null|undefined|NaN|Infinity",
			false,
		),
	}
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
		skipWS:       true,
		hasFastIdent: true,
		prefixMode:   prefixPython,
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

	// 6. Shell / Bash / Dockerfile
	shLex := &codeLexer{
		skipWS: true,
	}
	addLexerRules(shLex, []rawByteRule{
		{leads: "#", pattern: `#[^\n]*`, cls: "c1"},
		{leads: `"`, pattern: `"(?:\\.|[^"\\])*"`, cls: "s2"},
		{leads: "'", pattern: `'[^']*'`, cls: "s1"},
		{leads: "$", pattern: `\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|\$[@*#?$!0-9-]`, cls: "nv"},
		{leads: "itefdcwflrsuFRCEAWVUH", pattern: `(?:if|then|else|elif|fi|for|in|do|done|case|esac|while|until|function|return|exit|export|local|readonly|set|unset|FROM|RUN|CMD|ENTRYPOINT|COPY|ADD|ENV|ARG|WORKDIR|EXPOSE|VOLUME|USER|HEALTHCHECK)\b`, cls: "k"},
		{leads: "-", pattern: `--?[A-Za-z0-9][A-Za-z0-9_-]*`, cls: "na"},
		{leads: asciiDigits, pattern: `\d+\b`, cls: "m"},
		{leads: "|&;<>()=", pattern: `[|&;<>()=]+`, cls: "o"},
		{leads: "", pattern: `[^\s#"'$|&;<>()=]+`, cls: ""},
	})

	// 7. SQL
	sqlLex := &codeLexer{
		skipWS:           true,
		hasFastIdent:     true,
		identAllowDollar: true,
		identFoldCase:    true,
		hasSlashComments: true,
		hasDashComment:   true,
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

	// 8. JSON / YAML / TOML / INI / ENV
	dataLex := &codeLexer{
		skipWS: true,
	}
	addLexerRules(dataLex, []rawByteRule{
		{leads: "#;/", pattern: `[#;][^\n]*|//[^\n]*`, cls: "c1"},
		{leads: `"'`, pattern: `"(?:\\.|[^"\\])*"|'[^']*'`, cls: "s2"},
		{leads: "tfnyo", pattern: `(?:true|false|null|yes|no|on|off)\b`, cls: "kc"},
		{leads: "-" + asciiDigits, pattern: `-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`, cls: "m"},
		{leads: asciiAlpha + asciiDigits + "_.-", pattern: `[A-Za-z0-9_.-]+`, cls: "na"},
	})
	setPunctBytes(dataLex, "[],{}:=", "p")

	// 9. HTML / XML / SVG / Vue / Svelte (tag-state-aware scanner)
	htmlLex := &codeLexer{
		customScan: tokenizeHTML,
	}

	// 10. CSS / SCSS / LESS
	cssLex := &codeLexer{
		skipWS: true,
	}
	addLexerRules(cssLex, []rawByteRule{
		{leads: "/", pattern: `/\*[\s\S]*?\*/`, cls: "cm"},
		{leads: "@", pattern: `@[A-Za-z_-]+`, cls: "k"},
		{leads: "-", pattern: `--[A-Za-z0-9_-]+`, cls: "nv"},
		{leads: "#", pattern: `#[0-9a-fA-F]{3,8}\b`, cls: "m"},
		{leads: "-" + asciiDigits, pattern: `-?\d+(?:\.\d+)?(?:px|rem|em|vh|vw|%|s|ms|deg|fr)?`, cls: "m"},
		{leads: `"'`, pattern: `"[^"]*"|'[^']*'`, cls: "s"},
		{leads: ".#_" + asciiAlpha + "-", pattern: `[.#]?[A-Za-z_-][A-Za-z0-9_-]*`, cls: "nt"},
	})
	setPunctBytes(cssLex, "{};:,()>+~*[]=", "p")

	// 11. Diff / Patch
	diffLex := &codeLexer{}
	addLexerRules(diffLex, []rawByteRule{
		{leads: "+", pattern: `\+[^\n]*`, cls: "gi"},
		{leads: "-", pattern: `-[^\n]*`, cls: "gd"},
		{leads: "@", pattern: `@@[^\n]*`, cls: "gu"},
		{leads: "#", pattern: `#{1,6}\s[^\n]*`, cls: "gh"},
		{leads: "", pattern: `[^\n]+|\n`, cls: ""},
	})

	// 12. Markdown
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

	codeLexers = make(map[string]*codeLexer, 64)
	register := func(lx *codeLexer, aliases ...string) {
		for _, a := range aliases {
			codeLexers[a] = lx
		}
	}
	register(goLex, "go", "golang")
	register(tsLex, "ts", "typescript", "tsx", "js", "javascript", "jsx", "mjs", "cjs")
	register(rsLex, "rs", "rust")
	register(cFamLex, "c", "h", "cpp", "c++", "cc", "hpp", "cxx", "java", "kotlin", "kt", "csharp", "cs", "swift", "php", "dart")
	register(pyLex, "py", "python", "python3")
	register(shLex, "sh", "bash", "zsh", "shell", "console", "dockerfile", "docker", "nginx")
	register(sqlLex, "sql", "sqlite", "postgres", "postgresql", "pgsql", "mysql")
	register(dataLex, "json", "jsonc", "yaml", "yml", "toml", "ini", "env", "dotenv", "conf")
	register(htmlLex, "html", "htm", "xml", "svg", "vue", "svelte")
	register(cssLex, "css", "scss", "sass", "less")
	register(diffLex, "diff", "patch")
	register(mdLex, "md", "markdown")
}
