package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// Match the supplied primary column by its accessible label rather than
// generated classes or per-profile component IDs.
const linkedInPrimaryReady = `() => {
    const section = document.querySelector('section[aria-label="Primary content"]');
    return !!(section && section.getClientRects().length && section.innerText.trim());
}`

const linkedInPrimaryContent = `() => {
    const section = document.querySelector('section[aria-label="Primary content"]');
    if (!section) return null;
    const marker = section.querySelector('[componentkey="topcard-logo-image-referencekey"]');
    const topcard = marker && marker.closest('section');
    const heading = (topcard || section).querySelector('h1, h2');
    const photo = marker && marker.querySelector('img');
    let profile_pic_url = '';
    const source = photo && (photo.currentSrc || photo.getAttribute('src'));
    if (source) {
        try {
            const url = new URL(source, location.href);
            if (url.protocol === 'https:' || url.protocol === 'http:') profile_pic_url = url.href;
        } catch (_) {}
    }
    return {name: heading ? heading.innerText.trim() : '', content: section.innerText.trim(), html: section.outerHTML, profile_pic_url};
}`

// Advance through both ordinary pages and a primary column inside an overflow
// container. Sampling after each step gives lazy sections time to mount.
const linkedInScrollStep = `() => {
    const section = document.querySelector('section[aria-label="Primary content"]');
    if (!section) return null;
    const containers = [];
    for (let node = section; node && node !== document.body; node = node.parentElement) {
        if (/(auto|scroll)/.test(getComputedStyle(node).overflowY) && node.scrollHeight > node.clientHeight + 2) containers.push(node);
    }
    const root = document.scrollingElement;
    if (root && !containers.includes(root)) containers.push(root);
    let atBottom = true;
    const positions = [];
    for (const node of containers) {
        const bottom = node.scrollTop + node.clientHeight >= node.scrollHeight - 2;
        if (!bottom) {
            node.scrollTop = Math.min(node.scrollHeight, node.scrollTop + Math.max(1, node.clientHeight * 0.8));
            atBottom = false;
        }
        positions.push([node.scrollTop, node.scrollHeight, node.clientHeight]);
    }
    const visible = node => node.getClientRects().length && getComputedStyle(node).visibility !== 'hidden';
    const loading = section.getAttribute('aria-busy') === 'true' || [...section.querySelectorAll('[aria-busy="true"], [role="progressbar"], [role="status"], [data-testid*="loading"], [data-testid*="spinner"]')]
        .some(node => visible(node) && (node.getAttribute('role') !== 'status' || /loading/i.test(node.innerText)));
    return {atBottom, loading, signature: JSON.stringify([positions, section.innerText])};
}`

type linkedInProfileContent struct {
	Status        string    `json:"status"`
	URL           string    `json:"url"`
	FinalURL      string    `json:"final_url,omitempty"`
	Name          string    `json:"name,omitempty"`
	ProfilePicURL string    `json:"profile_pic_url"`
	Content       string    `json:"content,omitempty"`
	HTML          string    `json:"html,omitempty"`
	ExtractedAt   time.Time `json:"extracted_at,omitempty"`
	Error         string    `json:"error,omitempty"`
}

var errLinkedInLoginChanged = errors.New("LinkedIn login changed during lazy loading")

func loadLinkedInPrimary(parent, ctx context.Context, page *rod.Page, gate *loginGate, name string, generation uint64) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	previous := ""
	var settledSince time.Time
	for {
		if err := gate.Check(parent, page.Context(parent), name); err != nil {
			return err
		}
		if gate.generationValue() != generation {
			return errLinkedInLoginChanged
		}
		state, err := page.Context(ctx).Eval(linkedInScrollStep)
		if err != nil || state.Value.Nil() {
			if ctx.Err() != nil {
				return errors.New("LinkedIn primary content did not finish lazy loading before the timeout")
			}
			return errors.New("could not inspect LinkedIn primary content while scrolling")
		}
		signature := state.Value.Get("signature").Str()
		if state.Value.Get("atBottom").Bool() && !state.Value.Get("loading").Bool() && signature == previous {
			if settledSince.IsZero() {
				settledSince = time.Now()
			} else if time.Since(settledSince) >= 2*time.Second {
				return nil
			}
		} else {
			settledSince = time.Time{}
		}
		previous = signature
		select {
		case <-ctx.Done():
			return errors.New("LinkedIn primary content did not finish lazy loading before the timeout")
		case <-ticker.C:
		}
	}
}

