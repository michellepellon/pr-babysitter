// ABOUTME: The model gateway: a reverse proxy on 127.0.0.1 that gives the agent the Messages API
// ABOUTME: through a WIF token it never sees, forwarding only bodies that pass an allowlist (spec §4).

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// allowedFields are the top-level fields Claude Code sent in Task 0. Others, such
// as mcp_servers and container, can make Anthropic's servers reach the network.
var allowedFields = map[string]bool{"max_tokens": true, "messages": true, "metadata": true, "model": true,
	"output_config": true, "stream": true, "system": true, "thinking": true, "tools": true}

// forwardHeaders are the only request headers sent upstream; the rest are dropped,
// not refused, so SDK telemetry such as x-stainless-* can't fail a round. Task 0
// recorded only anthropic-beta and user-agent; the SDK always sends
// anthropic-version. The e2e suite must confirm this list. Content-Length comes
// from the request's ContentLength, and with Accept-Encoding dropped Go's
// transport asks for gzip and decodes it as it streams, as Task 0's proxy did.
var forwardHeaders = []string{"Authorization", "Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta", "User-Agent"}

const maxRequests = 400 // per round; each round runs its own gateway

type gateway struct {
	proxy              *httputil.ReverseProxy
	upstream           *url.URL
	oidcURL, oidcToken string            // the job's ACTIONS_ID_TOKEN_REQUEST_* values
	ids                map[string]string // federation rule, organization, service account, workspace
	now                func() time.Time
	log                *log.Logger

	requests  atomic.Int64
	mu        sync.Mutex // guards token and refreshAt
	token     string
	refreshAt time.Time
}

func newGateway(upstream *url.URL, oidcURL, oidcToken string, ids map[string]string, logw io.Writer) *gateway {
	l := log.New(logw, "gateway: ", log.LstdFlags)
	return &gateway{upstream: upstream, oidcURL: oidcURL, oidcToken: oidcToken, ids: ids, now: time.Now, log: l,
		proxy: &httputil.ReverseProxy{ErrorLog: l, Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Header = http.Header{}
			for _, k := range forwardHeaders {
				r.Out.Header[k] = r.In.Header[k]
			}
		}}}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.requests.Add(1) > maxRequests {
		http.Error(w, "gateway: request limit for this round reached", http.StatusTooManyRequests)
		return
	}
	body, err := checkRequest(r)
	if err != nil {
		g.log.Print("refused: ", err)
		http.Error(w, "gateway: "+err.Error(), http.StatusForbidden)
		return
	}
	token, err := g.currentToken()
	if err != nil {
		g.log.Print(err)
		http.Error(w, "gateway: could not get a model token", http.StatusBadGateway)
		return
	}
	// Forward the body as we parsed it, so Anthropic sees exactly what we checked
	// (no duplicate keys or other parser differences).
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	r.Header.Set("Authorization", "Bearer "+token)
	g.proxy.ServeHTTP(w, r)
}

// checkRequest returns the body to forward, or why the request is refused.
// Anything it can't parse or doesn't know is refused.
func checkRequest(r *http.Request) ([]byte, error) {
	if r.Method != "POST" || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") {
		return nil, fmt.Errorf("%s %s is not allowed", r.Method, r.URL.Path)
	}
	if q := r.URL.RawQuery; q != "" && q != "beta=true" {
		return nil, fmt.Errorf("query %q is not allowed", q)
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return nil, fmt.Errorf("body is not a JSON object: %v", err)
	}
	for k := range body {
		if !allowedFields[k] {
			return nil, fmt.Errorf("field %q is not allowed", k)
		}
	}
	if tools, ok := body["tools"]; ok {
		list, ok := tools.([]any)
		if !ok {
			return nil, errors.New("tools is not a list")
		}
		for _, t := range list {
			tool, ok := t.(map[string]any)
			// Server tools, such as web fetch, have their own types.
			if typ := tool["type"]; !ok || (typ != nil && typ != "" && typ != "custom") {
				return nil, fmt.Errorf("tool type %v is not allowed", typ)
			}
		}
	}
	if err := checkSources(body); err != nil {
		return nil, err
	}
	return json.Marshal(body)
}

