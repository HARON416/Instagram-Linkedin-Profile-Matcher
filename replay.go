package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Request values are held only for the lifetime of this tab. Chrome supplies
// cookies, Origin, Referer and browser headers itself on the replay.
type profileReplay struct {
	URL       string            `json:"url"`
	Body      string            `json:"body"`
	Headers   map[string]string `json:"headers"`
	ProfileID string            `json:"profile_id"`
}

func newProfileReplay(request *proto.NetworkRequest, body, id string) profileReplay {
	r := profileReplay{URL: request.URL, Body: body, ProfileID: id, Headers: map[string]string{}}
	for key, value := range request.Headers {
		switch strings.ToLower(key) {
		case "content-type", "x-csrftoken", "x-fb-friendly-name", "x-fb-lsd", "x-ig-app-id", "x-asbd-id":
			r.Headers[strings.ToLower(key)] = value.Str()
		}
	}
	return r
}

const replayProfileScript = `async (template) => {
    const unavailable = reason => ({attempted: false, reason});
    const req = name => { try { return window.require(name); } catch (_) { return null; } };
    const cookie = name => document.cookie.split(';').map(s => s.trim()).find(s => s.startsWith(name + '='))?.slice(name.length + 1);
    let address, form, headers, id;
    if (template) {
        address = new URL(template.url, location.href);
        if (address.origin !== location.origin || address.pathname.replace(/\/$/, '') !== '/api/graphql')
            return unavailable('observed request is not a same-origin GraphQL request');
        form = new URLSearchParams(template.body);
        headers = {...template.headers};
        id = template.profile_id;
    } else {
        if (location.hostname !== 'www.instagram.com' && location.hostname !== 'instagram.com')
            return unavailable('no observed profile request on this origin');
        // Resolve the deployed query rather than copying a doc_id or Relay
        // provider flags from a request made for another person/session.
        const artifact = req('PolarisProfilePageContentQuery.graphql') || req('PolarisProfilePageContentQuery$Parameters');
        const params = artifact?.params || artifact;
        if (!params?.id) return unavailable('live profile query document is unavailable');
        const asyncParams = req('getAsyncParams');
        if (typeof asyncParams !== 'function') return unavailable('live request parameters are unavailable');
        const username = decodeURIComponent(location.pathname.split('/').filter(Boolean)[0] || '').toLowerCase();
        const ids = new Set();
        const remember = value => {
            if (typeof value?.username !== 'string' || value.username.toLowerCase() !== username) return;
            for (const candidate of [value.pk, value.id]) {
                if ((typeof candidate === 'string' || typeof candidate === 'number') && /^\d+$/.test(String(candidate))) ids.add(String(candidate));
            }
        };
        // Hydration moves the server-loaded profile into Relay's normalized
        // store. Read only records for the current username, never the viewer
        // or a suggested account, and wait if hydration has not populated it.
        const environment = req('PolarisRelayEnvironment');
        const source = environment?.getStore?.()?.getSource?.();
        if (source?.getRecordIDs && source?.get) {
            for (const key of source.getRecordIDs()) remember(source.get(key));
        }
        let budget = 100000;
        const walk = value => {
            if (!value || typeof value !== 'object' || --budget <= 0) return;
            remember(value);
            for (const child of Object.values(value)) walk(child);
        };
        for (const script of document.querySelectorAll('script[type="application/json"]')) {
            try { walk(JSON.parse(script.textContent)); } catch (_) {}
        }
        if (ids.size !== 1) return unavailable('current profile ID is missing or ambiguous in Relay and embedded page data');
        id = [...ids][0];
        const variables = {id};
        for (const arg of artifact.operation?.argumentDefinitions || artifact.argumentDefinitions || artifact.fragment?.argumentDefinitions || []) {
            if (arg.kind === 'LocalArgument' && arg.defaultValue != null) variables[arg.name] = arg.defaultValue;
        }
        // Relay's provided variables are part of the loaded query artifact,
        // not ordinary argument defaults (which are null for these flags).
        for (const [name, provider] of Object.entries(params.providedVariables || {})) {
            if (typeof provider?.get !== 'function') return unavailable('live query provider is unavailable');
            variables[name] = provider.get();
        }
        variables.id = id;
        variables.enable_integrity_filters = true;
        form = new URLSearchParams(asyncParams('POST'));
        form.set('fb_api_caller_class', 'RelayModern');
        form.set('fb_api_req_friendly_name', 'PolarisProfilePageContentQuery');
        form.set('server_timestamps', 'true');
        form.set('variables', JSON.stringify(variables));
        form.set('doc_id', params.id);
        headers = {'content-type': 'application/x-www-form-urlencoded', 'x-fb-friendly-name': 'PolarisProfilePageContentQuery'};
        address = new URL('/api/graphql', location.origin);
    }
    // Refresh the session-dependent form fields together, including the
    // fb_dtsg/jazoest pair, while keeping the query's variables and document ID.
    const freshParams = req('getAsyncParams');
    if (template && typeof freshParams === 'function') {
        for (const [key, value] of Object.entries(freshParams('POST'))) {
            if (!['variables', 'doc_id', 'fb_api_req_friendly_name', 'fb_api_caller_class'].includes(key)) form.set(key, value);
        }
    }
    // Refresh readable session tokens immediately before dispatch. Never set a
    // Cookie header: credentials stay under Chrome's cookie policy.
    const csrf = cookie('csrftoken');
    if (csrf) headers['x-csrftoken'] = decodeURIComponent(csrf);
    const lsd = form.get('lsd') || req('LSD')?.token;
    if (lsd) { form.set('lsd', lsd); headers['x-fb-lsd'] = lsd; }
    const appID = req('CurrentAppID')?.getAppID?.();
    if (appID) headers['x-ig-app-id'] = String(appID);
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);
    try {
        const response = await fetch(address.href, {method: 'POST', credentials: 'same-origin', redirect: 'error', headers, body: form.toString(), signal: controller.signal});
        return {attempted: true, status: response.status, id, body: await response.text()};
    } catch (_) {
        return {attempted: true, id, error: 'live profile request failed or timed out'};
    } finally { clearTimeout(timer); }
}`

