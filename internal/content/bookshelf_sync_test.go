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
