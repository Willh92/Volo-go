package extpkg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/security"
)

const (
	DefaultStallTimeout    = 20 * time.Second
	DefaultHeaderTimeout   = 15 * time.Second
	DefaultMaxDuration     = 5 * time.Minute
	DefaultRawMaxDuration  = 1 * time.Minute
	DefaultRawMaxBytes     = 4 << 20 // 4 MiB ceiling for raw non-zip files (e.g. .json locale packs)
	DefaultMaxRedirects    = 5
	streamCopyBufBytes     = 32 << 10 // 32 KiB pooled buffer (zero heap bloat on 256 MiB archives)
	defaultDownloadUserAgt = "Volo-Go-Extension-Installer/1.0"
)

var (
	// ErrDownloadCanceled indicates the client/admin aborted the operation before commit.
	ErrDownloadCanceled = errors.New("extpkg: download canceled")
	// ErrDownloadStalled indicates the remote server stopped sending bytes past StallTimeout.
	ErrDownloadStalled = errors.New("extpkg: download stalled (idle timeout exceeded)")
	// ErrDownloadTimeout indicates the download exceeded MaxDuration (defeating Slowloris drip attacks).
	ErrDownloadTimeout = errors.New("extpkg: download exceeded maximum duration")
	// ErrNotZipArchive indicates the remote endpoint returned HTML/JSON/text instead of a ZIP binary.
	ErrNotZipArchive = errors.New("extpkg: remote resource is not a valid .zip archive")
	// ErrEmptyDownload indicates a non-zip file download returned a 0-byte body.
	ErrEmptyDownload = errors.New("extpkg: remote resource returned empty response body")
	// ErrChecksumMismatch indicates the downloaded archive failed SHA-256 verification.
	ErrChecksumMismatch = errors.New("extpkg: archive sha256 checksum mismatch")
	// ErrArchiveTooLarge indicates the remote archive exceeded the configured byte cap.
	ErrArchiveTooLarge = errors.New("extpkg: archive exceeds maximum allowed size")

	hexSHA256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

	copyBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, streamCopyBufBytes)
			return &b
		},
	}
)

// DownloadOptions configures SSRF-safe streaming file/ZIP retrieval to disk.
type DownloadOptions struct {
	URL            string
	ExpectedSHA256 string
	TempDir        string
	TempPrefix     string
	MaxBytes       int64
	StallTimeout   time.Duration
	HeaderTimeout  time.Duration
	MaxDuration    time.Duration
}

// DownloadedZip holds an open, verified temporary file on disk.
// Call Cleanup() when finished to close and unlink the temporary file.
type DownloadedZip struct {
	File   *os.File
	Path   string
	Size   int64
	SHA256 string
}

// DownloadResult aliases DownloadedZip for raw/non-zip streaming callers.
type DownloadResult = DownloadedZip

// Cleanup closes and deletes the temporary archive file idempotently.
func (d *DownloadedZip) Cleanup() {
	if d == nil {
		return
	}
	if d.File != nil {
		_ = d.File.Close()
		d.File = nil
	}
	if d.Path != "" {
		_ = os.Remove(d.Path)
		d.Path = ""
	}
}

// NormalizeSHA256 validates and normalizes an optional SHA-256 digest (accepting optional "sha256:" prefix).
func NormalizeSHA256(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if strings.HasPrefix(strings.ToLower(s), "sha256:") {
		s = strings.TrimSpace(s[7:])
	}
	if !hexSHA256Re.MatchString(s) {
		return "", fmt.Errorf("invalid sha256 digest (expected 64 hex characters)")
	}
	return strings.ToLower(s), nil
}

// PublicCloneDialer returns a copy of baseDialer with unrestricted public hostnames
// while preserving DNS-pinned SSRF IP protection, custom resolvers, and test loopback flags.
func PublicCloneDialer(base *security.OutboundSafeDialer) *security.OutboundSafeDialer {
	if base == nil {
		return &security.OutboundSafeDialer{Timeout: 10 * time.Second}
	}
	timeout := base.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &security.OutboundSafeDialer{
		AllowedHosts:          nil,
		Timeout:               timeout,
		Resolver:              base.Resolver,
		AllowLoopbackForTests: base.AllowLoopbackForTests,
	}
}

type dlApprovedProxyKey struct{}

func canonicalDownloadProxyAddr(u *url.URL) string {
	if u == nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	if port := u.Port(); port != "" {
		return net.JoinHostPort(host, port)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return net.JoinHostPort(host, "443")
	case "socks5", "socks5h":
		return net.JoinHostPort(host, "1080")
	default:
		return net.JoinHostPort(host, "80")
	}
}

type downloadProxyValidatingRT struct {
	dialer *security.OutboundSafeDialer
	next   *http.Transport
}

