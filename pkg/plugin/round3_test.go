package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound3_OIDCAdvancedProviderParamsAndEmailVerified(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
	})
	defer mgr.Close(ctx)

	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}

	// Plugin 1: Modern 3-arg OIDC plugin inspecting opts (nonce, PKCE) and returning varied email_verified forms.
	modernManifest := `{
		"id": "oidc-modern",
		"name": {"en-US": "OIDC Modern"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["provider:oidc"]
	}`
	modernJS := `
		function oidcProviderInfo() {
			return { id: "modern-idp", name: "Modern IdP" };
		}
		function oidcAuthURL(state, redirectURI, opts = {}) {
			const q = [
				"state=" + encodeURIComponent(state),
				"redirect_uri=" + encodeURIComponent(redirectURI)
			];
			if (opts.nonce) q.push("nonce=" + encodeURIComponent(opts.nonce));
			if (opts.code_challenge) q.push("code_challenge=" + encodeURIComponent(opts.code_challenge));
			if (opts.code_challenge_method) q.push("code_challenge_method=" + encodeURIComponent(opts.code_challenge_method));
			return "https://idp.example.com/oauth2/authorize?" + q.join("&");
		}
		function oidcExchange(code, redirectURI, opts = {}) {
			if (code === "code_bool_true") {
				return {
					subject: "u1",
					email: "u1@example.com",
					email_verified: true,
					nonce: opts.nonce || "",
					display_name: "U1:" + (opts.code_verifier || "")
				};
			}
			if (code === "code_str_true") {
				return { subject: "u2", email: "u2@example.com", email_verified: "true", nonce: "n2" };
			}
			if (code === "code_num_one") {
				return { subject: "u3", email: "u3@example.com", verified: 1, nonce: "n3" };
			}
			if (code === "code_str_false") {
				return { subject: "u4", email: "u4@example.com", email_verified: "false" };
			}
			// Absent email_verified must default to false!
			return { subject: "u5", email: "u5@example.com" };
		}
	`
	zipModern := buildZipBytes(t, map[string]string{
		"manifest.json": modernManifest,
		"main.js":       modernJS,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipModern), int64(len(zipModern)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip oidc-modern: %v", err)
	}
	if err := mgr.Enable(ctx, "oidc-modern"); err != nil {
		t.Fatalf("Enable oidc-modern: %v", err)
	}

	// Plugin 2: Legacy 2-arg OIDC plugin that ignores opts — adapter must append nonce & PKCE query params cleanly!
	legacyManifest := `{
		"id": "oidc-legacy",
		"name": {"en-US": "OIDC Legacy"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["provider:oidc"]
	}`
	legacyJS := `
		function oidcProviderInfo() {
			return { id: "legacy-idp", name: "Legacy IdP" };
		}
		function oidcAuthURL(state, redirectURI) {
			return "https://legacy.example.com/auth?state=" + encodeURIComponent(state) + "&redirect_uri=" + encodeURIComponent(redirectURI);
		}
		function oidcExchange(code, redirectURI) {
			return { subject: "leg_" + code, email: "legacy@example.com" };
		}
	`
	zipLegacy := buildZipBytes(t, map[string]string{
		"manifest.json": legacyManifest,
		"main.js":       legacyJS,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipLegacy), int64(len(zipLegacy)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip oidc-legacy: %v", err)
	}
	if err := mgr.Enable(ctx, "oidc-legacy"); err != nil {
		t.Fatalf("Enable oidc-legacy: %v", err)
	}

	// Verify Modern OIDCAdvancedProvider
	modernBase, ok := mgr.Providers().OIDC("modern-idp")
	if !ok {
		t.Fatal("expected modern-idp OIDC provider registered")
	}
	modernAdv, ok := modernBase.(OIDCAdvancedProvider)
	if !ok {
		t.Fatalf("expected gojaOIDCAdapter to implement OIDCAdvancedProvider, got %T", modernBase)
	}
	uStr, err := modernAdv.AuthCodeURLWithParams(OIDCAuthParams{
		State:               "st_abc",
		RedirectURI:         "https://blog.example.com/cb",
		Nonce:               "nonce_123",
		CodeChallenge:       "chal_xyz",
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		t.Fatalf("modern AuthCodeURLWithParams: %v", err)
	}
	parsedModern, err := url.Parse(uStr)
	if err != nil {
		t.Fatalf("parse modern URL: %v", err)
	}
	if got := parsedModern.Query()["nonce"]; len(got) != 1 || got[0] != "nonce_123" {
		t.Fatalf("expected single nonce=nonce_123, got %v", got)
	}
	if got := parsedModern.Query()["code_challenge"]; len(got) != 1 || got[0] != "chal_xyz" {
		t.Fatalf("expected single code_challenge=chal_xyz, got %v", got)
	}
	if got := parsedModern.Query().Get("code_challenge_method"); got != "S256" {
		t.Fatalf("expected code_challenge_method=S256, got %q", got)
	}

	id1, err := modernAdv.ExchangeWithParams(ctx, OIDCExchangeParams{
		Code:         "code_bool_true",
		RedirectURI:  "https://blog.example.com/cb",
		Nonce:        "nonce_123",
		CodeVerifier: "verif_secret",
	})
	if err != nil {
		t.Fatalf("ExchangeWithParams code_bool_true: %v", err)
	}
	if !id1.EmailVerified || id1.Nonce != "nonce_123" || id1.DisplayName != "U1:verif_secret" {
		t.Fatalf("unexpected id1: %+v", id1)
	}

	id2, err := modernAdv.Exchange(ctx, "code_str_true", "https://blog.example.com/cb")
	if err != nil || !id2.EmailVerified || id2.Nonce != "n2" {
		t.Fatalf("expected EmailVerified=true & Nonce=n2 for string 'true', got %+v err=%v", id2, err)
	}
	id3, err := modernAdv.Exchange(ctx, "code_num_one", "https://blog.example.com/cb")
	if err != nil || !id3.EmailVerified || id3.Nonce != "n3" {
		t.Fatalf("expected EmailVerified=true & Nonce=n3 for verified=1, got %+v err=%v", id3, err)
	}
	id4, err := modernAdv.Exchange(ctx, "code_str_false", "https://blog.example.com/cb")
	if err != nil || id4.EmailVerified {
		t.Fatalf("expected EmailVerified=false for 'false', got %+v err=%v", id4, err)
	}
	id5, err := modernAdv.Exchange(ctx, "code_absent", "https://blog.example.com/cb")
	if err != nil || id5.EmailVerified || id5.Nonce != "" {
		t.Fatalf("expected EmailVerified=false when absent, got %+v err=%v", id5, err)
	}

	// Verify Legacy 2-arg OIDC plugin gets missing nonce & PKCE query params appended automatically
	legacyBase, ok := mgr.Providers().OIDC("legacy-idp")
	if !ok {
		t.Fatal("expected legacy-idp OIDC provider registered")
	}
	legacyAdv := legacyBase.(OIDCAdvancedProvider)
	legURLStr, err := legacyAdv.AuthCodeURLWithParams(OIDCAuthParams{
		State:               "st_leg",
		RedirectURI:         "https://blog.example.com/cb",
		Nonce:               "auto_nonce",
		CodeChallenge:       "auto_chal",
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		t.Fatalf("legacy AuthCodeURLWithParams: %v", err)
	}
	parsedLeg, err := url.Parse(legURLStr)
	if err != nil {
		t.Fatalf("parse legacy URL: %v", err)
	}
	if parsedLeg.Query().Get("nonce") != "auto_nonce" ||
		parsedLeg.Query().Get("code_challenge") != "auto_chal" ||
		parsedLeg.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("expected auto-appended nonce & PKCE params on legacy URL, got %q", legURLStr)
	}
	legIdent, err := legacyAdv.ExchangeWithParams(ctx, OIDCExchangeParams{
		Code:         "abc",
		RedirectURI:  "https://blog.example.com/cb",
		Nonce:        "auto_nonce",
		CodeVerifier: "auto_verif",
	})
	if err != nil || legIdent.Subject != "leg_abc" || legIdent.EmailVerified {
		t.Fatalf("unexpected legacy ExchangeWithParams identity: %+v err=%v", legIdent, err)
	}
}

func TestRound3_DispatchHookPrivacyAutoSanitizationAndNestedLockStripping(t *testing.T) {
	// 1. Unit-test StripLockedMarkdownBlocks edge cases (nested directives, CommonMark <= 3 spaces, blockquotes, code fences, unclosed locks).
	rawMD := strings.Join([]string{
		"Public intro.",
		"",
		"```markdown",
		":::lock",
		"code example inside fence must remain visible",
		":::",
		"```",
		"",
		"    :::lock",
		"    4-space indented code block is not a directive",
		"    :::",
		"",
		"   :::lock Secret Title",
		"Top secret paragraph 1",
		"  :::info Nested Callout",
		"  Nested secret inside callout",
		"  :::details Deeply Nested",
		"  Deepest secret",
		"  :::",
		"  Still inside nested callout and outer lock!",
		"  :::",
		"Still inside outer lock after nested callout closed! MUST BE STRIPPED!",
		":::bookshelf:::",
		"Still locked after inline single-line directive!",
		"   :::",
		"",
		"> :::lock Quoted Lock",
		"> Secret inside blockquote",
		"> > :::warning Nested Quoted Warning",
		"> > Inner quoted secret",
		"> > :::",
		"> Still inside quoted lock!",
		"> :::",
		"",
		"Public outro.",
	}, "\n")

	stripped := StripLockedMarkdownBlocks(rawMD)
	if !strings.Contains(stripped, "code example inside fence must remain visible") {
		t.Fatalf("expected fenced code example preserved, got:\n%s", stripped)
	}
	if !strings.Contains(stripped, "4-space indented code block is not a directive") {
		t.Fatalf("expected 4-space indented code block preserved, got:\n%s", stripped)
	}
	if !strings.Contains(stripped, "Public intro.") || !strings.Contains(stripped, "Public outro.") {
		t.Fatalf("expected public text preserved, got:\n%s", stripped)
	}
	for _, secret := range []string{
		"Top secret paragraph 1",
		"Nested secret inside callout",
		"Deepest secret",
		"Still inside nested callout",
		"Still inside outer lock",
		"Still locked after inline",
		"Secret inside blockquote",
		"Inner quoted secret",
		"Still inside quoted lock",
	} {
		if strings.Contains(stripped, secret) {
			t.Fatalf("PRIVACY LEAK: %q survived StripLockedMarkdownBlocks:\n%s", secret, stripped)
		}
	}

	// Unclosed :::lock must fail closed to EOF.
	unclosed := "Before\n:::lock\n:::info\ninner\n:::\ntrailing secret without closing fence"
	if out := StripLockedMarkdownBlocks(unclosed); strings.Contains(out, "trailing secret") || strings.Contains(out, "inner") || !strings.Contains(out, "Before") {
		t.Fatalf("expected unclosed :::lock to strip to EOF, got: %q", out)
	}

	// 2. End-to-end test: calling Manager.DispatchHook directly with ArticleHookPayload and *ArticleHookPayload!
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
	})
	defer mgr.Close(ctx)
	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}

	spyManifest := `{
		"id": "hook-spy",
		"name": {"en-US": "Hook Spy"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["kv:rw", "hooks:article.published", "http:routes"],
		"hooks": ["article.published"]
	}`
	spyJS := `
		volo.on("article.published", (payload) => {
			const prev = JSON.parse(volo.kv.get("seen") || "[]");
			prev.push(payload);
			volo.kv.set("seen", JSON.stringify(prev), 0);
		});
		function onRequest() {
			return { status: 200, content_type: "application/json", body: volo.kv.get("seen") || "[]" };
		}
	`
	zipSpy := buildZipBytes(t, map[string]string{
		"manifest.json": spyManifest,
		"main.js":       spyJS,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipSpy), int64(len(zipSpy)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip hook-spy: %v", err)
	}
	if err := mgr.Enable(ctx, "hook-spy"); err != nil {
		t.Fatalf("Enable hook-spy: %v", err)
	}

	// Dispatch value ArticleHookPayload (password-protected)
	mgr.DispatchHook(ctx, HookArticlePublished, ArticleHookPayload{
		ID:                  "art-pass",
		Title:               "Protected Post",
		ContentMD:           "PASSWORD_PROTECTED_BODY",
		SummaryMD:           "PASSWORD_PROTECTED_SUMMARY",
		HTML:                "<p>PASSWORD_PROTECTED_HTML</p>",
		CoverURL:            "https://cdn.example.com/secret.png",
		AudioURL:            "https://cdn.example.com/secret.mp3",
		IsPasswordProtected: true,
	})

	// Dispatch pointer *ArticleHookPayload (nested :::lock) and ensure caller's struct is not mutated
	callerPayload := &ArticleHookPayload{
		ID:        "art-lock",
		Title:     "Partial Lock Post",
		ContentMD: "Public Hello\n:::lock\n:::info\nINNER_LOCKED_SECRET\n:::\nOUTER_LOCKED_SECRET\n:::\nPublic Bye",
		SummaryMD: ":::lock\nSUMMARY_SECRET\n:::\nVisible Summary",
	}
	mgr.DispatchHook(ctx, HookArticlePublished, callerPayload)
	if !strings.Contains(callerPayload.ContentMD, "OUTER_LOCKED_SECRET") {
		t.Fatal("DispatchHook mutated caller's *ArticleHookPayload in place")
	}

	// Dispatch typed nil *ArticleHookPayload safely (must be a clean no-op)
	var nilArticlePtr *ArticleHookPayload
	mgr.DispatchHook(ctx, HookArticlePublished, nilArticlePtr)

	req := httptest.NewRequest(http.MethodGet, "/plugins/hook-spy/api/seen", nil)
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from hook-spy, got %d: %s", rec.Code, rec.Body.String())
	}
	var seen []ArticleHookPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &seen); err != nil {
		t.Fatalf("unmarshal seen payloads: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 sanitized hook deliveries (nil skipped), got %d: %+v", len(seen), seen)
	}
	if seen[0].ContentMD != "" || seen[0].SummaryMD != "" || seen[0].HTML != "" || seen[0].CoverURL != "" || seen[0].AudioURL != "" || !seen[0].OmittedProtected {
		t.Fatalf("password-protected article leaked fields via DispatchHook: %+v", seen[0])
	}
	if strings.Contains(seen[1].ContentMD, "INNER_LOCKED_SECRET") ||
		strings.Contains(seen[1].ContentMD, "OUTER_LOCKED_SECRET") ||
		strings.Contains(seen[1].SummaryMD, "SUMMARY_SECRET") {
		t.Fatalf(":::lock secret leaked via DispatchHook(*ArticleHookPayload): %+v", seen[1])
	}
	if !strings.Contains(seen[1].ContentMD, "Public Hello") || !strings.Contains(seen[1].ContentMD, "Public Bye") {
		t.Fatalf("expected public portions preserved in seen[1]: %+v", seen[1])
	}
}

