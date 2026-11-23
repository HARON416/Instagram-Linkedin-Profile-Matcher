package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

type runOptions struct {
	Chrome           string
	DataDir          string
	Headless         bool
	Hold             bool
	BrowserProfiles  int
	Timeout          time.Duration
	DelayMin         time.Duration
	DelayMax         time.Duration
	ResultsDir       string
	LinkedIn         bool
	LinkedInProfiles bool
	LinkedInTimeout  time.Duration
	LinkedInDelayMin time.Duration
	LinkedInDelayMax time.Duration
	SearchTimeout    time.Duration
	SearchInterval   time.Duration
	Matching         bool
	MatchModel       string
	MatchTimeout     time.Duration
	MatchThreshold   float64
	MatchMargin      float64
	MatchesFile      string
	matcher          *jevClient
	matches          *matchExport
	search           *tinyFishClient
	login            *loginGate
	capture          *responseCapture
}

func (o runOptions) validate() error {
	if o.BrowserProfiles < 1 {
		return errors.New("browser-profiles must be at least 1")
	}
	if o.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if o.DelayMin < 0 || o.DelayMax < o.DelayMin {
		return errors.New("Instagram visit delays require 0 <= instagram-delay-min <= instagram-delay-max")
	}
	if o.LinkedInDelayMin < 0 || o.LinkedInDelayMax < o.LinkedInDelayMin {
		return errors.New("LinkedIn visit delays require 0 <= linkedin-delay-min <= linkedin-delay-max")
	}
	if o.Headless && o.Hold {
		return errors.New("headless mode requires -hold=false")
	}
	if o.DataDir == "" {
		return errors.New("browser data directory must not be empty")
	}
	if o.LinkedIn && (o.ResultsDir == "" || o.SearchTimeout <= 0 || o.SearchInterval < 0) {
		return errors.New("LinkedIn search requires capture, a positive search-timeout, and nonnegative search-interval")
	}
	if o.LinkedInProfiles && (!o.LinkedIn || o.LinkedInTimeout <= 0) {
		return errors.New("LinkedIn profile extraction requires LinkedIn search and a positive linkedin-timeout")
	}
	if o.Matching && (!o.LinkedInProfiles || strings.TrimSpace(o.MatchModel) == "" || o.MatchTimeout <= 0 ||
		o.MatchThreshold < 0 || o.MatchThreshold > 100 || o.MatchMargin < 0 || o.MatchMargin > 100 ||
		math.IsNaN(o.MatchThreshold) || math.IsNaN(o.MatchMargin)) {
		return errors.New("matching requires LinkedIn profile extraction, a model, a positive match-timeout, and threshold/margin between 0 and 100")
	}
	return nil
}

func runProfiles(ctx context.Context, profiles []Profile, options runOptions) (runErr error) {
	if err := options.validate(); err != nil {
		return err
	}
	if options.LinkedIn && options.search == nil {
		key := strings.TrimSpace(os.Getenv("TINYFISH_API_KEY"))
		if key == "" {
			return errors.New("set TINYFISH_API_KEY for LinkedIn search, or pass -linkedin-search=false")
		}
		options.search = newTinyFishClient(key, options.SearchTimeout, options.SearchInterval)
	}
	profiles = uniqueProfiles(profiles)
	if len(profiles) == 0 {
		return errors.New("no links to process")
	}
	if options.Matching {
		if options.matcher == nil {
			key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
			if key == "" {
				return errors.New("set OPENROUTER_API_KEY for Jev text matching, or pass -match=false")
			}
			options.matcher = newJevClient(key, options.MatchModel, options.MatchTimeout)
		}
		if options.matches == nil {
			if options.MatchesFile == "" {
				options.MatchesFile = filepath.Join(options.ResultsDir, "matches-"+time.Now().UTC().Format("20060102-150405.000000000")+".xlsx")
			}
			var err error
			options.matches, err = newMatchExport(options.MatchesFile)
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, options.matches.Close()) }()
			log.Printf("Jev text matching enabled; matches XLSX: %s", options.MatchesFile)
		}
	}
	if options.Chrome == "" {
		var found bool
		options.Chrome, found = launcher.LookPath()
		if !found {
			return errors.New("Chrome/Chromium was not found; install a browser or pass -chrome /path/to/browser")
		}
	}
	root, err := filepath.Abs(options.DataDir)
	if err != nil {
		return fmt.Errorf("resolve session root: %w", err)
	}
	options.DataDir = root
	if options.login == nil {
		options.login = newLoginGate(os.Stdin, os.Stderr)
	}
	log.Printf("Processing %d unique links with %d browser profiles; session root: %s", len(profiles), options.BrowserProfiles, root)
	if options.Hold {
		log.Print("Close the current link tab to advance that browser's queue; Ctrl+C stops all browser profiles.")
	}
	return processProfilePool(ctx, profiles, options.BrowserProfiles, func(ctx context.Context, id int) (profileWorker, error) {
		return openBrowserProfile(ctx, id, options)
	})
}

