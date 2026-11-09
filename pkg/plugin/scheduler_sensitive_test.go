package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func copyDirRecursive(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		rel, rErr := filepath.Rel(src, path)
		if rErr != nil {
			return rErr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, fErr := os.ReadFile(path)
		if fErr != nil {
			return fErr
		}
		return os.WriteFile(target, data, 0o640)
	})
	if err != nil {
		t.Fatalf("copyDirRecursive(%s -> %s): %v", src, dst, err)
	}
}

func TestCronRegistry_MultiplexingAndNonReentrantCoalescing(t *testing.T) {
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var runsA atomic.Int32
	var runsB atomic.Int32

	reg := newCronRegistry(
		2,
		func() time.Duration { return 3 * time.Millisecond },
		func(_ context.Context, pluginID string, _ string, handler string) error {
			if pluginID == "p-slow" && handler == "slowJob" {
				cur := concurrent.Add(1)
				for {
					prev := maxConcurrent.Load()
					if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
						break
					}
				}
				time.Sleep(45 * time.Millisecond)
				runsA.Add(1)
				concurrent.Add(-1)
				return nil
			}
			if pluginID == "p-fast" && handler == "fastJob" {
				runsB.Add(1)
			}
			return nil
		},
	)
	t.Cleanup(func() { reg.Close() })

	if got := len(reg.ListJobs("")); got != 0 {
		t.Fatalf("expected 0 jobs on idle init, got %d", got)
	}

	reg.BeginPlugin("p-slow")
	reg.BeginPlugin("p-fast")

	if err := reg.RegisterJob("p-slow", CronSpec{
		Name:            "job1",
		IntervalSeconds: 5,
		Handler:         "slowJob",
	}, CronSourceDynamic, true); err != nil {
		t.Fatalf("RegisterJob slow: %v", err)
	}
	if err := reg.RegisterJob("p-fast", CronSpec{
		Name:            "job2",
		IntervalSeconds: 5,
		Handler:         "fastJob",
	}, CronSourceDynamic, true); err != nil {
		t.Fatalf("RegisterJob fast: %v", err)
	}

	// Hammer Trigger while slowJob is running to prove non-reentrant coalescing.
	for i := 0; i < 6; i++ {
		reg.Trigger("p-slow", "job1")
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runsA.Load() >= 2 && runsB.Load() >= 1 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	if got := maxConcurrent.Load(); got > 1 {
		t.Fatalf("expected non-reentrant max concurrency 1 for same job, got %d", got)
	}
	if runsA.Load() < 2 || runsB.Load() < 1 {
		t.Fatalf("expected coalesced follow-up run (runsA=%d, runsB=%d)", runsA.Load(), runsB.Load())
	}

	reg.UnregisterPlugin("p-slow", true)
	reg.UnregisterJob("p-fast", "job2")
	if got := len(reg.ListJobs("")); got != 0 {
		t.Fatalf("expected 0 jobs after unregister, got %d", got)
	}
}

