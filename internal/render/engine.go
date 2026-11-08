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
	SourceURL    string `json:"source_url,omitempty"`
	ChapterCount int    `json:"chapter_count"`
}

// Options configures a single Markdown rendering pass.
type Options struct {
	Mode                RenderMode
	Locale              string
	ArticleID           string
	Unlocked            bool            // True when visitor unlocked whole-article/un-attributed :::lock password
	AuthorOrShareBypass bool            // True when staff author/admin or valid ShareToken bypasses all locks
	UnlockedPasswords   map[string]bool // Per-password unlock state for inline :::lock pwd="..." blocks
	IsReaderLoggedIn    bool            // True when authenticated reader/editor/admin (for :::lock type="user")
	HasAudio            bool            // Enables [mm:ss] / [hh:mm:ss] podcast seek buttons
	ShowLineNumbers     bool
	LockPromptTitle     string
	LockPromptAction    string
	FeedLockedNotice    string
	LookupBook          func(slug string) (BookCardInfo, bool)
	ListBooks           func() []BookCardInfo
	LookupMediaDims     func(src string) (width int, height int, ok bool)
}

// Output contains sanitized HTML plus extracted outline, plain text, first public image, and lazy-load hints.
type Output struct {
	HTML              string    `json:"html"`
	PlainText         string    `json:"plain_text"`
	FirstImageURL     string    `json:"first_image_url"`
	TOC               []TOCItem `json:"toc"`
	WordCount         int       `json:"word_count"`
	ReadingMinutes    int       `json:"reading_minutes"`
	HasLockBlock      bool      `json:"has_lock_block"`
	HasUnresolvedLock bool      `json:"has_unresolved_lock"`
	NeedsMath         bool      `json:"needs_math"`
	NeedsMermaid      bool      `json:"needs_mermaid"`
}

// Engine is a thread-safe, zero-CGO Markdown rendering pipeline powered by goldmark + chroma/v2 + bluemonday.
type Engine struct {
	mdOnce         sync.Once
	md             goldmark.Markdown
	sanOnce        sync.Once
	sanitizer      *bluemonday.Policy
	commentMDOnce  sync.Once
	commentMD      goldmark.Markdown
	commentSanOnce sync.Once
	commentSan     *bluemonday.Policy
}

var (
	defaultEngineOnce   sync.Once
	defaultEngineInst   *Engine
	inputCheckboxTypeRe = regexp.MustCompile(`^checkbox$`)
	safeMediaSrcAttrRe  = regexp.MustCompile(`^(?:https?://[^\s"'<>]+|/[^/\s"'<>\\][^\s"'<>]*)$`)
	chromaCommentClsRe  = regexp.MustCompile(`^(chroma|line|cl|ln|lnt|lntable|lntd|hl|[a-z]{1,3})$`)
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

func (e *Engine) ensureCommentMarkdown() goldmark.Markdown {
	e.commentMDOnce.Do(func() {
		e.commentMD = goldmark.New(
			goldmark.WithExtensions(
				extension.Table,
				extension.Strikethrough,
				extension.Linkify,
			),
		)
	})
	return e.commentMD
}

func (e *Engine) ensureSanitizer() *bluemonday.Policy {
	e.sanOnce.Do(func() {
		e.sanitizer = buildSanitizerPolicy()
	})
	return e.sanitizer
}

func (e *Engine) ensureCommentSanitizer() *bluemonday.Policy {
	e.commentSanOnce.Do(func() {
		e.commentSan = buildCommentSanitizerPolicy()
	})
	return e.commentSan
}

func buildSanitizerPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("id", "class", "tabindex", "aria-label", "aria-hidden", "role", "title").Globally()
	p.AllowAttrs("data-lang", "data-lock-type", "data-article-id", "data-audio-seek", "data-book-slug", "data-github-profile", "data-callout").Globally()
	p.AllowElements("figure", "figcaption", "section", "aside", "details", "summary", "mark", "kbd", "abbr", "button", "audio", "source", "input")
	p.AllowAttrs("type").OnElements("button", "source")
	p.AllowAttrs("type").Matching(inputCheckboxTypeRe).OnElements("input")
	p.AllowAttrs("checked", "disabled").OnElements("input")
	p.AllowAttrs("open").OnElements("details")
	p.AllowAttrs("controls", "preload").OnElements("audio", "source")
	p.AllowAttrs("src").Matching(safeMediaSrcAttrRe).OnElements("audio", "source")
	p.AllowAttrs("width", "height", "loading", "decoding", "src", "alt", "title").OnElements("img")
	p.AllowAttrs("href", "title", "target", "rel", "data-no-pjax").OnElements("a")
	p.RequireNoFollowOnLinks(false)
	p.AddTargetBlankToFullyQualifiedLinks(false)
	return p
}

