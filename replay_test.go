package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

func TestReplayEvaluationErrorsAreUsefulWithoutExposingDetails(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timed out"},
		{context.Canceled, "canceled"},
		{errors.New("private-cookie-or-request-value"), "browser connection"},
	} {
		message := replayEvaluationError(test.err)
		if !strings.Contains(message, test.want) || strings.Contains(message, "private-cookie") {
			t.Fatalf("unexpected sanitized error: %s", message)
		}
	}
}

func TestBrowserReplayUsesHydratedRelayAndProviderValues(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TESTS") != "1" {
		t.Skip("set RUN_BROWSER_TESTS=1 to launch local Chrome")
	}
	chrome, found := launcher.LookPath()
	if !found {
		t.Fatal("Chrome/Chromium not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	options := runOptions{Chrome: chrome, DataDir: t.TempDir(), Headless: true}
	worker, err := openBrowserProfile(ctx, 1, options)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	endpoint, err := launcher.ResolveURL(fmt.Sprint(browserDebugPort(browserProfileDir(options.DataDir, 1))))
	if err != nil {
		t.Fatal(err)
	}
	browser := rod.New().Context(ctx).ControlURL(endpoint)
	if err := browser.Connect(); err != nil {
		t.Fatal(err)
	}
	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	// The current profile is absent from embedded JSON, as on the live site.
	// Substitute only the surrounding page environment; execute the actual
	// production script and assert its outgoing form without contacting Instagram.
	script := `(mode) => {
        let calls = 0;
        const location = {hostname:'www.instagram.com', origin:'https://www.instagram.com', pathname:'/target/'};
        const document = {cookie:'csrftoken=fresh-csrf', querySelectorAll:()=>[]};
        const users = [{id:'999',username:'viewer'}, {id:'123',pk:'123',username:'target'}];
        if (mode === 'ambiguous') users.push({id:'456',username:'target'});
        const providers = {live_flag:{get:()=>false}};
        const window = {require:name=>{
            if (name === 'PolarisProfilePageContentQuery.graphql') return {params:{id:'live-doc',providedVariables:providers},operation:{argumentDefinitions:[{kind:'LocalArgument',name:'live_flag',defaultValue:null}]}};
            if (name === 'PolarisRelayEnvironment') return {getStore:()=>({getSource:()=>({getRecordIDs:()=>mode === 'loading' ? [] : users.map((_,i)=>i),get:i=>users[i]})})};
            if (name === 'getAsyncParams') return ()=>({fb_dtsg:'fresh-dtsg',jazoest:'paired-value',lsd:'fresh-lsd',__rev:'live-revision'});
            if (name === 'CurrentAppID') return {getAppID:()=> 'live-app'};
            return null;
        }};
        const fetch = async (url, options) => {
            calls++;
            const form = new URLSearchParams(options.body);
            const variables = JSON.parse(form.get('variables'));
            if (url !== 'https://www.instagram.com/api/graphql' || variables.id !== '123' || variables.live_flag !== false ||
                variables.enable_integrity_filters !== true || form.get('doc_id') !== 'live-doc' ||
                form.get('fb_dtsg') !== 'fresh-dtsg' || form.get('jazoest') !== 'paired-value' ||
                options.headers['x-ig-app-id'] !== 'live-app' || options.headers['x-csrftoken'] !== 'fresh-csrf' ||
                options.credentials !== 'same-origin') throw Error('incorrect request');
            return {status:200,text:async()=>JSON.stringify({data:{user:{username:'target',id:'123'}}})};
        };
        return (` + replayProfileScript + `)(null).then(result=>({...result,calls}));
    }`
	for _, mode := range []string{"ready", "ambiguous", "loading"} {
		t.Run(mode, func(t *testing.T) {
			value, err := page.Eval(script, mode)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ready" {
				if value.Value.Get("status").Int() != 200 || value.Value.Get("calls").Int() != 1 {
					t.Fatalf("replay did not use live Relay values: %s", value.Value.JSON("", ""))
				}
			} else if value.Value.Get("attempted").Bool() || value.Value.Get("calls").Int() != 0 {
				t.Fatalf("request sent without an unambiguous target ID: %s", value.Value.JSON("", ""))
			}
		})
	}
}

func TestReplayDoesNotCopyBrowserManagedHeaders(t *testing.T) {
	request := &proto.NetworkRequest{URL: "https://www.instagram.com/api/graphql", Headers: proto.NetworkHeaders{
		"Cookie": gson.New("session-secret"), "Authorization": gson.New("secret"),
		"User-Agent": gson.New("browser"), "X-CSRFToken": gson.New("live-token"),
	}}
	replay := newProfileReplay(request, "live-body", "123")
	if len(replay.Headers) != 1 || replay.Headers["x-csrftoken"] != "live-token" {
		t.Fatal("replay copied unexpected headers")
	}
}
