package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

func TestLoginPauseCoalescesProfilesAndWaitsForEnter(t *testing.T) {
	input, enter := io.Pipe()
	defer input.Close()
	defer enter.Close()
	gate := newLoginGate(input, io.Discard)
	var prompts atomic.Int32
	started := make(chan struct{})
	original := gate.prompt
	gate.prompt = func(ctx context.Context, name string) error {
		prompts.Add(1)
		close(started)
		return original(ctx, name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gate.pause(ctx, "browser-001", 0)
	gate.pause(ctx, "browser-002", 0)
	<-started
	done := make(chan error, 1)
	go func() { done <- gate.Wait(ctx) }()
	select {
	case <-done:
		t.Fatal("processing resumed without Enter")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := enter.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A stale detection from another browser must not create a second prompt.
	gate.pause(ctx, "browser-002", 0)
	if prompts.Load() != 1 {
		t.Fatalf("got %d prompts, want one shared prompt", prompts.Load())
	}
}

func TestLoginPauseCancellationAndUnavailableInput(t *testing.T) {
	gate := newLoginGate(strings.NewReader(""), io.Discard)
	gate.pause(context.Background(), "browser-001", 0)
	if err := gate.Wait(context.Background()); !errors.Is(err, io.EOF) || !errors.Is(err, errLoginConfirmation) {
		t.Fatalf("got %v, want terminal input error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	gate = &loginGate{resume: make(chan struct{})}
	cancel()
	if err := gate.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

func TestBrowserLoginDialogPausesAllProfilesUntilEnter(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TESTS") != "1" {
		t.Skip("set RUN_BROWSER_TESTS=1 to launch local Chrome")
	}
	input, enter := io.Pipe()
	defer input.Close()
	defer enter.Close()
	gate := newLoginGate(input, io.Discard)
	started := make(chan struct{}, 4)
	var prompts, visited atomic.Int32
	original := gate.prompt
	gate.prompt = func(ctx context.Context, name string) error {
		prompts.Add(1)
		started <- struct{}{}
		return original(ctx, name)
	}
	firstPages := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "logged_in", Value: r.URL.Query().Get("account"), Path: "/", MaxAge: 3600})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/profile" {
			return
		}
		if visited.Add(1) == 2 {
			close(firstPages)
		}
		select {
		case <-firstPages:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if _, err := r.Cookie("logged_in"); err != nil {
			_, _ = fmt.Fprint(w, `<html><body><div role="dialog" aria-modal="true">See photos, videos and more from someone. Sign up and never miss a post.<div role="button">Sign up</div><div role="button">Log in</div></div></body></html>`)
		} else {
			_, _ = fmt.Fprint(w, "<html><body>Logged in</body></html>")
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	options := runOptions{DataDir: t.TempDir(), BrowserProfiles: 2, Headless: true, Timeout: 10 * time.Second, login: gate}
	var profiles []Profile
	for i := range 4 {
		profiles = append(profiles, Profile{Username: fmt.Sprint(i), Link: fmt.Sprintf("%s/profile?id=%d", server.URL, i)})
	}
	done := make(chan error, 1)
	go func() { done <- runProfiles(ctx, profiles, options) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("pool exited without a login prompt: %v", err)
	case <-ctx.Done():
		t.Fatal("login prompt was not detected")
	}
	time.Sleep(100 * time.Millisecond)
	if visited.Load() != 2 {
		t.Fatalf("queue continued while login was paused: %d visits", visited.Load())
	}
	for id := 1; id <= 2; id++ {
		endpoint, err := launcher.ResolveURL(fmt.Sprint(browserDebugPort(browserProfileDir(options.DataDir, id))))
		if err != nil {
			t.Fatal(err)
		}
		inspector := rod.New().Context(ctx).ControlURL(endpoint)
		if err := inspector.Connect(); err != nil {
			t.Fatal(err)
		}
		pages, err := inspector.Pages()
		if err != nil || len(pages) != 1 {
			t.Fatalf("browser-%03d login tab was not preserved: pages=%d err=%v", id, len(pages), err)
		}
		// Simulate a user's completed login only on this local test site.
		if _, err := pages[0].Eval(`async (account) => { await fetch('/login?account=' + account); document.querySelector('[role="dialog"]').remove(); }`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := enter.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("pool did not resume after Enter: prompts=%d visited=%d", prompts.Load(), visited.Load())
	}
	if prompts.Load() != 1 || visited.Load() != 4 {
		t.Fatalf("prompts=%d visits=%d; want 1 prompt and 4 processed links", prompts.Load(), visited.Load())
	}
}