func readLinkedInProfile(parent context.Context, page *rod.Page, address string, timeout time.Duration, gate *loginGate, name string) *linkedInProfileContent {
	result := &linkedInProfileContent{Status: "error", URL: address}
	var ctx context.Context
	var cancel context.CancelFunc

navigation:
	for {
		if cancel != nil {
			cancel()
		}
		if err := gate.Wait(parent); err != nil {
			result.Error = err.Error()
			return result
		}
		generation := gate.generationValue()
		ctx, cancel = context.WithTimeout(parent, timeout)
		defer cancel()
		page = page.Context(ctx)
		if err := page.Navigate(address); err != nil {
			result.Error = "LinkedIn navigation failed"
			return result
		}
		if err := page.WaitLoad(); err != nil {
			result.Error = "LinkedIn page did not finish loading before the timeout"
			return result
		}
		ticker := time.NewTicker(200 * time.Millisecond)
		for {
			// Use the run context so manual login is not cut short by the
			// page timeout. After Enter, reload the requested profile with
			// a fresh timeout rather than extracting the authwall or home page.
			if err := gate.Check(parent, page.Context(parent), name); err != nil {
				ticker.Stop()
				result.Error = err.Error()
				return result
			}
			if gate.generationValue() != generation {
				ticker.Stop()
				continue navigation
			}
			if info, err := page.Info(); err == nil {
				result.FinalURL = info.URL
				parsed, _ := url.Parse(info.URL)
				if parsed != nil && (strings.HasPrefix(parsed.Path, "/login") || strings.HasPrefix(parsed.Path, "/authwall") || strings.HasPrefix(parsed.Path, "/checkpoint")) {
					ticker.Stop()
					result.Status = "login_required"
					result.Error = "LinkedIn requires login or verification; the supplied join form was not present"
					return result
				}
			}
			ready, err := page.Eval(linkedInPrimaryReady)
			if err == nil && ready.Value.Bool() {
				ticker.Stop()
				if err := loadLinkedInPrimary(parent, ctx, page, gate, name, generation); err != nil {
					if err == errLinkedInLoginChanged {
						continue navigation
					}
					result.Status = "loading_incomplete"
					result.Error = err.Error()
					return result
				}
				break navigation
			}
			select {
			case <-ctx.Done():
				ticker.Stop()
				result.Status = "element_missing"
				result.Error = "LinkedIn Primary content section did not become visible before the timeout"
				return result
			case <-ticker.C:
			}
		}
	}
	content, err := page.Eval(linkedInPrimaryContent)
	if err != nil || content.Value.Nil() {
		result.Error = "could not extract LinkedIn Primary content"
		return result
	}
	result.Name = content.Value.Get("name").Str()
	result.ProfilePicURL = content.Value.Get("profile_pic_url").Str()
	result.Content = content.Value.Get("content").Str()
	result.HTML = content.Value.Get("html").Str()
	if result.Content == "" {
		result.Error = "LinkedIn Primary content section was empty"
		return result
	}
	result.Status = "ok"
	result.ExtractedAt = time.Now().UTC()
	return result
}

// Reuse the current tab and session, so each browser handles one page at a time.
func visitLinkedInCandidates(ctx context.Context, page *rod.Page, name string, search *linkedInSearch, options runOptions, save func() error) error {
	if search == nil || search.Status != "ok" {
		return nil
	}
	for i := range search.Candidates {
		if err := options.login.Wait(ctx); err != nil {
			return err
		}
		delay := randomVisitDelay(options.LinkedInDelayMin, options.LinkedInDelayMax)
		if delay > 0 {
			log.Printf("[%s] waiting %s before LinkedIn visit", name, delay.Round(time.Millisecond))
		}
		if err := waitVisitDelay(ctx, delay); err != nil {
			return err
		}
		if err := options.login.Wait(ctx); err != nil {
			return err
		}
		candidate := &search.Candidates[i]
		log.Printf("[%s] opening LinkedIn candidate: %s", name, candidate.URL)
		candidate.Profile = readLinkedInProfile(ctx, page, candidate.URL, options.LinkedInTimeout, options.login, name)
		if err := save(); err != nil {
			return fmt.Errorf("save LinkedIn profile content: %w", err)
		}
		if candidate.Profile.Error != "" {
			log.Printf("[%s] LinkedIn profile content: %s; url=%s; reason=%s", name, candidate.Profile.Status, candidate.URL, candidate.Profile.Error)
		} else {
			log.Printf("[%s] LinkedIn profile content: %s", name, candidate.Profile.Status)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}
