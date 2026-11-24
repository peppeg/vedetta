package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// pubmedURLRegex captures the PMID of a PubMed article page.
var pubmedURLRegex = regexp.MustCompile(`(?i)^https?://(?:www\.)?(?:pubmed\.ncbi\.nlm\.nih\.gov/|ncbi\.nlm\.nih\.gov/pubmed/)(\d{1,9})(?:[/?#]|$)`)

// PubMedID returns the PMID of a PubMed article URL, or "".
func PubMedID(rawURL string) string {
	if m := pubmedURLRegex.FindStringSubmatch(strings.TrimSpace(rawURL)); m != nil {
		return m[1]
	}
	return ""
}

// EuropePMCReader reads a PubMed article through the public Europe PMC REST API
// (title, journal and abstract), keyless (idea I3a). PubMed pages often answer
// automated readers with an interstitial ("Cookies must be enabled"), while the
// same record is served cleanly by Europe PMC.
type EuropePMCReader struct {
	baseURL    string
	httpClient *http.Client
}

// NewEuropePMCReader creates the reader; an empty baseURL uses the public endpoint.
func NewEuropePMCReader(baseURL string) *EuropePMCReader {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://www.ebi.ac.uk/europepmc/webservices/rest/search"
	}
	return &EuropePMCReader{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (e *EuropePMCReader) Name() string { return "europepmc" }

type europePMCCore struct {
	ResultList struct {
		Result []struct {
			PMID         string `json:"pmid"`
			DOI          string `json:"doi"`
			Title        string `json:"title"`
			AuthorString string `json:"authorString"`
			PubYear      string `json:"pubYear"`
			AbstractText string `json:"abstractText"`
			JournalInfo  struct {
				Journal struct {
					Title string `json:"title"`
				} `json:"journal"`
			} `json:"journalInfo"`
		} `json:"result"`
	} `json:"resultList"`
}

var htmlTagRegex = regexp.MustCompile(`<[^>]+>`)

// Read returns the PubMed record of targetURL; it fails for non-PubMed URLs and
// for records without an abstract, so the caller falls back to its normal chain.
func (e *EuropePMCReader) Read(ctx context.Context, targetURL string) (*Document, error) {
	pmid := PubMedID(targetURL)
	if pmid == "" {
		return nil, fmt.Errorf("%w: not a PubMed article URL (%s)", ErrUnsupportedFormat, targetURL)
	}
	q := url.Values{}
	q.Set("query", "EXT_ID:"+pmid+" AND SRC:MED")
	q.Set("resultType", "core")
	q.Set("format", "json")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Vedetta-Agent/1.0")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("europepmc request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("europepmc returned status %d", resp.StatusCode)
	}
	var data europePMCCore
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode europepmc response: %w", err)
	}
	for _, r := range data.ResultList.Result {
		if r.PMID != pmid {
			continue
		}
		abstract := strings.TrimSpace(html.UnescapeString(htmlTagRegex.ReplaceAllString(r.AbstractText, " ")))
		if abstract == "" {
			return nil, fmt.Errorf("europepmc record %s has no abstract", pmid)
		}
		title := strings.TrimSpace(html.UnescapeString(htmlTagRegex.ReplaceAllString(r.Title, "")))
		var b strings.Builder
		b.WriteString(title)
		b.WriteString("\n\n")
		var ref []string
		for _, part := range []string{r.AuthorString, r.JournalInfo.Journal.Title, r.PubYear} {
			if p := strings.TrimSpace(part); p != "" {
				ref = append(ref, p)
			}
		}
		if r.DOI != "" {
			ref = append(ref, "doi:"+r.DOI)
		}
		if len(ref) > 0 {
			b.WriteString(strings.Join(ref, ". "))
			b.WriteString("\n\n")
		}
		b.WriteString("Abstract\n")
		b.WriteString(strings.Join(strings.Fields(abstract), " "))
		return &Document{
			Title:       title,
			URL:         targetURL,
			Content:     b.String(),
			Metadata:    map[string]string{"source": "europepmc", "pmid": pmid},
			ExtractedAt: time.Now(),
		}, nil
	}
	return nil, fmt.Errorf("europepmc has no record for PMID %s", pmid)
}
