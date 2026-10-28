package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	MaxConfigBytes = 64 << 10
	MaxRules       = 256
)

var ErrInvalid = errors.New("invalid pages.json")

// Parse rejects unknown fields and trailing JSON so a misspelled policy is
// never accepted as though it were enforced.
func Parse(data []byte) (*Config, error) {
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrInvalid, MaxConfigBytes)
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%w: expected one JSON document", ErrInvalid)
	}
	if c.SchemaVersion != 1 {
		return nil, fmt.Errorf("%w: schemaVersion must be 1", ErrInvalid)
	}
	return &c, nil
}

type compiledRule struct {
	pattern pattern
	rule    Rule
}
type compiledHeader struct {
	pattern pattern
	values  map[string]string
}

type Engine struct {
	config    Config
	files     map[string]bool
	rules     []compiledRule
	headers   []compiledHeader
	excludes  []pattern
	immutable []pattern
}

// Compile validates a config against one immutable release's file inventory.
// A nil config has the default static-file behavior.
func Compile(config *Config, files map[string]bool) (*Engine, error) {
	return compile(config, files, true)
}

// CompileRuntime validates routing syntax without loading a release inventory.
// File existence is checked by ResolveLookup while handling each request.
// The publisher still uses Compile for inventory-dependent validation, including
// redirect cycles, implicit canonical redirects and forbidden immutable file types.
// Runtime resolution returns one redirect without following it; repeating global
// cycle analysis per request would add quadratic work to published configurations.
func CompileRuntime(config *Config) (*Engine, error) {
	return compile(config, nil, false)
}

