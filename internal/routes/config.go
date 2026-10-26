// Package routes parses and resolves release-scoped pages.json configuration.
// It has no storage or HTTP server dependency; publishers and readers use the
// same validation and matching rules.
package routes

type Config struct {
	SchemaVersion int          `json:"schemaVersion"`
	Routing       Routing      `json:"routing,omitempty"`
	Headers       []HeaderRule `json:"headers,omitempty"`
	Assets        Assets       `json:"assets,omitempty"`
}

type Routing struct {
	CleanURLs     bool              `json:"cleanUrls,omitempty"`
	Index         string            `json:"index,omitempty"`
	TrailingSlash string            `json:"trailingSlash,omitempty"`
	Routes        []Rule            `json:"routes,omitempty"`
	Fallback      *Fallback         `json:"fallback,omitempty"`
	ErrorPages    map[string]string `json:"errorPages,omitempty"`
}

type Rule struct {
	From     string `json:"from"`
	Rewrite  string `json:"rewrite,omitempty"`
	Redirect string `json:"redirect,omitempty"`
	Status   int    `json:"status,omitempty"`
}

type Fallback struct {
	Rewrite string   `json:"rewrite"`
	Exclude []string `json:"exclude,omitempty"`
}

type HeaderRule struct {
	From string            `json:"from"`
	Set  map[string]string `json:"set"`
}

type Assets struct {
	Immutable []string `json:"immutable,omitempty"`
}

// Decision describes one nonrecursive lookup or redirect. Path is relative to
// the published release and empty for bodyless errors or redirects. Headers
// were matched against the original request path, never its rewritten target.
type Decision struct {
	Path     string
	Status   int
	Redirect string
	Headers  map[string]string
	Action   string
}

const (
	ActionFile     = "file"
	ActionRewrite  = "rewrite"
	ActionRedirect = "redirect"
	ActionFallback = "fallback"
	ActionNotFound = "not_found"
)
