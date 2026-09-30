package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := mergeBoundModelProvider(); err != nil {
		log.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	var backend Backend
	var proxy EndpointHandler
	if cfg.Enabled {
		client := newCAPIClient(cfg)
		backend = &CAPIBackend{client: client, config: cfg}
		proxy = NewEndpointProxy(cfg, client)
	}
	api := NewAPI(backend, proxy, cfg.OpenSandboxAPIKey)

	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           api,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		if proxy != nil {
			proxy.Close()
		}
	}()

	log.Printf("OpenSandbox-compatible CAPI app facade listening on %s", cfg.ListenAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func mergeBoundModelProvider() error {
	rawServices := os.Getenv("VCAP_SERVICES")
	if rawServices == "" {
		return nil
	}
	var services map[string][]struct {
		Name        string         `json:"name"`
		Credentials map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(rawServices), &services); err != nil {
		return fmt.Errorf("parse VCAP_SERVICES for OpenCode model binding: %w", err)
	}
	for _, bindings := range services {
		for _, binding := range bindings {
			if binding.Name != "dgx-spark-model" {
				continue
			}
			provider, ok := binding.Credentials["provider"].(map[string]any)
			if !ok {
				return errors.New("DGX Spark model binding has no provider configuration")
			}
			providerID, ok := provider["id"].(string)
			if !ok || providerID == "" {
				return errors.New("DGX Spark model binding has invalid provider id")
			}
			model, ok := binding.Credentials["model"].(string)
			if !ok || model == "" {
				return errors.New("DGX Spark model binding has no selected model")
			}
			providerModels, err := fetchOpenAICompatibleModels(provider)
			if err != nil {
				return fmt.Errorf("fetch DGX Spark /v1/models: %w", err)
			}
			if len(providerModels) == 0 {
				return errors.New("DGX Spark /v1/models returned no models")
			}
			selectedProvider, selectedModel, found := strings.Cut(model, "/")
			if !found || selectedProvider != providerID || providerModels[selectedModel] == nil {
				model = providerID + "/" + firstMapKey(providerModels)
			}
			provider["models"] = providerModels
			configDir := os.Getenv("HOME") + "/.opencode"
			path := configDir + "/opencode.json"
			data, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read generated OpenCode config: %w", err)
			}
			config := map[string]any{}
			if len(data) > 0 {
				if err := json.Unmarshal(data, &config); err != nil {
					return fmt.Errorf("parse generated OpenCode config: %w", err)
				}
			}
			providers, _ := config["provider"].(map[string]any)
			if providers == nil {
				providers = make(map[string]any)
			}
			providers[providerID] = provider
			config["provider"] = providers
			config["model"] = model
			if err := os.MkdirAll(configDir, 0755); err != nil {
				return fmt.Errorf("create OpenCode config directory: %w", err)
			}
			encoded, err := json.MarshalIndent(config, "", "  ")
			if err != nil {
				return fmt.Errorf("encode merged OpenCode config: %w", err)
			}
	if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		return fmt.Errorf("write merged OpenCode config: %w", err)
	}
			log.Printf("Loaded OpenCode model provider %s from service binding", providerID)
			return nil
		}
	}
	return nil
}

type openAIModelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
		TopProvider   struct {
			ContextLength       int `json:"context_length"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		} `json:"top_provider"`
	} `json:"data"`
}

func fetchOpenAICompatibleModels(provider map[string]any) (map[string]any, error) {
	options, ok := provider["options"].(map[string]any)
	if !ok {
		return nil, errors.New("provider options are missing")
	}
	baseURL, ok := options["baseURL"].(string)
	if !ok || strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("provider baseURL is missing")
	}
	modelsURL := strings.TrimRight(baseURL, "/") + "/models"
	request, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey, ok := options["apiKey"].(string); ok && apiKey != "" {
		request.SetBasicAuth(apiKey, "")
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, fmt.Errorf("endpoint returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	body, err := ioutil.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var catalog openAIModelsResponse
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, err
	}
	models := make(map[string]any, len(catalog.Data))
	for _, entry := range catalog.Data {
		if entry.ID == "" {
			continue
		}
		name := entry.Name
		if name == "" {
			name = entry.ID
		}
		contextLength := entry.ContextLength
		if contextLength < 1 {
			contextLength = entry.TopProvider.ContextLength
		}
		outputLimit := entry.TopProvider.MaxCompletionTokens
		if contextLength < 1 {
			contextLength = 32768
		}
		if outputLimit < 1 {
			outputLimit = contextLength
		}
		definition := map[string]any{
			"name":        name,
			"description": name,
			"limit":       map[string]any{"context": contextLength, "output": outputLimit},
		}
		models[entry.ID] = definition
	}
	return models, nil
}

func firstMapKey(values map[string]any) string {
	for key := range values {
		return key
	}
	return ""
}

type Config struct {
	ListenAddress            string
	APIURL                   string
	TokenURL                 string
	ClientID                 string
	ClientSecret             string
	AccessToken              string
	CACert                   string
	SpaceGUID                string
	AgentAppGUID             string
	IdentityDomain           string
	SandboxPort              int
	SandboxDiskMB            int
	Image                    string
	WorkspaceStorageOffering string
	AppPrefix                string
	OpenSandboxAPIKey        string
	Enabled                  bool
	InstanceCert             string
	InstanceKey              string
}

func loadConfig() (Config, error) {
	services, err := loadServiceConfig(os.Getenv("VCAP_SERVICES"))
	if err != nil {
		return Config{}, err
	}
	get := func(key, fallback string) string {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
		return strings.TrimSpace(fallback)
	}
	enabled := strings.EqualFold(get("OPEN_SANDBOX_API_ENABLED", "false"), "true")
	port := 44772
	if enabled {
		var portErr error
		port, portErr = strconv.Atoi(get("CF_SANDBOX_PORT", "44772"))
		if portErr != nil || port < 1 || port > 65535 {
			return Config{}, errors.New("CF_SANDBOX_PORT must be a TCP port from 1 through 65535")
		}
	}
	diskQuotaMB := 4096
	if raw := strings.TrimSpace(os.Getenv("CF_SANDBOX_DISK_QUOTA_MB")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 {
			return Config{}, errors.New("CF_SANDBOX_DISK_QUOTA_MB must be a positive integer")
		}
		diskQuotaMB = parsed
	} else if services.DiskQuotaMB > 0 {
		diskQuotaMB = services.DiskQuotaMB
	}
	cfg := Config{
		ListenAddress:            get("OPEN_SANDBOX_LISTEN", "127.0.0.1:18080"),
		APIURL:                   get("CF_API_URL", services.APIURL),
		TokenURL:                 get("CF_TOKEN_URL", services.TokenURL),
		ClientID:                 get("CF_CLIENT_ID", services.ClientID),
		ClientSecret:             get("CF_CLIENT_SECRET", services.ClientSecret),
		AccessToken:              get("CF_ACCESS_TOKEN", services.AccessToken),
		CACert:                   get("CF_CA_CERT", services.CACert),
		SpaceGUID:                get("CF_SPACE_GUID", services.SpaceGUID),
		AgentAppGUID:             get("CF_APP_GUID", ""),
		IdentityDomain:           get("CF_IDENTITY_DOMAIN", services.IdentityDomain),
		SandboxPort:              port,
		SandboxDiskMB:            diskQuotaMB,
		Image:                    get("CF_SANDBOX_IMAGE", services.Image),
		WorkspaceStorageOffering: get("CF_SANDBOX_WORKSPACE_STORAGE_OFFERING", ""),
		AppPrefix:                get("CF_SANDBOX_APP_PREFIX", "osb-sbx"),
		OpenSandboxAPIKey:        get("OPEN_SANDBOX_API_KEY", services.OpenSandboxAPIKey),
		Enabled:                  enabled,
		InstanceCert:             os.Getenv("CF_INSTANCE_CERT"),
		InstanceKey:              os.Getenv("CF_INSTANCE_KEY"),
	}
	if raw := os.Getenv("VCAP_APPLICATION"); raw != "" {
		var app struct {
			ApplicationID string `json:"application_id"`
			SpaceID       string `json:"space_id"`
		}
		if err := json.Unmarshal([]byte(raw), &app); err != nil {
			return Config{}, fmt.Errorf("parse VCAP_APPLICATION: %w", err)
		}
		if cfg.AgentAppGUID == "" {
			cfg.AgentAppGUID = app.ApplicationID
		}
		if cfg.SpaceGUID == "" {
			cfg.SpaceGUID = app.SpaceID
		}
	}
	if cfg.Enabled {
		if cfg.APIURL == "" || cfg.SpaceGUID == "" || cfg.AgentAppGUID == "" || cfg.Image == "" {
			return Config{}, errors.New("configure CF_API_URL, CF_SPACE_GUID, CF_SANDBOX_IMAGE, and app metadata (or bind cf-sandbox-api)")
		}
		if cfg.AccessToken == "" && (cfg.TokenURL == "" || (cfg.ClientID == "") != (cfg.ClientSecret == "")) {
			return Config{}, errors.New("configure UAA token URL and optional paired client credentials in cf-sandbox-api binding")
		}
		if cfg.TokenURL == "https://uaa.10.246.0.21.sslip.io" {
			cfg.TokenURL = strings.TrimRight(cfg.TokenURL, "/") + "/oauth/token"
		}
		if cfg.AccessToken == "" && (cfg.ClientID == "" || cfg.ClientSecret == "") {
			return Config{}, errors.New("authorization-code-only UAA clients need a pre-provisioned access token; unattended facade requires a client_credentials client")
		}
		if (cfg.InstanceCert == "") != (cfg.InstanceKey == "") {
			return Config{}, errors.New("CF_INSTANCE_CERT and CF_INSTANCE_KEY must be set together")
		}
		if cfg.OpenSandboxAPIKey == "" {
			return Config{}, errors.New("OPEN_SANDBOX_API_KEY must be set when the facade is enabled")
		}
	}
	host, _, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		return Config{}, errors.New("OPEN_SANDBOX_LISTEN must include a loopback host and port")
	}
	if host != "127.0.0.1" && host != "localhost" {
		return Config{}, errors.New("OPEN_SANDBOX_LISTEN must bind to loopback only")
	}
	return cfg, nil
}

type serviceConfig struct {
	APIURL, TokenURL, ClientID, ClientSecret, AccessToken, SpaceGUID, Image string
	DiskQuotaMB                                                             int
	CACert                                                                  string
	OpenSandboxAPIKey                                                       string
	IdentityDomain                                                          string
}

func loadServiceConfig(raw string) (serviceConfig, error) {
	if raw == "" {
		return serviceConfig{}, nil
	}
	var services map[string][]struct {
		Name        string         `json:"name"`
		Credentials map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(raw), &services); err != nil {
		return serviceConfig{}, fmt.Errorf("parse VCAP_SERVICES: %w", err)
	}
	for _, bindings := range services {
		for _, binding := range bindings {
			if binding.Name != "cf-sandbox-api" && !strings.HasPrefix(binding.Name, "cf-sandbox-api-") {
				continue
			}
			c := binding.Credentials
			get := func(key string) string {
				if value, ok := c[key].(string); ok {
					return value
				}
				return ""
			}
			diskQuotaMB := 4096
			if raw := get("disk_quota_mb"); raw != "" {
				if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
					diskQuotaMB = parsed
				}
			}
			return serviceConfig{APIURL: get("api_url"), TokenURL: get("token_url"), ClientID: get("client_id"), ClientSecret: get("client_secret"), AccessToken: get("access_token"), SpaceGUID: get("space_guid"), Image: get("sandbox_image"), DiskQuotaMB: diskQuotaMB, CACert: get("ca_cert"), OpenSandboxAPIKey: get("open_sandbox_api_key"), IdentityDomain: get("identity_domain")}, nil
		}
	}
	return serviceConfig{}, nil
}

func newCAPIClientTransport(cfg Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if cfg.CACert != "" && !rootCAs.AppendCertsFromPEM([]byte(cfg.CACert)) {
		rootCAs = x509.NewCertPool()
		rootCAs.AppendCertsFromPEM([]byte(cfg.CACert))
	}
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs}
	return &http.Client{Transport: transport, Timeout: 60 * time.Second}
}

func newIdentityHTTPClient(cfg Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if cfg.CACert != "" && !rootCAs.AppendCertsFromPEM([]byte(cfg.CACert)) {
		rootCAs = x509.NewCertPool()
		rootCAs.AppendCertsFromPEM([]byte(cfg.CACert))
	}
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs}
	transport.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		certPath, keyPath, err := currentInstanceIdentityPaths(cfg)
		if err != nil {
			return nil, err
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, err
		}
		return &cert, nil
	}
	return &http.Client{Transport: transport, Timeout: 0}
}

func currentInstanceIdentityPaths(cfg Config) (string, string, error) {
	certPath, keyPath := cfg.InstanceCert, cfg.InstanceKey
	if certPath == "" {
		certPath = os.Getenv("CF_INSTANCE_CERT")
	}
	if keyPath == "" {
		keyPath = os.Getenv("CF_INSTANCE_KEY")
	}
	if (certPath == "") != (keyPath == "") {
		return "", "", errors.New("CF_INSTANCE_CERT and CF_INSTANCE_KEY must be set together")
	}
	if certPath == "" || keyPath == "" {
		certPath, keyPath = os.Getenv("CF_INSTANCE_CERT"), os.Getenv("CF_INSTANCE_KEY")
	}
	if certPath == "" || keyPath == "" {
		return "", "", errors.New("CF app instance identity certificate is unavailable")
	}
	return certPath, keyPath, nil
}
