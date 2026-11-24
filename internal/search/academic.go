package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AcademicProvider implements search.Provider using the public Europe PMC REST API,
// which provides zero-config, keyless access to PubMed, PMC, and life sciences literature.
type AcademicProvider struct {
	baseURL    string
	httpClient *http.Client
}

type europePMCResponse struct {
	HitCount   int `json:"hitCount"`
	ResultList struct {
		Result []struct {
			ID           string `json:"id"`
			Source       string `json:"source"`
			PMID         string `json:"pmid"`
			DOI          string `json:"doi"`
			Title        string `json:"title"`
			AuthorString string `json:"authorString"`
			JournalTitle string `json:"journalTitle"`
			PubYear      string `json:"pubYear"`
			AbstractText string `json:"abstractText"`
		} `json:"result"`
	} `json:"resultList"`
}

var siteRegex = regexp.MustCompile(`(?i)site:[^\s]+`)

// NewAcademicProvider constructs a new zero-config Europe PMC / PubMed academic provider.
func NewAcademicProvider(customBaseURL ...string) *AcademicProvider {
	baseURL := "https://www.ebi.ac.uk/europepmc/webservices/rest/search"
	if len(customBaseURL) > 0 && customBaseURL[0] != "" {
		baseURL = strings.TrimRight(customBaseURL[0], "/")
	}
	return &AcademicProvider{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (a *AcademicProvider) Name() string {
	return "europepmc"
}

func (a *AcademicProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	// Strip site: operators for Europe PMC search syntax
	cleanQuery := strings.TrimSpace(siteRegex.ReplaceAllString(query, ""))
	cleanQuery = strings.Join(strings.Fields(cleanQuery), " ")
	if cleanQuery == "" {
		return nil, fmt.Errorf("empty academic query")
	}

	maxRes := opts.MaxResults
	if maxRes <= 0 {
		maxRes = 8
	}
	if maxRes > 25 {
		maxRes = 25
	}

	u, err := url.Parse(a.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid academic base url: %w", err)
	}

	q := u.Query()
	q.Set("query", cleanQuery)
	q.Set("format", "json")
	q.Set("pageSize", strconv.Itoa(maxRes))
	q.Set("resultType", "core")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create academic request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Vedetta-Academic/1.0 (mailto:vedetta@local)")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("academic request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrQuotaExhausted
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("academic returned status %d: %s", resp.StatusCode, string(body))
	}

	var data europePMCResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode academic response: %w", err)
	}

	var results []Result
	seen := make(map[string]bool)

	for _, item := range data.ResultList.Result {
		rawTitle := strings.TrimSpace(item.Title)
		// Clean trailing period
		rawTitle = strings.TrimSuffix(rawTitle, ".")
		if rawTitle == "" {
			continue
		}

		// Prioritize direct PubMed URL if PMID is present
		var docURL string
		pmid := strings.TrimSpace(item.PMID)
		doi := strings.TrimSpace(item.DOI)
		if pmid != "" {
			docURL = fmt.Sprintf("https://pubmed.ncbi.nlm.nih.gov/%s/", pmid)
		} else if doi != "" {
			docURL = fmt.Sprintf("https://doi.org/%s", doi)
		} else if item.ID != "" && item.Source != "" {
			docURL = fmt.Sprintf("https://europepmc.org/article/%s/%s", item.Source, item.ID)
		} else {
			continue
		}

		if seen[docURL] {
			continue
		}
		seen[docURL] = true

		// Build rich academic snippet
		var metaParts []string
		if item.AuthorString != "" {
			metaParts = append(metaParts, item.AuthorString)
		}
		if item.JournalTitle != "" {
			if item.PubYear != "" {
				metaParts = append(metaParts, fmt.Sprintf("%s (%s)", item.JournalTitle, item.PubYear))
			} else {
				metaParts = append(metaParts, item.JournalTitle)
			}
		} else if item.PubYear != "" {
			metaParts = append(metaParts, item.PubYear)
		}

		snippet := strings.TrimSpace(item.AbstractText)
		if len(metaParts) > 0 {
			header := "[" + strings.Join(metaParts, " | ") + "]"
			if snippet != "" {
				snippet = header + " " + snippet
			} else {
				snippet = header
			}
		}

		results = append(results, Result{
			Title:   rawTitle,
			URL:     docURL,
			Content: snippet,
			Engine:  "europepmc",
			Score:   1.0,
		})
	}

	return results, nil
}
