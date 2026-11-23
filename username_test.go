package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestValidateProfileUsername(t *testing.T) {
	tests := []struct {
		name, expected, id, body, actual string
		match                            bool
		unavailable                      bool
	}{
		{"match", "someone", "1", `{"data":{"user":{"id":"1","username":"someone"}}}`, "someone", true, false},
		{"normalized", " @SomeOne ", "1", `{"data":{"user":{"id":"1","username":"someone"}}}`, "someone", true, false},
		{"mismatch", "someone", "1", `{"data":{"user":{"id":"1","username":"other"}}}`, "other", false, false},
		{"target not recommendation", "someone", "1", `{"data":{"suggested":{"id":"2","username":"someone"},"profile":{"pk":1,"username":"other"}}}`, "other", false, false},
		{"nested numeric id", "someone", "9007199254740993", `{"data":{"xdt_profile":{"user":{"pk":9007199254740993,"username":"someone"}}}}`, "someone", true, false},
		{"username without id", "someone", "1", `{"data":{"user":{"username":"someone"}}}`, "someone", true, false},
		{"incremental", "someone", "1", `[{"data":{"user":{"id":"1","username":"someone"}}},{"data":{"extra":true}}]`, "someone", true, false},
		{"no username", "someone", "1", `{"data":{"user":{"id":"1"}}}`, "", false, true},
		{"wrong user id", "someone", "1", `{"data":{"user":{"id":"2","username":"someone"}}}`, "", false, true},
		{"ambiguous", "someone", "", `{"data":{"users":[{"username":"someone"},{"username":"other"}]}}`, "", false, true},
		{"missing XLSX username", "", "1", `{"data":{"user":{"id":"1","username":"someone"}}}`, "someone", false, true},
		{"ignore metadata username", "someone", "", `{"extensions":{"username":"someone"},"data":{"user":{}}}`, "", false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validateProfileUsername(test.expected, capturedResponse{ProfileID: test.id, Response: json.RawMessage(test.body)})
			if response.ResponseUsername != test.actual {
				t.Fatalf("response username=%q, want %q", response.ResponseUsername, test.actual)
			}
			if test.unavailable {
				if response.UsernameMatch != nil || response.UsernameCheckError == "" || response.Error == "" {
					t.Fatalf("missing/ambiguous identity was not flagged: %+v", response)
				}
			} else if response.UsernameMatch == nil || *response.UsernameMatch != test.match {
				t.Fatalf("username match=%v, want %v", response.UsernameMatch, test.match)
			}
			if !test.match && response.Error == "" {
				t.Fatal("mismatch was treated as success")
			}
		})
	}
}

func TestSavedUsernameMismatchIsExplicit(t *testing.T) {
	file, err := saveProfileResult(t.TempDir(), "browser-001", Profile{Username: "expected", Link: "https://example.com/expected"}, capturedResponse{
		HTTPStatus: 200, ProfileID: "1", Response: json.RawMessage(`{"data":{"user":{"id":"1","username":"actual"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var record profileResult
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if record.Status != "username_mismatch" || record.ResponseUsername != "actual" || record.UsernameMatch == nil || *record.UsernameMatch {
		t.Fatalf("mismatch not recorded correctly: %s", body)
	}
}

func TestProfilePictureURLsBelongToVerifiedTarget(t *testing.T) {
	body := json.RawMessage(`{"data":{"suggested":{"id":"2","username":"suggestion","profile_pic_url":"https://example.com/wrong.jpg"},"user":{"id":"1","username":"target","profile_pic_url":"https://example.com/target.jpg","hd_profile_pic_url_info":{"url":"https://example.com/target-hd.jpg"}}}}`)
	for _, expected := range []string{"target", "different"} {
		response := validateProfileUsername(expected, capturedResponse{ProfileID: "1", Response: body})
		if expected == "target" {
			if response.ProfilePicURL != "https://example.com/target.jpg" || response.HDProfilePicURL != "https://example.com/target-hd.jpg" {
				t.Fatal("target picture URLs were not extracted")
			}
		} else if response.ProfilePicURL != "" || response.HDProfilePicURL != "" {
			t.Fatal("picture URLs were associated with a mismatched username")
		}
	}
	missing := validateProfileUsername("target", capturedResponse{ProfileID: "1", Response: json.RawMessage(`{"data":{"user":{"id":"1","username":"target"}}}`)})
	if missing.ProfilePicURL != "" || missing.HDProfilePicURL != "" {
		t.Fatal("missing picture URLs must stay empty")
	}
}
