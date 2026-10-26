package deploy

import (
	"context"
	"fmt"
	"time"
)

func ControlKey(name string) string { return "sites/" + name + "/control.json" }

func (c *Catalog) GetControl(ctx context.Context, s *Site) (*Control, error) {
	if !ValidName(s.Name) || !ValidName(s.Owner) || s.RepositoryID == "" {
		return nil, ErrInvalid
	}
	ctrl := &Control{SchemaVersion: SchemaVersion, Owner: s.Owner, RepositoryID: s.RepositoryID, Mode: "auto"}
	err := c.read(ctx, ControlKey(s.Name), ctrl)
	if errorsIsNotFound(err) {
		return ctrl, nil
	}
	if err != nil {
		return nil, err
	}
	if ctrl.Owner != s.Owner || ctrl.RepositoryID != s.RepositoryID {
		return nil, ErrOwnership
	}
	if ctrl.SchemaVersion != SchemaVersion {
		return nil, ErrInvalid
	}
	switch ctrl.Mode {
	case "auto", "disabled":
		if ctrl.Target != "" {
			return nil, ErrInvalid
		}
	case "pinned", "paused":
		if !ValidID(ctrl.Target) {
			return nil, ErrInvalid
		}
	default:
		return nil, ErrInvalid
	}
	return ctrl, nil
}

func (c *Catalog) apply(ctx context.Context, s *Site, ctrl *Control) error {
	s.ControlMode = ctrl.Mode
	s.Disabled = ctrl.Mode == "disabled"
	switch ctrl.Mode {
	case "auto":
	case "disabled":
		s.Production = ""
		s.Aliases = map[string]Alias{}
		s.AssetReleases = map[string]time.Time{}
	case "pinned", "paused":
		r, e := c.GetRelease(ctx, s.Owner, s.Name, ctrl.Target)
		if e != nil {
			return e
		}
		if r.Origin.RepositoryID != s.RepositoryID {
			return ErrOwnership
		}
		s.Production = ctrl.Target
	default:
		return ErrInvalid
	}
	return nil
}

func (c *Catalog) ApplyControl(ctx context.Context, s *Site) error {
	ctrl, e := c.GetControl(ctx, s)
	if e != nil {
		return e
	}
	return c.apply(ctx, s, ctrl)
}

// SetControl is a synchronous owner-local helper, sharing its GC mutex.
// External processes must use RequestControl so only the source publishes.
// A failed final site PUT leaves the intent for its owner sync to reconcile.
func (c *Catalog) SetControl(ctx context.Context, name, owner, mode, target string) (*Site, error) {
	return c.setControl(ctx, name, owner, mode, target, true)
}

// RequestControl is the external CLI path: only the owning source materializes
// its intent into publication metadata, serialized with that source's GC.
func (c *Catalog) RequestControl(ctx context.Context, name, owner, mode, target string) (*Site, error) {
	return c.setControl(ctx, name, owner, mode, target, false)
}

func (c *Catalog) setControl(ctx context.Context, name, owner, mode, target string, materialize bool) (*Site, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	s, e := c.GetSite(ctx, name)
	if e != nil {
		return nil, e
	}
	if s.Owner != owner {
		return nil, ErrOwnership
	}
	if mode != "auto" && mode != "disabled" && mode != "pinned" && mode != "paused" {
		return nil, ErrInvalid
	}
	if mode == "pinned" || mode == "paused" {
		if target == "" {
			target = s.Production
		}
		if !ValidID(target) {
			return nil, fmt.Errorf("%w: a complete release is required", ErrInvalid)
		}
	} else if target != "" {
		return nil, ErrInvalid
	}
	ctrl := &Control{SchemaVersion: SchemaVersion, Owner: s.Owner, RepositoryID: s.RepositoryID, Mode: mode, Target: target, UpdatedAt: time.Now().UTC()}
	if e = c.prepareSite(ctx, s, ctrl); e != nil {
		return nil, e
	}
	if e = c.write(ctx, ControlKey(name), ctrl); e != nil {
		return nil, e
	}
	if !materialize {
		return c.GetSite(ctx, name)
	}
	if e = c.write(ctx, SiteKey(name), s); e != nil {
		return nil, fmt.Errorf("control saved; publication awaits source reconciliation: %w", e)
	}
	return s, nil
}

