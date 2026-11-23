package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
}

// Prefer the user object with the requested ID. Do not pick the first username
// in the response: suggested accounts and media owners can also have usernames.
func profileResponseUser(response json.RawMessage, profileID string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(response))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("profile response is not valid JSON")
	}
	all, matchingID, withoutID := map[string]map[string]any{}, map[string]map[string]any{}, map[string]map[string]any{}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if username, ok := value["username"].(string); ok && normalizeUsername(username) != "" {
				key := normalizeUsername(username)
				rememberUser(all, key, value)
				hasID, matches := false, false
				for _, field := range []string{"id", "pk"} {
					var id string
					switch v := value[field].(type) {
					case string:
						id = v
					case json.Number:
						id = v.String()
					}
					if id != "" {
						hasID = true
						matches = matches || id == profileID
					}
				}
				if profileID != "" && matches {
					rememberUser(matchingID, key, value)
				}
				if !hasID {
					rememberUser(withoutID, key, value)
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	// Only inspect GraphQL data, including incremental response chunks.
	chunks, isArray := value.([]any)
	if !isArray {
		chunks = []any{value}
	}
	for _, chunk := range chunks {
		if envelope, ok := chunk.(map[string]any); ok {
			walk(envelope["data"])
			if incremental, ok := envelope["incremental"].([]any); ok {
				for _, item := range incremental {
					if item, ok := item.(map[string]any); ok {
						walk(item["data"])
					}
				}
			}
		}
	}
	candidates := all
	if profileID != "" {
		candidates = matchingID
		if len(candidates) == 0 && len(all) == 1 && len(withoutID) == 1 {
			candidates = withoutID
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no unambiguous username for the requested profile was found")
	}
	if len(candidates) != 1 {
		return nil, fmt.Errorf("response contains multiple possible profile usernames")
	}
	for _, user := range candidates {
		return user, nil
	}
	return nil, fmt.Errorf("profile user is unavailable")
}

// Incremental chunks can repeat a user with only some fields present.
func rememberUser(users map[string]map[string]any, key string, incoming map[string]any) {
	if existing, ok := users[key]; ok {
		for field, value := range incoming {
			if value != nil {
				if text, ok := value.(string); !ok || text != "" {
					existing[field] = value
				}
			}
		}
	} else {
		copy := make(map[string]any, len(incoming))
		for field, value := range incoming {
			copy[field] = value
		}
		users[key] = copy
	}
}

func profileResponseUsername(response json.RawMessage, profileID string) (string, error) {
	user, err := profileResponseUser(response, profileID)
	if err != nil {
		return "", err
	}
	username, _ := user["username"].(string)
	return username, nil
}

func validateProfileUsername(expected string, response capturedResponse) capturedResponse {
	user, err := profileResponseUser(response.Response, response.ProfileID)
	username, _ := user["username"].(string)
	response.ResponseUsername = username
	response.ProfilePicURL = ""
	response.HDProfilePicURL = ""
	response.UsernameMatch = nil
	response.UsernameCheckError = ""
	if err == nil && normalizeUsername(expected) == "" {
		err = fmt.Errorf("XLSX username is empty")
	}
	if err != nil {
		response.UsernameCheckError = err.Error()
		if response.Error == "" {
			response.Error = "cannot verify response username: " + err.Error()
		}
		return response
	}
	match := normalizeUsername(expected) == normalizeUsername(username)
	response.UsernameMatch = &match
	if match {
		response.ProfilePicURL, _ = user["profile_pic_url"].(string)
		if hd, ok := user["hd_profile_pic_url_info"].(map[string]any); ok {
			response.HDProfilePicURL, _ = hd["url"].(string)
		}
	}
	if !match {
		response.UsernameCheckError = fmt.Sprintf("XLSX username %q does not match response username %q", expected, username)
		if response.Error == "" {
			response.Error = "username mismatch: " + response.UsernameCheckError
		}
	}
	return response
}
