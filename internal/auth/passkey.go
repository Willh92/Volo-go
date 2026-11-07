package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/pkg/id"
)

const (
	COSEAlgES256 = -7
	COSEAlgRS256 = -257

	authFlagUserPresent = 0x01
	authFlagUserVerify  = 0x04
	authFlagAttested    = 0x40

	maxCBORBytes = 64 * 1024
	maxCBORDepth = 8

	maxSeenPasskeyChallenges = 4096
	passkeyChallengeTTL      = 5 * time.Minute
)

var (
	ErrPasskeyVerifyFailed  = errors.New("auth: passkey ceremony verification failed")
	ErrPasskeyCloneDetected = errors.New("auth: passkey signature counter regression (possible cloned authenticator)")

	seenChallengesMu sync.Mutex
	seenChallenges   = make(map[string]int64)
)

func consumePasskeyChallengeOnce(purpose string, challengeB64 string, envExpiresAt int64) bool {
	if challengeB64 == "" {
		return false
	}
	key := purpose + ":" + challengeB64
	nowMS := time.Now().UnixMilli()

	seenChallengesMu.Lock()
	defer seenChallengesMu.Unlock()

	for k, exp := range seenChallenges {
		if nowMS > exp {
			delete(seenChallenges, k)
		}
	}
	if _, exists := seenChallenges[key]; exists {
		return false
	}
	for len(seenChallenges) >= maxSeenPasskeyChallenges {
		var oldestKey string
		var oldestExp int64
		first := true
		for k, exp := range seenChallenges {
			if first || exp < oldestExp {
				oldestKey = k
				oldestExp = exp
				first = false
			}
		}
		if first {
			break
		}
		delete(seenChallenges, oldestKey)
	}
	expMS := nowMS + int64(passkeyChallengeTTL/time.Millisecond)
	if envExpiresAt > expMS {
		expMS = envExpiresAt
	}
	seenChallenges[key] = expMS
	return true
}

type passkeyChallengeEnvelope struct {
	ChallengeB64 string `json:"c"`
	UserID       string `json:"u"`
	Purpose      string `json:"p"` // "register" | "login"
	RPID         string `json:"r"`
	Origin       string `json:"o"`
	ExpiresAt    int64  `json:"e"`
}

// PasskeyBeginResult contains both the browser PublicKeyCredentialCreationOptions/RequestOptions challenge
// and the server-sealed state token to submit back on completion.
type PasskeyBeginResult struct {
	ChallengeB64URL string `json:"challenge"`
	RPID            string `json:"rp_id"`
	Origin          string `json:"origin"`
	StateToken      string `json:"state_token"`
}

// VerifiedPasskeyRegistration holds validated credential material ready to persist in user_credentials.
type VerifiedPasskeyRegistration struct {
	CredentialIDB64URL string
	PublicKeySPKIB64   string
	COSEAlg            int
	SignCount          int64
}

