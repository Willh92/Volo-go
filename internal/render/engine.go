package render

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// RenderMode controls how locked blocks and interactive hooks are rendered.
type RenderMode string

const (
	RenderModeWeb  RenderMode = "web"
	RenderModeFeed RenderMode = "feed"
)

// TOCItem represents a heading in an article or page outline.
type TOCItem struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Level int    `json:"level"`
}

// BookCardInfo provides book metadata for `:::book` and `:::bookshelf` directives.
type BookCardInfo struct {
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	CoverURL     string `json:"cover_url"`
	ChapterCount int    `json:"chapter_count"`
}

// Options configures a single Markdown rendering pass.
type Options struct {
	Mode              RenderMode
	Locale            string
	ArticleID         string
	Unlocked          bool // True when visitor unlocked article/:::lock password
	IsReaderLoggedIn  bool // True when authenticated reader/editor/admin (for :::lock type="user")
	HasAudio          bool // Enables [mm:ss] / [hh:mm:ss] podcast seek buttons
	ShowLineNumbers   bool
	LockPromptTitle   string
	LockPromptAction  string
	FeedLockedNotice  string
	LookupBook        func(slug string) (BookCardInfo, bool)
	ListBooks         func() []BookCardInfo
	LookupMediaDims   func(src string) (width int, height int, ok bool)
}

// Output contains sanitized HTML plus extracted outline, plain text, first public image, and lazy-load hints.
type Output struct {
	HTML           string    `json:"html"`
	PlainText      string    `json:"plain_text"`
	FirstImageURL  string    `json:"first_image_url"`
	TOC            []TOCItem `json:"toc"`
	WordCount      int       `json:"word_count"`
	ReadingMinutes int       `json:"reading_minutes"`
	HasLockBlock   bool      `json:"has_lock_block"`
	NeedsMath      bool      `json:"needs_math"`
	NeedsMermaid   bool      `json:"needs_mermaid"`
}

// Engine is a thread-safe, zero-CGO Markdown rendering pipeline powered by goldmark + chroma/v2 + bluemonday.
type Engine struct {
	mdOnce    sync.Once
	md        goldmark.Markdown
	sanOnce   sync.Once
	sanitizer *bluemonday.Policy
}

var (
	defaultEngineOnce sync.Once
	defaultEngineInst *Engine
)

// Default returns a shared singleton Engine.
func Default() *Engine {
	defaultEngineOnce.Do(func() {
		defaultEngineInst = NewEngine()
	})
	return defaultEngineInst
}

// NewEngine constructs a configured Markdown rendering Engine with lazy parser and sanitizer initialization.
func NewEngine() *Engine {
	return &Engine{}
}

func (e *Engine) ensureMarkdown() goldmark.Markdown {
	e.mdOnce.Do(func() {
		e.md = goldmark.New(
			goldmark.WithExtensions(
				extension.GFM,
				extension.Footnote,
				extension.DefinitionList,
				extension.Typographer,
			),
			goldmark.WithParserOptions(
				parser.WithAutoHeadingID(),
			),
			goldmark.WithRendererOptions(
				gmhtml.WithUnsafe(), // Raw HTML is strictly filtered by bluemonday policy right after rendering
			),
		)
	})
	return e.md
}

func (e *Engine) ensureSanitizer() *bluemonday.Policy {
	e.sanOnce.Do(func() {
		e.sanitizer = buildSanitizerPolicy()
	})
	return e.sanitizer
}

func buildSanitizerPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("id", "class", "tabindex", "aria-label", "aria-hidden", "role", "title").Globally()
	p.AllowAttrs("data-lang", "data-lock-type", "data-article-id", "data-audio-seek", "data-book-slug", "data-github-profile").Globally()
	p.AllowElements("figure", "figcaption", "section", "aside", "details", "summary", "mark", "kbd", "abbr", "button", "audio", "source")
	p.AllowAttrs("type").OnElements("button", "input")
	p.AllowAttrs("checked", "disabled").OnElements("input")
	p.AllowAttrs("open").OnElements("details")
	p.AllowAttrs("controls", "preload", "src").OnElements("audio", "source")
	p.AllowAttrs("type").OnElements("source")
	p.AllowAttrs("width", "height", "loading", "decoding", "src", "alt", "title").OnElements("img")
	p.AllowAttrs("href", "title", "target", "rel").OnElements("a")
	p.RequireNoFollowOnLinks(false)
	p.AddTargetBlankToFullyQualifiedLinks(false)
	return p
}

