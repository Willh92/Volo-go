package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/pkg/id"
)

func TestMediaOrphanCacheSingleflightAndBoundedUsagePagination(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	now := time.Now().UnixMilli()

	// Complete setup
	stResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	var setupCSRF string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			setupCSRF = c.Value
		}
	}
	okSetup, okSetupBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"admin_username":     "medadmin",
		"admin_email":        "med@example.com",
		"admin_password":     "pass1234",
		"admin_display_name": "Med Admin",
		"site_title":         "Med Test",
		"default_locale":     "en-US",
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": setupCSRF})
	if okSetup.StatusCode != http.StatusOK {
		t.Fatalf("setup complete: %d", okSetup.StatusCode)
	}
	adminCookies := okSetup.Cookies()
	var setupDone struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(okSetupBody, &setupDone)
	adminCSRF := map[string]string{"X-CSRF-Token": setupDone.CSRFToken}

	// Seed 35 unused assets and 15 used assets (referenced in a published article).
	var mdRefs strings.Builder
	for i := 0; i < 50; i++ {
		aid := fmt.Sprintf("med_%03d_%s", i, id.Next())
		objKey := fmt.Sprintf("2026/11/img-%02d.webp", i)
		pubURL := "/uploads/" + objKey
		size := int64(1000 + i)
		if _, err := env.store.ExecContext(env.ctx,
			`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
			 VALUES (?, 'local', ?, ?, ?, '', 'image/webp', ?, 100, 100, ?, 1, '', ?)`,
			aid, objKey, pubURL, fmt.Sprintf("img-%02d.webp", i), size, fmt.Sprintf("sha-%02d", i), now+int64(i)); err != nil {
			t.Fatalf("seed media %d: %v", i, err)
		}
		if i < 15 {
			fmt.Fprintf(&mdRefs, "![img](%s)\n", pubURL)
		}
	}

	artResp, artBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Media Ref Post",
		"slug":       "media-ref-post",
		"content_md": mdRefs.String(),
		"status":     "published",
	}, adminCookies, adminCSRF)
	if artResp.StatusCode != http.StatusCreated {
		t.Fatalf("create article: %d %s", artResp.StatusCode, string(artBody))
	}

	// 1. Concurrent GET /api/admin/media?usage=unused (16 callers) on fresh cache -> all agree on counts.
	type mediaPageResp struct {
		Items []struct {
			ID       string `json:"id"`
			IsOrphan bool   `json:"is_orphan"`
			RefCount int    `json:"ref_count"`
		} `json:"items"`
		Total       int   `json:"total"`
		AllTotal    int   `json:"all_total"`
		OrphanCount int   `json:"orphan_count"`
		TotalBytes  int64 `json:"total_bytes"`
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	results := make(chan mediaPageResp, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, env.ts.URL+"/api/admin/media?usage=unused&page=2&per_page=10", nil)
			for _, c := range adminCookies {
				req.AddCookie(c)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			var out mediaPageResp
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				errs <- err
				return
			}
			results <- out
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatalf("concurrent media query error: %v", err)
	}
	for res := range results {
		if len(res.Items) != 10 || res.Total != 35 || res.AllTotal != 50 || res.OrphanCount != 35 {
			t.Fatalf("unexpected concurrent unused page 2: items=%d total=%d all=%d orphan=%d",
				len(res.Items), res.Total, res.AllTotal, res.OrphanCount)
		}
		for _, it := range res.Items {
			if !it.IsOrphan || it.RefCount != 0 {
				t.Fatalf("expected orphan item on unused page, got %+v", it)
			}
		}
	}

	// 2. Verify usage=used page 2 and unfiltered all with_refs=1 orphan_count match exact counts.
	usedResp, usedBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/media?usage=used&page=2&per_page=10", nil, adminCookies, nil)
	if usedResp.StatusCode != http.StatusOK {
		t.Fatalf("usage=used failed: %d", usedResp.StatusCode)
	}
	var usedOut mediaPageResp
	if err := json.Unmarshal(usedBody, &usedOut); err != nil {
		t.Fatal(err)
	}
	if len(usedOut.Items) != 5 || usedOut.Total != 15 || usedOut.OrphanCount != 35 {
		t.Fatalf("unexpected usage=used page 2: items=%d total=%d orphan=%d", len(usedOut.Items), usedOut.Total, usedOut.OrphanCount)
	}
	for _, it := range usedOut.Items {
		if it.IsOrphan || it.RefCount == 0 {
			t.Fatalf("expected used item, got %+v", it)
		}
	}

	allResp, allBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/media?with_refs=1&page=1&per_page=10", nil, adminCookies, nil)
	if allResp.StatusCode != http.StatusOK {
		t.Fatalf("with_refs=1 failed: %d", allResp.StatusCode)
	}
	var allOut mediaPageResp
	_ = json.Unmarshal(allBody, &allOut)
	if allOut.Total != 50 || allOut.OrphanCount != 35 || len(allOut.Items) != 10 {
		t.Fatalf("unexpected all with_refs=1: total=%d orphan=%d items=%d", allOut.Total, allOut.OrphanCount, len(allOut.Items))
	}
}
