package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"

	"github.com/lesomnus/page/internal/routes"
)

func releaseEngine(r *Release) (*routes.Engine, error) { return CompilePages(r.Pages, r.Files) }

// CompilePages is shared by local route simulation and publication validation.
func CompilePages(config *routes.Config, inventory map[string]File) (*routes.Engine, error) {
	files := make(map[string]bool, len(inventory))
	for p := range inventory {
		files[p] = true
	}
	e, err := routes.Compile(config, files)
	if err != nil {
		return nil, fmt.Errorf("%w: pages.json: %v", ErrInvalid, err)
	}
	for p, f := range inventory {
		if e.Immutable(p) {
			typ, _, _ := mime.ParseMediaType(f.ContentType)
			if typ == "text/html" || typ == "application/xhtml+xml" {
				return nil, fmt.Errorf("%w: HTML cannot be an immutable fallback asset", ErrInvalid)
			}
		}
	}
	return e, nil
}

// loadPages also supports manifests created before pages.json was interpreted.
// The original immutable file digest is verified before parsing legacy metadata.
func (c *Catalog) loadPages(ctx context.Context, r *Release) error {
	if f, ok := r.Files["pages.json"]; ok && r.Pages == nil {
		if f.Size > 65536 {
			return fmt.Errorf("%w: pages.json exceeds 64 KiB", ErrInvalid)
		}
		body, _, e := c.Store.Get(ctx, FileKey(r, "pages.json"))
		if e != nil {
			return e
		}
		b, e := io.ReadAll(io.LimitReader(body, 65537))
		body.Close()
		if e != nil {
			return e
		}
		h := sha256.Sum256(b)
		if int64(len(b)) != f.Size || hex.EncodeToString(h[:]) != f.SHA256 {
			return fmt.Errorf("%w: pages.json digest mismatch", ErrInvalid)
		}
		r.Pages, e = routes.Parse(b)
		if e != nil {
			return fmt.Errorf("%w: pages.json: %v", ErrInvalid, e)
		}
	}
	_, e := releaseEngine(r)
	return e
}

func immutableCompatible(old, next *Release) error {
	e, err := releaseEngine(old)
	if err != nil {
		return err
	}
	nextEngine, err := releaseEngine(next)
	if err != nil {
		return err
	}
	for p, f := range old.Files {
		if !e.Immutable(p) {
			continue
		}
		d := nextEngine.Resolve("/"+p, false)
		if d.Redirect != "" {
			return fmt.Errorf("%w: immutable asset URL redirected: /%s", ErrInvalid, p)
		}
		if d.Action == routes.ActionRewrite && d.Status != 200 {
			return fmt.Errorf("%w: immutable asset URL shadowed by a missing rewrite: /%s", ErrInvalid, p)
		}
		if n, ok := next.Files[d.Path]; d.Status == 200 && ok {
			oldType, _, _ := mime.ParseMediaType(f.ContentType)
			newType, _, _ := mime.ParseMediaType(n.ContentType)
			if n.SHA256 != f.SHA256 || oldType != newType {
				return fmt.Errorf("%w: immutable asset URL reused with different content or media type: /%s", ErrInvalid, p)
			}
		}
	}
	return nil
}
