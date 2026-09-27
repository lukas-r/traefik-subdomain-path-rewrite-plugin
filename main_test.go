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
