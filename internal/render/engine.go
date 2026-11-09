package render

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

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
	PreserveDirectives  bool // Emits non-editable data-volo-md-b64 containers for lossless WYSIWYG round-tripping
	LockPromptTitle     string
	LockPromptAction    string
	FeedLockedNotice    string
	LookupBook          func(slug string) (BookCardInfo, bool)
	ListBooks           func() []BookCardInfo
	LookupMediaDims     func(src string) (width int, height int, ok bool)
	CustomDirective     func(name string, attrs map[string]string, body string) (rawHTML string, ok bool)
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

// Engine is a thread-safe, zero-CGO Markdown rendering pipeline powered by goldmark, native lexers, and an HTML5 sanitizer.
type Engine struct {
	mdOnce         sync.Once
	md             goldmark.Markdown
	sanOnce        sync.Once
	sanitizer      *htmlSanitizer
	editorSanOnce  sync.Once
	editorSan      *htmlSanitizer
	commentMDOnce  sync.Once
	commentMD      goldmark.Markdown
	commentSanOnce sync.Once
	commentSan     *htmlSanitizer
}

var (
	defaultEngineOnce       sync.Once
	defaultEngineInst       *Engine
	inputCheckboxTypeRe     = regexp.MustCompile(`^checkbox$`)
	safeMediaSrcAttrRe      = regexp.MustCompile(`^(?:https?://[^\s"'<>]+|/[^/\s"'<>\\][^\s"'<>]*)$`)
	safeEmbedIframeRe       = regexp.MustCompile(`^https://(?:player\.bilibili\.com/player\.html\?bvid=BV[0-9A-Za-z]{10}(?:&(?:amp;)?autoplay=0)?|www\.youtube(?:-nocookie)?\.com/embed/[A-Za-z0-9_-]{11})$`)
	safeIframeSandboxRe     = regexp.MustCompile(`^allow-scripts allow-same-origin allow-presentation$`)
	safeIframeRefPolRe      = regexp.MustCompile(`^(?:no-referrer|strict-origin-when-cross-origin)$`)
	chromaCommentClsRe      = regexp.MustCompile(`^(chroma|line|cl|ln|lnt|lntable|lntd|hl|c1|s1|s2|[a-z]{1,3})$`)
	safeFontColorRe         = regexp.MustCompile(`^(?i)\s*(?:#[0-9a-f]{3,8}|[a-z]{2,24}|rgba?\(\s*\d{1,3}\s*,\s*\d{1,3}\s*,\s*\d{1,3}(?:\s*,\s*(?:0|1|0?\.\d+))?\s*\))\s*;?\s*$`)
	safeFontSizeRe          = regexp.MustCompile(`^[+-]?[1-7]$`)
	safeInlineStyleRe       = regexp.MustCompile(`^(?i)\s*(?:(?:color|background-color|font-size|font-weight|font-style|text-align|text-decoration)\s*:\s*(?:#[0-9a-f]{3,8}|[a-z0-9.% -]{1,32}|rgba?\(\s*\d{1,3}\s*,\s*\d{1,3}\s*,\s*\d{1,3}(?:\s*,\s*(?:0|1|0?\.\d+))?\s*\))\s*;?\s*)+$`)
	commentListStartRe      = regexp.MustCompile(`^[0-9]+$`)
	commentTableAlignRe     = regexp.MustCompile(`^(?i)(left|right|center|justify)$`)
	commentTableScrollClsRe = regexp.MustCompile(`^volo-table-scroll$`)
	tocPreserveSlotRe       = regexp.MustCompile(`<div class="volo-inline-toc-slot" data-volo-md-b64="([A-Za-z0-9+/=]+)"></div>`)
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
			),
			goldmark.WithParserOptions(
				parser.WithAutoHeadingID(),
			),
			goldmark.WithRendererOptions(
				gmhtml.WithUnsafe(),    // Raw HTML is strictly filtered by the HTML5 fragment sanitizer right after rendering
				gmhtml.WithHardWraps(), // Preserve single-newline soft line breaks (<br>) across paragraphs and blockquotes
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
			goldmark.WithRendererOptions(
				gmhtml.WithHardWraps(),
			),
		)
	})
	return e.commentMD
}

func (e *Engine) ensureSanitizer() *htmlSanitizer {
	e.sanOnce.Do(func() {
		e.sanitizer = newArticleSanitizer(false)
	})
	return e.sanitizer
}

func (e *Engine) ensureEditorSanitizer() *htmlSanitizer {
	e.editorSanOnce.Do(func() {
		e.editorSan = newArticleSanitizer(true)
	})
	return e.editorSan
}

func (e *Engine) ensureCommentSanitizer() *htmlSanitizer {
	e.commentSanOnce.Do(func() {
		e.commentSan = newCommentSanitizer()
	})
	return e.commentSan
}

func (e *Engine) sanitizeArticleHTML(rawHTML string, allowEditorDirectives bool) string {
	if allowEditorDirectives {
		return e.ensureEditorSanitizer().Sanitize(rawHTML)
	}
	return e.ensureSanitizer().Sanitize(rawHTML)
}

// SanitizeHTML passes an HTML fragment directly through the engine's strict HTML5 XSS sanitizer.
func (e *Engine) SanitizeHTML(rawHTML string) string {
	return e.sanitizeArticleHTML(rawHTML, false)
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
	rendered = wrapTablesInScroll(rendered)
	return e.ensureCommentSanitizer().Sanitize(rendered)
}

// RenderBasic renders Markdown into sanitized HTML with responsive table scroll wrappers.
func (e *Engine) RenderBasic(markdown string) string {
	if strings.TrimSpace(markdown) == "" {
		return ""
	}
	return wrapTablesInScroll(e.Render(markdown, Options{Mode: RenderModeWeb}).HTML)
}

// RenderSummary renders summary/excerpt Markdown into sanitized HTML after stripping locked blocks,
// wrapping any tables in .volo-table-scroll for narrow viewports.
func (e *Engine) RenderSummary(markdown string) string {
	clean := StripAllLockBlocks(markdown)
	if strings.TrimSpace(clean) == "" {
		return ""
	}
	return wrapTablesInScroll(e.Render(clean, Options{Mode: RenderModeWeb}).HTML)
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

	// Replace placeholders (code blocks, Mermaid diagrams, Math blocks/spans, Lock cards, Book cards, Custom directives) before sanitization.
	for token, fragment := range prep.replacements {
		slot := `<div data-volo-slot="` + token + `"></div>`
		rendered = strings.ReplaceAll(rendered, "<p>"+slot+"</p>", fragment)
		rendered = strings.ReplaceAll(rendered, slot, fragment)
		if !strings.HasPrefix(fragment, "<span ") {
			rendered = strings.ReplaceAll(rendered, "<p>"+token+"</p>", fragment)
		}
		rendered = strings.ReplaceAll(rendered, token, fragment)
	}

	// Transform GitHub-flavored Alerts (> [!NOTE], > [!TIP], > [!IMPORTANT], > [!WARNING], > [!CAUTION]) into .volo-callout blocks.
	rendered = transformGitHubAlerts(rendered, opts.Locale)

	// Transform extended inline Markdown syntax (==highlight==, ^sup^, ~sub~) outside code, math, and URLs.
	rendered = enhanceExtendedInlineFormatting(rendered)

	// Enhance <img> tags with intrinsic dimensions & lazy decoding and capture FirstImageURL from unlocked content.
	var firstImg string
	rendered = enhanceImages(rendered, opts.LookupMediaDims, &firstImg)

	// Enhance standalone media links (.mp4/.webm/.m4v, .mp3/.m4a/.ogg/.wav/.weba, Bilibili, YouTube) and responsive tables in web mode.
	if opts.Mode == RenderModeWeb {
		rendered = enhanceStandaloneMediaLinks(rendered)
		rendered = enhanceTablesAndGitHubShowcase(rendered)
	}

	// Convert [mm:ss] / [hh:mm:ss] timestamps outside code/links/math when podcast audio is attached.
	if opts.HasAudio && opts.Mode == RenderModeWeb {
		rendered = injectAudioTimestamps(rendered)
	}

	// Sanitize through the HTML5 fragment sanitizer to neutralize script/event-handler/scheme XSS vectors.
	safeHTML := e.sanitizeArticleHTML(rendered, opts.PreserveDirectives)
	if opts.Mode == RenderModeWeb {
		safeHTML = assignReadableHeadingIDs(safeHTML)
	}

	toc := extractHeadingsTOC(safeHTML)
	if opts.PreserveDirectives && strings.Contains(safeHTML, `class="volo-inline-toc-slot"`) {
		safeHTML = tocPreserveSlotRe.ReplaceAllStringFunc(safeHTML, func(slot string) string {
			m := tocPreserveSlotRe.FindStringSubmatch(slot)
			if len(m) < 2 {
				return ""
			}
			if opts.Mode == RenderModeWeb && len(toc) > 0 {
				return renderInlineTOCWithDirective(toc, opts.Locale, m[1])
			}
			return renderInlineTOCWithDirective(nil, opts.Locale, m[1])
		})
	}
	if strings.Contains(safeHTML, `<div class="volo-inline-toc-slot"></div>`) {
		if opts.Mode == RenderModeWeb && len(toc) > 0 {
			safeHTML = strings.ReplaceAll(safeHTML, `<div class="volo-inline-toc-slot"></div>`, renderInlineTOC(toc, opts.Locale))
		} else {
			safeHTML = strings.ReplaceAll(safeHTML, `<div class="volo-inline-toc-slot"></div>`, "")
		}
	}
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
	return wrapTablesInScroll(strings.TrimSpace(mdBuf.String()))
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
	var tracker blockLineTracker

	for i := 0; i < len(lines); {
		line := lines[i]
		info := tracker.next(line)
		if info.ok {
			if ch, fenceLen, fOk := parseFenceStart(info.rest); fOk {
				infoStr := strings.TrimSpace(info.rest[fenceLen:])
				lang := strings.Fields(infoStr)
				langName := ""
				if len(lang) > 0 {
					langName = strings.ToLower(lang[0])
				}
				i++
				var codeLines []string
				for i < len(lines) {
					if closeRest, cLineOk := parseChildBlockLine(lines[i], info.activeIndent); cLineOk {
						if cCh, cLen, cOk := parseFenceStart(closeRest); cOk && cCh == ch && cLen >= fenceLen && strings.TrimSpace(closeRest[cLen:]) == "" {
							i++
							break
						}
					}
					codeLines = append(codeLines, stripBlockPrefixCols(lines[i], info.quoteDepth, info.stripCols))
					i++
				}
				seq++
				tok := fmt.Sprintf("VOLOCMTCODE%dX%dEND", len(src), seq)
				slots[tok] = highlightCommentCodeChroma(strings.Join(codeLines, "\n"), langName)
				if info.openPrefix == "" {
					outLines = append(outLines, "", tok, "")
				} else {
					outLines = append(outLines, info.openPrefix+tok)
				}
				continue
			}
		}
		outLines = append(outLines, line)
		i++
	}
	return strings.Join(outLines, "\n"), slots
}

