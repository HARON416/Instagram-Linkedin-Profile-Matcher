package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLinkedInSearchName(t *testing.T) {
	cases := map[string]string{"John Doe | Coach": "John Doe", " John  Doe | Coach | Speaker ": "John Doe", "Mary O’Connor": "Mary O’Connor", " | Coach": "", "": ""}
	for input, expected := range cases {
		if got := linkedInSearchName(input); got != expected {
			t.Errorf("%q -> %q, want %q", input, got, expected)
		}
	}
}

func TestTinyFishSearchRequestAndCandidateFiltering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("X-API-Key") != "mock-key" || r.URL.Query().Get("query") != "John Doe LinkedIn" || r.URL.Query().Get("include_domains") != "linkedin.com" {
			t.Errorf("incorrect search request: method=%s query=%s", r.Method, r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"results":[{"position":1,"title":"John Doe","url":"https://www.linkedin.com/in/john-doe/","snippet":"Developer"},{"url":"https://linkedin.com.evil.example/in/john"},{"url":"https://www.linkedin.com/company/john"},{"url":"https://www.linkedin.com/in/"},{"url":"uk.linkedin.com/in/another-john"}]}`)
	}))
	defer server.Close()
	client := newTinyFishClient("mock-key", time.Second, 0)
	client.endpoint = server.URL
	result := client.Search(context.Background(), "John Doe | Coach")
	if result.Status != "ok" || result.SearchName != "John Doe" || len(result.Candidates) != 2 || result.MatchVerified {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestTinyFishErrorsDoNotLeakKeysOrRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(429)
		fmt.Fprint(w, "echoed mock-secret-key")
	}))
	defer server.Close()
	client := newTinyFishClient("mock-secret-key", time.Second, 0)
	client.endpoint = server.URL
	first := client.Search(context.Background(), "Person")
	second := client.Search(context.Background(), "Another Person")
	body, _ := json.Marshal([]linkedInSearch{first, second})
	if calls.Load() != 1 || first.Status != "rate_limited" || second.Error == "" || strings.Contains(string(body), "mock-secret-key") {
		t.Fatalf("calls=%d results=%s", calls.Load(), body)
	}
}

func TestEnrichmentUsesTargetFullNameAndSkipsFailedCapture(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("query") != "Target Person LinkedIn" {
			t.Errorf("wrong profile's full_name used: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer server.Close()
	client := newTinyFishClient("mock-key", time.Second, 0)
	client.endpoint = server.URL
	response := capturedResponse{ProfileID: "1", Response: json.RawMessage(`{"data":{"suggestion":{"id":"2","username":"other","full_name":"Wrong Person"},"user":{"id":"1","username":"target","full_name":"Target Person | Founder"}}}`)}
	if got := enrichLinkedIn(context.Background(), client, response); got.Status != "no_results" || got.FullName != "Target Person | Founder" || got.SearchName != "Target Person" {
		t.Fatalf("unexpected enrichment: %+v", got)
	}
	response.Error = "username mismatch"
	if got := enrichLinkedIn(context.Background(), client, response); got.Status != "skipped" {
		t.Fatalf("failed capture searched: %+v", got)
	}
	response.Error = ""
	response.Response = json.RawMessage(`{"data":{"user":{"id":"1","username":"target","full_name":" | Founder"}}}`)
	if got := enrichLinkedIn(context.Background(), client, response); got.Status != "skipped" {
		t.Fatalf("empty name searched: %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("got %d searches, want 1", calls.Load())
	}
}

func TestTinyFishWaitIsCancellable(t *testing.T) {
	client := newTinyFishClient("mock-key", time.Second, time.Hour)
	client.next = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.reserve(ctx); err == nil {
		t.Fatal("cancelled search limiter did not stop")
	}
}

func TestIncrementalUserKeepsFullName(t *testing.T) {
	user, err := profileResponseUser(json.RawMessage(`[{"data":{"user":{"id":"1","username":"target","full_name":"Target Person | Founder"}}},{"data":{"user":{"id":"1","username":"target"}}}]`), "1")
	if err != nil || user["full_name"] != "Target Person | Founder" {
		t.Fatalf("full_name lost across chunks: user=%v error=%v", user, err)
	}
}