// SanitizeHTML passes an HTML fragment directly through the engine's strict bluemonday XSS policy.
func (e *Engine) SanitizeHTML(rawHTML string) string {
	return e.ensureSanitizer().Sanitize(rawHTML)
}

// Render transforms Markdown into sanitized, zero-JS syntax-highlighted HTML while strictly
// enforcing `:::lock` fail-closed isolation before AST parsing.
func (e *Engine) Render(markdownSrc string, opts Options) Output {
	if opts.Mode == "" {
		opts.Mode = RenderModeWeb
	}
	prep := preprocessDirectives(markdownSrc, opts)

	var rawBuf bytes.Buffer
	_ = e.ensureMarkdown().Convert([]byte(prep.markdown), &rawBuf)
	rendered := rawBuf.String()

	// Replace placeholders (Chroma code blocks, Mermaid diagrams, Lock cards, Book cards) before sanitization.
	for token, fragment := range prep.replacements {
		rendered = strings.ReplaceAll(rendered, "<p>"+token+"</p>", fragment)
		rendered = strings.ReplaceAll(rendered, token, fragment)
	}

	// Enhance <img> tags with intrinsic dimensions & lazy decoding and capture FirstImageURL from unlocked content.
	var firstImg string
	rendered = enhanceImages(rendered, opts.LookupMediaDims, &firstImg)

	// Enhance GitHub showcase stats/tables and wrap prose tables for responsive overflow protection in web mode.
	if opts.Mode == RenderModeWeb {
		rendered = enhanceTablesAndGitHubShowcase(rendered)
	}

	// Convert [mm:ss] / [hh:mm:ss] timestamps outside code/links when podcast audio is attached.
	if opts.HasAudio && opts.Mode == RenderModeWeb {
		rendered = injectAudioTimestamps(rendered)
	}

	// Sanitize with bluemonday to neutralize any script/event-handler XSS vectors.
	safeHTML := e.ensureSanitizer().Sanitize(rendered)
	if opts.Mode == RenderModeWeb {
		safeHTML = assignReadableHeadingIDs(safeHTML)
	}

	toc := extractHeadingsTOC(safeHTML)
	plain := ExtractPlainTextFromHTML(safeHTML)
	wc, mins := ComputeReadingStats(plain)

	return Output{
		HTML:           safeHTML,
		PlainText:      plain,
		FirstImageURL:  firstImg,
		TOC:            toc,
		WordCount:      wc,
		ReadingMinutes: mins,
		HasLockBlock:   prep.hasLock,
		NeedsMath:      prep.needsMath,
		NeedsMermaid:   prep.needsMermaid,
	}
}

type preprocessResult struct {
	markdown     string
	replacements map[string]string
	hasLock      bool
	needsMath    bool
	needsMermaid bool
}

var attrPairRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_-]*)\s*=\s*"([^"]*)"`)

