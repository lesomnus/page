package serving

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

func publish(t *testing.T, c *deploy.Catalog, site, revision string, files map[string]string) *deploy.Release {
	t.Helper()
	var inputs []deploy.InputFile
	for p, body := range files {
		inputs = append(inputs, deploy.InputFile{Path: p, Size: int64(len(body)), Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }})
	}
	r, err := c.Publish(context.Background(), site, "test-source", deploy.Origin{Kind: "git", RepositoryID: "1-" + site, Repository: "acme/" + site + ".app", Revision: revision, Directory: "public"}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func save(t *testing.T, c *deploy.Catalog, r *deploy.Release, aliases map[string]deploy.Alias) {
	t.Helper()
	err := c.SaveSite(context.Background(), &deploy.Site{SchemaVersion: deploy.SchemaVersion, Name: r.Site, Owner: r.Owner, RepositoryID: r.Origin.RepositoryID, Repository: r.Origin.Repository, Production: r.ID, Aliases: aliases})
	if err != nil {
		t.Fatal(err)
	}
}

func handler(t *testing.T, c *deploy.Catalog, style string) *Handler {
	t.Helper()
	h, err := New(c, Config{BaseDomain: "example.com", PreviewStyle: style})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

func request(h http.Handler, method, host, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+target, nil)
	r.Host = host
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHostAndSnapshotRouting(t *testing.T) {
	c := deploy.New(storage.NewMemory())
	first := publish(t, c, "foo", "first", map[string]string{"index.html": "foo-v1", "assets/app.js": "foo-js", "docs/index.html": "docs"})
	preview := publish(t, c, "foo", "preview", map[string]string{"index.html": "preview"})
	token := deploy.AliasToken("branch", "feature/login")
	save(t, c, first, map[string]deploy.Alias{token: {Kind: "branch", Name: "feature/login", Revision: "preview", Release: preview.ID}})
	bar := publish(t, c, "bar", "first", map[string]string{"index.html": "bar"})
	save(t, c, bar, nil)
	h := handler(t, c, "flat")
	for _, tc := range []struct {
		host, path, body string
		status           int
	}{
		{"foo.example.com", "/", "foo-v1", 200},
		{"FOO.EXAMPLE.COM:8080", "/", "foo-v1", 200},
		{"bar.example.com", "/", "bar", 200},
		{deploy.Host("example.com", "flat", "foo", token), "/", "preview", 200},
		{"unknown.example.com", "/", "", 404},
		{"example.com", "/", "", 404},
		{"foo.example.com.evil.test", "/", "", 404},
		{"foo.example.com", "/assets/app.js", "foo-js", 200},
		{"foo.example.com", "/missing", "", 404},
		{"foo.example.com", "/docs", "", 301},
		{"foo.example.com", "/docs/", "docs", 200},
	} {
		t.Run(tc.host+tc.path, func(t *testing.T) {
			w := request(h, "GET", tc.host, tc.path, nil)
			if w.Code != tc.status || (tc.body != "" && w.Body.String() != tc.body) {
				t.Fatalf("got %d %q", w.Code, w.Body.String())
			}
		})
	}
	second := publish(t, c, "foo", "second", map[string]string{"index.html": "foo-v2"})
	save(t, c, second, nil)
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "foo-v1" {
		t.Fatalf("snapshot moved without refresh: %q", got)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "foo-v2" {
		t.Fatalf("snapshot did not move: %q", got)
	}
	if got := request(h, "GET", deploy.Host("example.com", "flat", "foo", token), "/", nil).Code; got != 404 {
		t.Fatalf("expired preview status %d", got)
	}
	nested := handler(t, c, "nested")
	save(t, c, second, map[string]deploy.Alias{token: {Kind: "branch", Name: "feature/login", Revision: "preview", Release: preview.ID}})
	if err := nested.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := request(nested, "GET", deploy.Host("example.com", "nested", "foo", token), "/", nil).Body.String(); got != "preview" {
		t.Fatalf("nested preview: %q", got)
	}
}

func TestConditionalAndRangeResponses(t *testing.T) {
	counted := &countingStore{Store: storage.NewMemory()}
	c := deploy.New(counted)
	r := publish(t, c, "foo", "first", map[string]string{"index.html": "0123456789", "empty.txt": ""})
	save(t, c, r, nil)
	h := handler(t, c, "")
	full := request(h, "GET", "foo.example.com", "/", nil)
	if full.Code != 200 || full.Header().Get("Content-Type") != "text/html; charset=utf-8" || full.Header().Get("Content-Length") != "10" {
		t.Fatalf("full headers: %v %d", full.Header(), full.Code)
	}
	etag := full.Header().Get("ETag")
	if etag == "" || full.Header().Get("Last-Modified") == "" {
		t.Fatal("missing validators")
	}
	before := counted.ranges.Load()
	head := request(h, "HEAD", "foo.example.com", "/", nil)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "10" || counted.ranges.Load() != before {
		t.Fatalf("HEAD fetched a body: %d %q", head.Code, head.Body.String())
	}
	for _, tc := range []struct {
		headers            map[string]string
		status             int
		body, contentRange string
	}{
		{map[string]string{"If-None-Match": etag}, 304, "", ""},
		{map[string]string{"If-Modified-Since": full.Header().Get("Last-Modified")}, 304, "", ""},
		{map[string]string{"If-Match": "\"wrong\""}, 412, "", ""},
		{map[string]string{"Range": "bytes=2-5"}, 206, "2345", "bytes 2-5/10"},
		{map[string]string{"Range": "bytes=-3"}, 206, "789", "bytes 7-9/10"},
		{map[string]string{"Range": "bytes=20-"}, 416, "", "bytes */10"},
		{map[string]string{"Range": "bytes=1-2", "If-Range": "\"wrong\""}, 200, "0123456789", ""},
		{map[string]string{"Range": "bytes=1-2", "If-Range": etag}, 206, "12", "bytes 1-2/10"},
	} {
		t.Run(fmt.Sprint(tc.headers), func(t *testing.T) {
			w := request(h, "GET", "foo.example.com", "/", tc.headers)
			if w.Code != tc.status || (tc.body != "" && w.Body.String() != tc.body) || w.Header().Get("Content-Range") != tc.contentRange {
				t.Fatalf("got %d %q %v", w.Code, w.Body.String(), w.Header())
			}
		})
	}
	empty := request(h, "GET", "foo.example.com", "/empty.txt", nil)
	if empty.Code != 200 || empty.Body.Len() != 0 {
		t.Fatalf("empty object: %d %q", empty.Code, empty.Body.String())
	}
	multi := request(h, "GET", "foo.example.com", "/", map[string]string{"Range": "bytes=0-1,8-9"})
	if multi.Code != 206 || !strings.HasPrefix(multi.Header().Get("Content-Type"), "multipart/byteranges;") || !strings.Contains(multi.Body.String(), "89") {
		t.Fatalf("multipart response: %d %q", multi.Code, multi.Body.String())
	}
}

type countingStore struct {
	storage.Store
	ranges   atomic.Int64
	failList atomic.Bool
}

func (s *countingStore) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, storage.Info, error) {
	s.ranges.Add(1)
	return s.Store.GetRange(ctx, key, off, length)
}
func (s *countingStore) List(ctx context.Context, prefix, cursor string, limit int) (storage.Page, error) {
	if s.failList.Load() {
		return storage.Page{}, errors.New("storage temporarily unavailable")
	}
	return s.Store.List(ctx, prefix, cursor, limit)
}

func TestRefreshFailurePreservesAndSuccessfulRemovalRevokes(t *testing.T) {
	store := &countingStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	r := publish(t, c, "foo", "first", map[string]string{"index.html": "still serving"})
	save(t, c, r, nil)
	h := handler(t, c, "")
	store.failList.Store(true)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "still serving" {
		t.Fatalf("source-independent serving failed: %q", got)
	}
	store.failList.Store(false)
	if err := store.Delete(context.Background(), deploy.SiteKey("foo")); err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("removed site returned %d", got)
	}
}

func TestInvalidPathsAndMethods(t *testing.T) {
	c := deploy.New(storage.NewMemory())
	r := publish(t, c, "foo", "first", map[string]string{"index.html": "safe"})
	save(t, c, r, nil)
	h := handler(t, c, "")
	for _, p := range []string{"/../index.html", "/%2e%2e/index.html", "/%252e%252e/index.html", "/a%2f..%2findex.html", "/a%5cindex.html", "//index.html", "/a/./index.html", "/%00", "/%09", "/%1f", "/%7f", "/%ff"} {
		w := request(h, "GET", "foo.example.com", p, nil)
		if w.Code != 400 {
			t.Fatalf("path %q returned %d", p, w.Code)
		}
	}
	w := request(h, "POST", "foo.example.com", "/", nil)
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("unexpected method response: %d", w.Code)
	}
	for _, cfg := range []Config{{BaseDomain: ""}, {BaseDomain: "evil/host"}, {BaseDomain: "example.com", PreviewStyle: "unknown"}, {BaseDomain: "example.com", RefreshInterval: -time.Second}} {
		if _, err := New(c, cfg); err == nil {
			t.Fatalf("accepted config %+v", cfg)
		}
	}
}

func TestRunCancelsAndRecoversFromStoreFailure(t *testing.T) {
	store := &countingStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	h, err := New(c, Config{BaseDomain: "example.com", RefreshInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for h.current.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.current.Load() == nil {
		cancel()
		t.Fatal("no initial snapshot")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run exit: %v", err)
	}
}
