package render

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"sync"
)

type chromaRule struct {
	re  *regexp.Regexp
	cls string // Chroma CSS class ("k", "kt", "kc", "s", "s1", "s2", "sb", "c1", "cm", "m", "nf", "na", "nt", "nv", "o", "p", "gh", "gi", "gd")
}

type codeLexer struct {
	rules []chromaRule
}

var (
	codeLexerOnce sync.Once
	codeLexers    map[string]*codeLexer
)

func highlightCodeChroma(code string, lang string, withLineNumbers bool) string {
	codeLexerOnce.Do(initCodeLexers)

	key := strings.ToLower(strings.TrimSpace(lang))
	if fields := strings.Fields(key); len(fields) > 0 {
		key = fields[0]
	}
	lx := codeLexers[key]

	var inner strings.Builder
	inner.Grow(len(code) + len(code)/2)

	if withLineNumbers {
		lines := strings.Split(code, "\n")
		if len(lines) > 1 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		for idx, ln := range lines {
			fmt.Fprintf(&inner, `<span class="line"><span class="ln">%d</span><span class="cl">`, idx+1)
			tokenizeInto(&inner, ln, lx)
			if idx < len(lines)-1 {
				inner.WriteByte('\n')
			}
			inner.WriteString(`</span></span>`)
		}
	} else {
		tokenizeInto(&inner, code, lx)
	}

	langAttr := ""
	if lang != "" {
		langAttr = ` data-lang="` + html.EscapeString(lang) + `"`
	}
	return `<figure class="volo-codeblock"` + langAttr + `><pre class="chroma"><code>` + inner.String() + `</code></pre></figure>`
}

func highlightCommentCodeChroma(code string, lang string) string {
	codeLexerOnce.Do(initCodeLexers)

	key := strings.ToLower(strings.TrimSpace(lang))
	if fields := strings.Fields(key); len(fields) > 0 {
		key = fields[0]
	}
	lx := codeLexers[key]

	var inner strings.Builder
	inner.Grow(len(code) + len(code)/2)
	tokenizeInto(&inner, code, lx)
	return `<pre class="chroma"><code>` + inner.String() + `</code></pre>`
}

func tokenizeInto(out *strings.Builder, src string, lx *codeLexer) {
	if lx == nil || len(src) == 0 {
		out.WriteString(html.EscapeString(src))
		return
	}
	i := 0
	n := len(src)
	for i < n {
		rem := src[i:]
		matched := false
		for _, r := range lx.rules {
			loc := r.re.FindStringIndex(rem)
			if loc != nil && loc[0] == 0 && loc[1] > 0 {
				seg := rem[:loc[1]]
				if r.cls == "" {
					out.WriteString(html.EscapeString(seg))
				} else {
					out.WriteString(`<span class="`)
					out.WriteString(r.cls)
					out.WriteString(`">`)
					out.WriteString(html.EscapeString(seg))
					out.WriteString(`</span>`)
				}
				i += loc[1]
				matched = true
				break
			}
		}
		if !matched {
			out.WriteString(html.EscapeString(src[i : i+1]))
			i++
		}
	}
}

func compileRuleList(pairs [][2]string) *codeLexer {
	rules := make([]chromaRule, 0, len(pairs))
	for _, p := range pairs {
		rules = append(rules, chromaRule{
			re:  regexp.MustCompile("^(?:" + p[0] + ")"),
			cls: p[1],
		})
	}
	return &codeLexer{rules: rules}
}

