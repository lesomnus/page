// Package serving reads published snapshots without consulting their sources.
package serving

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/routes"
	"github.com/lesomnus/page/internal/storage"
)

type Config struct {
	BaseDomain       string        `yaml:"base_domain"`
	PreviewStyle     string        `yaml:"preview_style"`
	PreviewAliasMode string        `yaml:"preview_alias_mode"`
	RefreshInterval  time.Duration `yaml:"refresh_interval"`
}

type retainedAsset struct {
	release *deploy.Release
	engine  *routes.Engine
	expires time.Time
}
type route struct {
	release *deploy.Release
	engine  *routes.Engine
	assets  []retainedAsset
}
type snapshot struct{ routes map[string]route }

type cachedSite struct {
	site *deploy.Site
	etag string
}

type releaseKey struct{ owner, site, id string }

type Handler struct {
	catalog  *deploy.Catalog
	config   Config
	refresh  sync.Mutex
	current  atomic.Pointer[snapshot]
	sites    map[string]cachedSite
	releases map[releaseKey]route
	now      func() time.Time
}

func New(catalog *deploy.Catalog, config Config) (*Handler, error) {
	if catalog == nil || catalog.Store == nil {
		return nil, errors.New("serving: no catalog")
	}
	config.BaseDomain = strings.TrimSuffix(strings.ToLower(config.BaseDomain), ".")
	if !validHostname(config.BaseDomain) {
		return nil, errors.New("serving: invalid base domain")
	}
	if config.PreviewStyle == "" {
		config.PreviewStyle = "flat"
	}
	if config.PreviewStyle != "flat" && config.PreviewStyle != "nested" {
		return nil, errors.New("serving: preview_style must be flat or nested")
	}
	var err error
	config.PreviewAliasMode, err = deploy.PreviewAliasMode(config.PreviewAliasMode)
	if err != nil {
		return nil, fmt.Errorf("serving: %w", err)
	}
	if config.RefreshInterval == 0 {
		config.RefreshInterval = 5 * time.Minute
	}
	if config.RefreshInterval < 0 {
		return nil, errors.New("serving: refresh_interval must be positive")
	}
	return &Handler{catalog: catalog, config: config, now: time.Now}, nil
}

// Refresh replaces the complete routing snapshot only after every referenced
// release has been read successfully. An unavailable store leaves readers alone.
func (h *Handler) Refresh(ctx context.Context) error {
	h.refresh.Lock()
	defer h.refresh.Unlock()
	objects, err := h.catalog.ListSiteObjects(ctx)
	if err != nil {
		return fmt.Errorf("serving: list sites: %w", err)
	}
	nextSites := make(map[string]cachedSite, len(objects))
	sites := make([]*deploy.Site, 0, len(objects))
	for _, info := range objects {
		cached, ok := h.sites[info.Key]
		if !ok || info.ETag == "" || cached.etag == "" || info.ETag != cached.etag {
			name := strings.TrimSuffix(strings.TrimPrefix(info.Key, "sites/"), "/site.json")
			site, fetched, e := h.catalog.GetSiteInfo(ctx, name)
			if e != nil {
				return fmt.Errorf("serving: load site %s: %w", name, e)
			}
			cached = cachedSite{site: site, etag: fetched.ETag}
		}
		nextSites[info.Key] = cached
		sites = append(sites, cached.site)
	}
	previews, err := deploy.PlanPreviews(sites, h.config.BaseDomain, h.config.PreviewStyle, h.config.PreviewAliasMode)
	if err != nil {
		return fmt.Errorf("serving: plan previews: %w", err)
	}
	bySite := map[string][]deploy.PreviewRoute{}
	for _, preview := range previews.Routes {
		bySite[preview.Site] = append(bySite[preview.Site], preview)
	}
	next := &snapshot{routes: make(map[string]route)}
	nextReleases := make(map[releaseKey]route)
	now := h.now()
	for _, site := range sites {
		if !deploy.ValidName(site.Name) {
			return errors.New("serving: invalid site name")
		}
		if site.Disabled {
			continue
		}
		load := func(id string) (route, error) {
			key := releaseKey{site.Owner, site.Name, id}
			if cached, ok := nextReleases[key]; ok {
				return cached, nil
			}
			result, ok := h.releases[key]
			if !ok {
				rel, e := h.catalog.GetRelease(ctx, site.Owner, site.Name, id)
				if e != nil {
					return route{}, fmt.Errorf("serving: load release %s: %w", id, e)
				}
				files := make(map[string]bool, len(rel.Files))
				for path := range rel.Files {
					files[path] = true
				}
				engine, e := routes.Compile(rel.Pages, files)
				if e != nil {
					return route{}, fmt.Errorf("serving: compile release %s: %w", id, e)
				}
				result = route{release: rel, engine: engine}
			}
			rel := result.release
			if !rel.Complete || rel.ID != id || rel.Site != site.Name || rel.Owner != site.Owner || rel.Origin.RepositoryID != site.RepositoryID {
				return route{}, errors.New("serving: invalid release ownership or completion")
			}
			nextReleases[key] = result
			return result, nil
		}
		retained := []retainedAsset{}
		if site.Production != "" {
			for id, expiry := range site.AssetReleases {
				if !now.Before(expiry) || id == site.Production {
					continue
				}
				old, e := load(id)
				if e != nil {
					return e
				}
				retained = append(retained, retainedAsset{release: old.release, engine: old.engine, expires: expiry})
			}
			sort.Slice(retained, func(i, j int) bool {
				left, right := retained[i].release, retained[j].release
				if !left.CreatedAt.Equal(right.CreatedAt) {
					return left.CreatedAt.After(right.CreatedAt)
				}
				return left.ID < right.ID
			})
		}
		add := func(host, id, revision string, production bool) error {
			if id == "" {
				return nil
			}
			result, e := load(id)
			if e != nil {
				return e
			}
			if revision != "" && revision != result.release.Origin.Revision {
				return errors.New("serving: preview revision does not match release")
			}
			if production {
				result.assets = retained
			}
			if !validHostname(host) {
				return errors.New("serving: invalid generated hostname")
			}
			if _, exists := next.routes[host]; exists {
				return errors.New("serving: duplicate hostname")
			}
			next.routes[host] = result
			return nil
		}
		if err := add(deploy.Host(h.config.BaseDomain, h.config.PreviewStyle, site.Name, ""), site.Production, "", true); err != nil {
			return err
		}
		for _, preview := range bySite[site.Name] {
			if err := add(preview.Host, preview.Alias.Release, preview.Alias.Revision, false); err != nil {
				return err
			}
		}
	}
	// Publish caches only with a complete routing snapshot. Partial inventory or
	// release failures must not acknowledge a fingerprint or prune old entries.
	h.sites = nextSites
	h.releases = nextReleases
	h.current.Store(next)
	return nil
}

