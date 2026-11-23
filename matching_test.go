package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

func matchFixture() capturedResponse {
	return validateProfileUsername("jane", capturedResponse{ProfileID: "1", Response: json.RawMessage(`{"data":{"user":{"id":"1","username":"jane","full_name":"Jane Doe","biography":"Engineer at Example Labs","profile_pic_url":"PRIVATE_IMAGE_URL","hd_profile_pic_url_info":{"url":"PRIVATE_HD_IMAGE_URL"},"bio_links":[{"title":"Work","url":"https://example.com"}]},"viewer":{"username":"PRIVATE_VIEWER"}}}`),
		LinkedInSearch: &linkedInSearch{Status: "ok", Candidates: []linkedInCandidate{
			{URL: "https://linkedin.com/in/jane", Profile: &linkedInProfileContent{Status: "ok", Name: "Jane Doe", Content: "Engineer at Example Labs", ProfilePicURL: "PRIVATE_LINKEDIN_PHOTO", HTML: "PRIVATE_HTML"}},
			{URL: "https://linkedin.com/in/another", Profile: &linkedInProfileContent{Status: "ok", Name: "Another Jane", Content: "Unrelated accountant"}},
			{URL: "https://linkedin.com/in/failed", Profile: &linkedInProfileContent{Status: "loading_incomplete"}},
		}}})
}

func TestJevMatchesTextAndExportsRequestedColumns(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer mock-key" {
			t.Error("incorrect method or authorization")
		}
		var payload struct {
			Model     string                     `json:"model"`
			State     map[string]json.RawMessage `json:"state"`
			Questions map[string]struct {
				Type string `json:"type"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		raw, _ := json.Marshal(payload.State)
		if strings.Contains(string(raw), "PRIVATE_") || !strings.Contains(string(raw), "Engineer at Example Labs") {
			t.Error("incorrect or excessive model input")
		}
		if payload.Model != defaultJevModel || payload.Questions["same_person"].Type != "noul" {
			t.Error("incorrect Decisions API payload")
		}
		probability := 0.3
		if strings.Contains(string(payload.State["linkedin"]), "Example Labs") {
			probability = 0.96
		}
		fmt.Fprintf(w, `{"answers":{"same_person":{"type":"noul","noul":%g}}}`, probability)
	}))
	defer server.Close()
	client := newJevClient("mock-key", defaultJevModel, time.Second)
	client.endpoint = server.URL
	result := client.Match(context.Background(), matchFixture(), 85, 10)
	if result.Status != "matched" || result.LinkedInURL != "https://linkedin.com/in/jane" || result.Score == nil || *result.Score != 96 || calls.Load() != 2 {
		t.Fatalf("incorrect match: %+v calls=%d", result, calls.Load())
	}
	if result.Candidates[2].Status != "skipped" {
		t.Fatal("failed LinkedIn extraction should not be scored")
	}
	response := matchFixture()
	response.Match = result
	jsonFile, err := saveProfileResult(t.TempDir(), "browser-001", Profile{Username: "jane", Link: "https://instagram.com/jane/"}, response)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(jsonFile)
	if err != nil {
		t.Fatal(err)
	}
	var record profileResult
	if err := json.Unmarshal(saved, &record); err != nil {
		t.Fatal(err)
	}
	if record.Match == nil || record.Match.Status != "matched" || record.LinkedInSearch.MatchVerified {
		t.Fatal("match details were not saved separately from identity verification")
	}
	path := filepath.Join(t.TempDir(), "matches.xlsx")
	export, err := newMatchExport(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := export.Write(Profile{Username: "jane", Link: "https://instagram.com/jane/"}, result); err != nil {
		t.Fatal(err)
	}
	if err := export.Close(); err != nil {
		t.Fatal(err)
	}
	rows := readMatchWorkbook(t, path)
	if len(rows) != 2 || strings.Join(rows[0], "|") != "username|instagram_url|linkedin_urls|linkedin_url|score" || rows[1][0] != "jane" || rows[1][1] != "https://instagram.com/jane/" || rows[1][2] != "https://linkedin.com/in/jane\nhttps://linkedin.com/in/another\nhttps://linkedin.com/in/failed" || rows[1][3] != "https://linkedin.com/in/jane" || rows[1][4] != "96" {
		t.Fatalf("incorrect export: %v", rows)
	}
	if _, err := newMatchExport(path); err == nil {
		t.Fatal("export must not overwrite an existing file")
	}
}

func TestJevRejectsMalformedScores(t *testing.T) {
	for _, body := range []string{`{}`, `{"answers":{"same_person":{"type":"noul","noul":null}}}`, `{"answers":{"same_person":{"type":"noul","noul":1.1}}}`, `{"answers":{"same_person":{"type":"noul","noul":-0.1}}}`, `{"answers":{"same_person":{"type":"choice","noul":0.9}}}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			client := newJevClient("secret-key", defaultJevModel, time.Second)
			client.endpoint = server.URL
			result := client.Match(context.Background(), matchFixture(), 85, 10)
			if result.Status != "error" || result.LinkedInURL != "" {
				t.Fatalf("invalid response selected a profile: %+v", result)
			}
		})
	}
}

