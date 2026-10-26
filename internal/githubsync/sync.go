// Package githubsync discovers GitHub repositories and owns their publications.
// Source policy lives here; the serving plane only consumes published snapshots.
package githubsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

type Config struct {
	ID                string        `yaml:"id"`
	Owner             string        `yaml:"owner"`
	OwnerType         string        `yaml:"owner_type"`
	Token             string        `yaml:"token"`
	APIURL            string        `yaml:"api_url"`
	RepositoryPattern string        `yaml:"repository_pattern"`
	Branch            string        `yaml:"branch"`
	ManifestPath      string        `yaml:"manifest_path"`
	PollInterval      time.Duration `yaml:"poll_interval"`
	GCGrace           time.Duration `yaml:"gc_grace"`
	AssetRetention    time.Duration `yaml:"asset_retention"`
	Branches          []string      `yaml:"branches"`
	Tags              []string      `yaml:"tags"`
	RecentCommits     int           `yaml:"recent_commits"`
	MaxArchiveBytes   int64         `yaml:"max_archive_bytes"`
	MaxFileBytes      int64         `yaml:"max_file_bytes"`
	MaxFiles          int           `yaml:"max_files"`
}

type Sync struct {
	cat     *deploy.Catalog
	cfg     Config
	pattern *regexp.Regexp
	client  *http.Client
	mu      sync.Mutex
	now     func() time.Time
}

func New(catalog *deploy.Catalog, cfg Config) (*Sync, error) {
	if catalog == nil || catalog.Store == nil {
		return nil, errors.New("GitHub sync requires a catalog")
	}
	if !deploy.ValidName(cfg.ID) {
		return nil, errors.New("GitHub sync id must be a stable DNS label")
	}
	if cfg.Owner == "" || strings.ContainsAny(cfg.Owner, "/\\\x00\r\n") {
		return nil, errors.New("GitHub owner is required")
	}
	if cfg.OwnerType == "" {
		cfg.OwnerType = "org"
	}
	if cfg.OwnerType != "org" && cfg.OwnerType != "user" {
		return nil, errors.New("owner_type must be org or user")
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	u, err := url.Parse(cfg.APIURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid GitHub API URL")
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	if cfg.RepositoryPattern == "" {
		cfg.RepositoryPattern = `^([a-z0-9][a-z0-9-]*)\.app$`
	}
	pattern, err := regexp.Compile(cfg.RepositoryPattern)
	if err != nil || pattern.NumSubexp() < 1 {
		return nil, errors.New("repository_pattern needs a capture group for the site name")
	}
	if cfg.ManifestPath == "" {
		cfg.ManifestPath = "manifest.json"
	}
	if !safePath(cfg.ManifestPath, false) {
		return nil, errors.New("manifest_path must be a relative repository file")
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Minute
	}
	if cfg.GCGrace == 0 {
		cfg.GCGrace = 24 * time.Hour
	}
	if cfg.AssetRetention == 0 {
		cfg.AssetRetention = 24 * time.Hour
	}
	if cfg.RecentCommits == 0 {
		cfg.RecentCommits = 10
	}
	if cfg.MaxArchiveBytes == 0 {
		cfg.MaxArchiveBytes = 256 << 20
	}
	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 64 << 20
	}
	if cfg.MaxFiles == 0 {
		cfg.MaxFiles = 10000
	}
	if cfg.Branches == nil {
		cfg.Branches = []string{"*"}
	}
	if cfg.PollInterval < 0 || cfg.GCGrace < 0 || cfg.AssetRetention < 0 || cfg.RecentCommits < 0 || cfg.RecentCommits > 1000 || cfg.MaxArchiveBytes < 1 || cfg.MaxFileBytes < 1 || cfg.MaxFiles < 1 {
		return nil, errors.New("invalid GitHub polling, retention or upload limits")
	}
	for _, p := range append(append([]string{}, cfg.Branches...), cfg.Tags...) {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("invalid ref pattern %q: %w", p, err)
		}
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many archive redirects")
		}
		if req.URL.Scheme != "https" && !(req.URL.Scheme == "http" && req.URL.Host == u.Host) {
			return errors.New("unsafe archive redirect")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}}
	return &Sync{cat: catalog, cfg: cfg, pattern: pattern, client: client, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Run scans immediately and then polls. An unsuccessful pass retains the last
// successful publication, logs its error, and is retried on the next interval.
func (s *Sync) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "GitHub sync failed", "source", s.cfg.ID, "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Once performs a complete repository listing before changing any publication.
// Missing repositories are retained: deletion and loss of access are not the
// same event, and removing a source never silently deletes its sites.
func (s *Sync) Once(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Operator intent is independent of forge reachability. Only the source
	// owner materializes it into publication state, keeping activation and GC
	// serialized even when the CLI runs in a separate process.
	controlErr := s.reconcileControls(ctx)
	endpoint := "/orgs/" + url.PathEscape(s.cfg.Owner) + "/repos"
	if s.cfg.OwnerType == "user" {
		endpoint = "/users/" + url.PathEscape(s.cfg.Owner) + "/repos"
		if s.cfg.Token != "" {
			// The public user endpoint never includes private personal repos.
			// User mode with credentials therefore requires a user access token
			// or PAT, rather than a GitHub App installation token.
			endpoint = "/user/repos?affiliation=owner"
		}
	}
	repos, err := listPages[repository](ctx, s, endpoint)
	if err != nil {
		return errors.Join(controlErr, err)
	}
	type candidate struct {
		repo repository
		name string
	}
	var candidates []candidate
	names := map[string]bool{}
	for _, repo := range repos {
		if s.cfg.OwnerType == "user" {
			owner, _, ok := strings.Cut(repo.FullName, "/")
			if !ok || !strings.EqualFold(owner, s.cfg.Owner) {
				continue
			}
		}
		match := s.pattern.FindStringSubmatch(repo.Name)
		if match == nil {
			continue
		}
		name := strings.ToLower(match[1])
		if !deploy.ValidName(name) {
			return fmt.Errorf("repository %s produces invalid site name %q", repo.FullName, name)
		}
		if names[name] {
			return fmt.Errorf("multiple repositories map to site %q", name)
		}
		if !validRepository(repo.FullName) || repo.ID <= 0 {
			return fmt.Errorf("invalid repository identity for %q", repo.Name)
		}
		names[name] = true
		candidates = append(candidates, candidate{repo, name})
	}
	var errs []error
	if controlErr != nil {
		errs = append(errs, controlErr)
	}
	for _, c := range candidates {
		if err := s.syncRepository(ctx, c.repo, c.name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.repo.FullName, err))
		}
	}
	return errors.Join(errs...)
}

