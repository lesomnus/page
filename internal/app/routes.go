package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/routes"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
)

func localRoutes(dir string) (*routes.Engine, int, error) {
	info, e := os.Lstat(dir)
	if e != nil {
		return nil, 0, e
	}
	if !info.IsDir() {
		return nil, 0, errors.New("publish root must be a directory")
	}
	files := map[string]deploy.File{}
	var config *routes.Config
	e = filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("publish path is not a regular file: %s", p)
		}
		if len(files) >= 10000 {
			return errors.New("too many publish files")
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		info, e := d.Info()
		if e != nil {
			return e
		}
		typ := mime.TypeByExtension(filepath.Ext(p))
		if typ == "" {
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			var buf [512]byte
			n, _ := f.Read(buf[:])
			f.Close()
			typ = http.DetectContentType(buf[:n])
		}
		files[rel] = deploy.File{Path: rel, Size: info.Size(), ContentType: typ}
		if rel == "pages.json" {
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			b, e := io.ReadAll(io.LimitReader(f, routes.MaxConfigBytes+1))
			f.Close()
			if e != nil {
				return e
			}
			config, e = routes.Parse(b)
			if e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return nil, 0, e
	}
	engine, e := deploy.CompilePages(config, files)
	return engine, len(files), e
}

func runRoutes(args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: page routes validate DIR | page routes test DIR PATH")
	}
	f := flag.NewFlagSet("routes "+args[0], flag.ContinueOnError)
	f.SetOutput(errOut)
	nav := f.Bool("navigation", true, "simulate an HTML navigation request")
	if e := f.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	pos := f.Args()
	needed := 1
	if args[0] == "test" {
		needed = 2
	} else if args[0] != "validate" {
		return errors.New("unknown routes command")
	}
	if len(pos) != needed {
		return errors.New("usage: page routes validate DIR | page routes test DIR PATH")
	}
	engine, count, e := localRoutes(pos[0])
	if e != nil {
		return e
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if args[0] == "validate" {
		return enc.Encode(map[string]any{"valid": true, "files": count})
	}
	return enc.Encode(engine.Resolve(pos[1], *nav))
}