func (c *responseCapture) replayProfile(ctx context.Context, page *rod.Page, name string) (capturedResponse, bool, string) {
	c.mu.Lock()
	template, exists := c.replays[page.TargetID]
	c.mu.Unlock()
	var input any
	if exists {
		input = template
	}
	value, err := page.Context(ctx).Eval(replayProfileScript, input)
	if err != nil {
		// Evaluation errors can contain script arguments, so never log them.
		return capturedResponse{Error: replayEvaluationError(err)}, true, ""
	}
	var result struct {
		Attempted bool   `json:"attempted"`
		Status    int    `json:"status"`
		ID        string `json:"id"`
		Body      string `json:"body"`
		Error     string `json:"error"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(value.Value.JSON("", "")), &result); err != nil {
		return capturedResponse{Error: "could not decode live profile request result"}, true, ""
	}
	if !result.Attempted {
		return capturedResponse{}, false, "; replay unavailable: " + result.Reason
	}
	log.Printf("[%s] live profile request completed (HTTP %d)", name, result.Status)
	response := capturedResponse{HTTPStatus: result.Status, ProfileID: result.ID, Error: result.Error}
	if response.Error == "" {
		response.Response, response.Error = parseGraphQLResponse([]byte(result.Body), result.Status)
	}
	return response, true, ""
}

func replayEvaluationError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "live profile evaluation timed out; request completion is unknown"
	}
	if errors.Is(err, context.Canceled) {
		return "live profile evaluation was canceled; request completion is unknown"
	}
	var evalError *rod.EvalError
	if errors.As(err, &evalError) {
		return "live profile evaluation raised a JavaScript exception; request completion is unknown"
	}
	return "live profile evaluation failed at the browser connection; request completion is unknown"
}
