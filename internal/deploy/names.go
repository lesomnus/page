package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// AliasToken is a DNS-safe, length-bounded lookup name. The original Git ref or
// source-specific name is kept in Alias.Name; this token need not be reversible.
func AliasToken(kind, name string) string {
	prefix := map[string]string{"branch": "b", "tag": "t", "commit": "c"}[kind]
	if prefix == "" {
		prefix = "p"
	}
	return prefix + "-" + boundedLabel(name, kind+"\x00"+name, 38)
}

// Host returns a production or preview hostname. Flat previews share a single
// wildcard certificate. Nested previews need a certificate covering *.site.base.
func Host(baseDomain, style, site, token string) string {
	baseDomain = strings.TrimSuffix(strings.ToLower(baseDomain), ".")
	if token == "" {
		return site + "." + baseDomain
	}
	if style == "nested" {
		return token + "." + site + "." + baseDomain
	}
	label := token + "--" + site
	if len(label) > 63 {
		label = boundedLabel(label, label, 63)
	}
	return label + "." + baseDomain
}

func boundedLabel(value, identity string, limit int) string {
	var b strings.Builder
	for _, c := range strings.ToLower(value) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "ref"
	}
	h := sha256.Sum256([]byte(identity))
	suffix := hex.EncodeToString(h[:6])
	if len(slug) > limit-len(suffix)-1 {
		slug = strings.TrimRight(slug[:limit-len(suffix)-1], "-")
	}
	return slug + "-" + suffix
}
