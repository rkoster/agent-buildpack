package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	items   []Sandbox
	get     Sandbox
	create  CreateRequest
	deleted string
	renewed time.Time
}

func (f *fakeBackend) Create(_ context.Context, r CreateRequest) (Sandbox, error) {
	f.create = r
	return Sandbox{ID: "sandbox-1", Status: SandboxStatus{State: "Running"}, CreatedAt: time.Now(), Entrypoint: r.Entrypoint}, nil
}
func (f *fakeBackend) List(context.Context) ([]Sandbox, error)      { return f.items, nil }
func (f *fakeBackend) Get(context.Context, string) (Sandbox, error) { return f.get, nil }
func (f *fakeBackend) Delete(_ context.Context, id string) error    { f.deleted = id; return nil }
func (f *fakeBackend) Renew(_ context.Context, _ string, t time.Time) (time.Time, error) {
	f.renewed = t
	return t, nil
}
func (f *fakeBackend) Endpoint(context.Context, string, int) (Endpoint, error) {
	return Endpoint{Endpoint: "http://127.0.0.1:18080/v1/sandboxes/sandbox-1/proxy/8080"}, nil
}

type fakeProxy struct {
	id   string
	port int
}

func (f *fakeProxy) ServeHTTP(w http.ResponseWriter, _ *http.Request, id string, port int) {
	f.id, f.port = id, port
	w.WriteHeader(http.StatusNoContent)
}
func (f *fakeProxy) ServeLocal(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 4 {
		f.id = parts[2]
		f.port, _ = strconv.Atoi(parts[4])
	}
	w.WriteHeader(http.StatusNoContent)
}
func (f *fakeProxy) Close() {}

