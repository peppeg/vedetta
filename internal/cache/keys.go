package cache

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"vedetta/internal/search"
)

// keyVersion prefixes every key: bump it to invalidate all entries.
const keyVersion = "v1"

// normalizeQuery lowercases and collapses whitespace, nothing else: two
// different queries stay different.
func normalizeQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

func sortedLower(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// SearchKey identifies a search by provider, normalized query and the options
// that change results. SessionID and Facet are left out: no provider reads them.
func SearchKey(provider, query string, opts search.Options) string {
	return fmt.Sprintf("%s|search|%s|%q|lang=%s|cat=%s|max=%d|bio=%t|auth=%s",
		keyVersion,
		strings.ToLower(strings.TrimSpace(provider)),
		normalizeQuery(query),
		strings.ToLower(strings.TrimSpace(opts.Language)),
		strings.Join(sortedLower(opts.Categories), ","),
		opts.MaxResults,
		opts.BiomedicalQuery,
		strings.Join(sortedLower(opts.AuthoritativeDomains), ","),
	)
}

// PageKey identifies a page by URL with lowercase scheme and host and no
// fragment. The reader is not part of the key: a page is the same page
// whoever reads it.
func PageKey(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	u, err := url.Parse(raw)
	if err != nil {
		return keyVersion + "|page|" + raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""
	return keyVersion + "|page|" + u.String()
}
