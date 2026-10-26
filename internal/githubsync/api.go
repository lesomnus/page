package githubsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (s *Sync) request(ctx context.Context, endpoint, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.APIURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "lesomnus-page")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if s.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &apiError{resp.StatusCode, endpoint}
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
