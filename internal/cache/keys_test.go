package cache

import (
	"testing"

	"vedetta/internal/search"
)

func TestSearchKey(t *testing.T) {
	base := search.Options{Language: "it", Categories: []string{"news", "general"}, MaxResults: 10}
	k := SearchKey("searxng", "Foo Bar", base)

	same := []struct {
		name  string
		query string
		opts  search.Options
	}{
		{"whitespace and case", "  foo   BAR ", base},
		{"category order", "foo bar", search.Options{Language: "it", Categories: []string{"general", "news"}, MaxResults: 10}},
		{"session and facet ignored", "foo bar", search.Options{Language: "it", Categories: []string{"news", "general"}, MaxResults: 10, SessionID: "s2", Facet: "offbeat"}},
	}
	for _, c := range same {
		if got := SearchKey("searxng", c.query, c.opts); got != k {
			t.Errorf("%s: key changed\n got %s\nwant %s", c.name, got, k)
		}
	}

	different := []struct {
		name     string
		provider string
		query    string
		opts     search.Options
	}{
		{"language", "searxng", "foo bar", search.Options{Language: "en", Categories: []string{"news", "general"}, MaxResults: 10}},
		{"max results", "searxng", "foo bar", search.Options{Language: "it", Categories: []string{"news", "general"}, MaxResults: 20}},
		{"biomedical", "searxng", "foo bar", search.Options{Language: "it", Categories: []string{"news", "general"}, MaxResults: 10, BiomedicalQuery: true}},
		{"authoritative domains", "searxng", "foo bar", search.Options{Language: "it", Categories: []string{"news", "general"}, MaxResults: 10, AuthoritativeDomains: []string{"pubmed.ncbi.nlm.nih.gov"}}},
		{"provider", "brave", "foo bar", base},
		{"word order", "searxng", "bar foo", base},
	}
	for _, c := range different {
		if got := SearchKey(c.provider, c.query, c.opts); got == k {
			t.Errorf("%s: key should differ, both %s", c.name, k)
		}
	}
}

func TestPageKey(t *testing.T) {
	k := PageKey("https://Example.ORG/Path?a=1")
	if got := PageKey("HTTPS://example.org/Path?a=1#section"); got != k {
		t.Errorf("scheme/host case and fragment should not matter: %s vs %s", got, k)
	}
	if got := PageKey("https://example.org/path?a=1"); got == k {
		t.Error("path case must matter")
	}
	if got := PageKey("https://example.org/Path?a=2"); got == k {
		t.Error("query string must matter")
	}
}