// A link is attempted once per run even when it appears in multiple XLSX rows.
func uniqueProfiles(profiles []Profile) []Profile {
	seen := make(map[string]bool)
	var unique []Profile
	for _, profile := range profiles {
		if !seen[profile.Link] {
			seen[profile.Link] = true
			unique = append(unique, profile)
		}
	}
	return unique
}

func browserProfileDir(root string, id int) string {
	return filepath.Join(root, fmt.Sprintf("browser-%03d", id))
}

type profileWorker struct {
	Visit func(context.Context, Profile) error
	Close func()
}

var errBrowserClosed = errors.New("browser profile was closed")

// Each worker opens its browser once, then visits links serially. All browsers
// finish initialization before links enter the queue.
func processProfilePool(ctx context.Context, profiles []Profile, count int, open func(context.Context, int) (profileWorker, error)) error {
	if count < 1 {
		return errors.New("browser-profiles must be at least 1")
	}
	jobs := make(chan Profile, len(profiles))
	for _, profile := range profiles {
		jobs <- profile
	}
	close(jobs)
	var workers, initialized sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	record := func(err error) {
		log.Print(err)
		mu.Lock()
		failures = append(failures, err)
		mu.Unlock()
	}
	ready := make(chan struct{})
	initialized.Add(count)
	for id := 1; id <= count; id++ {
		workers.Go(func() {
			worker, err := open(ctx, id)
			initialized.Done()
			if err != nil {
				record(fmt.Errorf("browser-%03d: %w", id, err))
				return
			}
			defer worker.Close()
			select {
			case <-ready:
			case <-ctx.Done():
				return
			}
			for {
				if ctx.Err() != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case profile, ok := <-jobs:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						return
					}
					if err := worker.Visit(ctx, profile); err != nil {
						if ctx.Err() != nil {
							return
						}
						record(fmt.Errorf("browser-%03d, link %s (%s): %w", id, profile.Link, profile.Username, err))
						if errors.Is(err, errBrowserClosed) || errors.Is(err, errLoginConfirmation) || errors.Is(err, errProfileRateLimited) {
							return
						}
					}
				}
			}
		})
	}
	initialized.Wait()
	close(ready)
	workers.Wait()
	if ctx.Err() != nil {
		return errors.Join(append(failures, ctx.Err())...)
	}
	if remaining := len(jobs); remaining > 0 {
		failures = append(failures, fmt.Errorf("%d links remain unprocessed because no browser profiles are available", remaining))
	}
	return errors.Join(failures...)
}

func openBrowserProfile(parent context.Context, id int, options runOptions) (profileWorker, error) {
	ctx, stop := context.WithCancel(parent)
	name := fmt.Sprintf("browser-%03d", id)
	dataDir := browserProfileDir(options.DataDir, id)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		stop()
		return profileWorker{}, fmt.Errorf("create session directory: %w", err)
	}
	// Use the interactive preset rather than Rod's automation/testing defaults.
	// Assign a nonzero port to each persistent profile and reject occupied
	// ports rather than attaching to an unrelated running browser.
	port := browserDebugPort(dataDir)
	if err := checkBrowserDebugPort(port); err != nil {
		stop()
		return profileWorker{}, err
	}
	l := launcher.NewUserMode().Context(ctx).Bin(options.Chrome).
		UserDataDir(dataDir).RemoteDebuggingPort(port).
		Leakless(true).Headless(options.Headless).Devtools(false).
		NoSandbox(true).Set("disable-notifications").Set("no-first-run")
	controlURL, err := l.Launch()
	if err != nil {
		stop()
		l.Kill()
		return profileWorker{}, fmt.Errorf("launch browser: %w", err)
	}
	browser := rod.New().ControlURL(controlURL).Context(ctx).NoDefaultDevice()
	if err := browser.Connect(); err != nil {
		l.Kill()
		stop()
		return profileWorker{}, fmt.Errorf("connect browser: %w", err)
	}
	if options.ResultsDir != "" {
		capture, err := startResponseCapture(ctx, browser)
		if err != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = browser.Context(closeCtx).Close()
			_ = waitBrowserExit(closeCtx, l.PID())
			cancel()
			stop()
			return profileWorker{}, err
		}
		options.capture = capture
	}
	closed := false
	visited := false
	log.Printf("[%s] browser started; debugging port: %d; persistent session: %s", name, port, dataDir)
	return profileWorker{
		Visit: func(ctx context.Context, profile Profile) error {
			if visited {
				delay := randomVisitDelay(options.DelayMin, options.DelayMax)
				if delay > 0 {
					log.Printf("[%s] waiting %s before the next profile link", name, delay.Round(time.Millisecond))
				}
				if err := waitVisitDelay(ctx, delay); err != nil {
					return err
				}
			}
			if err := options.login.Wait(ctx); err != nil {
				return err
			}
			visited = true
			err := runWorkflow(ctx, browser, name, profile, options)
			if err != nil && ctx.Err() == nil && !errors.Is(err, errBrowserClosed) && !errors.Is(err, errLoginConfirmation) {
				// A window can close during navigation, before the hold loop sees
				// its disconnect event. Retire that worker instead of failing
				// every remaining link against the disconnected browser.
				probeCtx, cancel := context.WithTimeout(ctx, time.Second)
				_, probeErr := (proto.BrowserGetVersion{}).Call(browser.Context(probeCtx))
				cancel()
				if probeErr != nil {
					err = errors.Join(errBrowserClosed, err)
				}
			}
			if errors.Is(err, errBrowserClosed) {
				closed = true
			}
			return err
		},
		Close: func() {
			defer stop()
			if options.capture != nil {
				defer func() { options.capture.stop(); <-options.capture.done }()
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if !closed {
				if err := browser.Context(closeCtx).Close(); err != nil {
					log.Printf("[%s] close browser: %v", name, err)
					l.Kill()
				}
			}
			// Close acknowledges before Chrome finishes saving its session.
			if err := waitBrowserExit(closeCtx, l.PID()); err != nil {
				log.Printf("[%s] wait for browser exit: %v", name, err)
				l.Kill()
			}
			// Never call launcher.Cleanup: persistent sessions must survive.
		},
	}, nil
}

