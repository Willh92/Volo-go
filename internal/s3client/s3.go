package s3client

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/security"
)

var (
	validS3EndpointRe  = regexp.MustCompile(`^https?://(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?(/[A-Za-z0-9._/-]*)?$`)
	validS3BucketRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	validS3KeyPrefixRe = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
)

// Config holds S3 / Cloudflare R2 / MinIO connection settings for pure-Go AWS SigV4 requests.
type Config struct {
	Endpoint      string
	Region        string
	Bucket        string
	AccessKey     string
	SecretKey     string
	PublicBaseURL string
	KeyPrefix     string
	PathStyle     bool
}

// Client executes pure-Go AWS Signature Version 4 (`AWS4-HMAC-SHA256`) object operations
// with strict TLS verification (`InsecureSkipVerify: false` always).
type Client struct {
	cfg        Config
	httpClient *http.Client
	nowFn      func() time.Time
}

// NewClient constructs a pure-Go SigV4 S3 client. When safeDialer is non-nil, all outbound connections
// pass through DNS-pinning SSRF validation.
func NewClient(cfg Config, safeDialer *security.OutboundSafeDialer) (*Client, error) {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.AccessKey = strings.TrimSpace(cfg.AccessKey)
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	cfg.KeyPrefix = strings.TrimSpace(cfg.KeyPrefix)
	cfg.Region = strings.TrimSpace(cfg.Region)
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3 endpoint, bucket, access_key, and secret_key are required")
	}
	if !validS3BucketRe.MatchString(cfg.Bucket) {
		return nil, fmt.Errorf("invalid s3 bucket name %q", cfg.Bucket)
	}
	if !validS3KeyPrefixRe.MatchString(cfg.KeyPrefix) || strings.Contains(cfg.KeyPrefix, "..") {
		return nil, fmt.Errorf("invalid s3 key_prefix %q", cfg.KeyPrefix)
	}
	if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
		cfg.Endpoint = "https://" + cfg.Endpoint
	}
	if !validS3EndpointRe.MatchString(cfg.Endpoint) {
		return nil, fmt.Errorf("invalid s3 endpoint %q", cfg.Endpoint)
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid s3 endpoint %q", cfg.Endpoint)
	}
	cfg.Endpoint = (&url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   strings.TrimRight(u.Path, "/"),
	}).String()

	if base := strings.TrimSpace(cfg.PublicBaseURL); base != "" {
		bu, pErr := url.Parse(base)
		if pErr != nil || (bu.Scheme != "https" && bu.Scheme != "http") || bu.Host == "" || bu.User != nil {
			return nil, fmt.Errorf("invalid s3 public_base_url %q (must be http:// or https://)", base)
		}
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if safeDialer != nil {
		tr.DialContext = safeDialer.DialContext
	} else {
		baseDialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, sErr := net.SplitHostPort(addr)
			if sErr != nil {
				return nil, sErr
			}
			ips, rErr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if rErr != nil || len(ips) == 0 {
				return nil, fmt.Errorf("s3 resolve %q failed: %w", host, rErr)
			}
			for _, ip := range ips {
				ip = ip.Unmap()
				if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || netip.MustParsePrefix("169.254.0.0/16").Contains(ip) {
					return nil, fmt.Errorf("s3 SSRF guard: link-local/metadata IP %s blocked", ip)
				}
			}
			return baseDialer.DialContext(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
		}
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   90 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		nowFn: time.Now,
	}, nil
}

func (c *Client) fullKey(objectKey string) string {
	k := strings.TrimLeft(objectKey, "/")
	pfx := strings.Trim(c.cfg.KeyPrefix, "/")
	if pfx != "" {
		return pfx + "/" + k
	}
	return k
}

// PublicURL resolves the public URL for an uploaded object key.
func (c *Client) PublicURL(objectKey string) string {
	fk := c.fullKey(objectKey)
	if base := strings.TrimRight(strings.TrimSpace(c.cfg.PublicBaseURL), "/"); base != "" {
		if bu, err := url.Parse(base); err == nil && (strings.EqualFold(bu.Scheme, "https") || strings.EqualFold(bu.Scheme, "http")) && bu.Host != "" {
			return base + "/" + escapeS3KeyPath(fk)
		}
	}
	u, _ := url.Parse(c.cfg.Endpoint)
	if c.cfg.PathStyle {
		return strings.TrimRight(u.String(), "/") + "/" + url.PathEscape(c.cfg.Bucket) + "/" + escapeS3KeyPath(fk)
	}
	u.Host = c.cfg.Bucket + "." + u.Host
	return strings.TrimRight(u.String(), "/") + "/" + escapeS3KeyPath(fk)
}

