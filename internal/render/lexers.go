package render

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed lexers_spec.gz
var packedLexersGZ []byte

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
	key = strings.TrimPrefix(key, "{.")
	key = strings.TrimPrefix(key, "{")
	key = strings.TrimPrefix(key, ".")
	for _, sep := range []byte{'{', ',', ';', ':'} {
		if idx := strings.IndexByte(key, sep); idx >= 0 {
			key = key[:idx]
		}
	}
	key = strings.TrimRight(key, "}")
	key = strings.TrimPrefix(key, "language-")
	key = strings.TrimPrefix(key, "lang-")
	return strings.TrimSpace(key)
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
	if strings.ContainsRune(code, '\r') {
		code = strings.ReplaceAll(code, "\r\n", "\n")
		code = strings.ReplaceAll(code, "\r", "\n")
	}
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
	if strings.ContainsRune(code, '\r') {
		code = strings.ReplaceAll(code, "\r\n", "\n")
		code = strings.ReplaceAll(code, "\r", "\n")
	}
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

func initCodeLexers() {
	codeLexers = make(map[string]*codeLexer, 128)
	gr, err := gzip.NewReader(bytes.NewReader(packedLexersGZ))
	if err != nil {
		return
	}
	raw, err := io.ReadAll(gr)
	_ = gr.Close()
	if err != nil {
		return
	}

	buildIdentMap := func(keywords, types, consts, builtins string, fold bool) map[string]string {
		if keywords == "" && types == "" && consts == "" && builtins == "" {
			return nil
		}
		m := make(map[string]string, 64)
		setWordMap(m, keywords, "k", fold)
		setWordMap(m, types, "kt", fold)
		setWordMap(m, consts, "kc", fold)
		setWordMap(m, builtins, "nb", false)
		return m
	}

	for _, rec := range strings.Split(string(raw), "\x1e") {
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) < 14 {
			continue
		}
		flags, _ := strconv.Atoi(f[1])
		pmVal, pmErr := strconv.ParseUint(f[2], 10, 8)
		if pmErr != nil {
			pmVal = 0
		}
		lx := &codeLexer{
			skipWS:           flags&1 != 0,
			hasFastIdent:     flags&2 != 0,
			identAllowDollar: flags&4 != 0,
			identFoldCase:    flags&8 != 0,
			rustMacroBang:    flags&16 != 0,
			hasSlashComments: flags&32 != 0,
			hasDashComment:   flags&64 != 0,
			detectFuncCall:   flags&128 != 0,
			detectTypeIdent:  flags&256 != 0,
			prefixMode:       identPrefixMode(uint8(pmVal)),
			dollarVarCls:     f[3],
			stmtDirective:    f[4],
			identMap:         buildIdentMap(f[5], f[6], f[7], f[8], flags&8 != 0),
		}
		switch f[0] {
		case "sh":
			lx.customScan = tokenizeShell
		case "json":
			lx.customScan = tokenizeJSON
		case "data":
			lx.customScan = tokenizeConfigData
		case "html":
			lx.customScan = tokenizeHTML
		case "css":
			lx.customScan = tokenizeCSS
		}
		if f[9] != "" {
			setOpBytes(lx, f[9])
		}
		if f[10] != "" {
			setPunctBytes(lx, f[10], f[11])
		}
		if f[12] != "" {
			rawRules := strings.Split(f[12], "\x1d")
			rules := make([]rawByteRule, 0, len(rawRules))
			for _, rr := range rawRules {
				parts := strings.Split(rr, "\x1c")
				if len(parts) == 3 {
					rules = append(rules, rawByteRule{leads: parts[0], pattern: parts[1], cls: parts[2]})
				}
			}
			addLexerRules(lx, rules)
		}
		if flags&512 != 0 {
			genericCodeLexer = lx
		}
		for _, alias := range strings.Split(f[13], ",") {
			if alias != "" {
				codeLexers[alias] = lx
			}
		}
	}
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
