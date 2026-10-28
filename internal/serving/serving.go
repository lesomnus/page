// Package serving reads published snapshots without consulting their sources.
package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/metacache"
	"github.com/lesomnus/page/internal/routes"
	"github.com/lesomnus/page/internal/storage"
)

type Config struct {
	BaseDomain       string           `yaml:"base_domain"`
	PreviewStyle     string           `yaml:"preview_style"`
	PreviewAliasMode string           `yaml:"preview_alias_mode"`
	RefreshInterval  time.Duration    `yaml:"refresh_interval"`
	MetadataCache    metacache.Config `yaml:"metadata_cache"`
}

// hostBinding keeps only namespace ownership. Publication and release metadata
// use a separate bounded cache; file inventories and bodies are not retained.
type hostBinding struct {
	site, owner, repository string
}
type snapshot struct{ routes map[string]hostBinding }

type Handler struct {
	catalog         *deploy.Catalog
	config          Config
	refresh         sync.Mutex
	current         atomic.Pointer[snapshot]
	metadata        *metacache.Cache
	cacheRevisionMu sync.Mutex
	cacheRevision   uint64
	now             func() time.Time
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
	metadata, err := metacache.New(config.MetadataCache)
	if err != nil {
		return nil, fmt.Errorf("serving: metadata cache: %w", err)
	}
	return &Handler{catalog: catalog, config: config, metadata: metadata, cacheRevision: catalog.PublicationRevision(), now: time.Now}, nil
}

// Close stops local cache maintenance after the HTTP server has stopped.
func (h *Handler) Close() { h.metadata.Close() }

// Refresh reconciles hostname ownership without loading release inventories.
// Explicit refreshes invalidate mutable cached publications even if listing
// fails. A failed refresh retains the namespace index, never freshens old data.
var errPublicationChanged = errors.New("serving: publication changed during namespace refresh")

func (h *Handler) Refresh(ctx context.Context) error {
	h.refresh.Lock()
	defer h.refresh.Unlock()
	for range 3 {
		err := h.refreshNamespace(ctx)
		if !errors.Is(err, errPublicationChanged) {
			return err
		}
	}
	return errPublicationChanged
}

