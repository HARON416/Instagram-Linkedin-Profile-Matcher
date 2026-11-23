package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const profileQueryName = "PolarisProfilePageContentQuery"

var errProfileRateLimited = errors.New("profile request was rate limited")

type capturedResponse struct {
	HTTPStatus         int             `json:"http_status,omitempty"`
	ProfileID          string          `json:"profile_id,omitempty"`
	Response           json.RawMessage `json:"response,omitempty"`
	Error              string          `json:"error,omitempty"`
	ResponseUsername   string          `json:"response_username,omitempty"`
	ProfilePicURL      string          `json:"profile_pic_url"`
	HDProfilePicURL    string          `json:"hd_profile_pic_url"`
	UsernameMatch      *bool           `json:"username_match"`
	UsernameCheckError string          `json:"username_check_error,omitempty"`
	LinkedInSearch     *linkedInSearch `json:"linkedin_search,omitempty"`
	Match              *profileMatch   `json:"match,omitempty"`
}

type captureRequest struct {
	target    proto.TargetTargetID
	profileID string
	status    int
}

type captureSession struct {
	*rod.Browser
	ctx context.Context
	id  proto.TargetSessionID
}

func (s captureSession) GetContext() context.Context         { return s.ctx }
func (s captureSession) GetSessionID() proto.TargetSessionID { return s.id }

type responseCapture struct {
	browser  *rod.Browser
	ctx      context.Context
	stop     context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	sessions map[proto.TargetSessionID]proto.TargetTargetID
	requests map[string]captureRequest
	results  map[proto.TargetTargetID]capturedResponse
	replays  map[proto.TargetTargetID]profileReplay
}

// Auto-attach pauses each new page's scripts until Network is enabled. This
// captures even an inline fetch on the initial page without a blank-tab reload.
func startResponseCapture(ctx context.Context, browser *rod.Browser) (*responseCapture, error) {
	ctx, cancel := context.WithCancel(ctx)
	c := &responseCapture{browser: browser, ctx: ctx, stop: cancel, done: make(chan struct{}),
		sessions: make(map[proto.TargetSessionID]proto.TargetTargetID), requests: make(map[string]captureRequest), results: make(map[proto.TargetTargetID]capturedResponse)}
	events := browser.Context(ctx).Event()
	go c.watch(events)
	err := (proto.TargetSetAutoAttach{AutoAttach: true, WaitForDebuggerOnStart: true, Flatten: true,
		Filter: proto.TargetTargetFilter{{Type: "page"}, {Exclude: true}}}).Call(browser.Context(ctx))
	if err != nil {
		cancel()
		<-c.done
		return nil, fmt.Errorf("arm response capture: %w", err)
	}
	return c, nil
}

func (c *responseCapture) watch(events <-chan *rod.Message) {
	defer close(c.done)
	for {
		select {
		case <-c.ctx.Done():
			return
		case message, ok := <-events:
			if !ok {
				return
			}
			c.handle(message)
		}
	}
}

func requestKey(session proto.TargetSessionID, id proto.NetworkRequestID) string {
	return string(session) + ":" + string(id)
}

