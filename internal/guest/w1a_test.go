package guest

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
)

func TestPassport_IssueAndWriteUnlockCookieWithSecure(t *testing.T) {
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("g", 32), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pm := NewPassportManager(kr, "http://localhost:8080")
	gp := GuestPassport{GuestID: "guest_abc123", IssuedAtMS: 1700000000000}

	recSecure := httptest.NewRecorder()
	pm.IssueCookieWithSecure(recSecure, gp, true)
	pm.WriteUnlockCookieWithSecure(recSecure, "bundle.sig", true)
	cookiesSecure := recSecure.Result().Cookies()
	if len(cookiesSecure) != 2 {
		t.Fatalf("expected 2 cookies, got %d", len(cookiesSecure))
	}
	for _, c := range cookiesSecure {
		if !c.Secure || !c.HttpOnly {
			t.Fatalf("expected Secure=true HttpOnly=true on %s, got Secure=%v HttpOnly=%v", c.Name, c.Secure, c.HttpOnly)
		}
	}

	// Verify explicit secure=false works even when manager publicURL is https://
	pm.SetPublicURL("https://blog.example.com")
	recPlain := httptest.NewRecorder()
	pm.IssueCookieWithSecure(recPlain, gp, false)
	pm.WriteUnlockCookieWithSecure(recPlain, "bundle.sig", false)
	for _, c := range recPlain.Result().Cookies() {
		if c.Secure {
			t.Fatalf("expected explicit Secure=false to be honored on %s", c.Name)
		}
	}

	// Verify legacy delegating IssueCookie / WriteUnlockCookie inherit manager https policy
	recDelegate := httptest.NewRecorder()
	pm.IssueCookie(recDelegate, gp)
	pm.WriteUnlockCookie(recDelegate, "bundle.sig")
	for _, c := range recDelegate.Result().Cookies() {
		if !c.Secure {
			t.Fatalf("expected delegating helper to use https Secure=true on %s", c.Name)
		}
	}
}
