package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/go-rod/rod"
)

// Match the supplied login dialog using its semantics, not generated classes
// or the Instagram username displayed inside it. Hidden dialogs do not count.
const loginDialogScript = `() => {
const visible = element => element.getClientRects().length && getComputedStyle(element).visibility !== 'hidden';
const instagram = Array.from(document.querySelectorAll('[role="dialog"][aria-modal="true"]')).some(dialog => {
    if (!dialog.getClientRects().length || getComputedStyle(dialog).visibility === 'hidden') return false;
    const text = dialog.innerText.replace(/\s+/g, ' ').toLowerCase();
    const login = Array.from(dialog.querySelectorAll('[role="button"], button, a')).some(button => /^log in$/i.test(button.innerText.trim()));
    return login && text.includes('see photos, videos and more from') && text.includes('sign up');
});
if (instagram) return 'Instagram';
const linkedin = Array.from(document.querySelectorAll('form[action*="/signup/api/cors/createAccount"]')).some(form => {
    const heading = form.querySelector('h1, h2');
    return visible(form) && heading && /join\s+linkedin/i.test(heading.innerText) &&
        form.querySelector('input[name="email-or-phone"]') && form.querySelector('input[type="password"]');
});
return linkedin ? 'LinkedIn' : '';
}`

var errLoginConfirmation = errors.New("login confirmation unavailable")

// One shared pause prevents multiple profiles from prompting or reading stdin
// simultaneously. A generation discards detections made before an Enter press.
type loginGate struct {
	mu         sync.Mutex
	generation uint64
	resume     chan struct{}
	err        error
	site       string
	prompt     func(context.Context, string) error
}

func newLoginGate(input io.Reader, output io.Writer) *loginGate {
	reader := bufio.NewReader(input)
	gate := &loginGate{site: "Instagram"}
	gate.prompt = func(ctx context.Context, name string) error {
		gate.mu.Lock()
		site := gate.site
		gate.mu.Unlock()
		fmt.Fprintf(output, "\n%s login required in %s. Processing is paused for all browser profiles.\n", site, name)
		if site == "LinkedIn" {
			fmt.Fprintln(output, "Log in to LinkedIn in the affected browser profiles.")
		} else {
			fmt.Fprintln(output, "Log in to a DIFFERENT Instagram account in EACH open browser profile.")
		}
		fmt.Fprintln(output, "Keep the current tabs open while logging in. After login, press Enter here to continue.")
		result := make(chan error, 1)
		go func() {
			_, err := reader.ReadString('\n')
			result <- err
		}()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-result:
			if err != nil {
				return fmt.Errorf("read login confirmation: %w", err)
			}
			fmt.Fprintln(output, "Login confirmed; resuming link processing.")
			return nil
		}
	}
	return gate
}

func (g *loginGate) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		g.mu.Lock()
		resume, err := g.resume, g.err
		g.mu.Unlock()
		if err != nil {
			return errors.Join(errLoginConfirmation, err)
		}
		if resume == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resume:
			// A new login pause may have started before this waiter woke up.
		}
	}
}

func (g *loginGate) Check(ctx context.Context, page *rod.Page, name string) error {
	if g == nil {
		return nil
	}
	if err := g.Wait(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	generation := g.generation
	g.mu.Unlock()
	result, err := page.Context(ctx).Eval(loginDialogScript)
	if err != nil {
		return fmt.Errorf("check login dialog: %w", err)
	}
	if site := result.Value.Str(); site != "" {
		g.pauseSite(ctx, name, site, generation)
	}
	return g.Wait(ctx)
}

func (g *loginGate) pause(ctx context.Context, name string, generation uint64) {
	g.pauseSite(ctx, name, "Instagram", generation)
}

func (g *loginGate) generationValue() uint64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generation
}

func (g *loginGate) pauseSite(ctx context.Context, name, site string, generation uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resume != nil || g.err != nil || g.generation != generation {
		return
	}
	resume := make(chan struct{})
	g.site = site
	g.resume = resume
	go func() {
		err := g.prompt(ctx, name)
		g.mu.Lock()
		g.err = err
		g.generation++
		g.resume = nil
		close(resume)
		g.mu.Unlock()
	}()
}
