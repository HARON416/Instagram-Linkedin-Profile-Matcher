package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

func TestPoolProcessesEachLinkOnceWithOneActiveLinkPerSession(t *testing.T) {
	profiles := make([]Profile, 12)
	for i := range profiles {
		profiles[i] = Profile{Username: fmt.Sprint(i), Link: fmt.Sprint(i)}
	}
	var opened, closed atomic.Int32
	active := make([]atomic.Int32, 2)
	var mu sync.Mutex
	visits := make(map[string]int)
	failure := errors.New("page failed")
	barrier := make(chan struct{})
	var started atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := processProfilePool(ctx, profiles, 2, func(_ context.Context, id int) (profileWorker, error) {
		opened.Add(1)
		return profileWorker{
			Close: func() { closed.Add(1) },
			Visit: func(ctx context.Context, profile Profile) error {
				if opened.Load() != 2 {
					t.Error("link processed before all browsers initialized")
				}
				if active[id-1].Add(1) != 1 {
					t.Error("concurrent links in one browser session")
				}
				defer active[id-1].Add(-1)
				if started.Add(1) == 2 {
					close(barrier)
				}
				select {
				case <-barrier:
				case <-ctx.Done():
					return ctx.Err()
				}
				mu.Lock()
				visits[profile.Link]++
				mu.Unlock()
				if profile.Link == "4" {
					return failure
				}
				return nil
			},
		}, nil
	})
	if !errors.Is(err, failure) {
		t.Fatalf("got %v, want page failure", err)
	}
	if opened.Load() != 2 || closed.Load() != 2 {
		t.Fatalf("opened=%d closed=%d; want 2 persistent workers", opened.Load(), closed.Load())
	}
	for _, profile := range profiles {
		if visits[profile.Link] != 1 {
			t.Errorf("link %s processed %d times", profile.Link, visits[profile.Link])
		}
	}
}

func TestBrowserOpensOnlyTheAssignedLinkWithoutBlankTabs(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TESTS") != "1" {
		t.Skip("set RUN_BROWSER_TESTS=1 to launch local Chrome")
	}
	chrome, found := launcher.LookPath()
	if !found {
		t.Fatal("Chrome/Chromium not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	options := runOptions{Chrome: chrome, DataDir: t.TempDir(), Headless: true, Timeout: 10 * time.Second}
	worker, err := openBrowserProfile(ctx, 1, options)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	endpoint, err := launcher.ResolveURL(fmt.Sprint(browserDebugPort(browserProfileDir(options.DataDir, 1))))
	if err != nil {
		t.Fatal(err)
	}
	inspector := rod.New().Context(ctx).ControlURL(endpoint)
	if err := inspector.Connect(); err != nil {
		t.Fatal(err)
	}
	assertNoTabs := func() {
		t.Helper()
		pages, err := inspector.Pages()
		if err != nil || len(pages) != 0 {
			t.Fatalf("expected no spare tabs; pages=%d, err=%v", len(pages), err)
		}
	}
	assertNoTabs()
	var visited atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profile" {
			return
		}
		pages, err := inspector.Pages()
		if err != nil || len(pages) != 1 {
			t.Errorf("expected only the assigned link tab; pages=%d, err=%v", len(pages), err)
		} else if info, err := pages[0].Info(); err != nil || info.URL == "about:blank" {
			t.Errorf("profile tab was blank or unavailable: %v", err)
		}
		visited.Add(1)
		_, _ = w.Write([]byte("<html><body>Profile</body></html>"))
	}))
	defer server.Close()
	for range 2 {
		if err := worker.Visit(ctx, Profile{Username: "test", Link: server.URL + "/profile"}); err != nil {
			t.Fatal(err)
		}
		assertNoTabs()
	}
	if visited.Load() != 2 {
		t.Fatalf("got %d visits, want 2", visited.Load())
	}
}

