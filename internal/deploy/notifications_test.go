package deploy

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lesomnus/page/internal/storage"
)

func assertNoNotice(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected change notification")
	default:
	}
}

func assertNotice(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatal("missing committed change notification")
	}
}

func TestCatalogNotificationsFollowSuccessfulWrites(t *testing.T) {
	c := New(storage.NewMemory())
	publications, stopPublications := c.WatchPublications()
	defer stopPublications()
	controls, stopControls := c.WatchControls()
	defer stopControls()
	ctx := context.Background()
	r, err := c.Publish(ctx, "foo", "source", origin("first"), []InputFile{input("index.html", "hello")})
	if err != nil {
		t.Fatal(err)
	}
	assertNoNotice(t, publications) // uploads are not publication boundaries
	assertNoNotice(t, controls)
	if err := c.SaveSite(ctx, siteFor(r)); err != nil {
		t.Fatal(err)
	}
	assertNotice(t, publications)
	assertNoNotice(t, controls)
	if s, err := c.GetSite(ctx, "foo"); err != nil || s.Production != r.ID {
		t.Fatalf("notification preceded publication: %v", err)
	}
	if _, err := c.RequestControl(ctx, "foo", "source", "disabled", ""); err != nil {
		t.Fatal(err)
	}
	assertNotice(t, controls)
	assertNoNotice(t, publications) // only the owner may apply the intent
}

type noticeFailingStore struct{ storage.Store }

func (s noticeFailingStore) Put(context.Context, string, io.Reader, int64, string) (storage.Info, error) {
	return storage.Info{}, errors.New("write failed")
}

func TestFailedWriteDoesNotNotify(t *testing.T) {
	c := New(noticeFailingStore{storage.NewMemory()})
	publications, stop := c.WatchPublications()
	defer stop()
	if err := c.write(context.Background(), SiteKey("foo"), map[string]string{"test": "failed"}); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	assertNoNotice(t, publications)
}

func TestNotificationsBroadcastCoalesceAndUnsubscribe(t *testing.T) {
	c := New(storage.NewMemory())
	first, stopFirst := c.WatchPublications()
	second, stopSecond := c.WatchPublications()
	defer stopSecond()
	controls, stopControls := c.WatchControls()
	defer stopControls()
	for range 100 {
		c.notifyWrite(SiteKey("foo"))
	}
	assertNotice(t, first)
	assertNotice(t, second)
	assertNoNotice(t, first)
	assertNoNotice(t, second)
	assertNoNotice(t, controls)
	stopFirst()
	stopFirst()
	c.notifyWrite(SiteKey("bar"))
	assertNoNotice(t, first)
	assertNotice(t, second)
	for _, key := range []string{"sites/foo/other.json", "sites/invalid.name/site.json", "sources/source/sites/foo/releases/id/manifest.json"} {
		c.notifyWrite(key)
	}
	assertNoNotice(t, second)
	assertNoNotice(t, controls)
}