func TestJevAPIProblemsPauseCallsWithoutLeakingCredentials(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				fmt.Fprint(w, "echoed secret-key")
			}))
			defer server.Close()
			client := newJevClient("secret-key", defaultJevModel, time.Second)
			client.endpoint = server.URL
			result := client.Match(context.Background(), matchFixture(), 85, 10)
			client.Match(context.Background(), matchFixture(), 85, 10)
			raw, _ := json.Marshal(result)
			if calls.Load() != 1 || result.Status != "error" || strings.Contains(string(raw), "secret-key") {
				t.Fatalf("unexpected failure behavior: calls=%d result=%s", calls.Load(), raw)
			}
		})
	}
}

func TestJevDoesNotForceWeakOrAmbiguousMatches(t *testing.T) {
	score := func(value float64) *float64 { return &value }
	for _, test := range []struct {
		name   string
		scores []*float64
		want   string
	}{
		{"weak", []*float64{score(70), score(20)}, "no_match"},
		{"close", []*float64{score(96), score(90)}, "ambiguous"},
		{"tie", []*float64{score(96), score(96)}, "ambiguous"},
		{"margin boundary", []*float64{score(95), score(85)}, "ambiguous"},
		{"missing", nil, "skipped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := &profileMatch{Threshold: 85, Margin: 10}
			for _, value := range test.scores {
				result.Candidates = append(result.Candidates, candidateMatch{URL: "https://linkedin.com/in/example", Status: "ok", Score: value})
			}
			selectProfileMatch(result)
			if result.Status != test.want || result.LinkedInURL != "" {
				t.Fatalf("unexpected selection: %+v", result)
			}
		})
	}
}

