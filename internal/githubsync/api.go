package githubsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var errNoManifest = errors.New("deployment manifest absent")

type repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}
type commitID struct {
	SHA string `json:"sha"`
}
type commit struct {
	SHA     string     `json:"sha"`
	Parents []commitID `json:"parents"`
}
type namedCommit struct {
	Name   string   `json:"name"`
	Commit commitID `json:"commit"`
}
type apiError struct {
	status   int
	endpoint string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitHub %s returned HTTP %d", e.endpoint, e.status)
}

// A private archive redirects to a short-lived signed URL. http.Client errors
// can contain that URL, including credentials, so only render the original API
// path and a safe error class. Keep the cause available for cancellation and
// network error checks without including its arbitrary text in sync logs.
type requestError struct {
	endpoint string
	cause    error
}

func (e *requestError) Error() string {
	cause := e.cause
	for {
		u, ok := cause.(*url.Error)
		if !ok {
			break
		}
		cause = u.Err
	}
	detail := fmt.Sprintf("transport error %T", cause)
	var network net.Error
	switch {
	case errors.Is(e.cause, context.Canceled):
		detail = "context canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		detail = "context deadline exceeded"
	case errors.As(e.cause, &network) && network.Timeout():
		detail = "timeout"
	case errors.Is(e.cause, io.ErrUnexpectedEOF):
		detail = "unexpected EOF"
	case errors.Is(e.cause, io.EOF):
		detail = "EOF"
	}
	return fmt.Sprintf("GitHub %s request failed: %s", e.endpoint, detail)
}

func (e *requestError) Unwrap() error { return e.cause }

func apiPath(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "<invalid endpoint>"
	}
	return u.EscapedPath()
}

func (s *Sync) request(ctx context.Context, endpoint, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.APIURL+endpoint, nil)
	if err != nil {
		return nil, &requestError{apiPath(endpoint), err}
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "lesomnus-page")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if s.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, &requestError{apiPath(endpoint), err}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &apiError{resp.StatusCode, apiPath(endpoint)}
	}
	return resp, nil
}

func (s *Sync) commit(ctx context.Context, repository, revision string) (commit, error) {
	var c commit
	resp, err := s.request(ctx, repoEndpoint(repository)+"/commits/"+url.PathEscape(revision), "application/vnd.github+json")
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&c); err != nil {
		return c, err
	}
	if !validSHA(c.SHA) {
		return c, errors.New("GitHub returned invalid commit SHA")
	}
	for _, p := range c.Parents {
		if !validSHA(p.SHA) {
			return c, errors.New("GitHub returned invalid parent SHA")
		}
	}
	return c, nil
}

func listPages[T any](ctx context.Context, s *Sync, endpoint string) ([]T, error) {
	var all []T
	seen := map[[32]byte]bool{}
	for page := 1; page <= 10000; page++ {
		separator := "?"
		if strings.Contains(endpoint, "?") {
			separator = "&"
		}
		resp, err := s.request(ctx, endpoint+separator+"per_page=100&page="+strconv.Itoa(page), "application/vnd.github+json")
		if err != nil {
			return nil, err
		}
		var items []T
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&items)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(encoded)
		if len(items) > 0 && seen[digest] {
			return nil, errors.New("GitHub pagination repeated a page")
		}
		seen[digest] = true
		all = append(all, items...)
		next := strings.Contains(resp.Header.Get("Link"), `rel="next"`)
		if !next && len(items) < 100 {
			return all, nil
		}
		if len(items) == 0 {
			return nil, errors.New("GitHub pagination did not advance")
		}
	}
	return nil, errors.New("GitHub pagination limit exceeded")
}
