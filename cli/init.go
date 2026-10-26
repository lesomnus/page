package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/trail"

	app "github.com/lesomnus/page"
	"github.com/lesomnus/page/cmd"
)

// NewCmdInit is `page init`: the first tenant, and somebody in it.
//
// It exists because there is nowhere else it could happen. A tenant is not put
// up from inside one, so the first row of a deployment cannot arrive over the
// API. What puts it there is [Server.Ungated], which is not a privilege anybody
// holds: it is a server instance this process was handed, reachable from this
// command and from nowhere a request can get to.
//
// Running it twice is an error rather than a no-op, because an alias is unique
// and the database says so. That is the right answer -- an `init` that quietly
// did nothing is one somebody runs against the wrong deployment and believes.
func NewCmdInit(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "init",
		Brief: "put up the first tenant and somebody in it",

		Flags: flg.Flags{
			&flg.String{Name: "tenant", Brief: "the alias of the tenant to create"},
			&flg.String{Name: "holder", Brief: "the alias of the holder to create in it"},
		},

		Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
			tenant, ok := flg.Find[string](self, "tenant")
			if !ok || tenant == "" {
				tenant = "acme"
			}

			holder, ok := flg.Find[string](self, "holder")
			if !ok || holder == "" {
				holder = "admin"
			}

			// An archive is kept with a key that signs its account, and a
			// process is refused one without it. A development deployment
			// that names an archive and no key is given one of its own --
			// there is no key everybody knows, which would sign forgeries for
			// anybody -- written beside the archive, and told where.
			if c.Audit.Archive != "" && c.Audit.Checkpoints.Key == "" {
				path, err := keyFor(c.Audit.Archive)
				if err != nil {
					return err
				}

				c.Audit.Checkpoints.Key = path
				self.Printf("the trail's archive is signed with a key of this deployment's own, written to %s\n", path)
				self.Printf("name it as audit.checkpoints.key, or PAGE_AUDIT_CHECKPOINTS_KEY, before serving\n\n")
			}

			s, err := cmd.Build(ctx, *c)
			if err != nil {
				return err
			}
			defer s.Close()

			// The schema, so that a fresh database is one this can run against.
			// A deployment with migrations of its own does that instead; see
			// payday's `migrate`.
			if err := Migrate(ctx, s); err != nil {
				return err
			}

			t, err := s.Ungated.Tenant().Add(ctx, app.TenantAddRequest_builder{
				Alias: tenant,
			}.Build())
			if err != nil {
				return fmt.Errorf("tenant %q: %w", tenant, err)
			}

			h, err := s.Ungated.Holder().Add(ctx, app.HolderAddRequest_builder{
				Tenant: app.TenantRef_builder{Id: t.GetId()}.Build(),
				Alias:  holder,
			}.Build())
			if err != nil {
				return fmt.Errorf("holder %q: %w", holder, err)
			}

			k, err := pdid.From(t.GetId())
			if err != nil {
				return err
			}
			j, err := pdid.From(h.GetId())
			if err != nil {
				return err
			}

			self.Printf("tenant %s is %s\n", tenant, k)
			self.Printf("holder %s is %s\n", holder, j)
			self.Printf("\nsign in as: @%s/%s\n", tenant, holder)

			return nil
		}),
	}
}

// keyFor writes a key for the trail's archive to sign its account with, made
// fresh, beside the archive, and answers the file it is in. A file already
// there is somebody's key, and is not written over.
func keyFor(archive string) (string, error) {
	doc, _, err := trail.GenerateKey()
	if err != nil {
		return "", err
	}

	path := filepath.Clean(archive) + "-checkpoints.pem"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("a key for the trail: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("a key for the trail: %s is one already; name it as audit.checkpoints.key, or move it away for a new one", path)
	}
	if err != nil {
		return "", fmt.Errorf("a key for the trail: %w", err)
	}
	if _, err := f.Write(doc); err != nil {
		f.Close()
		return "", fmt.Errorf("a key for the trail: %w", err)
	}

	return path, f.Close()
}