func TestRound3_NotifierDualEntrypointsCapabilityGatesFrozenVoloOnAndKanbanniangManifest(t *testing.T) {
	// 1. Validate plugins/kanbanniang/manifest.json has "ratelimit" permission and passes Manifest.Validate().
	kbBytes, err := os.ReadFile(filepath.Join("..", "..", "plugins", "kanbanniang", "manifest.json"))
	if err != nil {
		t.Fatalf("read kanbanniang manifest.json: %v", err)
	}
	kbManifest, err := ParseManifest(kbBytes)
	if err != nil {
		t.Fatalf("ParseManifest kanbanniang: %v", err)
	}
	if !kbManifest.HasPermission("ratelimit") {
		t.Fatalf("expected kanbanniang manifest to declare 'ratelimit' permission, got %v", kbManifest.Permissions)
	}

	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
	})
	defer mgr.Close(ctx)
	mgr.SetSiteInfoFunc(func() map[string]any {
		return map[string]any{"title": "Volo Test Blog", "lang": "en-US"}
	})
	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}

	// 2. Test both sendNotification(event) and notifySend(event) NotifierProviders!
	for _, tc := range []struct {
		id     string
		fnName string
	}{
		{id: "notif-send-notification", fnName: "sendNotification"},
		{id: "notif-notify-send", fnName: "notifySend"},
	} {
		mf := `{
			"id": "` + tc.id + `",
			"name": {"en-US": "` + tc.id + `"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["provider:notifier", "kv:rw", "http:routes"]
		}`
		js := `
			function ` + tc.fnName + `(ev) {
				volo.kv.set("last_title", ev.title + "|" + ev.event, 0);
				return true;
			}
			function onRequest() {
				return { status: 200, body: volo.kv.get("last_title") || "" };
			}
		`
		zb := buildZipBytes(t, map[string]string{"manifest.json": mf, "main.js": js}, "")
		if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zb), int64(len(zb)), true, "en-US"); err != nil {
			t.Fatalf("InstallFromZip %s: %v", tc.id, err)
		}
		if err := mgr.Enable(ctx, tc.id); err != nil {
			t.Fatalf("Enable %s: %v", tc.id, err)
		}
	}

	notifiers := mgr.Providers().Notifiers()
	if len(notifiers) != 2 {
		t.Fatalf("expected 2 registered notifiers, got %d", len(notifiers))
	}
	for _, n := range notifiers {
		if err := n.Send(ctx, NotificationEvent{Event: "comment.created", Title: "Hello Push"}); err != nil {
			t.Fatalf("Notifier %s Send failed: %v", n.Name(), err)
		}
	}
	for _, id := range []string{"notif-send-notification", "notif-notify-send"} {
		req := httptest.NewRequest(http.MethodGet, "/plugins/"+id+"/api/check", nil)
		rec := httptest.NewRecorder()
		mgr.Handler().ServeHTTP(rec, req)
		if rec.Body.String() != "Hello Push|comment.created" {
			t.Fatalf("expected %s to record notification, got %q", id, rec.Body.String())
		}
	}

	// 3. Test capability gates: unprivileged plugin lacks volo.crypto, volo.site, volo.rateLimit;
	// also verify calling volo.on() after top-level init throws TypeError and does NOT leak listeners!
	unprivManifest := `{
		"id": "cap-unpriv",
		"name": {"en-US": "Unprivileged"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["ui:slots"],
		"ui_slots": [{"slot": "admin.dashboard_card", "dynamic": true}]
	}`
	unprivJS := `
		function renderSlot() {
			let leakErr = "";
			try {
				volo.on("article.published", () => {});
			} catch (e) {
				leakErr = String(e.message || e);
			}
			return "<p>" + [
				"crypto=" + typeof volo.crypto,
				"site=" + typeof volo.site,
				"rl=" + typeof volo.rateLimit,
				"frozen=" + (leakErr.indexOf("initialization") >= 0 ? "yes" : "no")
			].join(",") + "</p>";
		}
	`
	zbUnpriv := buildZipBytes(t, map[string]string{"manifest.json": unprivManifest, "main.js": unprivJS}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zbUnpriv), int64(len(zbUnpriv)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip cap-unpriv: %v", err)
	}
	if err := mgr.Enable(ctx, "cap-unpriv"); err != nil {
		t.Fatalf("Enable cap-unpriv: %v", err)
	}
	cards := mgr.RenderDashboardCards(ctx, SlotRenderContext{Locale: "en-US"})
	if len(cards) != 1 || !strings.Contains(cards[0].HTML, "crypto=undefined,site=undefined,rl=undefined,frozen=yes") {
		t.Fatalf("unexpected capability gate / frozen volo.on output: %+v", cards)
	}

	// 4. Test privileged plugin with "crypto", "site:read", "ratelimit" permissions granted!
	privManifest := `{
		"id": "cap-priv",
		"name": {"en-US": "Privileged"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["crypto", "site:read", "ratelimit", "http:routes"]
	}`
	privJS := `
		function onRequest() {
			const hash = volo.crypto.sha256("volo");
			const mac = volo.crypto.hmacSha256("k", "m");
			const uuid = volo.crypto.randomUUID();
			const info = volo.site.info();
			const a1 = volo.rateLimit.allow("ip:1", 2, 60);
			const a2 = volo.rateLimit("ip:1", 2, 60);
			const a3 = volo.rateLimit.allow("ip:1", 2, 60);
			return {
				status: 200,
				content_type: "application/json",
				body: JSON.stringify({
					hash_len: hash.length,
					mac_len: mac.length,
					uuid_len: uuid.length,
					title: info.title,
					rl: [a1, a2, a3]
				})
			};
		}
	`
	zbPriv := buildZipBytes(t, map[string]string{"manifest.json": privManifest, "main.js": privJS}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zbPriv), int64(len(zbPriv)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip cap-priv: %v", err)
	}
	if err := mgr.Enable(ctx, "cap-priv"); err != nil {
		t.Fatalf("Enable cap-priv: %v", err)
	}
	reqPriv := httptest.NewRequest(http.MethodGet, "/plugins/cap-priv/api/test", nil)
	recPriv := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(recPriv, reqPriv)
	if recPriv.Code != http.StatusOK {
		t.Fatalf("expected 200 from cap-priv, got %d: %s", recPriv.Code, recPriv.Body.String())
	}
	var privOut struct {
		HashLen int    `json:"hash_len"`
		MacLen  int    `json:"mac_len"`
		UUIDLen int    `json:"uuid_len"`
		Title   string `json:"title"`
		RL      []bool `json:"rl"`
	}
	if err := json.Unmarshal(recPriv.Body.Bytes(), &privOut); err != nil {
		t.Fatal(err)
	}
	if privOut.HashLen != 64 || privOut.MacLen != 64 || privOut.UUIDLen != 36 || privOut.Title != "Volo Test Blog" {
		t.Fatalf("unexpected privileged SDK output: %+v", privOut)
	}
	if len(privOut.RL) != 3 || !privOut.RL[0] || !privOut.RL[1] || privOut.RL[2] {
		t.Fatalf("expected rateLimit max=2 to return [true, true, false], got %v", privOut.RL)
	}
}

