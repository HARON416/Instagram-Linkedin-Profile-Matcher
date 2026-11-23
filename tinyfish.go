package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const tinyFishSearchEndpoint = "https://api.search.tinyfish.ai"

type linkedInCandidate struct {
	Profile  *linkedInProfileContent `json:"profile,omitempty"`
	Position int                     `json:"position"`
	SiteName string                  `json:"site_name"`
	Title    string                  `json:"title"`
	Snippet  string                  `json:"snippet"`
	URL      string                  `json:"url"`
}

type linkedInSearch struct {
	Status        string              `json:"status"`
	FullName      string              `json:"full_name,omitempty"`
	SearchName    string              `json:"search_name,omitempty"`
	Query         string              `json:"query,omitempty"`
	SearchedAt    time.Time           `json:"searched_at,omitempty"`
	HTTPStatus    int                 `json:"http_status,omitempty"`
	Candidates    []linkedInCandidate `json:"candidates"`
	MatchVerified bool                `json:"match_verified"`
	Error         string              `json:"error,omitempty"`
}

type tinyFishClient struct {
	key           string
	endpoint      string
	http          *http.Client
	timeout       time.Duration
	interval      time.Duration
	mu            sync.Mutex
	next          time.Time
	blockedStatus int
}

func newTinyFishClient(key string, timeout, interval time.Duration) *tinyFishClient {
	return &tinyFishClient{key: key, endpoint: tinyFishSearchEndpoint, timeout: timeout, interval: interval,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func linkedInSearchName(fullName string) string {
	first, _, _ := strings.Cut(fullName, "|")
	return strings.Join(strings.Fields(first), " ")
}

// One limiter is shared by the whole browser pool, matching API-key-wide limits.
func (c *tinyFishClient) reserve(ctx context.Context) error {
	c.mu.Lock()
	if c.blockedStatus != 0 {
		status := c.blockedStatus
		c.mu.Unlock()
		return fmt.Errorf("TinyFish searches paused after HTTP %d; correct the API issue before restarting", status)
	}
	start := time.Now()
	if c.next.After(start) {
		start = c.next
	}
	c.next = start.Add(c.interval)
	c.mu.Unlock()
	if err := waitVisitDelay(ctx, time.Until(start)); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blockedStatus != 0 {
		return fmt.Errorf("TinyFish searches paused after HTTP %d; correct the API issue before restarting", c.blockedStatus)
	}
	return nil
}

func (c *tinyFishClient) Search(ctx context.Context, fullName string) linkedInSearch {
	name := linkedInSearchName(fullName)
	result := linkedInSearch{Status: "skipped", FullName: fullName, SearchName: name, Candidates: []linkedInCandidate{}}
	if name == "" {
		result.Error = "Instagram full_name is empty before the first |"
		return result
	}
	result.Query = name + " LinkedIn"
	result.Status = "error"
	if err := c.reserve(ctx); err != nil {
		result.Error = err.Error()
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	endpoint, err := url.Parse(c.endpoint)
	if err != nil {
		result.Error = "invalid TinyFish endpoint"
		return result
	}
	query := endpoint.Query()
	query.Set("query", result.Query)
	query.Set("include_domains", "linkedin.com")
	query.Set("purpose", "Find LinkedIn personal profile candidates for a person by full name")
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		result.Error = "could not create TinyFish search request"
		return result
	}
	request.Header.Set("X-API-Key", c.key)
	request.Header.Set("Accept", "application/json")
	result.SearchedAt = time.Now().UTC()
	response, err := c.http.Do(request)
	if err != nil {
		result.Error = "TinyFish request failed or timed out"
		return result
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	if response.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("TinyFish search returned HTTP %d", response.StatusCode)
		if response.StatusCode == 429 {
			result.Status = "rate_limited"
		}
		if response.StatusCode == 401 || response.StatusCode == 402 || response.StatusCode == 429 {
			c.mu.Lock()
			c.blockedStatus = response.StatusCode
			c.mu.Unlock()
		}
		// Error bodies may echo credentials; do not persist or log them.
		return result
	}
	var payload struct {
		Results *[]linkedInCandidate `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8*1024*1024)).Decode(&payload); err != nil || payload.Results == nil {
		result.Error = "TinyFish returned an invalid search response"
		return result
	}
	seen := map[string]bool{}
	for _, candidate := range *payload.Results {
		// Profile content must come from our browser visit, not a search field.
		candidate.Profile = nil
		parsed, err := url.Parse(candidate.URL)
		if err == nil && parsed.Scheme == "" {
			parsed, err = url.Parse("https://" + candidate.URL)
		}
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if host != "linkedin.com" && !strings.HasSuffix(host, ".linkedin.com") {
			continue
		}
		if !strings.HasPrefix(parsed.Path, "/in/") || strings.Trim(strings.TrimPrefix(parsed.Path, "/in/"), "/") == "" {
			continue
		}
		parsed.Fragment = ""
		candidate.URL = parsed.String()
		if !seen[candidate.URL] {
			result.Candidates = append(result.Candidates, candidate)
			seen[candidate.URL] = true
		}
	}
	result.Status = "ok"
	if len(result.Candidates) == 0 {
		result.Status = "no_results"
	}
	return result
}

func enrichLinkedIn(ctx context.Context, client *tinyFishClient, response capturedResponse) *linkedInSearch {
	if response.Error != "" {
		return &linkedInSearch{Status: "skipped", Candidates: []linkedInCandidate{}, Error: "Instagram capture or username validation failed"}
	}
	user, err := profileResponseUser(response.Response, response.ProfileID)
	if err != nil {
		return &linkedInSearch{Status: "skipped", Candidates: []linkedInCandidate{}, Error: err.Error()}
	}
	fullName, _ := user["full_name"].(string)
	result := client.Search(ctx, fullName)
	return &result
}
