package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Config configures a lightweight SigV4 S3-compatible backend (works with Cloudflare R2, Aliyun OSS S3 endpoint, MinIO, AWS S3).
type S3Config struct {
	Name           string
	Endpoint       string
	Region         string
	Bucket         string
	Prefix         string
	AccessKeyID    string
	SecretKey      string
	PublicBaseURL  string
	ForcePathStyle bool
	HTTPClient     *http.Client
}

// S3Backend implements Backend using pure-Go AWS SigV4 (zero SDK bloat, WASM-compatible).
type S3Backend struct {
	cfg    S3Config
	client *http.Client
}

func NewS3Backend(cfg S3Config) (*S3Backend, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 backend requires endpoint and bucket")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	if cfg.Name == "" {
		cfg.Name = "s3:" + cfg.Bucket
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	cfg.Prefix = CleanKey(cfg.Prefix)
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &S3Backend{cfg: cfg, client: client}, nil
}

func (s *S3Backend) Name() string { return s.cfg.Name }

func (s *S3Backend) fullKey(key string) string {
	k := CleanKey(key)
	if s.cfg.Prefix == "" {
		return k
	}
	if k == "" {
		return s.cfg.Prefix
	}
	return s.cfg.Prefix + "/" + k
}

func (s *S3Backend) objectURL(fullKey string) (string, error) {
	u, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return "", err
	}
	escaped := encodeS3Path(fullKey)
	if s.cfg.ForcePathStyle || strings.Contains(u.Host, "r2.cloudflarestorage.com") || strings.Contains(u.Host, "127.0.0.1") || strings.Contains(u.Host, "localhost") {
		u.Path = "/" + s.cfg.Bucket
		if escaped != "" {
			u.Path += "/" + escaped
		}
		return u.String(), nil
	}
	u.Host = s.cfg.Bucket + "." + u.Host
	u.Path = "/" + escaped
	return u.String(), nil
}

func (s *S3Backend) Get(ctx context.Context, key string) (*Object, error) {
	fk := s.fullKey(key)
	u, err := s.objectURL(fk)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	s.signRequest(req, emptySHA256)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("s3 GET %s: HTTP %d: %s", fk, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return &Object{
		Meta: metaFromHeaders(CleanKey(key), resp.Header),
		Body: resp.Body,
	}, nil
}

func (s *S3Backend) Head(ctx context.Context, key string) (ObjectMeta, error) {
	fk := s.fullKey(key)
	u, err := s.objectURL(fk)
	if err != nil {
		return ObjectMeta{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return ObjectMeta{}, err
	}
	s.signRequest(req, emptySHA256)
	resp, err := s.client.Do(req)
	if err != nil {
		return ObjectMeta{}, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ObjectMeta{}, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return ObjectMeta{}, fmt.Errorf("s3 HEAD %s: HTTP %d", fk, resp.StatusCode)
	}
	return metaFromHeaders(CleanKey(key), resp.Header), nil
}

func (s *S3Backend) Put(ctx context.Context, key string, body io.Reader, _ int64, opts PutOptions) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	payloadHash := hex.EncodeToString(sum[:])

	fk := s.fullKey(key)
	u, err := s.objectURL(fk)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	if opts.ContentType != "" {
		req.Header.Set("Content-Type", opts.ContentType)
	}
	if opts.CacheControl != "" {
		req.Header.Set("Cache-Control", opts.CacheControl)
	}
	s.signRequest(req, payloadHash)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("s3 PUT %s: HTTP %d: %s", fk, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func (s *S3Backend) Delete(ctx context.Context, key string) error {
	fk := s.fullKey(key)
	u, err := s.objectURL(fk)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	s.signRequest(req, emptySHA256)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("s3 DELETE %s: HTTP %d", fk, resp.StatusCode)
}

type listBucketResult struct {
	Contents []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *S3Backend) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	base, err := s.objectURL("")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("list-type", "2")
	fp := s.fullKey(prefix)
	if fp != "" {
		q.Set("prefix", fp)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	s.signRequest(req, emptySHA256)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("s3 LIST %s: HTTP %d: %s", fp, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var res listBucketResult
	if err := xml.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	out := make([]ObjectMeta, 0, len(res.Contents))
	for _, c := range res.Contents {
		k := c.Key
		if s.cfg.Prefix != "" {
			k = strings.TrimPrefix(k, s.cfg.Prefix+"/")
		}
		lm, _ := time.Parse(time.RFC3339, c.LastModified)
		out = append(out, ObjectMeta{
			Key:          CleanKey(k),
			Size:         c.Size,
			ETag:         c.ETag,
			LastModified: lm,
		})
	}
	return out, nil
}

func (s *S3Backend) PublicURL(key string) string {
	if s.cfg.PublicBaseURL == "" {
		return ""
	}
	return s.cfg.PublicBaseURL + "/" + s.fullKey(key)
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *S3Backend) signRequest(req *http.Request, payloadHash string) {
	if s.cfg.AccessKeyID == "" || s.cfg.SecretKey == "" {
		return
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("Host", req.URL.Host)

	signedHeadersMap := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		signedHeadersMap["content-type"] = ct
	}
	if cc := req.Header.Get("Cache-Control"); cc != "" {
		signedHeadersMap["cache-control"] = cc
	}

	keys := make([]string, 0, len(signedHeadersMap))
	for k := range signedHeadersMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonHeaders strings.Builder
	for _, k := range keys {
		canonHeaders.WriteString(k)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(signedHeadersMap[k]))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(keys, ";")

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := canonicalQueryString(req.URL.Query())
	canonicalReq := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	crHash := sha256Hex([]byte(canonicalReq))
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.cfg.Region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, scope, crHash)

	kDate := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.cfg.Region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKeyID, scope, signedHeaders, sig)
	req.Header.Set("Authorization", auth)
}

func canonicalQueryString(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEncode(k)+"="+awsEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func encodeS3Path(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = awsEncode(seg)
	}
	return strings.Join(segs, "/")
}

func awsEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func metaFromHeaders(key string, h http.Header) ObjectMeta {
	size, _ := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	lm, _ := http.ParseTime(h.Get("Last-Modified"))
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ObjectMeta{
		Key:          key,
		Size:         size,
		ContentType:  ct,
		CacheControl: h.Get("Cache-Control"),
		ETag:         h.Get("ETag"),
		LastModified: lm,
	}
}
