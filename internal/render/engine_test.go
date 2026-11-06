package render

import (
	"strings"
	"testing"
)

func TestEngineRenderFeaturesXSSAndFailClosedLocks(t *testing.T) {
	eng := NewEngine()

	md := strings.Join([]string{
		"# Architecture Overview",
		"",
		"Listen at [03:25] for the WAL benchmark and see [[go-sqlite-wal]].",
		"",
		"![Diagram](/uploads/arch.png)",
		"",
		"```go",
		"package main",
		"// :::lock inside a code fence MUST NOT be treated as a lock directive!",
		"const secretInCodeSample = \"VISIBLE_CODE_LITERAL\"",
		"```",
		"",
		"```mermaid",
		"graph LR; A-->B;",
		"```",
		"",
		"Inline math $E = mc^2$ and raw XSS <script>alert(1)</script><img src=x onerror=alert(2)>.",
		"",
		":::lock type=\"pwd\" pwd=\"topsecret\" hint=\"Passcode needed\"",
		"HIDDEN_LOCKED_PARAGRAPH_1",
		":::",
		"",
		":::lock type=\"user\"",
		"READER_ONLY_PARAGRAPH_2",
		":::",
		"",
		":::lock type=\"pwd\"",
		"UNCLOSED_EOF_SECRET_3",
	}, "\n")

	// 1. Anonymous un-unlocked render
	outAnon := eng.Render(md, Options{
		Mode:     RenderModeWeb,
		Locale:   "en-US",
		HasAudio: true,
		LookupMediaDims: func(src string) (int, int, bool) {
			if src == "/uploads/arch.png" {
				return 1280, 720, true
			}
			return 0, 0, false
		},
	})

	if !outAnon.HasLockBlock {
		t.Fatalf("expected HasLockBlock=true")
	}
	if !outAnon.NeedsMermaid || !outAnon.NeedsMath {
		t.Fatalf("expected NeedsMermaid=true and NeedsMath=true, got mermaid=%v math=%v", outAnon.NeedsMermaid, outAnon.NeedsMath)
	}
	if !strings.Contains(outAnon.HTML, "VISIBLE_CODE_LITERAL") {
		t.Fatalf("fenced code sample should remain visible: %s", outAnon.HTML)
	}
	for _, secret := range []string{"HIDDEN_LOCKED_PARAGRAPH_1", "READER_ONLY_PARAGRAPH_2", "UNCLOSED_EOF_SECRET_3"} {
		if strings.Contains(outAnon.HTML, secret) || strings.Contains(outAnon.PlainText, secret) {
			t.Fatalf("secret %q leaked into un-unlocked render!", secret)
		}
	}
	if strings.Contains(outAnon.HTML, "<script>") || strings.Contains(outAnon.HTML, "onerror") {
		t.Fatalf("XSS payload was not stripped by bluemonday: %s", outAnon.HTML)
	}
	if !strings.Contains(outAnon.HTML, `data-audio-seek="205"`) {
		t.Fatalf("expected [03:25] -> data-audio-seek=\"205\" button in HTML: %s", outAnon.HTML)
	}
	if !strings.Contains(outAnon.HTML, `width="1280"`) || !strings.Contains(outAnon.HTML, `height="720"`) {
		t.Fatalf("expected intrinsic width/height injected onto image: %s", outAnon.HTML)
	}
	if len(outAnon.TOC) == 0 || outAnon.TOC[0].Text != "Architecture Overview" {
		t.Fatalf("expected heading TOC extracted, got %+v", outAnon.TOC)
	}

	// 2. Signed-in Reader unlocks `type="user"` ONLY, while `type="pwd"` and unclosed EOF remain locked!
	outReader := eng.Render(md, Options{
		Mode:             RenderModeWeb,
		IsReaderLoggedIn: true,
	})
	if !strings.Contains(outReader.HTML, "READER_ONLY_PARAGRAPH_2") {
		t.Fatalf("expected signed-in reader to see type=user block")
	}
	if strings.Contains(outReader.HTML, "HIDDEN_LOCKED_PARAGRAPH_1") || strings.Contains(outReader.HTML, "UNCLOSED_EOF_SECRET_3") {
		t.Fatalf("reader without password must NOT see type=pwd blocks")
	}

	// 3. Password-unlocked Web render reveals locked blocks, whereas Feed mode NEVER reveals them even if Unlocked=true!
	outUnlockedWeb := eng.Render(md, Options{Mode: RenderModeWeb, Unlocked: true})
	if !strings.Contains(outUnlockedWeb.HTML, "HIDDEN_LOCKED_PARAGRAPH_1") || !strings.Contains(outUnlockedWeb.HTML, "UNCLOSED_EOF_SECRET_3") {
		t.Fatalf("unlocked web mode should reveal locked blocks")
	}
	outFeed := eng.Render(md, Options{Mode: RenderModeFeed, Unlocked: true})
	if strings.Contains(outFeed.HTML, "HIDDEN_LOCKED_PARAGRAPH_1") || strings.Contains(outFeed.HTML, "UNCLOSED_EOF_SECRET_3") {
		t.Fatalf("RenderModeFeed must NEVER emit locked content even if Unlocked=true!")
	}

	// 4. Outbound post slug extraction ignores links inside locked blocks and code fences!
	slugs := ExtractOutboundPostSlugs(md + "\n\n:::lock\n[[secret-outbound-target]]\n:::\n")
	if len(slugs) != 1 || slugs[0] != "go-sqlite-wal" {
		t.Fatalf("expected only public outbound slug 'go-sqlite-wal', got %v", slugs)
	}
	if gotPwd := ExtractFirstLockPassword(md); gotPwd != "topsecret" {
		t.Fatalf("expected ExtractFirstLockPassword='topsecret', got %q", gotPwd)
	}
}
