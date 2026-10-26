package githubsync

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/serving"
	"github.com/lesomnus/page/internal/storage"
)

// Exercise the durable controller path used by a separate CLI process.
func operatorIntent(t *testing.T, cat *deploy.Catalog, mode, target string) {
	t.Helper()
	site := getSite(t, cat)
	if _, err := cat.RequestControl(context.Background(), site.Name, site.Owner, mode, target); err != nil {
		t.Fatal(err)
	}
}

func TestPendingIntentIsAppliedByOwnerEvenWhenForgeFails(t *testing.T) {
	ctx := context.Background()
	f, server := fixture(t)
	cat := deploy.New(storage.NewMemory())
	cfg := config(server.URL)
	mustOnce(t, newSync(t, cat, cfg))
	before := getSite(t, cat)
	target := before.Aliases[deploy.AliasToken("commit", sha(1))].Release
	beforeJSON, _ := json.Marshal(before)
	operatorIntent(t, cat, "pinned", target)
	requested := getSite(t, cat)
	requestedJSON, _ := json.Marshal(requested)
	if !bytesEqual(beforeJSON, requestedJSON) {
		t.Fatal("separate controller wrote public site state")
	}
	control, err := cat.GetControl(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if control.UpdatedAt.Equal(before.ControlAppliedAt) {
		t.Fatal("new intent already appears applied")
	}
	f.mu.Lock()
	f.pageFailure = true
	f.mu.Unlock()
	if err := newSync(t, cat, cfg).Once(ctx); err == nil {
		t.Fatal("forge failure was not reported")
	}
	active := getSite(t, cat)
	if active.Production != target || !active.ControlAppliedAt.Equal(control.UpdatedAt) {
		t.Fatalf("owner did not activate pending pin independently of forge: %+v", active)
	}
	// A second failed poll has no new intent and must not rewrite publication.
	activeJSON, _ := json.Marshal(active)
	if err := newSync(t, cat, cfg).Once(ctx); err == nil {
		t.Fatal("forge failure was not reported")
	}
	againJSON, _ := json.Marshal(getSite(t, cat))
	if !bytesEqual(activeJSON, againJSON) {
		t.Fatal("failed poll rewrote an already applied control")
	}
}

func TestTargetDisappearingBeforeActivationKeepsPriorPublication(t *testing.T) {
	ctx := context.Background()
	f, server := fixture(t)
	cat := deploy.New(storage.NewMemory())
	cfg := config(server.URL)
	mustOnce(t, newSync(t, cat, cfg))
	before := getSite(t, cat)
	release, err := cat.Publish(ctx, before.Name, before.Owner, deploy.Origin{Kind: "git", RepositoryID: before.RepositoryID, Revision: sha(77), Directory: "public"}, []deploy.InputFile{{Path: "index.html", Size: 3, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("old")), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	target := release.ID
	beforeJSON, _ := json.Marshal(before)
	operatorIntent(t, cat, "pinned", target)
	// Simulate an already in-flight collector that withdrew completeness after
	// the controller validated the target, before the owner applied the intent.
	if err := cat.Store.Delete(ctx, deploy.ReleasePrefix(before.Owner, before.Name, target)+"manifest.json"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.pageFailure = true
	f.mu.Unlock()
	if err := newSync(t, cat, cfg).Once(ctx); err == nil {
		t.Fatal("unavailable activation target was not reported")
	}
	after := getSite(t, cat)
	afterJSON, _ := json.Marshal(after)
	if !bytesEqual(beforeJSON, afterJSON) {
		t.Fatal("failed activation exposed a dangling production reference")
	}
	control, err := cat.GetControl(ctx, after)
	if err != nil || control.UpdatedAt.Equal(after.ControlAppliedAt) {
		t.Fatalf("failed activation was not left visibly pending: %v %v", control, err)
	}
}

func TestPauseResumeAndRemoveAreAppliedDuringForgeOutage(t *testing.T) {
	for _, mode := range []string{"paused", "auto", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, server := fixture(t)
			cat := deploy.New(storage.NewMemory())
			cfg := config(server.URL)
			mustOnce(t, newSync(t, cat, cfg))
			before := getSite(t, cat)
			if mode == "auto" {
				operatorIntent(t, cat, "paused", before.Production)
				mustOnce(t, newSync(t, cat, cfg))
				before = getSite(t, cat)
			}
			operatorIntent(t, cat, mode, "")
			control, err := cat.GetControl(ctx, before)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.pageFailure = true
			reads := f.archiveReads
			f.mu.Unlock()
			if err := newSync(t, cat, cfg).Once(ctx); err == nil {
				t.Fatal("forge failure was not reported")
			}
			after := getSite(t, cat)
			if after.ControlMode != mode || !after.ControlAppliedAt.Equal(control.UpdatedAt) {
				t.Fatalf("owner did not apply %s during outage: %+v", mode, after)
			}
			if mode == "disabled" {
				if !after.Disabled || after.Production != "" || len(after.Aliases) != 0 || len(after.AssetReleases) != 0 {
					t.Fatal("removal did not withdraw every public reference")
				}
			} else if after.Disabled || after.Production != before.Production || len(after.Aliases) != len(before.Aliases) {
				t.Fatal("mode change unexpectedly changed source publications during outage")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.archiveReads != reads {
				t.Fatal("applying operator intent downloaded a source artifact")
			}
		})
	}
}

func TestOperatorProductionControlSurvivesSourcePollingAndRestart(t *testing.T) {
	ctx := context.Background()
	f, server := fixture(t)
	cat := deploy.New(storage.NewMemory())
	cfg := config(server.URL)
	cfg.RecentCommits = 1
	cfg.Branches = []string{"main"}
	cfg.Tags = []string{}
	mustOnce(t, newSync(t, cat, cfg))
	original := getSite(t, cat).Production
	operatorIntent(t, cat, "paused", original)
	f.mu.Lock()
	f.head, f.branches["main"] = sha(3), sha(3)
	f.mu.Unlock()
	mustOnce(t, newSync(t, cat, cfg))
	paused := getSite(t, cat)
	if paused.Production != original {
		t.Fatal("polling undid the durable production pause")
	}
	if paused.Aliases[deploy.AliasToken("branch", "refs/heads/main")].Revision != sha(3) {
		t.Fatal("pausing production unnecessarily stopped branch preview updates")
	}
	operatorIntent(t, cat, "auto", "")
	mustOnce(t, newSync(t, cat, cfg))
	automatic := getSite(t, cat)
	if automatic.Production == original {
		t.Fatal("resuming did not follow the source head")
	}
	operatorIntent(t, cat, "pinned", original)
	f.mu.Lock()
	f.head, f.branches["main"] = sha(4), sha(4)
	f.mu.Unlock()
	mustOnce(t, newSync(t, cat, cfg))
	pinned := getSite(t, cat)
	if pinned.Production != original || pinned.Aliases[deploy.AliasToken("branch", "refs/heads/main")].Revision != sha(4) {
		t.Fatal("pin did not preserve production independently of moving previews")
	}
	r, err := cat.GetRelease(ctx, pinned.Owner, pinned.Name, pinned.Production)
	if err != nil || r.Origin.Revision != sha(2) {
		t.Fatalf("pin restored the wrong snapshot: %v %v", r, err)
	}
}

func TestDisabledSiteCannotBeRediscoveredAndCanCleanUpWhileForgeFails(t *testing.T) {
	ctx := context.Background()
	f, server := fixture(t)
	cat := deploy.New(storage.NewMemory())
	cfg := config(server.URL)
	cfg.GCGrace = time.Nanosecond
	mustOnce(t, newSync(t, cat, cfg))
	original := getSite(t, cat).Production
	operatorIntent(t, cat, "disabled", "")
	mustOnce(t, newSync(t, cat, cfg))
	withdrawn := getSite(t, cat)
	if !withdrawn.Disabled || withdrawn.Production != "" || len(withdrawn.Aliases) != 0 {
		t.Fatal("discovery undid explicit site withdrawal")
	}
	f.mu.Lock()
	f.pageFailure = true
	reads := f.archiveReads
	f.mu.Unlock()
	s := newSync(t, cat, cfg)
	s.now = func() time.Time { return time.Now().UTC().Add(time.Hour) }
	if err := s.Once(ctx); err == nil {
		t.Fatal("failed forge listing unexpectedly succeeded")
	}
	if _, err := cat.GetRelease(ctx, withdrawn.Owner, withdrawn.Name, original); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("explicitly removed site was not collected independently of the forge: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.archiveReads != reads {
		t.Fatal("disabled source downloaded an artifact")
	}
}

func immutableArchive(t *testing.T, revision string) []byte {
	t.Helper()
	return tarball(t, []tarEntry{
		{"public/index.html", "snapshot-" + revision, tar.TypeReg},
		{"public/pages.json", `{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`, tar.TypeReg},
		{"public/assets/app." + revision + ".js", "bundle-" + revision, tar.TypeReg},
	})
}

func TestAssetRetentionKeepsOnlyPriorProductionWithoutReopeningPreview(t *testing.T) {
	ctx := context.Background()
	f, server := fixture(t)
	f.mu.Lock()
	f.archives[sha(2)] = immutableArchive(t, "two")
	f.archives[sha(3)] = immutableArchive(t, "three")
	f.mu.Unlock()
	cat := deploy.New(storage.NewMemory())
	cfg := config(server.URL)
	cfg.RecentCommits = 1
	cfg.Branches, cfg.Tags = []string{}, []string{}
	cfg.GCGrace = time.Nanosecond
	cfg.AssetRetention = 500 * time.Millisecond
	mustOnce(t, newSync(t, cat, cfg))
	original := getSite(t, cat).Production
	f.mu.Lock()
	f.head, f.branches["main"] = sha(3), sha(3)
	f.mu.Unlock()
	mustOnce(t, newSync(t, cat, cfg))
	after := getSite(t, cat)
	expires := after.AssetReleases[original]
	if expires.IsZero() || !time.Now().Before(expires) {
		t.Fatal("old production did not receive an asset-only lease")
	}
	if _, exists := after.Aliases[deploy.AliasToken("commit", sha(2))]; exists {
		t.Fatal("asset lease reopened the retired commit preview")
	}
	mustOnce(t, newSync(t, cat, cfg))
	if _, err := cat.GetRelease(ctx, after.Owner, after.Name, original); err != nil {
		t.Fatal("GC removed a leased asset snapshot:", err)
	}
	h, err := serving.New(cat, serving.Config{BaseDomain: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://foo.example.com/assets/app.two.js", nil)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "bundle-two" {
		t.Fatalf("published asset lease was not served: %d %q", response.Code, response.Body.String())
	}
	assertHTTP(t, h, deploy.Host("example.com", "flat", "foo", deploy.AliasToken("commit", sha(2))), 404, "")
	time.Sleep(time.Until(expires) + 10*time.Millisecond)
	// The first completed pass records retirement; the following one collects.
	mustOnce(t, newSync(t, cat, cfg))
	mustOnce(t, newSync(t, cat, cfg))
	if _, err := cat.GetRelease(ctx, after.Owner, after.Name, original); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired asset lease prevented owned GC: %v", err)
	}
}

func TestInvalidPagesOrImmutableURLReuseKeepsCurrentProduction(t *testing.T) {
	for _, failure := range []string{"invalid-pages", "immutable-url-reuse"} {
		t.Run(failure, func(t *testing.T) {
			f, server := fixture(t)
			oldEntries := []tarEntry{{"public/index.html", "old", tar.TypeReg}, {"public/pages.json", `{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`, tar.TypeReg}, {"public/assets/app.js", "old bundle", tar.TypeReg}}
			f.mu.Lock()
			f.archives[sha(2)] = tarball(t, oldEntries)
			f.mu.Unlock()
			cat := deploy.New(storage.NewMemory())
			cfg := config(server.URL)
			cfg.RecentCommits = 1
			cfg.Branches, cfg.Tags = []string{}, []string{}
			mustOnce(t, newSync(t, cat, cfg))
			before := getSite(t, cat)
			beforeJSON, _ := json.Marshal(before)
			pages := `{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`
			if failure == "invalid-pages" {
				pages = `{"schemaVersion":99}`
			}
			f.mu.Lock()
			f.head = sha(3)
			f.archives[sha(3)] = tarball(t, []tarEntry{{"public/index.html", "new", tar.TypeReg}, {"public/pages.json", pages, tar.TypeReg}, {"public/assets/app.js", "different bundle", tar.TypeReg}})
			f.mu.Unlock()
			if err := newSync(t, cat, cfg).Once(context.Background()); err == nil {
				t.Fatal("invalid new publication unexpectedly succeeded")
			}
			afterJSON, _ := json.Marshal(getSite(t, cat))
			if !bytesEqual(beforeJSON, afterJSON) {
				t.Fatalf("failed new publication changed current site:\n%s\n%s", beforeJSON, afterJSON)
			}
		})
	}
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }
