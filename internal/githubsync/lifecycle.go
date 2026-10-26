package githubsync

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/lesomnus/page/internal/deploy"
)

func (s *Sync) reconcileControls(ctx context.Context) error {
	sites, err := s.cat.ListSites(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, site := range sites {
		if site.Owner != s.cfg.ID {
			continue
		}
		control, err := s.cat.GetControl(ctx, site)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if control.Mode == "disabled" {
			if err := s.persistAndCollect(ctx, site); err != nil {
				errs = append(errs, err)
			}
		} else if !control.UpdatedAt.Equal(site.ControlAppliedAt) {
			// Do not garbage-collect or recompute source policy here. A
			// pending explicit pause/pin/resume can be applied while GitHub
			// is unavailable; a failed activation leaves the prior snapshot.
			site.AssetRetention = s.cfg.AssetRetention
			if err := s.cat.ApplyControl(ctx, site); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := s.cat.SaveSite(ctx, site); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// persistAndCollect is the source's lifecycle policy. The catalog applies the
// latest durable operator intent and records production asset leases before
// the source computes its retained set. Those leases are asset-only reader
// references, not public preview aliases.
func (s *Sync) persistAndCollect(ctx context.Context, site *deploy.Site) error {
	site.AssetRetention = s.cfg.AssetRetention
	if err := s.cat.ApplyControl(ctx, site); err != nil {
		return err
	}
	if err := s.cat.SaveSite(ctx, site); err != nil {
		return err
	}
	releases, err := s.cat.ListReleases(ctx, site.Owner, site.Name)
	if err != nil {
		return err
	}
	now := s.now()
	keep := retained(site, now)
	if site.Retired == nil {
		site.Retired = map[string]time.Time{}
	}
	for _, release := range releases {
		if keep[release.ID] {
			delete(site.Retired, release.ID)
		} else if _, exists := site.Retired[release.ID]; !exists {
			site.Retired[release.ID] = now
		}
	}
	if err := s.cat.SaveSite(ctx, site); err != nil {
		return err
	}
	// SaveSite may have observed an operator change made during the scan.
	keep = retained(site, now)
	var errs []error
	for id, retired := range site.Retired {
		if keep[id] || now.Sub(retired) < s.cfg.GCGrace {
			continue
		}
		if err := s.cat.DeleteRelease(ctx, site, id); err != nil {
			if errors.Is(err, deploy.ErrReferenced) {
				// A newly persisted pin is authoritative even before this
				// source's next pass has materialized it into public refs.
				continue
			}
			errs = append(errs, err)
			continue
		}
		delete(site.Retired, id)
		slog.InfoContext(ctx, "release collected", "source", s.cfg.ID, "site", site.Name, "release", id)
	}
	if err := s.cat.SaveSite(ctx, site); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func retained(site *deploy.Site, now time.Time) map[string]bool {
	keep := map[string]bool{}
	if site.Production != "" {
		keep[site.Production] = true
	}
	for _, alias := range site.Aliases {
		keep[alias.Release] = true
	}
	for id, until := range site.AssetReleases {
		if now.Before(until) {
			keep[id] = true
		}
	}
	return keep
}
