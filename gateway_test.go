// ABOUTME: Tests for the model gateway: the request allowlist, auth header swap, request cap,
// ABOUTME: and the WIF token exchange against httptest stand-ins for GitHub and Anthropic.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakes stands in for GitHub's OIDC endpoint and Anthropic's token and
// messages endpoints, and records what the gateway sent them.
type fakes struct {
	mu          sync.Mutex
	jwts        int             // OIDC tokens issued
	used        map[string]bool // assertions already exchanged
	exchanges   []map[string]string
	expiresIn   int
	failOIDC    bool
	failToken   bool
	lastHeaders http.Header
	lastBody    []byte
	stream      chan struct{} // if set, messages streams two events and waits on it between them
	oidc, api   *httptest.Server
}

func newFakes(t *testing.T) *fakes {
	f := &fakes{used: map[string]bool{}, expiresIn: 300}
	f.oidc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failOIDC || r.Header.Get("Authorization") != "Bearer gh-request-token" ||
			r.URL.Query().Get("audience") != "https://api.anthropic.com" || r.URL.Query().Get("api-version") != "2.0" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		f.jwts++
		json.NewEncoder(w).Encode(map[string]string{"value": fmt.Sprintf("jwt-secret-%d", f.jwts)})
	}))
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		switch r.URL.Path {
		case "/v1/oauth/token":
			defer f.mu.Unlock()
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			f.exchanges = append(f.exchanges, req)
			if f.failToken || f.used[req["assertion"]] {
				http.Error(w, `{"error":{"type":"authentication_error","message":"Authentication failed"}}`, http.StatusUnauthorized)
				return
			}
			f.used[req["assertion"]] = true
			json.NewEncoder(w).Encode(map[string]any{"access_token": "sk-ant-oat01-" + req["assertion"],
				"token_type": "Bearer", "expires_in": f.expiresIn, "scope": "workspace:inference"})
		default:
			f.lastHeaders = r.Header.Clone()
			f.lastBody, _ = io.ReadAll(r.Body)
			stream := f.stream
			f.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: one\n\n")
			w.(http.Flusher).Flush()
			if stream != nil {
				<-stream
			}
			fmt.Fprint(w, "event: two\n\n")
		}
	}))
	t.Cleanup(f.oidc.Close)
	t.Cleanup(f.api.Close)
	return f
}

// testGateway returns a gateway wired to the fakes, a settable clock, and its log.
func testGateway(t *testing.T, f *fakes) (*gateway, *time.Time, *bytes.Buffer) {
	upstream, _ := url.Parse(f.api.URL)
	var logs bytes.Buffer
	g := newGateway(upstream, f.oidc.URL+"/token?api-version=2.0", "gh-request-token", map[string]string{
		"federation_rule_id": "fdrl_1", "organization_id": "org-1",
		"service_account_id": "svac_1", "workspace_id": "wrkspc_1"}, &logs)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	return g, &now, &logs
}

func post(t *testing.T, g *gateway, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

const okBody = `{"model":"claude-opus-5-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`

func TestFilterAllowsOnlyMessagesEndpoints(t *testing.T) {
	for _, c := range []struct {
		method, target string
		ok             bool
	}{
		{"POST", "/v1/messages", true},
		{"POST", "/v1/messages?beta=true", true},
		{"POST", "/v1/messages/count_tokens", true},
		{"POST", "/v1/messages/count_tokens?beta=true", true},
		{"GET", "/v1/messages", false},
		{"PUT", "/v1/messages", false},
		{"POST", "/v1/messages/batches", false},
		{"POST", "/v1/files", false},
		{"POST", "/v1/oauth/token", false},
		{"POST", "/v1/messages/", false},
		{"POST", "/api/hello", false},
		{"POST", "/v1/messages?beta=false", false},
		{"POST", "/v1/messages?beta=true&x=1", false},
		{"POST", "/v1/messages?url=https://evil.example/", false},
	} {
		r := httptest.NewRequest(c.method, c.target, strings.NewReader(okBody))
		_, err := checkRequest(r)
		if (err == nil) != c.ok {
			t.Errorf("%s %s: err = %v, want ok=%v", c.method, c.target, err, c.ok)
		}
	}
}

func TestGatewayRefusesOtherQueriesByName(t *testing.T) {
	g, _, _ := testGateway(t, newFakes(t))
	w := post(t, g, "/v1/messages?evil=1", okBody)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "evil=1") {
		t.Errorf("got %d %q, want 403 naming the query", w.Code, w.Body.String())
	}
}

