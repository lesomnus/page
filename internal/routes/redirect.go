package routes

import "strings"

// A redirect can be followed by another redirect only when its output overlaps
// the next rule's input. Require that dependency graph to be acyclic. Captures
// are deliberately approximated independently: a policy that appears cyclic
// is rejected even if particular capture values would happen to terminate.
// This avoids accepting loops that a few sampled capture values cannot reveal.
func (e *Engine) checkRedirectDependencies() error {
	graph := make([][]int, len(e.rules))
	for i, from := range e.rules {
		if from.rule.Redirect == "" {
			continue
		}
		target := templateLanguage(from.rule.Redirect)
		for j, to := range e.rules {
			if to.rule.Redirect != "" && languagesOverlap(target, patternLanguage(to.pattern)) {
				graph[i] = append(graph[i], j)
			}
			// Rules are ordered. Once one rule covers every possible output,
			// subsequent rules cannot handle a redirected request.
			if patternCovers(to.pattern, target) {
				break
			}
		}
	}
	state := make([]uint8, len(graph))
	var visit func(int) bool
	visit = func(i int) bool {
		if state[i] == 1 {
			return true
		}
		if state[i] == 2 {
			return false
		}
		state[i] = 1
		for _, next := range graph[i] {
			if visit(next) {
				return true
			}
		}
		state[i] = 2
		return false
	}
	for i := range graph {
		if visit(i) {
			return invalid("redirect dependencies may form a cycle involving %q", e.rules[i].rule.From)
		}
	}
	return nil
}

type languageSegment struct {
	literal, template string
	wild              bool
}

func templateLanguage(target string) []languageSegment {
	if target == "/" {
		return nil
	}
	parts := strings.Split(target[1:], "/")
	result := make([]languageSegment, len(parts))
	for i, part := range parts {
		switch {
		case part == "*":
			result[i].wild = true
		case reference.MatchString(part):
			result[i].template = part
		default:
			result[i].literal = part
		}
	}
	return result
}

func patternLanguage(p pattern) []languageSegment {
	result := make([]languageSegment, len(p.segments))
	for i, part := range p.segments {
		switch {
		case part.wild:
			result[i].wild = true
		case part.name != "":
			result[i].template = ":capture"
		default:
			result[i].literal = part.literal
		}
	}
	return result
}

func segmentsOverlap(a, b languageSegment) bool {
	if a.template == "" && b.template == "" {
		return a.literal == b.literal
	}
	if a.template != "" && b.template != "" {
		return true
	}
	if a.template == "" {
		a, b = b, a
	}
	re, err := templateRegexp(a.template)
	return err == nil && re.MatchString(b.literal)
}

func languagesOverlap(a, b []languageSegment) bool {
	type position struct{ a, b int }
	seen := map[position]bool{}
	var match func(int, int) bool
	match = func(i, j int) bool {
		key := position{i, j}
		if seen[key] {
			return false
		}
		seen[key] = true
		if i == len(a) && j == len(b) {
			return true
		}
		if i < len(a) && a[i].wild {
			if match(i+1, j) {
				return true
			}
			if j < len(b) && !b[j].wild && match(i, j+1) {
				return true
			}
		}
		if j < len(b) && b[j].wild {
			if match(i, j+1) {
				return true
			}
			if i < len(a) && !a[i].wild && match(i+1, j) {
				return true
			}
		}
		return i < len(a) && j < len(b) && !a[i].wild && !b[j].wild && segmentsOverlap(a[i], b[j]) && match(i+1, j+1)
	}
	return match(0, 0)
}

func patternCovers(p pattern, target []languageSegment) bool {
	for i, s := range p.segments {
		if s.wild {
			return true
		}
		if i >= len(target) || target[i].wild {
			return false
		}
		t := target[i]
		if s.name != "" {
			if t.template == "" && t.literal == "" {
				return false
			}
		} else if t.template != "" || t.literal != s.literal {
			return false
		}
	}
	return len(p.segments) == len(target)
}
