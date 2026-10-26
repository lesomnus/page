package cmd

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"google.golang.org/grpc"

	"github.com/lesomnus/payday/access"
	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/grpcx"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/resource"
	"github.com/lesomnus/payday/trail"
	"github.com/lesomnus/payday/watch"
	"github.com/lesomnus/payday/web"

	app "github.com/lesomnus/page"
	"github.com/lesomnus/page/internal/ent"
	"github.com/lesomnus/page/server/bare"
	"github.com/lesomnus/page/server/pd"
)

// Server is a built app: the database it runs on and the two stacks it answers
// through.
type Server struct {
	Db  *sql.DB
	Ent *ent.Client

	// Drv is what the client was built on, kept because a transaction is begun
	// on a driver and a `*ent.Client` does not hand out the one it holds. It is
	// what `pd.Batch` puts a whole stack onto.
	Drv dialect.Driver

	// Dialect is what that driver speaks, which `Open` worked out and nothing
	// else can. It is kept because the guard below needs it, and re-deriving it
	// from the configuration would be a second answer to a question already
	// answered.
	Dialect string

	// Watch is what a change is published to once the call that made it has
	// answered. The broker is named rather than defaulted: the one that
	// publishes in this process is right for one replica and **silently wrong**
	// for two, since a subscriber on one never hears about a write on another.
	Watch *watch.Watch

	// Walled is what a caller reaches, and Ungated is what the deployment does
	// its own work through -- putting the first tenant there, working out who
	// is calling. Neither is a privilege anybody holds: the second is a server
	// instance somebody was handed, so going around the wall is a line of
	// wiring a reader can find rather than a rule that opens up whenever
	// nobody is asking.
	Walled  app.Server
	Ungated app.Server

	// Auth is how a credential is read, and nothing that faces anybody is
	// [auth.Plain] -- which believes what the caller writes about themselves.
	// Replacing it is the first thing this app has to do that payday cannot do
	// for it.
	//
	// It is set after [Build] rather than being one of its arguments, because
	// what reads a credential is often built from what [Build] made: a session
	// handler needs a store, and a store is a table in this database.
	Auth auth.Handler

	// Policy is what a caller may do and which tenants they may see, and nil is
	// the answer payday gives on its own: their own tenant and nothing more.
	//
	// A field beside [Server.Auth] because they are the pair -- who is calling,
	// and what that means here -- and because the two places it has to be
	// installed are not one place. `gate.Interceptor` covers the calls gRPC
	// dispatches; `c.Server.Guard` covers the operations inside a batch, which
	// arrive as one method carrying many and would otherwise be authorised as
	// the batch rather than as themselves.
	//
	// It is what a second binary is built from. A deployment that wants an
	// operator's path serves this app again with a policy that answers
	// differently, on an address only an operator can reach, rather than
	// putting a rule in here that says some row is special.
	Policy gate.Policy

	// Access is the access record's writer (payday#111): what a read's row
	// waits in to be written with the others of its second, and what writes a
	// download's at once. Its loop is in [Server.Spin], and each process runs
	// its own, since what it holds is in this one's memory.
	Access *access.Writer

	// Downloads are the methods that hand personal data out to be kept
	// somewhere else -- an export, a file -- which say what for or are
	// refused; see `access.Config.Downloads`. Which they are is the app's to
	// know.
	Downloads []string

	// Spin is whatever this deployment has to run besides answering requests.
	// It is a slice rather than a method because a server with nothing to run
	// should write nothing at all; see `payday/spin`.
	Spin []any
}

