package server_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/media"
	"github.com/Willh92/Volo-go/internal/notify"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/theme"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func copyTreeForM6(t *testing.T, srcDir, dstDir string) {
	t.Helper()
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o640)
	})
	if err != nil {
		t.Fatalf("copyTreeForM6(%s -> %s): %v", srcDir, dstDir, err)
	}
}

func makeRotatedGPSJPEG(t *testing.T, orientation uint16) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 120, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 120; x++ {
			if x < 60 && y < 40 {
				img.SetNRGBA(x, y, color.NRGBA{R: 230, G: 55, B: 55, A: 255})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{R: 25, G: 115, B: 195, A: 255})
			}
		}
	}
	var jpgBuf bytes.Buffer
	if err := jpeg.Encode(&jpgBuf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	jpgBytes := jpgBuf.Bytes()

	var tiff bytes.Buffer
	tiff.Write([]byte("II\x2a\x00"))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))

	makeStr := []byte("StudioCamera-GeoProbe\x00")
	gpsPayload := []byte("GPS_LAT_37.7749N_LON_122.4194W_M6_CANARY\x00")
	makeOffset := uint32(8 + 2 + 3*12 + 4)
	gpsOffset := makeOffset + uint32(len(makeStr))

	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x010F))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(2))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(len(makeStr)))
	_ = binary.Write(&tiff, binary.LittleEndian, makeOffset)

	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, orientation)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))

	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x8825))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(4))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, gpsOffset)

	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))
	tiff.Write(makeStr)
	tiff.Write(gpsPayload)

	var app1 bytes.Buffer
	app1.Write([]byte("Exif\x00\x00"))
	app1.Write(tiff.Bytes())
	segLen := uint16(app1.Len() + 2)

	var out bytes.Buffer
	out.Write(jpgBytes[:2])
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, segLen)
	out.Write(app1.Bytes())
	out.Write(jpgBytes[2:])
	return out.Bytes()
}

func makeSmallPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 40, G: 180, B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadMultipartMedia(t *testing.T, urlStr, filename, contentType string, data []byte, cookies []*http.Cookie, csrf string) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	hdr.Set("Content-Type", contentType)
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, urlStr, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(auth.CSRFHeaderName, csrf)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func TestM6_FullProductionSmokeLoop(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	pluginsDir := filepath.Join(tmp, "plugins")
	themesDir := filepath.Join(tmp, "themes")
	for _, d := range []string{dataDir, uploadDir, pluginsDir, themesDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	// Stage real external companion plugin (`./plugins/kanbanniang`) & theme (`./themes/volo-journal`)
	copyTreeForM6(t, filepath.Join(repoRoot, "plugins", "kanbanniang"), filepath.Join(pluginsDir, "kanbanniang"))
	copyTreeForM6(t, filepath.Join(repoRoot, "themes", "volo-journal"), filepath.Join(themesDir, "volo-journal"))

	secret := "0123456789abcdef0123456789abcdef-m6-e2e-secret"
	pubURL := "https://volo-m6.example.com"
	adminPath := "/console"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		AdminPath:  &adminPath,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "volo-m6.db"),
	})
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}

	cat, err := i18n.NewCatalog("zh-CN", "")
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	if _, err := settingsMgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       cfg.KeyRing,
		PluginsDir: pluginsDir,
		DataDir:    dataDir,
		Catalog:    cat,
	})
	if err := pluginMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart plugins: %v", err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	mediaPipe := media.NewPipeline(media.PipelineOptions{
		Store:     store,
		Settings:  settingsMgr,
		UploadDir: uploadDir,
		Plugins:   pluginMgr,
	})
	pluginMgr.Providers().RegisterStorage("builtin-media", mediaPipe)

	themesEng, err := theme.NewEngine(cat, themesDir)
	if err != nil {
		t.Fatalf("theme.NewEngine: %v", err)
	}

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 4<<20)
	cnt := content.NewService(content.ServiceOptions{
		Store:     store,
		Keys:      cfg.KeyRing,
		Settings:  settingsMgr,
		Passports: passports,
		PoW:       pow,
		Guard:     grd,
		Search:    srch,
		Renderer:  rnd,
		Plugins:   pluginMgr,
		Catalog:   cat,
	})
	authSvc := auth.NewService(store, cfg.KeyRing, settingsMgr, cfg.PublicURL)
	disp := notify.NewDispatcher(notify.DispatcherOptions{
		Store:     store,
		Keys:      cfg.KeyRing,
		Settings:  settingsMgr,
		Catalog:   cat,
		Plugins:   pluginMgr,
		PublicURL: cfg.PublicURL,
	})

	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:               cfg,
		Store:                store,
		Settings:             settingsMgr,
		Catalog:              cat,
		Auth:                 authSvc,
		Passports:            passports,
		PoW:                  pow,
		Plugins:              pluginMgr,
		Content:              cnt,
		Themes:               themesEng,
		Media:                mediaPipe,
		SendVerificationMail: disp.SendVerificationCode,
		NotifyCommentCreated: disp.NotifyCommentCreated,
		TestSMTP:             disp.SendTestSMTP,
		TestWebhook:          disp.TriggerTestWebhook,
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	// -------------------------------------------------------------------------
	// 1. Setup Wizard Initialization & Permanent Lockout
	// -------------------------------------------------------------------------
	setupResp, setupRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/setup/complete", map[string]any{
		"site_title":         "Volo Production Studio",
		"site_subtitle":      "Sovereign Pure-Go Publishing",
		"site_description":   "End-to-end M6 verification",
		"default_locale":     "zh-CN",
		"timezone":           "Asia/Shanghai",
		"accent":             "forest",
		"admin_username":     "founder",
		"admin_email":        "founder@example.com",
		"admin_display_name": "Founder",
		"admin_password":     "Volo#M6Strong!2026",
	}, nil, nil)
	if setupResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/setup/complete = %d %s", setupResp.StatusCode, string(setupRaw))
	}
	// Login via /api/admin/auth/login (since PublicURL is https:// and httptest is http://, capture raw Set-Cookie directly)
	loginResp, loginRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "founder",
		"password": "Volo#M6Strong!2026",
	}, nil, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/auth/login = %d %s", loginResp.StatusCode, string(loginRaw))
	}
	var loginData struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(loginRaw, &loginData)
	adminCookies := loginResp.Cookies()
	adminCSRF := map[string]string{auth.CSRFHeaderName: loginData.CSRFToken}

	// Enable llms.txt + podcast + low PoW difficulty for fast E2E comment test
	if r, b := doJSONReq(t, http.MethodPatch, ts.URL+"/api/admin/settings/feed", map[string]any{
		"enable_llms_txt":     true,
		"enable_raw_markdown": true,
		"podcast":             map[string]any{"enabled": true, "title": "Studio Cast"},
	}, adminCookies, adminCSRF); r.StatusCode != http.StatusOK {
		t.Fatalf("PATCH feed settings = %d %s", r.StatusCode, string(b))
	}
	if r, b := doJSONReq(t, http.MethodPatch, ts.URL+"/api/admin/settings/reading", map[string]any{
		"pow_difficulty_bits":   6,
		"enable_instant_search": true,
	}, adminCookies, adminCSRF); r.StatusCode != http.StatusOK {
		t.Fatalf("PATCH reading settings = %d %s", r.StatusCode, string(b))
	}

	// -------------------------------------------------------------------------
	// 2. Live HTTP Media Upload: GPS EXIF Stripping + Orientation=6 + Adaptive WebP
	// -------------------------------------------------------------------------
	gpsJPEG := makeRotatedGPSJPEG(t, 6) // 120x80 physical + Orientation=6 (90 deg CW) -> upright 80x120!
	if !media.ContainsEXIFOrGPS(gpsJPEG) {
		t.Fatalf("fixture sanity check failed: expected GPS EXIF in input JPEG")
	}

	upResp, upRaw := uploadMultipartMedia(t, ts.URL+"/api/admin/media/upload", "vacation-gps.jpg", "image/jpeg", gpsJPEG, adminCookies, loginData.CSRFToken)
	if upResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/media/upload = %d %s", upResp.StatusCode, string(upRaw))
	}
	var upOut struct {
		Asset domain.MediaAsset `json:"asset"`
		URL   string            `json:"url"`
	}
	if err := json.Unmarshal(upRaw, &upOut); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(upOut.Asset.PublicURL, ".jpg") || upOut.Asset.MIMEType != "image/jpeg" {
		t.Fatalf("expected re-encoded JPEG output (skipping lossless VP8L on JPEG), got url=%q mime=%q", upOut.Asset.PublicURL, upOut.Asset.MIMEType)
	}
	if upOut.Asset.Width != 80 || upOut.Asset.Height != 120 {
		t.Fatalf("expected upright swapped dimensions 80x120, got %dx%d", upOut.Asset.Width, upOut.Asset.Height)
	}
	if !upOut.Asset.EXIFStripped {
		t.Fatalf("expected exif_stripped=true on uploaded photo")
	}

	// Fetch served JPEG over HTTP & verify 100% zero EXIF/GPS residue + Range 206 support
	mediaGetResp, servedBytes := doJSONReq(t, http.MethodGet, ts.URL+upOut.Asset.PublicURL, nil, nil, nil)
	if mediaGetResp.StatusCode != http.StatusOK || len(servedBytes) == 0 {
		t.Fatalf("GET %s = %d (len=%d)", upOut.Asset.PublicURL, mediaGetResp.StatusCode, len(servedBytes))
	}
	if media.ContainsEXIFOrGPS(servedBytes) || bytes.Contains(servedBytes, []byte("GPS_LAT_37.7749N")) || bytes.Contains(servedBytes, []byte("StudioCamera")) {
		t.Fatalf("PRIVACY VIOLATION: served media %s still contains EXIF or GPS metadata!", upOut.Asset.PublicURL)
	}
	rangeResp, rangeBytes := doJSONReq(t, http.MethodGet, ts.URL+upOut.Asset.PublicURL, nil, nil, map[string]string{
		"Range": "bytes=0-11",
	})
	if rangeResp.StatusCode != http.StatusPartialContent || len(rangeBytes) != 12 || rangeBytes[0] != 0xFF || rangeBytes[1] != 0xD8 || rangeBytes[2] != 0xFF {
		t.Fatalf("expected 206 Partial Content JPEG SOI header, got status=%d len=%d", rangeResp.StatusCode, len(rangeBytes))
	}

	// Upload a second unreferenced PNG that will become an orphan
	orphResp, orphRaw := uploadMultipartMedia(t, ts.URL+"/api/admin/media/upload", "unused-draft.png", "image/png", makeSmallPNG(t), adminCookies, loginData.CSRFToken)
	if orphResp.StatusCode != http.StatusCreated {
		t.Fatalf("second upload failed: %d %s", orphResp.StatusCode, string(orphRaw))
	}
	var orphOut struct {
		Asset domain.MediaAsset `json:"asset"`
	}
	_ = json.Unmarshal(orphRaw, &orphOut)

	// -------------------------------------------------------------------------
	// 3. Publish Public Article + Password Vault Article + 9-Scope Orphan Prune
	// -------------------------------------------------------------------------
	catResp, catRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/categories", map[string]any{
		"name": "Engineering",
		"slug": "engineering",
	}, adminCookies, adminCSRF)
	if catResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/categories = %d %s", catResp.StatusCode, string(catRaw))
	}
	var catOut struct {
		Category domain.Category `json:"category"`
	}
	_ = json.Unmarshal(catRaw, &catOut)

	pubArtResp, pubArtRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/articles", map[string]any{
		"category_id":        catOut.Category.ID,
		"title":              "Pure Go Sovereign Engine",
		"slug":               "pure-go-sovereign-engine",
		"content_md":         "Welcome to M6!\n\n![cover](" + upOut.Asset.PublicURL + ")\n\n```go\npackage main\nfunc main() {}\n```",
		"status":             "published",
		"allow_comment":      true,
		"tags":               []string{"golang", "architecture"},
		"audio_url":          "https://cdn.example.com/ep1.mp3",
		"audio_duration_sec": 185,
		"audio_size_bytes":   2048000,
		"audio_mime":         "audio/mpeg",
	}, adminCookies, adminCSRF)
	if pubArtResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/articles (public) = %d %s", pubArtResp.StatusCode, string(pubArtRaw))
	}
	var pubArtOut struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(pubArtRaw, &pubArtOut)

	secretCanary := "M6_VAULT_ULTRA_SECRET_CANARY_991"
	privArtResp, privArtRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/articles", map[string]any{
		"category_id":   catOut.Category.ID,
		"title":         "Classified Design Memo",
		"slug":          "classified-memo",
		"content_md":    "Confidential payload: " + secretCanary,
		"status":        "published",
		"password":      "m6-vault-password",
		"allow_comment": true,
	}, adminCookies, adminCSRF)
	if privArtResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/articles (password) = %d %s", privArtResp.StatusCode, string(privArtRaw))
	}

	// Clean orphan media -> ONLY unused-draft.png (`orphOut.Asset.ID`) must be deleted!
	cleanResp, cleanRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/media/orphans?delete=1", nil, adminCookies, adminCSRF)
	if cleanResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/media/orphans?delete=1 = %d %s", cleanResp.StatusCode, string(cleanRaw))
	}
	var cleanOut struct {
		Orphans []domain.MediaAsset `json:"orphans"`
		Deleted bool                `json:"deleted"`
	}
	_ = json.Unmarshal(cleanRaw, &cleanOut)
	if !cleanOut.Deleted || len(cleanOut.Orphans) != 1 || cleanOut.Orphans[0].ID != orphOut.Asset.ID {
		t.Fatalf("expected exactly 1 pruned orphan (%s), got deleted=%v orphans=%+v", orphOut.Asset.ID, cleanOut.Deleted, cleanOut.Orphans)
	}
	if rGone, _ := doJSONReq(t, http.MethodGet, ts.URL+orphOut.Asset.PublicURL, nil, nil, nil); rGone.StatusCode != http.StatusNotFound {
		t.Fatalf("expected pruned orphan URL %s to return 404, got %d", orphOut.Asset.PublicURL, rGone.StatusCode)
	}
	if rKept, _ := doJSONReq(t, http.MethodGet, ts.URL+upOut.Asset.PublicURL, nil, nil, nil); rKept.StatusCode != http.StatusOK {
		t.Fatalf("referenced article image %s must survive orphan cleanup, got %d", upOut.Asset.PublicURL, rKept.StatusCode)
	}

	// Verify rendered public post includes CLS width="80" height="120" & syntax-highlighted .chroma block,
	// while classified canary never leaks across public surfaces!
	postResp, postHTMLBytes := doJSONReq(t, http.MethodGet, ts.URL+"/posts/pure-go-sovereign-engine", nil, nil, nil)
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /posts/pure-go-sovereign-engine = %d", postResp.StatusCode)
	}
	postHTML := string(postHTMLBytes)
	if !strings.Contains(postHTML, `width="80"`) || !strings.Contains(postHTML, `height="120"`) || !strings.Contains(postHTML, `class="chroma"`) {
		t.Fatalf("expected CLS intrinsic dimensions 80x120 and .chroma code highlighting in rendered post HTML:\n%s", postHTML)
	}
	for _, surf := range []string{"/", "/rss.xml", "/podcast.xml", "/sitemap.xml", "/llms.txt", "/llms-full.txt", "/api/v1/search?mode=index"} {
		sr, sb := doJSONReq(t, http.MethodGet, ts.URL+surf, nil, nil, nil)
		if sr.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", surf, sr.StatusCode)
		}
		if strings.Contains(string(sb), secretCanary) {
			t.Fatalf("PRIVACY LEAK on %s: found password-protected canary!", surf)
		}
	}

	// -------------------------------------------------------------------------
	// 4. External `./plugins/kanbanniang` Enable, Persistent Dock & `plugin_kv`
	// -------------------------------------------------------------------------
	plListResp, plListRaw := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/plugins", nil, adminCookies, nil)
	if plListResp.StatusCode != http.StatusOK || !strings.Contains(string(plListRaw), `"id":"kanbanniang"`) {
		t.Fatalf("expected external plugin kanbanniang in GET /api/admin/plugins, got %d %s", plListResp.StatusCode, string(plListRaw))
	}
	enResp, enRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/plugins/kanbanniang/enable", nil, adminCookies, adminCSRF)
	if enResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/plugins/kanbanniang/enable = %d %s", enResp.StatusCode, string(enRaw))
	}

	// Pat mascot twice via `/plugins/kanbanniang/api/pat`
	for wantPats := 1; wantPats <= 2; wantPats++ {
		patResp, patRaw := doJSONReq(t, http.MethodPost, ts.URL+"/plugins/kanbanniang/api/pat", nil, nil, nil)
		if patResp.StatusCode != http.StatusOK {
			t.Fatalf("POST /plugins/kanbanniang/api/pat = %d %s", patResp.StatusCode, string(patRaw))
		}
		var patOut struct {
			OK        bool  `json:"ok"`
			TotalPats int64 `json:"total_pats"`
		}
		_ = json.Unmarshal(patRaw, &patOut)
		if !patOut.OK || int(patOut.TotalPats) != wantPats {
			t.Fatalf("expected total_pats=%d, got %+v", wantPats, patOut)
		}
	}

	// Verify public home renders persistent mascot island with `data-pats="2"` and static plugin JS serves 200 OK
	_, homeWithMascot := doJSONReq(t, http.MethodGet, ts.URL+"/", nil, nil, nil)
	homeMascotStr := string(homeWithMascot)
	if !strings.Contains(homeMascotStr, `id="volo-kanbanniang-root"`) ||
		!strings.Contains(homeMascotStr, `<canvas id="soloKanbanniang"`) ||
		!strings.Contains(homeMascotStr, `data-pats="2"`) ||
		!strings.Contains(homeMascotStr, `/plugins/kanbanniang/assets/widget.js`) {
		t.Fatalf("expected persistent kanbanniang Live2D widget with data-pats=\"2\" on GET /, got:\n%s", homeMascotStr)
	}
	if wResp, wBody := doJSONReq(t, http.MethodGet, ts.URL+"/plugins/kanbanniang/assets/widget.js", nil, nil, nil); wResp.StatusCode != http.StatusOK || len(wBody) == 0 {
		t.Fatalf("GET /plugins/kanbanniang/assets/widget.js = %d", wResp.StatusCode)
	}
	if l2dResp, l2dBody := doJSONReq(t, http.MethodGet, ts.URL+"/plugins/kanbanniang/assets/live2d.js?v=2.0.0", nil, nil, nil); l2dResp.StatusCode != http.StatusOK || len(l2dBody) == 0 {
		t.Fatalf("GET /plugins/kanbanniang/assets/live2d.js = %d", l2dResp.StatusCode)
	}
	if mdlResp, mdlBody := doJSONReq(t, http.MethodGet, ts.URL+"/plugins/kanbanniang/api/model?model=Potion-Maker/Pio", nil, nil, nil); mdlResp.StatusCode != http.StatusOK || !strings.Contains(string(mdlBody), `"Potion-Maker/Pio"`) || (!strings.Contains(string(mdlBody), `"textures_webp"`) && !strings.Contains(string(mdlBody), `.webp"`)) {
		t.Fatalf("GET /plugins/kanbanniang/api/model = %d %s", mdlResp.StatusCode, string(mdlBody))
	}

	// -------------------------------------------------------------------------
	// 5. External `./themes/volo-journal` Discovery, `?skin=` Preview & Activation
	// -------------------------------------------------------------------------
	thListResp, thListRaw := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/themes", nil, adminCookies, nil)
	if thListResp.StatusCode != http.StatusOK || !strings.Contains(string(thListRaw), `"volo-journal"`) {
		t.Fatalf("expected external theme volo-journal in GET /api/admin/themes, got %d %s", thListResp.StatusCode, string(thListRaw))
	}

	// Admin previews `?skin=volo-journal` while active theme is still `volo-studio`
	prevResp, prevBytes := doJSONReq(t, http.MethodGet, ts.URL+"/?skin=volo-journal", nil, adminCookies, nil)
	if prevResp.StatusCode != http.StatusOK || !strings.Contains(string(prevBytes), `data-volo-skin="volo-journal"`) {
		t.Fatalf("expected admin ?skin=volo-journal preview to render data-volo-skin=\"volo-journal\", got status=%d", prevResp.StatusCode)
	}
	// Activate `volo-journal` globally and verify anonymous visitor gets `volo-journal` + inherited `volo-studio.css` + `volo-journal.css` + mascot!
	actResp, actRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/themes/activate", map[string]any{
		"theme_id": "volo-journal",
	}, adminCookies, adminCSRF)
	if actResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/themes/activate = %d %s", actResp.StatusCode, string(actRaw))
	}
	anonResp, anonBytes := doJSONReq(t, http.MethodGet, ts.URL+"/", nil, nil, nil)
	anonHTML := string(anonBytes)
	if anonResp.StatusCode != http.StatusOK ||
		!strings.Contains(anonHTML, `data-volo-skin="volo-journal"`) ||
		!strings.Contains(anonHTML, `id="volo-kanbanniang-root"`) {
		t.Fatalf("expected anonymous GET / to render activated volo-journal + kanbanniang mascot without conflict")
	}
	for _, cssAsset := range []string{"/themes/volo-journal/assets/volo-journal.css", "/themes/volo-journal/assets/volo-studio.css"} {
		cr, cb := doJSONReq(t, http.MethodGet, ts.URL+cssAsset, nil, nil, nil)
		if cr.StatusCode != http.StatusOK || len(cb) == 0 {
			t.Fatalf("GET %s = %d (len=%d)", cssAsset, cr.StatusCode, len(cb))
		}
	}

	// -------------------------------------------------------------------------
	// 6. Guest PoW Challenge + Persistent Guest Passport Comment Submission
	// -------------------------------------------------------------------------
	_, powRaw := doJSONReq(t, http.MethodGet, ts.URL+"/api/v1/pow/challenge", nil, nil, nil)
	var chal guest.PoWChallenge
	if err := json.Unmarshal(powRaw, &chal); err != nil {
		t.Fatal(err)
	}
	nonce := guest.SolveForTest(chal.Challenge, chal.DifficultyBits)

	cmtResp, cmtRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/v1/comments", map[string]any{
		"target_type":   "article",
		"target_id":     pubArtOut.Article.ID,
		"author_name":   "M6 Visitor",
		"notify_email":  "visitor@example.com",
		"content_md":    "Clean-slate pure Go feels blazing fast!",
		"pow_challenge": chal.Challenge,
		"pow_solution":  nonce,
		"env_fp":        "2560x1440|2|Asia/Shanghai|zh-CN|12",
	}, nil, nil)
	if cmtResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/comments = %d %s", cmtResp.StatusCode, string(cmtRaw))
	}
	_, listCmtRaw := doJSONReq(t, http.MethodGet, ts.URL+"/api/v1/comments?target_type=article&target_id="+pubArtOut.Article.ID, nil, nil, nil)
	if !strings.Contains(string(listCmtRaw), "Clean-slate pure Go feels blazing fast!") {
		t.Fatalf("expected guest comment in public comment list, got: %s", string(listCmtRaw))
	}
}