// DeriveRPIDAndOrigin extracts canonical WebAuthn RP ID (hostname) and Origin from configured PublicURL.
func DeriveRPIDAndOrigin(publicURL string) (rpID string, origin string, err error) {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil || u.Hostname() == "" {
		return "", "", errors.New("auth: invalid VOLO_PUBLIC_URL for WebAuthn RP ID")
	}
	return strings.ToLower(u.Hostname()), strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

// BeginPasskeyCeremony creates a fresh 32-byte WebAuthn challenge sealed inside an AES-256-GCM state token.
func BeginPasskeyCeremony(kr *config.KeyRing, userID string, purpose string, publicURL string) (*PasskeyBeginResult, error) {
	rpID, origin, err := DeriveRPIDAndOrigin(publicURL)
	if err != nil {
		return nil, err
	}
	rawChal, err := id.RandomHex(32)
	if err != nil {
		return nil, err
	}
	chalB64 := base64.RawURLEncoding.EncodeToString([]byte(rawChal[:32]))
	env := passkeyChallengeEnvelope{
		ChallengeB64: chalB64,
		UserID:       userID,
		Purpose:      purpose,
		RPID:         rpID,
		Origin:       origin,
		ExpiresAt:    time.Now().Add(5 * time.Minute).UnixMilli(),
	}
	b, _ := json.Marshal(env)
	sealed, err := kr.SealString(string(b), "auth:passkey:"+purpose)
	if err != nil {
		return nil, err
	}
	return &PasskeyBeginResult{
		ChallengeB64URL: chalB64,
		RPID:            rpID,
		Origin:          origin,
		StateToken:      sealed,
	}, nil
}

// FinishPasskeyRegistration validates a WebAuthn navigator.credentials.create() response (ES256 or RS256).
func FinishPasskeyRegistration(
	kr *config.KeyRing,
	stateToken string,
	expectedUserID string,
	clientDataJSON []byte,
	attestationObjectCBOR []byte,
) (*VerifiedPasskeyRegistration, error) {
	env, err := openPasskeyState(kr, stateToken, "register")
	if err != nil || env.UserID == "" || env.UserID != expectedUserID {
		return nil, ErrPasskeyVerifyFailed
	}

	if err := verifyClientDataJSON(clientDataJSON, "webauthn.create", env.ChallengeB64, env.Origin); err != nil {
		return nil, err
	}

	if len(attestationObjectCBOR) == 0 || len(attestationObjectCBOR) > maxCBORBytes {
		return nil, ErrPasskeyVerifyFailed
	}
	val, rest, err := decodeCBORItem(attestationObjectCBOR, 0)
	if err != nil || len(rest) != 0 {
		return nil, ErrPasskeyVerifyFailed
	}
	attMap, ok := val.(map[any]any)
	if !ok {
		return nil, ErrPasskeyVerifyFailed
	}
	authData, ok := attMap["authData"].([]byte)
	if !ok || len(authData) < 55 {
		return nil, ErrPasskeyVerifyFailed
	}

	// authData layout: rpIdHash(32) || flags(1) || signCount(4) || aaguid(16) || credIdLen(2) || credId || coseKeyCBOR
	rpHash := sha256.Sum256([]byte(env.RPID))
	if subtle.ConstantTimeCompare(authData[:32], rpHash[:]) != 1 {
		return nil, ErrPasskeyVerifyFailed
	}
	flags := authData[32]
	if flags&authFlagUserPresent == 0 || flags&authFlagAttested == 0 {
		return nil, ErrPasskeyVerifyFailed
	}
	signCount := int64(binary.BigEndian.Uint32(authData[33:37]))
	credIDLen := int(binary.BigEndian.Uint16(authData[53:55]))
	if credIDLen < 16 || credIDLen > 1024 || len(authData) < 55+credIDLen+1 {
		return nil, ErrPasskeyVerifyFailed
	}
	credID := authData[55 : 55+credIDLen]
	coseRaw := authData[55+credIDLen:]

	coseVal, _, err := decodeCBORItem(coseRaw, 0)
	if err != nil {
		return nil, ErrPasskeyVerifyFailed
	}
	coseMap, ok := coseVal.(map[any]any)
	if !ok {
		return nil, ErrPasskeyVerifyFailed
	}

	spkiBytes, coseAlg, err := parseCOSEPublicKeyToSPKI(coseMap)
	if err != nil {
		return nil, err
	}

	return &VerifiedPasskeyRegistration{
		CredentialIDB64URL: base64.RawURLEncoding.EncodeToString(credID),
		PublicKeySPKIB64:   base64.StdEncoding.EncodeToString(spkiBytes),
		COSEAlg:            coseAlg,
		SignCount:          signCount,
	}, nil
}

// FinishPasskeyAssertion validates a WebAuthn navigator.credentials.get() assertion, verifying
// origin, challenge, RP ID hash, User Presence, cryptographic signature, and monotonic signCount!
func FinishPasskeyAssertion(
	kr *config.KeyRing,
	stateToken string,
	storedPublicKeySPKIB64 string,
	storedCOSEAlg int,
	storedSignCount int64,
	clientDataJSON []byte,
	authenticatorData []byte,
	signature []byte,
) (newSignCount int64, err error) {
	env, err := openPasskeyState(kr, stateToken, "login")
	if err != nil {
		return 0, ErrPasskeyVerifyFailed
	}
	if err := verifyClientDataJSON(clientDataJSON, "webauthn.get", env.ChallengeB64, env.Origin); err != nil {
		return 0, err
	}
	if len(authenticatorData) < 37 || len(signature) == 0 || len(signature) > 1024 {
		return 0, ErrPasskeyVerifyFailed
	}

	rpHash := sha256.Sum256([]byte(env.RPID))
	if subtle.ConstantTimeCompare(authenticatorData[:32], rpHash[:]) != 1 {
		return 0, ErrPasskeyVerifyFailed
	}
	if authenticatorData[32]&authFlagUserPresent == 0 {
		return 0, ErrPasskeyVerifyFailed
	}
	newSignCount = int64(binary.BigEndian.Uint32(authenticatorData[33:37]))

	spkiBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(storedPublicKeySPKIB64))
	if err != nil {
		return 0, ErrPasskeyVerifyFailed
	}
	pubAny, err := x509.ParsePKIXPublicKey(spkiBytes)
	if err != nil {
		return 0, ErrPasskeyVerifyFailed
	}

	// Signed message in WebAuthn Level 2: authenticatorData || SHA256(clientDataJSON)
	clientHash := sha256.Sum256(clientDataJSON)
	signedPayload := make([]byte, 0, len(authenticatorData)+32)
	signedPayload = append(signedPayload, authenticatorData...)
	signedPayload = append(signedPayload, clientHash[:]...)
	digest := sha256.Sum256(signedPayload)

	switch storedCOSEAlg {
	case COSEAlgES256:
		ecPub, ok := pubAny.(*ecdsa.PublicKey)
		if !ok || !ecdsa.VerifyASN1(ecPub, digest[:], signature) {
			return 0, ErrPasskeyVerifyFailed
		}
	case COSEAlgRS256:
		rsaPub, ok := pubAny.(*rsa.PublicKey)
		if !ok || rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, digest[:], signature) != nil {
			return 0, ErrPasskeyVerifyFailed
		}
	default:
		return 0, ErrPasskeyVerifyFailed
	}

	// Clone detection: if the authenticator implements a non-zero signature counter,
	// any regression or stagnation MUST be rejected!
	if storedSignCount > 0 && newSignCount <= storedSignCount {
		return 0, ErrPasskeyCloneDetected
	}
	return newSignCount, nil
}

