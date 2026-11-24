package search

// Snippet is a search result's preview. The old pipeline used previews as
// contextual evidence; since map R1 only the reconciler's type remains, until
// its preview branches are removed.
type Snippet struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
}