// PutBytes uploads a payload with precomputed SHA-256 integrity header (`X-Amz-Content-Sha256`).
func (c *Client) PutBytes(ctx context.Context, objectKey string, data []byte, contentType string) (string, error) {
	fk := c.fullKey(objectKey)
	sum := sha256.Sum256(data)
	payloadHash := hex.EncodeToString(sum[:])

	targetURL, hostHdr, canonicalURI, err := c.buildObjectTarget(fk)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, targetURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(data))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.signRequest(req, hostHdr, canonicalURI, payloadHash)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("s3 PUT status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return c.PublicURL(objectKey), nil
}

func adaptivePutStreamTimeout(size int64) time.Duration {
	const (
		minTimeout = 90 * time.Second
		maxTimeout = 30 * time.Minute
	)
	if size <= 0 {
		return minTimeout
	}
	calc := 60*time.Second + time.Duration(size/(512*1024))*time.Second
	if calc < minTimeout {
		return minTimeout
	}
	if calc > maxTimeout {
		return maxTimeout
	}
	return calc
}

// PutStream uploads a seekable stream without buffering the entire archive in memory.
func (c *Client) PutStream(ctx context.Context, objectKey string, rs io.ReadSeeker, size int64, contentType string) (string, error) {
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, rs); err != nil {
		return "", err
	}
	payloadHash := hex.EncodeToString(h.Sum(nil))
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	fk := c.fullKey(objectKey)
	targetURL, hostHdr, canonicalURI, err := c.buildObjectTarget(fk)
	if err != nil {
		return "", err
	}
	uploadCtx, cancel := context.WithTimeout(ctx, adaptivePutStreamTimeout(size))
	defer cancel()

	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPut, targetURL, rs)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.signRequest(req, hostHdr, canonicalURI, payloadHash)

	streamClient := *c.httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("s3 streaming PUT status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return c.PublicURL(objectKey), nil
}

// DeleteObject removes an object from the bucket via signed `DELETE`.
func (c *Client) DeleteObject(ctx context.Context, objectKey string) error {
	fk := c.fullKey(objectKey)
	targetURL, hostHdr, canonicalURI, err := c.buildObjectTarget(fk)
	if err != nil {
		return err
	}
	emptyHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, targetURL, nil)
	if err != nil {
		return err
	}
	c.signRequest(req, hostHdr, canonicalURI, emptyHash)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("s3 DELETE status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// ObjectInfo summarizes a single S3 object returned by ListObjects.
type ObjectInfo struct {
	Key          string
	LastModified time.Time
	Size         int64
}

// ListObjects lists objects matching prefix using S3 ListObjectsV2 (`GET ?list-type=2`),
// returning keys relative to Config.KeyPrefix so they pass directly to DeleteObject.
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	targetURL, hostHdr, canonicalURI, err := c.buildObjectTarget("")
	if err != nil {
		return nil, err
	}
	searchPrefix := c.fullKey(prefix)
	pfxTrim := strings.Trim(c.cfg.KeyPrefix, "/")

	type xmlContent struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		Size         int64  `xml:"Size"`
	}
	type listBucketResult struct {
		IsTruncated           bool         `xml:"IsTruncated"`
		NextContinuationToken string       `xml:"NextContinuationToken"`
		Contents              []xmlContent `xml:"Contents"`
	}

	var out []ObjectInfo
	contToken := ""
	for page := 0; page < 20; page++ {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("max-keys", "1000")
		if searchPrefix != "" {
			q.Set("prefix", searchPrefix)
		}
		if contToken != "" {
			q.Set("continuation-token", contToken)
		}
		reqURL := targetURL + "?" + canonicalEncodeValues(q)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		c.signRequest(req, hostHdr, canonicalURI, emptyPayloadSHA256)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("s3 LIST status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var parsed listBucketResult
		if err := xml.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("s3 LIST parse XML: %w", err)
		}
		for _, item := range parsed.Contents {
			relKey := strings.TrimLeft(item.Key, "/")
			if pfxTrim != "" {
				if strings.HasPrefix(relKey, pfxTrim+"/") {
					relKey = strings.TrimPrefix(relKey, pfxTrim+"/")
				} else if relKey == pfxTrim {
					continue
				}
			}
			if relKey == "" {
				continue
			}
			var modTime time.Time
			if ts := strings.TrimSpace(item.LastModified); ts != "" {
				if t, pErr := time.Parse(time.RFC3339Nano, ts); pErr == nil {
					modTime = t.UTC()
				} else if t, pErr := time.Parse(time.RFC3339, ts); pErr == nil {
					modTime = t.UTC()
				}
			}
			out = append(out, ObjectInfo{
				Key:          relKey,
				LastModified: modTime,
				Size:         item.Size,
			})
		}
		if !parsed.IsTruncated || strings.TrimSpace(parsed.NextContinuationToken) == "" {
			break
		}
		contToken = strings.TrimSpace(parsed.NextContinuationToken)
	}
	return out, nil
}