func encodeDirectiveB64(rawMD string, opts Options) string {
	if !opts.PreserveDirectives || opts.Mode == RenderModeFeed {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(rawMD))
}

func injectPreserveAttrsOnRootTag(htmlFragment string, b64 string) string {
	if b64 == "" {
		return htmlFragment
	}
	trimmed := strings.TrimSpace(htmlFragment)
	attrChunk := ` contenteditable="false" data-volo-md-b64="` + b64 + `"`
	for _, rootTag := range []string{"div", "section", "nav", "aside"} {
		lower := strings.ToLower(trimmed)
		closeTag := "</" + rootTag + ">"
		if hasHTMLTagOpen(lower, rootTag) && strings.HasSuffix(lower, closeTag) {
			gt := scanHTMLTagOrCommentEnd(trimmed, 0)
			if gt > 1 && !strings.HasSuffix(trimmed[:gt], "/>") {
				// Verify this root tag does not close prematurely before the end of the fragment.
				depth := 1
				scan := gt
				singleRoot := true
				for scan < len(trimmed)-len(closeTag) {
					nextLt := strings.IndexByte(trimmed[scan:], '<')
					if nextLt < 0 {
						break
					}
					pos := scan + nextLt
					end := scanHTMLTagOrCommentEnd(trimmed, pos)
					if end < 0 {
						break
					}
					innerLower := strings.ToLower(trimmed[pos:end])
					if hasHTMLTagOpen(innerLower, rootTag) && !strings.HasSuffix(innerLower, "/>") {
						depth++
					} else if strings.HasPrefix(innerLower, "</"+rootTag) {
						depth--
						if depth == 0 {
							singleRoot = false
							break
						}
					}
					scan = end
				}
				if singleRoot {
					return trimmed[:gt-1] + attrChunk + trimmed[gt-1:]
				}
			}
		}
	}
	return `<div class="volo-custom-directive"` + attrChunk + `>` + htmlFragment + `</div>`
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
	emitSlot := func(openPrefix, tok string) {
		slot := `<div data-volo-slot="` + tok + `"></div>`
		if openPrefix == "" {
			outLines = append(outLines, "", slot, "")
		} else {
			outLines = append(outLines, openPrefix+slot)
		}
	}

	var tracker blockLineTracker
	inHTMLComment := false
	inRawPre := false

	processProseSegment := func(seg string) string {
		if seg == "" {
			return ""
		}
		processed, foundMath := extractLineMath(seg, nextToken, res.replacements)
		if foundMath {
			res.needsMath = true
		}
		return rewriteSubscriptOutsideCode(rewriteWikiLinksOutsideCode(processed))
	}

	i := 0
	for i < len(lines) {
		line := lines[i]

		// Handle active multi-line raw HTML comment (`<!-- ... -->`).
		if inHTMLComment {
			if closeIdx := strings.Index(line, "-->"); closeIdx >= 0 {
				inHTMLComment = false
				end := closeIdx + 3
				tail := line[end:]
				if strings.TrimSpace(tail) == "" {
					outLines = append(outLines, line)
				} else {
					outLines = append(outLines, line[:end]+processProseSegment(tail))
				}
			} else {
				outLines = append(outLines, line)
			}
			i++
			continue
		}

		// Handle active multi-line raw `<pre>...</pre>` block.
		if inRawPre {
			if closeEnd := findCaseInsensitivePreCloseEnd(line); closeEnd >= 0 {
				inRawPre = false
				tail := line[closeEnd:]
				if strings.TrimSpace(tail) == "" {
					outLines = append(outLines, line)
				} else {
					outLines = append(outLines, line[:closeEnd]+processProseSegment(tail))
				}
			} else {
				outLines = append(outLines, line)
			}
			i++
			continue
		}

		info := tracker.next(line)

		if info.ok {
			rest := info.rest

			// 0a. Raw HTML comment block (`<!-- ... -->`).
			if strings.HasPrefix(rest, "<!--") {
				openPos := strings.Index(line, "<!--")
				if closeRel := strings.Index(line[openPos+4:], "-->"); closeRel >= 0 {
					closeEnd := openPos + 4 + closeRel + 3
					tail := line[closeEnd:]
					if strings.TrimSpace(tail) == "" {
						outLines = append(outLines, line)
					} else {
						outLines = append(outLines, line[:closeEnd]+processProseSegment(tail))
					}
				} else {
					if hasClosingHTMLComment(lines[i+1:]) {
						inHTMLComment = true
					}
					outLines = append(outLines, line)
				}
				i++
				continue
			}

			// 0b. Raw `<pre>...</pre>` HTML block.
			if hasHTMLTagOpen(strings.ToLower(rest), "pre") {
				openPos := strings.IndexByte(line, '<')
				tagEnd := scanHTMLTagOrCommentEnd(line, openPos)
				if tagEnd > openPos && !strings.HasSuffix(strings.TrimSpace(line[openPos:tagEnd]), "/>") {
					if closeRel := findCaseInsensitivePreCloseEnd(line[tagEnd:]); closeRel >= 0 {
						closeEnd := tagEnd + closeRel
						tail := line[closeEnd:]
						if strings.TrimSpace(tail) == "" {
							outLines = append(outLines, line)
						} else {
							outLines = append(outLines, line[:closeEnd]+processProseSegment(tail))
						}
					} else {
						if hasClosingPreTag(lines[i+1:]) {
							inRawPre = true
						}
						outLines = append(outLines, line)
					}
					i++
					continue
				}
			}

			// 1. Fenced code block (` ```lang ` or `~~~lang`): highlight via native lexer or emit Mermaid container!
			if ch, fenceLen, ok := parseFenceStart(rest); ok {
				infoStr := strings.TrimSpace(rest[fenceLen:])
				lang := strings.Fields(infoStr)
				langName := ""
				if len(lang) > 0 {
					langName = strings.ToLower(lang[0])
				}
				i++
				var codeLines []string
				for i < len(lines) {
					if closeRest, cLineOk := parseChildBlockLine(lines[i], info.activeIndent); cLineOk {
						if cCh, cLen, cOk := parseFenceStart(closeRest); cOk && cCh == ch && cLen >= fenceLen && strings.TrimSpace(closeRest[cLen:]) == "" {
							i++
							break
						}
					}
					codeLines = append(codeLines, stripBlockPrefixCols(lines[i], info.quoteDepth, info.stripCols))
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
				emitSlot(info.openPrefix, tok)
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

				rawFenceLines := []string{rest}
				var innerLines []string
				inInnerFence := false
				var innerFenceCh byte
				innerFenceLen := 0
				lockNestDepth := 0

				for i < len(lines) {
					cur := lines[i]
					strippedCur := stripBlockPrefixCols(cur, info.quoteDepth, info.stripCols)
					if curRest, curOk := parseChildBlockLine(cur, info.activeIndent); curOk {
						if fCh, fLen, fOk := parseFenceStart(curRest); fOk {
							if !inInnerFence {
								inInnerFence = true
								innerFenceCh = fCh
								innerFenceLen = fLen
							} else if fCh == innerFenceCh && fLen >= innerFenceLen && strings.TrimSpace(curRest[fLen:]) == "" {
								inInnerFence = false
							}
							innerLines = append(innerLines, strippedCur)
							rawFenceLines = append(rawFenceLines, strippedCur)
							i++
							continue
						}
						if !inInnerFence {
							if curRest == ":::" {
								if lockNestDepth > 0 {
									lockNestDepth--
									innerLines = append(innerLines, strippedCur)
									rawFenceLines = append(rawFenceLines, strippedCur)
									i++
									continue
								}
								rawFenceLines = append(rawFenceLines, ":::")
								i++
								break
							}
							if isOpeningBlockDirective(curRest) {
								lockNestDepth++
							}
						}
					}
					innerLines = append(innerLines, strippedCur)
					rawFenceLines = append(rawFenceLines, strippedCur)
					i++
				}

				lockB64 := encodeDirectiveB64(strings.Join(rawFenceLines, "\n"), opts)

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
					innerOpts := opts
					if lockB64 != "" {
						// Outer lock wrapper already preserves the full :::lock fence verbatim.
						innerOpts.PreserveDirectives = false
					}
					innerPrep := preprocessDirectives(strings.Join(innerLines, "\n"), innerOpts)
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
					extraAttrs := ""
					if lockB64 != "" {
						extraAttrs = ` contenteditable="false" data-volo-md-b64="` + lockB64 + `"`
					}
					openDiv := `<div class="volo-lock-block volo-lock-block--unlocked" data-lock-type="` + html.EscapeString(lockType) + `"` + extraAttrs + `>`
					if info.openPrefix == "" {
						outLines = append(outLines, openDiv, "", innerPrep.markdown, "", `</div>`)
					} else {
						outLines = append(outLines, info.openPrefix+openDiv, info.contPrefix)
						for _, il := range strings.Split(innerPrep.markdown, "\n") {
							outLines = append(outLines, info.contPrefix+il)
						}
						outLines = append(outLines, info.contPrefix, info.contPrefix+`</div>`)
					}
				} else {
					// Locked! Inner lines are completely discarded before AST parsing — zero chance of leakage!
					res.hasUnresolvedLock = true
					tok := nextToken("LOCK")
					res.replacements[tok] = renderLockedPlaceholder(lockType, hint, opts, lockB64)
					emitSlot(info.openPrefix, tok)
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
				rawBookMD := rest
				i++
				if !hasInlineClose && i < len(lines) {
					if nextRest, nextOk := parseChildBlockLine(lines[i], info.activeIndent); nextOk && nextRest == ":::" {
						rawBookMD += "\n:::"
						i++
					}
				}
				bookB64 := encodeDirectiveB64(rawBookMD, opts)
				tok := nextToken("BOOK")
				cardHTML := renderBookCard(slug, opts)
				if bookB64 != "" {
					if cardHTML == "" {
						cardHTML = `<span class="volo-embed-book__title"><strong>:::book</strong></span>`
					}
					cardHTML = `<div class="volo-embed-book-wrap" contenteditable="false" data-volo-md-b64="` + bookB64 + `">` + cardHTML + `</div>`
				}
				res.replacements[tok] = cardHTML
				emitSlot(info.openPrefix, tok)
				continue
			}

			// 4. `:::bookshelf` / `:::bookshelf[limit="12"]` / `:::bookshelf:::`
			if isDirectivePrefix(rest, ":::bookshelf") {
				rawArgs := rest[len(":::bookshelf"):]
				hasInlineClose := strings.HasSuffix(strings.TrimSpace(rawArgs), ":::")
				attrs := parseDirectiveAttrs(rawArgs)
				limit := 0
				if rawLim := strings.TrimSpace(attrs["limit"]); rawLim != "" {
					if n, err := strconv.Atoi(rawLim); err == nil && n > 0 {
						limit = n
					}
				}
				rawShelfMD := rest
				i++
				if !hasInlineClose && i < len(lines) {
					if nextRest, nextOk := parseChildBlockLine(lines[i], info.activeIndent); nextOk && nextRest == ":::" {
						rawShelfMD += "\n:::"
						i++
					}
				}
				shelfB64 := encodeDirectiveB64(rawShelfMD, opts)
				tok := nextToken("BOOKSHELF")
				res.replacements[tok] = renderBookshelfGrid(opts, limit, shelfB64)
				emitSlot(info.openPrefix, tok)
				continue
			}

			// 5. Unknown `:::name[...]` custom plugin directives.
			if opts.CustomDirective != nil || opts.PreserveDirectives {
				if dirName, rawArgs, dOk := parseCustomDirectiveHeader(rest); dOk {
					attrs := parseDirectiveAttrs(rawArgs)
					body, rawCustomMD, nextIdx := scanCustomDirectiveBody(lines, i, info, rest, rawArgs)
					customB64 := encodeDirectiveB64(rawCustomMD, opts)
					if opts.CustomDirective != nil {
						if rawHTML, handled := opts.CustomDirective(dirName, attrs, body); handled {
							i = nextIdx
							tok := nextToken("CUSTOM")
							res.replacements[tok] = injectPreserveAttrsOnRootTag(rawHTML, customB64)
							emitSlot(info.openPrefix, tok)
							continue
						}
					}
					if customB64 != "" {
						i = nextIdx
						tok := nextToken("CUSTOM")
						label := ":::" + dirName
						res.replacements[tok] = `<div class="volo-custom-directive" contenteditable="false" data-volo-md-b64="` + customB64 + `"><p>` + html.EscapeString(label) + `</p></div>`
						emitSlot(info.openPrefix, tok)
						continue
					}
				}
			}

			// 5b. Standalone `[toc]` / `[TOC]` inline Table of Contents marker.
			if info.quoteDepth == 0 && info.activeIndent == 0 && strings.EqualFold(rest, "[toc]") {
				tocB64 := encodeDirectiveB64(rest, opts)
				tok := nextToken("TOC")
				if tocB64 != "" {
					res.replacements[tok] = `<div class="volo-inline-toc-slot" data-volo-md-b64="` + tocB64 + `"></div>`
				} else {
					res.replacements[tok] = `<div class="volo-inline-toc-slot"></div>`
				}
				emitSlot("", tok)
				i++
				continue
			}

			// 6. Standalone or multi-line display math (`$$...$$` or `\[...\]`).
			if mathBody, nextIdx, mOk := tryExtractBlockMath(lines, i, info, rest); mOk {
				res.needsMath = true
				tok := nextToken("MATHBLOCK")
				res.replacements[tok] = `<div class="volo-math-block">$$` + html.EscapeString(mathBody) + `$$</div>`
				emitSlot(info.openPrefix, tok)
				i = nextIdx
				continue
			}

			// 7. Single-line display/inline math, wiki-links, and subscripts (handling mid-line `<!--` / `<pre>` openings).
			outLine, openedComment, openedPre := processInlineLineWithRawHTMLState(line, processProseSegment)
			if openedComment && hasClosingHTMLComment(lines[i+1:]) {
				inHTMLComment = true
			}
			if openedPre && hasClosingPreTag(lines[i+1:]) {
				inRawPre = true
			}
			outLines = append(outLines, outLine)
			i++
			continue
		}

		// Indented (>= activeIndent + 4 columns) code block lines stay completely untouched!
		outLines = append(outLines, line)
		i++
	}

	res.markdown = strings.Join(outLines, "\n")
	return res
}

func hasClosingHTMLComment(lines []string) bool {
	for _, ln := range lines {
		if strings.Contains(ln, "-->") {
			return true
		}
	}
	return false
}

func hasClosingPreTag(lines []string) bool {
	for _, ln := range lines {
		if findCaseInsensitivePreCloseEnd(ln) >= 0 {
			return true
		}
	}
	return false
}

func findCaseInsensitivePreCloseEnd(s string) int {
	lower := strings.ToLower(s)
	offset := 0
	for offset < len(lower) {
		rel := strings.Index(lower[offset:], "</pre")
		if rel < 0 {
			return -1
		}
		pos := offset + rel
		after := pos + len("</pre")
		if after < len(lower) {
			ch := lower[after]
			if ch == '>' {
				return after + 1
			}
			if ch == ' ' || ch == '\t' || ch == '\r' {
				if gt := strings.IndexByte(lower[after:], '>'); gt >= 0 {
					return after + gt + 1
				}
			}
		}
		offset = pos + 5
	}
	return -1
}

// processInlineLineWithRawHTMLState applies `processSeg` outside unclosed mid-line `<!--` comments
// and unclosed mid-line `<pre>` blocks, tracking multi-line transitions cleanly.
func processInlineLineWithRawHTMLState(line string, processSeg func(string) string) (out string, openedComment bool, openedPre bool) {
	if !strings.Contains(line, "<!--") && !strings.Contains(strings.ToLower(line), "<pre") {
		return processSeg(line), false, false
	}

	var b strings.Builder
	b.Grow(len(line))
	segStart := 0

	for p := 0; p < len(line); {
		if line[p] == '`' {
			tickLen := 0
			for p+tickLen < len(line) && line[p+tickLen] == '`' {
				tickLen++
			}
			if closeIdx := findClosingBacktickRun(line, p+tickLen, tickLen); closeIdx >= 0 {
				p = closeIdx + tickLen
				continue
			}
			p += tickLen
			continue
		}
		if strings.HasPrefix(line[p:], "<!--") {
			if closeRel := strings.Index(line[p+4:], "-->"); closeRel >= 0 {
				p = p + 4 + closeRel + 3
				continue
			}
			b.WriteString(processSeg(line[segStart:p]))
			b.WriteString(line[p:])
			return b.String(), true, false
		}
		if line[p] == '<' && hasHTMLTagOpen(strings.ToLower(line[p:]), "pre") {
			tagEnd := scanHTMLTagOrCommentEnd(line, p)
			if tagEnd > p && !strings.HasSuffix(strings.TrimSpace(line[p:tagEnd]), "/>") {
				if closeRel := findCaseInsensitivePreCloseEnd(line[tagEnd:]); closeRel >= 0 {
					b.WriteString(processSeg(line[segStart:p]))
					endPre := tagEnd + closeRel
					b.WriteString(line[p:endPre])
					p = endPre
					segStart = p
					continue
				}
				b.WriteString(processSeg(line[segStart:p]))
				b.WriteString(line[p:])
				return b.String(), false, true
			}
		}
		p++
	}

	if segStart == 0 {
		return processSeg(line), false, false
	}
	b.WriteString(processSeg(line[segStart:]))
	return b.String(), false, false
}

type listItemContext struct {
	quoteDepth       int
	contentIndent    int
	sawBlank         bool
	emptyOnFirstLine bool
	justOpened       bool
}

type blockLineInfo struct {
	openPrefix   string
	contPrefix   string
	rest         string
	quoteDepth   int
	activeIndent int
	stripCols    int
	ok           bool
}

// blockLineTracker tracks CommonMark blockquote and list-item indentation across lines
// so child blocks (`    :::lock`, `    ```go`) inside `- item` or `10. item` are recognized
// identically to Goldmark while top-level 4-space indented code blocks remain untouched.
type blockLineTracker struct {
	stack []listItemContext
}

func scanBlockquotePrefix(line string) (quoteDepth int, pos int) {
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
			quoteDepth++
			continue
		}
		break
	}
	return quoteDepth, pos
}

