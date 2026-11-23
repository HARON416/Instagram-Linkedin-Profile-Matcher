package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

func TestMatchOnlyProfileQuery(t *testing.T) {
	form := url.Values{"fb_api_req_friendly_name": {profileQueryName}, "variables": {`{"id":"123"}`}, "fb_dtsg": {"secret-token"}}
	request := &proto.NetworkRequest{URL: "https://www.instagram.com/api/graphql", Method: "POST"}
	if !isGraphQLRequest(request) {
		t.Fatal("GraphQL endpoint not matched")
	}
	if match, id := matchProfileQuery(request, form.Encode()); !match || id != "123" {
		t.Fatalf("match=%v id=%q", match, id)
	}
	if match, _ := matchProfileQuery(request, "fb_api_req_friendly_name=OtherQuery"); match {
		t.Fatal("unrelated GraphQL query matched")
	}
	request.Headers = proto.NetworkHeaders{"X-FB-Friendly-Name": gson.New(profileQueryName)}
	if match, _ := matchProfileQuery(request, "variables=%7B%7D"); !match {
		t.Fatal("friendly header not matched")
	}
	request.URL = "https://www.instagram.com/api/graphql/not-the-endpoint"
	if isGraphQLRequest(request) {
		t.Fatal("unrelated endpoint matched")
	}
}

func TestGraphQLResponseParsing(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"success", `{"data":{"user":{"username":"someone"}}}`, 200, false},
		{"streamed", "for (;;);{\"data\":{\"user\":{}}}\n{\"data\":{\"extra\":1}}", 200, false},
		{"errors", `{"data":null,"errors":[{"message":"login"}]}`, 200, true},
		{"rate limit", `{"errors":[{"message":"slow down"}]}`, 429, true},
		{"invalid JSON", "<html>login</html>", 200, true},
		{"no data", `{"status":"fail"}`, 200, true},
		{"empty", "", 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, err := parseGraphQLResponse([]byte(tc.body), tc.status)
			if (err != "") != tc.wantError {
				t.Fatalf("error=%q", err)
			}
			if !tc.wantError && !json.Valid(response) {
				t.Fatal("result is not valid JSON")
			}
		})
	}
}