// Build opens the database and stacks the servers.
//
// The two hooks are the whole of what payday puts in the write and read paths,
// and both come out of what the schema declared: [pd.Minter] stamps a new row
// with the domain of its entity and refuses one of another, [pd.Wall] narrows
// every read to the tenants the caller may see.
func Build(ctx context.Context, c Config) (*Server, error) {
	db, dialect, err := c.Db.Open(ctx)
	if err != nil {
		return nil, err
	}

	// The ent client is the app's to build, which is why config hands back a
	// *sql.DB and the dialect rather than a client: the client is generated
	// into this app from this app's schema, and payday has no name for it.
	drv := entsql.OpenDB(dialect, db)
	client := ent.NewClient(ent.Driver(drv))

	// The server that talks to the database, twice: once as it is, and once
	// with the wall on it.
	//
	// Two things are said to it rather than to the stack, and for the same
	// reason -- both are about the statement that runs. The trail is kept by
	// the servers that do the writing, since every RPC that changes anything
	// has to report itself from inside the transaction that changes it. The
	// wall is a predicate and a predicate belongs in the WHERE.
	b, err := c.Watch.Build(ctx, c.Db)
	if err != nil {
		db.Close()
		return nil, err
	}

	w := watch.New(b)

	// The recorders, in the order they are told. The trail is required -- a
	// write it could not account for is undone -- and the rest say for
	// themselves whether they are; see `bare.Recorders`.
	// Every write is told to these, in this order, inside the transaction that
	// makes it. It is one value rather than the option given three times
	// because `WithRecorder` refuses to be given twice: neither losing one nor
	// quietly appending in call order is a thing a framework should decide, so
	// the order is written here where it can be read.
	// What the trail keeps of each kind's writes is the deployment's to say,
	// `audit.content` -- read here, where the trail is wired, so that a
	// setting nobody handed over is not a setting that does nothing.
	content, err := c.Audit.Contents()
	if err != nil {
		db.Close()
		return nil, err
	}
	// The access record's is last: what it writes at the commit is of the
	// data the call's writes wrote, so it is told of each and writes once.
	rec := bare.Recorders{
		pd.Recorder(content),
		pd.ResourceRecorder(w, c.Watch.Outbox, nil, resource.RecorderOptions{}),
		pd.AccessRecorder(),
	}

	opts := []bare.Option{bare.WithMinter(pd.Minter()), bare.WithRecorder(rec)}

	sink, err := pd.NewSink(client, opts...)
	if err != nil {
		db.Close()
		return nil, err
	}

	walled, err := pd.NewSink(client, append(opts, bare.WithScope(pd.Wall()))...)
	if err != nil {
		db.Close()
		return nil, err
	}

	// The stack a caller reaches. `pd.Gate` is outermost, so nothing behind it
	// asks again.
	stacked, err := app.Build(walled.WithWatch(w), pd.AuditBuild(), pd.GateBuild())
	if err != nil {
		db.Close()
		return nil, err
	}

	// And the same servers with no wall and no gate, which is what the
	// deployment does its own work through. It is not a privilege anybody
	// holds: it is an instance somebody was handed, so going around the wall
	// is a line of wiring a reader can find rather than a rule that opens up
	// whenever nobody is asking.
	ungated, err := app.Build(sink.WithWatch(w), pd.AuditBuild())
	if err != nil {
		db.Close()
		return nil, err
	}

	s := &Server{Db: db, Ent: client, Drv: drv, Dialect: dialect, Watch: w, Walled: stacked, Ungated: ungated}

	// The access record's reads are written a second at a time, by a loop of
	// each process's own that writes what it holds once more as it stops.
	s.Access = &access.Writer{Sink: pd.AccessSink(client)}
	s.Spin = append(s.Spin, s.Access)

	if c.Watch.Outbox && b != nil {
		// The loop that makes an event durable. It is not a layer and not a
		// method on any server -- `spin.Run` finds it in whatever is handed
		// over, which is what keeps a server that has no background work from
		// carrying an empty method saying so.
		s.Spin = append(s.Spin, pd.Drain(client, b, c.Watch.Every()))
	}

	// And the trail's retention, which is the one loop here that is a
	// **mechanism** rather than a tidy-up: nothing else applies a window, so an
	// outage of it is a deployment keeping records it said it would not.
	//
	// The policy is resolved and checked **here**, where a refusal stops the
	// process, rather than at the first pass a day later -- a `retain` with
	// nowhere to put what leaves it is a configuration that works and destroys
	// the evidence.
	//
	// Configured nowhere, this does nothing and the trail is kept forever. That
	// is the default because the alternative is a version upgrade deciding how
	// long somebody's evidence lasts; see `audit:` in the configuration file.
	p, err := c.Audit.Policy()
	if err != nil {
		db.Close()

		return nil, err
	}

	// And the turns a pass takes with whatever erases from the trail -- a
	// person forgotten, a tenant leaving -- on the deployment's lease
	// (`trail.Policy.Lease`), so that neither undoes the other. Its own, and
	// not the one the loops are run under: a loop's lease is which process
	// runs it, and the trail's turn is a lock any process takes for a moment.
	// A process that runs no loops (`none`) has nothing to take turns on.
	if c.Spin.Lease != "" && c.Spin.Lease != config.LeaseNone {
		if p.Lease, err = c.Spin.Build(ctx, c.Db); err != nil {
			db.Close()

			return nil, err
		}
	}
	if p.On() {
		log.From(ctx).InfoContext(ctx, "trail: retention", "policy", p.String())

		s.Spin = append(s.Spin, trail.Sweep(pd.TrailStore(client), p))
	}

	return s, nil
}

