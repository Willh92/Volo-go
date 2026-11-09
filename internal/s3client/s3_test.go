package s3client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestListObjectsPaginatedAndCanonicalQuerySigV4(t *testing.T) {
	fixedNow := time.Date(2026, 11, 8, 3, 4, 5, 0, time.UTC)
	const (
		region    = "us-east-1"
		accessKey = "AKIA_SIGV4"
		secretKey = "sigv4-super-secret"
	)

	// Verify RFC 3986 query escaping & parameter sorting (spaces -> %20, tilde unescaped, keys sorted).
	rawQ := "prefix=daily/volo%20auto~1&list-type=2&continuation-token=tok%2B2%2Fa"
	canonQ := canonicalQueryString(rawQ)
	wantCanonQ := "continuation-token=tok%2B2%2Fa&list-type=2&prefix=daily%2Fvolo%20auto~1"
	if canonQ != wantCanonQ {
		t.Fatalf("canonicalQueryString mismatch:\n got: %s\nwant: %s", canonQ, wantCanonQ)
	}

	var calls int
	var deletedKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Recompute expected SigV4 signature on the server side to guarantee query params were signed!
		expectedCanonReq := strings.Join([]string{
			r.Method,
			r.URL.EscapedPath(),
			canonicalQueryString(r.URL.RawQuery),
			"host:" + r.Host + "\n" +
				"x-amz-content-sha256:" + r.Header.Get("X-Amz-Content-Sha256") + "\n" +
				"x-amz-date:" + r.Header.Get("X-Amz-Date") + "\n",
			"host;x-amz-content-sha256;x-amz-date",
			r.Header.Get("X-Amz-Content-Sha256"),
		}, "\n")
		scope := "20261108/" + region + "/s3/aws4_request"
		sts := strings.Join([]string{
			"AWS4-HMAC-SHA256",
			"20261108T030405Z",
			scope,
			hmacSHA256HexForTest(expectedCanonReq),
		}, "\n")
		sig := hmacSHA256Hex(deriveSigV4Key(secretKey, "20261108", region, "s3"), sts)
		wantAuth := "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope + ", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=" + sig
		if r.Header.Get("Authorization") != wantAuth {
			t.Errorf("SigV4 Authorization mismatch on %s %s:\n got: %s\nwant: %s", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), wantAuth)
			w.WriteHeader(http.StatusForbidden)
			return
		}

		if r.Method == http.MethodDelete {
			deletedKey = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "" {
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <IsTruncated>true</IsTruncated>
  <NextContinuationToken>page 2+tok~ok</NextContinuationToken>
  <Contents>
    <Key>daily/volo-auto-20261108-010000-001.zip</Key>
    <LastModified>2026-11-08T01:00:00Z</LastModified>
    <Size>1024</Size>
  </Contents>
</ListBucketResult>`))
			return
		}
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>daily/volo-auto-20261108-020000-002.zip</Key>
    <LastModified>2026-11-08T02:00:00.500Z</LastModified>
    <Size>2048</Size>
  </Contents>
</ListBucketResult>`))
	}))
	defer srv.Close()

	cli, err := NewClient(Config{
		Endpoint:  srv.URL,
		Region:    region,
		Bucket:    "backups",
		AccessKey: accessKey,
		SecretKey: secretKey,
		KeyPrefix: "daily",
		PathStyle: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cli.nowFn = func() time.Time { return fixedNow }

	objs, err := cli.ListObjects(context.Background(), "volo-auto-")
	if err != nil {
		t.Fatalf("ListObjects failed: %v", err)
	}
	if len(objs) != 2 || calls != 2 {
		t.Fatalf("expected 2 objects across 2 pages, got len=%d calls=%d objs=%+v", len(objs), calls, objs)
	}
	if objs[0].Key != "volo-auto-20261108-010000-001.zip" || objs[1].Key != "volo-auto-20261108-020000-002.zip" {
		t.Fatalf("expected KeyPrefix stripped from returned keys, got %+v", objs)
	}
	if err := cli.DeleteObject(context.Background(), objs[0].Key); err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}
	if deletedKey != "/backups/daily/volo-auto-20261108-010000-001.zip" {
		t.Fatalf("unexpected DeleteObject path %q", deletedKey)
	}
}

func hmacSHA256HexForTest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
