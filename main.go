package traefik_subdomain_path_rewrite_plugin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	logger "github.com/lukas-r/traefik-subdomain-path-rewrite-plugin/pkg/logger"
)

const (
	typeName           = "SubdomainPathRewrite"
	ReplacedPathHeader = "X-Replaced-Path"
	ReplacedHostHeader = "X-Replaced-Host"
	FallbackURLHeader  = "X-Fallback-For"
)

// Config the plugin configuration.
type Config struct {
	RewriteSubdomain    bool   `json:"rewriteSubdomain,omitempty"`
	ReplacementHost     string `json:"replacementHost,omitempty"`
	BasePath            string `json:"basePath,omitempty"`
	KeepPath            bool   `json:"keepPath,omitempty"`
	FallbackPath        string `json:"fallbackPath,omitempty"`
	FallbackStatusCodes []int  `json:"fallbackStatusCodes,omitempty"`
	LogLevel            string `json:"logLevel,omitempty"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		RewriteSubdomain:    true,
		KeepPath:            true,
		FallbackStatusCodes: []int{http.StatusNotFound},
		LogLevel:            "INFO",
	}
}

// DynamicRewrite rewrites a subdomain into a path prefix and serves a fallback
// document for page navigations the backend cannot answer.
type DynamicRewrite struct {
	next                  http.Handler
	name                  string
	rewriteSubdomain      bool
	replacementHost       string
	basePath              string
	keepPath              bool
	fallbackPathComponent string
	fallbackStatusCodes   map[int]bool
	hostRegex             *regexp.Regexp
	log                   *logger.Log
}

// New creates a new subdomain rewrite middleware.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	log := logger.New(config.LogLevel, fmt.Sprintf("[%s] ", typeName))

	hostRegex, err := regexp.Compile(`^(?P<identifier>[^\.]+)\..+$`)
	if err != nil {
		return nil, err
	}

	if config.BasePath != "" && config.BasePath[0] != '/' {
		config.BasePath = "/" + config.BasePath
	}

	statusCodes := make(map[int]bool, len(config.FallbackStatusCodes))
	for _, code := range config.FallbackStatusCodes {
		statusCodes[code] = true
	}
	if len(statusCodes) == 0 {
		statusCodes[http.StatusNotFound] = true
	}

	return &DynamicRewrite{
		next:                  next,
		name:                  name,
		rewriteSubdomain:      config.RewriteSubdomain,
		replacementHost:       config.ReplacementHost,
		basePath:              config.BasePath,
		keepPath:              config.KeepPath,
		fallbackPathComponent: config.FallbackPath,
		fallbackStatusCodes:   statusCodes,
		hostRegex:             hostRegex,
		log:                   log,
	}, nil
}

func (dr *DynamicRewrite) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// These headers are the plugin's own; a client sending them must not be
	// able to skip the rewrite and reach paths outside its prefix.
	req.Header.Del(ReplacedPathHeader)
	req.Header.Del(ReplacedHostHeader)
	req.Header.Del(FallbackURLHeader)

	originalPath := req.URL.Path
	dr.log.Debug("Original request: host=%s path=%s", req.Host, originalPath)
	internalBasePath := dr.rewriteRequest(req.Host, req)

	if !dr.fallbackEligible(req, originalPath) {
		dr.next.ServeHTTP(rw, req)
		return
	}

	// Built before the first pass, so later middlewares that change the request
	// in place cannot leak into the fallback request.
	fallbackReq := dr.buildFallbackRequest(req, internalBasePath)

	// Normal responses stream straight through; only a fallback status is held
	// back so the fallback document can be served in its place.
	intercept := &interceptingWriter{rw: rw, header: http.Header{}, shouldIntercept: dr.isFallbackStatus}
	dr.next.ServeHTTP(intercept, req)
	if !intercept.intercepted {
		return
	}

	dr.log.Debug("Serving fallback %s for %s (status %d)", fallbackReq.URL.Path, req.URL.Path, intercept.status)
	dr.next.ServeHTTP(rw, fallbackReq)
}

func (dr *DynamicRewrite) rewriteRequest(host string, req *http.Request) string {
	dynamicIdentifier, baseHost := dr.extractDynamicIdentifierAndHost(host)
	dr.rewriteHost(req, baseHost)
	return dr.rewritePath(req, dynamicIdentifier)
}

func (dr *DynamicRewrite) extractDynamicIdentifierAndHost(host string) (string, string) {
	if !dr.rewriteSubdomain {
		return "", host
	}
	matches := dr.hostRegex.FindStringSubmatch(host)
	if len(matches) > 1 {
		dynamicIdentifier := matches[1]
		return dynamicIdentifier, host[len(dynamicIdentifier)+1:]
	}
	dr.log.Debug("No dynamic identifier found in host: %s", host)
	return "", host
}

func (dr *DynamicRewrite) rewriteHost(req *http.Request, baseHost string) {
	originalHost := req.Host
	newHost := baseHost
	if dr.replacementHost != "" {
		newHost = dr.replacementHost
	}
	req.Host = newHost
	req.Header.Add(ReplacedHostHeader, originalHost)
	dr.log.Debug("Rewritten host from %s to %s", originalHost, newHost)
}

func (dr *DynamicRewrite) rewritePath(req *http.Request, dynamicIdentifier string) string {
	originalPath := req.URL.Path
	req.Header.Add(ReplacedPathHeader, originalPath)

	newPath, basePath := dr.buildNewPath(req, dynamicIdentifier)
	setPath(req, newPath)
	dr.log.Debug("Rewritten path from %s to %s", originalPath, req.URL.Path)
	return basePath
}

func (dr *DynamicRewrite) buildNewPath(req *http.Request, dynamicIdentifier string) (string, string) {
	if dynamicIdentifier != "" {
		dynamicIdentifier = "/" + dynamicIdentifier
	}
	basePath := dr.basePath + dynamicIdentifier
	newPath := basePath
	if dr.keepPath {
		newPath += req.URL.EscapedPath()
	} else {
		newPath += "/"
	}
	return newPath, basePath
}

// fallbackEligible limits the fallback to page navigations, so a missing
// asset or API call keeps the backend's status instead of getting the HTML
// page. Browsers say so directly with Sec-Fetch-Mode; without it, a request
// counts as a navigation when it asks for HTML, or when it names no file and
// accepts anything.
func (dr *DynamicRewrite) fallbackEligible(req *http.Request, originalPath string) bool {
	if dr.fallbackPathComponent == "" {
		return false
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if mode := req.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	accept := req.Header.Get("Accept")
	if acceptsMediaType(accept, "text/html") || acceptsMediaType(accept, "application/xhtml+xml") {
		return true
	}
	namesFile := strings.Contains(path.Base(originalPath), ".")
	return !namesFile && (accept == "" || acceptsMediaType(accept, "*/*"))
}

// acceptsMediaType reports whether an Accept header lists the media type with
// a non-zero quality.
func acceptsMediaType(accept string, mediaType string) bool {
	for _, entry := range strings.Split(accept, ",") {
		fields := strings.Split(entry, ";")
		if strings.TrimSpace(fields[0]) != mediaType {
			continue
		}
		accepted := true
		for _, param := range fields[1:] {
			name, value, found := strings.Cut(strings.TrimSpace(param), "=")
			if found && strings.TrimSpace(name) == "q" {
				value = strings.TrimSpace(value)
				accepted = value != "0" && value != "0.0" && value != "0.00" && value != "0.000"
			}
		}
		if accepted {
			return true
		}
	}
	return false
}

func (dr *DynamicRewrite) isFallbackStatus(status int) bool {
	return dr.fallbackStatusCodes[status]
}

func (dr *DynamicRewrite) buildFallbackRequest(req *http.Request, internalBasePath string) *http.Request {
	fallbackReq := req.Clone(req.Context())
	fallbackReq.Header.Set(FallbackURLHeader, req.URL.Path)
	fallbackReq.Header.Del("Range")
	setPath(fallbackReq, dr.fallbackPath(req.URL.EscapedPath(), internalBasePath))
	return fallbackReq
}

func (dr *DynamicRewrite) fallbackPath(rewrittenPath string, internalBasePath string) string {
	if dr.fallbackPathComponent[0] == '/' {
		return strings.TrimSuffix(internalBasePath, "/") + dr.fallbackPathComponent
	}
	parts := strings.Split(rewrittenPath, "/")
	parts[len(parts)-1] = dr.fallbackPathComponent
	return strings.Join(parts, "/")
}

func setPath(req *http.Request, escapedPath string) {
	unescaped, err := url.PathUnescape(escapedPath)
	if err != nil {
		unescaped = escapedPath
	}
	req.URL.Path = unescaped
	req.URL.RawPath = escapedPath
	req.RequestURI = req.URL.RequestURI()
}

// interceptingWriter passes a response through unchanged unless its status is
// one to replace; then it swallows headers and body so the fallback response
// can be written to the real writer instead.
type interceptingWriter struct {
	rw              http.ResponseWriter
	header          http.Header
	shouldIntercept func(int) bool
	wroteHeader     bool
	intercepted     bool
	status          int
}

func (w *interceptingWriter) Header() http.Header {
	if w.wroteHeader && !w.intercepted {
		// Passed through: trailers set after the body must reach the real writer.
		return w.rw.Header()
	}
	return w.header
}

func (w *interceptingWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		// Informational responses (e.g. 103 Early Hints) pass through and are
		// not the final status.
		w.copyHeader()
		w.rw.WriteHeader(status)
		w.header = http.Header{}
		return
	}
	w.wroteHeader = true
	w.status = status
	if w.shouldIntercept(status) {
		w.intercepted = true
		return
	}
	w.copyHeader()
	w.rw.WriteHeader(status)
}

func (w *interceptingWriter) copyHeader() {
	target := w.rw.Header()
	for key, values := range w.header {
		target[key] = values
	}
}

func (w *interceptingWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.intercepted {
		return len(body), nil
	}
	return w.rw.Write(body)
}

func (w *interceptingWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.intercepted {
		return
	}
	if flusher, ok := w.rw.(http.Flusher); ok {
		flusher.Flush()
	}
}
