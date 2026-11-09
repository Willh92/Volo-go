package render

import (
	"fmt"
	"strings"
	"testing"
)

func TestRender_LaTeXMathProtectionAndCurrencyGuards(t *testing.T) {
	eng := NewEngine()

	t.Run("inline and display math survive Goldmark and Bluemonday intact", func(t *testing.T) {
		md := strings.Join([]string{
			`Inline subscript $x_1 + x_2$ and inequality $a < b & c$ and paren \(y_1^2 + y_2^2\).`,
			`中文行内公式$\alpha_1 + \beta_2$紧贴文字。`,
			"",
			"$$",
			`\begin{bmatrix}`,
			`a_{11} & a_{12} \\`,
			`a_{21} & a_{22}`,
			`\end{bmatrix}`,
			"$$",
			"",
			`\[`,
			`f(x) &= x_1 + x_2 \\`,
			`g(x) &= x < y`,
			`\]`,
		}, "\n")

		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if !out.NeedsMath {
			t.Fatalf("expected NeedsMath=true")
		}
		if strings.Contains(out.HTML, "<em>") {
			t.Fatalf("math underscores were corrupted into <em>: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `<span class="volo-math-inline">$x_1 + x_2$</span>`) {
			t.Fatalf("missing inline math span for x_1 + x_2: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `<span class="volo-math-inline">$a &lt; b &amp; c$</span>`) {
			t.Fatalf("expected HTML-escaped < and & inside inline math span: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `<span class="volo-math-inline">$y_1^2 + y_2^2$</span>`) {
			t.Fatalf("missing inline paren math span: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `<span class="volo-math-inline">$\alpha_1 + \beta_2$</span>`) {
			t.Fatalf("missing CJK adjacent inline math span: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `<div class="volo-math-block">$$`) {
			t.Fatalf("missing display math block wrapper: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `a_{11} &amp; a_{12} \\`) {
			t.Fatalf("matrix row break \\\\ or &amp; was corrupted in display math: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `f(x) &amp;= x_1 + x_2 \\`) || !strings.Contains(out.HTML, `g(x) &amp;= x &lt; y`) {
			t.Fatalf("\\[...\\] display math alignment or < escaped content corrupted: %s", out.HTML)
		}
		// Ensure WithHardWraps() did not inject <br> inside display math blocks.
		for _, part := range strings.Split(out.HTML, `<div class="volo-math-block">`) {
			if idx := strings.Index(part, "</div>"); idx >= 0 && strings.Contains(part[:idx], "<br") {
				t.Fatalf("unexpected <br> injected inside display math block: %s", part[:idx])
			}
		}
	})

	t.Run("currency, escaped dollars, inline code, and fenced code do not trigger math", func(t *testing.T) {
		md := strings.Join([]string{
			`Prices are $100 and $200 today, or $100/$200 range, plus escaped \$50 tag.`,
			"Inline code `$x_1 + x_2$` and ``$$y_1$$`` stay literal.",
			"```tex",
			"$$z_1 + z_2$$",
			"```",
		}, "\n")

		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if out.NeedsMath {
			t.Fatalf("expected NeedsMath=false when only currency/escaped/code dollars exist, got HTML=%s", out.HTML)
		}
		if strings.Contains(out.HTML, "volo-math-") {
			t.Fatalf("unexpected math wrapper in currency/code output: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, "$100 and $200") {
			t.Fatalf("currency text altered: %s", out.HTML)
		}
	})

	t.Run("mixed currency followed by real inline math on same line", func(t *testing.T) {
		md := `Budget is $100 and $200, while formula is $x_1 + x_2$.`
		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if !out.NeedsMath {
			t.Fatalf("expected NeedsMath=true")
		}
		if !strings.Contains(out.HTML, "$100 and $200") {
			t.Fatalf("currency prefix altered: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, `<span class="volo-math-inline">$x_1 + x_2$</span>`) {
			t.Fatalf("inline math after currency failed to extract: %s", out.HTML)
		}
	})
}

func TestRender_VideoSanitizerCustomDirectiveAndBookshelfLimit(t *testing.T) {
	eng := NewEngine()

	t.Run("video element and safe attributes allowed while unsafe URLs stripped", func(t *testing.T) {
		md := `<video src="/uploads/demo.mp4" poster="https://cdn.example.com/poster.jpg" controls preload="metadata" playsinline loop muted width="640" height="360" onclick="alert(1)"></video>` +
			`<video src="javascript:alert(1)" controls></video>`
		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if !strings.Contains(out.HTML, `<video`) || !strings.Contains(out.HTML, `src="/uploads/demo.mp4"`) || !strings.Contains(out.HTML, `poster="https://cdn.example.com/poster.jpg"`) {
			t.Fatalf("safe <video> attributes were stripped: %s", out.HTML)
		}
		if !strings.Contains(out.HTML, "controls") || !strings.Contains(out.HTML, "playsinline") || !strings.Contains(out.HTML, `width="640"`) {
			t.Fatalf("expected boolean/dimension attributes on <video>: %s", out.HTML)
		}
		if strings.Contains(out.HTML, "onclick") || strings.Contains(out.HTML, "javascript:") {
			t.Fatalf("unsafe attribute or scheme survived Bluemonday: %s", out.HTML)
		}
	})

	t.Run("CustomDirective hook renders and is sanitized by Bluemonday", func(t *testing.T) {
		md := ":::timeline[title=\" Milestones \"]\nStep 1\n:::\n"
		out := eng.Render(md, Options{
			Mode: RenderModeWeb,
			CustomDirective: func(name string, attrs map[string]string, body string) (string, bool) {
				if name != "timeline" {
					return "", false
				}
				return fmt.Sprintf(`<section><h3>%s</h3><p>%s</p><script>alert(1)</script></section>`, attrs["title"], strings.TrimSpace(body)), true
			},
		})
		if !strings.Contains(out.HTML, `<section><h3 id="milestones"> Milestones </h3><p>Step 1</p></section>`) {
			t.Fatalf("custom directive output missing: %s", out.HTML)
		}
		if strings.Contains(out.HTML, "<script") {
			t.Fatalf("custom directive XSS payload was not sanitized: %s", out.HTML)
		}
	})

	t.Run("bookshelf directive honors limit attribute", func(t *testing.T) {
		out := eng.Render(":::bookshelf[limit=\"2\"]\n:::\n", Options{
			Mode: RenderModeWeb,
			ListBooks: func() []BookCardInfo {
				return []BookCardInfo{
					{Slug: "b1", Title: "Book One", ChapterCount: 3},
					{Slug: "b2", Title: "Book Two", ChapterCount: 5},
					{Slug: "b3", Title: "Book Three", ChapterCount: 8},
				}
			},
		})
		if !strings.Contains(out.HTML, "Book One") || !strings.Contains(out.HTML, "Book Two") {
			t.Fatalf("expected first two books in bookshelf: %s", out.HTML)
		}
		if strings.Contains(out.HTML, "Book Three") {
			t.Fatalf("expected Book Three to be excluded by limit=2: %s", out.HTML)
		}
	})
}

func TestLexers_MultiLineLineNumbersAndUTF8RuneStep(t *testing.T) {
	code := "/* multi-line\n   comment 你好 🚀 */\nconst x = `line1\nline2`; // 注释\n"
	htmlOut := HighlightCodeHTML(code, "js", true)

	if strings.Contains(htmlOut, "\ufffd") {
		t.Fatalf("UTF-8 replacement char found in highlighted output: %s", htmlOut)
	}
	if !strings.Contains(htmlOut, "你好 🚀") || !strings.Contains(htmlOut, "注释") {
		t.Fatalf("multi-byte UTF-8 runes corrupted: %s", htmlOut)
	}
	// Both line 1 and line 2 of the /* ... */ block comment must carry <span class="cm"> inside their respective lines!
	if strings.Count(htmlOut, `<span class="cm">`) < 2 {
		t.Fatalf("expected multi-line comment span to reopen on line 2 when withLineNumbers=true: %s", htmlOut)
	}
	if strings.Count(htmlOut, `<span class="sb">`) < 2 {
		t.Fatalf("expected multi-line template literal span to reopen across newline when withLineNumbers=true: %s", htmlOut)
	}
	if strings.Count(htmlOut, `<span class="ln">`) != 4 {
		t.Fatalf("expected 4 numbered lines, got: %s", htmlOut)
	}
}

func TestRender_MobileTableScrollContainer(t *testing.T) {
	eng := NewEngine()
	tableMD := "| Col A | Col B |\n| --- | --- |\n| 1 | 2 |\n"

	commentHTML := eng.RenderComment(tableMD)
	if !strings.Contains(commentHTML, `<div class="volo-table-scroll"><table>`) {
		t.Fatalf("RenderComment did not preserve volo-table-scroll wrapper: %s", commentHTML)
	}

	basicHTML := eng.RenderBasic(tableMD)
	if !strings.Contains(basicHTML, `<div class="volo-table-scroll"><table>`) {
		t.Fatalf("RenderBasic missing volo-table-scroll wrapper: %s", basicHTML)
	}

	summaryHTML := eng.RenderSummary(tableMD + "\n:::lock\nsecret\n:::\n")
	if !strings.Contains(summaryHTML, `<div class="volo-table-scroll"><table>`) || strings.Contains(summaryHTML, "secret") {
		t.Fatalf("RenderSummary failed table scroll wrap or lock strip: %s", summaryHTML)
	}
}