func compile(config *Config, files map[string]bool, inventory bool) (*Engine, error) {
	c := Config{SchemaVersion: 1}
	if config != nil {
		b, err := json.Marshal(config)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		var parsed *Config
		parsed, err = Parse(b)
		if err != nil {
			return nil, err
		}
		c = *parsed
	}
	if c.Routing.Index == "" {
		c.Routing.Index = "index.html"
	}
	if !safeRelative(c.Routing.Index) || strings.Contains(c.Routing.Index, "/") {
		return nil, invalid("routing.index must be a file name")
	}
	if c.Routing.TrailingSlash == "" {
		c.Routing.TrailingSlash = "preserve"
	}
	if c.Routing.TrailingSlash != "preserve" && c.Routing.TrailingSlash != "append" && c.Routing.TrailingSlash != "remove" {
		return nil, invalid("unsupported trailingSlash")
	}
	total := len(c.Routing.Routes) + len(c.Headers) + len(c.Assets.Immutable)
	if c.Routing.Fallback != nil {
		total += len(c.Routing.Fallback.Exclude)
	}
	if total > MaxRules {
		return nil, invalid("too many rules")
	}
	e := &Engine{config: c}
	if inventory {
		e.files = make(map[string]bool, len(files))
	}
	for p, present := range files {
		if !safeRelative(p) {
			return nil, invalid("unsafe release file %q", p)
		}
		if present {
			e.files[p] = true
		}
	}
	seen := map[string]bool{}
	for _, r := range c.Routing.Routes {
		p, err := newPattern(r.From)
		if err != nil {
			return nil, err
		}
		if seen[r.From] {
			return nil, invalid("duplicate route %q", r.From)
		}
		seen[r.From] = true
		if (r.Rewrite == "") == (r.Redirect == "") {
			return nil, invalid("route %q needs exactly one rewrite or redirect", r.From)
		}
		target := r.Rewrite
		if r.Redirect != "" {
			target = r.Redirect
			if r.Status == 0 {
				r.Status = 302
			}
			if r.Status != 301 && r.Status != 302 && r.Status != 307 && r.Status != 308 {
				return nil, invalid("unsupported redirect status")
			}
		} else if r.Status != 0 {
			return nil, invalid("rewrite cannot set status")
		}
		if err = validateTemplate(target, p.names); err != nil {
			return nil, err
		}
		if inventory && r.Rewrite != "" && !e.hasTarget(target) {
			return nil, invalid("rewrite target %q does not match a release file", target)
		}
		e.rules = append(e.rules, compiledRule{p, r})
	}
	if f := c.Routing.Fallback; f != nil {
		if err := validateTemplate(f.Rewrite, nil); err != nil {
			return nil, err
		}
		if inventory && !e.files[strings.TrimPrefix(f.Rewrite, "/")] {
			return nil, invalid("fallback target %q is missing", f.Rewrite)
		}
		for _, raw := range f.Exclude {
			p, err := newPattern(raw)
			if err != nil {
				return nil, err
			}
			e.excludes = append(e.excludes, p)
		}
	}
	for status, target := range c.Routing.ErrorPages {
		if status != "404" {
			return nil, invalid("only errorPages.404 is supported")
		}
		if err := validateTemplate(target, nil); err != nil {
			return nil, err
		}
		if inventory && !e.files[strings.TrimPrefix(target, "/")] {
			return nil, invalid("error page %q is missing", target)
		}
	}
	for _, h := range c.Headers {
		p, err := newPattern(h.From)
		if err != nil {
			return nil, err
		}
		if len(h.Set) == 0 {
			return nil, invalid("header rule needs set values")
		}
		values := map[string]string{}
		for name, value := range h.Set {
			canonical, err := header(name, value)
			if err != nil {
				return nil, err
			}
			if _, exists := values[canonical]; exists {
				return nil, invalid("duplicate header name %q", name)
			}
			values[canonical] = value
		}
		e.headers = append(e.headers, compiledHeader{p, values})
	}
	for _, raw := range c.Assets.Immutable {
		p, err := newPattern(raw)
		if err != nil {
			return nil, err
		}
		for filename := range e.files {
			if matchesPath(p, "/"+filename) {
				ext := strings.ToLower(path.Ext(filename))
				if ext == ".html" || ext == ".htm" || ext == ".xhtml" || strings.EqualFold(path.Base(filename), "pages.json") {
					return nil, invalid("immutable assets may not include HTML or pages.json: %s", filename)
				}
			}
		}
		e.immutable = append(e.immutable, p)
	}
	if inventory {
		if err := e.checkRedirects(); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func safeRelative(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "/") && safeAbsolute("/"+p) && !strings.HasSuffix(p, "/")
}
func safeAbsolute(p string) bool {
	if len(p) == 0 || len(p) > 4096 || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || !utf8.ValidString(p) || strings.ContainsAny(p, "\\\x00\r\n\t%?#") {
		return false
	}
	if p == "/" {
		return true
	}
	parts := strings.Split(p[1:], "/")
	for i, s := range parts {
		if s == "." || s == ".." || (s == "" && i < len(parts)-1) {
			return false
		}
		for _, r := range s {
			if r < 32 || r == 127 {
				return false
			}
		}
	}
	return true
}

// Resolve matches rules and headers against an already URL-decoded path.
// Rewrites are one lookup, never another pass through the route list.
func (e *Engine) Resolve(requestPath string, navigation bool) Decision {
	d, _ := e.ResolveLookup(requestPath, navigation, func(p string) (bool, error) { return e.files[p], nil })
	return d
}

// ResolveLookup reads only the candidate file paths needed for this request.
// Lookup failures propagate rather than looking like absent files.
func (e *Engine) ResolveLookup(requestPath string, navigation bool, exists func(string) (bool, error)) (Decision, error) {
	headers := map[string]string{}
	if !safeAbsolute(requestPath) {
		return Decision{Status: 400, Headers: headers, Action: ActionNotFound}, nil
	}
	lookup := func(p string) (bool, error) {
		if !safeRelative(p) {
			return false, nil
		}
		if exists == nil {
			return false, errors.New("routes: no file lookup")
		}
		return exists(p)
	}
	notFound := func(action string) (Decision, error) {
		d := e.notFound(headers, action)
		if d.Path != "" {
			present, err := lookup(d.Path)
			if err != nil {
				return Decision{}, err
			}
			if !present {
				d.Path = ""
			}
		}
		return d, nil
	}
	for _, h := range e.headers {
		if matchesPath(h.pattern, requestPath) {
			for k, v := range h.values {
				headers[k] = v
			}
		}
	}
	for _, r := range e.rules {
		if !matchesPath(r.pattern, requestPath) {
			continue
		}
		captures, _ := r.pattern.match(requestPath)
		target := expand(r.rule.Rewrite, captures)
		if r.rule.Redirect != "" {
			target = expand(r.rule.Redirect, captures)
			if !safeAbsolute(target) {
				return notFound(ActionRewrite)
			}
			return Decision{Status: r.rule.Status, Redirect: target, Headers: headers, Action: ActionRedirect}, nil
		}
		if !safeAbsolute(target) {
			return notFound(ActionRewrite)
		}
		file := strings.TrimPrefix(target, "/")
		present, err := lookup(file)
		if err != nil {
			return Decision{}, err
		}
		if !present {
			return notFound(ActionRewrite)
		}
		return Decision{Path: file, Status: 200, Headers: headers, Action: ActionRewrite}, nil
	}
	file := strings.TrimPrefix(requestPath, "/")
	present, err := lookup(file)
	if err != nil {
		return Decision{}, err
	}
	if present {
		return Decision{Path: file, Status: 200, Headers: headers, Action: ActionFile}, nil
	}
	trimmed := strings.TrimSuffix(file, "/")
	if e.config.Routing.CleanURLs && trimmed != "" && path.Ext(trimmed) == "" {
		present, err := lookup(trimmed + ".html")
		if err != nil {
			return Decision{}, err
		}
		if present {
			if redirect := e.canonical(requestPath, false); redirect != "" {
				return Decision{Status: 301, Redirect: redirect, Headers: headers, Action: ActionRedirect}, nil
			}
			return Decision{Path: trimmed + ".html", Status: 200, Headers: headers, Action: ActionFile}, nil
		}
	}
	index := trimmed
	if index != "" {
		index += "/"
	}
	index += e.config.Routing.Index
	present, err = lookup(index)
	if err != nil {
		return Decision{}, err
	}
	if present {
		if redirect := e.canonical(requestPath, true); redirect != "" {
			return Decision{Status: 301, Redirect: redirect, Headers: headers, Action: ActionRedirect}, nil
		}
		return Decision{Path: index, Status: 200, Headers: headers, Action: ActionFile}, nil
	}
	if f := e.config.Routing.Fallback; navigation && f != nil {
		excluded := false
		for _, p := range e.excludes {
			if matchesPath(p, requestPath) {
				excluded = true
				break
			}
		}
		if !excluded {
			file := strings.TrimPrefix(f.Rewrite, "/")
			present, err := lookup(file)
			if err != nil {
				return Decision{}, err
			}
			if present {
				return Decision{Path: file, Status: 200, Headers: headers, Action: ActionFallback}, nil
			}
		}
	}
	return notFound(ActionNotFound)
}

func (e *Engine) canonical(p string, directory bool) string {
	if p == "/" {
		return ""
	}
	slash := strings.HasSuffix(p, "/")
	switch e.config.Routing.TrailingSlash {
	case "append":
		if !slash {
			return p + "/"
		}
	case "remove":
		if slash {
			return strings.TrimSuffix(p, "/")
		}
	case "preserve":
		if directory && !slash {
			return p + "/"
		}
	}
	return ""
}
func (e *Engine) notFound(headers map[string]string, action string) Decision {
	return Decision{Path: strings.TrimPrefix(e.config.Routing.ErrorPages["404"], "/"), Status: 404, Headers: headers, Action: action}
}

func (e *Engine) Immutable(filename string) bool {
	p := filename
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !safeAbsolute(p) {
		return false
	}
	for _, pattern := range e.immutable {
		if matchesPath(pattern, p) {
			return true
		}
	}
	return false
}

// MatchImmutable checks a previously published asset policy without compiling
// its unrelated routing rules. File type restrictions remain the publisher's
// and serving file envelope's responsibility.
func MatchImmutable(config *Config, filename string) (bool, error) {
	if config == nil {
		return false, nil
	}
	if len(config.Assets.Immutable) > MaxRules {
		return false, invalid("too many rules")
	}
	if !strings.HasPrefix(filename, "/") {
		filename = "/" + filename
	}
	if !safeAbsolute(filename) {
		return false, nil
	}
	matched := false
	for _, raw := range config.Assets.Immutable {
		p, err := newPattern(raw)
		if err != nil {
			return false, err
		}
		matched = matchesPath(p, filename) || matched
	}
	return matched, nil
}

// Matching headers and excludes needs no captures or path segment slices.
// Route captures are allocated only after a pattern has actually matched.
func matchesPath(p pattern, requestPath string) bool {
	rest := strings.TrimPrefix(requestPath, "/")
	remaining := requestPath != "/"
	for _, s := range p.segments {
		if s.wild {
			return true
		}
		if !remaining {
			return false
		}
		part, next, more := strings.Cut(rest, "/")
		if s.name != "" {
			if part == "" {
				return false
			}
		} else if s.literal != part {
			return false
		}
		rest, remaining = next, more
	}
	return !remaining
}

func (e *Engine) hasTarget(target string) bool {
	re, err := templateRegexp(target)
	if err != nil {
		return false
	}
	for filename := range e.files {
		if re.MatchString("/" + filename) {
			return true
		}
	}
	return false
}

func (e *Engine) checkRedirects() error {
	if err := e.checkRedirectDependencies(); err != nil {
		return err
	}
	seeds := []string{}
	for _, r := range e.rules {
		if r.rule.Redirect != "" {
			seeds = append(seeds, r.pattern.sample())
			if !strings.ContainsAny(r.rule.Redirect, ":*") {
				seeds = append(seeds, r.rule.Redirect)
			}
		}
	}
	for _, start := range seeds {
		seen := map[string]bool{}
		p := start
		for count := 0; count <= len(e.rules)+4; count++ {
			if seen[p] {
				return invalid("redirect cycle involving %q", p)
			}
			seen[p] = true
			d := e.Resolve(p, false)
			if d.Redirect == "" {
				break
			}
			if count == len(e.rules)+4 {
				return invalid("redirect chain does not terminate at %q", start)
			}
			p = d.Redirect
		}
	}
	return nil
}

func header(name, value string) (string, error) {
	if name == "" {
		return "", invalid("empty header name")
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return "", invalid("invalid header name %q", name)
		}
	}
	if !utf8.ValidString(value) {
		return "", invalid("invalid header value")
	}
	for _, c := range value {
		if (c < 32 && c != '\t') || c == 127 {
			return "", invalid("invalid header value")
		}
	}
	canonical := http.CanonicalHeaderKey(name)
	switch canonical {
	case "Host", "Content-Length", "Transfer-Encoding", "Connection", "Set-Cookie", "Content-Type", "Content-Range", "Content-Encoding", "Accept-Ranges", "Etag", "Last-Modified", "Date", "Trailer", "Keep-Alive", "Upgrade", "Te", "Proxy-Authenticate", "Proxy-Authorization", "Location":
		return "", invalid("header %q is controlled by the server", name)
	}
	if canonical == "X-Content-Type-Options" && strings.TrimSpace(strings.ToLower(value)) != "nosniff" {
		return "", invalid("X-Content-Type-Options must be nosniff")
	}
	return canonical, nil
}