func TestCreateSandboxLifecycle(t *testing.T) {
	backend, proxy := &fakeBackend{}, &fakeProxy{}
	api := NewAPI(backend, proxy, "")
	request := `{"image":{"uri":"example.test/sandbox:v1"},"timeout":600,"resourceLimits":{"memory":"512Mi"},"metadata":{"project":"demo"},"entrypoint":["/bin/sh"]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(request))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "sandbox-1" {
		t.Fatalf("response id: %v", response["id"])
	}
	if len(backend.create.Entrypoint) != 1 || backend.create.Metadata["project"] != "demo" {
		t.Fatalf("request not propagated: %+v", backend.create)
	}
}

func TestRejectUnsupportedCreationModes(t *testing.T) {
	tests := []struct{ name, body string }{
		{"missing image", `{"entrypoint":["/bin/sh"]}`},
		{"snapshot", `{"snapshotId":"snap-1","entrypoint":["/bin/sh"]}`},
		{"network policy", `{"image":{"uri":"img"},"entrypoint":["/bin/sh"],"networkPolicy":{"defaultAction":"deny"}}`},
		{"missing entrypoint", `{"image":{"uri":"img"}}`},
		{"reserved env", `{"image":{"uri":"img"},"entrypoint":["sh"],"env":{"VCAP_SERVICES":"{}"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := NewAPI(&fakeBackend{}, &fakeProxy{}, "")
			w := httptest.NewRecorder()
			api.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(tt.body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLoadServiceConfigFindsNamedProvisionerBinding(t *testing.T) {
	got, err := loadServiceConfig(`{"user-provided":[{"name":"cf-sandbox-api-opencode-agent-smoke","credentials":{"api_url":"https://api.example","token_url":"https://uaa.example","client_id":"opensandbox-capi","client_secret":"secret","space_guid":"space","sandbox_image":"example/image@sha256:abc","disk_quota_mb":4096,"ca_cert":"PEM","open_sandbox_api_key":"key"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "opensandbox-capi" || got.ClientSecret != "secret" || got.Image != "example/image@sha256:abc" || got.CACert != "PEM" || got.DiskQuotaMB != 4096 {
		t.Fatalf("named service binding not loaded: %+v", got)
	}
}

func TestMemoryUnitParsing(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int
	}{{"512Mi", 512}, {"1Gi", 1024}, {"2G", 2048}, {"bad", 768}} {
		if got := parseMemoryMB(test.input, 768); got != test.want {
			t.Errorf("parseMemoryMB(%q)=%d want %d", test.input, got, test.want)
		}
	}
}

func TestListSandboxPaginationAndFilters(t *testing.T) {
	backend := &fakeBackend{items: []Sandbox{
		{ID: "one", Status: SandboxStatus{State: "Running"}, Metadata: map[string]string{"team": "a"}},
		{ID: "two", Status: SandboxStatus{State: "Terminated"}, Metadata: map[string]string{"team": "a"}},
		{ID: "three", Status: SandboxStatus{State: "Running"}, Metadata: map[string]string{"team": "b"}},
	}}
	api := NewAPI(backend, &fakeProxy{}, "")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/sandboxes?state=Running&metadata=team%3Da&pageSize=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	var body struct {
		Items      []Sandbox `json:"items"`
		Pagination struct {
			TotalItems int  `json:"totalItems"`
			HasNext    bool `json:"hasNextPage"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != "one" || body.Pagination.TotalItems != 1 || body.Pagination.HasNext {
		t.Fatalf("unexpected list: %+v", body)
	}
}

func TestDeleteRenewEndpointAndAuth(t *testing.T) {
	backend, proxy := &fakeBackend{}, &fakeProxy{}
	api := NewAPI(backend, proxy, "")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/id-1", nil))
	if w.Code != http.StatusNoContent || backend.deleted != "id-1" {
		t.Fatalf("delete status=%d id=%q", w.Code, backend.deleted)
	}
	w = httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/sandboxes/id-1/endpoints/8080", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("endpoint status=%d", w.Code)
	}
	var endpoint Endpoint
	if err := json.Unmarshal(w.Body.Bytes(), &endpoint); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(endpoint.Endpoint, "http://127.0.0.1:18080") {
		t.Fatalf("endpoint=%s should be absolute", endpoint.Endpoint)
	}
	proxyReq := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/id-1/proxy/8080/health", nil)
	w = httptest.NewRecorder()
	api.ServeHTTP(w, proxyReq)
	if w.Code != http.StatusNoContent || proxy.id != "id-1" || proxy.port != 8080 {
		t.Fatalf("proxy request status=%d id=%s port=%d", w.Code, proxy.id, proxy.port)
	}
	api = NewAPI(backend, proxy, "secret")
	w = httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", w.Code)
	}
}

func TestAPIKeyIsIncludedInEndpointRequirements(t *testing.T) {
	backend, proxy := &fakeBackend{}, &fakeProxy{}
	api := NewAPI(backend, proxy, "secret")
	request := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sandbox-1/endpoints/8080", nil)
	request.Header.Set(opensandboxAPIKeyHeader, "secret")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var endpoint Endpoint
	if err := json.Unmarshal(response.Body.Bytes(), &endpoint); err != nil {
		t.Fatal(err)
	}
	if endpoint.Headers[opensandboxAPIKeyHeader] != "secret" {
		t.Fatalf("endpoint headers don't carry facade auth requirement: %#v", endpoint.Headers)
	}
	proxyRequest := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sandbox-1/proxy/8080/health", nil)
	proxyRequest.Header.Set(opensandboxAPIKeyHeader, "secret")
	proxyResponse := httptest.NewRecorder()
	api.ServeHTTP(proxyResponse, proxyRequest)
	if proxyResponse.Code != http.StatusNoContent {
		t.Fatalf("proxy status=%d body=%s", proxyResponse.Code, proxyResponse.Body.String())
	}
}

func TestCAPIStatusMapping(t *testing.T) {
	tests := []struct{ input, want int }{
		{http.StatusNotFound, http.StatusNotFound},
		{http.StatusForbidden, http.StatusForbidden},
		{http.StatusUnprocessableEntity, http.StatusBadRequest},
		{http.StatusInternalServerError, http.StatusBadGateway},
	}
	for _, test := range tests {
		if got := capiStatus(test.input); got != test.want {
			t.Fatalf("capiStatus(%d)=%d want=%d", test.input, got, test.want)
		}
	}
}

func TestFacadeDisabledStillServesHealth(t *testing.T) {
	api := NewAPI(nil, nil, "")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d", response.Code)
	}
	response = httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("facade disabled status=%d", response.Code)
	}
}

func TestAnnotationDataRoundTrip(t *testing.T) {
	metadata := map[string]string{"cf.app_guid": "guid", "project": "demo", "note": strings.Repeat("x", 400)}
	annotations := annotationMap(metadata)
	app := capiApp{}
	app.Metadata.Annotations = annotations
	decoded := appMetadata(app)
	for key, value := range metadata {
		if decoded[key] != value {
			t.Fatalf("annotation %q got %q want %q", key, decoded[key], value)
		}
	}
	for key, value := range annotations {
		if len(value) > 5000 {
			t.Fatalf("annotation %q exceeds CAPI value limit: %d", key, len(value))
		}
	}
}