func TestSavedResponseContainsNoRequestCredentials(t *testing.T) {
	directory := t.TempDir()
	profile := Profile{Username: "../../unsafe", Link: "https://www.instagram.com/someone/"}
	file, err := saveProfileResult(directory, "browser-001", profile, capturedResponse{HTTPStatus: 200, ProfileID: "123", Response: json.RawMessage(`{"data":{"user":{"username":"someone"}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(file) != directory {
		t.Fatal("output path escaped results directory")
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"username", "link", "browser_profile", "captured_at", "query", "status", "profile_id", "response"} {
		if _, exists := record[field]; !exists {
			t.Errorf("missing field %s", field)
		}
	}
	for _, forbidden := range []string{"cookie", "headers", "post_data", "sessionid", "fb_dtsg", "csrftoken"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("unexpected request credential field %s", forbidden)
		}
	}
}

func TestBrowserCapturesInitialResponseAndReplaysInvalidBody(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TESTS") != "1" {
		t.Skip("set RUN_BROWSER_TESTS=1 to launch local Chrome")
	}
	var matching, unrelated atomic.Int32
	var fourthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/api/graphql" {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if r.Form.Get("fb_api_req_friendly_name") != profileQueryName {
				unrelated.Add(1)
				fmt.Fprint(w, `{"data":{"unrelated":true}}`)
				return
			}
			matching.Add(1)
			var variables struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal([]byte(r.Form.Get("variables")), &variables)
			if variables.ID == "4" && fourthRequests.Add(1) == 1 {
				fmt.Fprint(w, "incomplete response")
				return
			}
			time.Sleep(150 * time.Millisecond)
			fmt.Fprintf(w, `{"data":{"user":{"id":%q,"username":%q,"biography":"Current bio","full_name":%q}}}`, variables.ID, "user"+variables.ID, "Person "+variables.ID+" | Coach")
			return
		}
		if r.URL.Path != "/profile" {
			return
		}
		w.Header().Set("Content-Type", "text/html")
		id := r.URL.Query().Get("id")
		// These requests fire immediately on the initial document, before load.
		fmt.Fprintf(w, `<html><body><script>
            fetch('/api/graphql',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:'fb_api_req_friendly_name=OtherQuery'}).then(response => response.json());
            const form=new URLSearchParams({fb_api_req_friendly_name:%q,variables:JSON.stringify({id:%q}),fb_dtsg:'must-not-be-saved'});
            fetch('/api/graphql',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded','X-FB-Friendly-Name':%q,'X-CSRFToken':'must-not-be-saved'},body:form}).then(response => response.json());
        </script></body></html>`, profileQueryName, id, profileQueryName)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var searches atomic.Int32
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		if strings.Contains(r.URL.Query().Get("query"), "Coach") || r.Header.Get("X-API-Key") != "mock-key" {
			t.Error("incorrect TinyFish enrichment request")
		}
		fmt.Fprint(w, `{"results":[{"title":"Candidate","url":"https://www.linkedin.com/in/candidate/"}]}`)
	}))
	defer searchServer.Close()
	options := runOptions{DataDir: t.TempDir(), ResultsDir: t.TempDir(), Headless: true, BrowserProfiles: 2, Timeout: 5 * time.Second, DelayMin: 10 * time.Millisecond, DelayMax: 20 * time.Millisecond}
	options.LinkedIn, options.SearchTimeout = true, 5*time.Second
	options.search = newTinyFishClient("mock-key", options.SearchTimeout, 0)
	options.search.endpoint = searchServer.URL
	var profiles []Profile
	for id := 1; id <= 4; id++ {
		profiles = append(profiles, Profile{Username: fmt.Sprintf("user%d", id), Link: fmt.Sprintf("%s/profile?id=%d", server.URL, id)})
	}
	if err := runProfiles(ctx, profiles, options); err != nil {
		t.Fatal(err)
	}
	if matching.Load() != 5 || unrelated.Load() != 4 || fourthRequests.Load() != 2 {
		t.Fatalf("matching=%d unrelated=%d fourth=%d; want one replay only for the invalid response", matching.Load(), unrelated.Load(), fourthRequests.Load())
	}
	if searches.Load() != 4 {
		t.Fatalf("got %d LinkedIn searches, want one per captured profile", searches.Load())
	}
	files, err := os.ReadDir(options.ResultsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("got %d output files, want 4", len(files))
	}
	for _, file := range files {
		body, err := os.ReadFile(filepath.Join(options.ResultsDir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "must-not-be-saved") {
			t.Fatal("request credentials were saved")
		}
		var record struct {
			LinkedInSearch   *linkedInSearch `json:"linkedin_search"`
			Username         string          `json:"username"`
			Status           string          `json:"status"`
			ResponseUsername string          `json:"response_username"`
			UsernameMatch    *bool           `json:"username_match"`
			Response         struct {
				Data struct {
					User struct {
						Username string `json:"username"`
					} `json:"user"`
				} `json:"data"`
			} `json:"response"`
		}
		if err := json.Unmarshal(body, &record); err != nil {
			t.Fatal(err)
		}
		if record.Status != "ok" || record.Username != record.Response.Data.User.Username || record.ResponseUsername != record.Username || record.UsernameMatch == nil || !*record.UsernameMatch {
			t.Fatalf("response assigned to wrong profile: %s", body)
		}
		if record.LinkedInSearch == nil || record.LinkedInSearch.Status != "ok" || len(record.LinkedInSearch.Candidates) != 1 || strings.Contains(record.LinkedInSearch.SearchName, "|") {
			t.Fatalf("LinkedIn enrichment missing or incorrect: %s", body)
		}
	}
}