func TestFilterRejectsUnknownTopLevelFields(t *testing.T) {
	for _, field := range []string{"mcp_servers", "container", "tool_choice", "context_management"} {
		body := `{"model":"m","max_tokens":1,"messages":[],"` + field + `":{}}`
		g, _, _ := testGateway(t, newFakes(t))
		w := post(t, g, "/v1/messages", body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), field) {
			t.Errorf("%s: got %d %q, want 403 naming the field", field, w.Code, w.Body.String())
		}
	}
}

func TestFilterRejectsUnparseableBodies(t *testing.T) {
	for _, body := range []string{"", "null", "not json", "[]", `{"model":"m"} {}`, `{"tools":{}}`, `{"tools":[1]}`} {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		if _, err := checkRequest(r); err == nil {
			t.Errorf("%q: accepted, want refused", body)
		}
	}
}

func TestFilterRejectsServerTools(t *testing.T) {
	for _, c := range []struct {
		tool string
		ok   bool
	}{
		{`{"name":"Bash","input_schema":{}}`, true},
		{`{"name":"Bash","type":null,"input_schema":{}}`, true},
		{`{"name":"Bash","type":"","input_schema":{}}`, true},
		{`{"name":"Bash","type":"custom","input_schema":{}}`, true},
		{`{"name":"web_fetch","type":"web_fetch_20250910"}`, false},
		{`{"name":"web_search","type":"web_search_20250305"}`, false},
		{`{"name":"code_execution","type":"code_execution_20250825"}`, false},
		{`{"name":"Bash","type":7}`, false},
	} {
		body := `{"model":"m","max_tokens":1,"messages":[],"tools":[` + c.tool + `]}`
		_, err := checkRequest(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.tool, err, c.ok)
		}
	}
}

