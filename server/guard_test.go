package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardServer is the studio with nothing real behind it: a stub engine, a
// model directory that only looks like one, and everything it keeps under
// t.TempDir(). No request in this file is supposed to reach an engine.
func guardServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg, err := NewConfig(Args{
		Iris:  stubIris(t, fixturePNG(t)),
		Model: fakeModelDir(t),
		Root:  t.TempDir(),
		Host:  "iris.local",
		// As main splits it: --allow-host is comma-separated, and an empty
		// flag yields one empty entry that has to be dropped.
		AllowedHosts: strings.Split("studio.example, ", ","),
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(cfg)
	t.Cleanup(runner.StopInteractive)
	srv := httptest.NewServer(NewApp(cfg, runner))
	t.Cleanup(srv.Close)
	return srv
}

type req struct {
	method string
	path   string
	host   string // overrides the Host header; empty keeps the server's own
	origin string
	fetch  string // Sec-Fetch-Site
	ctype  string
	body   string
	extra  map[string]string
}

func (r req) do(t *testing.T, srv *httptest.Server) (int, string) {
	t.Helper()
	method := r.method
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	request, err := http.NewRequest(method, srv.URL+r.path, body)
	if err != nil {
		t.Fatal(err)
	}
	if r.host != "" {
		request.Host = r.host
	}
	if r.origin != "" {
		request.Header.Set("Origin", r.origin)
	}
	if r.fetch != "" {
		request.Header.Set("Sec-Fetch-Site", r.fetch)
	}
	if r.ctype != "" {
		request.Header.Set("Content-Type", r.ctype)
	}
	for k, v := range r.extra {
		request.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(out)
}

// A Host header this server does not answer to is how DNS rebinding arrives:
// a name the attacker controls, re-pointed at 127.0.0.1 once the page is open.
func TestTheGuardRefusesAHostItDoesNotAnswerTo(t *testing.T) {
	srv := guardServer(t)

	for _, host := range []string{"evil.test", "evil.test:8720", "iris.local.evil.test"} {
		if status, body := (req{method: http.MethodGet, path: "/api/config", host: host}).do(t, srv); status != http.StatusForbidden {
			t.Errorf("Host %q: got %d %s, want 403", host, status, body)
		}
	}

	// A GET is refused too: the Host check is not about state changes, it is
	// about which server the browser thinks it is talking to.
	for _, host := range []string{"", "localhost:8720", "localhost", "127.0.0.1:8720", "[::1]:8720", "iris.local:8720", "studio.example:8720"} {
		r := req{method: http.MethodGet, path: "/api/config", host: host}
		if status, body := r.do(t, srv); status != http.StatusOK {
			t.Errorf("Host %q: got %d %s, want 200", host, status, body)
		}
	}
}

// The refusal names the flag, because the only way past it is to start the
// studio with that host allowed.
func TestTheHostRefusalNamesTheFlag(t *testing.T) {
	srv := guardServer(t)
	_, body := (req{method: http.MethodGet, path: "/api/config", host: "box.lan:8720"}).do(t, srv)
	if !strings.Contains(body, "--allow-host box.lan") {
		t.Errorf("refusal does not say how to allow the host: %s", body)
	}
}

// The attack the README described: a page you visit posts to the studio with
// a CORS-simple content type, so there is no preflight for the browser to
// refuse. The attacker never reads the response and does not need to.
func TestTheGuardRefusesASimpleCrossSitePost(t *testing.T) {
	srv := guardServer(t)

	cases := []struct {
		name string
		r    req
		want int
	}{
		{"a text/plain body dressed as JSON", req{path: "/api/cancel", ctype: "text/plain;charset=UTF-8", body: `{"id":"x"}`, origin: "https://evil.test"}, http.StatusForbidden},
		{"a form post", req{path: "/api/cancel", ctype: "application/x-www-form-urlencoded", body: "id=x", origin: "https://evil.test"}, http.StatusForbidden},
		{"a multipart form", req{path: "/api/render", ctype: "multipart/form-data; boundary=x", body: "--x--", origin: "https://evil.test"}, http.StatusForbidden},
		// No Origin at all, which is what a form navigation can look like:
		// Sec-Fetch-Site is then what says where it came from.
		{"no Origin, cross-site", req{path: "/api/cancel", ctype: "text/plain", body: `{"id":"x"}`, fetch: "cross-site"}, http.StatusForbidden},
		// An opaque origin — a sandboxed iframe or a data: URL.
		{"a null Origin", req{path: "/api/cancel", ctype: "application/json", body: `{"id":"x"}`, origin: "null"}, http.StatusForbidden},
		// Same site, so the Origin check lets it by, but a simple content
		// type still cannot reach a JSON endpoint.
		{"a same-site simple post", req{path: "/api/cancel", ctype: "text/plain", body: `{"id":"x"}`, fetch: "same-site"}, http.StatusUnsupportedMediaType},
		{"no content type at all", req{path: "/api/cancel", body: `{"id":"x"}`, fetch: "same-origin"}, http.StatusUnsupportedMediaType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if status, body := c.r.do(t, srv); status != c.want {
				t.Errorf("got %d %s, want %d", status, body, c.want)
			}
		})
	}
}

// What the page itself sends still goes through, or the guard would have
// fixed nothing and broken the studio.
func TestTheGuardLetsThePageThrough(t *testing.T) {
	srv := guardServer(t)

	cases := []struct {
		name string
		r    req
	}{
		{"same-origin JSON", req{path: "/api/cancel", ctype: "application/json", body: `{"id":"none"}`, origin: "http://" + strings.TrimPrefix(srv.URL, "http://")}},
		{"JSON with a charset", req{path: "/api/cancel", ctype: "application/json; charset=utf-8", body: `{"id":"none"}`, fetch: "same-origin"}},
		{"no Origin and no Sec-Fetch-Site", req{path: "/api/cancel", ctype: "application/json", body: `{"id":"none"}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := c.r.do(t, srv)
			if status != http.StatusOK {
				t.Fatalf("got %d %s, want 200", status, body)
			}
			if !strings.Contains(body, `"ok"`) {
				t.Errorf("did not reach the handler: %s", body)
			}
		})
	}
}

// An upload is a raw body with X-Filename naming it, so it cannot carry a
// JSON content type. The custom header is what forces the preflight instead.
func TestTheGuardTakesAnUploadOnItsHeader(t *testing.T) {
	srv := guardServer(t)

	if status, body := (req{path: "/api/upload", ctype: "text/plain", body: "png bytes"}).do(t, srv); status != http.StatusBadRequest {
		t.Errorf("upload without X-Filename: got %d %s, want 400", status, body)
	}
	r := req{path: "/api/upload", ctype: "text/plain", body: "png bytes", extra: map[string]string{"X-Filename": "ref.png"}}
	if status, body := r.do(t, srv); status != http.StatusOK {
		t.Errorf("upload with X-Filename: got %d %s, want 200", status, body)
	}
	// Still cross-site, though: the header exemption is downstream of the
	// Origin check, not instead of it.
	r.origin = "https://evil.test"
	if status, body := r.do(t, srv); status != http.StatusForbidden {
		t.Errorf("cross-origin upload: got %d %s, want 403", status, body)
	}
}

// helmstudio's SDK writes through the /helm/ proxy in the content types the
// platform API declares, which are not all application/json. Each of them
// still forces a preflight, so the guard takes them — and takes nothing else.
func TestTheGuardKeepsTheHelmProxyUsable(t *testing.T) {
	srv := guardServer(t)

	// With no HELM_API behind it the proxy answers for itself; what matters
	// here is only that the guard did not answer first.
	refused := func(status int) bool {
		return status == http.StatusForbidden || status == http.StatusUnsupportedMediaType || status == http.StatusBadRequest
	}

	allowed := []struct {
		name string
		r    req
	}{
		{"a merge patch", req{method: http.MethodPatch, path: "/helm/api/v1/jobs/j1", ctype: "application/merge-patch+json", body: `{"progress":0.5}`, fetch: "same-origin"}},
		{"a write with no body", req{method: http.MethodPost, path: "/helm/api/v1/jobs/j1:cancel", fetch: "same-origin"}},
		{"a DELETE with no body", req{method: http.MethodDelete, path: "/helm/api/v1/assets/a1", fetch: "same-origin"}},
		{"plain JSON", req{path: "/helm/api/v1/assets", ctype: "application/json", body: `{"path":"x.png"}`, fetch: "same-origin"}},
	}
	for _, c := range allowed {
		t.Run(c.name, func(t *testing.T) {
			if status, body := c.r.do(t, srv); refused(status) {
				t.Errorf("the guard refused an SDK write: %d %s", status, body)
			}
		})
	}

	// The exemption is for content types a cross-site request cannot forge,
	// not for the proxy as such.
	blocked := []struct {
		name string
		r    req
		want int
	}{
		{"a simple body on the proxy", req{path: "/helm/api/v1/assets", ctype: "text/plain", body: `{"path":"x.png"}`, fetch: "same-origin"}, http.StatusUnsupportedMediaType},
		{"an empty content type with a body", req{path: "/helm/api/v1/assets", body: `{"path":"x.png"}`, fetch: "same-origin"}, http.StatusUnsupportedMediaType},
		{"a cross-site merge patch", req{method: http.MethodPatch, path: "/helm/api/v1/jobs/j1", ctype: "application/merge-patch+json", body: `{}`, origin: "https://evil.test"}, http.StatusForbidden},
		{"an unrecognized Host", req{method: http.MethodGet, path: "/helm/api/v1/theme", host: "evil.test"}, http.StatusForbidden},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if status, body := c.r.do(t, srv); status != c.want {
				t.Errorf("got %d %s, want %d", status, body, c.want)
			}
		})
	}
}

// The shell is gone, not gated: there is no flag that brings it back, and a
// page kept open across the change is told so rather than being let through.
func TestTheTerminalNoLongerRunsAShell(t *testing.T) {
	srv := guardServer(t)
	marker := filepath.Join(t.TempDir(), "the-shell-ran")

	status, body := (req{
		path:  "/api/terminal",
		ctype: "application/json",
		body:  `{"command":"touch ` + marker + `"}`,
		fetch: "same-origin",
	}).do(t, srv)

	if status != http.StatusGone {
		t.Errorf("got %d %s, want 410", status, body)
	}
	// Whatever it answered, nothing may have run. Give a shell every chance
	// to have finished first: RunTerminal used to start one and return.
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a shell command ran: %s exists", marker)
	}
}

// A line typed into the terminal panel now reaches interactive iris, and says
// so when none is loaded. This is the whole of what that input does.
func TestTheTerminalInputGoesToInteractiveIris(t *testing.T) {
	srv := guardServer(t)
	status, body := (req{
		path:  "/api/interactive/input",
		ctype: "application/json",
		body:  `{"line":"!help"}`,
		fetch: "same-origin",
	}).do(t, srv)

	if status != http.StatusConflict {
		t.Fatalf("got %d %s, want 409", status, body)
	}
	if !strings.Contains(body, "load interactive iris first") {
		t.Errorf("unhelpful refusal: %s", body)
	}
}