func (c *Catalog) RequestRollback(ctx context.Context, name, owner, target string) (*Site, error) {
	return c.rollback(ctx, name, owner, target, false)
}
func (c *Catalog) Rollback(ctx context.Context, name, owner, target string) (*Site, error) {
	return c.rollback(ctx, name, owner, target, true)
}
func (c *Catalog) rollback(ctx context.Context, name, owner, target string, materialize bool) (*Site, error) {
	s, e := c.GetSite(ctx, name)
	if e != nil {
		return nil, e
	}
	if s.Owner != owner {
		return nil, ErrOwnership
	}
	if target == "" {
		for _, id := range s.History {
			if id == s.Production {
				continue
			}
			_, e = c.GetRelease(ctx, s.Owner, s.Name, id)
			if e == nil {
				target = id
				break
			}
			if !errorsIsNotFound(e) {
				return nil, e
			}
		}
	}
	if target == "" {
		return nil, fmt.Errorf("%w: no retained previous deployment", ErrInvalid)
	}
	return c.setControl(ctx, name, owner, "pinned", target, materialize)
}

// prepareSite merges durable lifecycle state with the writer's proposed view.
// Local writes share mutation; independent processes follow the one-owner rule.
func (c *Catalog) prepareSite(ctx context.Context, s *Site, override *Control) error {
	if e := validateSite(s); e != nil {
		return e
	}
	old, e := c.GetSite(ctx, s.Name)
	if e != nil && !errorsIsNotFound(e) {
		return e
	}
	if errorsIsNotFound(e) {
		old = nil
	}
	if old != nil && (old.Owner != s.Owner || old.RepositoryID != s.RepositoryID) {
		return ErrOwnership
	}
	ctrl := override
	if ctrl == nil {
		ctrl, e = c.GetControl(ctx, s)
		if e != nil {
			return e
		}
	}
	if e = c.apply(ctx, s, ctrl); e != nil {
		return e
	}
	s.ControlAppliedAt = ctrl.UpdatedAt
	now := time.Now().UTC()
	leases := map[string]time.Time{}
	if old != nil {
		s.CreatedAt = old.CreatedAt
		s.History = append([]string(nil), old.History...)
		for id, until := range old.AssetReleases {
			if until.After(now) {
				leases[id] = until
			}
		}
		if s.AssetRetention == 0 {
			s.AssetRetention = old.AssetRetention
		}
		if old.Production != "" && old.Production != s.Production {
			s.History = append([]string{old.Production}, s.History...)
			if !s.Disabled && s.AssetRetention > 0 {
				previous, e := c.GetRelease(ctx, old.Owner, old.Name, old.Production)
				if e != nil {
					return e
				}
				if previous.Pages != nil && len(previous.Pages.Assets.Immutable) > 0 {
					leases[old.Production] = now.Add(s.AssetRetention)
				}
			}
		}
	}
	if s.Disabled {
		leases = map[string]time.Time{}
	}
	delete(leases, s.Production)
	s.AssetReleases = leases
	seen := map[string]bool{}
	history := []string{}
	for _, id := range s.History {
		if id == s.Production || seen[id] {
			continue
		}
		seen[id] = true
		history = append(history, id)
		if len(history) == 32 {
			break
		}
	}
	s.History = history
	refs := map[string]bool{}
	if s.Production != "" {
		refs[s.Production] = true
	}
	for _, a := range s.Aliases {
		refs[a.Release] = true
	}
	for id := range leases {
		refs[id] = true
	}
	loaded := map[string]*Release{}
	for id := range refs {
		r, e := c.GetRelease(ctx, s.Owner, s.Name, id)
		if e != nil {
			return e
		}
		if r.Origin.RepositoryID != s.RepositoryID {
			return fmt.Errorf("%w: release repository mismatch", ErrInvalid)
		}
		loaded[id] = r
	}
	for _, a := range s.Aliases {
		if a.Revision != loaded[a.Release].Origin.Revision {
			return fmt.Errorf("%w: alias revision mismatch", ErrInvalid)
		}
	}
	if s.Production != "" && (old == nil || old.Production != s.Production) {
		protected := map[string]bool{}
		for id := range leases {
			protected[id] = true
		}
		if old != nil && old.Production != "" {
			protected[old.Production] = true
		}
		for id := range protected {
			if id == s.Production {
				continue
			}
			r := loaded[id]
			if r == nil {
				r, e = c.GetRelease(ctx, s.Owner, s.Name, id)
				if e != nil {
					return e
				}
			}
			if e = immutableCompatible(r, loaded[s.Production]); e != nil {
				return e
			}
		}
	}
	s.UpdatedAt = now
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	return validateSite(s)
}