var (
	commentListStartRe  = regexp.MustCompile(`^[0-9]+$`)
	commentTableAlignRe = regexp.MustCompile(`^(?i)(left|right|center|justify)$`)
)

func buildCommentSanitizerPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowStandardURLs()
	p.AllowElements(
		"p", "br", "hr", "blockquote",
		"ul", "ol", "li", "dl", "dt", "dd",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"strong", "b", "em", "i", "del", "s", "strike", "ins", "sub", "sup", "kbd", "q", "cite", "abbr",
		"pre", "code", "span",
		"table", "thead", "tbody", "tfoot", "tr", "th", "td",
	)
	p.AllowAttrs("title").OnElements("abbr")
	p.AllowAttrs("start").Matching(commentListStartRe).OnElements("ol")
	p.AllowAttrs("align").Matching(commentTableAlignRe).OnElements("th", "td")
	p.AllowAttrs("href", "title").OnElements("a")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.AllowImages()
	p.AllowAttrs("class").Matching(chromaCommentClsRe).OnElements("pre", "code", "span", "table", "tbody", "tr", "td")
	return p
}

// SanitizeHTML passes an HTML fragment directly through the engine's strict bluemonday XSS policy.
func (e *Engine) SanitizeHTML(rawHTML string) string {
	return e.ensureSanitizer().Sanitize(rawHTML)
}

// RenderComment renders untrusted visitor/reader comment Markdown into strictly sanitized HTML
// WITHOUT raw HTML passthrough, WITHOUT footnotes (preventing #fn:1 collisions), and WITHOUT
// id, input, button, audio, source, tabindex, or data-* attributes.
func (e *Engine) RenderComment(markdown string) string {
	if strings.TrimSpace(markdown) == "" {
		return ""
	}
	mdClean, codeSlots := preprocessCommentCodeBlocks(markdown)
	var rawBuf bytes.Buffer
	_ = e.ensureCommentMarkdown().Convert([]byte(mdClean), &rawBuf)
	rendered := rawBuf.String()
	for token, fragment := range codeSlots {
		rendered = strings.ReplaceAll(rendered, "<p>"+token+"</p>", fragment)
		rendered = strings.ReplaceAll(rendered, token, fragment)
	}
	return e.ensureCommentSanitizer().Sanitize(rendered)
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
		slot := `<div data-volo-slot="` + token + `"></div>`
		rendered = strings.ReplaceAll(rendered, "<p>"+slot+"</p>", fragment)
		rendered = strings.ReplaceAll(rendered, slot, fragment)
		rendered = strings.ReplaceAll(rendered, "<p>"+token+"</p>", fragment)
		rendered = strings.ReplaceAll(rendered, token, fragment)
	}

	// Transform GitHub-flavored Alerts (> [!NOTE], > [!TIP], > [!IMPORTANT], > [!WARNING], > [!CAUTION]) into .volo-callout blocks.
	rendered = transformGitHubAlerts(rendered, opts.Locale)

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
		HTML:              safeHTML,
		PlainText:         plain,
		FirstImageURL:     firstImg,
		TOC:               toc,
		WordCount:         wc,
		ReadingMinutes:    mins,
		HasLockBlock:      prep.hasLock,
		HasUnresolvedLock: prep.hasUnresolvedLock,
		NeedsMath:         prep.needsMath,
		NeedsMermaid:      prep.needsMermaid,
	}
}

