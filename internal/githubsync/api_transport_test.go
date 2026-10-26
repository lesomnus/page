package githubsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

type githubTransportFunc func(*http.Request) (*http.Response, error)

func (f githubTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPrivateArchiveRedirectFailureDoesNotExposeDownloadCredentials(t *testing.T) {
	const downloadURL = "https://archive-user-sentinel:archive-password-sentinel@download.invalid/archive?token=download-token-sentinel#fragment-sentinel"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-read-token-sentinel" {
			t.Error("private archive API request did not authenticate")
		}
		http.Redirect(w, r, downloadURL, http.StatusFound)
	}))
	defer api.Close()
	cfg := config(api.URL)
	cfg.Token = "private-read-token-sentinel"
	s := newSync(t, deploy.New(storage.NewMemory()), cfg)
	failure := errors.New("download failed")
	var redirected bool
	s.client.Transport = githubTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "download.invalid" {
			redirected = true
			// A transport's error can repeat the signed URL, not just url.Error.URL.
			return nil, fmt.Errorf("download %s: %w", r.URL, failure)
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	_, err := s.request(context.Background(), "/repos/acme/foo.app/tarball/"+sha(1), "application/vnd.github+json")
	if err == nil || !redirected || !errors.Is(err, failure) {
		t.Fatalf("redirect error lost its cause: redirected=%t, error=%v", redirected, err)
	}
	message := err.Error()
	for _, secret := range []string{"private-read-token-sentinel", "archive-user-sentinel", "archive-password-sentinel", "download-token-sentinel", "fragment-sentinel", "download.invalid"} {
		if strings.Contains(message, secret) {
			t.Fatalf("transport error exposed download credentials: %s", message)
		}
	}
	if !strings.Contains(message, "/repos/acme/foo.app/tarball/"+sha(1)) || !strings.Contains(message, "transport error") {
		t.Fatalf("safe API endpoint or error class missing: %s", message)
	}
}

func TestPrivateArchiveRedirectDoesNotForwardAPIAuthorization(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-read-token" {
			t.Error("private archive API request did not authenticate")
		}
		http.Redirect(w, r, "https://download.invalid/archive?token=signed-download-token", http.StatusFound)
	}))
	defer api.Close()
	cfg := config(api.URL)
	cfg.Token = "private-read-token"
	s := newSync(t, deploy.New(storage.NewMemory()), cfg)
	var redirected bool
	s.client.Transport = githubTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "download.invalid" {
			return http.DefaultTransport.RoundTrip(r)
		}
		redirected = true
		if r.Header.Get("Authorization") != "" {
			t.Error("GitHub authorization was forwarded to archive host")
		}
		if r.URL.Query().Get("token") != "signed-download-token" {
			t.Error("signed download URL was changed")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("archive content")), Request: r}, nil
	})
	resp, err := s.request(context.Background(), "/repos/acme/foo.app/tarball/"+sha(1), "application/vnd.github+json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !redirected || string(body) != "archive content" {
		t.Fatalf("private archive download failed: redirected=%t, error=%v", redirected, err)
	}
}

func TestRequestFailureKeepsContextErrorsAndRedactsEndpointQueries(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			s := newSync(t, deploy.New(storage.NewMemory()), config("https://api.invalid"))
			s.client.Transport = githubTransportFunc(func(r *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("failed %s: %w", r.URL, cause)
			})
			_, err := s.request(context.Background(), "/orgs/acme/repos?access_token=query-secret-sentinel", "application/vnd.github+json")
			if !errors.Is(err, cause) {
				t.Fatalf("lost context error: %v", err)
			}
			if strings.Contains(err.Error(), "query-secret-sentinel") || !strings.Contains(err.Error(), cause.Error()) {
				t.Fatalf("unsafe or unhelpful context error: %v", err)
			}
		})
	}
}

func TestHTTPStatusFailureRedactsEndpointQuery(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer api.Close()
	s := newSync(t, deploy.New(storage.NewMemory()), config(api.URL))
	_, err := s.request(context.Background(), "/orgs/acme/repos?access_token=query-secret-sentinel", "application/vnd.github+json")
	if err == nil || strings.Contains(err.Error(), "query-secret-sentinel") || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("unsafe or unhelpful status error: %v", err)
	}
}

func TestInvalidRequestURLDoesNotExposeCredentials(t *testing.T) {
	s := newSync(t, deploy.New(storage.NewMemory()), config("https://api.invalid"))
	_, err := s.request(context.Background(), "/orgs/acme/repos?token=query-secret-sentinel\x00", "application/vnd.github+json")
	if err == nil || strings.Contains(err.Error(), "query-secret-sentinel") {
		t.Fatalf("request construction error exposed credentials: %v", err)
	}
}
