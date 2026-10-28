package serving

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/metacache"
	"github.com/lesomnus/page/internal/routes"
)

// A projection is immutable under its owner/site/release identity. Legacy
// descriptors deliberately have no marker so a later source backfill is found
// after the short freshness interval or the next publication notification.
const projectionValidator = "page-serving-projection-v1"

type compiledRelease struct {
	release *deploy.ServingRelease
	engine  *routes.Engine
}

func (h *Handler) invalidateMetadata() {
	h.cacheRevisionMu.Lock()
	defer h.cacheRevisionMu.Unlock()
	revision := h.catalog.PublicationRevision()
	h.metadata.InvalidateMutable()
	h.cacheRevision = revision
}

// SaveSite increments PublicationRevision before returning. Checking it here
// makes a publication revoke an old cached Site even when Run has not consumed
// its coalesced notification yet, or its subsequent namespace LIST fails.
func (h *Handler) observePublications() {
	h.cacheRevisionMu.Lock()
	defer h.cacheRevisionMu.Unlock()
	if revision := h.catalog.PublicationRevision(); revision != h.cacheRevision {
		h.metadata.InvalidateMutable()
		h.cacheRevision = revision
	}
}

func (h *Handler) cachedSite(ctx context.Context, name string) (*deploy.Site, error) {
	h.observePublications()
	value, err := h.metadata.Get(ctx, deploy.SiteKey(name), metacache.Policy{
		Mutable: true,
		Load: func(ctx context.Context) ([]byte, string, error) {
			site, info, err := h.catalog.GetSiteInfo(ctx, name)
			if err != nil {
				return nil, "", err
			}
			data, err := json.Marshal(site)
			return data, info.ETag, err
		},
		Decode: func(data []byte) (any, error) {
			return deploy.DecodeSite(data, name)
		},
		Revalidate: func(ctx context.Context, etag string) (bool, error) {
			info, err := h.catalog.Store.Stat(ctx, deploy.SiteKey(name))
			return err == nil && etag != "" && info.ETag == etag, err
		},
	})
	if err != nil {
		return nil, err
	}
	return value.(*deploy.Site), nil
}

func (h *Handler) cachedRelease(ctx context.Context, site *deploy.Site, id string) (*compiledRelease, error) {
	h.observePublications()
	value, err := h.metadata.Get(ctx, deploy.ReleasePrefix(site.Owner, site.Name, id)+"serving.json", metacache.Policy{
		// Modern descriptors only need the local identity validator below.
		// Marking both formats mutable lets legacy descriptors discover a newly
		// backfilled projection without adding another cache lifetime mechanism.
		Mutable: true,
		Load: func(ctx context.Context) ([]byte, string, error) {
			release, err := h.catalog.GetServingRelease(ctx, site.Owner, site.Name, id)
			if err != nil {
				return nil, "", err
			}
			data, err := json.Marshal(release)
			validator := ""
			if release.FileMetadataVersion == 1 {
				validator = projectionValidator
			}
			return data, validator, err
		},
		Decode: func(data []byte) (any, error) {
			release, err := deploy.DecodeServingRelease(data, site.Owner, site.Name, id)
			if err != nil {
				return nil, err
			}
			engine, err := routes.CompileRuntime(release.Pages)
			if err != nil {
				return nil, err
			}
			return &compiledRelease{release: release, engine: engine}, nil
		},
		Revalidate: func(_ context.Context, etag string) (bool, error) {
			return etag == projectionValidator, nil
		},
	})
	if err != nil {
		return nil, err
	}
	compiled := value.(*compiledRelease)
	if compiled.release.Origin.RepositoryID != site.RepositoryID {
		return nil, errors.New("serving: release repository ownership mismatch")
	}
	return compiled, nil
}
