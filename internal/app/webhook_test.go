package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/githubsync"
	"github.com/lesomnus/page/internal/storage"
)

func signedWebhook(body, event string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("Content-Type", "application/json")
	mac := hmac.New(sha256.New, []byte("test-webhook-secret"))
	mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestGitHubWebhookAuthenticationAndPayloadBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		event  string
		change func(*http.Request)
		status int
	}{
		{"missing signature", `{}`, "push", func(r *http.Request) { r.Header.Del("X-Hub-Signature-256") }, http.StatusUnauthorized},
		{"old SHA1 signature", `{}`, "push", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha1="+strings.Repeat("a", 40)) }, http.StatusUnauthorized},
		{"malformed signature", `{}`, "push", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("z", 64)) }, http.StatusUnauthorized},
		{"wrong signature", `{}`, "push", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64)) }, http.StatusUnauthorized},
		{"tampered payload", `{"repository":{"full_name":"acme/foo.hday.page"}}`, "push", func(r *http.Request) { r.Body = http.NoBody }, http.StatusUnauthorized},
		{"malformed JSON", `{"repository":`, "push", nil, http.StatusBadRequest},
		{"trailing JSON", `{} {}`, "push", nil, http.StatusBadRequest},
		{"missing repository", `{}`, "push", nil, http.StatusBadRequest},
		{"missing event", `{}`, "", nil, http.StatusBadRequest},
		{"body too large", strings.Repeat("x", maxWebhookBytes+1), "push", nil, http.StatusRequestEntityTooLarge},
		{"wrong path", `{}`, "ping", func(r *http.Request) { r.URL.Path = "/" }, http.StatusNotFound},
		{"wrong method", `{}`, "ping", func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notified := false
			h, err := newGitHubWebhookHandler("test-webhook-secret", func(string) { notified = true })
			if err != nil {
				t.Fatal(err)
			}
			r := signedWebhook(tc.body, tc.event)
			if tc.change != nil {
				tc.change(r)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || notified {
				t.Fatalf("status %d want %d; notified=%t", w.Code, tc.status, notified)
			}
			if strings.Contains(w.Body.String(), "test-webhook-secret") || strings.Contains(w.Body.String(), "acme/foo.hday.page") || strings.Contains(w.Body.String(), r.Header.Get("X-Hub-Signature-256")) && r.Header.Get("X-Hub-Signature-256") != "" {
				t.Fatal("webhook response echoed sensitive input")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("webhook response can be cached")
			}
		})
	}
}

func TestGitHubWebhookPingsAndSupportedEvents(t *testing.T) {
	for _, event := range []string{"ping", "push", "create", "delete", "repository", "issues"} {
		t.Run(event, func(t *testing.T) {
			var names []string
			h, err := newGitHubWebhookHandler("test-webhook-secret", func(name string) { names = append(names, name) })
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, signedWebhook(`{"repository":{"full_name":"acme/foo.hday.page"},"after":"never-used-as-a-revision"}`, event))
			want := http.StatusAccepted
			if event == "ping" {
				want = http.StatusOK
			}
			if w.Code != want {
				t.Fatalf("got %d want %d", w.Code, want)
			}
			if event == "ping" || event == "issues" {
				if len(names) != 0 {
					t.Fatal("non-publication event notified a source")
				}
			} else if len(names) != 1 || names[0] != "acme/foo.hday.page" {
				t.Fatalf("wrong repository notification: %#v", names)
			}
		})
	}
}

func TestGitHubWebhookOfficialSignatureVector(t *testing.T) {
	// GitHub's published example validates compatibility without generating
	// the expected signature using the same implementation as the receiver.
	h, err := newGitHubWebhookHandler("It's a Secret to Everybody", func(string) { t.Fatal("non-JSON example notified a source") })
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("Hello, World!"))
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-Hub-Signature-256", "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("external signature was rejected before JSON validation: %d", w.Code)
	}
}

func TestGitHubWebhookConstructorRequiresSecretAndSources(t *testing.T) {
	if _, err := newGitHubWebhookHandler("", func(string) {}); err == nil {
		t.Fatal("accepted missing secret")
	}
	if _, err := newGitHubWebhookHandler("secret", nil); err == nil {
		t.Fatal("accepted missing notifier")
	}
	if _, err := githubWebhookHandler("secret", nil); err == nil {
		t.Fatal("accepted missing sources")
	}
	if _, err := githubWebhookHandler("secret", []*githubsync.Sync{nil}); err == nil {
		t.Fatal("accepted nil source")
	}
}

func TestGitHubWebhookOnlyWakesConfiguredRepositories(t *testing.T) {
	seen := make(chan struct{}, 10)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		seen <- struct{}{}
		w.Write([]byte("[]"))
	}))
	defer server.Close()
	source, err := githubsync.New(deploy.New(storage.NewMemory()), githubsync.Config{ID: "source", Owner: "acme", APIURL: server.URL, RepositoryPattern: `^([a-z0-9][a-z0-9-]*)\.hday\.page$`, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("source did not stop")
		}
	}()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("source startup did not run")
	}
	h, err := githubWebhookHandler("test-webhook-secret", []*githubsync.Sync{source})
	if err != nil {
		t.Fatal(err)
	}
	for _, full := range []string{"acme/homepage", "other/foo.hday.page"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedWebhook(`{"repository":{"full_name":"`+full+`"}}`, "push"))
		if w.Code != http.StatusAccepted {
			t.Fatal(w.Code)
		}
	}
	time.Sleep(350 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("unrelated homepage or owner caused source activity")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedWebhook(`{"repository":{"full_name":"acme/foo.hday.page"}}`, "push"))
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code)
	}
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("matching webhook did not wake source")
	}
}
