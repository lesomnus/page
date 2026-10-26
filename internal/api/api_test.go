package api

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	page "github.com/lesomnus/page"
	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestGeneratedServicesOverGRPCWithoutDatabase(t *testing.T) {
	ctx := context.Background()
	cat := deploy.New(storage.NewMemory())
	for _, name := range []string{"foo", "bar"} {
		body := "hello"
		r, e := cat.Publish(ctx, name, "source", deploy.Origin{Kind: "git", RepositoryID: name, Repository: "org/" + name + ".app", Revision: "abc", Directory: "public"}, []deploy.InputFile{{Path: "index.html", Size: int64(len(body)), Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }}})
		if e != nil {
			t.Fatal(e)
		}
		e = cat.SaveSite(ctx, &deploy.Site{SchemaVersion: 1, Name: name, Owner: "source", RepositoryID: name, Repository: r.Origin.Repository, Production: r.ID})
		if e != nil {
			t.Fatal(e)
		}
	}
	l := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	Register(g, cat)
	go g.Serve(l)
	t.Cleanup(g.Stop)
	conn, e := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sites := page.NewSiteServiceClient(conn)
	first, e := sites.List(ctx, page.SiteListRequest_builder{Size: 1}.Build())
	if e != nil {
		t.Fatal(e)
	}
	if len(first.GetItems()) != 1 || first.GetNext() == "" {
		t.Fatalf("bad first page: %v", first)
	}
	second, e := sites.List(ctx, page.SiteListRequest_builder{Size: 1, After: first.GetNext()}.Build())
	if e != nil {
		t.Fatal(e)
	}
	if len(second.GetItems()) != 1 || second.GetNext() != "" || string(first.GetItems()[0].GetId()) == string(second.GetItems()[0].GetId()) {
		t.Fatalf("bad second page: %v", second)
	}
	got, e := sites.Get(ctx, page.SiteGetRequest_builder{Ref: page.SiteRef_builder{Id: first.GetItems()[0].GetId()}.Build()}.Build())
	if e != nil || got.GetAlias() != first.GetItems()[0].GetAlias() {
		t.Fatalf("get mismatch: %v %v", got, e)
	}
	if _, e = sites.List(ctx, page.SiteListRequest_builder{After: first.GetNext(), Reverse: true}.Build()); status.Code(e) != codes.InvalidArgument {
		t.Fatalf("cross-direction cursor: %v", e)
	}
	filtered, e := sites.List(ctx, page.SiteListRequest_builder{Filters: []*page.SiteFilter{page.SiteFilter_builder{Ref: page.SiteRef_builder{Id: got.GetId()}.Build()}.Build()}}.Build())
	if e != nil || len(filtered.GetItems()) != 1 {
		t.Fatalf("filter: %v %v", filtered, e)
	}
	if _, e = sites.Get(ctx, page.SiteGetRequest_builder{}.Build()); status.Code(e) != codes.InvalidArgument {
		t.Fatalf("missing ref: %v", e)
	}
	deployments := page.NewDeploymentServiceClient(conn)
	rs, e := deployments.List(ctx, page.DeploymentListRequest_builder{}.Build())
	if e != nil || len(rs.GetItems()) != 2 {
		t.Fatalf("deployments: %v %v", rs, e)
	}
	if rs.GetItems()[0].GetSourceKind() != "git" || rs.GetItems()[0].GetSourceId() != "source" {
		t.Fatal("lost origin")
	}
}