// RenderTrustedMarkdownHTML converts Admin-owned Markdown and embedded raw HTML/script blocks
// (such as Site.NoticeBoard, restricted strictly to RoleAdmin) via Goldmark without stripping
// custom tags, inline styles, iframes, or ad/widget scripts.
func (e *Engine) RenderTrustedMarkdownHTML(markdownSrc string) string {
	clean := strings.ReplaceAll(markdownSrc, "\x00", "")
	clean = strings.ReplaceAll(clean, "\r\n", "\n")
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return ""
	}
	var mdBuf bytes.Buffer
	if err := e.ensureMarkdown().Convert([]byte(clean), &mdBuf); err != nil {
		return clean
	}
	return strings.TrimSpace(mdBuf.String())
}

type preprocessResult struct {
	markdown          string
	replacements      map[string]string
	hasLock           bool
	hasUnresolvedLock bool
	needsMath         bool
	needsMermaid      bool
}

var attrPairRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'\]:]+))`)

func preprocessCommentCodeBlocks(src string) (string, map[string]string) {
	normalized := strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	slots := make(map[string]string)
	outLines := make([]string, 0, len(lines))
	seq := 0

	for i := 0; i < len(lines); {
		line := lines[i]
		if prefix, rest, ok := parseBlockLinePrefix(line); ok {
			if ch, fenceLen, fOk := parseFenceStart(rest); fOk {
				info := strings.TrimSpace(rest[fenceLen:])
				lang := strings.Fields(info)
				langName := ""
				if len(lang) > 0 {
					langName = strings.ToLower(lang[0])
				}
				i++
				var codeLines []string
				for i < len(lines) {
					if _, closeRest, cLineOk := parseBlockLinePrefix(lines[i]); cLineOk {
						if cCh, cLen, cOk := parseFenceStart(closeRest); cOk && cCh == ch && cLen >= fenceLen && strings.TrimSpace(closeRest[cLen:]) == "" {
							i++
							break
						}
					}
					codeLines = append(codeLines, stripBlockPrefix(lines[i], prefix))
					i++
				}
				seq++
				tok := fmt.Sprintf("VOLOCMTCODE%dX%dEND", len(src), seq)
				slots[tok] = highlightCommentCodeChroma(strings.Join(codeLines, "\n"), langName)
				if prefix == "" {
					outLines = append(outLines, "", tok, "")
				} else {
					outLines = append(outLines, prefix+tok)
				}
				continue
			}
		}
		outLines = append(outLines, line)
		i++
	}
	return strings.Join(outLines, "\n"), slots
}

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
	emitSlot := func(prefix, tok string) {
		slot := `<div data-volo-slot="` + tok + `"></div>`
		if prefix == "" {
			outLines = append(outLines, "", slot, "")
		} else {
			outLines = append(outLines, prefix+slot)
		}
	}

	i := 0
	for i < len(lines) {
		line := lines[i]
		prefix, rest, lineOk := parseBlockLinePrefix(line)

		if lineOk {
			// 1. Fenced code block (` ```lang ` or `~~~lang`): highlight via Chroma or emit Mermaid container!
			if ch, fenceLen, ok := parseFenceStart(rest); ok {
				info := strings.TrimSpace(rest[fenceLen:])
				lang := strings.Fields(info)
				langName := ""
				if len(lang) > 0 {
					langName = strings.ToLower(lang[0])
				}
				i++
				var codeLines []string
				for i < len(lines) {
					if _, closeRest, cLineOk := parseBlockLinePrefix(lines[i]); cLineOk {
						if cCh, cLen, cOk := parseFenceStart(closeRest); cOk && cCh == ch && cLen >= fenceLen && strings.TrimSpace(closeRest[cLen:]) == "" {
							i++
							break
						}
					}
					codeLines = append(codeLines, stripBlockPrefix(lines[i], prefix))
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
				emitSlot(prefix, tok)
				continue
			}

			// 2. `:::lock [type="pwd|user"] [pwd="..."] [hint="..."]` — FAIL-CLOSED if unclosed at EOF!
			if isDirectivePrefix(rest, ":::lock") {
				res.hasLock = true
				attrs := parseDirectiveAttrs(rest[len(":::lock"):])
				lockType := strings.ToLower(strings.TrimSpace(attrs["type"]))
				if lockType == "" {
					lockType = "pwd"
				}
				blockPwd := strings.TrimSpace(attrs["pwd"])
				if blockPwd == "" {
					blockPwd = strings.TrimSpace(attrs["password"])
				}
				hint := strings.TrimSpace(attrs["hint"])
				i++

				var innerLines []string
				inInnerFence := false
				var innerFenceCh byte
				innerFenceLen := 0
				lockNestDepth := 0

				for i < len(lines) {
					cur := lines[i]
					if _, curRest, curOk := parseBlockLinePrefix(cur); curOk {
						if fCh, fLen, fOk := parseFenceStart(curRest); fOk {
							if !inInnerFence {
								inInnerFence = true
								innerFenceCh = fCh
								innerFenceLen = fLen
							} else if fCh == innerFenceCh && fLen >= innerFenceLen && strings.TrimSpace(curRest[fLen:]) == "" {
								inInnerFence = false
							}
							innerLines = append(innerLines, stripBlockPrefix(cur, prefix))
							i++
							continue
						}
						if !inInnerFence {
							if curRest == ":::" {
								if lockNestDepth > 0 {
									lockNestDepth--
									innerLines = append(innerLines, stripBlockPrefix(cur, prefix))
									i++
									continue
								}
								i++
								break
							}
							if isOpeningBlockDirective(curRest) {
								lockNestDepth++
							}
						}
					}
					innerLines = append(innerLines, stripBlockPrefix(cur, prefix))
					i++
				}

				canView := false
				if opts.Mode == RenderModeWeb {
					if opts.AuthorOrShareBypass {
						canView = true
					} else if lockType == "user" {
						canView = opts.IsReaderLoggedIn
					} else if blockPwd != "" {
						if opts.UnlockedPasswords != nil {
							canView = opts.UnlockedPasswords[blockPwd]
						} else {
							canView = opts.Unlocked
						}
					} else {
						canView = opts.Unlocked
					}
				}

				if canView {
					seq++
					lockPrefix := "INNER" + strconv.Itoa(seq)
					innerPrep := preprocessDirectives(strings.Join(innerLines, "\n"), opts)
					for k, v := range innerPrep.replacements {
						res.replacements[lockPrefix+k] = v
						innerPrep.markdown = strings.ReplaceAll(innerPrep.markdown, k, lockPrefix+k)
					}
					if innerPrep.hasLock {
						res.hasLock = true
					}
					if innerPrep.hasUnresolvedLock {
						res.hasUnresolvedLock = true
					}
					if innerPrep.needsMath {
						res.needsMath = true
					}
					if innerPrep.needsMermaid {
						res.needsMermaid = true
					}
					openDiv := `<div class="volo-lock-block volo-lock-block--unlocked" data-lock-type="` + html.EscapeString(lockType) + `">`
					if prefix == "" {
						outLines = append(outLines, openDiv, "", innerPrep.markdown, "", `</div>`)
					} else {
						outLines = append(outLines, prefix+openDiv, prefix)
						for _, il := range strings.Split(innerPrep.markdown, "\n") {
							outLines = append(outLines, prefix+il)
						}
						outLines = append(outLines, prefix, prefix+`</div>`)
					}
				} else {
					// Locked! Inner lines are completely discarded before AST parsing — zero chance of leakage!
					res.hasUnresolvedLock = true
					tok := nextToken("LOCK")
					res.replacements[tok] = renderLockedPlaceholder(lockType, hint, opts)
					emitSlot(prefix, tok)
				}
				continue
			}

			// 3. `:::book id="..."` / `:::book slug="..."` / `:::book id="...":::`
			if isDirectivePrefix(rest, ":::book") && !isDirectivePrefix(rest, ":::bookshelf") {
				rawArgs := rest[len(":::book"):]
				hasInlineClose := strings.HasSuffix(strings.TrimSpace(rawArgs), ":::")
				attrs := parseDirectiveAttrs(rawArgs)
				slug := attrs["slug"]
				if slug == "" {
					slug = attrs["id"]
				}
				if slug == "" {
					slug = attrs["book"]
				}
				i++
				if !hasInlineClose && i < len(lines) {
					if _, nextRest, nextOk := parseBlockLinePrefix(lines[i]); nextOk && nextRest == ":::" {
						i++
					}
				}
				tok := nextToken("BOOK")
				res.replacements[tok] = renderBookCard(slug, opts)
				emitSlot(prefix, tok)
				continue
			}

			// 4. `:::bookshelf` / `:::bookshelf:::`
			if isDirectivePrefix(rest, ":::bookshelf") {
				rawArgs := rest[len(":::bookshelf"):]
				hasInlineClose := strings.HasSuffix(strings.TrimSpace(rawArgs), ":::")
				i++
				if !hasInlineClose && i < len(lines) {
					if _, nextRest, nextOk := parseBlockLinePrefix(lines[i]); nextOk && nextRest == ":::" {
						i++
					}
				}
				tok := nextToken("BOOKSHELF")
				res.replacements[tok] = renderBookshelfGrid(opts)
				emitSlot(prefix, tok)
				continue
			}
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

// parseBlockLinePrefix strips optional CommonMark `>` blockquote prefixes (each preceded by 0..3 spaces)
// and verifies the remaining line has at most 0..3 leading spaces (NOT a 4-space/tab indented code block).
func parseBlockLinePrefix(line string) (prefix string, rest string, ok bool) {
	pos := 0
	for pos < len(line) {
		sp := 0
		for sp < 4 && pos+sp < len(line) && line[pos+sp] == ' ' {
			sp++
		}
		if sp <= 3 && pos+sp < len(line) && line[pos+sp] == '>' {
			pos += sp + 1
			if pos < len(line) && line[pos] == ' ' {
				pos++
			}
			continue
		}
		break
	}
	sp := 0
	for pos+sp < len(line) && line[pos+sp] == ' ' {
		sp++
		if sp >= 4 {
			return "", "", false
		}
	}
	if pos+sp < len(line) && line[pos+sp] == '\t' {
		return "", "", false
	}
	pos += sp
	return line[:pos], strings.TrimRight(line[pos:], " \t\r"), true
}

// stripBlockPrefix removes matching blockquote `>` markers and up to the opening line's leading spaces.
func stripBlockPrefix(line, fencePrefix string) string {
	if fencePrefix == "" {
		return line
	}
	hasQuote := strings.Contains(fencePrefix, ">")
	pos := 0
	if hasQuote {
		quotesWanted := strings.Count(fencePrefix, ">")
		quotesSeen := 0
		for quotesSeen < quotesWanted && pos < len(line) {
			sp := 0
			for sp < 4 && pos+sp < len(line) && line[pos+sp] == ' ' {
				sp++
			}
			if sp <= 3 && pos+sp < len(line) && line[pos+sp] == '>' {
				pos += sp + 1
				if pos < len(line) && line[pos] == ' ' {
					pos++
				}
				quotesSeen++
				continue
			}
			break
		}
	}
	trailSpaces := 0
	for i := len(fencePrefix) - 1; i >= 0 && fencePrefix[i] == ' '; i-- {
		if hasQuote && i > 0 && fencePrefix[i-1] == '>' {
			break
		}
		trailSpaces++
	}
	for removed := 0; removed < trailSpaces && pos < len(line) && line[pos] == ' '; removed++ {
		pos++
	}
	return line[pos:]
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
	if ch == '`' && strings.IndexByte(trimmed[n:], '`') >= 0 {
		return 0, 0, false
	}
	return ch, n, true
}

func isOpeningBlockDirective(rest string) bool {
	if !strings.HasPrefix(rest, ":::") || len(rest) <= 3 {
		return false
	}
	c := rest[3]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	if strings.HasSuffix(strings.TrimSpace(rest[3:]), ":::") {
		return false
	}
	return true
}

func isDirectivePrefix(trimmed string, directive string) bool {
	if !strings.HasPrefix(trimmed, directive) {
		return false
	}
	rest := trimmed[len(directive):]
	if rest == "" || rest == ":::" {
		return true
	}
	return rest[0] == ' ' || rest[0] == '\t' || rest[0] == '['
}

var bracketArgRe = regexp.MustCompile(`^\[\s*([^\]]+?)\s*\]$`)

func parseDirectiveAttrs(raw string) map[string]string {
	out := make(map[string]string)
	clean := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ":::"))
	if clean == "" {
		return out
	}
	matches := attrPairRe.FindAllStringSubmatch(clean, -1)
	for _, m := range matches {
		val := m[2]
		if val == "" && m[3] != "" {
			val = m[3]
		} else if val == "" && m[4] != "" {
			val = m[4]
		}
		out[strings.ToLower(m[1])] = val
	}
	if len(out) == 0 {
		if bm := bracketArgRe.FindStringSubmatch(clean); len(bm) >= 2 {
			out["slug"] = strings.Trim(bm[1], `"'`)
		} else if !strings.ContainsAny(clean, " \t=") {
			out["slug"] = strings.Trim(clean, `"'`)
		}
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

func formatBookReadCTA(locale string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "en") {
		return "Read →"
	}
	return "开始阅读 →"
}

func bookMonogram(title string) string {
	for _, r := range strings.TrimSpace(title) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return strings.ToUpper(string(r))
		}
	}
	return "B"
}