func TestOfficialSensitiveWordsPlugin_Houbb64kAndOnlineConvertSync(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	pluginsDir := filepath.Join(tmpDir, "plugins")
	srcPluginDir := filepath.Join("..", "..", "plugins", "sensitive-words")
	copyDirRecursive(t, srcPluginDir, filepath.Join(pluginsDir, "sensitive-words"))

	// Stand up a mock upstream houbb/sensitive-word-data server supporting ETag / 304.
	var remoteHits atomic.Int32
	const mockETag = `"houbb-v2-etag"`
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteHits.Add(1)
		if r.Header.Get("If-None-Match") == mockETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", mockETag)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Serve raw houbb format including corrupted control-byte noise + upstream English noise + new custom entry
		_, _ = io.WriteString(w, strings.Join([]string{
			"# Remote houbb incremental test feed",
			"远程热更新违禁词样本",
			"2kshr7b\x16yrwq",
			"duck",
			"!远程热更新豁免短语",
		}, "\n"))
	}))
	defer remoteSrv.Close()

	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		PluginsDir:            pluginsDir,
		DataDir:               filepath.Join(tmpDir, "data"),
		Catalog:               cat,
		AllowLoopbackForTests: true,
	})
	mgr.SetRequestInspector(func(r *http.Request) (bool, string) {
		return r.Header.Get("X-Test-Admin") == "1", "127.0.0.0"
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if err := mgr.Enable(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// 1. Lazy standby check: auto_update defaults to false -> 0 scheduled cron jobs, and neither dashboard card nor GET /status forces eager 64k compile.
	if got := len(mgr.CronJobs()); got != 0 {
		t.Fatalf("expected 0 cron jobs when auto_update=false, got %d", got)
	}
	slots := mgr.RenderDashboardCards(ctx, SlotRenderContext{Locale: "zh-CN"})
	if len(slots) != 1 || !strings.Contains(slots[0].HTML, "按需加载") {
		t.Fatalf("expected lazy standby dashboard card before first check, got %+v", slots)
	}

	unauthStatusReq := httptest.NewRequest(http.MethodGet, "/plugins/sensitive-words/api/status", nil)
	unauthStatusRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(unauthStatusRec, unauthStatusReq)
	if unauthStatusRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthenticated GET /status, got %d (%s)", unauthStatusRec.Code, unauthStatusRec.Body.String())
	}

	idleStatusReq := httptest.NewRequest(http.MethodGet, "/plugins/sensitive-words/api/status", nil)
	idleStatusReq.Header.Set("X-Test-Admin", "1")
	idleStatusRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(idleStatusRec, idleStatusReq)
	if idleStatusRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin idle GET /status, got %d (%s)", idleStatusRec.Code, idleStatusRec.Body.String())
	}
	var idleStatus map[string]any
	if err := json.Unmarshal(idleStatusRec.Body.Bytes(), &idleStatus); err != nil {
		t.Fatalf("decode idle /status: %v", err)
	}
	if compiled, _ := idleStatus["compiled"].(bool); compiled {
		t.Fatalf("expected compiled=false on idle standby GET /status, got %+v", idleStatus)
	}

	// 2. First comment check lazily stream-compiles the 64k+ houbb dictionary in Go.
	blockCases := []string{
		"你真是个傻逼",
		"推薦澳門娛樂城給你", // Traditional -> Simplified ("澳门娱乐城")
		"組織賣淫違法",    // Traditional -> Simplified ("卖淫")
		"傻**逼**东西",  // Markdown split
		"傻 * 逼",     // Separator piercing
		"傻傻傻逼逼",     // Repeat collapsing
		"曹操他妈的",     // Allowlist "曹操" must NOT blind overlapping "操他妈"
	}
	for _, content := range blockCases {
		verdict := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
			TargetType: "article",
			TargetID:   "art-1",
			AuthorName: "普通读者",
			ContentMD:  content,
			Locale:     "zh-CN",
			IsGuest:    true,
		})
		if verdict.Action != SpamActionReject {
			t.Errorf("expected comment %q to be rejected, got %+v", content, verdict)
		}
	}

	// 3. Clean technical/blog comments & allowlisted phrases (including "博主"/"管理员" in comment body!) must pass.
	allowCases := []string{
		"感谢博主的深度技术分享！站长写的 Go 并发剖析非常清晰。",
		"三国演义里曹操孟德用兵如神，祝大家生日快乐！",
		"刚买了一台傻瓜相机和一支 2B铅笔，顺便研究 Python duck typing 与 gender 字段设计。",
	}
	for _, content := range allowCases {
		verdict := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
			TargetType: "article",
			TargetID:   "art-1",
			AuthorName: "普通读者",
			ContentMD:  content,
			Locale:     "zh-CN",
			IsGuest:    true,
		})
		if verdict.Action != SpamActionPass && verdict.Action != SpamActionAllow {
			t.Errorf("false positive on clean comment %q: %+v", content, verdict)
		}
	}

	// 4. Nickname guard blocks both sensitive words AND reserved impersonation handles, while passing normal handles.
	for _, badNick := range []string{"系统管理员", "官方客服", "博主", "webmaster", "傻逼青年", "澳門百家樂"} {
		nv := mgr.Providers().EvaluateNickname(ctx, NicknameCheckInput{
			Nickname: badNick,
			Locale:   "zh-CN",
			Source:   "reader_register",
		})
		if nv.Action != SpamActionReject {
			t.Errorf("expected nickname %q to be rejected, got %+v", badNick, nv)
		}
	}
	if cleanNV := mgr.Providers().EvaluateNickname(ctx, NicknameCheckInput{
		Nickname: "清风明月读书人",
		Locale:   "zh-CN",
		Source:   "reader_register",
	}); cleanNV.Action != SpamActionPass && cleanNV.Action != SpamActionAllow {
		t.Fatalf("expected clean nickname to pass, got %+v", cleanNV)
	}

	// 5. Category toggle: disabling cat_politics ("08宪章" is Tag 0 in houbb) hot-reloads via onConfigChange.
	if vBefore := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
		TargetType: "article",
		TargetID:   "art-1",
		AuthorName: "读者",
		ContentMD:  "测试08宪章词条",
		Locale:     "zh-CN",
	}); vBefore.Action != SpamActionReject {
		t.Fatalf("expected Tag 0 term to block when cat_politics=true, got %+v", vBefore)
	}

	if _, err := mgr.UpdateConfig(ctx, "sensitive-words", map[string]any{
		"cat_politics": false,
	}, "zh-CN"); err != nil {
		t.Fatalf("UpdateConfig(cat_politics=false): %v", err)
	}
	if vAfter := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
		TargetType: "article",
		TargetID:   "art-1",
		AuthorName: "读者",
		ContentMD:  "测试08宪章词条",
		Locale:     "zh-CN",
	}); vAfter.Action != SpamActionPass && vAfter.Action != SpamActionAllow {
		t.Fatalf("expected Tag 0 term to pass when cat_politics=false, got %+v", vAfter)
	}

	// 6. Enable auto_update pointing to remoteSrv -> registers unified cron job, runs immediate native conversion sync,
	// blocks newly synced word while respecting "!远程热更新豁免短语" and dropping corrupted/duck noise.
	if _, err := mgr.UpdateConfig(ctx, "sensitive-words", map[string]any{
		"auto_update":           true,
		"update_url":            remoteSrv.URL + "/sensitive_word_dict.txt",
		"update_interval_hours": 12,
	}, "zh-CN"); err != nil {
		t.Fatalf("UpdateConfig(auto_update=true): %v", err)
	}
	if got := len(mgr.CronJobs()); got != 1 {
		t.Fatalf("expected 1 cron job registered when auto_update=true, got %d", got)
	}

	syncDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(syncDeadline) {
		if remoteHits.Load() >= 1 {
			v := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
				TargetType: "article",
				TargetID:   "art-1",
				AuthorName: "读者",
				ContentMD:  "包含远程热更新违禁词样本的评论",
				Locale:     "zh-CN",
			})
			if v.Action == SpamActionReject {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if remoteHits.Load() < 1 {
		t.Fatal("expected immediate initial remote sync via CronRegistry")
	}

	if vSynced := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
		TargetType: "article",
		TargetID:   "art-1",
		AuthorName: "读者",
		ContentMD:  "包含远程热更新违禁词样本的评论",
		Locale:     "zh-CN",
	}); vSynced.Action != SpamActionReject {
		t.Fatalf("expected newly synced remote word to be blocked, got %+v", vSynced)
	}
	if vAllowRemote := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
		TargetType: "article",
		TargetID:   "art-1",
		AuthorName: "读者",
		ContentMD:  "讨论远程热更新豁免短语与 duck typing",
		Locale:     "zh-CN",
	}); vAllowRemote.Action != SpamActionPass && vAllowRemote.Action != SpamActionAllow {
		t.Fatalf("expected remote allowlist & sanitized noise not to false-positive, got %+v", vAllowRemote)
	}

	// Trigger cron again (non-forced) -> should send If-None-Match and receive 304 Not Modified cleanly.
	hitsBefore304 := remoteHits.Load()
	mgr.TriggerCronJob("sensitive-words", "sync_dict")
	deadline304 := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline304) && remoteHits.Load() <= hitsBefore304 {
		time.Sleep(20 * time.Millisecond)
	}
	if remoteHits.Load() <= hitsBefore304 {
		t.Fatal("expected manual cron trigger to contact remote server for conditional 304 check")
	}

	// Verify non-admin POST /sync is rejected with 403 and GET /status reports 64k+ words and remote sync metadata.
	unauthSyncReq := httptest.NewRequest(http.MethodPost, "/plugins/sensitive-words/api/sync", nil)
	unauthRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(unauthRec, unauthSyncReq)
	if unauthRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthenticated POST /sync, got %d (%s)", unauthRec.Code, unauthRec.Body.String())
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/plugins/sensitive-words/api/status", nil)
	statusReq.Header.Set("X-Test-Admin", "1")
	statusRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("GET /status failed: status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	var parsedStatus map[string]any
	if err := json.Unmarshal(statusRec.Body.Bytes(), &parsedStatus); err != nil {
		t.Fatalf("decode /status json: %v", err)
	}
	if wc, _ := parsedStatus["word_count"].(float64); wc < 60000 {
		t.Fatalf("expected 60k+ compiled houbb words in /status, got %v", parsedStatus["word_count"])
	}

	// 7. Verify AuthorURL screening, ja-JP localized reason, and DELETE /remote-cache cleanup.
	urlVerdict := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
		TargetType: "article",
		TargetID:   "art-1",
		AuthorName: "読者",
		AuthorURL:  "https://澳門百家樂.example.com/landing",
		ContentMD:  "とても参考になりました！",
		Locale:     "ja-JP",
		IsGuest:    true,
	})
	if urlVerdict.Action != SpamActionReject || !strings.Contains(urlVerdict.Reason, "制限された語句") {
		t.Fatalf("expected AuthorURL spam rejection with ja-JP message, got %+v", urlVerdict)
	}

	delCacheReq := httptest.NewRequest(http.MethodDelete, "/plugins/sensitive-words/api/remote-cache", nil)
	delCacheReq.Header.Set("X-Test-Admin", "1")
	delCacheRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(delCacheRec, delCacheReq)
	if delCacheRec.Code != http.StatusOK || !strings.Contains(delCacheRec.Body.String(), `"ok":true`) {
		t.Fatalf("DELETE /remote-cache failed: status=%d body=%s", delCacheRec.Code, delCacheRec.Body.String())
	}

	// 8. Turning auto_update off immediately unregisters the cron job (zero background wakeups).
	if _, err := mgr.UpdateConfig(ctx, "sensitive-words", map[string]any{
		"auto_update": false,
		"update_url":  remoteSrv.URL + "/sensitive_word_dict.txt",
	}, "zh-CN"); err != nil {
		t.Fatalf("UpdateConfig(auto_update=false): %v", err)
	}
	if got := len(mgr.CronJobs()); got != 0 {
		t.Fatalf("expected 0 cron jobs after disabling auto_update, got %d", got)
	}

	// 9. Uninstall triggers onUninstall() cleanly and purges plugin_kv.
	if err := mgr.Uninstall(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Uninstall(sensitive-words): %v", err)
	}
}