func preprocessDirectives(src string, opts Options) preprocessResult {
	normalized := strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	res := preprocessResult{
		replacements: make(map[string]string),
	}
	var outLines []string
	seq := 0
	nextToken := func(kind string) string {
		seq++
		return fmt.Sprintf("VOLOPLACEHOLDER%s%dEND", kind, seq)
	}

	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// 1. Fenced code block (` ```lang ` or `~~~lang`): highlight via Chroma or emit Mermaid container!
		if ch, fenceLen, ok := parseFenceStart(trimmed); ok {
			info := strings.TrimSpace(trimmed[fenceLen:])
			lang := strings.Fields(info)
			langName := ""
			if len(lang) > 0 {
				langName = strings.ToLower(lang[0])
			}
			i++
			var codeLines []string
			for i < len(lines) {
				closeTrim := strings.TrimSpace(lines[i])
				if cCh, cLen, cOk := parseFenceStart(closeTrim); cOk && cCh == ch && cLen >= fenceLen && strings.TrimSpace(closeTrim[cLen:]) == "" {
					i++
					break
				}
				codeLines = append(codeLines, lines[i])
				i++
			}
			codeBody := strings.Join(codeLines, "\n")
			tok := nextToken("CODE")
			if langName == "mermaid" {
				res.needsMermaid = true
				res.replacements[tok] = `<pre class="mermaid">` + html.EscapeString(codeBody) + `</pre>`
			} else {
				res.replacements[tok] = highlightCodeChroma(codeBody, langName, opts.ShowLineNumbers)
			}
			outLines = append(outLines, "", tok, "")
			continue
		}

		// 2. `:::lock [type="pwd|user"] [hint="..."]` — FAIL-CLOSED if unclosed at EOF!
		if isDirectivePrefix(trimmed, ":::lock") {
			res.hasLock = true
			attrs := parseDirectiveAttrs(trimmed[len(":::lock"):])
			lockType := strings.ToLower(strings.TrimSpace(attrs["type"]))
			if lockType == "" {
				lockType = "pwd"
			}
			hint := strings.TrimSpace(attrs["hint"])
			i++

			var innerLines []string
			inInnerFence := false
			var innerFenceCh byte
			innerFenceLen := 0

			for i < len(lines) {
				cur := lines[i]
				curTrim := strings.TrimSpace(cur)
				if fCh, fLen, fOk := parseFenceStart(curTrim); fOk {
					if !inInnerFence {
						inInnerFence = true
						innerFenceCh = fCh
						innerFenceLen = fLen
					} else if fCh == innerFenceCh && fLen >= innerFenceLen && strings.TrimSpace(curTrim[fLen:]) == "" {
						inInnerFence = false
					}
					innerLines = append(innerLines, cur)
					i++
					continue
				}
				if !inInnerFence && curTrim == ":::" {
					i++
					break
				}
				innerLines = append(innerLines, cur)
				i++
			}

			canView := opts.Unlocked || (lockType == "user" && opts.IsReaderLoggedIn)
			if canView && opts.Mode == RenderModeWeb {
				outLines = append(outLines, `<div class="volo-lock-block volo-lock-block--unlocked" data-lock-type="`+html.EscapeString(lockType)+`">`, "")
				innerPrep := preprocessDirectives(strings.Join(innerLines, "\n"), opts)
				for k, v := range innerPrep.replacements {
					res.replacements["INNER"+strconv.Itoa(seq)+k] = v
					innerPrep.markdown = strings.ReplaceAll(innerPrep.markdown, k, "INNER"+strconv.Itoa(seq)+k)
				}
				if innerPrep.needsMath {
					res.needsMath = true
				}
				if innerPrep.needsMermaid {
					res.needsMermaid = true
				}
				outLines = append(outLines, innerPrep.markdown, "", `</div>`)
			} else {
				// Locked! Inner lines are completely discarded before AST parsing — zero chance of leakage!
				tok := nextToken("LOCK")
				res.replacements[tok] = renderLockedPlaceholder(lockType, hint, opts)
				outLines = append(outLines, "", tok, "")
			}
			continue
		}

		// 3. `:::book id="..."` / `:::book slug="..."`
		if isDirectivePrefix(trimmed, ":::book") && !isDirectivePrefix(trimmed, ":::bookshelf") {
			attrs := parseDirectiveAttrs(trimmed[len(":::book"):])
			slug := attrs["slug"]
			if slug == "" {
				slug = attrs["id"]
			}
			i++
			// Consume optional closing `:::` if block-style was used
			if i < len(lines) && strings.TrimSpace(lines[i]) == ":::" {
				i++
			}
			tok := nextToken("BOOK")
			res.replacements[tok] = renderBookCard(slug, opts)
			outLines = append(outLines, "", tok, "")
			continue
		}

		// 4. `:::bookshelf`
		if isDirectivePrefix(trimmed, ":::bookshelf") {
			i++
			if i < len(lines) && strings.TrimSpace(lines[i]) == ":::" {
				i++
			}
			tok := nextToken("BOOKSHELF")
			res.replacements[tok] = renderBookshelfGrid(opts)
			outLines = append(outLines, "", tok, "")
			continue
		}

		// Detect math expressions outside fenced code blocks (`$$...$$`, `\[...\]`, `\(...\)`, or `$...$`).
		if !res.needsMath && containsMathSyntax(line) {
			res.needsMath = true
		}

		// Convert obsidian-style `[[slug]]` or `[[slug|Title]]` wiki links outside inline code.
		outLines = append(outLines, rewriteWikiLinksOutsideCode(line))
		i++
	}

	res.markdown = strings.Join(outLines, "\n")
	return res
}