func TestPoolCancellationStopsQueuedLinks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 10)
	done := make(chan error, 1)
	var closed atomic.Int32
	go func() {
		done <- processProfilePool(ctx, make([]Profile, 10), 1, func(context.Context, int) (profileWorker, error) {
			return profileWorker{
				Close: func() { closed.Add(1) },
				Visit: func(ctx context.Context, _ Profile) error { started <- struct{}{}; <-ctx.Done(); return ctx.Err() },
			}, nil
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	select {
	case <-started:
		t.Fatal("a second link started while the first was active")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	if len(started) != 0 || closed.Load() != 1 {
		t.Fatal("queue continued or session was not closed")
	}
}

func TestPoolReportsLinksLeftAfterAllLaunchesFail(t *testing.T) {
	failure := errors.New("browser launch failed")
	err := processProfilePool(context.Background(), []Profile{{Link: "one"}}, 2, func(context.Context, int) (profileWorker, error) {
		return profileWorker{}, failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("got %v, want launch failure", err)
	}
}

func TestBrowserOptionsRejectInvalidValues(t *testing.T) {
	valid := runOptions{DataDir: "data", BrowserProfiles: 2, Timeout: time.Second}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, options := range []runOptions{
		{DataDir: "data", BrowserProfiles: 0, Timeout: time.Second},
		{DataDir: "data", BrowserProfiles: -1, Timeout: time.Second},
		{DataDir: "data", BrowserProfiles: 1, Timeout: 0},
		{DataDir: "data", BrowserProfiles: 1, Timeout: time.Second, Headless: true, Hold: true},
		{BrowserProfiles: 1, Timeout: time.Second},
	} {
		if options.validate() == nil {
			t.Errorf("expected error for %+v", options)
		}
	}
}

func TestStableBrowserDirectoriesAndUniqueLinkQueue(t *testing.T) {
	root := t.TempDir()
	if directory := browserProfileDir(root, 1); directory != filepath.Join(root, "browser-001") || directory == browserProfileDir(root, 2) {
		t.Fatalf("unexpected session directory: %s", directory)
	}
	first := Profile{Username: "first", Link: "https://example.com/one"}
	duplicate := Profile{Username: "different label", Link: first.Link}
	second := Profile{Username: "second", Link: "https://example.com/two"}
	if got := uniqueProfiles([]Profile{first, duplicate, second}); !reflect.DeepEqual(got, []Profile{first, second}) {
		t.Fatalf("unexpected unique link queue: %v", got)
	}
}

func TestBrowserPoolIsolatesAndPersistsSessionsAcrossLinks(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TESTS") != "1" {
		t.Skip("set RUN_BROWSER_TESTS=1 to launch local Chrome")
	}
	var newSessions, visited, reused, storageChecks, invalidStorage atomic.Int32
	barrier := make(chan struct{})
	var mu sync.Mutex
	sessions := make(map[string]int)
	links := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/storage" {
			storageChecks.Add(1)
			cookie, err := r.Cookie("login")
			if err != nil || r.URL.Query().Get("value") != cookie.Value {
				invalidStorage.Add(1)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/profile" {
			return
		}
		n := visited.Add(1)
		token := ""
		if cookie, err := r.Cookie("login"); err == nil {
			token = cookie.Value
			reused.Add(1)
		} else {
			token = fmt.Sprintf("session-%d", newSessions.Add(1))
		}
		mu.Lock()
		sessions[token]++
		links[r.URL.Query().Get("link")]++
		mu.Unlock()
		if n == 2 {
			close(barrier)
		}
		select {
		case <-barrier:
		case <-r.Context().Done():
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "login", Value: token, Path: "/", MaxAge: 3600})
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>
            const stored = localStorage.getItem('login');
            if (stored === null) localStorage.setItem('login', %q);
            const image = document.createElement('img');
            image.src = '/storage?value=' + encodeURIComponent(localStorage.getItem('login')) + '&link=%s';
            document.body.appendChild(image);
        </script></body></html>`, token, r.URL.Query().Get("link"))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	options := runOptions{DataDir: t.TempDir(), Headless: true, BrowserProfiles: 2, Timeout: 10 * time.Second}
	var profiles []Profile
	for i := 0; i < 6; i++ {
		profiles = append(profiles, Profile{Username: fmt.Sprint(i), Link: fmt.Sprintf("%s/profile?link=%d", server.URL, i)})
	}
	profiles = append(profiles, profiles[0])
	for range 2 {
		if err := runProfiles(ctx, profiles, options); err != nil {
			t.Fatal(err)
		}
	}
	if visited.Load() != 12 || newSessions.Load() != 2 || reused.Load() != 10 || storageChecks.Load() != 12 || invalidStorage.Load() != 0 {
		t.Fatalf("visits=%d new sessions=%d reused=%d storage checks=%d invalid storage=%d", visited.Load(), newSessions.Load(), reused.Load(), storageChecks.Load(), invalidStorage.Load())
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	for link, count := range links {
		if count != 2 {
			t.Errorf("link %s visited %d times, want once per run", link, count)
		}
	}
	directories, err := os.ReadDir(options.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(directories) != 2 {
		t.Fatalf("got %d session directories, want 2", len(directories))
	}
}