func openPasskeyState(kr *config.KeyRing, stateToken string, expectedPurpose string) (*passkeyChallengeEnvelope, error) {
	opened, err := kr.OpenString(strings.TrimSpace(stateToken), "auth:passkey:"+expectedPurpose)
	if err != nil || opened == "" {
		return nil, ErrPasskeyVerifyFailed
	}
	var env passkeyChallengeEnvelope
	if err := json.Unmarshal([]byte(opened), &env); err != nil {
		return nil, ErrPasskeyVerifyFailed
	}
	if env.Purpose != expectedPurpose || time.Now().UnixMilli() > env.ExpiresAt {
		return nil, ErrPasskeyVerifyFailed
	}
	if !consumePasskeyChallengeOnce(env.Purpose, env.ChallengeB64, env.ExpiresAt) {
		return nil, ErrPasskeyVerifyFailed
	}
	return &env, nil
}

func verifyClientDataJSON(raw []byte, expectedType string, expectedChallengeB64 string, expectedOrigin string) error {
	if len(raw) == 0 || len(raw) > 8192 {
		return ErrPasskeyVerifyFailed
	}
	var cd struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if err := json.Unmarshal(raw, &cd); err != nil {
		return ErrPasskeyVerifyFailed
	}
	if cd.Type != expectedType || cd.CrossOrigin {
		return ErrPasskeyVerifyFailed
	}
	chalNorm := strings.TrimRight(strings.TrimSpace(cd.Challenge), "=")
	if subtle.ConstantTimeCompare([]byte(chalNorm), []byte(expectedChallengeB64)) != 1 {
		return ErrPasskeyVerifyFailed
	}
	if !strings.EqualFold(strings.TrimSpace(cd.Origin), expectedOrigin) {
		return ErrPasskeyVerifyFailed
	}
	return nil
}

func parseCOSEPublicKeyToSPKI(m map[any]any) ([]byte, int, error) {
	kty, ok1 := cborInt(m[int64(1)])
	alg, ok2 := cborInt(m[int64(3)])
	if !ok1 || !ok2 {
		return nil, 0, ErrPasskeyVerifyFailed
	}

	switch {
	case kty == 2 && alg == COSEAlgES256:
		crv, okCrv := cborInt(m[int64(-1)])
		xBytes, okX := m[int64(-2)].([]byte)
		yBytes, okY := m[int64(-3)].([]byte)
		if !okCrv || crv != 1 || !okX || !okY || len(xBytes) != 32 || len(yBytes) != 32 {
			return nil, 0, ErrPasskeyVerifyFailed
		}
		// Validate point is strictly on NIST P-256 curve via stdlib crypto/ecdh!
		uncompressed := make([]byte, 65)
		uncompressed[0] = 0x04
		copy(uncompressed[1:33], xBytes)
		copy(uncompressed[33:65], yBytes)
		if _, err := ecdh.P256().NewPublicKey(uncompressed); err != nil {
			return nil, 0, ErrPasskeyVerifyFailed
		}
		pub := &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}
		spki, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, 0, ErrPasskeyVerifyFailed
		}
		return spki, COSEAlgES256, nil

	case kty == 3 && alg == COSEAlgRS256:
		nBytes, okN := m[int64(-1)].([]byte)
		eBytes, okE := m[int64(-2)].([]byte)
		if !okN || !okE || len(nBytes) < 256 || len(eBytes) == 0 || len(eBytes) > 4 {
			return nil, 0, ErrPasskeyVerifyFailed
		}
		eInt := 0
		for _, b := range eBytes {
			eInt = (eInt << 8) | int(b)
		}
		if eInt < 3 || eInt%2 == 0 {
			return nil, 0, ErrPasskeyVerifyFailed
		}
		pub := &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: eInt,
		}
		spki, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, 0, ErrPasskeyVerifyFailed
		}
		return spki, COSEAlgRS256, nil

	default:
		return nil, 0, fmt.Errorf("%w: unsupported COSE kty=%d alg=%d", ErrPasskeyVerifyFailed, kty, alg)
	}
}

