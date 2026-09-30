package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const opensandboxAPIKeyHeader = "OPEN-SANDBOX-API-KEY"

type Backend interface {
	Create(context.Context, CreateRequest) (Sandbox, error)
	List(context.Context) ([]Sandbox, error)
	Get(context.Context, string) (Sandbox, error)
	Delete(context.Context, string) error
	Renew(context.Context, string, time.Time) (time.Time, error)
	Endpoint(context.Context, string, int) (Endpoint, error)
}

type EndpointHandler interface {
	ServeHTTP(http.ResponseWriter, *http.Request, string, int)
	ServeLocal(http.ResponseWriter, *http.Request)
	Close()
}

type API struct {
	backend Backend
	proxy   EndpointHandler
	apiKey  string
}

func NewAPI(backend Backend, proxy EndpointHandler, apiKey string) *API {
	return &API{backend: backend, proxy: proxy, apiKey: strings.TrimSpace(apiKey)}
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{"healthy": true})
		return
	}
	if a.apiKey != "" && r.Header.Get(opensandboxAPIKeyHeader) != a.apiKey {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid OpenSandbox API key")
		return
	}
	if !a.backendEnabled() {
		writeError(w, http.StatusServiceUnavailable, "FACADE_DISABLED", "set OPEN_SANDBOX_API_ENABLED=true to enable the facade")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/ping" {
		writeJSON(w, http.StatusOK, map[string]any{"message": "pong"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1"), "/")
	if path == "" {
		path = "/"
	}
	if path == "/sandboxes" {
		a.sandboxes(w, r)
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "sandboxes" {
		id, err := url.PathUnescape(parts[1])
		if err != nil || !validSandboxID(id) {
			writeError(w, http.StatusBadRequest, "INVALID_SANDBOX_ID", "invalid sandbox ID")
			return
		}
		if len(parts) >= 3 && parts[2] == "proxy" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch && r.Method != http.MethodDelete && r.Method != http.MethodOptions {
				methodNotAllowed(w)
				return
			}
			a.proxy.ServeLocal(w, r)
			return
		}
		if len(parts) == 2 {
			a.sandbox(w, r, id)
			return
		}
		if len(parts) == 4 && parts[2] == "endpoints" {
			port, err := strconv.Atoi(parts[3])
			if err != nil || port < 1 || port > 65535 {
				writeError(w, http.StatusBadRequest, "INVALID_PORT", "port must be from 1 through 65535")
				return
			}
			if r.Method == http.MethodGet {
				if r.URL.Query().Get("expires") != "" {
					writeError(w, http.StatusNotImplemented, "UNSUPPORTED", "signed endpoints are not supported")
					return
				}
				endpoint, err := a.backend.Endpoint(r.Context(), id, port)
				if err != nil {
					writeBackendError(w, err)
					return
				}
				if a.apiKey != "" {
					endpoint.Headers = map[string]string{opensandboxAPIKeyHeader: a.apiKey}
				}
				writeJSON(w, http.StatusOK, endpoint)
				return
			}
		}
		if len(parts) == 3 && parts[2] == "renew-expiration" && r.Method == http.MethodPost {
			a.renew(w, r, id)
			return
		}
		if len(parts) == 2 && r.Method == http.MethodPatch {
			writeError(w, http.StatusNotImplemented, "UNSUPPORTED", "metadata updates are not supported")
			return
		}
	}
	http.NotFound(w, r)
}

func (a *API) backendEnabled() bool {
	return a.backend != nil && a.proxy != nil
}

func (a *API) sandboxes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var request CreateRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if err := request.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		sandbox, err := a.backend.Create(r.Context(), request)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		w.Header().Set("Location", "/v1/sandboxes/"+url.PathEscape(sandbox.ID))
		writeJSON(w, http.StatusAccepted, sandbox.CreateResponse())
	case http.MethodGet:
		items, err := a.backend.List(r.Context())
		if err != nil {
			writeBackendError(w, err)
			return
		}
		page, pageSize, err := pageParameters(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_PAGINATION", err.Error())
			return
		}
		states := r.URL.Query()["state"]
		metadata := parseMetadataFilter(r.URL.Query().Get("metadata"))
		filtered := make([]Sandbox, 0, len(items))
		for _, item := range items {
			if len(states) > 0 && !contains(states, item.Status.State) {
				continue
			}
			if !metadataMatches(item.Metadata, metadata) {
				continue
			}
			filtered = append(filtered, item)
		}
		total := len(filtered)
		start := (page - 1) * pageSize
		if start > total {
			start = total
		}
		end := start + pageSize
		if end > total {
			end = total
		}
		pages := 0
		if total > 0 {
			pages = (total + pageSize - 1) / pageSize
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":      filtered[start:end],
			"pagination": map[string]any{"page": page, "pageSize": pageSize, "totalItems": total, "totalPages": pages, "hasNextPage": page < pages},
		})
	default:
		methodNotAllowed(w)
	}
}

func (a *API) sandbox(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		sandbox, err := a.backend.Get(r.Context(), id)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sandbox)
	case http.MethodDelete:
		if err := a.backend.Delete(r.Context(), id); err != nil {
			writeBackendError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (a *API) renew(w http.ResponseWriter, r *http.Request, id string) {
	var request struct {
		ExpiresAt string `json:"expiresAt"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	expires, err := time.Parse(time.RFC3339, request.ExpiresAt)
	if err != nil || !expires.After(time.Now()) {
		writeError(w, http.StatusBadRequest, "INVALID_EXPIRATION", "expiresAt must be a future RFC3339 timestamp")
		return
	}
	updated, err := a.backend.Renew(r.Context(), id, expires)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"expiresAt": updated.UTC().Format(time.RFC3339)})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"code": code, "message": message})
}

func writeBackendError(w http.ResponseWriter, err error) {
	var backendErr *BackendError
	if errors.As(err, &backendErr) {
		writeError(w, backendErr.Status, backendErr.Code, backendErr.Message)
		return
	}
	writeError(w, http.StatusBadGateway, "CAPI_ERROR", err.Error())
}

func pageParameters(r *http.Request) (int, int, error) {
	page, pageSize := 1, 20
	var err error
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			return 0, 0, errors.New("page must be a positive integer")
		}
	}
	if raw := r.URL.Query().Get("pageSize"); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 100 {
			return 0, 0, errors.New("pageSize must be from 1 through 100")
		}
	}
	return page, pageSize, nil
}

func parseMetadataFilter(raw string) map[string]string {
	result := map[string]string{}
	decoded, decodeErr := url.QueryUnescape(raw)
	if decodeErr == nil {
		raw = decoded
	}
	for _, pair := range strings.Split(raw, "&") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		result[key] = value
	}
	return result
}

func metadataMatches(have, want map[string]string) bool {
	for key, value := range want {
		if have[key] != value {
			return false
		}
	}
	return true
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

func validSandboxID(id string) bool {
	return id != "" && len(id) <= 255 && !strings.ContainsAny(id, "/\\\x00")
}

func methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, POST, DELETE, PATCH")
	writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
}

func getenv(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func backendUnavailable(resource string) error {
	return &BackendError{Status: http.StatusServiceUnavailable, Code: "CAPI_UNAVAILABLE", Message: fmt.Sprintf("CAPI %s is unavailable", resource)}
}