func initCodeLexers() {
	cStyle := func(keywords, types string) *codeLexer {
		return compileRuleList([][2]string{
			{`\s+`, ""},
			{`//[^\n]*`, "c1"},
			{`/\*[\s\S]*?\*/`, "cm"},
			{"`[^`]*`", "sb"},
			{`"(?:\\.|[^"\\\n])*"`, "s2"},
			{`'(?:\\.|[^'\\\n])*'`, "s1"},
			{`@[A-Za-z_][A-Za-z0-9_]*`, "nd"},
			{`(?:` + keywords + `)\b`, "k"},
			{`(?:` + types + `)\b`, "kt"},
			{`(?:true|false|null|nil|undefined|iota|None)\b`, "kc"},
			{`0[xX][0-9a-fA-F_]+|0[bB][01_]+|\d[\d_]*(?:\.[\d_]+)?(?:[eE][+-]?\d+)?`, "m"},
			{`[A-Za-z_][A-Za-z0-9_]*`, ""},
			{`[+\-*/%&|^!~<>?:=]+`, "o"},
			{`[{}()\[\];,.]`, "p"},
		})
	}

	goLex := cStyle(
		`break|case|chan|const|continue|default|defer|else|fallthrough|for|func|go|goto|if|import|interface|map|package|range|return|select|struct|switch|type|var`,
		`any|bool|byte|comparable|complex64|complex128|error|float32|float64|int|int8|int16|int32|int64|rune|string|uint|uint8|uint16|uint32|uint64|uintptr`,
	)
	tsLex := cStyle(
		`async|await|break|case|catch|class|const|continue|debugger|default|delete|do|else|enum|export|extends|finally|for|from|function|if|implements|import|in|instanceof|interface|let|new|of|package|private|protected|public|readonly|return|static|super|switch|this|throw|try|type|typeof|var|void|while|with|yield`,
		`any|bigint|boolean|never|number|object|string|symbol|unknown|Promise|Record|Partial|Array|Map|Set`,
	)
	rsLex := cStyle(
		`as|async|await|break|const|continue|crate|dyn|else|enum|extern|fn|for|if|impl|in|let|loop|match|mod|move|mut|pub|ref|return|self|Self|static|struct|super|trait|type|unsafe|use|where|while`,
		`bool|char|f32|f64|i8|i16|i32|i64|i128|isize|str|String|u8|u16|u32|u64|u128|usize|Vec|Option|Result|Box`,
	)
	cFamLex := cStyle(
		`abstract|assert|async|await|break|case|catch|class|const|continue|default|do|else|enum|extends|final|finally|for|goto|guard|if|implements|import|in|interface|internal|let|namespace|new|open|override|package|private|protected|public|return|sealed|sizeof|static|struct|super|switch|synchronized|this|throw|throws|try|typedef|using|val|var|virtual|volatile|when|where|while`,
		`auto|bool|boolean|byte|char|decimal|double|float|int|int32_t|int64_t|long|short|signed|size_t|string|String|uint32_t|uint64_t|unsigned|void`,
	)
	pyLex := compileRuleList([][2]string{
		{`\s+`, ""},
		{`#[^\n]*`, "c1"},
		{`"""[\s\S]*?"""|'''[\s\S]*?'''`, "sd"},
		{`[fFrRbBuU]?"(?:\\.|[^"\\\n])*"|[fFrRbBuU]?'(?:\\.|[^'\\\n])*'`, "s"},
		{`@[A-Za-z_][A-Za-z0-9_.]*`, "nd"},
		{`(?:and|as|assert|async|await|break|class|continue|def|del|elif|else|except|finally|for|from|global|if|import|in|is|lambda|match|nonlocal|not|or|pass|raise|return|try|while|with|yield)\b`, "k"},
		{`(?:True|False|None|self|cls)\b`, "kc"},
		{`(?:int|float|str|bytes|bool|list|dict|set|tuple|Any|Optional)\b`, "kt"},
		{`\d[\d_]*(?:\.[\d_]+)?`, "m"},
		{`[A-Za-z_][A-Za-z0-9_]*`, ""},
		{`[+\-*/%&|^~<>!=:@]+`, "o"},
		{`[{}()\[\];,.]`, "p"},
	})
	shLex := compileRuleList([][2]string{
		{`\s+`, ""},
		{`#[^\n]*`, "c1"},
		{`"(?:\\.|[^"\\])*"`, "s2"},
		{`'[^']*'`, "s1"},
		{`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|\$[@*#?$!0-9-]`, "nv"},
		{`(?:if|then|else|elif|fi|for|in|do|done|case|esac|while|until|function|return|exit|export|local|readonly|set|unset|FROM|RUN|CMD|ENTRYPOINT|COPY|ADD|ENV|ARG|WORKDIR|EXPOSE|VOLUME|USER|HEALTHCHECK)\b`, "k"},
		{`--?[A-Za-z0-9][A-Za-z0-9_-]*`, "na"},
		{`\d+`, "m"},
		{`[|&;<>()=]+`, "o"},
		{`[^\s#"'$|&;<>()=]+`, ""},
	})
	sqlLex := compileRuleList([][2]string{
		{`\s+`, ""},
		{`--[^\n]*`, "c1"},
		{`/\*[\s\S]*?\*/`, "cm"},
		{`'(?:''|[^'])*'`, "s1"},
		{`"(?:""|[^"])*"|` + "`[^`]*`", ""},
		{`(?i)(?:SELECT|FROM|WHERE|INSERT|INTO|VALUES|UPDATE|SET|DELETE|CREATE|ALTER|DROP|TABLE|INDEX|UNIQUE|PRIMARY|KEY|FOREIGN|REFERENCES|JOIN|LEFT|RIGHT|INNER|OUTER|ON|AS|AND|OR|NOT|NULL|IS|IN|EXISTS|BETWEEN|LIKE|ORDER|BY|GROUP|HAVING|LIMIT|OFFSET|UNION|ALL|DISTINCT|CASE|WHEN|THEN|ELSE|END|BEGIN|COMMIT|ROLLBACK|TRANSACTION|PRAGMA|WITH|RETURNING|CONFLICT|DO|NOTHING|DEFAULT|CASCADE|IF)\b`, "k"},
		{`(?i)(?:INTEGER|INT|BIGINT|TEXT|VARCHAR|BOOLEAN|BOOL|BLOB|BYTEA|REAL|DOUBLE|NUMERIC|TIMESTAMP|JSON|JSONB)\b`, "kt"},
		{`\d+(?:\.\d+)?`, "m"},
		{`[=<>!+\-*/%|]+`, "o"},
		{`[(),;.]`, "p"},
		{`[A-Za-z_][A-Za-z0-9_$]*`, ""},
	})
	dataLex := compileRuleList([][2]string{
		{`\s+`, ""},
		{`[#;][^\n]*|//[^\n]*`, "c1"},
		{`"(?:\\.|[^"\\])*"|'[^']*'`, "s2"},
		{`(?:true|false|null|yes|no|on|off)\b`, "kc"},
		{`-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`, "m"},
		{`[A-Za-z0-9_.-]+`, "na"},
		{`[\[\]{},:=]`, "p"},
	})
	htmlLex := compileRuleList([][2]string{
		{`<!--[\s\S]*?-->`, "cm"},
		{`<!DOCTYPE[^>]*>`, "cp"},
		{`</?[A-Za-z][A-Za-z0-9:-]*|/?>`, "nt"},
		{`"(?:\\.|[^"\\])*"|'[^']*'`, "s"},
		{`[A-Za-z_:][A-Za-z0-9_.:-]*`, "na"},
		{`&[A-Za-z0-9#]+;`, "ni"},
		{`=|[^<&"'>=]+`, ""},
	})
	cssLex := compileRuleList([][2]string{
		{`\s+`, ""},
		{`/\*[\s\S]*?\*/`, "cm"},
		{`@[A-Za-z_-]+`, "k"},
		{`--[A-Za-z0-9_-]+`, "nv"},
		{`#[0-9a-fA-F]{3,8}\b`, "m"},
		{`-?\d+(?:\.\d+)?(?:px|rem|em|vh|vw|%|s|ms|deg|fr)?`, "m"},
		{`"[^"]*"|'[^']*'`, "s"},
		{`[.#]?[A-Za-z_-][A-Za-z0-9_-]*`, "nt"},
		{`[{};:,()>+~*\[\]=]`, "p"},
	})
	diffLex := compileRuleList([][2]string{
		{`\+[^\n]*`, "gi"},
		{`-[^\n]*`, "gd"},
		{`@@[^\n]*`, "gu"},
		{`#{1,6}\s[^\n]*`, "gh"},
		{`[^\n]+|\n`, ""},
	})
	mdLex := compileRuleList([][2]string{
		{`#{1,6}\s[^\n]*`, "gh"},
		{"```[^\\n]*|`[^`\\n]+`", "sb"},
		{`\*\*[^*\n]+\*\*|__[^_\n]+__`, "gs"},
		{`!?\[[^\]\n]*\]\([^)\n]*\)`, "nt"},
		{`>\s?`, "k"},
		{`(?:[-*+]|\d+\.)\s`, "k"},
		{"[^\\n`*_\\[\\]#>+-]+|.|\\n", ""},
	})

	codeLexers = map[string]*codeLexer{}
	register := func(lx *codeLexer, aliases ...string) {
		for _, a := range aliases {
			codeLexers[a] = lx
		}
	}
	register(goLex, "go", "golang")
	register(tsLex, "ts", "typescript", "tsx", "js", "javascript", "jsx", "mjs", "cjs")
	register(rsLex, "rs", "rust")
	register(cFamLex, "c", "h", "cpp", "c++", "cc", "hpp", "java", "kotlin", "kt", "csharp", "cs", "swift", "php")
	register(pyLex, "py", "python", "python3")
	register(shLex, "sh", "bash", "zsh", "shell", "console", "dockerfile", "docker", "nginx")
	register(sqlLex, "sql", "sqlite", "postgres", "postgresql", "pgsql", "mysql")
	register(dataLex, "json", "jsonc", "yaml", "yml", "toml", "ini", "env", "dotenv", "conf")
	register(htmlLex, "html", "htm", "xml", "svg", "vue", "svelte")
	register(cssLex, "css", "scss", "sass", "less")
	register(diffLex, "diff", "patch")
	register(mdLex, "md", "markdown")
}