func TestRound3_JSVMSemanticsES6SyntaxAndStdlib(t *testing.T) {
	t.Run("VarHoistingToFunctionAndModuleRoot", func(t *testing.T) {
		vm := newJSRuntime()
		val, err := evalJSScript(vm, `
			function testFn(param) {
				var param; // must NOT clobber parameter binding
				let beforeAssign = typeof hoistedInIf;
				if (true) {
					var hoistedInIf = "from_if";
				}
				for (var i = 0; i < 3; i++) {
					var insideLoop = i * 10;
				}
				try {
					throw new Error("boom");
				} catch (e) {
					var hoistedInCatch = "caught";
				}
				return [param, beforeAssign, hoistedInIf, i, insideLoop, hoistedInCatch].join("|");
			}
			if (false) {
				var moduleHoisted = 123;
			}
			return testFn("arg0") + "|" + String(moduleHoisted);
		`)
		if err != nil {
			t.Fatalf("VarHoisting failed: %v", err)
		}
		if got := val.Export(); got != "arg0|undefined|from_if|3|20|caught|undefined" {
			t.Fatalf("unexpected var hoisting result: %v", got)
		}
	})

	t.Run("ForLetPerIterationClosureCapture", func(t *testing.T) {
		vm := newJSRuntime()
		val, err := evalJSScript(vm, `
			const fns = [];
			for (let i = 0; i < 5; i++) {
				if (i === 2) continue;
				fns.push(() => i);
			}
			return fns.map(fn => fn()).join(",");
		`)
		if err != nil {
			t.Fatalf("ForLet closure failed: %v", err)
		}
		if got := val.Export(); got != "0,1,3,4" {
			t.Fatalf("expected per-iteration let capture '0,1,3,4', got %v", got)
		}
	})

	t.Run("RFC3986EncodeDecodeURIComponent", func(t *testing.T) {
		vm := newJSRuntime()
		val, err := evalJSScript(vm, `
			const enc = encodeURIComponent("a b+c-_.!~*'()你好");
			const decPlus = decodeURIComponent("a+b%20c");
			const roundtrip = decodeURIComponent(enc);
			let badURI = false;
			try {
				decodeURIComponent("%E4%B8");
			} catch (e) {
				badURI = true;
			}
			return JSON.stringify({ enc, decPlus, roundtrip, badURI });
		`)
		if err != nil {
			t.Fatalf("URI component test failed: %v", err)
		}
		var out struct {
			Enc       string `json:"enc"`
			DecPlus   string `json:"decPlus"`
			Roundtrip string `json:"roundtrip"`
			BadURI    bool   `json:"badURI"`
		}
		if err := json.Unmarshal([]byte(val.toString()), &out); err != nil {
			t.Fatal(err)
		}
		if out.Enc != "a%20b%2Bc-_.!~*'()%E4%BD%A0%E5%A5%BD" {
			t.Fatalf("RFC 3986 encodeURIComponent mismatch: got %q", out.Enc)
		}
		if out.DecPlus != "a+b c" {
			t.Fatalf("decodeURIComponent('a+b%%20c') must NOT turn '+' into space: got %q", out.DecPlus)
		}
		if out.Roundtrip != "a b+c-_.!~*'()你好" || !out.BadURI {
			t.Fatalf("unexpected roundtrip/badURI: %+v", out)
		}
	})

	t.Run("TemplateLiteralNestedBracesCommentsStringsAndRegex", func(t *testing.T) {
		vm := newJSRuntime()
		val, err := evalJSScript(vm, "const res = `A:${({ k: { v: 7 } }).k.v}|B:${\"}\" + '}' + `nested:${1 + 2}`}|C:${/* } */ 4 + // }\n5}|D:${\"x}y\".replace(/\\}/g, \"-\")}`;\nreturn res;")
		if err != nil {
			t.Fatalf("TemplateLiteral nested scanner failed: %v", err)
		}
		if got := val.Export(); got != "A:7|B:}}nested:3|C:9|D:x-y" {
			t.Fatalf("unexpected template literal output: %v", got)
		}
	})

	t.Run("OptionalChainingPropertyComputedCallAndShortCircuit", func(t *testing.T) {
		vm := newJSRuntime()
		val, err := evalJSScript(vm, `
			let sideEffects = 0;
			const bump = () => { sideEffects++; return "k"; };
			const nilObj = null;
			const a = nilObj?.prop;
			const b = nilObj?.[bump()];
			const c = nilObj?.(bump());
			const d = nilObj?.deep.chain[bump()](bump());
			const real = {
				arr: [10, 20],
				fn: (x, y) => x + y
			};
			const e = real?.arr?.[1];
			const f = real?.fn?.(3, 4);
			const g = real?.missing?.(bump());
			let parenThrew = false;
			try {
				const _ = (nilObj?.prop).mustThrow;
			} catch (err) {
				parenThrew = true;
			}
			return JSON.stringify({
				undef: [a === undefined, b === undefined, c === undefined, d === undefined, g === undefined],
				sideEffects,
				e,
				f,
				parenThrew
			});
		`)
		if err != nil {
			t.Fatalf("OptionalChaining failed: %v", err)
		}
		if got := val.toString(); got != `{"e":20,"f":7,"parenThrew":true,"sideEffects":0,"undef":[true,true,true,true,true]}` {
			t.Fatalf("unexpected optional chaining result: %s", got)
		}
	})

	t.Run("SwitchContinuePropagationAndDefaultFallthroughOrder", func(t *testing.T) {
		vm := newJSRuntime()
		val, err := evalJSScript(vm, `
			const log = [];
			for (let i = 0; i < 4; i++) {
				switch (i) {
					case 1:
						log.push("skip1");
						continue;
					default:
						log.push("def" + i);
						break;
				}
				log.push("after" + i);
			}

			// When default appears BEFORE case 2 and case 1 falls through into default:
			const ft = [];
			switch (1) {
				case 1:
					ft.push("c1");
				default:
					ft.push("def");
				case 2:
					ft.push("c2");
					break;
				case 3:
					ft.push("c3");
			}
			return log.join(",") + "|" + ft.join(",");
		`)
		if err != nil {
			t.Fatalf("Switch semantics failed: %v", err)
		}
		if got := val.Export(); got != "def0,after0,skip1,def2,after2,def3,after3|c1,def,c2" {
			t.Fatalf("unexpected switch continue/fallthrough result: %v", got)
		}
	})

	t.Run("StdlibBuiltinsNumberArrayObjectStringRegExpFunction", func(t *testing.T) {
		vm := newJSRuntime()
		val, err := evalJSScript(vm, `
			// Number
			const numChecks = [
				Number.isNaN(NaN),
				!Number.isNaN("NaN"),
				Number.isFinite(42),
				!Number.isFinite(Infinity),
				Number.isInteger(10.0),
				!Number.isInteger(10.5),
				Number.parseInt("0xff", 16) === 255,
				Number.parseFloat("3.25px") === 3.25,
				(3.14159).toFixed(2) === "3.14",
				(255).toString(16) === "ff",
				0b1010 === 10 && 0o17 === 15
			];
			if (!numChecks.every(Boolean)) throw new Error("Number checks failed: " + JSON.stringify(numChecks));

			// Array
			const fromStr = Array.from("ab", (ch, idx) => ch + idx).join(",");
			const base = [1, 2, 3];
			const conc = base.concat([4, 5], 6);
			const sum = conc.reduce((acc, v) => acc + v, 0);
			const found = conc.find(x => x > 4);
			const foundIdx = conc.findIndex(x => x > 4);
			const hasEven = conc.some(x => x % 2 === 0);
			const allPos = conc.every(x => x > 0);
			const mut = [3, 1, 2];
			mut.sort((a, b) => a - b);
			mut.reverse(); // [3, 2, 1]
			const shifted = mut.shift(); // 3 -> [2, 1]
			const newLen = mut.unshift(9, 8); // 4 -> [9, 8, 2, 1]
			const removed = mut.splice(1, 2, 7); // removed [8, 2], mut -> [9, 7, 1]
			const flattened = [1, [2, [3, 4]]].flat(2).join(",");

			// Object
			const obj = { a: 1, b: 2 };
			const vals = Object.values(obj).join(",");
			const rebuilt = Object.fromEntries(Object.entries(obj).map(([k, v]) => [k.toUpperCase(), v * 10]));
			const ownChecks = [
				Object.hasOwn(obj, "a"),
				!Object.hasOwn(obj, "toString"),
				obj.hasOwnProperty("b"),
				Object.prototype.hasOwnProperty.call(obj, "a")
			];
			if (!ownChecks.every(Boolean)) throw new Error("Object ownChecks failed");

			// String & RegExp callback replacers
			const chStr = String.fromCharCode(65, 66, 67);
			const padded = "7".padStart(3, "0") + "-" + "7".padEnd(3, "0");
			const trimmed = "  hi  ".trimStart() + "|" + "  hi  ".trimEnd();
			const lastIdx = "ababa".lastIndexOf("ba");
			const repCb = "item-12 and item-34".replace(/item-(\d+)/g, (full, d, offset) => d + "@" + offset);
			const repAllCb = "a1-a1".replaceAll("a1", (m, off) => m.toUpperCase() + off);

			// Function.prototype.{call, apply, bind}
			function greet(prefix, suffix) {
				return prefix + this.name + suffix;
			}
			const c1 = greet.call({ name: "Volo" }, "[", "]");
			const c2 = greet.apply({ name: "Go" }, ["<", ">"]);
			const bound = greet.bind({ name: "Bound" }, "(");
			const c3 = bound(")");

			return JSON.stringify({
				fromStr, sum, found, foundIdx, hasEven, allPos,
				shifted, newLen, removed, mut, flattened,
				vals, rebuilt, chStr, padded, trimmed, lastIdx,
				repCb, repAllCb, c1, c2, c3
			});
		`)
		if err != nil {
			t.Fatalf("Stdlib built-ins failed: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(val.toString()), &out); err != nil {
			t.Fatal(err)
		}
		if out["fromStr"] != "a0,b1" || out["sum"] != float64(21) || out["found"] != float64(5) || out["foundIdx"] != float64(4) {
			t.Fatalf("unexpected Array metrics: %+v", out)
		}
		if out["flattened"] != "1,2,3,4" || out["chStr"] != "ABC" || out["padded"] != "007-700" || out["trimmed"] != "hi  |  hi" || out["lastIdx"] != float64(3) {
			t.Fatalf("unexpected String/flat metrics: %+v", out)
		}
		if out["repCb"] != "12@0 and 34@12" || out["repAllCb"] != "A10-A13" {
			t.Fatalf("unexpected String replace/replaceAll callback result: %+v", out)
		}
		if out["c1"] != "[Volo]" || out["c2"] != "<Go>" || out["c3"] != "(Bound)" {
			t.Fatalf("unexpected Function call/apply/bind result: %+v", out)
		}
	})

	t.Run("ES6DefaultsRestSpreadShorthandAndDestructuring", func(t *testing.T) {
		vm := newJSRuntime()
		val, err := evalJSScript(vm, `
			// Default & rest params in function and arrow
			function calc(a = 2, b = a * 3, ...extra) {
				return a + b + extra.reduce((s, x) => s + x, 0);
			}
			const arrowCalc = (x = 10, ...more) => x + more.length;

			// Array, call, and object spread + object shorthand & concise methods
			const part1 = [1, 2];
			const part2 = [3, 4];
			const combined = [0, ...part1, ...part2, 5];
			const maxSpread = Math.max(...combined);

			const tag = "volo";
			const baseObj = { a: 1, b: 2 };
			const mergedObj = {
				...baseObj,
				b: 20,
				tag,
				["dyn_" + 1]: 99,
				describe(sep = ":") {
					const innerArrow = () => this.tag + sep + this.b;
					return innerArrow();
				}
			};

			// Object & array destructuring (const/let/var, defaults, nested, rest, and assignment swap)
			const { a, b: renamedB = 5, missing = 42, ...restProps } = { a: 7, b: undefined, x: 8, y: 9 };
			const [first, , third = 30, ...tail] = [10, 20, undefined, 40, 50];
			let p = "left", q = "right";
			[p, q] = [q, p];

			return JSON.stringify({
				c1: calc(),
				c2: calc(undefined, 5, 10, 20),
				ac: arrowCalc(undefined, "a", "b"),
				maxSpread,
				desc: mergedObj.describe("-"),
				dyn: mergedObj.dyn_1,
				destructObj: [a, renamedB, missing, restProps.x + restProps.y],
				destructArr: [first, third, tail.join(",")],
				swapped: p + "|" + q
			});
		`)
		if err != nil {
			t.Fatalf("ES6 ergonomics test failed: %v", err)
		}
		const want = `{"ac":12,"c1":8,"c2":37,"desc":"volo-20","destructArr":[10,30,"40,50"],"destructObj":[7,5,42,17],"dyn":99,"maxSpread":5,"swapped":"right|left"}`
		if got := val.toString(); got != want {
			t.Fatalf("ES6 ergonomics mismatch:\ngot:  %s\nwant: %s", got, want)
		}
	})
}
