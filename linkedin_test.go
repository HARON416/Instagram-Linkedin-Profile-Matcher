package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

func TestLinkedInCandidatesLoadExtractAndContinueAfterMissingElement(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TESTS") != "1" {
		t.Skip("set RUN_BROWSER_TESTS=1 to launch local Chrome")
	}
	var visited []string
	var visitedMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/complete-login" {
			http.SetCookie(w, &http.Cookie{Name: "linkedin_logged_in", Value: "1", Path: "/"})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/initial" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "saved", Path: "/"})
			fmt.Fprint(w, "<html><body>Initial profile</body></html>")
			return
		}
		if r.URL.Path == "/login" {
			fmt.Fprint(w, "<html><body>Sign in</body></html>")
			return
		}
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/profile-photo.jpg" {
			w.Header().Set("Content-Type", "image/svg+xml")
			fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20"><rect width="20" height="20" fill="blue"/></svg>`)
			return
		}
		visitedMu.Lock()
		visited = append(visited, r.URL.Path)
		visitedMu.Unlock()
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "saved" {
			t.Error("browser session was not reused")
		}
		if r.URL.Path == "/blocked" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if r.URL.Path == "/missing" {
			fmt.Fprint(w, "<html><body>No top card here</body></html>")
			return
		}
		if r.URL.Path == "/needs-login" {
			if _, err := r.Cookie("linkedin_logged_in"); err != nil {
				fmt.Fprint(w, `<html><body><form class="join-form" action="/signup/api/cors/createAccount" method="post"><h1>Join LinkedIn</h1><input name="email-or-phone"><input name="password" type="password"><button type="button">Sign in</button></form></body></html>`)
				return
			}
		}
		if r.URL.Path == "/busy" {
			fmt.Fprint(w, `<section aria-label="Primary content" aria-busy="true"><h1>Jane Doe</h1><p>Still loading</p></section>`)
			return
		}
		// The primary section arrives after load. A nested scroll container
		// loads two batches only when its bottom comes into view.
		fmt.Fprint(w, `<html><body><aside>Sidebar must be excluded</aside><script>
            setTimeout(() => {
                const nested = location.pathname === '/first';
                const photo = nested ? '<img src="/profile-photo.jpg?size=200&amp;v=1">' : '<svg aria-label="Default profile photo"></svg>';
                document.body.insertAdjacentHTML('beforeend',
                    '<section aria-label="Primary content" style="' + (nested ? 'height:400px;overflow-y:auto' : '') + '"><section><a componentkey="topcard-logo-image-referencekey">' + photo + '</a><h2>Jane Doe</h2><p>Software Engineer</p></section><section><h2>About</h2><p>About text</p></section><section><h2>Experience</h2><p>Experience text</p></section><section><h2>Education</h2><p>Education text</p></section>' + (nested ? '<div style="height:600px">Initial content</div>' : '') + '</section>');
                const primary = document.querySelector('section[aria-label="Primary content"]');
                let batches = 0;
                primary.addEventListener('scroll', () => {
                    if (batches >= 2 || primary.getAttribute('aria-busy') === 'true' || primary.scrollTop + primary.clientHeight < primary.scrollHeight - 2) return;
                    primary.setAttribute('aria-busy', 'true');
                    const batch = ++batches;
                    setTimeout(() => {
                        primary.insertAdjacentHTML('beforeend', '<div style="height:220px"><p>Lazy batch ' + batch + '</p></div>');
                        primary.setAttribute('aria-busy', 'false');
                    }, 700);
                });
            }, 150);
        </script></body></html>`)

	}))
	defer server.Close()
	chrome, found := launcher.LookPath()
	if !found {
		t.Fatal("Chrome/Chromium missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	l := launcher.NewUserMode().Context(ctx).Bin(chrome).UserDataDir(t.TempDir()).RemoteDebuggingPort(0).Leakless(true).Headless(true).NoSandbox(false)
	endpoint, err := l.Launch()
	if err != nil {
		t.Fatal(err)
	}
	browser := rod.New().Context(ctx).ControlURL(endpoint).NoDefaultDevice()
	if err := browser.Connect(); err != nil {
		l.Kill()
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := browser.Context(cleanup).Close(); err != nil {
			l.Kill()
		}
		if err := waitBrowserExit(cleanup, l.PID()); err != nil {
			l.Kill()
		}
	}()
	page, err := browser.Page(proto.TargetCreateTarget{URL: server.URL + "/initial"})
	if err != nil {
		t.Fatal(err)
	}
	if err := page.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	search := &linkedInSearch{Status: "ok", Candidates: []linkedInCandidate{
		{URL: server.URL + "/first"}, {URL: server.URL + "/missing"}, {URL: server.URL + "/blocked"}, {URL: server.URL + "/last"},
	}}
	saves := 0
	options := runOptions{LinkedInTimeout: 8 * time.Second}
	err = visitLinkedInCandidates(ctx, page, "browser-001", search, options, func() error {
		saves++
		pages, err := browser.Pages()
		if err != nil {
			return err
		}
		if len(pages) != 1 {
			return fmt.Errorf("expected one active tab, found %d", len(pages))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if saves != 4 {
		t.Fatalf("got %d incremental saves, want 4", saves)
	}
	for _, index := range []int{0, 3} {
		result := search.Candidates[index].Profile
		if result == nil || result.Status != "ok" || result.Name != "Jane Doe" || !strings.Contains(result.Content, "Software Engineer") || !strings.Contains(result.HTML, "topcard-logo-image-referencekey") || strings.Contains(result.Content, "Sidebar must be excluded") {
			t.Fatalf("incorrect extracted section at %d: %+v", index, result)
		}
	}
	if search.Candidates[1].Profile.Status != "element_missing" || search.Candidates[2].Profile.Status != "login_required" {
		t.Fatalf("candidate failures not classified: %+v", search.Candidates)
	}
	if got := search.Candidates[0].Profile.ProfilePicURL; got != server.URL+"/profile-photo.jpg?size=200&v=1" {
		t.Fatalf("incorrect LinkedIn photo URL: %q", got)
	}
	if got := search.Candidates[3].Profile.ProfilePicURL; got != "" {
		t.Fatalf("default SVG photo must leave the URL empty: %q", got)
	}
	for _, text := range []string{"About text", "Experience text", "Education text", "Lazy batch 1", "Lazy batch 2"} {
		if !strings.Contains(search.Candidates[0].Profile.Content, text) {
			t.Fatalf("primary content is missing %q: %s", text, search.Candidates[0].Profile.Content)
		}
	}

	visitedMu.Lock()
	if len(visited) != 4 || visited[0] != "/first" || visited[3] != "/last" {
		visitedMu.Unlock()
		t.Fatalf("candidates not processed sequentially: %v", visited)
	}
	visitedMu.Unlock()

	busy := readLinkedInProfile(ctx, page, server.URL+"/busy", time.Second, nil, "browser-001")
	if busy.Status != "loading_incomplete" || busy.Content != "" {
		t.Fatalf("unfinished lazy loading must not be saved as a successful capture: %+v", busy)
	}

	input, enter := io.Pipe()
	defer input.Close()
	defer enter.Close()
	var output bytes.Buffer
	gate := newLoginGate(input, &output)
	started := make(chan struct{}, 1)
	original := gate.prompt
	gate.prompt = func(ctx context.Context, name string) error {
		started <- struct{}{}
		return original(ctx, name)
	}
	options.LinkedInTimeout = 4 * time.Second
	options.login = gate
	loginSearch := &linkedInSearch{Status: "ok", Candidates: []linkedInCandidate{{URL: server.URL + "/needs-login"}}}
	done := make(chan error, 1)
	go func() {
		done <- visitLinkedInCandidates(ctx, page, "browser-001", loginSearch, options, func() error { return nil })
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("LinkedIn join form did not trigger the login prompt")
	}
	// Manual login must be allowed to take longer than the page timeout.
	time.Sleep(options.LinkedInTimeout + 100*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("login wait ended before Enter: %v", err)
	default:
	}
	if _, err := page.Eval(`async () => { await fetch('/complete-login'); }`); err != nil {
		t.Fatal(err)
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
		t.Fatal("LinkedIn extraction did not resume after Enter")
	}
	if result := loginSearch.Candidates[0].Profile; result == nil || result.Status != "ok" || result.Name != "Jane Doe" {
		t.Fatalf("profile was not extracted after login: %+v", result)
	}
	if !strings.Contains(output.String(), "LinkedIn login required") || strings.Contains(output.String(), "Instagram account") {
		t.Fatalf("wrong site in login prompt: %s", output.String())
	}
}
