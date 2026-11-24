package reader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPubMedID(t *testing.T) {
	cases := map[string]string{
		"https://pubmed.ncbi.nlm.nih.gov/31452104/":           "31452104",
		"https://pubmed.ncbi.nlm.nih.gov/31452104":            "31452104",
		"https://www.ncbi.nlm.nih.gov/pubmed/12345?dopt=Abst": "12345",
		"https://pmc.ncbi.nlm.nih.gov/articles/PMC6712345/":   "",
		"https://pubmed.ncbi.nlm.nih.gov/?term=melatonin":     "",
		"https://example.com/31452104":                        "",
	}
	for u, want := range cases {
		if got := PubMedID(u); got != want {
			t.Errorf("PubMedID(%q) = %q, want %q", u, got, want)
		}
	}
}

const europePMCFixture = `{"resultList":{"result":[{"pmid":"31452104","doi":"10.1/x","title":"Melatonin for <i>jet lag</i>.","authorString":"Rossi A, Bianchi B.","pubYear":"2019","abstractText":"<h4>Background</h4>Melatonin reduces jet lag &amp; improves sleep.","journalInfo":{"journal":{"title":"Sleep Med"}}}]}}`

func TestEuropePMCReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("query"), "EXT_ID:31452104") {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(europePMCFixture))
	}))
	defer srv.Close()

	doc, err := NewEuropePMCReader(srv.URL).Read(context.Background(), "https://pubmed.ncbi.nlm.nih.gov/31452104/")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Melatonin for jet lag." || doc.URL != "https://pubmed.ncbi.nlm.nih.gov/31452104/" {
		t.Errorf("title/url = %q / %q", doc.Title, doc.URL)
	}
	for _, want := range []string{"Sleep Med", "doi:10.1/x", "Melatonin reduces jet lag & improves sleep."} {
		if !strings.Contains(doc.Content, want) {
			t.Errorf("content missing %q:\n%s", want, doc.Content)
		}
	}
}

type stubReader struct {
	name  string
	doc   *Document
	calls int
}

func (s *stubReader) Name() string { return s.name }
func (s *stubReader) Read(ctx context.Context, u string) (*Document, error) {
	s.calls++
	return s.doc, nil
}

func TestCompositeReader_PubMedRouting(t *testing.T) {
	page := &stubReader{name: "http", doc: &Document{URL: "x", Content: strings.Repeat("pagina normale ", 40)}}
	api := &stubReader{name: "europepmc", doc: &Document{URL: "x", Content: "Abstract text"}}
	c := NewCompositeReader(CompositeReaderConfig{Chain: []Reader{page}, PubMed: api})

	if doc, _ := c.Read(context.Background(), "https://pubmed.ncbi.nlm.nih.gov/1/"); doc.Content != "Abstract text" || page.calls != 0 {
		t.Errorf("PubMed URL should be read via the API only (page calls %d)", page.calls)
	}
	if _, _ = c.Read(context.Background(), "https://example.com/a"); page.calls != 1 || api.calls != 1 {
		t.Errorf("other URLs must use the chain (page %d, api %d)", page.calls, api.calls)
	}
}
