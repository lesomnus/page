// Package app wires the independent data plane and source-owned sync commands.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/lesomnus/page/internal/api"
	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/githubsync"
	"github.com/lesomnus/page/internal/serving"
	"github.com/lesomnus/page/internal/storage"
	"github.com/lesomnus/payday/version"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen          string              `yaml:"listen"`
	APIListen       string              `yaml:"api_listen"`
	BaseDomain      string              `yaml:"base_domain"`
	PreviewStyle    string              `yaml:"preview_style"`
	RefreshInterval time.Duration       `yaml:"refresh_interval"`
	Storage         storage.S3Config    `yaml:"storage"`
	GitHub          []githubsync.Config `yaml:"github"`
}

func Load(name string) (Config, error) {
	c := Config{Listen: ":8080", APIListen: "127.0.0.1:50051", BaseDomain: "example.com", PreviewStyle: "flat", RefreshInterval: 5 * time.Second}
	f, e := os.Open(name)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return c, errors.New("configuration must contain one YAML document")
	}
	seen := map[string]bool{}
	for i := range c.GitHub {
		g := &c.GitHub[i]
		if seen[g.ID] {
			return c, errors.New("duplicate GitHub source id")
		}
		seen[g.ID] = true
		if g.Token == "" {
			g.Token = os.Getenv("GITHUB_TOKEN")
		} else {
			g.Token = os.ExpandEnv(g.Token)
		}
	}
	return c, nil
}