// HealthCheck verifies bucket write/delete connectivity.
func (c *Client) HealthCheck(ctx context.Context) error {
	probeKey := ".volo-healthcheck-probe"
	if _, err := c.PutBytes(ctx, probeKey, []byte("ok"), "text/plain"); err != nil {
		return err
	}
	return c.DeleteObject(ctx, probeKey)
}

func (c *Client) buildObjectTarget(fullKey string) (targetURL string, hostHdr string, canonicalURI string, err error) {
	u, err := url.Parse(c.cfg.Endpoint)
	if err != nil {
		return "", "", "", err
	}
	escapedKey := escapeS3KeyPath(fullKey)
	basePath := strings.TrimRight(u.EscapedPath(), "/")
	if c.cfg.PathStyle {
		hostHdr = u.Host
		if escapedKey != "" {
			canonicalURI = basePath + "/" + url.PathEscape(c.cfg.Bucket) + "/" + escapedKey
		} else {
			canonicalURI = basePath + "/" + url.PathEscape(c.cfg.Bucket)
		}
	} else {
		hostHdr = c.cfg.Bucket + "." + u.Host
		u.Host = hostHdr
		if escapedKey != "" {
			canonicalURI = basePath + "/" + escapedKey
		} else if basePath != "" {
			canonicalURI = basePath
		} else {
			canonicalURI = "/"
		}
	}
	targetURL = u.Scheme + "://" + hostHdr + canonicalURI
	if !validS3TargetURLRe.MatchString(targetURL) {
		return "", "", "", fmt.Errorf("invalid s3 target url")
	}
	return targetURL, hostHdr, canonicalURI, nil
}

var validS3TargetURLRe = regexp.MustCompile(`^https?://(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?(/[^\s?#]*)?$`)

func (c *Client) signRequest(req *http.Request, hostHdr string, canonicalURI string, payloadHash string) {
	now := c.nowFn().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Host = hostHdr
	req.Header.Set("Host", hostHdr)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headersToSign := map[string]string{
		"host":                 hostHdr,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if ct := strings.TrimSpace(req.Header.Get("Content-Type")); ct != "" {
		headersToSign["content-type"] = ct
	}

	keys := make([]string, 0, len(headersToSign))
	for k := range headersToSign {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonHeaders strings.Builder
	for _, k := range keys {
		canonHeaders.WriteString(k)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(headersToSign[k]))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(keys, ";")

	canonQuery := ""
	if req.URL != nil && req.URL.RawQuery != "" {
		canonQuery = canonicalQueryString(req.URL.RawQuery)
	}

	canonicalReq := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	canonHash := sha256.Sum256([]byte(canonicalReq))
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, c.cfg.Region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(canonHash[:]),
	}, "\n")

	signingKey := deriveSigV4Key(c.cfg.SecretKey, dateStamp, c.cfg.Region, "s3")
	sig := hmacSHA256Hex(signingKey, stringToSign)

	authHeader := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey, scope, signedHeaders, sig,
	)
	req.Header.Set("Authorization", authHeader)
}

func awsURIQueryEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	const upperhex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0F])
		}
	}
	return b.String()
}

func canonicalEncodeValues(vals url.Values) string {
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(vals))
	for k, vs := range vals {
		ek := awsURIQueryEscape(k)
		if len(vs) == 0 {
			pairs = append(pairs, kv{k: ek, v: ""})
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, kv{k: ek, v: awsURIQueryEscape(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

func canonicalQueryString(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	if vals, err := url.ParseQuery(rawQuery); err == nil {
		return canonicalEncodeValues(vals)
	}
	vals := url.Values{}
	for _, seg := range strings.Split(rawQuery, "&") {
		if seg == "" {
			continue
		}
		k, v, hasEq := strings.Cut(seg, "=")
		uk, errK := url.QueryUnescape(k)
		if errK != nil {
			uk = k
		}
		uv := ""
		if hasEq {
			if decV, errV := url.QueryUnescape(v); errV == nil {
				uv = decV
			} else {
				uv = v
			}
		}
		vals.Add(uk, uv)
	}
	return canonicalEncodeValues(vals)
}

func deriveSigV4Key(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hmacSHA256Hex(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, data))
}

func escapeS3KeyPath(key string) string {
	trimmed := strings.TrimLeft(key, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(p), "+", "%20")
	}
	return strings.Join(parts, "/")
}
