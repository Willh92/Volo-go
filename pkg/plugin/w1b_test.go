package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderPreMarkdownSummaryPostHTMLAndStateHash(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		Catalog:    cat,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	defer func() { _ = mgr.Close(ctx) }()

	if h := mgr.RenderHooksStateHash(); h != "" {
		t.Fatalf("expected empty RenderHooksStateHash with 0 render plugins, got %q", h)
	}

	// Plugin 1: updates both content_md and summary_md in render.pre_markdown, and returns plain string in render.post_html.
	zip1 := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "render-alpha",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Render Alpha"},
			"permissions": ["hooks:render.pre_markdown", "hooks:render.post_html"],
			"config_schema": [
				{"key": "badge", "type": "string", "default": "v1", "label": {"en-US": "Badge"}}
			]
		}`,
		"main.js": `
			volo.on("render.pre_markdown", function(p) {
				var badge = volo.config.get("badge") || "";
				return {
					content_md: p.content_md + "\n\n[" + badge + "]",
					summary_md: "[" + badge + "] " + p.summary_md
				};
			});
			volo.on("render.post_html", function(p) {
				return "<section class=\"alpha\">" + p.html + "</section>";
			});
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zip1), int64(len(zip1)), true, "en-US"); err != nil {
		t.Fatalf("install render-alpha: %v", err)
	}

	// Still disabled -> hash must be "".
	if h := mgr.RenderHooksStateHash(); h != "" {
		t.Fatalf("expected empty hash while render-alpha is disabled, got %q", h)
	}

	if err := mgr.Enable(ctx, "render-alpha"); err != nil {
		t.Fatalf("enable render-alpha: %v", err)
	}
	hash1 := mgr.RenderHooksStateHash()
	if len(hash1) != 64 {
		t.Fatalf("expected 64-char SHA-256 hex RenderHooksStateHash, got %q", hash1)
	}

	// Plugin 2: returns { html: "..." } object in render.post_html.
	zip2 := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "render-beta",
			"version": "1.2.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Render Beta"},
			"permissions": ["hooks:render.post_html"]
		}`,
		"main.js": `
			exports.hooks = {
				"render.post_html": function(p) {
					return { html: p.html + "<footer class=\"beta\">ok</footer>" };
				}
			};
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zip2), int64(len(zip2)), true, "en-US"); err != nil {
		t.Fatalf("install render-beta: %v", err)
	}
	if err := mgr.Enable(ctx, "render-beta"); err != nil {
		t.Fatalf("enable render-beta: %v", err)
	}

	hash2 := mgr.RenderHooksStateHash()
	if hash2 == "" || hash2 == hash1 {
		t.Fatalf("expected hash to change when second render plugin enabled: %q vs %q", hash1, hash2)
	}

	in := ArticleHookPayload{
		ID:        "art-1",
		ContentMD: "Hello body",
		SummaryMD: "Short intro",
		HTML:      "<p>Hello body</p>",
	}
	contentMD, summaryMD := mgr.DispatchRenderPreMarkdownFull(ctx, in)
	if contentMD != "Hello body\n\n[v1]" {
		t.Fatalf("unexpected content_md: %q", contentMD)
	}
	if summaryMD != "[v1] Short intro" {
		t.Fatalf("expected summary_md readback, got %q", summaryMD)
	}
	if compatMD := mgr.DispatchRenderPreMarkdown(ctx, in); compatMD != contentMD {
		t.Fatalf("DispatchRenderPreMarkdown mismatch: %q vs %q", compatMD, contentMD)
	}

	postHTML := mgr.DispatchRenderPostHTML(ctx, in)
	wantHTML := `<section class="alpha"><p>Hello body</p></section><footer class="beta">ok</footer>`
	if postHTML != wantHTML {
		t.Fatalf("unexpected postHTML:\n got: %q\nwant: %q", postHTML, wantHTML)
	}

	// Updating render plugin config must change RenderHooksStateHash.
	if _, err := mgr.UpdateConfig(ctx, "render-alpha", map[string]any{"badge": "v2"}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	hash3 := mgr.RenderHooksStateHash()
	if hash3 == "" || hash3 == hash2 {
		t.Fatalf("expected hash to update after config change: %q vs %q", hash2, hash3)
	}

	// Disabling all render plugins returns RenderHooksStateHash back to "".
	_ = mgr.Disable(ctx, "render-alpha")
	_ = mgr.Disable(ctx, "render-beta")
	if h := mgr.RenderHooksStateHash(); h != "" {
		t.Fatalf("expected empty hash when all render plugins disabled, got %q", h)
	}
}