func measureLeadingIndent(s string) (cols int, bytesConsumed int) {
	for bytesConsumed < len(s) {
		ch := s[bytesConsumed]
		if ch == ' ' {
			cols++
			bytesConsumed++
		} else if ch == '\t' {
			cols += 4 - (cols % 4)
			bytesConsumed++
		} else {
			break
		}
	}
	return cols, bytesConsumed
}

func isThematicBreakLine(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	marker := trimmed[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	count := 0
	for i := 0; i < len(trimmed); i++ {
		ch := trimmed[i]
		if ch == marker {
			count++
		} else if ch != ' ' && ch != '\t' && ch != '\r' {
			return false
		}
	}
	return count >= 3
}

func parseCommonMarkListMarker(s string) (markerBytes int, ok bool) {
	if len(s) == 0 || isThematicBreakLine(s) {
		return 0, false
	}
	c0 := s[0]
	if c0 == '-' || c0 == '*' || c0 == '+' {
		if len(s) == 1 || s[1] == ' ' || s[1] == '\t' || s[1] == '\r' {
			return 1, true
		}
		return 0, false
	}
	k := 0
	for k < len(s) && k < 9 && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	if k > 0 && k < len(s) && (s[k] == '.' || s[k] == ')') {
		if k+1 == len(s) || s[k+1] == ' ' || s[k+1] == '\t' || s[k+1] == '\r' {
			return k + 1, true
		}
	}
	return 0, false
}

func interruptsLazyListContinuation(trimmed string) bool {
	if trimmed == "" {
		return true
	}
	if isThematicBreakLine(trimmed) {
		return true
	}
	if _, ok := parseCommonMarkListMarker(trimmed); ok {
		return true
	}
	if _, _, ok := parseFenceStart(trimmed); ok {
		return true
	}
	if strings.HasPrefix(trimmed, ":::") {
		return true
	}
	if trimmed[0] == '#' {
		h := 0
		for h < len(trimmed) && h <= 6 && trimmed[h] == '#' {
			h++
		}
		if h >= 1 && h <= 6 && (h == len(trimmed) || trimmed[h] == ' ' || trimmed[h] == '\t') {
			return true
		}
	}
	return false
}

func (t *blockLineTracker) next(line string) blockLineInfo {
	quoteDepth, pos := scanBlockquotePrefix(line)

	// Pop lists nested inside deeper blockquotes that have ended.
	for len(t.stack) > 0 && t.stack[len(t.stack)-1].quoteDepth > quoteDepth {
		t.stack = t.stack[:len(t.stack)-1]
	}

	afterQuote := line[pos:]
	if strings.TrimSpace(afterQuote) == "" {
		for idx := len(t.stack) - 1; idx >= 0; idx-- {
			if t.stack[idx].quoteDepth != quoteDepth {
				break
			}
			if t.stack[idx].emptyOnFirstLine && t.stack[idx].justOpened {
				t.stack = t.stack[:idx]
				continue
			}
			t.stack[idx].sawBlank = true
			t.stack[idx].justOpened = false
		}
		return blockLineInfo{
			openPrefix: line[:pos],
			contPrefix: line[:pos],
			rest:       "",
			quoteDepth: quoteDepth,
			ok:         true,
		}
	}

	col, indentBytes := measureLeadingIndent(afterQuote)
	contentPos := pos + indentBytes
	trimmedRest := strings.TrimRight(line[contentPos:], " \t\r")

	// Pop active list contexts at this quoteDepth whose indentation is not satisfied.
	for len(t.stack) > 0 {
		top := &t.stack[len(t.stack)-1]
		if top.quoteDepth != quoteDepth {
			break
		}
		if col < top.contentIndent && (top.sawBlank || interruptsLazyListContinuation(trimmedRest)) {
			t.stack = t.stack[:len(t.stack)-1]
			continue
		}
		break
	}

	activeIndent := 0
	if len(t.stack) > 0 && t.stack[len(t.stack)-1].quoteDepth == quoteDepth {
		top := &t.stack[len(t.stack)-1]
		activeIndent = top.contentIndent
		top.sawBlank = false
		top.emptyOnFirstLine = false
		top.justOpened = false
	}

	if col > activeIndent+3 {
		return blockLineInfo{ok: false}
	}

	// Check whether this line itself opens a list item (`- ...`, `1. ...`).
	if mLen, isList := parseCommonMarkListMarker(line[contentPos:]); isList {
		afterMarkerStart := contentPos + mLen
		remAfterMarker := line[afterMarkerStart:]
		spCols := 0
		spBytes := 0
		baseCol := col + mLen
		for spBytes < len(remAfterMarker) {
			ch := remAfterMarker[spBytes]
			if ch == ' ' {
				spCols++
				spBytes++
			} else if ch == '\t' {
				adv := 4 - ((baseCol + spCols) % 4)
				spCols += adv
				spBytes++
			} else {
				break
			}
		}
		tailAfterSpaces := strings.TrimRight(remAfterMarker[spBytes:], " \t\r")
		if tailAfterSpaces == "" {
			newIndent := baseCol + 1
			t.stack = append(t.stack, listItemContext{
				quoteDepth:       quoteDepth,
				contentIndent:    newIndent,
				emptyOnFirstLine: true,
				justOpened:       true,
			})
			cont := line[:pos] + strings.Repeat(" ", newIndent)
			return blockLineInfo{
				openPrefix:   line[:afterMarkerStart+spBytes],
				contPrefix:   cont,
				rest:         "",
				quoteDepth:   quoteDepth,
				activeIndent: newIndent,
				stripCols:    newIndent,
				ok:           true,
			}
		}
		if spCols >= 5 {
			// CommonMark: 5+ spaces after a list marker begins an indented code block inside the list item!
			newIndent := baseCol + 1
			t.stack = append(t.stack, listItemContext{
				quoteDepth:    quoteDepth,
				contentIndent: newIndent,
			})
			return blockLineInfo{ok: false}
		}
		newIndent := baseCol + spCols
		t.stack = append(t.stack, listItemContext{
			quoteDepth:    quoteDepth,
			contentIndent: newIndent,
			justOpened:    true,
		})
		cont := line[:pos] + strings.Repeat(" ", newIndent)
		return blockLineInfo{
			openPrefix:   line[:afterMarkerStart+spBytes],
			contPrefix:   cont,
			rest:         tailAfterSpaces,
			quoteDepth:   quoteDepth,
			activeIndent: newIndent,
			stripCols:    newIndent,
			ok:           true,
		}
	}

	return blockLineInfo{
		openPrefix:   line[:contentPos],
		contPrefix:   line[:contentPos],
		rest:         trimmedRest,
		quoteDepth:   quoteDepth,
		activeIndent: activeIndent,
		stripCols:    col,
		ok:           true,
	}
}

func parseChildBlockLine(line string, activeIndent int) (rest string, ok bool) {
	_, pos := scanBlockquotePrefix(line)
	col, indentBytes := measureLeadingIndent(line[pos:])
	if col > activeIndent+3 {
		return "", false
	}
	return strings.TrimRight(line[pos+indentBytes:], " \t\r"), true
}

func stripBlockPrefixCols(line string, quoteDepth int, stripCols int) string {
	pos := 0
	quotesSeen := 0
	for quotesSeen < quoteDepth && pos < len(line) {
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
	if stripCols <= 0 {
		return line[pos:]
	}
	col := 0
	for pos < len(line) && col < stripCols {
		ch := line[pos]
		if ch == ' ' {
			col++
			pos++
		} else if ch == '\t' {
			adv := 4 - (col % 4)
			pos++
			if col+adv > stripCols {
				return strings.Repeat(" ", (col+adv)-stripCols) + line[pos:]
			}
			col += adv
		} else {
			break
		}
	}
	return line[pos:]
}

func parseCustomDirectiveHeader(rest string) (name string, rawArgs string, ok bool) {
	if !strings.HasPrefix(rest, ":::") || len(rest) <= 3 {
		return "", "", false
	}
	c0 := rest[3]
	if !((c0 >= 'a' && c0 <= 'z') || (c0 >= 'A' && c0 <= 'Z')) {
		return "", "", false
	}
	k := 3
	for k < len(rest) {
		ch := rest[k]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			k++
			continue
		}
		break
	}
	dirName := strings.ToLower(rest[3:k])
	rem := rest[k:]
	if rem != "" && rem != ":::" && rem[0] != ' ' && rem[0] != '\t' && rem[0] != '[' {
		return "", "", false
	}
	return dirName, rem, true
}

func scanCustomDirectiveBody(lines []string, openIdx int, info blockLineInfo, openRest string, rawArgs string) (body string, rawMD string, nextIdx int) {
	if strings.HasSuffix(strings.TrimSpace(rawArgs), ":::") {
		return "", openRest, openIdx + 1
	}
	var innerLines []string
	inInnerFence := false
	var innerFenceCh byte
	innerFenceLen := 0
	nestDepth := 0

	for j := openIdx + 1; j < len(lines); j++ {
		cur := lines[j]
		stripped := stripBlockPrefixCols(cur, info.quoteDepth, info.stripCols)
		if curRest, curOk := parseChildBlockLine(cur, info.activeIndent); curOk {
			if fCh, fLen, fOk := parseFenceStart(curRest); fOk {
				if !inInnerFence {
					inInnerFence = true
					innerFenceCh = fCh
					innerFenceLen = fLen
				} else if fCh == innerFenceCh && fLen >= innerFenceLen && strings.TrimSpace(curRest[fLen:]) == "" {
					inInnerFence = false
				}
				innerLines = append(innerLines, stripped)
				continue
			}
			if !inInnerFence {
				if curRest == ":::" {
					if nestDepth > 0 {
						nestDepth--
						innerLines = append(innerLines, stripped)
						continue
					}
					bodyStr := strings.Join(innerLines, "\n")
					if len(innerLines) == 0 {
						return "", openRest + "\n:::", j + 1
					}
					return bodyStr, openRest + "\n" + bodyStr + "\n:::", j + 1
				}
				if isOpeningBlockDirective(curRest) {
					nestDepth++
				}
			}
		}
		innerLines = append(innerLines, stripped)
	}
	// Leaf directive without closing `:::`
	return "", openRest, openIdx + 1
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

func renderLockedPlaceholder(lockType string, hint string, opts Options, directiveB64 string) string {
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
	b.WriteString(`<aside class="volo-lock-card" data-lock-type="` + html.EscapeString(lockType) + `" data-article-id="` + html.EscapeString(opts.ArticleID) + `"`)
	if directiveB64 != "" {
		b.WriteString(` contenteditable="false" data-volo-md-b64="` + directiveB64 + `"`)
	}
	b.WriteByte('>')
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

func bookMonogram(title string) string {
	for _, r := range strings.TrimSpace(title) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return strings.ToUpper(string(r))
		}
	}
	return "B"
}

func writeBookCardInner(b *strings.Builder, info BookCardInfo, locale string) {
	if cover := strings.TrimSpace(info.CoverURL); cover != "" && isSafeImageSrc(cover) {
		b.WriteString(`<span class="volo-embed-book__cover-link" aria-hidden="true">`)
		b.WriteString(`<img class="volo-embed-book__cover" src="` + html.EscapeString(cover) + `" alt="` + html.EscapeString(info.Title) + `" loading="lazy" decoding="async">`)
		b.WriteString(`</span>`)
	} else {
		b.WriteString(`<span class="volo-embed-book__cover volo-embed-book__cover--fallback" aria-hidden="true">`)
		b.WriteString(`<span class="volo-embed-book__spine"></span>`)
		b.WriteString(`<span class="volo-embed-book__monogram">` + html.EscapeString(bookMonogram(info.Title)) + `</span>`)
		b.WriteString(`</span>`)
	}
	b.WriteString(`<span class="volo-embed-book__body">`)
	b.WriteString(`<span class="volo-embed-book__head">`)
	b.WriteString(`<span class="volo-embed-book__title"><strong>` + html.EscapeString(info.Title) + `</strong></span>`)
	if info.ChapterCount > 0 {
		b.WriteString(`<span class="volo-embed-book__chapters">` + html.EscapeString(formatBookChapterBadge(info.ChapterCount, locale)) + `</span>`)
	}
	b.WriteString(`</span>`)
	if desc := strings.TrimSpace(info.Description); desc != "" {
		b.WriteString(`<span class="volo-embed-book__desc">` + html.EscapeString(desc) + `</span>`)
	}
	b.WriteString(`</span>`)
	b.WriteString(`<span class="volo-embed-book__arrow" aria-hidden="true">→</span>`)
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
	bookHref := "/books/" + url.PathEscape(info.Slug)
	var b strings.Builder
	b.WriteString(`<a class="volo-embed-book" href="` + bookHref + `" data-book-slug="` + html.EscapeString(info.Slug) + `">`)
	writeBookCardInner(&b, info, opts.Locale)
	b.WriteString(`</a>`)
	return b.String()
}

func renderBookshelfGrid(opts Options, limit int, directiveB64 string) string {
	var books []BookCardInfo
	if opts.ListBooks != nil {
		books = opts.ListBooks()
	}
	if limit > 0 && len(books) > limit {
		books = books[:limit]
	}
	extraAttrs := ""
	if directiveB64 != "" {
		extraAttrs = ` contenteditable="false" data-volo-md-b64="` + directiveB64 + `"`
	}
	if len(books) == 0 {
		emptyMsg := "暂无已发布的在线书籍"
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(opts.Locale)), "en") {
			emptyMsg = "No online books published yet."
		}
		return `<section class="volo-embed-bookshelf volo-embed-bookshelf--empty"` + extraAttrs + `><p class="volo-embed-bookshelf__empty">` + html.EscapeString(emptyMsg) + `</p></section>`
	}
	var b strings.Builder
	b.WriteString(`<section class="volo-embed-bookshelf"` + extraAttrs + `>`)
	for _, bk := range books {
		bookHref := "/books/" + url.PathEscape(bk.Slug)
		b.WriteString(`<a class="volo-book-tile" href="` + bookHref + `" data-book-slug="` + html.EscapeString(bk.Slug) + `">`)
		writeBookCardInner(&b, bk, opts.Locale)
		b.WriteString(`</a>`)
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
	// Walk HTML tags so we NEVER rewrite timestamps inside <code>, <pre>, <a>, <button>, math containers, or HTML attributes!
	var out strings.Builder
	out.Grow(len(htmlSrc) + 64)
	skipDepth := 0
	mathSpanDepth := 0
	mathDivDepth := 0
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
			if strings.HasPrefix(lowerTag, "<span") && strings.Contains(lowerTag, "volo-math-inline") {
				mathSpanDepth++
			} else if strings.HasPrefix(lowerTag, "</span") && mathSpanDepth > 0 {
				mathSpanDepth--
			} else if strings.HasPrefix(lowerTag, "<div") && strings.Contains(lowerTag, "volo-math-block") {
				mathDivDepth++
			} else if strings.HasPrefix(lowerTag, "</div") && mathDivDepth > 0 {
				mathDivDepth--
			} else if strings.HasPrefix(lowerTag, "<code") || strings.HasPrefix(lowerTag, "<pre") || strings.HasPrefix(lowerTag, "<a ") || lowerTag == "<a>" || strings.HasPrefix(lowerTag, "<button") {
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
		if skipDepth > 0 || mathSpanDepth > 0 || mathDivDepth > 0 || !strings.Contains(textSeg, "[") {
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

func countPrecedingBackslashes(s string, pos int) int {
	n := 0
	for pos-1-n >= 0 && s[pos-1-n] == '\\' {
		n++
	}
	return n
}

func findUnescapedDelim(s string, delim string) int {
	offset := 0
	for offset <= len(s)-len(delim) {
		idx := strings.Index(s[offset:], delim)
		if idx < 0 {
			return -1
		}
		absIdx := offset + idx
		if countPrecedingBackslashes(s, absIdx)%2 == 0 {
			return absIdx
		}
		offset = absIdx + 1
	}
	return -1
}

// tryExtractBlockMath extracts standalone single-line or multi-line `$$...$$` and `\[...\]` display math blocks.
func tryExtractBlockMath(lines []string, startIdx int, info blockLineInfo, rest string) (body string, nextIdx int, ok bool) {
	var openDelim, closeDelim string
	if strings.HasPrefix(rest, "$$") {
		openDelim, closeDelim = "$$", "$$"
	} else if strings.HasPrefix(rest, `\[`) {
		openDelim, closeDelim = `\[`, `\]`
	} else {
		return "", 0, false
	}

	firstAfterOpen := rest[len(openDelim):]
	if cIdx := findUnescapedDelim(firstAfterOpen, closeDelim); cIdx >= 0 {
		if strings.TrimSpace(firstAfterOpen[cIdx+len(closeDelim):]) == "" {
			return firstAfterOpen[:cIdx], startIdx + 1, true
		}
		return "", 0, false
	}

	var middleLines []string
	for j := startIdx + 1; j < len(lines); j++ {
		if jRest, jOk := parseChildBlockLine(lines[j], info.activeIndent); jOk {
			if _, _, fOk := parseFenceStart(jRest); fOk {
				return "", 0, false
			}
			if strings.HasPrefix(jRest, ":::") {
				return "", 0, false
			}
		}
		stripped := strings.TrimRight(stripBlockPrefixCols(lines[j], info.quoteDepth, info.stripCols), "\r")
		if endPos := findUnescapedDelim(stripped, closeDelim); endPos >= 0 {
			if strings.TrimSpace(stripped[endPos+len(closeDelim):]) == "" {
				var parts []string
				if strings.TrimSpace(firstAfterOpen) != "" {
					parts = append(parts, firstAfterOpen)
				}
				parts = append(parts, middleLines...)
				if strings.TrimSpace(stripped[:endPos]) != "" {
					parts = append(parts, stripped[:endPos])
				}
				return strings.Join(parts, "\n"), j + 1, true
			}
		}
		middleLines = append(middleLines, stripped)
	}
	return "", 0, false
}

func isASCIIWordByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isInlineSpaceByte(b byte) bool {
	return b == ' ' || b == '\t'
}

// isReferenceLinkDefLine reports whether `line` is a CommonMark link reference definition (`[id]: url ...`),
// excluding GFM footnote definitions (`[^1]: ...`), wiki-links (`[[...]]`), and GitHub alerts (`[!NOTE]`).
func isReferenceLinkDefLine(line string) bool {
	_, pos := scanBlockquotePrefix(line)
	col, indentBytes := measureLeadingIndent(line[pos:])
	if col > 3 {
		return false
	}
	s := line[pos+indentBytes:]
	if len(s) < 4 || s[0] != '[' || s[1] == '^' || s[1] == '[' || s[1] == '!' {
		return false
	}
	closeIdx := -1
	for k := 1; k < len(s); k++ {
		if s[k] == '\\' && k+1 < len(s) {
			k++
			continue
		}
		if s[k] == '[' {
			return false
		}
		if s[k] == ']' {
			closeIdx = k
			break
		}
	}
	if closeIdx <= 1 || closeIdx+1 >= len(s) || s[closeIdx+1] != ':' {
		return false
	}
	dest := strings.TrimSpace(s[closeIdx+2:])
	return dest != ""
}

// scanMarkdownLinkDestEnd scans `](...)` starting at `bracketPos` (where `line[bracketPos] == ']'`
// and `line[bracketPos+1] == '('`), honoring balanced parentheses, backslash escapes, `<...>` destinations,
// and optional quoted link titles. Returns the index immediately after the closing `)`, or -1 if unclosed.
func scanMarkdownLinkDestEnd(line string, bracketPos int) int {
	if bracketPos+1 >= len(line) || line[bracketPos] != ']' || line[bracketPos+1] != '(' {
		return -1
	}
	if countPrecedingBackslashes(line, bracketPos)%2 == 1 {
		return -1
	}
	q := bracketPos + 2
	for q < len(line) && isInlineSpaceByte(line[q]) {
		q++
	}
	if q < len(line) && line[q] == '<' {
		q++
		for q < len(line) && line[q] != '>' {
			if line[q] == '\\' && q+1 < len(line) {
				q += 2
				continue
			}
			if line[q] == '<' {
				return -1
			}
			q++
		}
		if q >= len(line) {
			return -1
		}
		q++ // past '>'
	}
	depth := 1
	for q < len(line) && depth > 0 {
		ch := line[q]
		if ch == '\\' && q+1 < len(line) {
			q += 2
			continue
		}
		if (ch == '"' || ch == '\'') && q > bracketPos+2 && isInlineSpaceByte(line[q-1]) {
			quote := ch
			q++
			for q < len(line) && line[q] != quote {
				if line[q] == '\\' && q+1 < len(line) {
					q += 2
					continue
				}
				q++
			}
			if q < len(line) {
				q++
			}
			continue
		}
		if ch == '(' {
			depth++
		} else if ch == ')' {
			depth--
		}
		q++
	}
	if depth == 0 {
		return q
	}
	return -1
}

func isMathStartAfterCurrencyDollar(b byte) bool {
	switch b {
	case '\\', '{', '(', '[', '-', '+', '|':
		return true
	default:
		return false
	}
}

// findClosingBacktickRun locates the matching CommonMark closing backtick run of exact length `tickLen`
// starting at `searchStart`, or returns -1 if unmatched.
func findClosingBacktickRun(line string, searchStart int, tickLen int) int {
	for k := searchStart; k < len(line); {
		if line[k] != '`' {
			k++
			continue
		}
		run := 0
		for k+run < len(line) && line[k+run] == '`' {
			run++
		}
		if run == tickLen {
			return k
		}
		k += run
	}
	return -1
}

// extractLineMath scans a single Markdown line outside CommonMark inline code spans, raw HTML tags/comments,
// Markdown link destinations `](...)`, and URLs, replacing display math (`$$...$$`, `\[...\]`) and inline math
// (`\(...\)`, `$...$`) with safe alphanumeric placeholders while preserving currency (`$100 and $200`, `预算$100，公式$\alpha$`).
func extractLineMath(line string, nextToken func(string) string, replacements map[string]string) (string, bool) {
	if !strings.Contains(line, "$") && !strings.Contains(line, `\(`) && !strings.Contains(line, `\[`) {
		return line, false
	}
	if isReferenceLinkDefLine(line) {
		return line, false
	}

	var out strings.Builder
	out.Grow(len(line))
	foundMath := false

	for p := 0; p < len(line); {
		ch := line[p]

		// 1. CommonMark inline backtick code spans (exact run-length matching).
		if ch == '`' {
			tickLen := 0
			for p+tickLen < len(line) && line[p+tickLen] == '`' {
				tickLen++
			}
			if closeIdx := findClosingBacktickRun(line, p+tickLen, tickLen); closeIdx >= 0 {
				out.WriteString(line[p : closeIdx+tickLen])
				p = closeIdx + tickLen
				continue
			}
			out.WriteString(line[p : p+tickLen])
			p += tickLen
			continue
		}

		// 2. Raw HTML tags (`<div attr="a > b $1 $2">`), HTML comments (`<!-- ... -->`), and inline `<pre>...</pre>`.
		if ch == '<' && p+1 < len(line) {
			if tagEnd := scanHTMLTagOrCommentEnd(line, p); tagEnd > p {
				lowerTag := strings.ToLower(line[p:tagEnd])
				if hasHTMLTagOpen(lowerTag, "pre") && !strings.HasSuffix(strings.TrimSpace(lowerTag), "/>") {
					if closeRel := findCaseInsensitivePreCloseEnd(line[tagEnd:]); closeRel >= 0 {
						endPre := tagEnd + closeRel
						out.WriteString(line[p:endPre])
						p = endPre
						continue
					}
				}
				out.WriteString(line[p:tagEnd])
				p = tagEnd
				continue
			}
		}

		// 2b. Markdown inline link/image destination `](...)` with balanced parentheses.
		if ch == ']' && p+1 < len(line) && line[p+1] == '(' {
			if destEnd := scanMarkdownLinkDestEnd(line, p); destEnd > p {
				out.WriteString(line[p:destEnd])
				p = destEnd
				continue
			}
		}

		// 2c. Bare URLs and paths (`https://...`, `http://...`, `www....`, custom `scheme://...`).
		if urlEnd := skipURLOrPathToken(line, p); urlEnd > p {
			out.WriteString(line[p:urlEnd])
			p = urlEnd
			continue
		}

		// 3. Backslash sequences (`\[...\]`, `\(...\)`, `\$`, `\\`, etc.).
		if ch == '\\' && p+1 < len(line) {
			next := line[p+1]
			if next == '[' {
				if endRel := findUnescapedDelim(line[p+2:], `\]`); endRel >= 0 {
					body := line[p+2 : p+2+endRel]
					tok := nextToken("MATHBLOCK")
					replacements[tok] = `<div class="volo-math-block">$$` + html.EscapeString(body) + `$$</div>`
					out.WriteString(tok)
					foundMath = true
					p += 2 + endRel + 2
					continue
				}
			} else if next == '(' {
				if endRel := findUnescapedDelim(line[p+2:], `\)`); endRel >= 0 {
					body := line[p+2 : p+2+endRel]
					tok := nextToken("MATHINLINE")
					replacements[tok] = `<span class="volo-math-inline">$` + html.EscapeString(body) + `$</span>`
					out.WriteString(tok)
					foundMath = true
					p += 2 + endRel + 2
					continue
				}
			}
			out.WriteString(line[p : p+2])
			p += 2
			continue
		}

		// 4. Unescaped `$` or `$$`.
		if ch == '$' {
			if p+1 < len(line) && line[p+1] == '$' {
				if endRel := findUnescapedDelim(line[p+2:], "$$"); endRel >= 0 {
					body := line[p+2 : p+2+endRel]
					tok := nextToken("MATHBLOCK")
					replacements[tok] = `<div class="volo-math-block">$$` + html.EscapeString(body) + `$$</div>`
					out.WriteString(tok)
					foundMath = true
					p += 2 + endRel + 2
					continue
				}
				// Unclosed `$$` run: emit literally.
				run := 2
				for p+run < len(line) && line[p+run] == '$' {
					run++
				}
				out.WriteString(line[p : p+run])
				p += run
				continue
			}

			// Single `$` inline math:
			// - Must not be preceded by ASCII word byte (e.g., `US$50`)
			// - Must not start with unescaped whitespace
			// - Scan to next unescaped `$` (never crossing `` ` `` or `$$`):
			//   - Must not end with unescaped whitespace
			//   - Must not be immediately followed by an ASCII digit/word byte or `$`
			//   - Currency-bridging guard: when opening `$` starts with digits (`$100`) and the next `$`
			//     is followed by `\`, `{`, `(`, `[`, `-`, `+`, `|` (`预算$100，公式$\alpha$`), treat `$100` as currency!
			if (p == 0 || !isASCIIWordByte(line[p-1])) && p+1 < len(line) && !isInlineSpaceByte(line[p+1]) {
				opensWithDigit := line[p+1] >= '0' && line[p+1] <= '9'
				closeIdx := -1
				for q := p + 1; q < len(line); q++ {
					if line[q] == '`' {
						break
					}
					if line[q] == '$' && countPrecedingBackslashes(line[p+1:], q-(p+1))%2 == 0 {
						if q+1 < len(line) && line[q+1] == '$' {
							break
						}
						prevUnescapedSpace := isInlineSpaceByte(line[q-1]) && countPrecedingBackslashes(line[p+1:], (q-1)-(p+1))%2 == 0
						nextWordOrDollar := q+1 < len(line) && (isASCIIWordByte(line[q+1]) || line[q+1] == '$')
						currencyBridge := opensWithDigit && q+1 < len(line) && isMathStartAfterCurrencyDollar(line[q+1])
						if !prevUnescapedSpace && !nextWordOrDollar && !currencyBridge {
							closeIdx = q
						}
						// Always stop at the first unescaped `$` so failed currency `$` never bridges into later math.
						break
					}
				}
				if closeIdx > p+1 {
					body := line[p+1 : closeIdx]
					tok := nextToken("MATHINLINE")
					replacements[tok] = `<span class="volo-math-inline">$` + html.EscapeString(body) + `$</span>`
					out.WriteString(tok)
					foundMath = true
					p = closeIdx + 1
					continue
				}
			}
		}

		out.WriteByte(ch)
		p++
	}

	return out.String(), foundMath
}

func stripInlineCodeSpans(line string) string {
	var b strings.Builder
	for p := 0; p < len(line); {
		if line[p] == '`' {
			tickLen := 0
			for p+tickLen < len(line) && line[p+tickLen] == '`' {
				tickLen++
			}
			if closeIdx := findClosingBacktickRun(line, p+tickLen, tickLen); closeIdx >= 0 {
				p = closeIdx + tickLen
				continue
			}
			b.WriteString(line[p : p+tickLen])
			p += tickLen
			continue
		}
		b.WriteByte(line[p])
		p++
	}
	return b.String()
}

var wikiLinkRe = regexp.MustCompile(`\[\[([a-zA-Z0-9._-]+)(?:\|([^\]]+))?\]\]`)

func rewriteWikiLinksOutsideCode(line string) string {
	if !strings.Contains(line, "[[") {
		return line
	}
	if isReferenceLinkDefLine(line) {
		return line
	}
	var out strings.Builder
	out.Grow(len(line))
	lastPlain := 0
	flushPlain := func(end int) {
		if end <= lastPlain {
			return
		}
		seg := line[lastPlain:end]
		out.WriteString(wikiLinkRe.ReplaceAllStringFunc(seg, func(m string) string {
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
		}))
	}
	for p := 0; p < len(line); {
		if line[p] == '`' {
			tickLen := 0
			for p+tickLen < len(line) && line[p+tickLen] == '`' {
				tickLen++
			}
			if closeIdx := findClosingBacktickRun(line, p+tickLen, tickLen); closeIdx >= 0 {
				flushPlain(p)
				out.WriteString(line[p : closeIdx+tickLen])
				p = closeIdx + tickLen
				lastPlain = p
				continue
			}
			p += tickLen
			continue
		}
		if line[p] == '<' && p+1 < len(line) {
			if tagEnd := scanHTMLTagOrCommentEnd(line, p); tagEnd > p {
				flushPlain(p)
				lowerTag := strings.ToLower(line[p:tagEnd])
				if hasHTMLTagOpen(lowerTag, "pre") && !strings.HasSuffix(strings.TrimSpace(lowerTag), "/>") {
					if closeRel := findCaseInsensitivePreCloseEnd(line[tagEnd:]); closeRel >= 0 {
						endPre := tagEnd + closeRel
						out.WriteString(line[p:endPre])
						p = endPre
						lastPlain = p
						continue
					}
				}
				out.WriteString(line[p:tagEnd])
				p = tagEnd
				lastPlain = p
				continue
			}
		}
		if line[p] == ']' && p+1 < len(line) && line[p+1] == '(' {
			if destEnd := scanMarkdownLinkDestEnd(line, p); destEnd > p {
				flushPlain(p)
				out.WriteString(line[p:destEnd])
				p = destEnd
				lastPlain = p
				continue
			}
		}
		p++
	}
	flushPlain(len(line))
	return out.String()
}

func isGoldmarkLinkifiedURL(tok string) bool {
	if len(tok) >= 4 && strings.EqualFold(tok[:4], "www.") {
		return true
	}
	colon := strings.IndexByte(tok, ':')
	if colon <= 0 {
		return false
	}
	switch strings.ToLower(tok[:colon]) {
	case "http", "https", "ftp", "mailto":
		return true
	default:
		return false
	}
}

func writeTildeProtectedToken(out *strings.Builder, tok string) {
	if isGoldmarkLinkifiedURL(tok) || !strings.Contains(tok, "~") {
		out.WriteString(tok)
		return
	}
	// Escape `~` as `&#126;` in plain-text file paths (`/~user/file~1~2`, `~/dir/file~1~`) and custom
	// deep-link URLs (`huanai://open/~u/a~1~`) so Goldmark's GFM Strikethrough extension does not
	// rewrite `~user/file~` into `<del>user/file</del>` before HTML rendering!
	for i := 0; i < len(tok); i++ {
		if tok[i] == '~' {
			out.WriteString("&#126;")
		} else {
			out.WriteByte(tok[i])
		}
	}
}

func rewriteSubscriptOutsideCode(line string) string {
	if !strings.Contains(line, "~") {
		return line
	}
	if isReferenceLinkDefLine(line) {
		return line
	}
	var out strings.Builder
	out.Grow(len(line) + 16)
	for p := 0; p < len(line); {
		if line[p] == '`' {
			tickLen := 0
			for p+tickLen < len(line) && line[p+tickLen] == '`' {
				tickLen++
			}
			if closeIdx := findClosingBacktickRun(line, p+tickLen, tickLen); closeIdx >= 0 {
				out.WriteString(line[p : closeIdx+tickLen])
				p = closeIdx + tickLen
				continue
			}
			out.WriteString(line[p : p+tickLen])
			p += tickLen
			continue
		}
		if line[p] == '<' && p+1 < len(line) {
			if tagEnd := scanHTMLTagOrCommentEnd(line, p); tagEnd > p {
				lowerTag := strings.ToLower(line[p:tagEnd])
				if hasHTMLTagOpen(lowerTag, "pre") && !strings.HasSuffix(strings.TrimSpace(lowerTag), "/>") {
					if closeRel := findCaseInsensitivePreCloseEnd(line[tagEnd:]); closeRel >= 0 {
						endPre := tagEnd + closeRel
						out.WriteString(line[p:endPre])
						p = endPre
						continue
					}
				}
				out.WriteString(line[p:tagEnd])
				p = tagEnd
				continue
			}
		}
		if line[p] == ']' && p+1 < len(line) && line[p+1] == '(' {
			if destEnd := scanMarkdownLinkDestEnd(line, p); destEnd > p {
				out.WriteString(line[p:destEnd])
				p = destEnd
				continue
			}
		}
		if urlOrPathEnd := skipURLOrPathToken(line, p); urlOrPathEnd > p {
			writeTildeProtectedToken(&out, line[p:urlOrPathEnd])
			p = urlOrPathEnd
			continue
		}
		if line[p] == '~' && p > 0 && line[p-1] == '/' && (p+1 == len(line) || line[p+1] != '~') {
			out.WriteString("&#126;")
			p++
			continue
		}
		if p+2 < len(line) && line[p] == '~' &&
			(p == 0 || (line[p-1] != '~' && line[p-1] != '\\' && line[p-1] != '/')) &&
			line[p+1] != '~' && line[p+1] != '/' && !isASCIIWhitespace(line[p+1]) {
			limit := len(line)
			if p+48 < limit {
				limit = p + 48
			}
			closed := -1
			for q := p + 1; q < limit; q++ {
				ch := line[q]
				if isASCIIWhitespace(ch) || ch == '<' || ch == '>' || ch == '`' || ch == '[' || ch == ']' || ch == '/' {
					break
				}
				if ch == '~' {
					if q > p+1 && line[q-1] != '\\' && (q+1 == len(line) || line[q+1] != '~') {
						closed = q
					}
					break
				}
			}
			if closed > p+1 {
				out.WriteString("<sub>")
				out.WriteString(html.EscapeString(line[p+1 : closed]))
				out.WriteString("</sub>")
				p = closed + 1
				continue
			}
		}
		out.WriteByte(line[p])
		p++
	}
	return out.String()
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
// outside fenced code blocks while honoring CommonMark indentation, blockquotes, list items, and nested directives.
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

	var tracker blockLineTracker
	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceIndent := 0
	inLock := false
	lockNestDepth := 0
	lockIndent := 0

	for _, line := range lines {
		if inFence {
			if rest, ok := parseChildBlockLine(line, fenceIndent); ok {
				if ch, n, fOk := parseFenceStart(rest); fOk && ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
					inFence = false
				}
			}
			if !inLock {
				out = append(out, line)
			}
			continue
		}
		if inLock {
			if rest, ok := parseChildBlockLine(line, lockIndent); ok {
				if ch, n, fOk := parseFenceStart(rest); fOk {
					inFence = true
					fenceChar = ch
					fenceLen = n
					fenceIndent = lockIndent
					continue
				}
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
			}
			continue
		}

		info := tracker.next(line)
		if info.ok {
			if ch, n, ok := parseFenceStart(info.rest); ok {
				inFence = true
				fenceChar = ch
				fenceLen = n
				fenceIndent = info.activeIndent
				out = append(out, line)
				continue
			}
			if isDirectivePrefix(info.rest, ":::lock") {
				inLock = true
				lockNestDepth = 0
				lockIndent = info.activeIndent
				continue
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// StripAllCodeAndLockBlocks removes both fenced code blocks and `:::lock` blocks (including unclosed locks).
func StripAllCodeAndLockBlocks(src string) string {
	noLock := StripAllLockBlocks(src)
	lines := strings.Split(strings.ReplaceAll(noLock, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	var tracker blockLineTracker
	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceIndent := 0
	for _, line := range lines {
		if inFence {
			if rest, ok := parseChildBlockLine(line, fenceIndent); ok {
				if ch, n, fOk := parseFenceStart(rest); fOk && ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
					inFence = false
				}
			}
			continue
		}
		info := tracker.next(line)
		if info.ok {
			if ch, n, ok := parseFenceStart(info.rest); ok {
				inFence = true
				fenceChar = ch
				fenceLen = n
				fenceIndent = info.activeIndent
				continue
			}
		}
		out = append(out, stripInlineCodeSpans(line))
	}
	return strings.Join(out, "\n")
}

// ExtractAllLockPasswords scans Markdown outside fenced code blocks for all unique `:::lock ... pwd="..."`
// (or `password="..."`) attributes in order of appearance.
func ExtractAllLockPasswords(src string) []string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var tracker blockLineTracker
	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceIndent := 0
	var out []string
	seen := make(map[string]struct{})
	for _, line := range lines {
		if inFence {
			if rest, ok := parseChildBlockLine(line, fenceIndent); ok {
				if ch, n, fOk := parseFenceStart(rest); fOk && ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
					inFence = false
				}
			}
			continue
		}
		info := tracker.next(line)
		if !info.ok {
			continue
		}
		if ch, n, ok := parseFenceStart(info.rest); ok {
			inFence = true
			fenceChar = ch
			fenceLen = n
			fenceIndent = info.activeIndent
			continue
		}
		if isDirectivePrefix(info.rest, ":::lock") {
			attrs := parseDirectiveAttrs(info.rest[len(":::lock"):])
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
	var tracker blockLineTracker
	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceIndent := 0
	for _, line := range lines {
		if inFence {
			if rest, ok := parseChildBlockLine(line, fenceIndent); ok {
				if ch, n, fOk := parseFenceStart(rest); fOk && ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest[n:]) == "" {
					inFence = false
				}
			}
			continue
		}
		info := tracker.next(line)
		if !info.ok {
			continue
		}
		if ch, n, ok := parseFenceStart(info.rest); ok {
			inFence = true
			fenceChar = ch
			fenceLen = n
			fenceIndent = info.activeIndent
			continue
		}
		if isDirectivePrefix(info.rest, ":::lock") {
			return true
		}
	}
	return false
}
