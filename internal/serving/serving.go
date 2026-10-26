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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

type Config struct {
	BaseDomain      string        `yaml:"base_domain"`
	PreviewStyle    string        `yaml:"preview_style"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

type route struct{ release *deploy.Release }
type snapshot struct{ routes map[string]route }

type Handler struct {
	catalog *deploy.Catalog
	config  Config
	refresh sync.Mutex
	current atomic.Pointer[snapshot]
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
	if config.RefreshInterval == 0 {
		config.RefreshInterval = 5 * time.Second
	}
	if config.RefreshInterval < 0 {
		return nil, errors.New("serving: refresh_interval must be positive")
	}
	return &Handler{catalog: catalog, config: config}, nil
}

// Refresh replaces the complete routing snapshot only after every referenced
// release has been read successfully. An unavailable store leaves readers alone.
func (h *Handler) Refresh(ctx context.Context) error {
	h.refresh.Lock()
	defer h.refresh.Unlock()
	sites, err := h.catalog.ListSites(ctx)
	if err != nil {
		return fmt.Errorf("serving: list sites: %w", err)
	}
	next := &snapshot{routes: make(map[string]route)}
	for _, site := range sites {
		if !deploy.ValidName(site.Name) {
			return errors.New("serving: invalid site name")
		}
		loaded := make(map[string]*deploy.Release)
		add := func(token, id, revision string) error {
			if id == "" {
				return nil
			}
			if token != "" && !deploy.ValidName(token) {
				return errors.New("serving: invalid preview token")
			}
			r := loaded[id]
			if r == nil {
				var e error
				r, e = h.catalog.GetRelease(ctx, site.Owner, site.Name, id)
				if e != nil {
					return fmt.Errorf("serving: load release %s: %w", id, e)
				}
				if !r.Complete || r.Site != site.Name || r.Owner != site.Owner || r.Origin.RepositoryID != site.RepositoryID {
					return errors.New("serving: invalid release ownership or completion")
				}
				loaded[id] = r
			}
			if revision != "" && revision != r.Origin.Revision {
				return errors.New("serving: preview revision does not match release")
			}
			host := deploy.Host(h.config.BaseDomain, h.config.PreviewStyle, site.Name, token)
			if !validHostname(host) {
				return errors.New("serving: invalid generated hostname")
			}
			if _, exists := next.routes[host]; exists {
				return errors.New("serving: duplicate hostname")
			}
			next.routes[host] = route{release: r}
			return nil
		}
		if err := add("", site.Production, ""); err != nil {
			return err
		}
		for token, alias := range site.Aliases {
			if err := add(token, alias.Release, alias.Revision); err != nil {
				return err
			}
		}
	}
	h.current.Store(next)
	return nil
}

// Run performs startup recovery and polls publication metadata. Once started,
// refresh failures are reported while the last complete routing snapshot stays.
func (h *Handler) Run(ctx context.Context) error {
	if h.current.Load() == nil {
		if err := h.Refresh(ctx); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(h.config.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := h.Refresh(ctx); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "refresh serving metadata", slog.Any("err", err))
			}
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
	rel := route.release
	file, ok := rel.Files[p]
	if !ok {
		index := strings.TrimSuffix(p, "/")
		if index != "" {
			index += "/"
		}
		index += "index.html"
		file, ok = rel.Files[index]
		if ok {
			if !strings.HasSuffix(r.URL.Path, "/") {
				target := url.URL{Path: r.URL.Path + "/", RawQuery: r.URL.RawQuery}
				http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
				return
			}
			p = index
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	key := deploy.FileKey(rel, p)
	info, err := h.catalog.Store.Stat(r.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
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
		contentType = mime.TypeByExtension(path.Ext(p))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	if len(file.SHA256) == 64 {
		w.Header().Set("ETag", `"`+file.SHA256+`"`)
	}
	reader := &objectReader{ctx: r.Context(), store: h.catalog.Store, key: key, size: file.Size}
	defer reader.Close()
	http.ServeContent(w, r, path.Base(p), rel.CreatedAt, reader)
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
