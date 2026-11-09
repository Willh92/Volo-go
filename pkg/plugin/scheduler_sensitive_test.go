package plugin

import (
	"context"
	"encoding/json"
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

	// 7. Turning auto_update off immediately unregisters the cron job (zero background wakeups).
	if _, err := mgr.UpdateConfig(ctx, "sensitive-words", map[string]any{
		"auto_update": false,
		"update_url":  remoteSrv.URL + "/sensitive_word_dict.txt",
	}, "zh-CN"); err != nil {
		t.Fatalf("UpdateConfig(auto_update=false): %v", err)
	}
	if got := len(mgr.CronJobs()); got != 0 {
		t.Fatalf("expected 0 cron jobs after disabling auto_update, got %d", got)
	}
}