func (s *Server) Close() error { return s.Db.Close() }

// Grpc builds the server every call arrives at.
//
// The chain is payday's and the order is this app's to read: what records a
// call is outside everything, then the recovery, then the deadline a call that
// named none is given, then how often one caller may ask, then what is closed
// to callers entirely.
//
// It is separate from [Server.Serve] so that a test can travel exactly this
// and answer on a listener that is a channel; see pdtest.
func (s *Server) Grpc(ctx context.Context, c Config, opts ...grpc.ServerOption) (*grpc.Server, error) {
	// Who is calling comes first, since everything after it reads the frame.
	// `Plain` believes what the caller writes, which is right for a sandbox
	// and for tests and is not something to serve where anyone can reach it.
	//
	// [Server.Auth] replaces it, which is how this app reads something else --
	// a certificate, a token, a session cookie. It is a field rather than a
	// configuration setting for the reason payday suggests two deployments
	// rather than a flag: a mistake in a YAML file must not be able to turn
	// authentication off.
	h := s.Auth
	if h == nil {
		h = auth.Plain()
	}

	chain := grpcx.Serving(ctx, grpcx.WithDeadline(c.Server.CallTimeout())).
		WithUnary(auth.InterceptorUnary(h, Resolver(s.Ungated), auth.PublicDefault)).
		WithStream(auth.InterceptorStream(h, Resolver(s.Ungated), auth.PublicDefault)).
		WithUnary(grpcx.LimitUnary(c.Server.Limiter(), gate.ByTenant(), pd.Cost)).
		With(s.accessed().Interceptor()).
		With(gate.Interceptor(s.Policy)).
		With(s.Watch.Interceptor()).
		WithUnary(grpcx.ClosedUnary(c.Server.Closed()))

	os := append(opts, chain.ServerOptions()...)
	// A certificate that cannot be read is a server that must not start, which
	// is why this answers with an error: `GrpcOptions` reads the files
	// `server.tls` names.
	vs, err := c.Server.GrpcOptions()
	if err != nil {
		return nil, err
	}

	os = append(os, vs...)

	g := grpc.NewServer(os...)
	app.RegisterServer(g, s.Walled)

	// The batch, with the same rules the chain above enforces -- read off the
	// same configuration rather than written out again, which is the only way
	// the two stay in step. What they enforce by looking at the method gRPC
	// dispatched, this enforces per operation.
	// Both endpoints share the operation limiter, so switching API names cannot
	// replenish a caller's Batch budget.
	guard := c.Server.Guard(s.Policy)
	if b, err := pd.Batch(s.Walled, s.Drv, guard); err == nil {
		pdpb.RegisterBatchServiceServer(g, b)

		// Its Watch evaluates a filter because `cli/filter.go` links the
		// language, and not because of anything here: this package is the
		// sandbox's too, and a language named in it is linked into the page.
		resources, err := pd.Resources(s.Walled, s.Drv, guard, s.Watch,
			pd.ResourceProjector(s.Ent, pd.Wall(), guard))
		if err != nil {
			return nil, err
		}
		pdpb.RegisterResourceServiceServer(g, resources)
	} else {
		// A deployment that closed nothing, limits nothing and has no policy.
		// It is a real thing to be and it is also what a guard nobody filled in
		// looks like, so the batch is not served rather than served open.
		log.From(ctx).WarnContext(ctx, "no batch", slog.Any("err", err))
	}

	return g, nil
}

