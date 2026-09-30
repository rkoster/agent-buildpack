package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProvisionWorkspaceServiceUsesCAPIAppCredentialBinding(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v3/service_offerings":
			if got := r.URL.Query().Get("names"); got != "Garage" {
				t.Errorf("offering filter=%q", got)
			}
			_, _ = w.Write([]byte(`{"resources":[{"guid":"offering-guid"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v3/service_plans":
			_, _ = w.Write([]byte(`{"resources":[{"guid":"plan-guid","available":true}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v3/service_instances":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "workspace-sync-sandboxguid" {
				t.Errorf("service instance name=%v", body["name"])
			}
			_, _ = w.Write([]byte(`{"guid":"instance-guid"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v3/service_instances/instance-guid":
			_, _ = w.Write([]byte(`{"guid":"instance-guid","last_operation":{"state":"succeeded"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v3/service_credential_bindings":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode binding: %v", err)
			}
			if body["type"] != "app" || body["name"] != "workspace-sync" {
				t.Errorf("binding type/name=%v/%v", body["type"], body["name"])
			}
			relationships := body["relationships"].(map[string]any)
			for relationship, wantGUID := range map[string]string{"app": "sandbox-guid", "service_instance": "instance-guid"} {
				data := relationships[relationship].(map[string]any)["data"].(map[string]any)
				if data["guid"] != wantGUID {
					t.Errorf("%s relationship GUID=%v want %s", relationship, data["guid"], wantGUID)
				}
			}
			_, _ = w.Write([]byte(`{"guid":"binding-guid"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v3/service_credential_bindings/binding-guid/details":
			_, _ = w.Write([]byte(`{"credentials":{"bucket":"cf-esb","region":"garage","endpoint":"http://garage.example","access_key_id":"access","secret_access_key":"secret"}}`))
		default:
			t.Errorf("unexpected CAPI request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := &capiClient{
		http:        server.Client(),
		config:      Config{APIURL: server.URL, AccessToken: "test-token", SpaceGUID: "space-guid", WorkspaceStorageOffering: "Garage"},
		accessToken: "test-token",
		tokenExpiry: time.Now().Add(time.Hour),
		mu:          make(chan struct{}, 1),
	}
	client.mu <- struct{}{}
	backend := &CAPIBackend{client: client, config: client.config}
	instanceGUID, bindingGUID, credentials, err := backend.provisionWorkspaceService(context.Background(), "sandbox-guid")
	if err != nil {
		t.Fatal(err)
	}
	if instanceGUID != "instance-guid" || bindingGUID != "binding-guid" {
		t.Fatalf("instance/binding=%q/%q", instanceGUID, bindingGUID)
	}
	if credentials["bucket"] != "cf-esb" || credentials["access_key_id"] != "access" {
		t.Fatalf("credentials=%v", credentials)
	}
	if indexOf(calls, "POST /v3/service_credential_bindings") < indexOf(calls, "POST /v3/service_instances") {
		t.Fatalf("service binding must be created after the service instance: %v", calls)
	}
}

func TestSetProcessCommandStartsImageBootstrapBeforeWorkload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v3/apps/sandbox-guid/processes":
			_, _ = w.Write([]byte(`{"resources":[{"guid":"process-guid","type":"web","memory_in_mb":1024}]}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v3/processes/process-guid":
			var body struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			want := "'/opt/opensandbox/bootstrap' '/bin/sh' '-c' 'sleep 3600'"
			if body.Command != want {
				t.Errorf("process command=%q want %q", body.Command, want)
			}
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v3/processes/process-guid/actions/scale":
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected CAPI request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	client := &capiClient{http: server.Client(), config: Config{APIURL: server.URL, AccessToken: "test-token"}, accessToken: "test-token", tokenExpiry: time.Now().Add(time.Hour), mu: make(chan struct{}, 1)}
	client.mu <- struct{}{}
	backend := &CAPIBackend{client: client, config: client.config}
	if err := backend.setProcessCommand(context.Background(), "sandbox-guid", CreateRequest{Entrypoint: []string{"/bin/sh", "-c", "sleep 3600"}}); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedRouteCreatesOwnerPolicyBeforeMapping(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v3/apps/sandbox-guid":
			_ = json.NewEncoder(w).Encode(capiApp{GUID: "sandbox-guid", Metadata: struct {
				Annotations map[string]string `json:"annotations"`
			}{Annotations: annotationMap(map[string]string{"cf.app_guid": "sandbox-guid", "cf.space_guid": "space-guid", "cf.routes": "{}"})}})
		case r.Method == http.MethodGet && r.URL.Path == "/v3/domains":
			_, _ = w.Write([]byte(`{"resources":[{"guid":"domain-guid","name":"apps.identity","internal":false,"enforce_route_policies":true}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v3/routes":
			_, _ = w.Write([]byte(`{"guid":"route-guid"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v3/route_policies":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["source"] != "cf:app:agent-guid" {
				t.Errorf("route source policy=%v", body["source"])
			}
			_, _ = w.Write([]byte(`{"guid":"policy-guid"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v3/routes/route-guid/destinations":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode destination mapping: %v", err)
			}
			destinations := body["destinations"].([]any)
			destination := destinations[0].(map[string]any)
			if _, hasWeight := destination["weight"]; hasWeight {
				t.Errorf("weighted destinations are only accepted when replacing all destinations")
			}
			if destination["port"] != float64(44772) {
				t.Errorf("destination port=%v want 44772", destination["port"])
			}
			_, _ = w.Write([]byte(`{"destinations":[]}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v3/apps/sandbox-guid":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			metadata := body["metadata"].(map[string]any)
			annotations := metadata["annotations"].(map[string]any)
			if annotations["cf.opensandbox/data"] == nil {
				t.Errorf("route metadata was not persisted: %v", annotations)
			}
		default:
			t.Errorf("unexpected CAPI request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := &capiClient{
		http:        server.Client(),
		config:      Config{APIURL: server.URL, AccessToken: "test-token", SpaceGUID: "space-guid", AgentAppGUID: "agent-guid", IdentityDomain: "apps.identity", AppPrefix: "osb-sbx", ListenAddress: "127.0.0.1:18080"},
		accessToken: "test-token",
		tokenExpiry: time.Now().Add(time.Hour),
		mu:          make(chan struct{}, 1),
	}
	client.mu <- struct{}{}
	backend := &CAPIBackend{client: client, config: client.config}
	endpoint, err := backend.Endpoint(context.Background(), "sandbox-guid", 44772)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Endpoint != "http://127.0.0.1:18080/v1/sandboxes/sandbox-guid/proxy/44772" {
		t.Fatalf("endpoint URL=%q should use the localhost facade proxy", endpoint.Endpoint)
	}

	got, err := backend.ensureProtectedRoute(context.Background(), "sandbox-guid", 44772)
	if err != nil {
		t.Fatal(err)
	}
	if got != "osb-sbx-sandbox-guid-44772.apps.identity" {
		t.Fatalf("route host=%q", got)
	}
	policyIndex := indexOf(calls, "POST /v3/route_policies")
	mapIndex := indexOf(calls, "POST /v3/routes/route-guid/destinations")
	if policyIndex < 0 || mapIndex < 0 || policyIndex >= mapIndex {
		t.Fatalf("owner policy must exist before destination mapping: %v", calls)
	}
}

func TestIdentityClientReloadsRotatedCertificateOnNewConnection(t *testing.T) {
	seen := make(chan string, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate missing", http.StatusUnauthorized)
			return
		}
		seen <- r.TLS.PeerCertificates[0].Subject.CommonName
		_, _ = w.Write([]byte("ok"))
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	server.StartTLS()
	defer server.Close()

	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "instance.crt"), filepath.Join(dir, "instance.key")
	writeClientCert(t, certPath, keyPath, "instance-one")
	rootPool := x509.NewCertPool()
	rootPool.AddCert(server.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootPool, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		return &cert, err
	}}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	do := func() {
		t.Helper()
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", response.StatusCode)
		}
	}
	do()
	if got := <-seen; got != "instance-one" {
		t.Fatalf("first cert=%q", got)
	}
	client.CloseIdleConnections()
	writeClientCert(t, certPath, keyPath, "instance-two")
	do()
	if got := <-seen; got != "instance-two" {
		t.Fatalf("rotated cert=%q", got)
	}
}

func writeClientCert(t *testing.T, certPath, keyPath, name string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}