type wantedRef struct{ kind, name, sha string }

func (s *Sync) syncRepository(ctx context.Context, repo repository, name string) error {
	site, err := s.cat.GetSite(ctx, name)
	if errors.Is(err, storage.ErrNotFound) {
		site = &deploy.Site{SchemaVersion: deploy.SchemaVersion, Name: name, Owner: s.cfg.ID, RepositoryID: strconv.FormatInt(repo.ID, 10), Repository: repo.FullName, Aliases: map[string]deploy.Alias{}, Retired: map[string]time.Time{}}
	} else if err != nil {
		return err
	}
	if site.Owner != s.cfg.ID || site.RepositoryID != strconv.FormatInt(repo.ID, 10) {
		return deploy.ErrOwnership
	}
	control, err := s.cat.GetControl(ctx, site)
	if err != nil {
		return err
	}
	if control.Mode == "disabled" {
		return s.persistAndCollect(ctx, site)
	}
	branch := s.cfg.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if branch == "" {
		return errors.New("repository has no production branch")
	}
	productionRef := branch
	if !strings.HasPrefix(productionRef, "refs/heads/") {
		productionRef = "refs/heads/" + productionRef
	}
	head, err := s.commit(ctx, repo.FullName, productionRef)
	if err != nil {
		return err
	}
	refs := []wantedRef{}
	if len(s.cfg.Branches) > 0 {
		branches, err := listPages[namedCommit](ctx, s, repoEndpoint(repo.FullName)+"/branches")
		if err != nil {
			return err
		}
		for _, b := range branches {
			if matches(s.cfg.Branches, b.Name) {
				refs = append(refs, wantedRef{"branch", "refs/heads/" + b.Name, b.Commit.SHA})
			}
		}
	}
	if len(s.cfg.Tags) > 0 {
		tags, err := listPages[namedCommit](ctx, s, repoEndpoint(repo.FullName)+"/tags")
		if err != nil {
			return err
		}
		for _, t := range tags {
			if matches(s.cfg.Tags, t.Name) {
				refs = append(refs, wantedRef{"tag", "refs/tags/" + t.Name, t.Commit.SHA})
			}
		}
	}
	selected := map[string]bool{head.SHA: true}
	for _, ref := range refs {
		if !validSHA(ref.sha) {
			return errors.New("invalid ref commit SHA")
		}
		selected[ref.sha] = true
	}
	current := head
	for i := 0; i < s.cfg.RecentCommits; i++ {
		selected[current.SHA] = true
		if len(current.Parents) == 0 || i+1 == s.cfg.RecentCommits {
			break
		}
		current, err = s.commit(ctx, repo.FullName, current.Parents[0].SHA)
		if err != nil {
			return err
		}
	}
	shas := make([]string, 0, len(selected))
	for sha := range selected {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	published := map[string]*deploy.Release{}
	for _, sha := range shas {
		r, err := s.publish(ctx, repo, name, sha)
		if errors.Is(err, errNoManifest) {
			slog.InfoContext(ctx, "revision has no deployment manifest", "repository", repo.FullName, "revision", sha)
			continue
		}
		if err != nil {
			return err
		} // No ref removal or GC after an incomplete pass.
		published[sha] = r
	}
	production := site.Production
	if r := published[head.SHA]; r != nil {
		production = r.ID
	}
	if production == "" {
		return s.collectUnpublished(ctx, site)
	} // Discovery does not publish an empty site.
	aliases := map[string]deploy.Alias{}
	for _, ref := range refs {
		token := deploy.AliasToken(ref.kind, ref.name)
		if r := published[ref.sha]; r != nil {
			aliases[token] = deploy.Alias{Kind: ref.kind, Name: ref.name, Revision: ref.sha, Release: r.ID}
		} else if old, ok := site.Aliases[token]; ok {
			aliases[token] = old
		}
	}
	for sha, r := range published {
		aliases[deploy.AliasToken("commit", sha)] = deploy.Alias{Kind: "commit", Name: sha, Revision: sha, Release: r.ID}
	}
	// Preserve a stable commit URL for a selected ref whose newer publication
	// failed or lost its manifest, while keeping that ref's last good snapshot.
	for _, a := range aliases {
		if a.Kind != "commit" {
			aliases[deploy.AliasToken("commit", a.Revision)] = deploy.Alias{Kind: "commit", Name: a.Revision, Revision: a.Revision, Release: a.Release}
		}
	}
	site.Repository = repo.FullName
	site.Production = production
	site.Aliases = aliases
	if err := s.persistAndCollect(ctx, site); err != nil {
		return err
	}
	slog.InfoContext(ctx, "repository synchronized", "source", s.cfg.ID, "repository", repo.FullName, "site", name, "production", site.Production, "previews", len(site.Aliases))
	return nil
}

// Failed first uploads have no published Site to carry retirement timestamps.
// A successful source pass can still collect old, incomplete owned uploads by
// their durable creation time without ever publishing an empty site.
func (s *Sync) collectUnpublished(ctx context.Context, site *deploy.Site) error {
	releases, err := s.cat.ListReleases(ctx, site.Owner, site.Name)
	if err != nil {
		return err
	}
	var errs []error
	for _, release := range releases {
		if release.Complete || s.now().Sub(release.CreatedAt) < s.cfg.GCGrace {
			continue
		}
		if err := s.cat.DeleteRelease(ctx, site, release.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func matches(patterns []string, name string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}
func validRepository(name string) bool {
	p := strings.Split(name, "/")
	return len(p) == 2 && p[0] != "" && p[1] != "" && p[0] != "." && p[1] != "." && p[0] != ".." && p[1] != ".." && !strings.ContainsAny(name, "\\\x00\r\n")
}
func repoEndpoint(name string) string {
	p := strings.SplitN(name, "/", 2)
	return "/repos/" + url.PathEscape(p[0]) + "/" + url.PathEscape(p[1])
}
func validSHA(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, c := range sha {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
