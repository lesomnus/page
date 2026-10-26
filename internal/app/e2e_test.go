package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/githubsync"
	"github.com/lesomnus/page/internal/serving"
	"github.com/lesomnus/page/internal/storage"
	"gopkg.in/yaml.v3"
)

// This test crosses the public command, GitHub protocol, live S3 API, durable
// metadata, HTTP data plane and lifecycle policy. It never needs a live forge.
func TestMVPWithS3(t *testing.T) {
	endpoint := os.Getenv("PAGE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set PAGE_TEST_S3_ENDPOINT to exercise a live S3 API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sha1, sha2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	var head atomic.Value
	head.Store(sha1)
	var fail atomic.Bool
	archive := func(body string) []byte {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tr := tar.NewWriter(gz)
		data := []byte(body)
		if e := tr.WriteHeader(&tar.Header{Name: "repo/public/index.html", Size: int64(len(data)), Mode: 0644}); e != nil {
			t.Fatal(e)
		}
		tr.Write(data)
		tr.Close()
		gz.Close()
		return b.Bytes()
	}
	archives := map[string][]byte{sha1: archive("version one"), sha2: archive("version two")}
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "forge offline", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/orgs/acme/repos":
			io.WriteString(w, `[{"id":42,"name":"foo.app","full_name":"acme/foo.app","default_branch":"main"}]`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/foo.app/commits/"):
			json.NewEncoder(w).Encode(map[string]any{"sha": head.Load().(string), "parents": []any{}})
		case r.URL.Path == "/repos/acme/foo.app/branches":
			json.NewEncoder(w).Encode([]any{map[string]any{"name": "main", "commit": map[string]string{"sha": head.Load().(string)}}})
		case r.URL.Path == "/repos/acme/foo.app/contents/manifest.json":
			io.WriteString(w, `{"schemaVersion":1,"publish":{"directory":"public"}}`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/foo.app/tarball/"):
			sha := strings.TrimPrefix(r.URL.Path, "/repos/acme/foo.app/tarball/")
			if b, ok := archives[sha]; ok {
				w.Write(b)
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer forge.Close()
	bucket := os.Getenv("PAGE_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "page-storage-test"
	}
	cfg := Config{BaseDomain: "example.com", PreviewStyle: "flat", Storage: storage.S3Config{Bucket: bucket, Prefix: "mvp-" + strconv.FormatInt(time.Now().UnixNano(), 36), Region: "us-east-1", Endpoint: endpoint, PathStyle: true}, GitHub: []githubsync.Config{{ID: "source", Owner: "acme", APIURL: forge.URL, RecentCommits: 1, GCGrace: time.Millisecond}}}
	b, e := yaml.Marshal(cfg)
	if e != nil {
		t.Fatal(e)
	}
	file := configFile(t, string(b))
	var out, errOut bytes.Buffer
	store, e := storage.NewS3(ctx, cfg.Storage)
	if e != nil {
		t.Fatal(e)
	}
	cat := deploy.New(store)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cursor := ""
		var keys []string
		for {
			p, e := store.List(cleanup, "", cursor, 1000)
			if e != nil {
				t.Log(e)
				return
			}
			for _, o := range p.Objects {
				keys = append(keys, o.Key)
			}
			if p.NextCursor == "" {
				break
			}
			cursor = p.NextCursor
		}
		for _, k := range keys {
			if e := store.Delete(cleanup, k); e != nil {
				t.Log(e)
			}
		}
	})
	syncOnce := func() {
		t.Helper()
		if e := Run(ctx, []string{"sync", "--once", "--config", file}, &out, &errOut); e != nil {
			t.Fatal(e)
		}
	}
	syncOnce()
	s, e := cat.GetSite(ctx, "foo")
	if e != nil {
		t.Fatal(e)
	}
	oldID := s.Production
	h, e := serving.New(deploy.New(store), serving.Config{BaseDomain: "example.com"})
	if e != nil {
		t.Fatal(e)
	}
	check := func(host string, status int, body string) {
		t.Helper()
		if e := h.Refresh(ctx); e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequest("GET", "http://"+host+"/", nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != status || (body != "" && w.Body.String() != body) {
			t.Fatalf("%s: status%d body%q", host, w.Code, w.Body.String())
		}
	}
	check("foo.example.com", 200, "version one")
	oldHost := deploy.Host("example.com", "flat", "foo", deploy.AliasToken("commit", sha1))
	check(oldHost, 200, "version one")
	head.Store(sha2)
	syncOnce()
	check("foo.example.com", 200, "version two")
	check(oldHost, 404, "")
	fail.Store(true)
	if e = Run(ctx, []string{"sync", "--once", "--config", file}, &out, &errOut); e == nil {
		t.Fatal("forge failure accepted")
	}
	check("foo.example.com", 200, "version two")
	fail.Store(false)
	// The API round trips above already exceed the explicit one millisecond grace.
	syncOnce()
	if _, e = cat.GetRelease(ctx, "source", "foo", oldID); !errors.Is(e, storage.ErrNotFound) {
		t.Fatalf("retired release not collected: %v", e)
	}
	out.Reset()
	if e = Run(ctx, []string{"status", "--config", file}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), sha2) || !strings.Contains(out.String(), "https://foo.example.com") {
		t.Fatalf("missing publication status: %s", out.String())
	}
}
