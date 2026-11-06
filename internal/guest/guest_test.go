package guest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
)

func TestGuestPassportSelfHealingAndPasswordBoundUnlock(t *testing.T) {
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("k", 32), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pm := NewPassportManager(kr, "https://blog.example.com")

	// 1. First visit mints a new GuestPassport and sets HttpOnly Secure cookie
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	id1, ok := pm.ResolveOrHeal(rec1, req1, true)
	if !ok || id1.Source != "minted" {
		t.Fatalf("expected minted guest passport, got %+v", id1)
	}
	cookies1 := rec1.Result().Cookies()
	if len(cookies1) != 1 || !cookies1[0].HttpOnly || !cookies1[0].Secure {
		t.Fatalf("expected HttpOnly+Secure cookie, got %+v", cookies1)
	}

	// 2. Simulate Safari ITP wiping cookie, while localStorage/IndexedDB sends X-Volo-Guest-Passport header -> MUST self-heal cookie!
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set(PassportHeaderName, id1.SignedToken)
	id2, ok := pm.ResolveOrHeal(rec2, req2, false)
	if !ok || id2.GuestID != id1.GuestID || id2.Source != "header_healed" {
		t.Fatalf("expected header_healed passport matching %s, got %+v", id1.GuestID, id2)
	}
	if len(rec2.Result().Cookies()) != 1 {
		t.Fatal("expected self-healed Set-Cookie on header recovery")
	}

	// 3. Forged / tampered GuestPassport header MUST be rejected!
	reqTampered := httptest.NewRequest(http.MethodGet, "/", nil)
	reqTampered.Header.Set(PassportHeaderName, id1.SignedToken+"tamper")
	if _, okBad := pm.ResolveOrHeal(nil, reqTampered, false); okBad {
		t.Fatal("expected forged GuestPassport token to be rejected")
	}

	// 4. Persistent Article Unlock bound to BOTH GuestID AND SHA256(currentPassword)!
	recUnlock := httptest.NewRecorder()
	reqUnlock := httptest.NewRequest(http.MethodPost, "/posts/secret/unlock", nil)
	_, err = pm.GrantArticleUnlock(recUnlock, reqUnlock, id1.GuestID, "art_secret", "pass-v1")
	if err != nil {
		t.Fatalf("GrantArticleUnlock failed: %v", err)
	}
	unlockCookie := recUnlock.Result().Cookies()[0]

	reqCheck := httptest.NewRequest(http.MethodGet, "/posts/secret", nil)
	reqCheck.AddCookie(unlockCookie)
	if !pm.HasArticleUnlock(reqCheck, id1.GuestID, "art_secret", "pass-v1") {
		t.Fatal("expected HasArticleUnlock=true for matching guestID and password")
	}
	// Another guest stealing or replaying under a different GuestID MUST fail!
	if pm.HasArticleUnlock(reqCheck, "gp_other_visitor", "art_secret", "pass-v1") {
		t.Fatal("unlock cookie must not validate for a different GuestID")
	}
	// If author rotates the article password from "pass-v1" to "pass-v2", old unlock cookie MUST immediately fail!
	if pm.HasArticleUnlock(reqCheck, id1.GuestID, "art_secret", "pass-v2") {
		t.Fatal("unlock cookie must auto-invalidate when article password changes")
	}
}

func TestPoWChallengeReplayHoneypotEnvFPAndAvatarSVG(t *testing.T) {
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("p", 32), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// 1. PoW challenge + solution + replay & honeypot defense
	pow := NewPoWVerifier(kr)
	ch, err := pow.IssueChallenge(10)
	if err != nil {
		t.Fatal(err)
	}
	sol := SolveForTest(ch.Challenge, ch.DifficultyBits)

	if err := pow.Verify(ch.Challenge, sol, "bot-filled-website"); !errors.Is(err, ErrHoneypotTriggered) {
		t.Fatalf("expected ErrHoneypotTriggered, got %v", err)
	}
	if err := pow.Verify(ch.Challenge, "invalid-nonce", ""); !errors.Is(err, ErrPoWInvalid) {
		t.Fatalf("expected ErrPoWInvalid on wrong solution, got %v", err)
	}
	if err := pow.Verify(ch.Challenge, sol, ""); err != nil {
		t.Fatalf("expected valid PoW solution to pass, got %v", err)
	}
	if err := pow.Verify(ch.Challenge, sol, ""); !errors.Is(err, ErrPoWReplayed) {
		t.Fatalf("expected ErrPoWReplayed on replay attempt, got %v", err)
	}

	// 2. Peppered env_fp determinism & non-empty check
	fp1 := ComputeEnvFPHash(kr, "1920x1080|2|Asia/Shanghai|zh-CN|8")
	fp2 := ComputeEnvFPHash(kr, "1920x1080|2|Asia/Shanghai|zh-CN|8")
	if fp1 == "" || fp1 != fp2 {
		t.Fatalf("expected deterministic peppered env_fp hash, got %q vs %q", fp1, fp2)
	}

	// 3. Deterministic offline SVG avatar + 304 ETag negotiation
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/avatars/alice.svg", nil)
	ServeAvatarHTTP(rec, req, "alice.svg")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg ") {
		t.Fatalf("unexpected SVG avatar response: code=%d body=%s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	rec304 := httptest.NewRecorder()
	req304 := httptest.NewRequest(http.MethodGet, "/avatars/alice.svg", nil)
	req304.Header.Set("If-None-Match", etag)
	ServeAvatarHTTP(rec304, req304, "alice.svg")
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified on matching ETag, got %d", rec304.Code)
	}
}