func TestJevSkipsInvalidInstagramAndConcurrentXLSXWrites(t *testing.T) {
	client := newJevClient("unused", defaultJevModel, time.Second)
	response := matchFixture()
	response.Error = "capture failed"
	if result := client.Match(context.Background(), response, 85, 10); result.Status != "skipped" {
		t.Fatalf("invalid Instagram should be skipped: %+v", result)
	}
	path := filepath.Join(t.TempDir(), "matches.xlsx")
	export, err := newMatchExport(path)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			if err := export.Write(Profile{Username: fmt.Sprint(i), Link: "https://instagram.com/example"}, &profileMatch{Status: "no_match"}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	group.Wait()
	if err := export.Close(); err != nil {
		t.Fatal(err)
	}
	rows := readMatchWorkbook(t, path)
	if len(rows) != 21 {
		t.Fatalf("missing or corrupt concurrent rows: count=%d", len(rows))
	}
}

func TestPartialJevFailureKeepsAvailableScoresWithoutSelecting(t *testing.T) {
	score := 96.0
	result := &profileMatch{Threshold: 85, Margin: 10, Candidates: []candidateMatch{
		{URL: "https://linkedin.com/in/one", Status: "ok", Score: &score},
		{URL: "https://linkedin.com/in/two", Status: "error", Error: "request failed"},
	}}
	selectProfileMatch(result)
	if result.Status != "error" || result.LinkedInURL != "" || result.Score == nil || *result.Score != 96 {
		t.Fatalf("partial failure forced a match or lost available score: %+v", result)
	}
}

func readMatchWorkbook(t *testing.T, path string) [][]string {
	t.Helper()
	workbook, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer workbook.Close()
	rows, err := workbook.GetRows("Matches", excelize.Options{RawCellValue: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		for col := 0; col < 5; col++ {
			formula, err := workbook.GetCellFormula("Matches", fmt.Sprintf("%c%d", 'A'+col, i+1))
			if err != nil || formula != "" {
				t.Fatalf("unexpected formula: %q err=%v", formula, err)
			}
		}
		for len(row) < 5 {
			row = append(row, "")
		}
		rows[i] = row
	}
	return rows
}

func TestMatchWorkbookPersistsEachRowAndKeepsLiteralText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matches.xlsx")
	export, err := newMatchExport(path)
	if err != nil {
		t.Fatal(err)
	}
	defer export.Close()
	if rows := readMatchWorkbook(t, path); len(rows) != 1 {
		t.Fatal("new workbook is missing its header")
	}
	score := 95.25
	if err := export.Write(Profile{Username: "=Literal & <name> ☃", Link: "https://example.com/?a=1&b=2"}, &profileMatch{Score: &score}); err != nil {
		t.Fatal(err)
	}
	rows := readMatchWorkbook(t, path)
	if len(rows) != 2 || rows[1][0] != "=Literal & <name> ☃" || rows[1][1] != "https://example.com/?a=1&b=2" || rows[1][2] != "" || rows[1][3] != "" || rows[1][4] != "95.25" {
		t.Fatalf("incorrect workbook row: %v", rows)
	}
	workbook, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer workbook.Close()
	cellType, err := workbook.GetCellType("Matches", "E2")
	// Excelize may omit t="n": an omitted type is numeric in SpreadsheetML.
	if err != nil || (cellType != excelize.CellTypeNumber && cellType != excelize.CellTypeUnset) {
		t.Fatalf("score is not numeric: %v %v", cellType, err)
	}
	styleID, err := workbook.GetCellStyle("Matches", "E2")
	if err != nil {
		t.Fatal(err)
	}
	style, err := workbook.GetStyle(styleID)
	if err != nil || style.NumFmt != 2 {
		t.Fatalf("score lost its two-decimal format: %+v %v", style, err)
	}
	panes, err := workbook.GetPanes("Matches")
	if err != nil || !panes.Freeze || panes.YSplit != 1 || panes.TopLeftCell != "A2" {
		t.Fatalf("header is not frozen: %+v %v", panes, err)
	}
	// Verify the application's existing XLSX reader can consume the new workbook.
	profiles, err := extractProfiles(path)
	if err != nil || len(profiles) != 1 || profiles[0].Username != "=Literal & <name> ☃" {
		t.Fatalf("XLSX round trip failed: %v %v", profiles, err)
	}
}

func TestWorkbookPairsEveryScoreWithCandidateAndListsAllURLs(t *testing.T) {
	for _, mode := range []string{"weak", "ambiguous", "partial failure", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			top, other := 48.0, 20.0
			if mode == "ambiguous" || mode == "partial failure" {
				top, other = 96, 90
			}
			result := &profileMatch{Threshold: 85, Margin: 10, Candidates: []candidateMatch{
				{URL: "https://linkedin.com/in/top", Status: "ok", Score: &top},
				{URL: "https://linkedin.com/in/other", Status: "ok", Score: &other},
				{URL: "https://linkedin.com/in/unloaded", Status: "skipped"},
			}, LinkedInURLs: []string{"https://linkedin.com/in/top", "https://linkedin.com/in/other", "https://linkedin.com/in/unloaded", "https://linkedin.com/in/top"}}
			if mode == "partial failure" {
				result.Candidates[1].Status = "error"
				result.Candidates[1].Score = nil
			}
			selectProfileMatch(result)
			if result.LinkedInURL != "" {
				t.Fatal("test fixture unexpectedly accepted an identity match")
			}
			if mode == "legacy" {
				result.BestCandidateURL = ""
				result.LinkedInURLs = nil
			}
			path := filepath.Join(t.TempDir(), "matches.xlsx")
			export, err := newMatchExport(path)
			if err != nil {
				t.Fatal(err)
			}
			defer export.Close()
			if err := export.Write(Profile{Username: "example", Link: "https://instagram.com/example"}, result); err != nil {
				t.Fatal(err)
			}
			rows := readMatchWorkbook(t, path)
			if rows[1][2] != "https://linkedin.com/in/top\nhttps://linkedin.com/in/other\nhttps://linkedin.com/in/unloaded" || rows[1][3] != "https://linkedin.com/in/top" || rows[1][4] != fmt.Sprint(top) {
				t.Fatalf("score and URLs are not paired correctly: %v", rows[1])
			}
		})
	}
}