func writeBookCardInner(b *strings.Builder, info BookCardInfo, locale string) {
	bookHref := "/books/" + url.PathEscape(info.Slug)
	if cover := strings.TrimSpace(info.CoverURL); cover != "" && isSafeImageSrc(cover) {
		b.WriteString(`<a class="volo-embed-book__cover-link" href="` + bookHref + `" tabindex="-1" aria-hidden="true">`)
		b.WriteString(`<img class="volo-embed-book__cover" src="` + html.EscapeString(cover) + `" alt="` + html.EscapeString(info.Title) + `" loading="lazy" decoding="async">`)
		b.WriteString(`</a>`)
	} else {
		b.WriteString(`<a class="volo-embed-book__cover volo-embed-book__cover--fallback" href="` + bookHref + `" tabindex="-1" aria-hidden="true">`)
		b.WriteString(`<span class="volo-embed-book__spine"></span>`)
		b.WriteString(`<span class="volo-embed-book__monogram">` + html.EscapeString(bookMonogram(info.Title)) + `</span>`)
		b.WriteString(`</a>`)
	}
	b.WriteString(`<div class="volo-embed-book__body">`)
	b.WriteString(`<div class="volo-embed-book__head">`)
	b.WriteString(`<a class="volo-embed-book__title" href="` + bookHref + `"><strong>` + html.EscapeString(info.Title) + `</strong></a>`)
	if info.ChapterCount > 0 {
		b.WriteString(`<span class="volo-embed-book__chapters">` + html.EscapeString(formatBookChapterBadge(info.ChapterCount, locale)) + `</span>`)
	}
	b.WriteString(`</div>`)
	if desc := strings.TrimSpace(info.Description); desc != "" {
		b.WriteString(`<p class="volo-embed-book__desc">` + html.EscapeString(desc) + `</p>`)
	}
	b.WriteString(`<div class="volo-embed-book__foot">`)
	b.WriteString(`<a class="volo-embed-book__cta" href="` + bookHref + `">` + html.EscapeString(formatBookReadCTA(locale)) + `</a>`)
	b.WriteString(`</div>`)
	b.WriteString(`</div>`)
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
	writeBookCardInner(&b, info, opts.Locale)
	b.WriteString(`</section>`)
	return b.String()
}

