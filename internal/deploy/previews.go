package deploy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	PreviewAliasQualified = "qualified"
	PreviewAliasTagFirst  = "tag-first"
)

// PreviewRoute projects an existing publication alias to a reader hostname.
// Token remains the durable alias key; naming modes never rewrite Storage.
type PreviewRoute struct {
	Site    string
	Token   string
	Alias   Alias
	Host    string
	Primary bool
}

// PreviewProblem explains why an alias has no short URL. Its qualified URL is
// still served, including a branch shadowed by a tag at the short hostname.
type PreviewProblem struct {
	Site      string `json:"site"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	LegacyURL string `json:"qualifiedURL"`
	Problem   string `json:"problem"`
}

type PreviewPlan struct {
	Routes   []PreviewRoute
	Problems []PreviewProblem
}

func PreviewAliasMode(mode string) (string, error) {
	if mode == "" {
		return PreviewAliasQualified, nil
	}
	if mode != PreviewAliasQualified && mode != PreviewAliasTagFirst {
		return "", errors.New("preview_alias_mode must be qualified or tag-first")
	}
	return mode, nil
}

// PlanPreviews is shared by HTTP serving and status output. Production and
// qualified hostnames are reserved globally before readable aliases are added.
// A bad short name affects only that short URL, never another site's routes.
func PlanPreviews(sites []*Site, baseDomain, style, mode string) (PreviewPlan, error) {
	mode, err := PreviewAliasMode(mode)
	if err != nil {
		return PreviewPlan{}, err
	}
	if style == "" {
		style = "flat"
	}
	if style != "flat" && style != "nested" {
		return PreviewPlan{}, errors.New("preview_style must be flat or nested")
	}
	type owner struct{ site, token string }
	occupied := map[string]owner{}
	reserved := map[string]owner{}
	plan := PreviewPlan{}
	ordered := append([]*Site(nil), sites...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	for _, s := range ordered {
		if !ValidName(s.Name) {
			return plan, errors.New("invalid site name")
		}
		host := Host(baseDomain, style, s.Name, "")
		if _, exists := reserved[host]; exists {
			return plan, errors.New("duplicate production hostname")
		}
		// Disabled and empty sites retain ownership of their production name.
		reserved[host] = owner{s.Name, ""}
		if !s.Disabled && s.Production != "" {
			occupied[host] = owner{s.Name, ""}
		}
	}
	type candidate struct {
		route PreviewRoute
		rank  int
	}
	groups := map[string][]candidate{}
	problem := func(r PreviewRoute, why string) {
		plan.Problems = append(plan.Problems, PreviewProblem{r.Site, r.Alias.Kind, r.Alias.Name, "https://" + r.Host, why})
	}
	for _, s := range ordered {
		if s.Disabled {
			continue
		}
		tokens := make([]string, 0, len(s.Aliases))
		for token := range s.Aliases {
			tokens = append(tokens, token)
		}
		sort.Strings(tokens)
		for _, token := range tokens {
			if !ValidName(token) {
				return plan, errors.New("invalid preview token")
			}
			a := s.Aliases[token]
			r := PreviewRoute{s.Name, token, a, Host(baseDomain, style, s.Name, token), mode == PreviewAliasQualified || a.Kind == "commit"}
			if _, exists := occupied[r.Host]; exists {
				return plan, errors.New("duplicate qualified hostname")
			}
			occupied[r.Host] = owner{s.Name, token}
			if _, exists := reserved[r.Host]; !exists {
				reserved[r.Host] = owner{s.Name, token}
			}
			plan.Routes = append(plan.Routes, r)
			if mode != PreviewAliasTagFirst || (a.Kind != "tag" && a.Kind != "branch") {
				continue
			}
			name, rank := strings.TrimPrefix(a.Name, "refs/heads/"), 1
			if a.Kind == "tag" {
				name, rank = strings.TrimPrefix(a.Name, "refs/tags/"), 2
			}
			token := dnsSlug(name)
			if !ValidName(token) {
				problem(r, "ref has no DNS-safe short label of at most 63 characters")
				continue
			}
			label := token
			if style == "flat" {
				label += "--" + s.Name
			}
			if !ValidName(label) {
				problem(r, "short preview hostname exceeds the 63-character DNS label limit")
				continue
			}
			r.Primary = true
			r.Host = Host(baseDomain, style, s.Name, token)
			if len(r.Host) > 253 {
				problem(plan.Routes[len(plan.Routes)-1], "short preview hostname exceeds the 253-character DNS name limit")
				continue
			}
			groups[r.Host] = append(groups[r.Host], candidate{r, rank})
		}
	}
	if mode == PreviewAliasTagFirst {
		hosts := make([]string, 0, len(groups))
		for host := range groups {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		for _, host := range hosts {
			candidates := groups[host]
			rank := 0
			for _, c := range candidates {
				if c.rank > rank {
					rank = c.rank
				}
			}
			winners := []candidate{}
			for _, c := range candidates {
				if c.rank == rank {
					winners = append(winners, c)
				}
			}
			why := ""
			if len(winners) != 1 {
				why = fmt.Sprintf("ambiguous short %s hostname after DNS normalization", winners[0].route.Alias.Kind)
			} else if own, exists := reserved[host]; exists && own != (owner{winners[0].route.Site, winners[0].route.Token}) {
				why = "short hostname conflicts with a production or qualified preview hostname"
			}
			for _, c := range candidates {
				legacy := c.route
				legacy.Host = Host(baseDomain, style, c.route.Site, c.route.Token)
				if why != "" {
					problem(legacy, why)
				} else if c.rank != rank {
					problem(legacy, "a published tag takes precedence at the short hostname")
				}
			}
			if why != "" {
				continue
			}
			winner := winners[0].route
			if own, exists := occupied[host]; exists && own == (owner{winner.Site, winner.Token}) {
				for i := range plan.Routes {
					if plan.Routes[i].Host == host {
						plan.Routes[i].Primary = true
					}
				}
			} else {
				plan.Routes = append(plan.Routes, winner)
			}
		}
	}
	return plan, nil
}
