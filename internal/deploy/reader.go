package deploy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lesomnus/page/internal/storage"
)

// GetSiteInfo returns independently decoded publication state and the metadata
// from the same GET response. A reader must not associate this body with an
// earlier LIST's ETag: a writer may have replaced the object between requests.
// No mutable Site pointer is shared with a publisher or another caller.
func (c *Catalog) GetSiteInfo(ctx context.Context, name string) (*Site, storage.Info, error) {
	if !ValidName(name) {
		return nil, storage.Info{}, ErrInvalid
	}
	var s Site
	info, err := c.readInfo(ctx, SiteKey(name), &s)
	if err != nil {
		return nil, storage.Info{}, err
	}
	if err := validateSite(&s); err != nil {
		return nil, storage.Info{}, err
	}
	if s.Name != name {
		return nil, storage.Info{}, ErrInvalid
	}
	if s.Aliases == nil {
		s.Aliases = map[string]Alias{}
	}
	if s.Retired == nil {
		s.Retired = map[string]time.Time{}
	}
	return &s, info, nil
}

// ListSiteObjects reads the complete inventory, retaining object fingerprints
// so serving can reuse unchanged publication state without GET or HEAD calls.
// Control intent objects in the same prefix are not public routing state.
func (c *Catalog) ListSiteObjects(ctx context.Context) ([]storage.Info, error) {
	objects, err := c.list(ctx, "sites/")
	if err != nil {
		return nil, err
	}
	out := make([]storage.Info, 0, len(objects))
	seen := map[string]bool{}
	for _, info := range objects {
		rest, ok := strings.CutPrefix(info.Key, "sites/")
		if !ok {
			continue
		}
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[1] != "site.json" {
			continue
		}
		if !ValidName(parts[0]) || seen[info.Key] {
			return nil, fmt.Errorf("%w: listed site identity", ErrInvalid)
		}
		seen[info.Key] = true
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