// Run watches committed publications and periodically reconciles missed events.
// Subscribe before the startup refresh so a publication during recovery leaves
// another wakeup pending. Failures retain the last snapshot and retry promptly.
func (h *Handler) Run(ctx context.Context) error {
	return h.run(ctx, time.Second)
}

func (h *Handler) run(ctx context.Context, retryInitial time.Duration) error {
	changes, unsubscribe := h.catalog.WatchPublications()
	defer unsubscribe()
	ticker := time.NewTicker(h.config.RefreshInterval)
	defer ticker.Stop()
	var retry *time.Timer
	var retryC <-chan time.Time
	delay := retryInitial
	retries := 0
	stopRetry := func() {
		if retry != nil {
			retry.Stop()
			retry = nil
		}
		retryC = nil
	}
	defer stopRetry()
	resetRetry := func() {
		stopRetry()
		delay, retries = retryInitial, 0
	}
	refresh := func() error {
		err := h.Refresh(ctx)
		if err == nil {
			resetRetry()
			return nil
		}
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "refresh serving metadata", slog.Any("err", err))
			// A bad manifest cannot recover by polling faster forever. Try three
			// short retries, then wait for another publication or reconciliation.
			if retry == nil && retries < 3 {
				retry = time.NewTimer(delay)
				retryC = retry.C
				delay *= 2
				retries++
			}
		}
		return err
	}
	if err := refresh(); err != nil && h.current.Load() == nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changes:
			resetRetry()
			refresh()
		case <-ticker.C:
			resetRetry()
			refresh()
		case <-retryC:
			retry, retryC = nil, nil
			refresh()
		}
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host, ok := requestHost(r.Host)
	if !ok {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	s := h.current.Load()
	if s == nil {
		http.Error(w, "publication metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	route, ok := s.routes[host]
	if !ok {
		http.NotFound(w, r)
		return
	}
	p, ok := requestPath(r)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	decision := route.engine.Resolve("/"+p, isNavigation(r, p))
	if decision.Status == http.StatusBadRequest {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	rel := route.release
	if (decision.Action == routes.ActionNotFound || decision.Action == routes.ActionFallback) && p != "" {
		if old, file, ok := h.previousAsset(route, p); ok {
			rel = old
			decision.Path = file.Path
			decision.Status = http.StatusOK
			decision.Action = routes.ActionFile
		}
	}
	if decision.Redirect != "" {
		target, e := url.Parse(decision.Redirect)
		if e != nil {
			http.Error(w, "invalid redirect", http.StatusInternalServerError)
			return
		}
		if target.RawQuery == "" {
			target.RawQuery = r.URL.RawQuery
		}
		applyHeaders(w, decision.Headers)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.Redirect(w, r, target.String(), decision.Status)
		return
	}
	file, exists := rel.Files[decision.Path]
	if !exists || decision.Path == "" {
		h.notFound(w, r, decision.Headers)
		return
	}
	key := deploy.FileKey(rel, decision.Path)
	info, err := h.catalog.Store.Stat(r.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			h.notFound(w, r, decision.Headers)
		} else {
			http.Error(w, "object unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	if info.Size != file.Size {
		http.Error(w, "object metadata mismatch", http.StatusServiceUnavailable)
		return
	}
	contentType := file.ContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(decision.Path))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	if decision.Status == http.StatusOK && len(file.SHA256) == 64 {
		w.Header().Set("ETag", `"`+file.SHA256+`"`)
	}
	applyHeaders(w, decision.Headers)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	reader := &objectReader{ctx: r.Context(), store: h.catalog.Store, key: key, size: file.Size}
	defer reader.Close()
	if decision.Status == http.StatusNotFound {
		// Error documents retain their error status and do not turn into conditional
		// 304 or Range responses merely because they have a static file body.
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			if _, err := io.CopyN(w, reader, file.Size); err != nil {
				slog.ErrorContext(r.Context(), "serve error document", slog.Any("err", err))
			}
		}
		return
	}
	http.ServeContent(w, r, path.Base(decision.Path), rel.CreatedAt, reader)
}

func applyHeaders(w http.ResponseWriter, headers map[string]string) {
	for name, value := range headers {
		w.Header().Set(name, value)
	}
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request, headers map[string]string) {
	const body = "404 page not found\n"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	applyHeaders(w, headers)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		io.WriteString(w, body)
	}
}

func (h *Handler) previousAsset(current route, p string) (*deploy.Release, deploy.File, bool) {
	now := h.now()
	for _, old := range current.assets {
		if !now.Before(old.expires) || !old.engine.Immutable(p) {
			continue
		}
		file, ok := old.release.Files[p]
		if !ok || htmlAsset(file) || p == "pages.json" {
			continue
		}
		return old.release, file, true
	}
	return nil, deploy.File{}, false
}

func htmlAsset(file deploy.File) bool {
	media, _, _ := mime.ParseMediaType(file.ContentType)
	ext := strings.ToLower(path.Ext(file.Path))
	return strings.EqualFold(media, "text/html") || strings.EqualFold(media, "application/xhtml+xml") || ext == ".html" || ext == ".htm" || ext == ".xhtml"
}

func isNavigation(r *http.Request, p string) bool {
	destination := r.Header.Get("Sec-Fetch-Dest")
	if destination != "" && destination != "document" && destination != "iframe" && destination != "empty" {
		return false
	}
	accept := r.Header.Get("Accept")
	simplePath := path.Ext(p) == "" || strings.EqualFold(path.Ext(p), ".html") || strings.EqualFold(path.Ext(p), ".htm")
	if accept == "" {
		return simplePath
	}
	for _, item := range strings.Split(accept, ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		if q, ok := params["q"]; ok {
			value, err := strconv.ParseFloat(q, 64)
			if err != nil || value <= 0 {
				continue
			}
		}
		if strings.EqualFold(media, "text/html") || strings.EqualFold(media, "application/xhtml+xml") {
			return true
		}
		if (media == "*/*" || strings.EqualFold(media, "text/*")) && simplePath {
			return true
		}
	}
	return false
}

func validHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if !deploy.ValidName(part) {
			return false
		}
	}
	return true
}