// checkSources refuses any "source", at any depth, that isn't a base64 or text
// object. Other kinds, such as URL images and PDFs, make Anthropic fetch a URL.
func checkSources(v any) error {
	var children []any
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if src, _ := child.(map[string]any); k == "source" && src["type"] != "base64" && src["type"] != "text" {
				return fmt.Errorf("source type %v is not allowed", src["type"])
			}
			children = append(children, child)
		}
	case []any:
		children = v
	}
	for _, child := range children {
		if err := checkSources(child); err != nil {
			return err
		}
	}
	return nil
}

// currentToken returns the Anthropic token, exchanging a fresh GitHub OIDC
// token for a new one 60 seconds before the old one expires. Anthropic accepts
// each OIDC token only once.
func (g *gateway) currentToken() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	start := g.now()
	if g.token != "" && start.Before(g.refreshAt) {
		return g.token, nil
	}
	// Request shape from GitHub's toolkit:
	// https://github.com/actions/toolkit/blob/main/packages/core/src/oidc-utils.ts
	req, _ := http.NewRequest("GET", g.oidcURL+"&audience="+url.QueryEscape("https://api.anthropic.com"), nil)
	req.Header.Set("Authorization", "Bearer "+g.oidcToken)
	var id struct{ Value string }
	if err := callJSON("OIDC token", req, &id); err != nil {
		return "", err
	}
	// Exchange fields from Task 0's working run 37673094894 (sandbox repo, spike-q67-wif.yml).
	fields := map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": id.Value}
	maps.Copy(fields, g.ids)
	data, _ := json.Marshal(fields)
	req, _ = http.NewRequest("POST", g.upstream.JoinPath("/v1/oauth/token").String(), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := callJSON("token exchange", req, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" || tok.ExpiresIn <= 0 {
		return "", errors.New("token exchange: no token or expiry in the response")
	}
	g.token, g.refreshAt = tok.AccessToken, start.Add(time.Duration(tok.ExpiresIn)*time.Second-time.Minute)
	return g.token, nil
}

// callJSON decodes a 200 response into out. Its errors never carry a URL,
// header, or body, any of which can hold a token.
func callJSON(step string, req *http.Request, out any) error {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("%s: %v", step, errors.Unwrap(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", step, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: bad response", step)
	}
	return nil
}

// cmdGateway runs the gateway on 127.0.0.1:$BABYSIT_PORT until it fails.
func cmdGateway(args []string) int {
	env, ok := envInputs("gateway", args, []string{"BABYSIT_PORT", "ACTIONS_ID_TOKEN_REQUEST_URL",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN", "FEDERATION_RULE_ID", "ORGANIZATION_ID", "SERVICE_ACCOUNT_ID", "WORKSPACE_ID"})
	if !ok {
		return 2
	}
	port, err := strconv.Atoi(env["BABYSIT_PORT"])
	if err != nil || port <= 0 {
		fmt.Fprintln(os.Stderr, "gateway: BABYSIT_PORT must be a port number")
		return 2
	}
	upstream, _ := url.Parse("https://api.anthropic.com")
	g := newGateway(upstream, env["ACTIONS_ID_TOKEN_REQUEST_URL"], env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"], map[string]string{
		"federation_rule_id": env["FEDERATION_RULE_ID"], "organization_id": env["ORGANIZATION_ID"],
		"service_account_id": env["SERVICE_ACCOUNT_ID"], "workspace_id": env["WORKSPACE_ID"]}, os.Stderr)
	fmt.Fprintln(os.Stderr, "gateway:", http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", port), g))
	return 1
}