func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	root := flag.NewFlagSet("page", flag.ContinueOnError)
	root.SetOutput(errOut)
	configPath := root.String("config", "page.yaml", "configuration file")
	root.Usage = func() {
		fmt.Fprintln(out, "Usage: page [--config FILE] <serve|sync|run|status|config|version|routes|releases|promote|rollback|pin|pause|resume|remove> [arguments]")
		fmt.Fprintln(out, "serve: S3-backed HTTP and read-only payday gRPC API; sync: GitHub polling; run: both")
	}
	if e := root.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	args = root.Args()
	if len(args) == 0 {
		root.Usage()
		return nil
	}
	command := args[0]
	if command == "routes" {
		return runRoutes(args[1:], out, errOut)
	}
	if command == "version" {
		fmt.Fprintln(out, "page "+version.Get().String())
		return nil
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(configPath, "config", *configPath, "configuration file")
	once := fs.Bool("once", false, "perform one source synchronization pass")
	source := fs.String("source", "", "assert the owning source configuration ID")
	wait := fs.Duration("wait", 0, "wait for the owner sync to apply deployment control")
	ordered, e := orderedFlags(args[1:])
	if e != nil {
		return e
	}
	if e := fs.Parse(ordered); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if fs.NArg() != 0 && !controlCommand(command) {
		return errors.New("unexpected positional arguments")
	}
	if command != "serve" && command != "sync" && command != "run" && command != "config" && command != "status" && !controlCommand(command) {
		return fmt.Errorf("unknown command %q", command)
	}
	if *once && command != "sync" {
		return errors.New("--once is only valid for sync")
	}
	if !controlCommand(command) && (*source != "" || *wait != 0) {
		return errors.New("source/wait options are only valid for deployment control")
	}
	c, e := Load(*configPath)
	if e != nil {
		return fmt.Errorf("load config: %w", e)
	}
	if command == "config" {
		for i := range c.GitHub {
			if c.GitHub[i].Token != "" {
				c.GitHub[i].Token = "<redacted>"
			}
		}
		b, e := yaml.Marshal(c)
		if e != nil {
			return e
		}
		_, e = out.Write(b)
		return e
	}
	store, e := storage.NewS3(ctx, c.Storage)
	if e != nil {
		return e
	}
	catalog := deploy.New(store)
	if controlCommand(command) {
		return runControl(ctx, catalog, command, fs.Args(), *source, *wait, out)
	}
	if command == "status" {
		return Status(ctx, c, catalog, out)
	}
	var syncs []*githubsync.Sync
	if command != "serve" {
		if len(c.GitHub) == 0 {
			return errors.New("configure at least one github source for sync/run")
		}
		for _, g := range c.GitHub {
			s, e := githubsync.New(catalog, g)
			if e != nil {
				return e
			}
			syncs = append(syncs, s)
		}
	}
	if command == "sync" && *once {
		var failures []error
		for _, s := range syncs {
			if e := s.Once(ctx); e != nil {
				failures = append(failures, e)
			}
		}
		return errors.Join(failures...)
	}
	g, runCtx := errgroup.WithContext(ctx)
	for _, s := range syncs {
		g.Go(func() error { return s.Run(runCtx) })
	}
	if command != "sync" {
		g.Go(func() error { return Serve(runCtx, c, catalog) })
	}
	e = g.Wait()
	if errors.Is(e, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return e
}

func Serve(ctx context.Context, c Config, catalog *deploy.Catalog) error {
	h, e := serving.New(catalog, serving.Config{BaseDomain: c.BaseDomain, PreviewStyle: c.PreviewStyle, RefreshInterval: c.RefreshInterval})
	if e != nil {
		return e
	}
	if e = h.Refresh(ctx); e != nil {
		return e
	}
	l, e := net.Listen("tcp", c.Listen)
	if e != nil {
		return e
	}
	defer l.Close()
	var apiListener net.Listener
	if c.APIListen != "" {
		apiListener, e = net.Listen("tcp", c.APIListen)
		if e != nil {
			return e
		}
		defer apiListener.Close()
	}
	g, runCtx := errgroup.WithContext(ctx)
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	slog.InfoContext(ctx, "serving static sites", "address", l.Addr().String(), "base_domain", c.BaseDomain, "preview_style", c.PreviewStyle)
	g.Go(func() error { return h.Run(runCtx) })
	g.Go(func() error { return serveHTTP(runCtx, server, l) })
	if apiListener != nil {
		rpc := grpc.NewServer()
		api.Register(rpc, catalog)
		reflection.Register(rpc)
		slog.InfoContext(ctx, "read-only resource API", "address", apiListener.Addr().String())
		g.Go(func() error { return serveGRPC(runCtx, rpc, apiListener) })
	}
	e = g.Wait()
	if errors.Is(e, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return e
}

func serveHTTP(ctx context.Context, s *http.Server, l net.Listener) error {
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	select {
	case e := <-done:
		s.Close()
		return e
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if e := s.Shutdown(shutdown); e != nil {
			s.Close()
		}
		<-done
		return ctx.Err()
	}
}
func serveGRPC(ctx context.Context, s *grpc.Server, l net.Listener) error {
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	select {
	case e := <-done:
		s.Stop()
		return e
	case <-ctx.Done():
		stopped := make(chan struct{})
		go func() { s.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			s.Stop()
			<-stopped
		}
		<-done
		return ctx.Err()
	}
}

func Status(ctx context.Context, c Config, cat *deploy.Catalog, out io.Writer) error {
	sites, e := cat.ListSites(ctx)
	if e != nil {
		return e
	}
	type preview struct {
		Kind     string `json:"kind"`
		Ref      string `json:"ref"`
		Revision string `json:"revision"`
		URL      string `json:"url"`
	}
	type site struct {
		Name       string               `json:"name"`
		Owner      string               `json:"owner"`
		Production string               `json:"productionRelease"`
		URL        string               `json:"url"`
		Previews   []preview            `json:"previews"`
		History    []string             `json:"history,omitempty"`
		Assets     map[string]time.Time `json:"assetReleases,omitempty"`
		Control    controlStatus        `json:"control"`
	}
	result := []site{}
	for _, s := range sites {
		x := site{Name: s.Name, Owner: s.Owner, Production: s.Production, URL: "https://" + deploy.Host(c.BaseDomain, c.PreviewStyle, s.Name, ""), Previews: []preview{}}
		x.History = s.History
		x.Assets = s.AssetReleases
		x.Control, e = getControlStatus(ctx, cat, s)
		if e != nil {
			return e
		}
		tokens := make([]string, 0, len(s.Aliases))
		for token := range s.Aliases {
			tokens = append(tokens, token)
		}
		sort.Strings(tokens)
		for _, token := range tokens {
			a := s.Aliases[token]
			x.Previews = append(x.Previews, preview{a.Kind, a.Name, a.Revision, "https://" + deploy.Host(c.BaseDomain, c.PreviewStyle, s.Name, token)})
		}
		result = append(result, x)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}