func cborInt(v any) (int, bool) {
	if i, ok := v.(int64); ok {
		return int(i), true
	}
	return 0, false
}

// --- Safe, zero-dependency bounded definite-length RFC 8949 CBOR decoder ---

func decodeCBORItem(b []byte, depth int) (any, []byte, error) {
	if len(b) == 0 || depth > maxCBORDepth {
		return nil, nil, errors.New("cbor: truncated or max depth exceeded")
	}
	major := b[0] >> 5
	info := b[0] & 0x1f
	arg, rest, err := readCBORArg(b[1:], info)
	if err != nil {
		return nil, nil, err
	}

	switch major {
	case 0: // Unsigned int
		return int64(arg), rest, nil
	case 1: // Negative int (-1 - arg)
		return -1 - int64(arg), rest, nil
	case 2: // Byte string
		if arg > uint64(len(rest)) || arg > maxCBORBytes {
			return nil, nil, errors.New("cbor: byte string out of bounds")
		}
		cp := bytes.Clone(rest[:arg])
		return cp, rest[arg:], nil
	case 3: // UTF-8 string
		if arg > uint64(len(rest)) || arg > maxCBORBytes {
			return nil, nil, errors.New("cbor: text string out of bounds")
		}
		return string(rest[:arg]), rest[arg:], nil
	case 4: // Array
		if arg > 256 {
			return nil, nil, errors.New("cbor: array too large")
		}
		arr := make([]any, 0, arg)
		cur := rest
		for i := uint64(0); i < arg; i++ {
			elem, next, dErr := decodeCBORItem(cur, depth+1)
			if dErr != nil {
				return nil, nil, dErr
			}
			arr = append(arr, elem)
			cur = next
		}
		return arr, cur, nil
	case 5: // Map
		if arg > 128 {
			return nil, nil, errors.New("cbor: map too large")
		}
		m := make(map[any]any, arg)
		cur := rest
		for i := uint64(0); i < arg; i++ {
			k, nextK, kErr := decodeCBORItem(cur, depth+1)
			if kErr != nil {
				return nil, nil, kErr
			}
			// Only allow comparable scalar keys (string / int64) so map[k] never panics!
			switch k.(type) {
			case string, int64:
			default:
				return nil, nil, errors.New("cbor: non-scalar map key rejected")
			}
			v, nextV, vErr := decodeCBORItem(nextK, depth+1)
			if vErr != nil {
				return nil, nil, vErr
			}
			m[k] = v
			cur = nextV
		}
		return m, cur, nil
	case 7: // Booleans / null
		switch info {
		case 20:
			return false, rest, nil
		case 21:
			return true, rest, nil
		case 22, 23:
			return nil, rest, nil
		default:
			return nil, nil, errors.New("cbor: unsupported simple/float type")
		}
	default:
		return nil, nil, errors.New("cbor: unsupported major type")
	}
}

func readCBORArg(b []byte, info byte) (uint64, []byte, error) {
	switch {
	case info < 24:
		return uint64(info), b, nil
	case info == 24:
		if len(b) < 1 {
			return 0, nil, errors.New("cbor: short uint8")
		}
		return uint64(b[0]), b[1:], nil
	case info == 25:
		if len(b) < 2 {
			return 0, nil, errors.New("cbor: short uint16")
		}
		return uint64(binary.BigEndian.Uint16(b[:2])), b[2:], nil
	case info == 26:
		if len(b) < 4 {
			return 0, nil, errors.New("cbor: short uint32")
		}
		return uint64(binary.BigEndian.Uint32(b[:4])), b[4:], nil
	case info == 27:
		if len(b) < 8 {
			return 0, nil, errors.New("cbor: short uint64")
		}
		return binary.BigEndian.Uint64(b[:8]), b[8:], nil
	default:
		return 0, nil, errors.New("cbor: indefinite length not allowed")
	}
}
