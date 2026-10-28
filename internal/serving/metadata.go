package serving

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/metacache"
	"github.com/lesomnus/page/internal/routes"
)

type compiledRelease struct {
	release *deploy.ServingRelease
	engine  *routes.Engine
}

func (h *Handler) invalidateMetadata() (uint64, uint64) {
	h.cacheRevisionMu.Lock()
	defer h.cacheRevisionMu.Unlock()
	revision := h.catalog.PublicationRevision()
	h.metadata.InvalidateMutable()
	h.cacheRevision = revision
	return revision, h.metadata.Generation()
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
	})
	if err != nil {
		return nil, err
	}
	return value.(*deploy.Site), nil
}

func (h *Handler) cachedRelease(ctx context.Context, site *deploy.Site, id string) (*compiledRelease, error) {
	h.observePublications()
	// A retired release can be collected and later published again with the
	// same source identity and ID but a new CreatedAt. Scope immutable cached
	// metadata to the publication that selected it, so both memory and disk
	// tiers load the new incarnation after a later Site write.
	key := deploy.ReleasePrefix(site.Owner, site.Name, id) + "serving.json@" + site.UpdatedAt.UTC().Format(time.RFC3339Nano)
	value, err := h.metadata.Get(ctx, key, metacache.Policy{
		Mutable: false,
		Load: func(ctx context.Context) ([]byte, string, error) {
			release, err := h.catalog.GetServingRelease(ctx, site.Owner, site.Name, id)
			if err != nil {
				return nil, "", err
			}
			data, err := json.Marshal(release)
			return data, "", err
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