func waitBrowserExit(ctx context.Context, pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer process.Release()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := process.Signal(syscall.Signal(0))
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runWorkflow(ctx context.Context, browser *rod.Browser, name string, profile Profile, options runOptions) error {
	eventCtx, stop := context.WithCancel(ctx)
	defer stop()
	events := browser.Context(eventCtx).Event()
	navCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	// Browser.Page creates about:blank before navigating. Create the target
	// directly at the profile URL so the user never gets a spare blank tab.
	target, err := (proto.TargetCreateTarget{URL: profile.Link}).Call(browser.Context(navCtx))
	if err != nil {
		return fmt.Errorf("create link tab: %w", err)
	}
	page, err := browser.Context(navCtx).PageFromTarget(target.TargetID)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = (proto.TargetCloseTarget{TargetID: target.TargetID}).Call(browser.Context(closeCtx))
		return fmt.Errorf("connect link tab: %w", err)
	}
	if options.capture != nil {
		defer options.capture.forget(page.TargetID)
	}
	defer func() {
		// Keep login tabs available in every session while the global prompt
		// waits for confirmation. Cancellation still permits normal cleanup.
		_ = options.login.Wait(ctx)
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = page.Context(closeCtx).Close()
	}()
	log.Printf("[%s] processing %s: %s", name, profile.Username, profile.Link)
	// The target can be attached before its initial navigation commits. Wait
	// for that commit so WaitLoad cannot mistake the initial document for a
	// completed profile page and close the tab too early.
	if err := page.Wait(rod.Eval(`() => window.location.href !== "about:blank"`)); err != nil {
		return fmt.Errorf("wait for navigation: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return fmt.Errorf("wait for page: %w", err)
	}
	cancel()
	if err := options.login.Check(ctx, page, name); err != nil {
		return err
	}
	if options.capture != nil {
		response := options.capture.wait(ctx, page.Context(ctx), name, options)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		response = validateProfileUsername(profile.Username, response)
		if options.LinkedIn {
			response.LinkedInSearch = enrichLinkedIn(ctx, options.search, response)
			log.Printf("[%s] LinkedIn search: %s", name, response.LinkedInSearch.Status)
		}
		filename, err := saveProfileResult(options.ResultsDir, name, profile, response)
		if err != nil {
			return err
		}
		log.Printf("[%s] profile response saved: %s", name, filename)
		var visitErr error
		if response.Error == "" && options.LinkedInProfiles {
			visitErr = visitLinkedInCandidates(ctx, page.Context(ctx), name, response.LinkedInSearch, options, func() error {
				_, err := saveProfileResult(options.ResultsDir, name, profile, response)
				return err
			})
		}
		if options.Matching {
			response.Match = options.matcher.Match(ctx, response, options.MatchThreshold, options.MatchMargin)
			if _, err := saveProfileResult(options.ResultsDir, name, profile, response); err != nil {
				return err
			}
			if err := options.matches.Write(profile, response.Match); err != nil {
				return fmt.Errorf("write matches XLSX: %w", err)
			}
			log.Printf("[%s] Jev text match: %s; candidate=%s; reason=%s", name, response.Match.Status, response.Match.LinkedInURL, response.Match.Error)
		}
		if visitErr != nil {
			return visitErr
		}
		if response.HTTPStatus == 429 {
			return errProfileRateLimited
		}
		if response.Error != "" {
			return fmt.Errorf("capture profile response: %s", response.Error)
		}
	}
	if !options.Hold {
		if options.capture != nil {
			return options.login.Wait(ctx)
		}
		// Instagram may insert its login dialog after the load event.
		if options.login != nil {
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-deadline.C:
					return options.login.Wait(ctx)
				case <-ticker.C:
					if err := options.login.Check(ctx, page, name); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	log.Printf("[%s] ready; close this link tab to process the next link", name)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := options.login.Check(ctx, page, name); err != nil {
				return err
			}
		case event, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errBrowserClosed
			}
			var destroyed proto.TargetTargetDestroyed
			if event.Load(&destroyed) && destroyed.TargetID == page.TargetID {
				return nil
			}
		}
	}
}
