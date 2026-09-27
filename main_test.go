package traefik_subdomain_path_rewrite_plugin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const previewHost = "2636.frontend-preview.example.com"

type seenRequest struct {
	host string
	path string
}

// fakeWebsite serves a fixed set of objects like an S3 website endpoint and
// answers every other path with missStatus.
type fakeWebsite struct {
	mu         sync.Mutex
	objects    map[string]string
	missStatus int
	seen       []seenRequest
}

func (f *fakeWebsite) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, seenRequest{host: req.Host, path: req.URL.Path})
	f.mu.Unlock()
	body, ok := f.objects[req.URL.Path]
	if !ok {
		rw.Header().Set("Content-Type", "application/xml")
		rw.Header().Set("X-Backend-Miss", "1")
		rw.WriteHeader(f.missStatus)
		_, _ = io.WriteString(rw, "<Error>missing</Error>")
		return
	}
	rw.Header().Set("Content-Type", contentType(req.URL.Path))
	_, _ = io.WriteString(rw, body)
}

func contentType(p string) string {
	if strings.HasSuffix(p, ".js") {
		return "application/javascript"
	}
	return "text/html"
}

func newPlugin(t *testing.T, next *fakeWebsite, configure func(*Config)) http.Handler {
	return newPluginFor(t, http.HandlerFunc(next.ServeHTTP), configure)
}

// newPluginFor takes a plain handler; Yaegi cannot pass an interpreted struct as http.Handler.
func newPluginFor(t *testing.T, next http.Handler, configure func(*Config)) http.Handler {
	t.Helper()
	config := CreateConfig()
	config.ReplacementHost = "frontend-testing.web.internal"
	config.FallbackPath = "/index.html"
	if configure != nil {
		configure(config)
	}
	handler, err := New(context.Background(), next, config, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler
}

func serve(handler http.Handler, method, target, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+previewHost+target, nil)
	req.Host = previewHost
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func website(missStatus int) *fakeWebsite {
	return &fakeWebsite{
		missStatus: missStatus,
		objects: map[string]string{
			"/2636/index.html":    "<html>preview 2636</html>",
			"/2636/assets/app.js": "console.log('app')",
			"/2636-ui/index.html": "<html>storybook 2636</html>",
		},
	}
}

func TestRewritesSubdomainIntoPathAndHost(t *testing.T) {
	backend := website(http.StatusNotFound)
	rec := serve(newPlugin(t, backend, nil), http.MethodGet, "/assets/app.js", "")

	if rec.Code != http.StatusOK || rec.Body.String() != "console.log('app')" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if got := backend.seen[0]; got.host != "frontend-testing.web.internal" || got.path != "/2636/assets/app.js" {
		t.Fatalf("backend saw %+v", got)
	}
}

func TestBasePathPrefixesTheRewrittenPath(t *testing.T) {
	backend := website(http.StatusNotFound)
	backend.objects["/frontend-testing/2636/assets/app.js"] = "prefixed"
	handler := newPlugin(t, backend, func(c *Config) { c.BasePath = "frontend-testing" })

	rec := serve(handler, http.MethodGet, "/assets/app.js", "")

	if rec.Body.String() != "prefixed" || backend.seen[0].path != "/frontend-testing/2636/assets/app.js" {
		t.Fatalf("got %q, backend saw %+v", rec.Body.String(), backend.seen[0])
	}
}

func TestDeepLinkNavigationServesTheFallbackWith200(t *testing.T) {
	backend := website(http.StatusNotFound)
	rec := serve(newPlugin(t, backend, nil), http.MethodGet, "/employees/42/profile", "")

	if rec.Code != http.StatusOK || rec.Body.String() != "<html>preview 2636</html>" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Backend-Miss") != "" || rec.Header().Get("Content-Type") != "text/html" {
		t.Fatalf("headers of the missed response leaked into the fallback: %v", rec.Header())
	}
	if len(backend.seen) != 2 || backend.seen[1].path != "/2636/index.html" {
		t.Fatalf("backend saw %+v", backend.seen)
	}
}

