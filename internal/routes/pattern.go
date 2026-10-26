package routes

import (
	"regexp"
	"strings"
)

var parameter = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var reference = regexp.MustCompile(`:([A-Za-z_][A-Za-z0-9_]*)|\*`)

type segment struct {
	literal, name string
	wild          bool
}
type pattern struct {
	raw      string
	segments []segment
	names    map[string]bool
}

func newPattern(raw string) (pattern, error) {
	p := pattern{raw: raw, names: map[string]bool{}}
	if !safeAbsolute(raw) {
		return p, invalid("unsafe pattern %q", raw)
	}
	if raw == "/" {
		return p, nil
	}
	parts := strings.Split(raw[1:], "/")
	for i, s := range parts {
		x := segment{literal: s}
		if s == "*" {
			if i != len(parts)-1 {
				return p, invalid("wildcard must be the last segment")
			}
			x = segment{name: "*", wild: true}
			p.names["*"] = true
		} else if strings.HasPrefix(s, ":") {
			name := s[1:]
			if !parameter.MatchString(name) || p.names[name] {
				return p, invalid("invalid or duplicate capture %q", s)
			}
			x = segment{name: name}
			p.names[name] = true
		} else if strings.ContainsAny(s, ":*") {
			return p, invalid("captures and wildcards occupy a whole pattern segment")
		}
		p.segments = append(p.segments, x)
	}
	return p, nil
}

func (p pattern) match(requestPath string) (map[string]string, bool) {
	parts := []string{}
	if requestPath != "/" {
		parts = strings.Split(strings.TrimPrefix(requestPath, "/"), "/")
	}
	captures := map[string]string{}
	for i, s := range p.segments {
		if s.wild {
			if i > len(parts) {
				return nil, false
			}
			captures["*"] = strings.Join(parts[i:], "/")
			return captures, true
		}
		if i >= len(parts) {
			return nil, false
		}
		if s.name != "" {
			if parts[i] == "" {
				return nil, false
			}
			captures[s.name] = parts[i]
		} else if s.literal != parts[i] {
			return nil, false
		}
	}
	return captures, len(parts) == len(p.segments)
}

func (p pattern) sample() string {
	if len(p.segments) == 0 {
		return "/"
	}
	parts := make([]string, 0, len(p.segments))
	for _, s := range p.segments {
		if s.name != "" {
			parts = append(parts, "sample")
		} else {
			parts = append(parts, s.literal)
		}
	}
	return "/" + strings.Join(parts, "/")
}

func validateTemplate(target string, names map[string]bool) error {
	if !safeAbsolute(target) {
		return invalid("unsafe target %q", target)
	}
	for _, match := range reference.FindAllStringSubmatchIndex(target, -1) {
		token := target[match[0]:match[1]]
		name := "*"
		if token != "*" {
			name = token[1:]
		}
		if !names[name] {
			return invalid("target uses unknown capture %q", token)
		}
		if token == "*" {
			before := match[0] - 1
			after := match[1]
			if (before >= 0 && target[before] != '/') || (after < len(target) && target[after] != '/') {
				return invalid("wildcard output must occupy a path segment")
			}
		}
	}
	// Every colon is a capture marker. A malformed marker must not silently
	// become a literal URL that differs from what the author intended.
	remaining := reference.ReplaceAllString(target, "")
	if strings.ContainsAny(remaining, ":*") {
		return invalid("malformed target capture")
	}
	return nil
}

func expand(template string, captures map[string]string) string {
	return reference.ReplaceAllStringFunc(template, func(token string) string {
		if token == "*" {
			return captures["*"]
		}
		return captures[token[1:]]
	})
}

func templateRegexp(template string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	at := 0
	for _, m := range reference.FindAllStringIndex(template, -1) {
		b.WriteString(regexp.QuoteMeta(template[at:m[0]]))
		if template[m[0]:m[1]] == "*" {
			b.WriteString(".*")
		} else {
			b.WriteString("[^/]+")
		}
		at = m[1]
	}
	b.WriteString(regexp.QuoteMeta(template[at:]))
	b.WriteString("$")
	return regexp.Compile(b.String())
}
