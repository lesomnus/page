// Package api connects payday-generated read services to object storage.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	page "github.com/lesomnus/page"
	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Sites struct {
	page.UnimplementedSiteServiceServer
	Catalog *deploy.Catalog
}
type Deployments struct {
	page.UnimplementedDeploymentServiceServer
	Catalog *deploy.Catalog
}

func Register(g grpc.ServiceRegistrar, c *deploy.Catalog) {
	page.RegisterSiteServiceServer(g, &Sites{Catalog: c})
	page.RegisterDeploymentServiceServer(g, &Deployments{Catalog: c})
}

func id(domain byte, key string) []byte {
	sum := sha256.Sum256([]byte(key))
	b := append([]byte(nil), sum[:16]...)
	b[6] = 0x80 | (b[6] & 15)
	b[8] = 0x80 | (b[8] & 63)
	b[9] = domain
	return b
}
func siteMessage(s *deploy.Site) *page.Site {
	return page.Site_builder{Id: id(8, s.Owner+"/"+s.RepositoryID+"/"+s.Name), Alias: s.Name, SourceId: s.Owner, RepositoryId: s.RepositoryID, Repository: s.Repository, ProductionRelease: s.Production, DateCreated: timestamppb.New(s.CreatedAt)}.Build()
}
func deploymentMessage(r *deploy.Release) *page.Deployment {
	return page.Deployment_builder{Id: id(9, r.Owner+"/"+r.Site+"/"+r.ID), Alias: r.ID, SourceId: r.Owner, SiteAlias: r.Site, Revision: r.Origin.Revision, SourceKind: r.Origin.Kind, ReleaseId: r.ID, DateCreated: timestamppb.New(r.CreatedAt)}.Build()
}
func rpcError(err error) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return status.Error(codes.NotFound, "resource not found")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	default:
		return status.Error(codes.Unavailable, "publication storage unavailable")
	}
}

func (s *Sites) all(ctx context.Context) ([]*page.Site, error) {
	all, e := s.Catalog.ListSites(ctx)
	if e != nil {
		return nil, rpcError(e)
	}
	out := make([]*page.Site, 0, len(all))
	for _, x := range all {
		out = append(out, siteMessage(x))
	}
	return out, nil
}
func (s *Deployments) all(ctx context.Context) ([]*page.Deployment, error) {
	sites, e := s.Catalog.ListSites(ctx)
	if e != nil {
		return nil, rpcError(e)
	}
	var out []*page.Deployment
	for _, site := range sites {
		rs, e := s.Catalog.ListReleases(ctx, site.Owner, site.Name)
		if e != nil {
			return nil, rpcError(e)
		}
		for _, r := range rs {
			if r.Complete {
				out = append(out, deploymentMessage(r))
			}
		}
	}
	return out, nil
}

func (s *Sites) Get(ctx context.Context, r *page.SiteGetRequest) (*page.Site, error) {
	if r.GetSelect() != nil {
		return nil, status.Error(codes.Unimplemented, "field selection is not supported")
	}
	if len(r.GetRef().GetId()) != 16 {
		return nil, status.Error(codes.InvalidArgument, "a resource UUID is required")
	}
	all, e := s.all(ctx)
	if e != nil {
		return nil, e
	}
	for _, x := range all {
		if bytes.Equal(x.GetId(), r.GetRef().GetId()) {
			return x, nil
		}
	}
	return nil, status.Error(codes.NotFound, "site not found")
}
func (s *Deployments) Get(ctx context.Context, r *page.DeploymentGetRequest) (*page.Deployment, error) {
	if r.GetSelect() != nil {
		return nil, status.Error(codes.Unimplemented, "field selection is not supported")
	}
	if len(r.GetRef().GetId()) != 16 {
		return nil, status.Error(codes.InvalidArgument, "a resource UUID is required")
	}
	all, e := s.all(ctx)
	if e != nil {
		return nil, e
	}
	for _, x := range all {
		if bytes.Equal(x.GetId(), r.GetRef().GetId()) {
			return x, nil
		}
	}
	return nil, status.Error(codes.NotFound, "deployment not found")
}

type cursor struct {
	Kind    string
	Reverse bool
	ID      string
	Filter  string
}

func paginate[T interface{ GetId() []byte }](all []T, kind string, size int32, after string, reverse bool, filters [][]byte) ([]T, string, error) {
	if len(filters) > 64 || size < 0 {
		return nil, "", status.Error(codes.InvalidArgument, "invalid pagination or too many filters")
	}
	if size == 0 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	fh := sha256.New()
	for _, f := range filters {
		if len(f) != 16 {
			return nil, "", status.Error(codes.InvalidArgument, "filter requires a resource UUID")
		}
		fh.Write(f)
	}
	fingerprint := hex.EncodeToString(fh.Sum(nil))
	var key []byte
	if after != "" {
		b, e := base64.RawURLEncoding.DecodeString(after)
		var c cursor
		if e != nil || json.Unmarshal(b, &c) != nil || c.Kind != kind || c.Reverse != reverse || c.Filter != fingerprint {
			return nil, "", status.Error(codes.InvalidArgument, "cursor does not match this query")
		}
		key, e = hex.DecodeString(c.ID)
		if e != nil || len(key) != 16 {
			return nil, "", status.Error(codes.InvalidArgument, "invalid cursor")
		}
	}
	sort.Slice(all, func(i, j int) bool {
		cmp := bytes.Compare(all[i].GetId(), all[j].GetId())
		if reverse {
			return cmp > 0
		}
		return cmp < 0
	})
	var selected []T
	for _, x := range all {
		ok := true
		for _, f := range filters {
			if !bytes.Equal(f, x.GetId()) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if len(key) > 0 {
			cmp := bytes.Compare(x.GetId(), key)
			if (!reverse && cmp <= 0) || (reverse && cmp >= 0) {
				continue
			}
		}
		selected = append(selected, x)
	}
	next := ""
	if len(selected) > int(size) {
		selected = selected[:size]
		b, _ := json.Marshal(cursor{kind, reverse, hex.EncodeToString(selected[len(selected)-1].GetId()), fingerprint})
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	return selected, next, nil
}
func (s *Sites) List(ctx context.Context, r *page.SiteListRequest) (*page.SiteListResponse, error) {
	all, e := s.all(ctx)
	if e != nil {
		return nil, e
	}
	var filters [][]byte
	for _, f := range r.GetFilters() {
		filters = append(filters, f.GetRef().GetId())
	}
	items, next, e := paginate(all, "site", r.GetSize(), r.GetAfter(), r.GetReverse(), filters)
	if e != nil {
		return nil, e
	}
	return page.SiteListResponse_builder{Items: items, Next: next}.Build(), nil
}
func (s *Deployments) List(ctx context.Context, r *page.DeploymentListRequest) (*page.DeploymentListResponse, error) {
	all, e := s.all(ctx)
	if e != nil {
		return nil, e
	}
	var filters [][]byte
	for _, f := range r.GetFilters() {
		filters = append(filters, f.GetRef().GetId())
	}
	items, next, e := paginate(all, "deployment", r.GetSize(), r.GetAfter(), r.GetReverse(), filters)
	if e != nil {
		return nil, e
	}
	return page.DeploymentListResponse_builder{Items: items, Next: next}.Build(), nil
}
