package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

func orderedFlags(args []string) ([]string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "--source" || a == "--wait":
			if i+1 == len(args) {
				return nil, fmt.Errorf("missing value for %s", a)
			}
			flags = append(flags, a, args[i+1])
			i++
		case strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "--source=") || strings.HasPrefix(a, "--wait=") || strings.HasPrefix(a, "--once=") || a == "--once" || a == "--help" || a == "-h":
			flags = append(flags, a)
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown option %s", a)
		default:
			pos = append(pos, a)
		}
	}
	return append(flags, pos...), nil
}

func controlCommand(command string) bool {
	switch command {
	case "releases", "promote", "rollback", "pin", "pause", "resume", "remove":
		return true
	}
	return false
}

type controlStatus struct {
	Site       string          `json:"site"`
	Production string          `json:"productionRelease"`
	Disabled   bool            `json:"disabled"`
	Desired    *deploy.Control `json:"desiredControl"`
	Applied    bool            `json:"controlApplied"`
	Problem    string          `json:"problem,omitempty"`
}

func getControlStatus(ctx context.Context, cat *deploy.Catalog, s *deploy.Site) (controlStatus, error) {
	c, e := cat.GetControl(ctx, s)
	if e != nil {
		return controlStatus{}, e
	}
	applied := s.ControlAppliedAt.Equal(c.UpdatedAt) && ((s.ControlMode == c.Mode) || (s.ControlMode == "" && c.Mode == "auto"))
	result := controlStatus{Site: s.Name, Production: s.Production, Disabled: s.Disabled, Desired: c, Applied: applied}
	if c.Target != "" {
		_, e := cat.GetRelease(ctx, s.Owner, s.Name, c.Target)
		if errors.Is(e, storage.ErrNotFound) {
			result.Problem = "requested release is no longer retained"
			result.Applied = false
		} else if e != nil {
			return result, e
		}
	}
	return result, nil
}

func runControl(ctx context.Context, cat *deploy.Catalog, command string, pos []string, owner string, wait time.Duration, out io.Writer) error {
	if len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("usage: page %s SITE [RELEASE]", command)
	}
	if (command == "promote" && len(pos) != 2) || ((command == "pause" || command == "resume" || command == "remove" || command == "releases") && len(pos) != 1) {
		return fmt.Errorf("invalid arguments for %s", command)
	}
	if wait < 0 {
		return errors.New("wait must not be negative")
	}
	s, e := cat.GetSite(ctx, pos[0])
	if e != nil {
		return e
	}
	if owner == "" {
		owner = s.Owner
	}
	if owner != s.Owner {
		return deploy.ErrOwnership
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if command == "releases" {
		rs, e := cat.ListReleases(ctx, s.Owner, s.Name)
		if e != nil {
			return e
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].CreatedAt.After(rs[j].CreatedAt) })
		type entry struct {
			ID         string    `json:"id"`
			Revision   string    `json:"revision"`
			CreatedAt  time.Time `json:"createdAt"`
			Complete   bool      `json:"complete"`
			Production bool      `json:"production"`
			AssetUntil time.Time `json:"assetUntil,omitempty"`
		}
		entries := []entry{}
		for _, r := range rs {
			entries = append(entries, entry{r.ID, r.Origin.Revision, r.CreatedAt, r.Complete, s.Production == r.ID, s.AssetReleases[r.ID]})
		}
		return enc.Encode(entries)
	}
	target := ""
	if len(pos) == 2 {
		target = pos[1]
	}
	mode := map[string]string{"promote": "pinned", "pin": "pinned", "pause": "paused", "resume": "auto", "remove": "disabled"}[command]
	if command == "rollback" {
		s, e = cat.RequestRollback(ctx, s.Name, owner, target)
	} else {
		s, e = cat.RequestControl(ctx, s.Name, owner, mode, target)
	}
	if e != nil {
		return e
	}
	state, e := getControlStatus(ctx, cat, s)
	if e != nil {
		return e
	}
	if wait > 0 && !state.Applied {
		waiting, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for !state.Applied {
			select {
			case <-waiting.Done():
				enc.Encode(state)
				return fmt.Errorf("control saved; waiting for owner sync: %w", waiting.Err())
			case <-ticker.C:
				s, e = cat.GetSite(waiting, s.Name)
				if e != nil {
					return e
				}
				state, e = getControlStatus(waiting, cat, s)
				if e != nil {
					return e
				}
				if state.Problem != "" {
					enc.Encode(state)
					return errors.New(state.Problem)
				}
			}
		}
	}
	return enc.Encode(state)
}