func (c *responseCapture) handle(message *rod.Message) {
	var attached proto.TargetAttachedToTarget
	if message.Load(&attached) {
		if attached.TargetInfo == nil || attached.TargetInfo.Type != "page" {
			return
		}
		session := captureSession{Browser: c.browser, ctx: c.ctx, id: attached.SessionID}
		c.sessions[attached.SessionID] = attached.TargetInfo.TargetID
		buffer := 16 * 1024 * 1024
		postLimit := 256 * 1024
		err := (proto.NetworkEnable{MaxTotalBufferSize: &buffer, MaxResourceBufferSize: &buffer, MaxPostDataSize: &postLimit}).Call(session)
		if err != nil {
			c.set(attached.TargetInfo.TargetID, capturedResponse{Error: "unable to enable network capture"})
		}
		// Always resume the page, including when capture setup failed.
		if err := (proto.RuntimeRunIfWaitingForDebugger{}).Call(session); err != nil {
			c.set(attached.TargetInfo.TargetID, capturedResponse{Error: "unable to resume captured page"})
		}
		return
	}
	session := captureSession{Browser: c.browser, ctx: c.ctx, id: message.SessionID}
	var request proto.NetworkRequestWillBeSent
	if message.Load(&request) {
		target, ok := c.sessions[message.SessionID]
		if !ok || request.Request == nil || !isGraphQLRequest(request.Request) {
			return
		}
		post := request.Request.PostData
		if post == "" && request.Request.HasPostData {
			body, err := (proto.NetworkGetRequestPostData{RequestID: request.RequestID}).Call(session)
			if err == nil {
				post = body.PostData
			}
		}
		matches, profileID := matchProfileQuery(request.Request, post)
		if matches {
			c.mu.Lock()
			if c.replays == nil {
				c.replays = make(map[proto.TargetTargetID]profileReplay)
			}
			c.replays[target] = newProfileReplay(request.Request, post, profileID)
			c.mu.Unlock()
			c.requests[requestKey(message.SessionID, request.RequestID)] = captureRequest{target: target, profileID: profileID}
		}
		return
	}
	var response proto.NetworkResponseReceived
	if message.Load(&response) {
		key := requestKey(message.SessionID, response.RequestID)
		if request, ok := c.requests[key]; ok && response.Response != nil {
			request.status = response.Response.Status
			c.requests[key] = request
		}
		return
	}
	var finished proto.NetworkLoadingFinished
	if message.Load(&finished) {
		key := requestKey(message.SessionID, finished.RequestID)
		request, ok := c.requests[key]
		if !ok {
			return
		}
		delete(c.requests, key)
		result := capturedResponse{HTTPStatus: request.status, ProfileID: request.profileID}
		body, err := (proto.NetworkGetResponseBody{RequestID: finished.RequestID}).Call(session)
		if err != nil {
			result.Error = "matching response body was unavailable"
		} else {
			data := []byte(body.Body)
			if body.Base64Encoded {
				data, err = base64.StdEncoding.DecodeString(body.Body)
			}
			if err != nil {
				result.Error = "matching response body could not be decoded"
			} else {
				result.Response, result.Error = parseGraphQLResponse(data, request.status)
			}
		}
		c.set(request.target, result)
		return
	}
	var failed proto.NetworkLoadingFailed
	if message.Load(&failed) {
		key := requestKey(message.SessionID, failed.RequestID)
		if request, ok := c.requests[key]; ok {
			c.set(request.target, capturedResponse{ProfileID: request.profileID, Error: "matching GraphQL request failed to load"})
			delete(c.requests, key)
		}
		return
	}
	var destroyed proto.TargetTargetDestroyed
	if message.Load(&destroyed) {
		c.forget(destroyed.TargetID)
		for id, target := range c.sessions {
			if target == destroyed.TargetID {
				delete(c.sessions, id)
				for key, request := range c.requests {
					if request.target == target {
						delete(c.requests, key)
					}
				}
			}
		}
	}
}

func isGraphQLRequest(request *proto.NetworkRequest) bool {
	parsed, err := url.Parse(request.URL)
	return err == nil && strings.TrimRight(parsed.Path, "/") == "/api/graphql" && request.Method == "POST"
}

func matchProfileQuery(request *proto.NetworkRequest, post string) (bool, string) {
	form, err := url.ParseQuery(post)
	if err != nil {
		return false, ""
	}
	matches := form.Get("fb_api_req_friendly_name") == profileQueryName
	for name, value := range request.Headers {
		if strings.EqualFold(name, "x-fb-friendly-name") && value.Str() == profileQueryName {
			matches = true
		}
	}
	if !matches {
		return false, ""
	}
	var variables struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(form.Get("variables")), &variables)
	return true, variables.ID
}