func parseFenceStart(trimmed string) (byte, int, bool) {
	if len(trimmed) < 3 {
		return 0, 0, false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	return ch, n, true
}

func isDirectivePrefix(trimmed string, directive string) bool {
	if !strings.HasPrefix(trimmed, directive) {
		return false
	}
	rest := trimmed[len(directive):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '['
}

func parseDirectiveAttrs(raw string) map[string]string {
	out := make(map[string]string)
	for _, m := range attrPairRe.FindAllStringSubmatch(raw, -1) {
		out[strings.ToLower(m[1])] = m[2]
	}
	return out
}

func renderLockedPlaceholder(lockType string, hint string, opts Options) string {
	if opts.Mode == RenderModeFeed {
		notice := opts.FeedLockedNotice
		if notice == "" {
			if strings.HasPrefix(strings.ToLower(opts.Locale), "en") {
				notice = "Protected content — please visit the original post to unlock."
			} else {
				notice = "受保护内容，请前往原文解锁阅读。"
			}
		}
		return `<blockquote><p>` + html.EscapeString(notice) + `</p></blockquote>`
	}

	title := opts.LockPromptTitle
	if title == "" {
		if lockType == "user" {
			if strings.HasPrefix(strings.ToLower(opts.Locale), "en") {
				title = "Reader sign-in required to view this section"
			} else {
				title = "本段内容仅对已登录读者开放"
			}
		} else {
			if strings.HasPrefix(strings.ToLower(opts.Locale), "en") {
				title = "This section is password-protected"
			} else {
				title = "本段内容受口令保护"
			}
		}
	}
	action := opts.LockPromptAction
	if action == "" {
		if strings.HasPrefix(strings.ToLower(opts.Locale), "en") {
			action = "Unlock"
		} else {
			action = "解锁阅读"
		}
	}

	var b strings.Builder
	b.WriteString(`<aside class="volo-lock-card" data-lock-type="` + html.EscapeString(lockType) + `" data-article-id="` + html.EscapeString(opts.ArticleID) + `">`)
	b.WriteString(`<p class="volo-lock-title">` + html.EscapeString(title) + `</p>`)
	if hint != "" {
		b.WriteString(`<p class="volo-lock-hint">` + html.EscapeString(hint) + `</p>`)
	}
	if lockType == "user" {
		b.WriteString(`<a class="volo-btn volo-btn--sm" href="/reader/login">` + html.EscapeString(action) + `</a>`)
	}
	b.WriteString(`</aside>`)
	return b.String()
}

func formatBookChapterBadge(count int, locale string) string {
	if count <= 0 {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "en") {
		if count == 1 {
			return "1 chapter"
		}
		return fmt.Sprintf("%d chapters", count)
	}
	return fmt.Sprintf("%d 章", count)
}

func renderBookCard(slug string, opts Options) string {
	cleanSlug := strings.TrimSpace(slug)
	if cleanSlug == "" {
		return ""
	}
	info := BookCardInfo{Slug: cleanSlug, Title: cleanSlug}
	if opts.LookupBook != nil {
		if found, ok := opts.LookupBook(cleanSlug); ok {
			info = found
		}
	}
	var b strings.Builder
	b.WriteString(`<section class="volo-embed-book" data-book-slug="` + html.EscapeString(info.Slug) + `">`)
	if cover := strings.TrimSpace(info.CoverURL); cover != "" && isSafeImageSrc(cover) {
		b.WriteString(`<img class="volo-embed-book__cover" src="` + html.EscapeString(cover) + `" alt="` + html.EscapeString(info.Title) + `" loading="lazy" decoding="async">`)
	}
	b.WriteString(`<div class="volo-embed-book__body">`)
	b.WriteString(`<a href="/books/` + url.PathEscape(info.Slug) + `"><strong>` + html.EscapeString(info.Title) + `</strong></a>`)
	if info.ChapterCount > 0 {
		b.WriteString(` <span class="volo-embed-book__chapters">` + html.EscapeString(formatBookChapterBadge(info.ChapterCount, opts.Locale)) + `</span>`)
	}
	if info.Description != "" {
		b.WriteString(`<p>` + html.EscapeString(info.Description) + `</p>`)
	}
	b.WriteString(`</div></section>`)
	return b.String()
}

func renderBookshelfGrid(opts Options) string {
	if opts.ListBooks == nil {
		return `<section class="volo-embed-bookshelf"></section>`
	}
	books := opts.ListBooks()
	var b strings.Builder
	b.WriteString(`<section class="volo-embed-bookshelf">`)
	for _, bk := range books {
		b.WriteString(`<article class="volo-book-tile" data-book-slug="` + html.EscapeString(bk.Slug) + `">`)
		if cover := strings.TrimSpace(bk.CoverURL); cover != "" && isSafeImageSrc(cover) {
			b.WriteString(`<img class="volo-embed-book__cover" src="` + html.EscapeString(cover) + `" alt="` + html.EscapeString(bk.Title) + `" loading="lazy" decoding="async">`)
		}
		b.WriteString(`<div class="volo-embed-book__body">`)
		b.WriteString(`<a href="/books/` + url.PathEscape(bk.Slug) + `"><strong>` + html.EscapeString(bk.Title) + `</strong></a>`)
		if bk.ChapterCount > 0 {
			b.WriteString(` <span class="volo-embed-book__chapters">` + html.EscapeString(formatBookChapterBadge(bk.ChapterCount, opts.Locale)) + `</span>`)
		}
		if bk.Description != "" {
			b.WriteString(`<p>` + html.EscapeString(bk.Description) + `</p>`)
		}
		b.WriteString(`</div></article>`)
	}
	b.WriteString(`</section>`)
	return b.String()
}

var (
	imgTagRe   = regexp.MustCompile(`<img\b[^>]*>`)
	imgSrcRe   = regexp.MustCompile(`\bsrc="([^"]+)"`)
	imgWidthRe = regexp.MustCompile(`\bwidth="[^"]*"`)
)

func enhanceImages(htmlSrc string, lookupDims func(string) (int, int, bool), firstImg *string) string {
	return imgTagRe.ReplaceAllStringFunc(htmlSrc, func(tag string) string {
		m := imgSrcRe.FindStringSubmatch(tag)
		if len(m) < 2 {
			return tag
		}
		src := html.UnescapeString(m[1])
		if firstImg != nil && *firstImg == "" && isSafeImageSrc(src) {
			*firstImg = src
		}
		var attrs strings.Builder
		if !strings.Contains(tag, "loading=") {
			attrs.WriteString(` loading="lazy"`)
		}
		if !strings.Contains(tag, "decoding=") {
			attrs.WriteString(` decoding="async"`)
		}
		if lookupDims != nil && !imgWidthRe.MatchString(tag) {
			if w, h, ok := lookupDims(src); ok && w > 0 && h > 0 {
				fmt.Fprintf(&attrs, ` width="%d" height="%d"`, w, h)
			}
		}
		if attrs.Len() == 0 {
			return tag
		}
		if strings.HasSuffix(tag, "/>") {
			return strings.TrimSuffix(tag, "/>") + attrs.String() + " />"
		}
		return strings.TrimSuffix(tag, ">") + attrs.String() + ">"
	})
}

func isSafeImageSrc(src string) bool {
	s := strings.TrimSpace(src)
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

var timestampTokenRe = regexp.MustCompile(`\[(\d{1,2}):([0-5]\d)(?::([0-5]\d))?\]`)

func injectAudioTimestamps(htmlSrc string) string {
	// Walk HTML tags so we NEVER rewrite timestamps inside <code>, <pre>, <a>, or HTML attributes!
	var out strings.Builder
	out.Grow(len(htmlSrc) + 64)
	skipDepth := 0
	i := 0
	for i < len(htmlSrc) {
		if htmlSrc[i] == '<' {
			end := strings.IndexByte(htmlSrc[i:], '>')
			if end < 0 {
				out.WriteString(htmlSrc[i:])
				break
			}
			tag := htmlSrc[i : i+end+1]
			lowerTag := strings.ToLower(tag)
			if strings.HasPrefix(lowerTag, "<code") || strings.HasPrefix(lowerTag, "<pre") || strings.HasPrefix(lowerTag, "<a ") || lowerTag == "<a>" || strings.HasPrefix(lowerTag, "<button") {
				skipDepth++
			} else if strings.HasPrefix(lowerTag, "</code") || strings.HasPrefix(lowerTag, "</pre") || strings.HasPrefix(lowerTag, "</a") || strings.HasPrefix(lowerTag, "</button") {
				if skipDepth > 0 {
					skipDepth--
				}
			}
			out.WriteString(tag)
			i += end + 1
			continue
		}
		nextTag := strings.IndexByte(htmlSrc[i:], '<')
		var textSeg string
		if nextTag < 0 {
			textSeg = htmlSrc[i:]
			i = len(htmlSrc)
		} else {
			textSeg = htmlSrc[i : i+nextTag]
			i += nextTag
		}
		if skipDepth > 0 || !strings.Contains(textSeg, "[") {
			out.WriteString(textSeg)
			continue
		}
		rewritten := timestampTokenRe.ReplaceAllStringFunc(textSeg, func(match string) string {
			sub := timestampTokenRe.FindStringSubmatch(match)
			if len(sub) < 3 {
				return match
			}
			var sec int
			if sub[3] != "" {
				h, _ := strconv.Atoi(sub[1])
				m, _ := strconv.Atoi(sub[2])
				s, _ := strconv.Atoi(sub[3])
				sec = h*3600 + m*60 + s
			} else {
				m, _ := strconv.Atoi(sub[1])
				s, _ := strconv.Atoi(sub[2])
				sec = m*60 + s
			}
			return fmt.Sprintf(`<button type="button" class="volo-ts-seek" data-audio-seek="%d">%s</button>`, sec, html.EscapeString(match))
		})
		out.WriteString(rewritten)
	}
	return out.String()
}

var headingTagRe = regexp.MustCompile(`(?is)<h([1-4])\b[^>]*\bid="([^"]+)"[^>]*>(.*?)</h[1-4]>`)
var stripAllTagsRe = regexp.MustCompile(`<[^>]+>`)

func extractHeadingsTOC(safeHTML string) []TOCItem {
	matches := headingTagRe.FindAllStringSubmatch(safeHTML, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]TOCItem, 0, len(matches))
	for _, m := range matches {
		lvl, _ := strconv.Atoi(m[1])
		txt := strings.TrimSpace(html.UnescapeString(stripAllTagsRe.ReplaceAllString(m[3], "")))
		if txt != "" {
			out = append(out, TOCItem{
				ID:    m[2],
				Text:  txt,
				Level: lvl,
			})
		}
	}
	return out
}

// ExtractPlainTextFromHTML strips all HTML tags and collapses whitespace for safe summaries & search indexing.
func ExtractPlainTextFromHTML(safeHTML string) string {
	noTags := stripAllTagsRe.ReplaceAllString(safeHTML, " ")
	unescaped := html.UnescapeString(noTags)
	return strings.Join(strings.Fields(unescaped), " ")
}

// ComputeReadingStats counts CJK characters + non-CJK words and estimates reading minutes (~300 units/min, min 1).
func ComputeReadingStats(plainText string) (wordCount int, readingMinutes int) {
	cjk := 0
	inWord := false
	words := 0
	for _, r := range plainText {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
			inWord = false
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if !inWord {
				words++
				inWord = true
			}
		} else {
			inWord = false
		}
	}
	total := cjk + words
	if total == 0 {
		return 0, 1
	}
	mins := (total + 299) / 300
	if mins < 1 {
		mins = 1
	}
	return total, mins
}

func containsMathSyntax(line string) bool {
	// Strip inline backtick spans first so `$HOME` in code spans never triggers KaTeX!
	clean := stripInlineCodeSpans(line)
	if strings.Contains(clean, "$$") || strings.Contains(clean, `\(`) || strings.Contains(clean, `\[`) {
		return true
	}
	first := strings.IndexByte(clean, '$')
	if first < 0 {
		return false
	}
	if first > 0 && clean[first-1] == '\\' {
		return false
	}
	second := strings.IndexByte(clean[first+1:], '$')
	return second > 0
}

func stripInlineCodeSpans(line string) string {
	var b strings.Builder
	inCode := false
	for i := 0; i < len(line); i++ {
		if line[i] == '`' {
			inCode = !inCode
			continue
		}
		if !inCode {
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

var wikiLinkRe = regexp.MustCompile(`\[\[([a-zA-Z0-9._-]+)(?:\|([^\]]+))?\]\]`)

func rewriteWikiLinksOutsideCode(line string) string {
	if !strings.Contains(line, "[[") {
		return line
	}
	// Preserve inline code spans untouched.
	parts := strings.Split(line, "`")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = wikiLinkRe.ReplaceAllStringFunc(parts[i], func(m string) string {
			sub := wikiLinkRe.FindStringSubmatch(m)
			if len(sub) < 2 {
				return m
			}
			slug := sub[1]
			label := slug
			if len(sub) >= 3 && strings.TrimSpace(sub[2]) != "" {
				label = strings.TrimSpace(sub[2])
			}
			return fmt.Sprintf("[%s](/posts/%s)", label, url.PathEscape(slug))
		})
	}
	return strings.Join(parts, "`")
}

var internalPostLinkRe = regexp.MustCompile(`(?:/posts/|/articles/\d{4}/\d{2}/\d{2}/)([a-zA-Z0-9._-]+?)(?:\.html)?(?:[)"'\s#?]|$)`)

// ExtractOutboundPostSlugs extracts referenced internal post slugs (`[[slug]]` and `/posts/{slug}`)
// ONLY after stripping fenced code blocks and ALL `:::lock ... :::` blocks (preventing backlink leaks!).
func ExtractOutboundPostSlugs(rawMarkdown string) []string {
	stripped := StripAllCodeAndLockBlocks(rawMarkdown)
	seen := make(map[string]bool)
	var slugs []string
	add := func(s string) {
		clean := strings.TrimSpace(strings.TrimSuffix(s, ".html"))
		if clean != "" && !seen[clean] {
			seen[clean] = true
			slugs = append(slugs, clean)
		}
	}
	for _, m := range wikiLinkRe.FindAllStringSubmatch(stripped, -1) {
		if len(m) >= 2 {
			add(m[1])
		}
	}
	for _, m := range internalPostLinkRe.FindAllStringSubmatch(stripped, -1) {
		if len(m) >= 2 {
			add(m[1])
		}
	}
	return slugs
}

// StripAllLockBlocks removes all `:::lock ... :::` blocks outside fenced code blocks,
// failing closed to EOF if an opening `:::lock` is unclosed.
func StripAllLockBlocks(src string) string {
	if !strings.Contains(src, ":::lock") {
		return src
	}
	normalized := strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	out := make([]string, 0, len(lines))

	inFence := false
	var fenceChar byte
	fenceLen := 0
	inLock := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if ch, n, ok := parseFenceStart(trimmed); ok {
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = n
			} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				inFence = false
			}
			if !inLock {
				out = append(out, line)
			}
			continue
		}
		if !inFence {
			if !inLock && isDirectivePrefix(trimmed, ":::lock") {
				inLock = true
				continue
			}
			if inLock && trimmed == ":::" {
				inLock = false
				continue
			}
		}
		if !inLock {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// StripAllCodeAndLockBlocks removes both fenced code blocks and `:::lock` blocks (including unclosed locks).
func StripAllCodeAndLockBlocks(src string) string {
	noLock := StripAllLockBlocks(src)
	lines := strings.Split(strings.ReplaceAll(noLock, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if ch, n, ok := parseFenceStart(trimmed); ok {
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = n
			} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				inFence = false
			}
			continue
		}
		if !inFence {
			out = append(out, stripInlineCodeSpans(line))
		}
	}
	return strings.Join(out, "\n")
}

// ExtractFirstLockPassword scans Markdown outside fenced code blocks for `:::lock ... pwd="..."` (or `password="..."`)
// so public articles with inline password-locked blocks can be unlocked via /api/v1/articles/{id}/unlock.
func ExtractFirstLockPassword(src string) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if ch, n, ok := parseFenceStart(trimmed); ok {
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = n
			} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				inFence = false
			}
			continue
		}
		if !inFence && isDirectivePrefix(trimmed, ":::lock") {
			attrs := parseDirectiveAttrs(trimmed[len(":::lock"):])
			if p := strings.TrimSpace(attrs["pwd"]); p != "" {
				return p
			}
			if p := strings.TrimSpace(attrs["password"]); p != "" {
				return p
			}
		}
	}
	return ""
}

// ContainsLockDirective reports whether src contains at least one `:::lock` directive outside fenced code blocks.
func ContainsLockDirective(src string) bool {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if ch, n, ok := parseFenceStart(trimmed); ok {
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = n
			} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				inFence = false
			}
			continue
		}
		if !inFence && isDirectivePrefix(trimmed, ":::lock") {
			return true
		}
	}
	return false
}