func requestHost(value string) (string, bool) {
	if strings.Contains(value, ":") {
		host, port, err := net.SplitHostPort(value)
		if err != nil {
			return "", false
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 0 || p > 65535 {
			return "", false
		}
		value = host
	}
	value = strings.TrimSuffix(strings.ToLower(value), ".")
	return value, validHostname(value)
}

func requestPath(r *http.Request) (string, bool) {
	p := r.URL.Path
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n%") {
		return "", false
	}
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, part := range parts {
		if part == "." || part == ".." || (part == "" && i < len(parts)-1) {
			return "", false
		}
	}
	return strings.TrimPrefix(p, "/"), true
}

// objectReader lets net/http implement conditional, single and multipart Range
// responses while each seek is backed by a streaming object-store range read.
type objectReader struct {
	ctx       context.Context
	store     storage.Store
	key       string
	size, pos int64
	body      io.ReadCloser
}

func (r *objectReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		var err error
		r.body, _, err = r.store.GetRange(r.ctx, r.key, r.pos, r.size-r.pos)
		if err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > r.size-r.pos {
		p = p[:r.size-r.pos]
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	return n, err
}

func (r *objectReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return 0, errors.New("invalid seek")
	}
	if next < 0 {
		return 0, errors.New("negative seek")
	}
	if next != r.pos {
		_ = r.Close()
		r.pos = next
	}
	return r.pos, nil
}

func (r *objectReader) Close() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}