func TestNavigationWithHtmlAcceptFallsBackEvenWithADotInThePath(t *testing.T) {
	rec := serve(newPlugin(t, website(http.StatusNotFound), nil), http.MethodGet, "/reports/2026.09", "text/html,application/xhtml+xml")

	if rec.Code != http.StatusOK || rec.Body.String() != "<html>preview 2636</html>" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestMissingAssetKeepsTheBackendStatus(t *testing.T) {
	backend := website(http.StatusNotFound)
	rec := serve(newPlugin(t, backend, nil), http.MethodGet, "/assets/definitely-missing.js", "*/*")

	if rec.Code != http.StatusNotFound || rec.Body.String() != "<Error>missing</Error>" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if len(backend.seen) != 1 {
		t.Fatalf("a missing asset must not trigger a fallback request, backend saw %+v", backend.seen)
	}
}

func TestConfiguredFallbackStatusCodesAreHonoured(t *testing.T) {
	backend := website(http.StatusForbidden)
	rec := serve(newPlugin(t, backend, func(c *Config) { c.FallbackStatusCodes = []int{403, 404} }), http.MethodGet, "/employees", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "<html>preview 2636</html>" {
		t.Fatalf("403 with fallbackStatusCodes [403 404]: got %d %q", rec.Code, rec.Body.String())
	}

	rec = serve(newPlugin(t, website(http.StatusForbidden), nil), http.MethodGet, "/employees", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("403 with the default codes must pass through, got %d", rec.Code)
	}
}

func TestMissingFallbackDocumentDoesNotLoop(t *testing.T) {
	backend := website(http.StatusNotFound)
	delete(backend.objects, "/2636/index.html")
	rec := serve(newPlugin(t, backend, nil), http.MethodGet, "/employees", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
	if len(backend.seen) != 2 {
		t.Fatalf("expected the original request and exactly one fallback, backend saw %+v", backend.seen)
	}
}

func TestNonGetRequestsNeverFallBack(t *testing.T) {
	backend := website(http.StatusNotFound)
	rec := serve(newPlugin(t, backend, nil), http.MethodPost, "/employees", "")

	if rec.Code != http.StatusNotFound || len(backend.seen) != 1 {
		t.Fatalf("got %d, backend saw %+v", rec.Code, backend.seen)
	}
}

func TestStorybookSubdomainMapsToItsOwnPrefix(t *testing.T) {
	backend := website(http.StatusNotFound)
	req := httptest.NewRequest(http.MethodGet, "http://2636-ui.frontend-preview.example.com/", nil)
	req.Host = "2636-ui.frontend-preview.example.com"
	rec := httptest.NewRecorder()
	newPlugin(t, backend, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "<html>storybook 2636</html>" {
		t.Fatalf("got %d %q, backend saw %+v", rec.Code, rec.Body.String(), backend.seen)
	}
}

func TestResponsesStreamBeforeTheBackendFinishes(t *testing.T) {
	// 64 KiB exceeds the server's write buffer, so the first chunk reaches the
	// client only if nothing holds the whole response back. Flushing is
	// optional: under Yaegi the wrapped writer does not expose http.Flusher.
	firstChunk := strings.Repeat("a", 64*1024)
	release := make(chan struct{})
	backend := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(rw, firstChunk)
		if flusher, ok := rw.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
		_, _ = io.WriteString(rw, "tail")
	})
	server := httptest.NewServer(newPluginFor(t, backend, nil))
	defer server.Close()
	defer close(release)

	// A navigation path, so the response goes through the intercepting writer.
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/live/feed", nil)
	req.Host = previewHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	received := make(chan int, 1)
	go func() {
		buf := make([]byte, 32*1024)
		n, _ := io.ReadAtLeast(resp.Body, buf, len(buf))
		received <- n
	}()
	select {
	case n := <-received:
		if n < 32*1024 {
			t.Fatalf("read only %d bytes", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no data arrived while the backend was still writing: the response is buffered")
	}
}

func TestClientCannotSkipTheRewriteWithThePluginsOwnHeaders(t *testing.T) {
	backend := website(http.StatusNotFound)
	backend.objects["/internal/secret.txt"] = "outside the MR prefix"
	req := httptest.NewRequest(http.MethodGet, "http://"+previewHost+"/internal/secret.txt", nil)
	req.Host = previewHost
	req.Header.Set(ReplacedPathHeader, "x")
	req.Header.Set(ReplacedHostHeader, "x")
	req.Header.Set(FallbackURLHeader, "x")
	rec := httptest.NewRecorder()
	newPlugin(t, backend, nil).ServeHTTP(rec, req)

	if rec.Body.String() == "outside the MR prefix" {
		t.Fatal("a spoofed header skipped the rewrite")
	}
	if got := backend.seen[0]; got.path != "/2636/internal/secret.txt" || got.host != "frontend-testing.web.internal" {
		t.Fatalf("backend saw %+v", got)
	}
}

func TestEarlyHintsDoNotReplaceTheFinalStatus(t *testing.T) {
	site := website(http.StatusNotFound)
	backend := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/2636/moved" {
			rw.Header().Set("Link", "</app.css>; rel=preload")
			rw.WriteHeader(http.StatusEarlyHints)
			rw.Header().Set("Location", "/elsewhere")
			rw.WriteHeader(http.StatusMovedPermanently)
			return
		}
		if req.URL.Path == "/2636/employees/42" {
			rw.WriteHeader(http.StatusEarlyHints)
		}
		site.ServeHTTP(rw, req)
	})
	server := httptest.NewServer(newPluginFor(t, backend, nil))
	defer server.Close()
	// A bare transport does not follow redirects (and avoids a Yaegi limitation with CheckRedirect).
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()

	for _, tc := range []struct {
		path, wantBody, wantLocation string
		wantStatus                   int
	}{
		{"/employees/42", "<html>preview 2636</html>", "", http.StatusOK},
		{"/moved", "", "/elsewhere", http.StatusMovedPermanently},
	} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+tc.path, nil)
		req.Host = previewHost
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.wantStatus || string(body) != tc.wantBody || resp.Header.Get("Location") != tc.wantLocation {
			t.Fatalf("%s: got %d %q location=%q", tc.path, resp.StatusCode, body, resp.Header.Get("Location"))
		}
	}
}

