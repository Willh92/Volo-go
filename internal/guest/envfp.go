package guest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Willh92/Volo-go/internal/config"
)

const EnvFPHeaderName = "X-Volo-Env-FP"

// ComputeEnvFPHash derives a server-peppered, irreversible HMAC-SHA256 digest from lightweight
// client environment signals (screen/DPR/timezone/lang/hardwareConcurrency/WebGL renderer).
//
// SECURITY RED-LINE INVARIANT:
// EnvFPHash MUST ONLY be used for auxiliary anti-spam rate-limiting, UV deduplication, and
// reaction deduplication. It MUST NEVER grant permission to edit/delete comments, unlock
// protected content, or claim historical guest comments!
func ComputeEnvFPHash(kr *config.KeyRing, rawClientFP string) string {
	cleaned := strings.TrimSpace(rawClientFP)
	if cleaned == "" || kr == nil {
		return ""
	}
	if len(cleaned) > 512 {
		cleaned = cleaned[:512]
	}
	pepper := kr.EnvFPPepperKey()
	mac := hmac.New(sha256.New, pepper[:])
	mac.Write([]byte("volo-env-fp:v1:"))
	mac.Write([]byte(cleaned))
	return hex.EncodeToString(mac.Sum(nil))
}
