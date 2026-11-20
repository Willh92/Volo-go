package render

import (
	"strings"
	"testing"
)

func TestM1RenderEdgeCases(t *testing.T) {
	eng := Default()

	t.Run("TablesSkipCodeAndHandleNested", func(t *testing.T) {
		md := "```html\n<table><tr><td>not prose</td></tr></table>\n```\n\n| A | B |\n|---|---|\n| 1 | 2 |\n"
		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if strings.Contains(out.HTML, `<div class="volo-table-scroll"><table><tr><td>not prose`) {
			t.Fatalf("code block table must not be wrapped: %s", out.HTML)
		}
		if strings.Count(out.HTML, `class="volo-table-scroll"`) != 1 {
			t.Fatalf("expected exactly 1 wrapped prose table, got: %s", out.HTML)
		}
		nestedHTML := enhanceTablesAndGitHubShowcase(`<table><tr><td><table><tr><td>inner</td></tr></table></td></tr></table>`)
		if strings.Count(nestedHTML, `class="volo-table-scroll"`) != 1 || !strings.HasSuffix(nestedHTML, `</table></div>`) {
			t.Fatalf("nested table mis-wrapped: %s", nestedHTML)
		}
	})

	t.Run("CrossTagHighlightWithVoidElements", func(t *testing.T) {
		md := `==before <img src="/a.png" alt="pic"> after== and ==word<wbr>break== and ==mark<br>text==`
		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if strings.Count(out.HTML, "<mark>") != 3 || strings.Count(out.HTML, "</mark>") != 3 {
			t.Fatalf("expected 3 closed <mark> tags across void elements, got: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, "<mark>mark<br>text</mark>") {
			t.Fatalf("expected ==mark<br>text== rendered with <mark>, got: %s", out.HTML)
		}
		// Block boundaries such as <p> or <div> must still abort unclosed ==.
		blockOut := eng.Render("==unclosed\n\n<div>next</div>==", Options{Mode: RenderModeWeb})
		if strings.Contains(blockOut.HTML, "<mark>") {
			t.Fatalf("block boundary must still abort unclosed highlight: %s", blockOut.HTML)
		}
	})

	t.Run("WrapTablesWithTransformQuotedGT", func(t *testing.T) {
		wrapped := enhanceTablesAndGitHubShowcase(`<table title="a>b"><tr><td>cell</td></tr></table>`)
		want := `<div class="volo-table-scroll"><table title="a>b"><tr><td>cell</td></tr></table></div>`
		if wrapped != want {
			t.Fatalf("expected quoted '>' in <table> attr scanned cleanly:\ngot:  %s\nwant: %s", wrapped, want)
		}
	})

	t.Run("EnhanceImagesPostSanitizeAndCaseInsensitive", func(t *testing.T) {
		md := "<img src=\"/pic.png\" alt=\"a>b\" LOADING=\"eager\">\n\n```html\n<img src=\"/in-code.png\">\n```\n"
		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if out.FirstImageURL != "/pic.png" {
			t.Fatalf("expected FirstImageURL=/pic.png, got %q", out.FirstImageURL)
		}
		if strings.Contains(out.HTML, `decoding="async"`) == false {
			t.Fatalf("expected decoding=async injected: %s", out.HTML)
		}
		if strings.Contains(out.HTML, `alt="a>b"`) || strings.Contains(out.HTML, `alt="a&gt;b"`) == false {
			t.Fatalf("expected sanitized alt=\"a&gt;b\" without split inside attr: %s", out.HTML)
		}
		if strings.Count(strings.ToLower(out.HTML), "loading=") != 1 {
			t.Fatalf("expected existing LOADING=\"eager\" respected (not duplicated) and code block skipped: %s", out.HTML)
		}
	})

	t.Run("ListInterruptedByBlockquoteBeforeIndentedCode", func(t *testing.T) {
		md := "- item\n> quote\n\n    :::lock\n    secret\n    :::\n"
		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if out.HasLockBlock || strings.Contains(out.HTML, "volo-lock") {
			t.Fatalf("indented code after list->blockquote must NOT become live :::lock directive: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `<code>:::lock`) {
			t.Fatalf("expected CommonMark indented code block, got: %s", out.HTML)
		}
	})

	t.Run("FenceInfoAttrsPrefixCRLFAndAliases", func(t *testing.T) {
		for _, tc := range []struct {
			fence string
			want  string
		}{
			{"```go{1,3-5}\npackage main\n```", `data-lang="go"`},
			{"```{.python .numberLines}\ndef f(): pass\n```", `data-lang="python"`},
			{"```language-go\nvar x = 1\n```", `data-lang="go"`},
			{"```c#\nvar x = 1;\n```", `data-lang="c#"`},
			{"```objc\nreturn 1;\n```", `data-lang="objc"`},
			{"```bat\nif true; then echo hi; fi\n```", `data-lang="bat"`},
		} {
			out := eng.Render(tc.fence, Options{Mode: RenderModeWeb})
			if !strings.Contains(out.HTML, tc.want) || !strings.Contains(out.HTML, `<span class="k">`) {
				t.Fatalf("fence %q: expected %s and highlighted keyword span, got: %s", tc.fence, tc.want, out.HTML)
			}
		}

		// Direct HighlightCodeHTML with CRLF + line numbers must normalize \r\n without stray \r
		hl := HighlightCodeHTML("package main\r\nfunc main() {}\r\n", "go", true)
		if strings.ContainsRune(hl, '\r') {
			t.Fatalf("expected CRLF normalized without stray \\r: %q", hl)
		}
		if strings.Count(hl, `<span class="line">`) != 2 {
			t.Fatalf("expected 2 numbered lines, got: %s", hl)
		}
	})

	t.Run("HTMLAttrBackslashDoesNotEscapeQuote", func(t *testing.T) {
		hl := HighlightCodeHTML(`<input value="C:\"><span>ok</span>`, "html", false)
		if !strings.Contains(hl, `<span class="s">&#34;C:\&#34;</span>`) || !strings.Contains(hl, `<span class="nt">&lt;span&gt;</span>`) {
			t.Fatalf("expected HTML attribute string to close at quote after backslash, got: %s", hl)
		}
	})
}

func TestAllLexerAliasesHighlight(t *testing.T) {
	// Ensure all 22 lexer families and key aliases resolve and produce highlighted spans.
	samples := []struct {
		lang string
		code string
		want string
	}{
		{"go", "package main\nfunc f() int { return len(\"hi\") }", `<span class="nb">len</span>`},
		{"tsx", "const x: Promise<number> = fetch('/a')", `<span class="nb">fetch</span>`},
		{"rust", "fn main() { let s: Option<i32> = None; }", `<span class="kc">None</span>`},
		{"c#", "public class Foo { void Bar() { return; } }", `<span class="k">public</span>`},
		{"php", "<?php $x = true; echo strlen('a'); ?>", `<span class="nv">$x</span>`},
		{"python", "def f(x: int) -> bool:\n    return len([1]) == 1", `<span class="nb">len</span>`},
		{"ruby", "def hello(x)\n  puts :ok\nend", `<span class="ss">:ok</span>`},
		{"lua", "local function f() return math.max(1, 2) end", `<span class="nb">math</span>`},
		{"graphql", "query Q($id: ID!) { user(id: $id) { name } }", `<span class="kt">ID</span>`},
		{"proto", "syntax = \"proto3\"; message M { string id = 1; }", `<span class="k">message</span>`},
		{"hcl", "resource \"aws_s3\" \"b\" { count = 1 }", `<span class="k">resource</span>`},
		{"elixir", "defmodule M do def f, do: :ok end", `<span class="ss">:ok</span>`},
		{"powershell", "if ($true) { echo 'hi' }", `<span class="k">if</span>`},
		{"sql", "SELECT id FROM users WHERE active = TRUE;", `<span class="k">SELECT</span>`},
		{"json", "{\"a\": 123, \"b\": true}", `<span class="nt">&#34;a&#34;</span>`},
		{"yaml", "server:\n  port: 8080\n  tls: true", `<span class="nt">port</span>`},
		{"nginx", "server { listen 80; return 200; }", `<span class="k">server</span>`},
		{"svelte", "<div class=\"a\">hi</div>", `<span class="nt">&lt;div</span>`},
		{"scss", ".box { color: #fff; }", `<span class="na">color</span>`},
		{"diff", "+added\n-removed\n@@ -1,1 +1,1 @@", `<span class="gi">+added</span>`},
		{"markdown", "# Title\n**bold**", `<span class="gh"># Title</span>`},
		{"haskell", "if true then 1 else 0", `<span class="k">if</span>`},
	}
	for _, s := range samples {
		got := HighlightCodeHTML(s.code, s.lang, false)
		if !strings.Contains(got, s.want) {
			t.Fatalf("lang %q missing %q in %s", s.lang, s.want, got)
		}
	}

	// Verify exact alias count and non-nil resolution for all expected aliases.
	expectedAliases := []string{
		"go", "golang",
		"ts", "typescript", "tsx", "js", "javascript", "jsx", "mjs", "cjs", "mts", "cts",
		"rs", "rust",
		"c", "h", "cpp", "c++", "cc", "hpp", "cxx", "java", "kotlin", "kt", "csharp", "cs", "c#", "objective-c", "objc", "swift", "dart", "scala", "zig", "prisma",
		"php", "php8", "phtml",
		"py", "python", "python3",
		"ruby", "rb",
		"lua",
		"graphql", "gql",
		"proto", "protobuf",
		"hcl", "tf", "terraform",
		"elixir", "ex", "exs", "r", "perl", "pl",
		"sh", "bash", "zsh", "fish", "ash", "shell", "console", "shellsession", "sh-session", "terminal", "dockerfile", "docker", "powershell", "ps1", "pwsh", "make", "makefile", "bat", "cmd",
		"sql", "sqlite", "postgres", "postgresql", "pgsql", "mysql",
		"json", "jsonc", "json5", "geojson",
		"yaml", "yml", "toml", "ini", "env", "dotenv", "properties", "systemd", "service", "editorconfig",
		"nginx", "conf", "caddy", "caddyfile", "apache", "apacheconf", "haproxy", "vhost",
		"html", "htm", "xml", "svg", "vue", "svelte",
		"css", "scss", "sass", "less",
		"diff", "patch",
		"md", "markdown",
		"haskell", "hs", "ada", "vhdl",
	}
	codeLexerOnce.Do(initCodeLexers)
	if len(codeLexers) != len(expectedAliases) {
		t.Fatalf("codeLexers len=%d, want %d", len(codeLexers), len(expectedAliases))
	}
	for _, a := range expectedAliases {
		if codeLexers[a] == nil {
			t.Fatalf("missing lexer alias %q", a)
		}
	}
	if genericCodeLexer == nil {
		t.Fatalf("genericCodeLexer is nil")
	}
}