func TestTrailersPassThrough(t *testing.T) {
	backend := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Trailer", "X-Checksum")
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, "body")
		rw.Header().Set("X-Checksum", "abc")
	})
	server := httptest.NewServer(newPluginFor(t, backend, nil))
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/feed", nil)
	req.Host = previewHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := resp.Trailer.Get("X-Checksum"); got != "abc" {
		t.Fatalf("trailer X-Checksum = %q", got)
	}
}

func TestLaterMiddlewareChangesDoNotLeakIntoTheFallback(t *testing.T) {
	site := website(http.StatusNotFound)
	site.objects["/p/2636/catalog/index.html"] = "<html>catalog</html>"
	var prefixes []string
	addPrefix := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		req.URL.Path = "/p" + req.URL.Path
		req.Header.Add("X-Forwarded-Prefix", "/p")
		prefixes = append(prefixes, strings.Join(req.Header.Values("X-Forwarded-Prefix"), ","))
		site.ServeHTTP(rw, req)
	})
	req := httptest.NewRequest(http.MethodGet, "http://"+previewHost+"/catalog/missing", nil)
	req.Host = previewHost
	rec := httptest.NewRecorder()
	newPluginFor(t, addPrefix, func(c *Config) { c.FallbackPath = "index.html" }).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "<html>catalog</html>" {
		t.Fatalf("got %d %q, backend saw %+v", rec.Code, rec.Body.String(), site.seen)
	}
	if len(prefixes) != 2 || prefixes[1] != "/p" {
		t.Fatalf("X-Forwarded-Prefix per pass: %v", prefixes)
	}
}

func TestOnlyNavigationsFallBack(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		headers      map[string]string
		wantFallback bool
	}{
		{"browser navigation", "/employees/42", map[string]string{"Sec-Fetch-Mode": "navigate", "Accept": "text/html"}, true},
		{"browser navigation to a dotted route", "/reports/2026.09", map[string]string{"Sec-Fetch-Mode": "navigate"}, true},
		{"browser fetch of a missing route", "/employees/42", map[string]string{"Sec-Fetch-Mode": "cors", "Accept": "*/*"}, false},
		{"JSON API call without fetch metadata", "/api/users", map[string]string{"Accept": "application/json"}, false},
		{"html explicitly refused", "/employees", map[string]string{"Accept": "text/html;q=0, application/json"}, false},
		{"plain client without Accept", "/employees/42", nil, true},
		{"plain client accepting anything", "/employees/42", map[string]string{"Accept": "*/*"}, true},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+previewHost+tc.target, nil)
		req.Host = previewHost
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		newPlugin(t, website(http.StatusNotFound), nil).ServeHTTP(rec, req)
		gotFallback := rec.Code == http.StatusOK && rec.Body.String() == "<html>preview 2636</html>"
		if gotFallback != tc.wantFallback {
			t.Fatalf("%s: fallback=%v, got %d %q", tc.name, gotFallback, rec.Code, rec.Body.String())
		}
	}
}

func TestEarlyHintHeadersDoNotLeakIntoTheFinalResponse(t *testing.T) {
	backend := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Link", "</a.js>; rel=preload")
		rw.WriteHeader(http.StatusEarlyHints)
		// httputil.ReverseProxy, as used by Traefik, clears the header map it
		// got from Header() after forwarding each 1xx.
		for key := range rw.Header() {
			delete(rw.Header(), key)
		}
		rw.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(rw, "final")
	})
	server := httptest.NewServer(newPluginFor(t, backend, nil))
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/page", nil)
	req.Host = previewHost
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Link") != "" {
		t.Fatalf("got %d, Link=%q on the final response", resp.StatusCode, resp.Header.Get("Link"))
	}
}

func TestUpgradeRequestsAndAcceptEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name         string
		headers      map[string]string
		wantFallback bool
	}{
		{"websocket upgrade", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}, false},
		{"uppercase media type", map[string]string{"Accept": "TEXT/HTML"}, true},
		{"uppercase zero quality", map[string]string{"Accept": "text/html;Q=0, application/json"}, false},
		{"trailing-dot zero quality", map[string]string{"Accept": "text/html;q=0., application/json"}, false},
		{"small positive quality", map[string]string{"Accept": "text/html;q=0.001"}, true},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+previewHost+"/employees", nil)
		req.Host = previewHost
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		newPlugin(t, website(http.StatusNotFound), nil).ServeHTTP(rec, req)
		gotFallback := rec.Code == http.StatusOK && rec.Body.String() == "<html>preview 2636</html>"
		if gotFallback != tc.wantFallback {
			t.Fatalf("%s: fallback=%v, got %d", tc.name, gotFallback, rec.Code)
		}
	}
}