func TestSecretFieldTypeSealingRedactionAndRuntimeUnseal(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Catalog:    cat,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	mgr.SetSecretSealer(kr.SecretBox())
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "secret-probe",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Secret Probe"},
			"permissions": ["http:routes"],
			"config_schema": [
				{"key": "endpoint", "type": "string", "default": "https://api.example.com", "label": {"en-US": "Endpoint"}},
				{"key": "api_secret", "type": "secret", "label": {"en-US": "API Secret"}}
			]
		}`,
		"main.js": `
			exports.onRequest = function(req) {
				var allCfg = volo.config.all();
				return {
					status: 200,
					body: JSON.stringify({
						direct: volo.config.get("api_secret"),
						fromAll: allCfg.api_secret,
						endpoint: volo.config.get("endpoint")
					})
				};
			};
		`,
	}, "")

	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("install secret-probe: %v", err)
	}
	if err := mgr.Enable(ctx, "secret-probe"); err != nil {
		t.Fatalf("enable secret-probe: %v", err)
	}

	// Save plaintext secret -> sealed with v1:... via SecretSealer, redacted to "********" for Admin UI.
	sum1, err := mgr.SaveConfig(ctx, "secret-probe", map[string]any{
		"endpoint":   "https://one.example.com",
		"api_secret": "top-secret-live-token-99",
	}, "en-US")
	if err != nil {
		t.Fatalf("SaveConfig 1: %v", err)
	}
	if _, leaked := sum1.PublicConfig["api_secret"]; leaked {
		t.Fatal("secret field must not appear in PublicConfig")
	}
	if sum1.Config["api_secret"] != SecretMaskSentinel {
		t.Fatalf("expected LocalizedPlugins/Summary Config[api_secret] == %q, got %#v", SecretMaskSentinel, sum1.Config["api_secret"])
	}
	redacted, ok := mgr.GetConfigRedacted("secret-probe")
	if !ok || redacted["api_secret"] != SecretMaskSentinel {
		t.Fatalf("expected GetConfigRedacted to return %q, got %#v", SecretMaskSentinel, redacted)
	}
	localized := mgr.LocalizedPlugins("en-US")
	if len(localized) != 1 || localized[0].Config["api_secret"] != SecretMaskSentinel {
		t.Fatalf("expected LocalizedPlugins to redact secret to %q, got %+v", SecretMaskSentinel, localized)
	}

	// Verify stored in-memory secret starts with "v1:" and does NOT equal plaintext.
	mp, _ := mgr.getPlugin("secret-probe")
	mp.mu.Lock()
	storedSealed := mp.secretConfig["api_secret"]
	mp.mu.Unlock()
	if !strings.HasPrefix(storedSealed, "v1:") || strings.Contains(storedSealed, "top-secret-live-token-99") {
		t.Fatalf("expected v1: sealed secret in memory, got %q", storedSealed)
	}

	// Saving "********" preserves the existing stored secret!
	if _, err := mgr.SaveConfig(ctx, "secret-probe", map[string]any{
		"endpoint":   "https://two.example.com",
		"api_secret": SecretMaskSentinel,
	}, "en-US"); err != nil {
		t.Fatalf("SaveConfig with sentinel: %v", err)
	}
	mp.mu.Lock()
	afterSentinel := mp.secretConfig["api_secret"]
	mp.mu.Unlock()
	if afterSentinel != storedSealed {
		t.Fatalf("saving %q did not preserve sealed secret: got %q want %q", SecretMaskSentinel, afterSentinel, storedSealed)
	}

	// Verify JS VM receives unsealed plaintext via volo.config.get and volo.config.all!
	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/plugins/secret-probe/api/check")
	if err != nil {
		t.Fatalf("GET route: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, `"direct":"top-secret-live-token-99"`) ||
		!strings.Contains(bodyStr, `"fromAll":"top-secret-live-token-99"`) ||
		!strings.Contains(bodyStr, `"endpoint":"https://two.example.com"`) {
		t.Fatalf("expected JS runtime to receive unsealed secret, got %s", bodyStr)
	}
}

func TestExportZipSecurityAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	pluginsDir := filepath.Join(tmpDir, "plugins")
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Catalog:      cat,
		SecretSealer: kr.SecretBox(),
		PluginsDir:   pluginsDir,
		DataDir:      filepath.Join(tmpDir, "data"),
	})
	defer func() { _ = mgr.Close(ctx) }()

	srcZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "exportable-demo",
			"version": "1.1.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Exportable Demo"},
			"config_schema": [
				{"key": "webhook_token", "type": "secret", "default": "leaky-default-secret", "label": {"en-US": "Token"}},
				{"key": "title", "type": "string", "default": "Hello", "label": {"en-US": "Title"}}
			]
		}`,
		"main.js":          `exports.onEnable = function() {};`,
		"assets/style.css": `body { color: #111; }`,
		"local-state.json": `{"sync_cursor": 42, "webhook_token": "raw-unmasked-local-secret", "api_key": "raw-key-123"}`,
		".env":             `RAW_ENV_SECRET=should-never-export`,
		"secrets.json":     `{"raw":"should-never-export"}`,
		"cache.tmp":        `transient`,
	}, "")

	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(srcZip), int64(len(srcZip)), true, "en-US"); err != nil {
		t.Fatalf("install exportable-demo: %v", err)
	}

	// 1. Unsafe plugin IDs must be rejected.
	for _, badID := range []string{"", "../escape", "a/b", "a\\b", "UPPERCASE", ".hidden"} {
		var out bytes.Buffer
		if err := mgr.ExportZip(badID, &out); err == nil {
			t.Fatalf("expected invalid plugin ID %q to be rejected", badID)
		}
	}

	// 2. Symlink inside plugin dir must cause ExportZip to fail closed.
	symlinkPath := filepath.Join(pluginsDir, "exportable-demo", "escape-link.txt")
	outsideSecret := filepath.Join(tmpDir, "host-secret.txt")
	_ = os.WriteFile(outsideSecret, []byte("HOST_SECRET"), 0o600)
	if err := os.Symlink(outsideSecret, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	var symBuf bytes.Buffer
	if err := mgr.ExportZip("exportable-demo", &symBuf); err == nil {
		t.Fatal("expected ExportZip to reject plugin containing symlink")
	}
	_ = os.Remove(symlinkPath)

	// 3. Clean export strips unmasked secrets while preserving legitimate code & state.
	var exported bytes.Buffer
	if err := mgr.ExportZip("exportable-demo", &exported); err != nil {
		t.Fatalf("ExportZip failed: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(exported.Bytes()), int64(exported.Len()))
	if err != nil {
		t.Fatalf("open exported zip: %v", err)
	}
	entries := make(map[string]string, len(zr.File))
	for _, zf := range zr.File {
		rc, oErr := zf.Open()
		if oErr != nil {
			t.Fatalf("open entry %s: %v", zf.Name, oErr)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		entries[zf.Name] = string(b)
	}

	for _, forbidden := range []string{".env", "secrets.json", "cache.tmp"} {
		if _, exists := entries[forbidden]; exists {
			t.Fatalf("forbidden file %q leaked into exported zip", forbidden)
		}
	}
	for name, content := range entries {
		for _, secretStr := range []string{"leaky-default-secret", "raw-unmasked-local-secret", "raw-key-123", "should-never-export"} {
			if strings.Contains(content, secretStr) {
				t.Fatalf("raw secret %q leaked into exported file %q: %s", secretStr, name, content)
			}
		}
	}
	if !strings.Contains(entries["local-state.json"], `"sync_cursor": 42`) {
		t.Fatalf("expected non-secret local state preserved, got %s", entries["local-state.json"])
	}

	// Exported archive can be cleanly reinstalled.
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(exported.Bytes()), int64(exported.Len()), true, "en-US"); err != nil {
		t.Fatalf("reinstall exported zip: %v", err)
	}
}

func TestCustomMarkdownDirectivesAndCircuitBreaker(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:       store,
		Keys:        kr,
		Catalog:     cat,
		PluginsDir:  filepath.Join(tmpDir, "plugins"),
		DataDir:     filepath.Join(tmpDir, "data"),
		HookTimeout: 60 * time.Millisecond,
		MaxFailures: 2,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "directive-pack",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Directive Pack"},
			"permissions": ["render"]
		}`,
		"main.js": `
			volo.registerDirective("callout-box", function(ctx) {
				var kind = (ctx.attrs && ctx.attrs.type) ? ctx.attrs.type : "note";
				return '<div class="callout callout-' + volo.escapeHTML(kind) + '">' + volo.escapeHTML(ctx.body) + '</div>';
			});
			volo.registerDirective("hang-forever", function(ctx) {
				while (true) {}
			});
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("install directive-pack: %v", err)
	}
	if err := mgr.Enable(ctx, "directive-pack"); err != nil {
		t.Fatalf("enable directive-pack: %v", err)
	}

	htmlOut, ok := mgr.DispatchRenderDirective(ctx, "callout-box", map[string]string{"type": "tip"}, "Fast & Safe")
	if !ok || htmlOut != `<div class="callout callout-tip">Fast &amp; Safe</div>` {
		t.Fatalf("unexpected directive output: ok=%v html=%q", ok, htmlOut)
	}

	// Reserved "lock" directive can never be intercepted by plugins.
	if _, hijacked := mgr.DispatchRenderDirective(ctx, "lock", nil, "secret"); hijacked {
		t.Fatal("plugin must never intercept reserved :::lock directive")
	}

	// Trigger timeout twice -> circuit breaker trips to CIRCUIT_OPEN.
	for i := 0; i < 2; i++ {
		if _, handled := mgr.DispatchRenderDirective(ctx, "hang-forever", nil, "loop"); handled {
			t.Fatal("expected hang-forever directive to fail on timeout")
		}
	}
	sum, _ := mgr.Get("directive-pack", "en-US")
	if sum.CircuitState != CircuitOpen || sum.IsEnabled {
		t.Fatalf("expected CIRCUIT_OPEN and auto-disabled after 2 directive timeouts, got %+v", sum)
	}
}

