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
	originalPath := req.URL.Path
	dr.log.Debug("Original request: host=%s path=%s", req.Host, originalPath)
	internalBasePath := dr.rewriteRequest(req.Host, req)

	if !dr.fallbackEligible(req, originalPath) {
		dr.next.ServeHTTP(rw, req)
		return
	}

	// Normal responses stream straight through; only a fallback status is held
	// back so the fallback document can be served in its place.
	intercept := &interceptingWriter{rw: rw, header: http.Header{}, shouldIntercept: dr.isFallbackStatus}
	dr.next.ServeHTTP(intercept, req)
	if !intercept.intercepted {
		return
	}

	fallbackReq := dr.buildFallbackRequest(req, internalBasePath)
	dr.log.Debug("Serving fallback %s for %s (status %d)", fallbackReq.URL.Path, req.URL.Path, intercept.status)
	dr.next.ServeHTTP(rw, fallbackReq)
}

func (dr *DynamicRewrite) rewriteRequest(host string, req *http.Request) string {
	dynamicIdentifier, baseHost := dr.extractDynamicIdentifierAndHost(host)
	basePath := "/"
	if req.Header.Get(ReplacedHostHeader) == "" {
		dr.rewriteHost(req, baseHost)
	}
	if req.Header.Get(ReplacedPathHeader) == "" {
		basePath = dr.rewritePath(req, dynamicIdentifier)
	}
	return basePath
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

// fallbackEligible limits the fallback to page navigations: a request for a
// file (a last path segment with an extension) keeps the backend's status, so
// a missing asset stays a 404 instead of turning into the HTML page.
func (dr *DynamicRewrite) fallbackEligible(req *http.Request, originalPath string) bool {
	if dr.fallbackPathComponent == "" || req.Header.Get(FallbackURLHeader) != "" {
		return false
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if strings.Contains(req.Header.Get("Accept"), "text/html") {
		return true
	}
	return !strings.Contains(path.Base(originalPath), ".")
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
	return w.header
}

func (w *interceptingWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	if w.shouldIntercept(status) {
		w.intercepted = true
		return
	}
	target := w.rw.Header()
	for key, values := range w.header {
		target[key] = values
	}
	w.rw.WriteHeader(status)
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