// accessed is what the access record's interceptor is told of this app: whose
// data a message carries, read off what its schema declared `personal`; where a
// call came from; and which of its calls are downloads.
//
// Where a call came from is the connection's here. Behind a proxy that is the
// proxy, and the caller is in a header only the deployment knows whether to
// believe: answer `Address` with that, past the proxies it trusts.
func (s *Server) accessed() access.Config {
	return access.Config{
		Subjects:  pd.Subjects,
		Refs:      pd.SubjectRefs,
		Address:   access.Peer,
		Downloads: s.Downloads,
		Writer:    s.Access,
	}
}

// Serve answers on `l` until the context is done, and lets what it is
// answering end before it returns.
func (s *Server) Serve(ctx context.Context, c Config, l net.Listener) error {
	g, err := s.Grpc(ctx, c)
	if err != nil {
		return err
	}

	stop, err := s.serveHttp(ctx, c, g)
	if err != nil {
		return err
	}
	defer stop()

	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		g.GracefulStop()
		close(stopped)
	}()

	err = g.Serve(l)
	if ctx.Err() == nil {
		return err
	}

	// Asked to stop: what was being answered ends first, and what it read is
	// written after it -- the access record's own last write, as its loop
	// stops, is too early for a call that answers while the server drains.
	<-stopped
	last, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Access.Flush(last); err != nil {
		log.From(ctx).ErrorContext(ctx, "access: the record waiting to be written was lost as the server stopped", "rows", s.Access.Pending(), "err", err)
	}

	return err
}

// serveHttp is the second listener, for whatever cannot speak gRPC -- which is
// every browser. Nothing is opened unless the configuration named an address.
//
// It is the **same** `g`: a page reaches the handlers a gRPC client reaches,
// through the interceptors a gRPC client goes through, behind the same wall.
// There is no second stack here for a rule to be missing from.
func (s *Server) serveHttp(ctx context.Context, c Config, g *grpc.Server) (func(), error) {
	if !c.Server.Http.Serves() {
		return func() {}, nil
	}

	h, err := web.New(c.Server.Http, g)
	if err != nil {
		return nil, err
	}

	// Whatever this app serves over HTTP goes here, on the same mux and behind
	// the same cross-origin answer. Signing a browser in is the one to expect,
	// and `auth/authsession` is most of it: the endpoint, the cookie and its
	// attributes, the expiry and the store it is kept in. What it takes from
	// this app is a `Verify`, because the people are in this app's schema and
	// what checking their secret means is this app's to say.
	//
	//	h.Handle("GET /session", sessions.Serve(login))     // which session the browser holds
	//	h.Handle("POST /session", sessions.Serve(login))    // sign in
	//	h.Handle("DELETE /session", sessions.Serve(login))  // sign out
	//
	// with `s.Auth = sessions.Handler()` before [Server.Grpc]. `ts/src/session.tsx`
	// is the page's half: it holds no credential, so it asks the GET which
	// session it holds and names its store by the answer.
	//
	// A gRPC path is `/<service>/<method>`, so an ordinary route cannot collide
	// with one -- and `ServeMux` panics rather than shadowing if one somehow
	// does.

	l, err := net.Listen("tcp", c.Server.Http.Addr)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Handler: h}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.From(ctx).ErrorContext(ctx, "http", slog.Any("err", err))
		}
	}()

	log.From(ctx).InfoContext(ctx, "http", slog.String("addr", l.Addr().String()))

	return func() { srv.Close() }, nil
}