func TestFilterRejectsSourcesOtherThanBase64OrText(t *testing.T) {
	msg := func(content string) string {
		return `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[` + content + `]}]}`
	}
	for _, c := range []struct {
		name, body string
		ok         bool
	}{
		{"base64 image", msg(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}`), true},
		{"text document", msg(`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"x"}}`), true},
		{"image by URL", msg(`{"type":"image","source":{"type":"url","url":"https://evil.example/?d=secret"}}`), false},
		{"PDF by URL", msg(`{"type":"document","source":{"type":"url","url":"https://evil.example/a.pdf"}}`), false},
		{"file by ID", msg(`{"type":"document","source":{"type":"file","file_id":"file_1"}}`), false},
		{"source not an object", msg(`{"type":"search_result","source":"https://evil.example/","title":"t","content":[]}`), false},
		{"URL image in tool_result", msg(`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"url","url":"https://evil.example/"}}]}`), false},
		{"URL image deep in system", `{"model":"m","max_tokens":1,"messages":[],"system":[{"type":"text","text":"x","extra":[{"source":{"type":"url"}}]}]}`, false},
	} {
		_, err := checkRequest(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(c.body)))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestGatewayForwardsOnlyAllowedHeaders(t *testing.T) {
	f := newFakes(t)
	g, _, _ := testGateway(t, f)
	r := httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(okBody))
	r.Header.Set("X-Api-Key", "placeholder")
	r.Header.Set("Authorization", "Bearer agent-made-this-up")
	r.Header.Set("X-Evil", "1")
	r.Header.Set("X-Stainless-Lang", "js")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := f.lastHeaders.Get("X-Api-Key"); got != "" {
		t.Errorf("x-api-key forwarded: %q", got)
	}
	for _, h := range []string{"X-Evil", "X-Stainless-Lang"} {
		if got := f.lastHeaders.Get(h); got != "" {
			t.Errorf("%s forwarded: %q", h, got)
		}
	}
	for _, h := range []string{"Anthropic-Version", "Content-Type"} {
		if f.lastHeaders.Get(h) != r.Header.Get(h) {
			t.Errorf("%s not forwarded", h)
		}
	}
	if got := f.lastHeaders.Get("Authorization"); got != "Bearer sk-ant-oat01-jwt-secret-1" {
		t.Errorf("authorization = %q, want the exchanged token", got)
	}
}

// TestGatewayPassesClaudeCodeRequest replays the body fields and betas that
// Claude Code 2.1.292 sent in Task 0 (testdata/task0/q67-runner-37673094894.txt).
func TestGatewayPassesClaudeCodeRequest(t *testing.T) {
	raw, err := os.ReadFile("testdata/task0/q67-runner-37673094894.txt")
	if err != nil {
		t.Fatal(err)
	}
	var seen struct {
		Beta     string   `json:"beta"`
		BodyKeys []string `json:"body_keys"`
		Tools    []struct {
			Name, Type string
		} `json:"tools"`
	}
	for line := range strings.Lines(string(raw)) {
		if rest, ok := strings.CutPrefix(line, "Q6_PROXY="); ok && strings.Contains(rest, `"event":"request"`) {
			if err := json.Unmarshal([]byte(rest), &seen); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if len(seen.BodyKeys) == 0 {
		t.Fatal("no recorded request in testdata")
	}
	var tools []map[string]any
	for _, tool := range seen.Tools {
		tools = append(tools, map[string]any{"name": tool.Name, "description": "d",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}})
	}
	sample := map[string]any{
		"max_tokens":    32000,
		"metadata":      map[string]any{"user_id": "u"},
		"model":         "claude-opus-5-5",
		"output_config": map[string]any{"effort": "high"},
		"stream":        true,
		"system":        []any{map[string]any{"type": "text", "text": "You are Claude Code.", "cache_control": map[string]any{"type": "ephemeral"}}},
		"thinking":      map[string]any{"type": "adaptive"},
		"tools":         tools,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Run ls -a"}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "t", "signature": "s"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls -a"}}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "."}}},
		},
	}
	body := map[string]any{}
	for _, k := range seen.BodyKeys {
		v, ok := sample[k]
		if !ok {
			t.Fatalf("testdata has field %q with no sample value", k)
		}
		body[k] = v
	}
	data, _ := json.Marshal(body)

	f := newFakes(t)
	g, _, _ := testGateway(t, f)
	r := httptest.NewRequest("POST", "/v1/messages?beta=true", bytes.NewReader(data))
	r.Header.Set("Anthropic-Beta", seen.Beta)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if f.lastHeaders.Get("Anthropic-Beta") != seen.Beta {
		t.Errorf("betas not forwarded")
	}
	var got, want any
	json.Unmarshal(f.lastBody, &got)
	json.Unmarshal(data, &want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("forwarded body differs:\n got %s\nwant %s", f.lastBody, data)
	}
}

func TestGatewayRefusesRequest401(t *testing.T) {
	g, _, _ := testGateway(t, newFakes(t))
	for i := 1; i <= 400; i++ {
		if w := post(t, g, "/v1/messages", okBody); w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
	}
	if w := post(t, g, "/v1/messages", okBody); w.Code != http.StatusTooManyRequests {
		t.Errorf("request 401: status %d, want 429", w.Code)
	}
}

func TestGatewayExchangesOnFirstRequest(t *testing.T) {
	f := newFakes(t)
	g, _, _ := testGateway(t, f)
	if len(f.exchanges) != 0 {
		t.Fatal("exchanged before any request")
	}
	post(t, g, "/v1/messages", okBody)
	want := map[string]string{
		"grant_type":         "urn:ietf:params:oauth:grant-type:jwt-bearer",
		"assertion":          "jwt-secret-1",
		"federation_rule_id": "fdrl_1",
		"organization_id":    "org-1",
		"service_account_id": "svac_1",
		"workspace_id":       "wrkspc_1",
	}
	if len(f.exchanges) != 1 || fmt.Sprint(f.exchanges[0]) != fmt.Sprint(want) {
		t.Errorf("exchanges = %v, want one of %v", f.exchanges, want)
	}
}

func TestGatewayRefreshesSixtySecondsEarly(t *testing.T) {
	f := newFakes(t)
	g, now, _ := testGateway(t, f)
	start := *now
	for _, c := range []struct {
		after     time.Duration
		exchanges int
	}{
		{0, 1},
		{239 * time.Second, 1},
		{240 * time.Second, 2},
		{479 * time.Second, 2},
		{480 * time.Second, 3},
	} {
		*now = start.Add(c.after)
		if w := post(t, g, "/v1/messages", okBody); w.Code != http.StatusOK {
			t.Fatalf("at %v: status %d", c.after, w.Code)
		}
		if len(f.exchanges) != c.exchanges {
			t.Errorf("at %v: %d exchanges, want %d", c.after, len(f.exchanges), c.exchanges)
		}
	}
	// The fake refuses a reused assertion, so three successes mean three fresh OIDC tokens.
	if f.jwts != 3 {
		t.Errorf("fetched %d OIDC tokens, want 3", f.jwts)
	}
}

func TestGatewayStreamsEventByEvent(t *testing.T) {
	f := newFakes(t)
	f.stream = make(chan struct{})
	g, _, _ := testGateway(t, f)
	srv := httptest.NewServer(g)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(okBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	// The upstream holds back event two until we have read event one.
	if line, err := rd.ReadString('\n'); err != nil || line != "event: one\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
	close(f.stream)
	rest, _ := io.ReadAll(rd)
	if string(rest) != "\nevent: two\n\n" {
		t.Errorf("rest = %q", rest)
	}
}

func TestGatewayFailedExchangeIs502WithoutTokens(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*fakes)
	}{
		{"oidc fails", func(f *fakes) { f.failOIDC = true }},
		{"exchange fails", func(f *fakes) { f.failToken = true }},
	} {
		f := newFakes(t)
		c.set(f)
		g, _, logs := testGateway(t, f)
		w := post(t, g, "/v1/messages", okBody)
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s: status %d, want 502", c.name, w.Code)
		}
		out := logs.String() + w.Body.String()
		if logs.Len() == 0 {
			t.Errorf("%s: nothing logged", c.name)
		}
		for _, secret := range []string{"gh-request-token", "jwt-secret", "sk-ant-"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s: log or body contains %q: %s", c.name, secret, out)
			}
		}
	}
}

func TestCmdGatewayNeedsItsEnvironment(t *testing.T) {
	for _, k := range []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN",
		"FEDERATION_RULE_ID", "ORGANIZATION_ID", "SERVICE_ACCOUNT_ID", "WORKSPACE_ID"} {
		t.Setenv(k, "x")
	}
	// An empty or named host would listen on every interface, or wherever the name resolves.
	for _, addr := range []string{"", "8199", ":8199", "localhost:8199", "10.199.0.1", "10.199.0.1:0", "10.199.0.1:x"} {
		t.Setenv("BABYSIT_GATEWAY", addr)
		if code := cmdGateway(nil); code != 2 {
			t.Errorf("BABYSIT_GATEWAY=%q: exit %d, want 2", addr, code)
		}
	}
	t.Setenv("BABYSIT_GATEWAY", "10.199.0.1:1")
	t.Setenv("WORKSPACE_ID", "")
	if msg := stderrOf(t, func() { cmdGateway(nil) }); !strings.Contains(msg, "missing WORKSPACE_ID") {
		t.Errorf("printed %q, want missing WORKSPACE_ID", msg)
	}
}
