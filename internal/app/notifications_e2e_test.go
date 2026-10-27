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
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/githubsync"
	"github.com/lesomnus/page/internal/serving"
	"github.com/lesomnus/page/internal/storage"
)

// Cross the complete notification path with both periodic timers set to an
// hour: signed delivery -> source reconciliation -> atomic publication -> reader.
func TestWebhookPublishesAndRefreshesWithoutPolling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	firstSHA, secondSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	archive := func(body string) []byte {
		t.Helper()
		var data bytes.Buffer
		gz := gzip.NewWriter(&data)
		tr := tar.NewWriter(gz)
		if err := tr.WriteHeader(&tar.Header{Name: "repo/public/index.html", Size: int64(len(body)), Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tr, body); err != nil {
			t.Fatal(err)
		}
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return data.Bytes()
	}
	archives := map[string][]byte{firstSHA: archive("version one"), secondSHA: archive("version two")}
	var head atomic.Value
	head.Store(firstSHA)
	var sourceCalls atomic.Int64
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/orgs/acme/repos":
			io.WriteString(w, `[{"id":42,"name":"foo.hday.page","full_name":"acme/foo.hday.page","default_branch":"main"}]`)
		case r.URL.Path == "/repos/acme/foo.hday.page/branches":
			json.NewEncoder(w).Encode([]any{map[string]any{"name": "main", "commit": map[string]string{"sha": head.Load().(string)}}})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/foo.hday.page/commits/"):
			json.NewEncoder(w).Encode(map[string]any{"sha": head.Load().(string), "parents": []any{}})
		case r.URL.Path == "/repos/acme/foo.hday.page/contents/page.yaml":
			io.WriteString(w, "schemaVersion: 1\npublish:\n  directory: public\n")
		case strings.HasPrefix(r.URL.Path, "/repos/acme/foo.hday.page/tarball/"):
			if body, ok := archives[strings.TrimPrefix(r.URL.Path, "/repos/acme/foo.hday.page/tarball/")]; ok {
				w.Write(body)
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(forge.Close)
	cat := deploy.New(storage.NewMemory())
	source, err := githubsync.New(cat, githubsync.Config{ID: "source", Owner: "acme", APIURL: forge.URL, RepositoryPattern: `^([a-z0-9][a-z0-9-]*)\.hday\.page$`, Branches: []string{"main"}, RecentCommits: 1, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := serving.New(cat, serving.Config{BaseDomain: "example.com", RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	readerHTTP := httptest.NewServer(reader)
	t.Cleanup(readerHTTP.Close)
	readerDone, sourceDone := make(chan error, 1), make(chan error, 1)
	go func() { readerDone <- reader.Run(ctx) }()
	// A non-503 response proves startup recovery and subscription have completed
	// before the source's first publication is allowed to happen.
	client := &http.Client{Timeout: time.Second}
	read := func() (int, string) {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, readerHTTP.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = "foo.example.com"
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}
	await := func(condition func() bool) {
		t.Helper()
		for !condition() {
			select {
			case <-ctx.Done():
				t.Fatal("notification path did not converge before periodic timers")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	await(func() bool { code, _ := read(); return code == http.StatusNotFound })
	go func() { sourceDone <- source.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for _, done := range []<-chan error{readerDone, sourceDone} {
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("notification worker returned %v", err)
				}
			case <-time.After(time.Second):
				t.Error("notification worker did not stop")
			}
		}
	})
	await(func() bool { code, body := read(); return code == http.StatusOK && body == "version one" })
	first, err := cat.GetSite(ctx, "foo")
	if err != nil {
		t.Fatal(err)
	}
	hook, err := githubWebhookHandler("test-webhook-secret", []*githubsync.Sync{source})
	if err != nil {
		t.Fatal(err)
	}
	webhookHTTP := httptest.NewServer(hook)
	t.Cleanup(webhookHTTP.Close)
	deliver := func(fullName string, authenticated bool) int {
		t.Helper()
		// A stale payload revision must not select what gets published. The
		// source must discover secondSHA from the authoritative forge API.
		request := signedWebhook(`{"repository":{"full_name":"`+fullName+`"},"ref":"refs/heads/main","after":"`+firstSHA+`"}`, "push")
		request.URL, err = url.Parse(webhookHTTP.URL + "/webhooks/github")
		if err != nil {
			t.Fatal(err)
		}
		request.RequestURI, request.Host = "", ""
		if !authenticated {
			request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	head.Store(secondSHA)
	before := sourceCalls.Load()
	if code := deliver("acme/foo.hday.page", false); code != http.StatusUnauthorized {
		t.Fatalf("invalid signature accepted: %d", code)
	}
	if code := deliver("acme/homepage", true); code != http.StatusAccepted {
		t.Fatalf("unrelated delivery not acknowledged: %d", code)
	}
	time.Sleep(350 * time.Millisecond)
	if got := sourceCalls.Load(); got != before {
		t.Fatalf("invalid or unrelated delivery queried source: %d -> %d", before, got)
	}
	unchanged, err := cat.GetSite(ctx, "foo")
	if err != nil || unchanged.Production != first.Production {
		t.Fatalf("invalid or unrelated delivery changed publication: %#v %v", unchanged, err)
	}
	if code, body := read(); code != http.StatusOK || body != "version one" {
		t.Fatalf("invalid or unrelated delivery changed serving: %d %q", code, body)
	}
	if code := deliver("acme/foo.hday.page", true); code != http.StatusAccepted {
		t.Fatalf("valid delivery rejected: %d", code)
	}
	await(func() bool { code, body := read(); return code == http.StatusOK && body == "version two" })
	second, err := cat.GetSite(ctx, "foo")
	if err != nil || second.Production == first.Production {
		t.Fatalf("webhook did not publish a new release: %#v %v", second, err)
	}
	release, err := cat.GetRelease(ctx, second.Owner, second.Name, second.Production)
	if err != nil || release.Origin.Revision != secondSHA {
		t.Fatalf("payload revision overrode current source state: %#v %v", release, err)
	}
}