func (h *Handler) refreshNamespace(ctx context.Context) error {
	revision, generation := h.invalidateMetadata()
	sites, err := h.catalog.ListSites(ctx)
	if err != nil {
		return fmt.Errorf("serving: list sites: %w", err)
	}
	previews, err := deploy.PlanPreviews(sites, h.config.BaseDomain, h.config.PreviewStyle, h.config.PreviewAliasMode)
	if err != nil {
		return fmt.Errorf("serving: plan previews: %w", err)
	}
	next := &snapshot{routes: make(map[string]hostBinding)}
	bindings := make(map[string]hostBinding, len(sites))
	add := func(host string, binding hostBinding) error {
		if !validHostname(host) {
			return errors.New("serving: invalid generated hostname")
		}
		if _, exists := next.routes[host]; exists {
			return errors.New("serving: duplicate hostname")
		}
		next.routes[host] = binding
		return nil
	}
	for _, site := range sites {
		binding := hostBinding{site.Name, site.Owner, site.RepositoryID}
		bindings[site.Name] = binding
		if !site.Disabled && site.Production != "" {
			if err := add(deploy.Host(h.config.BaseDomain, h.config.PreviewStyle, site.Name, ""), binding); err != nil {
				return err
			}
		}
	}
	for _, preview := range previews.Routes {
		if preview.Alias.Release == "" {
			continue
		}
		if err := add(preview.Host, bindings[preview.Site]); err != nil {
			return err
		}
	}
	// The periodic source read supplies the disk tier. It does not count as
	// an HTTP access or keep idle decoded values resident in memory.
	if h.catalog.PublicationRevision() != revision || h.metadata.Generation() != generation {
		return errPublicationChanged
	}
	for _, site := range sites {
		data, err := json.Marshal(site)
		if err != nil {
			return fmt.Errorf("serving: encode site cache: %w", err)
		}
		h.metadata.SeedDisk(deploy.SiteKey(site.Name), metacache.Policy{Mutable: true}, data, "", generation)
	}
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
	binding, ok := s.routes[host]
	if !ok {
		http.NotFound(w, r)
		return
	}
	p, ok := requestPath(r)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	site, err := h.cachedSite(r.Context(), binding.site)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "publication metadata unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	if site.Disabled || site.Owner != binding.owner || site.RepositoryID != binding.repository {
		http.NotFound(w, r)
		return
	}
	id, revision := site.Production, ""
	production := host == deploy.Host(h.config.BaseDomain, h.config.PreviewStyle, site.Name, "")
	if !production {
		id = ""
		candidates, err := h.previewSites(r.Context(), host, site)
		if err != nil {
			http.Error(w, "publication metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		previews, err := deploy.PlanPreviews(candidates, h.config.BaseDomain, h.config.PreviewStyle, h.config.PreviewAliasMode)
		if err != nil {
			http.Error(w, "publication metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		for _, preview := range previews.Routes {
			if preview.Host == host && preview.Site == binding.site {
				id, revision = preview.Alias.Release, preview.Alias.Revision
				break
			}
		}
	}
	if id == "" {
		http.NotFound(w, r)
		return
	}
	compiled, err := h.cachedRelease(r.Context(), site, id)
	if err != nil || (revision != "" && revision != compiled.release.Origin.Revision) {
		http.Error(w, "release metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	rel, engine := compiled.release, compiled.engine
	// Only the handful of candidate paths considered by this request live here.
	files := map[string]*deploy.ServingFile{}
	lookup := func(p string) (bool, error) {
		if file, ok := files[p]; ok {
			return file != nil, nil
		}
		file, err := h.catalog.GetServingFile(r.Context(), rel, p)
		if errors.Is(err, storage.ErrNotFound) {
			files[p] = nil
			return false, nil
		}
		if err != nil {
			return false, err
		}
		files[p] = file
		return true, nil
	}
	decision, err := engine.ResolveLookup("/"+p, isNavigation(r, p), lookup)
	if err != nil {
		http.Error(w, "file metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	if decision.Status == http.StatusBadRequest {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	selected := files[decision.Path]
	if production && (decision.Action == routes.ActionNotFound || decision.Action == routes.ActionFallback) && p != "" {
		old, file, err := h.previousAsset(r.Context(), site, p)
		if err != nil {
			http.Error(w, "asset metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		if file != nil {
			rel, selected = old, file
			decision.Path = file.File.Path
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
	if selected == nil || decision.Path == "" {
		h.notFound(w, r, decision.Headers)
		return
	}
	file := selected.File
	key := deploy.ServingObjectKey(rel, decision.Path)
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

// Flat hostnames can be shared by different site/ref splits: app--bar--foo
// could be token app--bar on foo, token app on bar--foo, or a production name.
// Derive those bounded candidates from the DNS label and read their current
// publication state, including empty/disabled sites that reserve their names.
// Nested hostnames separate the site into its own label and cannot overlap.
func (h *Handler) previewSites(ctx context.Context, host string, current *deploy.Site) ([]*deploy.Site, error) {
	sites := []*deploy.Site{current}
	if h.config.PreviewStyle == "nested" {
		return sites, nil
	}
	label, ok := strings.CutSuffix(host, "."+h.config.BaseDomain)
	if !ok || strings.Contains(label, ".") || len(label) > 63 {
		return nil, errors.New("serving: invalid flat preview hostname")
	}
	load := func(name string) error {
		if name == current.Name || !deploy.ValidName(name) {
			return nil
		}
		site, err := h.cachedSite(ctx, name)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		sites = append(sites, site)
		return nil
	}
	if err := load(label); err != nil {
		return nil, err
	}
	for at := 0; at+1 < len(label); at++ {
		if label[at:at+2] == "--" {
			if err := load(label[at+2:]); err != nil {
				return nil, err
			}
		}
	}
	return sites, nil
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

func (h *Handler) servingRelease(ctx context.Context, site *deploy.Site, id string) (*deploy.ServingRelease, error) {
	compiled, err := h.cachedRelease(ctx, site, id)
	if err != nil {
		return nil, err
	}
	return compiled.release, nil
}

func (h *Handler) previousAsset(ctx context.Context, site *deploy.Site, p string) (*deploy.ServingRelease, *deploy.ServingFile, error) {
	if p == "pages.json" || htmlAsset(deploy.File{Path: p}) {
		return nil, nil, nil
	}
	now := h.now()
	var chosen *deploy.ServingRelease
	var selected *deploy.ServingFile
	for id, expiry := range site.AssetReleases {
		if !now.Before(expiry) || id == site.Production {
			continue
		}
		old, err := h.servingRelease(ctx, site, id)
		if err != nil {
			return nil, nil, err
		}
		if chosen != nil && (old.CreatedAt.Before(chosen.CreatedAt) || (old.CreatedAt.Equal(chosen.CreatedAt) && old.ID >= chosen.ID)) {
			continue
		}
		file, err := h.catalog.GetServingFile(ctx, old, p)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if !file.Immutable || htmlAsset(file.File) {
			continue
		}
		chosen, selected = old, file
	}
	return chosen, selected, nil
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
