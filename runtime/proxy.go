package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

type EndpointProxy struct {
	config Config
	capi   *capiClient
	client *http.Client
	mu     sync.Mutex
	ports  map[string]map[int]string
}

func (p *EndpointProxy) Publish(_ context.Context, sandboxID string, port int, _ string) (string, error) {
	if !validSandboxID(sandboxID) || port < 1 || port > 65535 {
		return "", &BackendError{Status: http.StatusBadRequest, Code: "INVALID_ENDPOINT", Message: "invalid endpoint target"}
	}
	if _, err := p.localEndpoint(context.Background(), sandboxID, port); err != nil {
		return "", err
	}
	return "http://" + p.config.ListenAddress + "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/proxy/" + strconv.Itoa(port), nil
}

func NewEndpointProxy(cfg Config, client *capiClient) *EndpointProxy {
	p := &EndpointProxy{config: cfg, capi: client, client: newIdentityHTTPClient(cfg), ports: make(map[string]map[int]string)}
	return p
}

func (p *EndpointProxy) Close() {}

func (p *EndpointProxy) ServeHTTP(w http.ResponseWriter, r *http.Request, sandboxID string, port int) {
	p.serve(w, r, sandboxID, port)
}

func (p *EndpointProxy) localEndpoint(ctx context.Context, sandboxID string, port int) (string, error) {
	if !validSandboxID(sandboxID) {
		return "", &BackendError{Status: http.StatusNotFound, Code: "SANDBOX_NOT_FOUND", Message: "sandbox not found"}
	}
	backend := &CAPIBackend{client: p.capi, config: p.config}
	if _, err := backend.ensureProtectedRoute(ctx, sandboxID, port); err != nil {
		return "", err
	}
	p.mu.Lock()
	if byPort := p.ports[sandboxID]; byPort != nil && byPort[port] != "" {
		endpoint := byPort[port]
		p.mu.Unlock()
		return endpoint, nil
	}
	p.mu.Unlock()

	app, err := backend.getOwnedApp(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	metadata := appMetadata(app)
	var routes map[string]routeRecord
	if err := json.Unmarshal([]byte(metadata["cf.routes"]), &routes); err != nil {
		return "", backendUnavailable("protected route")
	}
	record := routes[strconv.Itoa(port)]
	endpoint := "https://" + record.Host + "." + record.Domain
	p.mu.Lock()
	if p.ports[sandboxID] == nil {
		p.ports[sandboxID] = make(map[int]string)
	}
	p.ports[sandboxID][port] = endpoint
	p.mu.Unlock()
	return endpoint, nil
}

func (p *EndpointProxy) ServeLocal(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "v1" || parts[1] != "sandboxes" || parts[3] != "proxy" {
		http.NotFound(w, r)
		return
	}
	id, err := url.PathUnescape(parts[2])
	if err != nil || !validSandboxID(id) {
		writeError(w, http.StatusBadRequest, "INVALID_SANDBOX_ID", "invalid sandbox ID")
		return
	}
	port, err := strconv.Atoi(parts[4])
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, "INVALID_PORT", "port must be from 1 through 65535")
		return
	}
	p.serve(w, r, id, port)
}

func (p *EndpointProxy) serve(w http.ResponseWriter, r *http.Request, id string, port int) {
	endpoint, err := p.localEndpoint(r.Context(), id, port)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	base, _ := url.Parse(endpoint)
	reverse := p.reverseProxy(base)
	reverse.Director = func(req *http.Request) {
		path := "/"
		marker := "/proxy/" + strconv.Itoa(port)
		if i := strings.Index(r.URL.Path, marker); i >= 0 && len(r.URL.Path) > i+len(marker) {
			path = r.URL.Path[i+len(marker):]
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
		}
		setProxyTarget(req, r, base, path)
	}
	reverse.ServeHTTP(w, r)
}

func (p *EndpointProxy) reverseProxy(base *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(base)
	proxy.FlushInterval = -1
	proxy.Transport = p.client.Transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("sandbox endpoint proxy %s: %v", base.Host, err)
		http.Error(w, "sandbox endpoint unavailable", http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del(opensandboxAPIKeyHeader)
		return nil
	}
	return proxy
}

func setProxyTarget(req, incoming *http.Request, base *url.URL, path string) {
	req.URL.Scheme, req.URL.Host, req.Host = base.Scheme, base.Host, base.Host
	req.URL.Path, req.URL.RawPath, req.URL.RawQuery = path, "", incoming.URL.RawQuery
	req.Header.Set("X-Forwarded-Host", incoming.Host)
	req.Header.Del(opensandboxAPIKeyHeader)
}