func TestDispatchFederationArticleAsyncPrivacyAndCircuitBreaker(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      cat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		RouteTimeout: 80 * time.Millisecond,
		MaxFailures:  2,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "fed-bridge",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Federation Bridge"},
			"permissions": ["federation", "kv"]
		}`,
		"main.js": `
			volo.registerFederation({
				publish: function(ev) {
					if (ev.title === "FAIL_ME") {
						throw new Error("remote inbox 502");
					}
					volo.kv.set("last_publish", JSON.stringify({
						action: ev.action,
						title: ev.title,
						content: ev.content_md,
						omitted: !!ev.omitted_protected
					}));
				},
				deliver: function(ev) {
					volo.kv.set("last_deliver", ev.action + ":" + ev.id);
				}
			});
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("install fed-bridge: %v", err)
	}
	if err := mgr.Enable(ctx, "fed-bridge"); err != nil {
		t.Fatalf("enable fed-bridge: %v", err)
	}

	// 1. Partial :::lock content is stripped before reaching federation plugin!
	mgr.DispatchFederationArticle(ctx, "publish", map[string]any{
		"id":         "post-1",
		"title":      "Hello Fediverse",
		"content_md": "Public intro\n:::lock\nHidden VIP code\n:::\nPublic outro",
	})
	mgr.WaitFederation()

	kv := NewKVStore(store, "fed-bridge")
	gotPub, ok, err := kv.Get(ctx, "last_publish")
	if err != nil || !ok {
		t.Fatalf("expected last_publish in KV, ok=%v err=%v", ok, err)
	}
	if strings.Contains(gotPub, "Hidden VIP code") || !strings.Contains(gotPub, "Public intro") {
		t.Fatalf("10th Privacy Surface failed on federation payload: %s", gotPub)
	}

	// 2. Deliver action routes to deliver handler and logs completion.
	mgr.DispatchFederationArticle(ctx, "deliver", map[string]any{
		"id":    "post-2",
		"title": "Second Note",
	})
	mgr.WaitFederation()
	gotDeliv, ok, _ := kv.Get(ctx, "last_deliver")
	if !ok || gotDeliv != "deliver:post-2" {
		t.Fatalf("expected deliver:post-2, got %q", gotDeliv)
	}
	logs, err := mgr.Logger().Tail("fed-bridge", 20)
	if err != nil {
		t.Fatalf("Tail logs: %v", err)
	}
	foundFedLog := false
	for _, entry := range logs {
		if strings.HasPrefix(entry.Event, "federation:") {
			foundFedLog = true
			break
		}
	}
	if !foundFedLog {
		t.Fatalf("expected federation log entry, got %+v", logs)
	}

	// 3. Repeated failures trip the circuit breaker.
	for i := 0; i < 2; i++ {
		mgr.DispatchFederationArticle(ctx, "publish", map[string]any{
			"id":    "bad",
			"title": "FAIL_ME",
		})
		mgr.WaitFederation()
	}
	sum, _ := mgr.Get("fed-bridge", "en-US")
	if sum.CircuitState != CircuitOpen || sum.IsEnabled {
		t.Fatalf("expected CIRCUIT_OPEN after 2 federation errors, got %+v", sum)
	}
}

