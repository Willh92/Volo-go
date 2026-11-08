package s3client

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientRejectsUnsafePublicBaseURLAndScalesStreamTimeout(t *testing.T) {
	// 1. Reject javascript:, data:, or hostless PublicBaseURL
	for _, bad := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"ftp://files.example.com/pub",
		"/relative/only",
	} {
		_, err := NewClient(Config{
			Endpoint:      "https://s3.example.com",
			Region:        "auto",
			Bucket:        "volo",
			AccessKey:     "AKIA_TEST",
			SecretKey:     "SECRET_TEST",
			PublicBaseURL: bad,
		}, nil)
		if err == nil {
			t.Fatalf("expected NewClient to reject unsafe PublicBaseURL %q", bad)
		}
	}

	// 2. Adaptive stream timeout scales cleanly from 90s up to 30m cap.
	if got := adaptivePutStreamTimeout(1024); got != 90*time.Second {
		t.Fatalf("expected 90s minimum stream timeout, got %v", got)
	}
	large500MB := adaptivePutStreamTimeout(500 << 20)
	if large500MB <= 90*time.Second || large500MB > 30*time.Minute {
		t.Fatalf("expected 500 MiB stream timeout in (90s, 30m], got %v", large500MB)
	}
	if capped := adaptivePutStreamTimeout(50 << 30); capped != 30*time.Minute {
		t.Fatalf("expected 30m ceiling on huge stream timeout, got %v", capped)
	}

	// 3. End-to-end PutStream & PublicURL against local SigV4 mock endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli, err := NewClient(Config{
		Endpoint:      srv.URL,
		Region:        "us-east-1",
		Bucket:        "media",
		AccessKey:     "AKIA123",
		SecretKey:     "secret456",
		PublicBaseURL: "https://cdn.example.com/assets",
		PathStyle:     true,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient valid config failed: %v", err)
	}
	gotURL, err := cli.PutStream(context.Background(), "2026/11/clip.mp4", bytes.NewReader([]byte("mp4-bytes")), 9, "video/mp4")
	if err != nil {
		t.Fatalf("PutStream failed: %v", err)
	}
	if gotURL != "https://cdn.example.com/assets/2026/11/clip.mp4" {
		t.Fatalf("unexpected PublicURL: %q", gotURL)
	}
}
