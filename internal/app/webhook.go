package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/lesomnus/page/internal/githubsync"
)

// Repository/ref changes only need a small part of a GitHub payload. Larger
// deliveries can be recovered by the source's normal reconciliation interval.
const maxWebhookBytes = 2 << 20

func githubWebhookHandler(secret string, syncs []*githubsync.Sync) (http.Handler, error) {
	if len(syncs) == 0 {
		return nil, errors.New("GitHub webhook requires a source")
	}
	for _, source := range syncs {
		if source == nil {
			return nil, errors.New("GitHub webhook has an invalid source")
		}
	}
	return newGitHubWebhookHandler(secret, func(fullName string) {
		for _, source := range syncs {
			source.NotifyRepository(fullName)
		}
	})
}

func newGitHubWebhookHandler(secret string, notify func(string)) (http.Handler, error) {
	if secret == "" {
		return nil, errors.New("GitHub webhook requires a secret")
	}
	if notify == nil {
		return nil, errors.New("GitHub webhook requires a notifier")
	}
	key := []byte(secret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/webhooks/github" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Reject absent/malformed authentication before reading a request body.
		signature := r.Header.Get("X-Hub-Signature-256")
		if !strings.HasPrefix(signature, "sha256=") || len(signature) != len("sha256=")+sha256.Size*2 {
			http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
			return
		}
		provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
		if err != nil {
			http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
			return
		}
		body := http.MaxBytesReader(w, r.Body, maxWebhookBytes)
		defer body.Close()
		payload, err := io.ReadAll(body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "webhook payload too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "cannot read webhook payload", http.StatusBadRequest)
			}
			return
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(payload)
		if !hmac.Equal(provided, mac.Sum(nil)) {
			http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
			return
		}
		event := r.Header.Get("X-GitHub-Event")
		switch event {
		case "ping", "push", "create", "delete", "repository":
		case "":
			http.Error(w, "missing webhook event", http.StatusBadRequest)
			return
		default:
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, "ignored\n")
			return
		}
		var delivery struct {
			Repository *struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(payload, &delivery); err != nil {
			http.Error(w, "invalid webhook payload", http.StatusBadRequest)
			return
		}
		if event == "ping" {
			io.WriteString(w, "pong\n")
			return
		}
		if delivery.Repository == nil || delivery.Repository.FullName == "" {
			http.Error(w, "missing webhook repository", http.StatusBadRequest)
			return
		}
		// The payload never supplies the revision to publish. Each matching
		// source wakes its single writer, which reads current state from GitHub.
		notify(delivery.Repository.FullName)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "accepted\n")
	}), nil
}