func TestServePluginAssetHardening(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		Catalog:    cat,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "asset-sec",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Asset Security"}
		}`,
		"main.js":                `exports.onEnable = function() {};`,
		"assets/app.css":         `body { margin: 0; }`,
		"assets/polyglot.unkext": `<html><script>alert(1)</script></html>`,
		"assets/run.sh":          `#!/bin/sh`,
		"assets/icon.svg":        `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
		"assets/page.xhtml":      `<html xmlns="http://www.w3.org/1999/xhtml"></html>`,
		"assets/inc.shtml":       `<html></html>`,
		"assets/doc.html":        `<html></html>`,
		"assets/doc.htm":         `<html></html>`,
		"assets/feed.xml":        `<rss></rss>`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("install asset-sec: %v", err)
	}
	if err := mgr.Enable(ctx, "asset-sec"); err != nil {
		t.Fatalf("enable asset-sec: %v", err)
	}

	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	// Unknown and executable extensions get octet-stream + attachment + strict CSP.
	for _, p := range []string{"polyglot.unkext", "run.sh"} {
		resp, err := http.Get(srv.URL + "/plugins/asset-sec/assets/" + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Fatalf("%s: expected Content-Type application/octet-stream, got %q", p, got)
		}
		if got := resp.Header.Get("Content-Disposition"); got != "attachment" {
			t.Fatalf("%s: expected Content-Disposition attachment, got %q", p, got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
			t.Fatalf("%s: expected strict CSP, got %q", p, got)
		}
	}

	// Markup extensions ALWAYS get Content-Security-Policy: default-src 'none'; sandbox.
	for _, p := range []string{"icon.svg", "page.xhtml", "inc.shtml", "doc.html", "doc.htm", "feed.xml"} {
		resp, err := http.Get(srv.URL + "/plugins/asset-sec/assets/" + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
			t.Fatalf("%s: expected CSP \"default-src 'none'; sandbox\", got %q", p, got)
		}
	}
}

func TestJSVMMemoryBoundsObjectKeyCapAndPoolRotation(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		Catalog:    cat,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "mem-bounds",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Memory Bounds"},
			"permissions": ["hooks:comment.created"]
		}`,
		"main.js": `
			var localInits = 0;
			localInits++;
			exports.onHook = function(name, payload) {
				if (payload && payload.explode) {
					try {
						var big = {};
						for (var i = 0; i < 20000; i++) {
							big["k" + i] = i;
						}
					} catch (swallowed) {
						// Must NOT be swallowable by user JS catch!
					}
					return { survived: true };
				}
				return { ok: localInits };
			};
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("install mem-bounds: %v", err)
	}
	if err := mgr.Enable(ctx, "mem-bounds"); err != nil {
		t.Fatalf("enable mem-bounds: %v", err)
	}

	mp, _ := mgr.getPlugin("mem-bounds")
	rt := mp.gojaRT

	// 1. Exceeding maxJSObjectKeys (16384) panics with jsLimitError and is recovered by host runtime.
	_, _, err := rt.invoke(ctx, 2*time.Second, "onHook", "comment.created", map[string]any{"explode": true})
	if err == nil || !strings.Contains(err.Error(), "16384") {
		t.Fatalf("expected object key limit error mentioning 16384, got %v", err)
	}

	// 2. Pooled VM rotates after 1000 uses instead of living in vmPool forever.
	beforeCreated := rt.vmCreatedCount.Load()
	for i := 0; i < maxPooledVMUses+5; i++ {
		if _, _, callErr := rt.invoke(ctx, time.Second, "onHook", "comment.created", map[string]any{"explode": false}); callErr != nil {
			t.Fatalf("invoke #%d failed: %v", i, callErr)
		}
	}
	// Trigger one more checkout after eviction at use #1000 -> must construct a fresh VM!
	afterCreated := rt.vmCreatedCount.Load()
	if afterCreated <= beforeCreated {
		t.Fatalf("expected pooledVM rotation after %d uses (before=%d after=%d)", maxPooledVMUses, beforeCreated, afterCreated)
	}
}

func TestPluginSensitiveLexiconReadClearAndOnUninstall(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	pluginsDir := filepath.Join(tmpDir, "plugins")
	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		Catalog:    cat,
		PluginsDir: pluginsDir,
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	defer func() { _ = mgr.Close(ctx) }()

	var uninstallHookCalls int
	mgr.SetSiteInfoFunc(func() map[string]any {
		uninstallHookCalls++
		return map[string]any{"title": "Volo"}
	})

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "lexicon-guard",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "index.js",
			"name": {"zh-CN": "敏感词库守护", "en-US": "Lexicon Guard"},
			"permissions": ["kv", "provider:antispam", "site:read"]
		}`,
		"dict/words.txt":       "badword1\nbadword2\n违禁词测试\n",
		"dict/large_words.txt": strings.Repeat("动态敏感词\n", 8000),
		"dict/rules.json":      `{"extra":["spamlink"]}`,
		"index.js": `
			var rawTxt = volo.readAsset("dict/words.txt");
			var ruleObj = volo.assets.readJSON("dict/rules.json");
			var baseWords = rawTxt.split("\n").map(function(s) { return s.trim(); }).filter(Boolean).concat(ruleObj.extra || []);

			module.exports = {
				onEnable: function() {
					if (!volo.assets.exists("dict/words.txt") || volo.assets.exists("../outside.txt")) {
						throw new Error("unexpected volo.assets.exists result");
					}
					var blocked = false;
					try { volo.readAsset("../outside.txt"); } catch (e) { blocked = true; }
					if (!blocked) { throw new Error("expected traversal to fail"); }

					// Read > 120 KiB bundled dictionary and persist to KV (enabled by 512 KiB MaxKVValueBytes), then clear & re-seed
					var bigPayload = volo.readAsset("dict/large_words.txt");
					volo.kv.set("dynamic_dict", bigPayload, 0);
					volo.kv.clear();
					if (volo.kv.get("dynamic_dict") !== null) {
						throw new Error("expected volo.kv.clear() to wipe dynamic_dict");
					}
					volo.kv.set("custom_word", "广告微信号", 0);
				},
				checkSpam: function(input) {
					var content = String((input && input.content_md) || "");
					var custom = volo.kv.get("custom_word") || "";
					var all = custom ? baseWords.concat([custom]) : baseWords;
					for (var i = 0; i < all.length; i++) {
						if (all[i] && content.indexOf(all[i]) >= 0) {
							return { action: "reject", reason: "matched:" + all[i] };
						}
					}
					return { action: "allow" };
				},
				onUninstall: function() {
					if (baseWords.length === 4 && volo.kv.get("custom_word") === "广告微信号") {
						volo.site.info();
					}
				}
			};
		`,
	}, "")

	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "zh-CN"); err != nil {
		t.Fatalf("InstallFromZip lexicon-guard: %v", err)
	}
	if err := mgr.Enable(ctx, "lexicon-guard"); err != nil {
		t.Fatalf("Enable lexicon-guard: %v", err)
	}

	// Verify both bundled file lexicon ("违禁词测试") and KV custom lexicon ("广告微信号") reject spam.
	v1 := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{ContentMD: "正常交流，包含违禁词测试内容"})
	if v1.Action != SpamActionReject || !strings.Contains(v1.Reason, "违禁词测试") {
		t.Fatalf("expected bundled lexicon rejection, got verdict=%+v", v1)
	}
	v2 := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{ContentMD: "加我广告微信号领资料"})
	if v2.Action != SpamActionReject || !strings.Contains(v2.Reason, "广告微信号") {
		t.Fatalf("expected KV lexicon rejection, got verdict=%+v", v2)
	}

	// Disable first, then Uninstall: onUninstall() MUST still fire before KV and directory are wiped!
	if err := mgr.Disable(ctx, "lexicon-guard"); err != nil {
		t.Fatalf("Disable lexicon-guard: %v", err)
	}
	if err := mgr.Uninstall(ctx, "lexicon-guard"); err != nil {
		t.Fatalf("Uninstall lexicon-guard: %v", err)
	}
	if uninstallHookCalls != 1 {
		t.Fatalf("expected onUninstall() to fire exactly once with intact KV before wipe, got %d", uninstallHookCalls)
	}
	pDir := filepath.Join(pluginsDir, "lexicon-guard")
	if _, statErr := os.Stat(pDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected plugin dir %s to be deleted after Uninstall, statErr=%v", pDir, statErr)
	}
	kv := NewKVStore(store, "lexicon-guard")
	if items, lErr := kv.ListPrefix(ctx, "", 50, ""); lErr != nil || len(items) != 0 {
		t.Fatalf("expected 0 plugin_kv items after Uninstall, got len=%d err=%v", len(items), lErr)
	}
}