// Preserve the complete JSON response, including incremental GraphQL chunks.
// Request headers, POST parameters and session credentials are never persisted.
func parseGraphQLResponse(body []byte, status int) (json.RawMessage, string) {
	body = bytes.TrimSpace(body)
	body = bytes.TrimPrefix(body, []byte("for (;;);"))
	decoder := json.NewDecoder(bytes.NewReader(body))
	var chunks []json.RawMessage
	hasErrors, hasData := false, false
	for {
		var chunk json.RawMessage
		err := decoder.Decode(&chunk)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "matching response was not valid JSON"
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(chunk, &envelope); err != nil || envelope == nil {
			return nil, "matching response was not a GraphQL object"
		}
		if data := envelope["data"]; len(data) > 0 && string(data) != "null" {
			hasData = true
		}
		if fields := envelope["errors"]; len(fields) > 0 && string(fields) != "null" && string(fields) != "[]" {
			hasErrors = true
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) == 0 {
		return nil, "matching response was empty"
	}
	response := chunks[0]
	if len(chunks) > 1 {
		response, _ = json.Marshal(chunks)
	}
	if status < 200 || status >= 300 {
		return response, fmt.Sprintf("matching response returned HTTP %d", status)
	}
	if hasErrors {
		return response, "matching response contains GraphQL errors"
	}
	if !hasData {
		return response, "matching response contains no profile data"
	}
	return response, ""
}

func (c *responseCapture) set(target proto.TargetTargetID, result capturedResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A later response after login can replace an earlier failed response.
	previous, exists := c.results[target]
	if !exists || previous.Error != "" || result.Error == "" {
		c.results[target] = result
	}
}
func (c *responseCapture) snapshot(target proto.TargetTargetID) (capturedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, exists := c.results[target]
	return value, exists
}
func (c *responseCapture) forget(target proto.TargetTargetID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.results, target)
	delete(c.replays, target)
}

func (c *responseCapture) wait(ctx context.Context, page *rod.Page, name string, options runOptions) capturedResponse {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	remaining := options.Timeout
	previous := time.Now()
	started := previous
	replayed := false
	replayError := ""
	for {
		// The user may need arbitrarily long to finish logging in. Login pause
		// time does not count against the capture timeout.
		beforeCheck := time.Now()
		if err := options.login.Check(ctx, page, name); err != nil {
			return capturedResponse{Error: err.Error()}
		}
		remaining -= beforeCheck.Sub(previous)
		previous = time.Now()
		if result, ok := c.snapshot(page.TargetID); ok {
			if result.Error == "" || result.HTTPStatus >= 400 || len(result.Response) > 0 {
				return result
			}
		}
		// Give the initial response a chance to arrive before issuing one
		// fallback request. Preparation can be retried while modules load;
		// an actual network attempt is never retried automatically.
		if !replayed && time.Since(started) >= time.Second && remaining > 0 {
			replayCtx, cancel := context.WithTimeout(ctx, remaining)
			result, attempted, reason := c.replayProfile(replayCtx, page, name)
			cancel()
			replayError = reason
			if attempted {
				replayed = true
				c.set(page.TargetID, result)
				return result
			}
		}
		if remaining <= 0 {
			if result, ok := c.snapshot(page.TargetID); ok {
				return result
			}
			return capturedResponse{Error: "timed out waiting for PolarisProfilePageContentQuery response" + replayError}
		}
		select {
		case <-ctx.Done():
			return capturedResponse{Error: ctx.Err().Error()}
		case <-ticker.C:
		}
	}
}

type profileResult struct {
	Username       string    `json:"username"`
	Link           string    `json:"link"`
	BrowserProfile string    `json:"browser_profile"`
	CapturedAt     time.Time `json:"captured_at"`
	Query          string    `json:"query"`
	Status         string    `json:"status"`
	capturedResponse
}

func saveProfileResult(directory, name string, profile Profile, response capturedResponse) (string, error) {
	response = validateProfileUsername(profile.Username, response)
	status := "ok"
	if response.Error != "" {
		status = "error"
	}
	if response.HTTPStatus == http.StatusTooManyRequests {
		status = "rate_limited"
	} else if strings.HasPrefix(response.Error, "timed out waiting for PolarisProfilePageContentQuery response") {
		status = "missing_response"
	} else if strings.HasPrefix(response.Error, "username mismatch:") {
		status = "username_mismatch"
	} else if strings.HasPrefix(response.Error, "cannot verify response username:") {
		status = "username_unavailable"
	}
	result := profileResult{Username: profile.Username, Link: profile.Link, BrowserProfile: name, CapturedAt: time.Now().UTC(), Query: profileQueryName, Status: status, capturedResponse: response}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode profile result: %w", err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", fmt.Errorf("create results directory: %w", err)
	}
	label := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, profile.Username)
	if len(label) > 32 {
		label = label[:32]
	}
	if label == "" {
		label = "profile"
	}
	hash := sha256.Sum256([]byte(profile.Link))
	destination := filepath.Join(directory, fmt.Sprintf("%s-%x.json", label, hash[:8]))
	file, err := os.CreateTemp(directory, ".profile-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(data, '\n')); err != nil {
		file.Close()
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(file.Name(), destination); err != nil {
		return "", err
	}
	return destination, nil
}