func renderBookshelfGrid(opts Options) string {
	var books []BookCardInfo
	if opts.ListBooks != nil {
		books = opts.ListBooks()
	}
	if len(books) == 0 {
		emptyMsg := "暂无已发布的在线书籍"
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(opts.Locale)), "en") {
			emptyMsg = "No online books published yet."
		}
		return `<section class="volo-embed-bookshelf volo-embed-bookshelf--empty"><p class="volo-embed-bookshelf__empty">` + html.EscapeString(emptyMsg) + `</p></section>`
	}
	var b strings.Builder
	b.WriteString(`<section class="volo-embed-bookshelf">`)
	for _, bk := range books {
		b.WriteString(`<article class="volo-book-tile" data-book-slug="` + html.EscapeString(bk.Slug) + `">`)
		writeBookCardInner(&b, bk, opts.Locale)
		b.WriteString(`</article>`)
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
	proseImgCount := 0
	return imgTagRe.ReplaceAllStringFunc(htmlSrc, func(tag string) string {
		m := imgSrcRe.FindStringSubmatch(tag)
		if len(m) < 2 {
			return tag
		}
		src := html.UnescapeString(m[1])
		isEmbedCover := strings.Contains(tag, "volo-embed-book__cover")
		if !isEmbedCover {
			proseImgCount++
		}
		if firstImg != nil && *firstImg == "" && !isEmbedCover && isSafeImageSrc(src) {
			*firstImg = src
		}
		var attrs strings.Builder
		if !strings.Contains(tag, "loading=") && (isEmbedCover || proseImgCount > 1) {
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
	if strings.HasPrefix(s, "//") || strings.HasPrefix(s, `/\`) {
		return false
	}
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
			rawTS := strings.TrimSuffix(strings.TrimPrefix(match, "["), "]")
			return fmt.Sprintf(`<button type="button" class="volo-audio-ts" data-audio-seek="%d">▶ %s</button>`, sec, html.EscapeString(rawTS))
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

// StripLockedMarkdownBlocks is an alias for StripAllLockBlocks, removing all `:::lock ... :::` blocks
// outside fenced code blocks while honoring CommonMark indentation, blockquotes, and nested directives.
func StripLockedMarkdownBlocks(src string) string {
	return StripAllLockBlocks(src)
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
	lockNestDepth := 0

	for _, line := range lines {
		if _, rest, lineOk := parseBlockLinePrefix(line); lineOk {
			if ch, n, ok := parseFenceStart(rest); ok {
				if !inFence {
					inFence = true
					fenceChar = ch
					fenceLen = n
				} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
					inFence = false
				}
				if !inLock {
					out = append(out, line)
				}
				continue
			}
			if !inFence {
				if !inLock && isDirectivePrefix(rest, ":::lock") {
					inLock = true
					lockNestDepth = 0
					continue
				}
				if inLock {
					if rest == ":::" {
						if lockNestDepth > 0 {
							lockNestDepth--
							continue
						}
						inLock = false
						continue
					}
					if isOpeningBlockDirective(rest) {
						lockNestDepth++
					}
					continue
				}
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
		if _, rest, lineOk := parseBlockLinePrefix(line); lineOk {
			if ch, n, ok := parseFenceStart(rest); ok {
				if !inFence {
					inFence = true
					fenceChar = ch
					fenceLen = n
				} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
					inFence = false
				}
				continue
			}
		}
		if !inFence {
			out = append(out, stripInlineCodeSpans(line))
		}
	}
	return strings.Join(out, "\n")
}

// ExtractAllLockPasswords scans Markdown outside fenced code blocks for all unique `:::lock ... pwd="..."`
// (or `password="..."`) attributes in order of appearance.
func ExtractAllLockPasswords(src string) []string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	inFence := false
	var fenceChar byte
	fenceLen := 0
	var out []string
	seen := make(map[string]struct{})
	for _, line := range lines {
		_, rest, lineOk := parseBlockLinePrefix(line)
		if !lineOk {
			continue
		}
		if ch, n, ok := parseFenceStart(rest); ok {
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = n
			} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
				inFence = false
			}
			continue
		}
		if !inFence && isDirectivePrefix(rest, ":::lock") {
			attrs := parseDirectiveAttrs(rest[len(":::lock"):])
			p := strings.TrimSpace(attrs["pwd"])
			if p == "" {
				p = strings.TrimSpace(attrs["password"])
			}
			if p != "" {
				if _, exists := seen[p]; !exists {
					seen[p] = struct{}{}
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// ExtractFirstLockPassword scans Markdown outside fenced code blocks for `:::lock ... pwd="..."` (or `password="..."`)
// so public articles with inline password-locked blocks can be unlocked via /api/v1/articles/{id}/unlock.
func ExtractFirstLockPassword(src string) string {
	pwds := ExtractAllLockPasswords(src)
	if len(pwds) == 0 {
		return ""
	}
	return pwds[0]
}

// ContainsLockDirective reports whether src contains at least one `:::lock` directive outside fenced code blocks.
func ContainsLockDirective(src string) bool {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	inFence := false
	var fenceChar byte
	fenceLen := 0
	for _, line := range lines {
		_, rest, lineOk := parseBlockLinePrefix(line)
		if !lineOk {
			continue
		}
		if ch, n, ok := parseFenceStart(rest); ok {
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = n
			} else if ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
				inFence = false
			}
			continue
		}
		if !inFence && isDirectivePrefix(rest, ":::lock") {
			return true
		}
	}
	return false
}
