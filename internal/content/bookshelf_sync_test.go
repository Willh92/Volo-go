package content

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
)

func TestExtractAndRewriteBookHTML_StripsBuiltInTOCAndRewritesLinks(t *testing.T) {
	baseURL, err := url.Parse("https://docs.example.com/guide/part1/intro.html")
	if err != nil {
		t.Fatal(err)
	}

	bk := BookDetailView{
		Book: domain.Book{
			ID:        "bk_1",
			Slug:      "go-handbook",
			Title:     "Go Handbook",
			SourceURL: "https://docs.example.com/guide/",
			EmbedMode: "iframe",
		},
		FlatChapters: []domain.BookChapter{
			{ID: "ch_intro", BookID: "bk_1", Title: "Introduction", URL: "https://docs.example.com/guide/part1/intro.html"},
			{ID: "ch_next", BookID: "bk_1", Title: "Concurrency", URL: "https://docs.example.com/guide/part2/concurrency.html"},
			{ID: "ch_idx", BookID: "bk_1", Title: "Appendix", URL: "https://docs.example.com/guide/appendix/index.html"},
		},
	}

	gitbookDoc := `<!DOCTYPE html>
<html>
<body>
  <div class="book">
    <div class="book-summary">
      <nav role="navigation">
        <ul class="summary">
          <li><a href="intro.html">Built-in Redundant TOC Item</a></li>
          <li><a href="../part2/concurrency.html">Built-in Concurrency</a></li>
        </ul>
      </nav>
    </div>
    <div class="book-body">
      <div class="body-inner">
        <div class="book-header" role="navigation"><h1>Header Chrome</h1></div>
        <div class="page-wrapper">
          <section class="normal markdown-section">
            <h1>Introduction</h1>
            <p>Welcome to the Go Handbook prose body with enough text to pass extraction threshold.</p>
            <h2><a id="goroutines" class="plugin-anchor" href="#goroutines">#</a>Goroutines Overview</h2>
            <p>Jump to <a href="#goroutines">same-page goroutines</a> or read <a href="../part2/concurrency.md#channels">Concurrency Channels</a> or <a href="../appendix/">Appendix</a>.</p>
            <p>External reference: <a href="https://go.dev/doc/">Go Official Docs</a>.</p>
            <img src="../assets/arch.png" alt="Architecture">
          </section>
        </div>
      </div>
    </div>
    <a href="../part2/concurrency.html" class="navigation navigation-next">Next</a>
  </div>
</body>
</html>`

	htmlOut, ok := extractAndRewriteBookHTML(gitbookDoc, baseURL, bk, bk.FlatChapters[0], render.Default())
	if !ok {
		t.Fatal("expected extractAndRewriteBookHTML to succeed on GitBook HTML")
	}
	if strings.Contains(htmlOut, "Built-in Redundant TOC Item") || strings.Contains(htmlOut, "Header Chrome") {
		t.Fatalf("expected built-in GitBook TOC and header chrome to be stripped, got:\n%s", htmlOut)
	}
	if strings.Contains(htmlOut, "<h1>Introduction</h1>") {
		t.Fatalf("expected duplicate leading <h1> matching chapter title to be stripped, got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `<h2 id="goroutines">Goroutines Overview</h2>`) {
		t.Fatalf("expected permalink anchor ID lifted onto <h2> and '#' glyph stripped, got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `href="#goroutines"`) {
		t.Fatalf("expected in-chapter #goroutines anchor link preserved, got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `href="/books/go-handbook?ch=ch_next#channels"`) {
		t.Fatalf("expected relative .md/.html chapter link rewritten to /books/go-handbook?ch=ch_next#channels, got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `href="/books/go-handbook?ch=ch_idx"`) {
		t.Fatalf("expected directory index link ../appendix/ rewritten to ch_idx, got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `href="https://go.dev/doc/"`) || !strings.Contains(htmlOut, `target="_blank"`) || !strings.Contains(htmlOut, `data-no-pjax="1"`) {
		t.Fatalf("expected external link to carry target=_blank and data-no-pjax=1, got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `src="https://docs.example.com/guide/assets/arch.png"`) {
		t.Fatalf("expected relative image src resolved against chapter URL, got:\n%s", htmlOut)
	}

	// Empty SPA shell must return ok=false so book_reader.html falls back to <iframe>
	spaShell := `<!DOCTYPE html><html><body><div id="app"></div><script src="/app.js"></script></body></html>`
	if _, spaOK := extractAndRewriteBookHTML(spaShell, baseURL, bk, bk.FlatChapters[0], render.Default()); spaOK {
		t.Fatal("expected pure client-side empty SPA shell to fall back (ok=false)")
	}
}

func TestGitBook3SearchResultsWrapperAndNestedSidebarTOC(t *testing.T) {
	baseURL, err := url.Parse("https://books.example.com/go/")
	if err != nil {
		t.Fatal(err)
	}

	// Real-world GitBook 3 layout (`gitbook-plugin-search` / `search-pro`) wraps `.markdown-section`
	// inside `<div id="book-search-results"><div class="search-noresults">...</div><div class="search-results">...</div></div>`.
	gitbook3Doc := `<!DOCTYPE html>
<html lang="zh-hans">
<body>
  <div class="book with-summary">
    <div class="book-summary">
      <div id="book-search-input" role="search"><input type="text" placeholder="输入并搜索"></div>
      <nav role="navigation">
        <ul class="summary">
          <li class="chapter active" data-level="1.1" data-path="./">
            <a href="./">前言</a>
          </li>
          <li class="chapter" data-level="1.2" data-path="ebook/01.0.html">
            <a href="ebook/01.0.html">第一部分：学习 Go 语言</a>
            <ul class="articles">
              <li class="chapter" data-level="1.2.1" data-path="ebook/01.1.html">
                <a href="ebook/01.1.html">1.1 起源与发展</a>
              </li>
            </ul>
          </li>
          <li class="divider"></li>
          <li><a href="https://www.gitbook.com" target="blank" class="gitbook-link">本书使用 GitBook 发布</a></li>
        </ul>
      </nav>
    </div>
    <div class="book-body">
      <div class="body-inner">
        <div class="page-wrapper" tabindex="-1" role="main">
          <div class="page-inner">
            <div id="book-search-results">
              <div class="search-noresults">
                <section class="normal markdown-section">
                  <h1 id="go-语言入门指南">Go 语言入门指南</h1>
                  <p>《The Way to Go》中文译本，全面介绍 Go 语言核心特性与并发编程模型，点击<a href="ebook/01.1.html#origin">1.1 起源与发展</a>开始阅读。</p>
                </section>
              </div>
              <div class="search-results">
                <div class="has-results"><h1 class="search-results-title">search overlay chrome</h1></div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</body>
</html>`

	toc := parseHTMLSidebarTOC("bk_go", gitbook3Doc, baseURL)
	if len(toc) != 3 {
		t.Fatalf("expected 3 chapters from GitBook 3 sidebar (excluding gitbook-link/divider), got %d: %+v", len(toc), toc)
	}
	if toc[0].Depth != 0 || toc[1].Depth != 0 || toc[2].Depth != 1 || toc[2].ParentID != toc[1].ID {
		t.Fatalf("expected nested <ul class=\"articles\"> to produce Depth=1 child under chapter 2, got %+v", toc)
	}

	bk := BookDetailView{
		Book: domain.Book{
			ID:        "bk_go",
			Slug:      "the-way-to-go",
			Title:     "Go 语言入门指南",
			SourceURL: "https://books.example.com/go/",
			EmbedMode: "inline",
		},
		FlatChapters: toc,
	}

	htmlOut, ok := extractAndRewriteBookHTML(gitbook3Doc, baseURL, bk, toc[0], render.Default())
	if !ok {
		t.Fatal("expected GitBook 3 #book-search-results > .search-noresults > section.markdown-section to extract cleanly")
	}
	if strings.Contains(htmlOut, "本书使用 GitBook 发布") || strings.Contains(htmlOut, "search overlay chrome") || strings.Contains(htmlOut, "第一部分：学习 Go 语言") {
		t.Fatalf("expected built-in GitBook sidebar and .search-results overlay stripped, got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `href="/books/the-way-to-go?ch=`+toc[2].ID+`#origin"`) {
		t.Fatalf("expected relative chapter link ebook/01.1.html#origin rewritten to Volo reader chapter URL, got:\n%s", htmlOut)
	}
}

func TestExtractAndRewriteBookHTML_NormalizesCodeBlockLineNumbers(t *testing.T) {
	baseURL, err := url.Parse("https://books.example.com/chapter2/widget_intro.html")
	if err != nil {
		t.Fatal(err)
	}
	bk := BookDetailView{
		Book: domain.Book{
			ID:        "bk_ui",
			Slug:      "ui-handbook",
			Title:     "UI 组件开发手册",
			SourceURL: "https://books.example.com/",
			EmbedMode: "inline",
		},
		FlatChapters: []domain.BookChapter{
			{ID: "ch_2_2", BookID: "bk_ui", Title: "2.2 组件接口简介", URL: "https://books.example.com/chapter2/widget_intro.html"},
		},
	}

	doc := `<!DOCTYPE html>
<html><body>
<main class="page">
  <div class="theme-default-content">
    <h1 id="_2-2-组件接口简介"><a href="#_2-2-组件接口简介" class="header-anchor">#</a> 2.2 组件接口简介</h1>
    <h2 id="_2-2-2-组件接口"><a href="#_2-2-2-组件接口" class="header-anchor">#</a> 2.2.2 组件接口</h2>
    <p>在声明式 UI 框架中，核心组件接口定义如下：</p>
    <div class="language-dart line-numbers-mode"><pre class="language-dart"><code><span class="token metadata symbol">@immutable</span> <span class="token comment">// 不可变的</span>
<span class="token keyword">abstract</span> <span class="token keyword">class</span> <span class="token class-name">Widget</span> <span class="token keyword">extends</span> <span class="token class-name">DiagnosticableTree</span> <span class="token punctuation">{</span>
  <span class="token comment">/* multi-line
   * doc comment */</span>
  <span class="token keyword">const</span> <span class="token class-name">Widget</span><span class="token punctuation">(</span><span class="token punctuation">{</span> <span class="token keyword">this</span><span class="token punctuation">.</span>key <span class="token punctuation">}</span><span class="token punctuation">)</span><span class="token punctuation">;</span>
<span class="token punctuation">}</span>
</code></pre> <div class="line-numbers-wrapper"><span class="line-number">1</span><br><span class="line-number">2</span><br><span class="line-number">3</span><br><span class="line-number">4</span><br><span class="line-number">5</span><br><span class="line-number">6</span><br></div></div>

    <div class="language-ts vp-adaptive-theme line-numbers-mode">
      <button title="Copy Code" class="copy"></button>
      <span class="lang">ts</span>
      <pre class="shiki"><code><span class="line"><span class="token keyword">const</span> a = <span class="token number">1</span>;</span>
<span class="line highlighted"><span class="token keyword">const</span> b = <span class="token number">2</span>;</span></code></pre>
      <div class="line-numbers-wrapper" aria-hidden="true"><span class="line-number">1</span><br><span class="line-number">2</span><br></div>
    </div>

    <figure class="highlight dart">
      <table><tbody><tr>
        <td class="gutter"><pre><span class="line">1</span><br><span class="line">2</span><br></pre></td>
        <td class="code"><pre><span class="line"><span class="keyword">void</span> main() {</span><br><span class="line">  print(<span class="string">'ok'</span>);</span><br></pre></td>
      </tr></tbody></table>
    </figure>

    <pre class="line-numbers language-go"><code class="language-go">package main
func main() {}
<span aria-hidden="true" class="line-numbers-rows"><span></span><span></span></span></code></pre>
  </div>
</main>
</body></html>`

	htmlOut, ok := extractAndRewriteBookHTML(doc, baseURL, bk, bk.FlatChapters[0], render.Default())
	if !ok {
		t.Fatal("expected extractAndRewriteBookHTML to succeed")
	}

	// 1. External VuePress/VitePress/Prism/Hexo gutter wrappers and layout tables must be completely gone.
	for _, forbidden := range []string{
		"line-numbers-wrapper",
		"line-numbers-rows",
		`class="line-number"`,
		`class="gutter"`,
		"<table",
		`class="copy"`,
	} {
		if strings.Contains(htmlOut, forbidden) {
			t.Fatalf("expected %q to be stripped/normalized, got:\n%s", forbidden, htmlOut)
		}
	}

	// 2. VuePress Dart block must become a 6-line canonical Chroma figure with Prism tokens intact.
	if !strings.Contains(htmlOut, `<figure class="volo-codeblock" data-lang="dart"><pre class="chroma"><code>`) {
		t.Fatalf("expected canonical volo-codeblock figure with data-lang=dart, got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `<span class="ln">6</span>`) || strings.Contains(htmlOut, `<span class="ln">7</span>`) {
		t.Fatalf("expected exactly 6 line numbers on VuePress Dart block (trailing newline trimmed), got:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `<span class="token metadata symbol">@immutable</span>`) {
		t.Fatalf("expected Prism token classes preserved inside .cl, got:\n%s", htmlOut)
	}
	// Multi-line comment across lines 3 and 4 must close and reopen cleanly inside each .cl.
	if !strings.Contains(htmlOut, `<span class="ln">3</span><span class="cl">  <span class="token comment">/* multi-line</span>`) ||
		!strings.Contains(htmlOut, `<span class="ln">4</span><span class="cl"><span class="token comment">   * doc comment */</span>`) {
		t.Fatalf("expected multi-line comment span balanced across lines 3 and 4, got:\n%s", htmlOut)
	}

	// 3. VitePress/Shiki block must have 2 lines (not 3 or 4 from inter-line \n) and preserve highlighted line `.hl`.
	if !strings.Contains(htmlOut, `data-lang="ts"`) || !strings.Contains(htmlOut, `<span class="line hl"><span class="ln">2</span>`) {
		t.Fatalf("expected VitePress/Shiki block normalized with data-lang=ts and .line.hl on line 2, got:\n%s", htmlOut)
	}

	// 4. Plain Prism Go block without existing token spans must be syntax-highlighted by Volo's Go lexer with 2 line numbers.
	if !strings.Contains(htmlOut, `data-lang="go"`) || !strings.Contains(htmlOut, `<span class="k">package</span>`) {
		t.Fatalf("expected plain Go block highlighted via Volo lexer with data-lang=go, got:\n%s", htmlOut)
	}
}
