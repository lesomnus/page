// Package deploy defines the durable publication contract shared by writers and readers.
package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/page/internal/routes"
	"github.com/lesomnus/page/internal/storage"
)

const SchemaVersion = 1

var (
	ErrInvalid    = errors.New("invalid publication metadata")
	ErrOwnership  = errors.New("site belongs to another source")
	ErrReferenced = errors.New("release is still referenced")
	labelPattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

type Origin struct {
	Kind         string `json:"kind"`
	RepositoryID string `json:"repositoryId"`
	Repository   string `json:"repository"`
	Revision     string `json:"revision"`
	Directory    string `json:"directory"`
}

type File struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	SHA256      string `json:"sha256"`
}

type Release struct {
	SchemaVersion int             `json:"schemaVersion"`
	ID            string          `json:"id"`
	Site          string          `json:"site"`
	Owner         string          `json:"owner"`
	Origin        Origin          `json:"origin"`
	CreatedAt     time.Time       `json:"createdAt"`
	Complete      bool            `json:"complete"`
	Files         map[string]File `json:"files"`
	Pages         *routes.Config  `json:"pages,omitempty"`
}

// Alias is a source-specific name resolved to an already published snapshot.
type Alias struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Release  string `json:"release"`
}

type Site struct {
	SchemaVersion    int                  `json:"schemaVersion"`
	Name             string               `json:"name"`
	Owner            string               `json:"owner"`
	RepositoryID     string               `json:"repositoryId"`
	Repository       string               `json:"repository"`
	Production       string               `json:"production"`
	Aliases          map[string]Alias     `json:"aliases"`
	Retired          map[string]time.Time `json:"retired,omitempty"`
	UpdatedAt        time.Time            `json:"updatedAt"`
	CreatedAt        time.Time            `json:"createdAt"`
	History          []string             `json:"history,omitempty"`
	AssetReleases    map[string]time.Time `json:"assetReleases,omitempty"`
	AssetRetention   time.Duration        `json:"assetRetention,omitempty"`
	Disabled         bool                 `json:"disabled,omitempty"`
	ControlMode      string               `json:"controlMode,omitempty"`
	ControlAppliedAt time.Time            `json:"controlAppliedAt,omitempty"`
}

// Control is operator intent, separate from the sync's derived publication.
type Control struct {
	SchemaVersion int       `json:"schemaVersion"`
	Owner         string    `json:"owner"`
	RepositoryID  string    `json:"repositoryId"`
	Mode          string    `json:"mode"`
	Target        string    `json:"target,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type InputFile struct {
	Path        string
	Size        int64
	ContentType string
	Open        func() (io.ReadCloser, error)
}

type Catalog struct {
	Store    storage.Store
	mutation sync.Mutex
	changes  changeHub
}

func New(store storage.Store) *Catalog { return &Catalog{Store: store} }
func ValidName(s string) bool          { return len(s) > 0 && len(s) <= 63 && labelPattern.MatchString(s) }
func ValidID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil && s == strings.ToLower(s)
}
func ReleaseID(owner, site string, origin Origin) string {
	h := sha256.Sum256([]byte(strings.Join([]string{owner, site, origin.Kind, origin.RepositoryID, origin.Revision, origin.Directory}, "\x00")))
	return hex.EncodeToString(h[:])
}
func SiteKey(name string) string { return "sites/" + name + "/site.json" }
func ReleasePrefix(owner, site, id string) string {
	return "sources/" + owner + "/sites/" + site + "/releases/" + id + "/"
}
func FileKey(r *Release, p string) string { return ReleasePrefix(r.Owner, r.Site, r.ID) + "files/" + p }

// These capabilities deliberately contain no SQL driver or transaction.
type Reader interface {
	GetSite(context.Context, string) (*Site, error)
	ListSites(context.Context) ([]*Site, error)
	GetRelease(context.Context, string, string, string) (*Release, error)
}