func TestPluginLifecycle5Stage_DefaultURLOutboundGuard_And_CronPanicRecovery(t *testing.T) {
	ctx := context.Background()

	// 1. Verify CronRegistry recovers from panics in worker callbacks and records LastError.
	reg := newCronRegistry(2, func() time.Duration { return 10 * time.Millisecond }, func(_ context.Context, _ string, _ string, _ string) error {
		panic("boom inside cron callback")
	})
	defer reg.Close()
	reg.BeginPlugin("panic-plug")
	if err := reg.RegisterJob("panic-plug", CronSpec{
		Name:            "explode",
		IntervalSeconds: 60,
		Handler:         "run",
	}, CronSourceDynamic, true); err != nil {
		t.Fatalf("RegisterJob: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		jobs := reg.ListJobs("panic-plug")
		if len(jobs) == 1 && strings.Contains(jobs[0].LastError, "cron panic:") {
			recovered = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !recovered {
		t.Fatalf("expected CronRegistry to recover panic into LastError, got %+v", reg.ListJobs("panic-plug"))
	}

	// 2. Verify 5-stage lifecycle (onInstall -> onEnable -> onConfigChange -> onDisable -> onUninstall)
	// AND verify unvetted manifest default URL cannot bypass empty allowed_hosts until admin explicitly configures it!
	var outboundHits atomic.Int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		outboundHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	}))
	defer targetSrv.Close()

	store, kr, cat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		Catalog:               cat,
		PluginsDir:            pluginsDir,
		DataDir:               filepath.Join(tmpDir, "data"),
		HookTimeout:           2 * time.Second,
		RouteTimeout:          2 * time.Second,
		AllowLoopbackForTests: true,
	})
	defer func() { _ = mgr.Close(ctx) }()

	manifestV1 := `{
		"id": "lifecycle-guard",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"en-US": "Lifecycle Guard"},
		"permissions": ["kv", "http:outbound", "http:routes"],
		"allowed_hosts": [],
		"config_schema": [
			{
				"key": "endpoint",
				"type": "url",
				"default": "` + targetSrv.URL + `/ping",
				"label": {"en-US": "Endpoint"}
			}
		]
	}`
	mainJS := `
	module.exports = {
		onInstall: function(info) {
			volo.kv.set("install_stage", (info && info.is_upgrade ? "upgrade:" : "fresh:") + info.version, 0);
		},
		onEnable: function() {
			var prev = volo.kv.get("events") || "";
			volo.kv.set("events", prev + "enable,", 0);
		},
		onConfigChange: function() {
			var prev = volo.kv.get("events") || "";
			volo.kv.set("events", prev + "config,", 0);
		},
		onDisable: function() {
			var prev = volo.kv.get("events") || "";
			volo.kv.set("events", prev + "disable,", 0);
		},
		onUninstall: function() {
			volo.kv.set("uninstalled_marker", "1", 0);
		},
		onRequest: function(req) {
			if (req.path === "/probe") {
				var ep = volo.config.get("endpoint");
				try {
					var r = volo.http.fetch(ep, { method: "GET" });
					return { status: 200, content_type: "application/json", body: JSON.stringify({ ok: true, body: r.body, install: volo.kv.get("install_stage"), events: volo.kv.get("events") }) };
				} catch (e) {
					return { status: 403, content_type: "application/json", body: JSON.stringify({ ok: false, err: String(e) }) };
				}
			}
			return { status: 404, body: "not found" };
		}
	};
	`

	zipV1 := buildZipBytes(t, map[string]string{
		"manifest.json": manifestV1,
		"main.js":       mainJS,
	}, "")
	sumV1, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipV1), int64(len(zipV1)), false, "en-US")
	if err != nil {
		t.Fatalf("InstallFromZip v1: %v", err)
	}
	if len(sumV1.AllowedHosts) != 0 {
		t.Fatalf("expected 0 effective AllowedHosts for unvetted default URL, got %v", sumV1.AllowedHosts)
	}
	if err := mgr.Enable(ctx, "lifecycle-guard"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// Probe with default URL must be blocked (allowed_hosts is empty and admin has not explicitly set endpoint).
	probeReq1 := httptest.NewRequest(http.MethodGet, "/plugins/lifecycle-guard/api/probe", nil)
	probeRec1 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(probeRec1, probeReq1)
	if probeRec1.Code != http.StatusForbidden || outboundHits.Load() != 0 {
		t.Fatalf("expected unvetted default URL fetch to be blocked (403, 0 hits), got status=%d hits=%d body=%s",
			probeRec1.Code, outboundHits.Load(), probeRec1.Body.String())
	}

	// Admin explicitly saves the endpoint URL -> now dynamically permitted!
	sumCfg, err := mgr.UpdateConfig(ctx, "lifecycle-guard", map[string]any{
		"endpoint": targetSrv.URL + "/explicit",
	}, "en-US")
	if err != nil {
		t.Fatalf("UpdateConfig explicit URL: %v", err)
	}
	if len(sumCfg.AllowedHosts) != 1 {
		t.Fatalf("expected 1 effective AllowedHost after explicit admin URL config, got %v", sumCfg.AllowedHosts)
	}

	probeReq2 := httptest.NewRequest(http.MethodGet, "/plugins/lifecycle-guard/api/probe", nil)
	probeRec2 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(probeRec2, probeReq2)
	if probeRec2.Code != http.StatusOK || outboundHits.Load() != 1 {
		t.Fatalf("expected explicit admin URL fetch to succeed, got status=%d hits=%d body=%s",
			probeRec2.Code, outboundHits.Load(), probeRec2.Body.String())
	}
	if !strings.Contains(probeRec2.Body.String(), `"install":"fresh:1.0.0"`) || !strings.Contains(probeRec2.Body.String(), `"events":"enable,config,"`) {
		t.Fatalf("unexpected lifecycle KV trace: %s", probeRec2.Body.String())
	}

	// Upgrade v1.0.0 -> v1.1.0 via InstallFromZip(overwrite=true) -> triggers onInstall({is_upgrade:true, previous_version:"1.0.0", version:"1.1.0"}).
	manifestV2 := strings.Replace(manifestV1, `"1.0.0"`, `"1.1.0"`, 1)
	zipV2 := buildZipBytes(t, map[string]string{
		"manifest.json": manifestV2,
		"main.js":       mainJS,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipV2), int64(len(zipV2)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip v2 upgrade: %v", err)
	}
	if err := mgr.Enable(ctx, "lifecycle-guard"); err != nil {
		t.Fatalf("Enable v2 upgrade: %v", err)
	}
	probeReq3 := httptest.NewRequest(http.MethodGet, "/plugins/lifecycle-guard/api/probe", nil)
	probeRec3 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(probeRec3, probeReq3)
	if !strings.Contains(probeRec3.Body.String(), `"install":"upgrade:1.1.0"`) {
		t.Fatalf("expected upgrade:1.1.0 after overwrite install, got %s", probeRec3.Body.String())
	}

	if err := mgr.Disable(ctx, "lifecycle-guard"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := mgr.Uninstall(ctx, "lifecycle-guard"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
}

func TestUnifiedChainedInterception_And_PluginPriorityReordering(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      cat,
		PluginsDir:   pluginsDir,
		DataDir:      filepath.Join(tmpDir, "data"),
		HookTimeout:  2 * time.Second,
		RouteTimeout: 2 * time.Second,
	})
	defer func() { _ = mgr.Close(ctx) }()

	// Install two JS plugins ("alpha-filter" and "zeta-guard") that subscribe to common before_* hooks,
	// render.post_html, and provider:antispam to verify BOTH unified chained interception AND priority reordering.
	alphaZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "alpha-filter",
			"version": "1.0.0",
			"priority": 50,
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Alpha Filter"},
			"permissions": ["hooks:*", "render:*", "provider:antispam"],
			"hooks": ["comment.before_create", "user.before_register", "article.before_save", "render.post_html"]
		}`,
		"main.js": `
			volo.on("comment.before_create", function(payload) {
				// Partial payload return: must preserve id/author_name while ratcheting status -> pending!
				return {
					content_md: payload.content_md + " [alpha]",
					action: "moderate"
				};
			});
			volo.on("user.before_register", function(payload) {
				if (payload.username === "blocked_by_alpha") {
					throw volo.reject("alpha blocked registration", "ALPHA_DENY");
				}
				return { trace: (payload.trace || "") + "alpha>" };
			});
			volo.on("render.post_html", function(payload) {
				return { html: payload.html + "[A]" };
			});
			module.exports = {
				checkSpam: function(input) {
					if (input.content_md.indexOf("spam-both") >= 0) {
						return { action: "reject", score: 1, reason: "rejected-by-alpha" };
					}
					return { action: "moderate", score: 0.5, reason: "moderated-by-alpha" };
				}
			};
		`,
	}, "")

	zetaZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "zeta-guard",
			"version": "1.0.0",
			"priority": 80,
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Zeta Guard"},
			"permissions": ["hooks:*", "render:*", "provider:antispam"],
			"hooks": ["comment.before_create", "user.before_register", "article.before_save", "render.post_html"]
		}`,
		"main.js": `
			// Register two intra-plugin volo.on listeners to verify intra-plugin short-circuiting!
			volo.on("article.before_save", function(payload) {
				if (payload.title.indexOf("DRAFT_BLOCK") >= 0) {
					return volo.reject("zeta blocked article save", "ZETA_ARTICLE");
				}
				return { title: payload.title + " (verified)" };
			});
			volo.on("article.before_save", function(payload) {
				// Must NOT run when the first listener returned volo.reject(...)!
				return { title: payload.title + " [second-listener]" };
			});
			volo.on("comment.before_create", function(payload) {
				// Attempts to set status="approved", but alpha already ratcheted status="pending"!
				return {
					content_md: payload.content_md + " [zeta]",
					status: "approved"
				};
			});
			volo.on("user.before_register", function(payload) {
				if (payload.username === "blocked_by_zeta") {
					return { allow: false, reason: "zeta blocked registration" };
				}
				return { trace: (payload.trace || "") + "zeta>" };
			});
			volo.on("render.post_html", function(payload) {
				return { html: payload.html + "[Z]" };
			});
			module.exports = {
				checkSpam: function(input) {
					if (input.content_md.indexOf("spam-both") >= 0) {
						return { action: "reject", score: 1, reason: "rejected-by-zeta" };
					}
					return { action: "pass", score: 0 };
				}
			};
		`,
	}, "")

	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(alphaZip), int64(len(alphaZip)), false, "en-US"); err != nil {
		t.Fatalf("install alpha: %v", err)
	}
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zetaZip), int64(len(zetaZip)), false, "en-US"); err != nil {
		t.Fatalf("install zeta: %v", err)
	}
	if err := mgr.Enable(ctx, "alpha-filter"); err != nil {
		t.Fatalf("enable alpha: %v", err)
	}
	if err := mgr.Enable(ctx, "zeta-guard"); err != nil {
		t.Fatalf("enable zeta: %v", err)
	}

	// 1. Non-destructive partial map chaining + monotonic pending status ratcheting across alpha(50) -> zeta(80):
	cmtOut, err := mgr.DispatchHook(ctx, HookCommentBeforeCreate, map[string]any{
		"id":          "c_1",
		"author_name": "Alice",
		"content_md":  "hello",
		"status":      "approved",
	})
	if err != nil {
		t.Fatalf("DispatchHook(comment.before_create): %v", err)
	}
	cmtMap := cmtOut.(map[string]any)
	if cmtMap["id"] != "c_1" || cmtMap["author_name"] != "Alice" {
		t.Fatalf("expected untouched keys preserved across partial map returns, got %+v", cmtMap)
	}
	if cmtMap["content_md"] != "hello [alpha] [zeta]" {
		t.Fatalf("expected priority chain alpha->zeta content_md, got %q", cmtMap["content_md"])
	}
	if cmtMap["status"] != "pending" {
		t.Fatalf("expected status ratcheted to pending despite zeta returning approved, got %q", cmtMap["status"])
	}

	// 2. Unified interception on user.before_register via `throw volo.reject(...)` MUST NOT increment FailureCount!
	_, regErr := mgr.DispatchHook(ctx, HookUserBeforeRegister, map[string]any{
		"username": "blocked_by_alpha",
	})
	var hookRej *HookRejectedError
	if !errors.As(regErr, &hookRej) || hookRej.PluginID != "alpha-filter" || hookRej.Code != "ALPHA_DENY" || hookRej.Reason != "alpha blocked registration" {
		t.Fatalf("expected HookRejectedError from alpha-filter, got %v", regErr)
	}
	alphaSum, _ := mgr.Get("alpha-filter", "en-US")
	if alphaSum.FailureCount != 0 || alphaSum.CircuitState != CircuitClosed {
		t.Fatalf("expected FailureCount=0 after business-rule volo.reject, got failures=%d circuit=%s", alphaSum.FailureCount, alphaSum.CircuitState)
	}

	// 3. Intra-plugin short-circuit + article.before_save interception & mutation:
	_, artRejErr := mgr.DispatchHook(ctx, HookArticleBeforeSave, ArticleHookPayload{
		ID:        "a1",
		Title:     "DRAFT_BLOCK secret",
		ContentMD: "body",
	})
	if !errors.As(artRejErr, &hookRej) || hookRej.PluginID != "zeta-guard" || hookRej.Code != "ZETA_ARTICLE" {
		t.Fatalf("expected article.before_save rejection from zeta-guard, got %v", artRejErr)
	}
	artOkRaw, artOkErr := mgr.DispatchHook(ctx, HookArticleBeforeSave, ArticleHookPayload{
		ID:        "a1",
		Title:     "Clean Post",
		ContentMD: "body",
	})
	if artOkErr != nil {
		t.Fatalf("expected clean article.before_save to pass, got %v", artOkErr)
	}
	if artOk := artOkRaw.(ArticleHookPayload); artOk.Title != "Clean Post (verified) [second-listener]" {
		t.Fatalf("expected chained intra-plugin title transformation, got %q", artOk.Title)
	}

	// 4. Verify initial order (alpha=50 before zeta=80) across render.post_html, RenderHooksStateHash, and AntiSpamProvider:
	hashBefore := mgr.RenderHooksStateHash()
	renderedBefore := mgr.DispatchArticleHook(ctx, HookRenderPostHTML, ArticleHookPayload{HTML: "<p>x</p>"})
	if renderedBefore.HTML != "<p>x</p>[A][Z]" {
		t.Fatalf("expected <p>x</p>[A][Z] before reorder, got %q", renderedBefore.HTML)
	}
	spamBefore := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{ContentMD: "spam-both"})
	if spamBefore.Reason != "rejected-by-alpha" {
		t.Fatalf("expected alpha (priority 50) to short-circuit spam-both first, got %+v", spamBefore)
	}
	modMerged := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{ContentMD: "ordinary text"})
	if modMerged.Action != SpamActionModerate || modMerged.Reason != "moderated-by-alpha" {
		t.Fatalf("expected moderate+pass across providers to ratchet to moderate, got %+v", modMerged)
	}

	// 5. Reorder in Plugin Center: put zeta-guard FIRST -> ["zeta-guard", "alpha-filter"]!
	reordered, err := mgr.Reorder(ctx, []string{"zeta-guard", "alpha-filter"}, "en-US")
	if err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	if len(reordered) != 2 || reordered[0].ID != "zeta-guard" || reordered[1].ID != "alpha-filter" {
		t.Fatalf("unexpected List order after Reorder: %+v", reordered)
	}
	if reordered[0].Priority >= reordered[1].Priority {
		t.Fatalf("expected zeta priority (%d) < alpha priority (%d)", reordered[0].Priority, reordered[1].Priority)
	}

	// Render cache hash MUST change when render plugin execution order flips!
	hashAfter := mgr.RenderHooksStateHash()
	if hashAfter == "" || hashAfter == hashBefore {
		t.Fatalf("expected RenderHooksStateHash to change after Reorder (%q vs %q)", hashBefore, hashAfter)
	}
	renderedAfter := mgr.DispatchArticleHook(ctx, HookRenderPostHTML, ArticleHookPayload{HTML: "<p>x</p>"})
	if renderedAfter.HTML != "<p>x</p>[Z][A]" {
		t.Fatalf("expected <p>x</p>[Z][A] after Reorder, got %q", renderedAfter.HTML)
	}
	// Same-type AntiSpamProvider chain MUST ALSO flip so zeta short-circuits first!
	spamAfter := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{ContentMD: "spam-both"})
	if spamAfter.Reason != "rejected-by-zeta" {
		t.Fatalf("expected zeta to short-circuit spam-both first after Reorder, got %+v", spamAfter)
	}
	// Domain hook chain MUST ALSO flip (zeta>alpha>)!
	traceOut, err := mgr.DispatchHook(ctx, HookUserBeforeRegister, map[string]any{"username": "alice"})
	if err != nil {
		t.Fatalf("DispatchHook(user.before_register): %v", err)
	}
	if traceOut.(map[string]any)["trace"] != "zeta>alpha>" {
		t.Fatalf("expected zeta>alpha> trace after Reorder, got %+v", traceOut)
	}

	// 6. SetPriority(alpha-filter, 5) puts alpha back ahead of zeta (priority 10) and persists across UpdateConfig!
	if _, err := mgr.SetPriority(ctx, "alpha-filter", 5, "en-US"); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	if _, err := mgr.UpdateConfig(ctx, "alpha-filter", map[string]any{}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	alphaAfterCfg, _ := mgr.Get("alpha-filter", "en-US")
	if alphaAfterCfg.Priority != 5 {
		t.Fatalf("expected priority 5 preserved after UpdateConfig, got %d", alphaAfterCfg.Priority)
	}
	spamRestored := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{ContentMD: "spam-both"})
	if spamRestored.Reason != "rejected-by-alpha" {
		t.Fatalf("expected alpha (priority 5) to run first again, got %+v", spamRestored)
	}
}

