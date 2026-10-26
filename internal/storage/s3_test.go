package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3ContractOverHTTP(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	server := httptest.NewServer(newS3Fixture(t))
	defer server.Close()
	store, err := NewS3(context.Background(), S3Config{Bucket: "test-bucket", Prefix: "isolated/", Region: "us-east-1", Endpoint: server.URL, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	checkContract(t, store)
	// Unseekable streams are supported without buffering the full upload.
	reader, writer := io.Pipe()
	go func() { _, _ = writer.Write([]byte("stream")); _ = writer.Close() }()
	if _, err := store.Put(context.Background(), "stream", reader, 6, "text/plain"); err != nil {
		t.Fatal(err)
	}
	reader.Close()
	defer store.Delete(context.Background(), "stream")
}

func TestS3ConfigurationAndRanges(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []S3Config{{Region: "x"}, {Bucket: "x", Region: "x", Prefix: "../outside"}, {Bucket: "x", Region: "x", Endpoint: "file:///tmp/s3"}, {Bucket: "x", Region: "x", Endpoint: "https://user:secret@example.com"}} {
		if _, err := NewS3(ctx, cfg); err == nil {
			t.Fatalf("accepted invalid config %+v", cfg)
		}
	}
	store := &S3{}
	if _, _, err := store.GetRange(ctx, "key", -1, 1); err == nil {
		t.Fatal("negative range accepted")
	}
	if _, _, err := store.GetRange(ctx, "key", 1, 0); err == nil {
		t.Fatal("empty range accepted")
	}
}

// This integration test exercises the same contract against a real S3 API.
// Set PAGE_TEST_S3_ENDPOINT (for example http://localhost:9000), standard AWS
// credentials, and optionally PAGE_TEST_S3_BUCKET / AWS_REGION. Only objects
// in a freshly generated test prefix are touched; the bucket is never deleted.
func TestS3Integration(t *testing.T) {
	endpoint := os.Getenv("PAGE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set PAGE_TEST_S3_ENDPOINT for real S3 API contract tests")
	}
	bucket := os.Getenv("PAGE_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "page-storage-test"
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	store, err := NewS3(context.Background(), S3Config{Bucket: bucket, Prefix: fmt.Sprintf("contract-%d", time.Now().UnixNano()), Region: region, Endpoint: endpoint, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.client.HeadBucket(context.Background(), &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		_, err = store.client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		if err != nil {
			t.Fatalf("prepare test bucket: %v", err)
		}
	}
	checkContract(t, store)
}

type fixtureObject struct {
	body              []byte
	contentType, etag string
	modified          time.Time
}
type s3Fixture struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string]fixtureObject
}

func newS3Fixture(t *testing.T) *s3Fixture {
	return &s3Fixture{t: t, objects: map[string]fixtureObject{}}
}

func (s *s3Fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		s.t.Error("request was not SigV4 signed")
		w.WriteHeader(403)
		return
	}
	if r.URL.Path == "/test-bucket" && r.URL.Query().Get("list-type") == "2" {
		s.list(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/test-bucket/isolated/") {
		s.t.Errorf("object escaped configured prefix: %s", r.URL.Path)
		w.WriteHeader(400)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
	if r.Method == http.MethodPut {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if int64(len(body)) != r.ContentLength {
			s.t.Logf("content length mismatch: body=%d content-length=%d transfer=%v", len(body), r.ContentLength, r.TransferEncoding)
			http.Error(w, "short body", 400)
			return
		}
		obj := fixtureObject{body: body, contentType: r.Header.Get("Content-Type"), etag: fmt.Sprintf("\"%x\"", sha256.Sum256(body)), modified: time.Now().UTC().Truncate(time.Second)}
		s.mu.Lock()
		s.objects[key] = obj
		s.mu.Unlock()
		w.Header().Set("ETag", obj.etag)
		return
	}
	s.mu.Lock()
	obj, ok := s.objects[key]
	if r.Method == http.MethodDelete {
		delete(s.objects, key)
	}
	s.mu.Unlock()
	if r.Method == http.MethodDelete {
		w.WriteHeader(204)
		return
	}
	if !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(404)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>")
		}
		return
	}
	body := obj.body
	status := http.StatusOK
	if rr := r.Header.Get("Range"); rr != "" {
		var start, end int64
		if _, err := fmt.Sscanf(rr, "bytes=%d-%d", &start, &end); err != nil || start < 0 || start >= int64(len(body)) || end < start {
			w.WriteHeader(416)
			return
		}
		end = min(end, int64(len(body)-1))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		body = body[start : end+1]
		status = 206
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Content-Type", obj.contentType)
	w.Header().Set("ETag", obj.etag)
	w.Header().Set("Last-Modified", obj.modified.Format(http.TimeFormat))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (s *s3Fixture) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if !strings.HasPrefix(prefix, "isolated/") {
		s.t.Error("list escaped namespace")
		w.WriteHeader(400)
		return
	}
	after := ""
	if cursor := r.URL.Query().Get("continuation-token"); cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		after = string(b)
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("max-keys"))
	if limit <= 0 {
		limit = 1000
	}
	type item struct {
		Key          string
		Size         int64
		ETag         string
		LastModified time.Time
	}
	out := struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
		Contents              []item
	}{}
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) && key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys[:min(len(keys), limit)] {
		obj := s.objects[key]
		out.Contents = append(out.Contents, item{Key: key, Size: int64(len(obj.body)), ETag: obj.etag, LastModified: obj.modified})
	}
	if len(keys) > limit {
		out.IsTruncated = true
		out.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(keys[limit-1]))
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}