func (rt *downloadProxyValidatingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := ValidatePublicHTTPURL(req.URL.String(), rt.dialer.AllowLoopbackForTests)
	if err != nil {
		return nil, err
	}
	if rt.next.Proxy != nil {
		if proxyURL, pErr := rt.next.Proxy(req); pErr == nil && proxyURL != nil {
			host := u.Hostname()
			if _, lErr := netip.ParseAddr(host); lErr != nil {
				resolve := rt.dialer.Resolver
				if resolve == nil {
					resolve = func(ctx context.Context, h string) ([]netip.Addr, error) {
						return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
					}
				}
				ips, rErr := resolve(req.Context(), host)
				if rErr != nil {
					return nil, fmt.Errorf("extpkg: dns lookup %q: %w", host, rErr)
				}
				if len(ips) == 0 {
					return nil, fmt.Errorf("%w: %q resolved to zero addresses", security.ErrSSRFBlocked, host)
				}
				for _, ip := range ips {
					if !(rt.dialer.AllowLoopbackForTests && ip.Zone() == "" && ip.Unmap().IsLoopback()) && !security.IsPublicRoutableIP(ip) {
						return nil, fmt.Errorf("%w: resolved address %s for %q is non-public", security.ErrSSRFBlocked, ip.String(), host)
					}
				}
			}
			if proxyAddr := canonicalDownloadProxyAddr(proxyURL); proxyAddr != "" {
				req = req.WithContext(context.WithValue(req.Context(), dlApprovedProxyKey{}, proxyAddr))
			}
		}
	}
	return rt.next.RoundTrip(req)
}

// StreamVerifyZip streams a remote .zip archive directly into a disk temp file using a fixed 32 KiB buffer,
// enforcing SSRF checks across redirects, HTTPS->HTTP downgrade protection, total duration + idle-read stall timeouts,
// truncation checks, ZIP magic bytes, and single-pass SHA-256 verification without buffering the archive in RAM.
func StreamVerifyZip(ctx context.Context, baseDialer *security.OutboundSafeDialer, opts DownloadOptions) (*DownloadedZip, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 32 << 20
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultMaxDuration
	}
	if opts.TempPrefix == "" {
		opts.TempPrefix = ".volo-ext-dl-"
	}
	return streamDownloadToFile(ctx, baseDialer, opts, true, ".zip", "application/zip, application/octet-stream, */*;q=0.8")
}

// DownloadRawFile streams and SHA-256-verifies a non-zip remote file (such as a .json language pack <= 4 MiB)
// into a temporary file on disk without requiring PK\x03\x04 ZIP magic bytes.
func DownloadRawFile(ctx context.Context, dialer *security.OutboundSafeDialer, opts DownloadOptions) (*DownloadResult, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultRawMaxBytes
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultRawMaxDuration
	}
	if opts.TempPrefix == "" {
		opts.TempPrefix = ".volo-raw-dl-"
	}
	return streamDownloadToFile(ctx, dialer, opts, false, ".tmp", "application/json, application/octet-stream, */*;q=0.8")
}

func streamDownloadToFile(
	ctx context.Context,
	baseDialer *security.OutboundSafeDialer,
	opts DownloadOptions,
	requireZip bool,
	tempExt string,
	acceptHeader string,
) (*DownloadedZip, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDownloadCanceled, err)
	}
	dialer := PublicCloneDialer(baseDialer)
	parsedURL, err := ValidatePublicHTTPURL(opts.URL, dialer.AllowLoopbackForTests)
	if err != nil {
		return nil, err
	}
	expectedSum, err := NormalizeSHA256(opts.ExpectedSHA256)
	if err != nil {
		return nil, err
	}
	maxBytes := opts.MaxBytes
	stallTimeout := opts.StallTimeout
	if stallTimeout <= 0 {
		stallTimeout = DefaultStallTimeout
	}
	headerTimeout := opts.HeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = DefaultHeaderTimeout
	}
	maxDur := opts.MaxDuration
	if maxDur <= 0 {
		maxDur = DefaultMaxDuration
	}

	if opts.TempDir != "" {
		if mkErr := os.MkdirAll(opts.TempDir, 0o750); mkErr != nil {
			return nil, fmt.Errorf("extpkg: prepare temp dir: %w", mkErr)
		}
	}

	durCtx, cancelDur := context.WithTimeout(ctx, maxDur)
	defer cancelDur()

	dlCtx, cancelDL := context.WithCancelCause(durCtx)
	defer cancelDL(nil)

	proxySocketDialer := &net.Dialer{
		Timeout:   dialer.Timeout,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(dCtx context.Context, network, addr string) (net.Conn, error) {
			if approvedProxy, ok := dCtx.Value(dlApprovedProxyKey{}).(string); ok && approvedProxy != "" && addr == approvedProxy {
				return proxySocketDialer.DialContext(dCtx, network, addr)
			}
			return dialer.DialContext(dCtx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: false,
		},
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: &downloadProxyValidatingRT{dialer: dialer, next: transport},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= DefaultMaxRedirects {
				return fmt.Errorf("extpkg: exceeded maximum redirects (%d)", DefaultMaxRedirects)
			}
			if len(via) > 0 && strings.EqualFold(via[len(via)-1].URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
				return fmt.Errorf("%w: redirect scheme downgrade from https to %q is prohibited", security.ErrSSRFBlocked, req.URL.Scheme)
			}
			if _, vErr := ValidatePublicHTTPURL(req.URL.String(), dialer.AllowLoopbackForTests); vErr != nil {
				return vErr
			}
			// Strip sensitive headers across cross-host redirects (e.g. github.com -> objects.githubusercontent.com).
			if len(via) > 0 && (!strings.EqualFold(via[0].URL.Host, req.URL.Host) || !strings.EqualFold(via[len(via)-1].URL.Host, req.URL.Host)) {
				req.Header.Del("Authorization")
				req.Header.Del("Proxy-Authorization")
				req.Header.Del("Cookie")
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("extpkg: build download request: %w", err)
	}
	req.Header.Set("User-Agent", defaultDownloadUserAgt)
	req.Header.Set("Accept", acceptHeader)

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", ErrDownloadCanceled, ctx.Err())
		}
		if errors.Is(context.Cause(dlCtx), ErrDownloadStalled) {
			return nil, ErrDownloadStalled
		}
		if durCtx.Err() != nil {
			return nil, fmt.Errorf("%w (%s)", ErrDownloadTimeout, maxDur)
		}
		return nil, fmt.Errorf("extpkg: download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("extpkg: remote server returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("%w: Content-Length %d exceeds %d bytes limit", ErrArchiveTooLarge, resp.ContentLength, maxBytes)
	}

	tmpFile, err := os.CreateTemp(opts.TempDir, opts.TempPrefix+"*"+tempExt)
	if err != nil {
		return nil, fmt.Errorf("extpkg: create temp archive: %w", err)
	}
	tmpPath := tmpFile.Name()
	succeeded := false
	defer func() {
		if !succeeded {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// Stall watchdog cancels dlCtx (closing the underlying TCP socket immediately) if zero bytes arrive within stallTimeout.
	watchdog := time.AfterFunc(stallTimeout, func() {
		cancelDL(ErrDownloadStalled)
	})
	defer watchdog.Stop()

	bufPtr := copyBufPool.Get().(*[]byte)
	buf := *bufPtr
	defer copyBufPool.Put(bufPtr)

	hasher := sha256.New()
	var written int64
	var head [4]byte
	headFilled := 0

	limited := io.LimitReader(resp.Body, maxBytes+1)
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", ErrDownloadCanceled, ctx.Err())
		}
		if durCtx.Err() != nil {
			return nil, fmt.Errorf("%w (%s)", ErrDownloadTimeout, maxDur)
		}
		n, rErr := limited.Read(buf)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			chunk := buf[:n]
			if headFilled < len(head) {
				copied := copy(head[headFilled:], chunk)
				headFilled += copied
			}
			written += int64(n)
			if written > maxBytes {
				return nil, fmt.Errorf("%w: payload exceeds %d bytes limit", ErrArchiveTooLarge, maxBytes)
			}
			if _, wErr := tmpFile.Write(chunk); wErr != nil {
				return nil, fmt.Errorf("extpkg: write temp archive: %w", wErr)
			}
			_, _ = hasher.Write(chunk)
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				break
			}
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%w: %w", ErrDownloadCanceled, ctx.Err())
			}
			if errors.Is(context.Cause(dlCtx), ErrDownloadStalled) {
				return nil, ErrDownloadStalled
			}
			if durCtx.Err() != nil {
				return nil, fmt.Errorf("%w (%s)", ErrDownloadTimeout, maxDur)
			}
			return nil, fmt.Errorf("extpkg: network stream interrupted: %w", rErr)
		}
	}

	if written == 0 {
		if requireZip {
			return nil, fmt.Errorf("%w: empty response body", ErrNotZipArchive)
		}
		return nil, ErrEmptyDownload
	}
	if resp.ContentLength >= 0 && written != resp.ContentLength {
		return nil, fmt.Errorf("extpkg: truncated transfer (received %d of %d bytes)", written, resp.ContentLength)
	}
	if requireZip && !isZipMagic(head[:headFilled]) {
		ct := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
		if strings.Contains(ct, "text/html") || bytes.HasPrefix(bytes.TrimSpace(head[:headFilled]), []byte("<")) {
			return nil, fmt.Errorf("%w: URL returned an HTML webpage instead of a direct .zip download asset", ErrNotZipArchive)
		}
		return nil, fmt.Errorf("%w: missing PK zip header", ErrNotZipArchive)
	}

	actualSum := hex.EncodeToString(hasher.Sum(nil))
	if expectedSum != "" && subtle.ConstantTimeCompare([]byte(actualSum), []byte(expectedSum)) != 1 {
		return nil, fmt.Errorf("%w: expected %s, got %s", ErrChecksumMismatch, expectedSum, actualSum)
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("extpkg: rewind temp archive: %w", err)
	}
	succeeded = true
	return &DownloadedZip{
		File:   tmpFile,
		Path:   tmpPath,
		Size:   written,
		SHA256: actualSum,
	}, nil
}

func isZipMagic(head []byte) bool {
	if len(head) < 4 {
		return false
	}
	// Local file header PK\x03\x04 or empty archive EOCD PK\x05\x06
	return head[0] == 'P' && head[1] == 'K' &&
		((head[2] == 0x03 && head[3] == 0x04) || (head[2] == 0x05 && head[3] == 0x06))
}
