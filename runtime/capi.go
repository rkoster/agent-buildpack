package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type CAPIBackend struct {
	client *capiClient
	config Config
}

type serviceCredentialBindingResource struct {
	GUID          string `json:"guid"`
	LastOperation struct {
		State string `json:"state"`
	} `json:"last_operation"`
}

func isServiceCredentials(credentials map[string]any) bool {
	return credentials != nil && credentials["bucket"] != nil && credentials["access_key_id"] != nil && credentials["secret_access_key"] != nil && credentials["endpoint"] != nil
}

func (b *CAPIBackend) provisionWorkspaceService(ctx context.Context, sandboxAppGUID string) (string, string, map[string]any, error) {
	var offeringResponse struct {
		Resources []struct {
			GUID string `json:"guid"`
		} `json:"resources"`
	}
	query := url.Values{"names": {b.config.WorkspaceStorageOffering}, "per_page": {"100"}}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/service_offerings?"+query.Encode(), nil, &offeringResponse); err != nil {
		return "", "", nil, fmt.Errorf("find workspace service offering: %w", err)
	}
	if len(offeringResponse.Resources) == 0 || offeringResponse.Resources[0].GUID == "" {
		return "", "", nil, &BackendError{Status: http.StatusFailedDependency, Code: "WORKSPACE_SERVICE_UNAVAILABLE", Message: "configured workspace service offering is not available"}
	}
	var planResponse struct {
		Resources []struct {
			GUID      string `json:"guid"`
			Available bool   `json:"available"`
		} `json:"resources"`
	}
	query = url.Values{"service_offering_guids": {offeringResponse.Resources[0].GUID}, "names": {"ephemeral"}, "per_page": {"100"}}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/service_plans?"+query.Encode(), nil, &planResponse); err != nil {
		return "", "", nil, fmt.Errorf("find workspace service plan: %w", err)
	}
	if len(planResponse.Resources) == 0 || planResponse.Resources[0].GUID == "" || !planResponse.Resources[0].Available {
		return "", "", nil, &BackendError{Status: http.StatusFailedDependency, Code: "WORKSPACE_PLAN_UNAVAILABLE", Message: "configured workspace service plan is not available"}
	}
	name := "workspace-sync-" + shortGUID(sandboxAppGUID)
	body := map[string]any{
		"type": "managed",
		"name": name,
		"relationships": map[string]any{
			"space":        map[string]any{"data": map[string]string{"guid": b.config.SpaceGUID}},
			"service_plan": map[string]any{"data": map[string]string{"guid": planResponse.Resources[0].GUID}},
		},
	}
	var instance struct {
		GUID string `json:"guid"`
	}
	_, instanceHeaders, err := b.client.do(ctx, http.MethodPost, "/v3/service_instances", body, &instance)
	if err != nil {
		instance.GUID, _ = b.findServiceInstanceGUID(ctx, name)
		return instance.GUID, "", nil, &workspaceServiceError{instanceGUID: instance.GUID, err: fmt.Errorf("create workspace service instance: %w", err)}
	}
	if err := b.waitJob(ctx, instanceHeaders.Get("Location"), "create workspace service instance"); err != nil {
		if instance.GUID == "" {
			instance.GUID, _ = b.findServiceInstanceGUID(ctx, name)
		}
		return instance.GUID, "", nil, &workspaceServiceError{instanceGUID: instance.GUID, err: err}
	}
	if instance.GUID == "" {
		instance.GUID, err = b.findServiceInstanceGUID(ctx, name)
		if err != nil {
			return "", "", nil, err
		}
	}
	if err := b.waitLastOperation(ctx, "/v3/service_instances/"+url.PathEscape(instance.GUID), "workspace service instance"); err != nil {
		return instance.GUID, "", nil, &workspaceServiceError{instanceGUID: instance.GUID, err: err}
	}
	var binding serviceCredentialBindingResource
	bindingBody := map[string]any{
		"type": "app",
		"name": "workspace-sync",
		"relationships": map[string]any{
			"service_instance": map[string]any{"data": map[string]string{"guid": instance.GUID}},
			"app":              map[string]any{"data": map[string]string{"guid": sandboxAppGUID}},
		},
	}
	// A v3 app credential binding makes CAPI inject the broker credentials into
	// the sandbox's VCAP_SERVICES when the app starts; never copy them through
	// app environment variables or the facade response.
	_, bindingHeaders, err := b.client.do(ctx, http.MethodPost, "/v3/service_credential_bindings", bindingBody, &binding)
	if err != nil {
		binding.GUID, _ = b.findWorkspaceBindingGUID(ctx, instance.GUID, sandboxAppGUID)
		return instance.GUID, binding.GUID, nil, &workspaceServiceError{instanceGUID: instance.GUID, bindingGUID: binding.GUID, err: fmt.Errorf("bind Garage bucket service to sandbox: %w", err)}
	}
	if err := b.waitJob(ctx, bindingHeaders.Get("Location"), "create workspace service binding"); err != nil {
		if binding.GUID == "" {
			binding.GUID, _ = b.findWorkspaceBindingGUID(ctx, instance.GUID, sandboxAppGUID)
		}
		return instance.GUID, binding.GUID, nil, &workspaceServiceError{instanceGUID: instance.GUID, bindingGUID: binding.GUID, err: err}
	}
	if binding.GUID == "" {
		binding.GUID, err = b.findWorkspaceBindingGUID(ctx, instance.GUID, sandboxAppGUID)
		if err != nil {
			return instance.GUID, "", nil, &workspaceServiceError{instanceGUID: instance.GUID, err: err}
		}
	}
	bindingCredentials, err := b.serviceBindingCredentials(ctx, binding.GUID)
	if err != nil {
		return instance.GUID, binding.GUID, nil, &workspaceServiceError{instanceGUID: instance.GUID, bindingGUID: binding.GUID, err: err}
	}
	if !isServiceCredentials(bindingCredentials) {
		return instance.GUID, binding.GUID, nil, &workspaceServiceError{instanceGUID: instance.GUID, bindingGUID: binding.GUID, err: errors.New("Garage binding is missing bucket, endpoint, or S3 credentials")}
	}
	bindingCredentials["__instance_guid"] = instance.GUID
	bindingCredentials["__binding_guid"] = binding.GUID
	return instance.GUID, binding.GUID, bindingCredentials, nil
}

type workspaceServiceError struct {
	instanceGUID string
	bindingGUID  string
	err          error
}

func (e *workspaceServiceError) Error() string { return e.err.Error() }
func (e *workspaceServiceError) Unwrap() error { return e.err }

func (b *CAPIBackend) serviceBindingCredentials(ctx context.Context, bindingGUID string) (map[string]any, error) {
	var response struct {
		Credentials map[string]any `json:"credentials"`
	}
	path := "/v3/service_credential_bindings/" + url.PathEscape(bindingGUID) + "/details"
	if _, _, err := b.client.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, fmt.Errorf("get Garage service binding credentials: %w", err)
	}
	return response.Credentials, nil
}

func shortGUID(guid string) string {
	guid = strings.ReplaceAll(guid, "-", "")
	if len(guid) > 20 {
		return guid[:20]
	}
	return guid
}

func (b *CAPIBackend) findServiceInstanceGUID(ctx context.Context, name string) (string, error) {
	var response struct {
		Resources []struct {
			GUID string `json:"guid"`
		} `json:"resources"`
	}
	query := url.Values{"names": {name}, "space_guids": {b.config.SpaceGUID}, "per_page": {"100"}}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/service_instances?"+query.Encode(), nil, &response); err != nil {
		return "", fmt.Errorf("find created workspace service instance: %w", err)
	}
	if len(response.Resources) == 0 || response.Resources[0].GUID == "" {
		return "", errors.New("created workspace service instance was not discoverable")
	}
	return response.Resources[0].GUID, nil
}

func (b *CAPIBackend) findWorkspaceBindingGUID(ctx context.Context, serviceGUID, appGUID string) (string, error) {
	var response struct {
		Resources []struct {
			GUID          string `json:"guid"`
			Relationships struct {
				App struct {
					Data struct {
						GUID string `json:"guid"`
					} `json:"data"`
				} `json:"app"`
			} `json:"relationships"`
		} `json:"resources"`
	}
	query := url.Values{"service_instance_guids": {serviceGUID}, "per_page": {"100"}}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/service_credential_bindings?"+query.Encode(), nil, &response); err != nil {
		return "", fmt.Errorf("find created workspace binding: %w", err)
	}
	for _, binding := range response.Resources {
		if binding.Relationships.App.Data.GUID == appGUID && binding.GUID != "" {
			return binding.GUID, nil
		}
	}
	return "", errors.New("created workspace service binding was not discoverable")
}

func (b *CAPIBackend) waitLastOperation(ctx context.Context, path, label string) error {
	if strings.HasSuffix(path, "/") {
		return nil
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		var state struct {
			GUID          string `json:"guid"`
			LastOperation struct {
				State       string `json:"state"`
				Description string `json:"description"`
			} `json:"last_operation"`
		}
		if _, _, err := b.client.do(ctx, http.MethodGet, path, nil, &state); err != nil {
			return fmt.Errorf("check %s status: %w", label, err)
		}
		if state.GUID == "" && state.LastOperation.State == "" {
			return nil
		}
		switch strings.ToUpper(state.LastOperation.State) {
		case "SUCCEEDED":
			return nil
		case "FAILED":
			return fmt.Errorf("%s failed: %s", label, state.LastOperation.Description)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s", label)
		case <-ticker.C:
		}
	}
}

func (b *CAPIBackend) deleteWorkspaceService(ctx context.Context, serviceGUID, bindingGUID string) error {
	if bindingGUID != "" {
		_, headers, err := b.client.do(ctx, http.MethodDelete, "/v3/service_credential_bindings/"+url.PathEscape(bindingGUID), nil, nil)
		if err != nil && !isCAPIStatus(err, http.StatusNotFound) {
			return fmt.Errorf("delete workspace service binding: %w", err)
		}
		if err == nil {
			if err := b.waitJob(ctx, headers.Get("Location"), "delete workspace binding"); err != nil {
				return err
			}
		}
	}
	if serviceGUID != "" {
		_, headers, err := b.client.do(ctx, http.MethodDelete, "/v3/service_instances/"+url.PathEscape(serviceGUID), nil, nil)
		if err != nil && !isCAPIStatus(err, http.StatusNotFound) {
			return fmt.Errorf("delete workspace service instance: %w", err)
		}
		if err == nil {
			if err := b.waitJob(ctx, headers.Get("Location"), "delete workspace service instance"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *CAPIBackend) waitJob(ctx context.Context, location, label string) error {
	if location == "" {
		return nil
	}
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(u.Path, "/v3/jobs/") {
		return fmt.Errorf("%s returned an invalid CAPI job location", label)
	}
	path := u.Path
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	for {
		var job struct {
			State  string `json:"state"`
			Errors []struct {
				Detail string `json:"detail"`
			} `json:"errors"`
		}
		if _, _, err := b.client.do(ctx, http.MethodGet, path, nil, &job); err != nil {
			return fmt.Errorf("poll %s job: %w", label, err)
		}
		switch strings.ToUpper(job.State) {
		case "COMPLETE":
			return nil
		case "FAILED":
			message := "CAPI async operation failed"
			if len(job.Errors) > 0 && job.Errors[0].Detail != "" {
				message = job.Errors[0].Detail
			}
			return fmt.Errorf("%s: %s", label, message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s", label)
		case <-ticker.C:
		}
	}
}

type capiClient struct {
	http        *http.Client
	config      Config
	accessToken string
	tokenExpiry time.Time
	mu          chan struct{}
}

func newCAPIClient(cfg Config) *capiClient {
	c := &capiClient{http: newCAPIHTTPClient(cfg), config: cfg, accessToken: cfg.AccessToken, mu: make(chan struct{}, 1)}
	c.mu <- struct{}{}
	if cfg.AccessToken != "" {
		c.tokenExpiry = time.Now().Add(5 * time.Minute)
	}
	return c
}

func newCAPIHTTPClient(cfg Config) *http.Client {
	return newCAPIClientTransport(cfg)
}

func (c *capiClient) token(ctx context.Context) (string, error) {
	<-c.mu
	defer func() { c.mu <- struct{}{} }()
	if c.accessToken != "" && time.Until(c.tokenExpiry) > time.Minute {
		return c.accessToken, nil
	}
	if c.config.TokenURL == "" {
		if c.accessToken != "" {
			return c.accessToken, nil
		}
		return "", errors.New("CAPI access token not configured")
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	tokenURL := c.config.TokenURL
	if !strings.HasSuffix(tokenURL, "/oauth/token") {
		tokenURL = strings.TrimRight(tokenURL, "/") + "/oauth/token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.config.ClientID != "" && c.config.ClientSecret != "" {
		req.SetBasicAuth(c.config.ClientID, c.config.ClientSecret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("UAA token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return "", err
	}
	if token.AccessToken == "" {
		return "", errors.New("UAA response did not include an access token")
	}
	c.accessToken = token.AccessToken
	if token.ExpiresIn < 1 {
		token.ExpiresIn = 300
	}
	c.tokenExpiry = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

func (c *capiClient) do(ctx context.Context, method, path string, input, output any) (int, http.Header, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.config.APIURL, "/")+path, body)
	if err != nil {
		return 0, nil, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		var envelope struct {
			Errors []struct {
				Title  string `json:"title"`
				Detail string `json:"detail"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(payload, &envelope)
		message := strings.TrimSpace(string(payload))
		if len(envelope.Errors) > 0 && envelope.Errors[0].Detail != "" {
			message = envelope.Errors[0].Detail
		}
		if message == "" {
			message = resp.Status
		}
		return resp.StatusCode, resp.Header, &BackendError{Status: capiStatus(resp.StatusCode), Code: "CAPI_ERROR", Message: message}
	}
	if output != nil && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(output); err != nil {
			return resp.StatusCode, resp.Header, err
		}
	}
	return resp.StatusCode, resp.Header, nil
}

func capiStatus(status int) int {
	switch status {
	case http.StatusUnauthorized:
		return http.StatusBadGateway
	case http.StatusForbidden:
		return http.StatusForbidden
	case http.StatusConflict:
		return http.StatusConflict
	case http.StatusGone:
		return http.StatusGone
	case http.StatusNotFound:
		return http.StatusNotFound
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		return http.StatusBadRequest
	case http.StatusTooManyRequests:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func (b *CAPIBackend) Create(ctx context.Context, request CreateRequest) (Sandbox, error) {
	now := time.Now().UTC()
	name := appSafeName(b.config.AppPrefix, now)
	appBody := map[string]any{
		"name":                  name,
		"relationships":         map[string]any{"space": map[string]any{"data": map[string]string{"guid": b.config.SpaceGUID}}},
		"environment_variables": sandboxEnvironment(request.Env),
	}
	imageBased := request.Image != nil && strings.TrimSpace(request.Image.URI) != ""
	if imageBased {
		appBody["lifecycle"] = map[string]any{"type": "docker", "data": map[string]any{}}
	}
	var app capiApp
	if _, _, err := b.client.do(ctx, http.MethodPost, "/v3/apps", appBody, &app); err != nil {
		return Sandbox{}, err
	}
	cleanup := true
	metadataPersisted := false
	workspaceServiceGUID := ""
	workspaceBindingGUID := ""
	workspaceInstanceCreated := false
	defer func() {
		if cleanup {
			if workspaceServiceGUID != "" && workspaceInstanceCreated {
				_ = b.deleteWorkspaceService(context.Background(), workspaceServiceGUID, workspaceBindingGUID)
			}
			if metadataPersisted {
				_ = b.Delete(context.Background(), app.GUID)
			} else {
				_, _, _ = b.client.do(context.Background(), http.MethodDelete, "/v3/apps/"+url.PathEscape(app.GUID), nil, nil)
			}
		}
	}()
	metadata := cloneMap(request.Metadata)
	metadata["cf.app_guid"] = app.GUID
	metadata["cf.space_guid"] = b.config.SpaceGUID
	metadata["cf.app_name"] = name
	if imageBased {
		metadata["cf.image"] = request.Image.URI
		if len(request.Image.URI) > 512 {
			return Sandbox{}, &BackendError{Status: http.StatusBadRequest, Code: "IMAGE_REF_TOO_LONG", Message: "image URI exceeds the CAPI image reference length limit"}
		}
	}
	metadata["cf.port"] = strconv.Itoa(b.config.SandboxPort)
	metadata["cf.routes"] = "{}"
	entrypoint, _ := json.Marshal(request.Entrypoint)
	metadata["cf.entrypoint"] = string(entrypoint)
	if imageBased && b.config.WorkspaceStorageOffering != "" {
		instanceGUID, bindingGUID, _, provisionErr := b.provisionWorkspaceService(ctx, app.GUID)
		if provisionErr != nil {
			var partial *workspaceServiceError
			if errors.As(provisionErr, &partial) {
				instanceGUID, bindingGUID = partial.instanceGUID, partial.bindingGUID
				workspaceServiceGUID, workspaceBindingGUID = instanceGUID, bindingGUID
				workspaceInstanceCreated = instanceGUID != ""
			}
			if instanceGUID != "" && !metadataPersisted {
				if cleanupErr := b.deleteWorkspaceService(context.Background(), instanceGUID, bindingGUID); cleanupErr != nil {
					return Sandbox{}, fmt.Errorf("%w (workspace cleanup also failed: %v)", provisionErr, cleanupErr)
				}
				workspaceServiceGUID, workspaceBindingGUID = "", ""
			}
			return Sandbox{}, fmt.Errorf("provision sandbox workspace service: %w", provisionErr)
		}
		workspaceServiceGUID, workspaceBindingGUID = instanceGUID, bindingGUID
		workspaceInstanceCreated = instanceGUID != ""
		metadata["cf.workspace_service_guid"] = instanceGUID
		metadata["cf.workspace_binding_guid"] = bindingGUID
	}
	if request.Timeout != nil {
		metadata["cf.expires_at"] = now.Add(time.Duration(*request.Timeout) * time.Second).Format(time.RFC3339)
	}
	if len(request.Image.URI) > 512 {
		return Sandbox{}, &BackendError{Status: http.StatusBadRequest, Code: "IMAGE_REF_TOO_LONG", Message: "image URI exceeds the CAPI image reference length limit"}
	}
	encodedMetadata, _ := json.Marshal(metadata)
	if len(encodedMetadata) > 4500 {
		return Sandbox{}, &BackendError{Status: http.StatusBadRequest, Code: "METADATA_TOO_LARGE", Message: "sandbox metadata exceeds the CAPI app annotation limit"}
	}
	if err := b.persistMetadata(ctx, app.GUID, metadata); err != nil {
		return Sandbox{}, fmt.Errorf("persist initial sandbox ownership: %w", err)
	}
	metadataPersisted = true

	if imageBased {
		packageData := map[string]any{"image": request.Image.URI}
		if request.Image.Auth != nil {
			packageData["username"] = request.Image.Auth.Username
			packageData["password"] = request.Image.Auth.Password
		}
		packageBody := map[string]any{
			"type":          "docker",
			"data":          packageData,
			"relationships": map[string]any{"app": map[string]any{"data": map[string]string{"guid": app.GUID}}},
		}
		var pkg capiPackage
		if _, _, err := b.client.do(ctx, http.MethodPost, "/v3/packages", packageBody, &pkg); err != nil {
			return Sandbox{}, err
		}
		var build capiBuild
		if _, _, err := b.client.do(ctx, http.MethodPost, "/v3/builds", map[string]any{"package": map[string]string{"guid": pkg.GUID}}, &build); err != nil {
			return Sandbox{}, err
		}
		build, err := b.waitForBuild(ctx, build.GUID)
		if err != nil {
			return Sandbox{}, err
		}
		if build.Droplet.GUID == "" {
			return Sandbox{}, errors.New("CAPI build completed without a droplet GUID")
		}
		if _, _, err := b.client.do(ctx, http.MethodPatch, "/v3/apps/"+url.PathEscape(app.GUID)+"/relationships/current_droplet", map[string]any{"data": map[string]string{"guid": build.Droplet.GUID}}, nil); err != nil {
			return Sandbox{}, err
		}
		if err := b.setProcessCommand(ctx, app.GUID, request); err != nil {
			return Sandbox{}, err
		}
	}
	if _, _, err := b.client.do(ctx, http.MethodPost, "/v3/apps/"+url.PathEscape(app.GUID)+"/actions/start", map[string]any{}, nil); err != nil {
		return Sandbox{}, err
	}
	app, err := b.waitForApp(ctx, app.GUID)
	if err != nil {
		return Sandbox{}, err
	}
	if _, err := b.ensureProtectedRoute(ctx, app.GUID, b.config.SandboxPort); err != nil {
		return Sandbox{}, err
	}
	app, err = b.getOwnedApp(ctx, app.GUID)
	if err != nil {
		return Sandbox{}, err
	}
	if _, err := b.waitForApp(ctx, app.GUID); err != nil {
		return Sandbox{}, err
	}
	cleanup = false
	return appToSandbox(app, request.Image, request.Entrypoint, metadata, now), nil
}

func (b *CAPIBackend) List(ctx context.Context) ([]Sandbox, error) {
	apps, err := b.listApps(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Sandbox, 0, len(apps))
	for _, app := range apps {
		metadata := appMetadata(app)
		if metadata["cf.space_guid"] != b.config.SpaceGUID || metadata["cf.app_guid"] != app.GUID {
			continue
		}
		if expires := metadata["cf.expires_at"]; expires != "" {
			if expiry, parseErr := time.Parse(time.RFC3339, expires); parseErr == nil && time.Now().After(expiry) {
				go func(guid string) { _ = b.Delete(context.Background(), guid) }(app.GUID)
				continue
			}
		}
		result = append(result, appToSandbox(app, &ImageSpec{URI: metadata["cf.image"]}, nil, metadata, app.CreatedAt))
	}
	return result, nil
}

func (b *CAPIBackend) Get(ctx context.Context, id string) (Sandbox, error) {
	app, err := b.getOwnedApp(ctx, id)
	if err != nil {
		return Sandbox{}, err
	}
	metadata := appMetadata(app)
	if expires := metadata["cf.expires_at"]; expires != "" {
		if expiry, parseErr := time.Parse(time.RFC3339, expires); parseErr == nil && time.Now().After(expiry) {
			go func() { _ = b.Delete(context.Background(), app.GUID) }()
		}
	}
	var image *ImageSpec
	if metadata["cf.image"] != "" {
		image = &ImageSpec{URI: metadata["cf.image"]}
	}
	entrypoint := []string{"/bin/sh"}
	if value := metadata["cf.entrypoint"]; value != "" {
		_ = json.Unmarshal([]byte(value), &entrypoint)
	}
	return appToSandbox(app, image, entrypoint, metadata, app.CreatedAt), nil
}

func (b *CAPIBackend) Delete(ctx context.Context, id string) error {
	app, err := b.getOwnedApp(ctx, id)
	if err != nil {
		return err
	}
	metadata := appMetadata(app)
	if err := b.deleteWorkspaceService(ctx, metadata["cf.workspace_service_guid"], metadata["cf.workspace_binding_guid"]); err != nil {
		return err
	}
	var routes map[string]routeRecord
	_ = json.Unmarshal([]byte(metadata["cf.routes"]), &routes)
	for _, route := range routes {
		if route.RouteGUID != "" {
			if err := b.removeRoutePolicy(ctx, route.RouteGUID); err != nil {
				return err
			}
			var destinations struct {
				Destinations []struct {
					GUID string `json:"guid"`
				} `json:"destinations"`
			}
			if _, _, listErr := b.client.do(ctx, http.MethodGet, "/v3/routes/"+url.PathEscape(route.RouteGUID)+"/destinations", nil, &destinations); listErr == nil {
				for _, destination := range destinations.Destinations {
					if _, _, err := b.client.do(ctx, http.MethodDelete, "/v3/routes/"+url.PathEscape(route.RouteGUID)+"/destinations/"+url.PathEscape(destination.GUID), nil, nil); err != nil && !isCAPIStatus(err, http.StatusNotFound) {
						return fmt.Errorf("delete sandbox route destination: %w", err)
					}
				}
			}
			if _, _, err := b.client.do(ctx, http.MethodDelete, "/v3/routes/"+url.PathEscape(route.RouteGUID), nil, nil); err != nil && !isCAPIStatus(err, http.StatusNotFound) {
				return fmt.Errorf("delete sandbox route: %w", err)
			}
		}
	}
	_, _, err = b.client.do(ctx, http.MethodDelete, "/v3/apps/"+url.PathEscape(id), nil, nil)
	return err
}

func (b *CAPIBackend) setSandboxEnvironment(ctx context.Context, appGUID string, additional map[string]string) error {
	var appEnvironment struct {
		EnvironmentVariables map[string]string `json:"environment_variables"`
	}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/apps/"+url.PathEscape(appGUID)+"/env", nil, &appEnvironment); err != nil {
		return fmt.Errorf("read sandbox environment: %w", err)
	}
	if appEnvironment.EnvironmentVariables == nil {
		appEnvironment.EnvironmentVariables = make(map[string]string)
	}
	for key, value := range additional {
		appEnvironment.EnvironmentVariables[key] = value
	}
	_, _, err := b.client.do(ctx, http.MethodPatch, "/v3/apps/"+url.PathEscape(appGUID), map[string]any{"environment_variables": appEnvironment.EnvironmentVariables}, nil)
	if err != nil {
		return fmt.Errorf("patch sandbox environment: %w", err)
	}
	return nil
}

func (b *CAPIBackend) removeRoutePolicy(ctx context.Context, routeGUID string) error {
	var response struct {
		Resources []struct {
			GUID   string `json:"guid"`
			Source string `json:"source"`
		} `json:"resources"`
	}
	query := url.Values{"route_guids": {routeGUID}, "per_page": {"100"}}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/route_policies?"+query.Encode(), nil, &response); err != nil {
		return fmt.Errorf("list policies before route deletion: %w", err)
	}
	want := "cf:app:" + b.config.AgentAppGUID
	for _, policy := range response.Resources {
		if policy.Source != want {
			return &BackendError{Status: http.StatusConflict, Code: "FOREIGN_ROUTE_POLICY", Message: "refusing to delete a route with a policy owned by another caller"}
		}
		if _, _, err := b.client.do(ctx, http.MethodDelete, "/v3/route_policies/"+url.PathEscape(policy.GUID), nil, nil); err != nil && !isCAPIStatus(err, http.StatusNotFound) {
			return fmt.Errorf("delete sandbox route policy: %w", err)
		}
	}
	return nil
}

func isCAPIStatus(err error, status int) bool {
	var backendErr *BackendError
	return errors.As(err, &backendErr) && backendErr.Status == status
}

func (b *CAPIBackend) Renew(ctx context.Context, id string, expires time.Time) (time.Time, error) {
	app, err := b.getOwnedApp(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	metadata := appMetadata(app)
	metadata["cf.expires_at"] = expires.UTC().Format(time.RFC3339)
	err = b.persistMetadata(ctx, id, metadata)
	return expires.UTC(), err
}

func (b *CAPIBackend) Endpoint(ctx context.Context, id string, port int) (Endpoint, error) {
	if _, err := b.ensureProtectedRoute(ctx, id, port); err != nil {
		return Endpoint{}, err
	}
	return Endpoint{Endpoint: "http://" + b.config.ListenAddress + "/v1/sandboxes/" + url.PathEscape(id) + "/proxy/" + strconv.Itoa(port)}, nil
}

func (b *CAPIBackend) ensureProtectedRoute(ctx context.Context, appGUID string, port int) (string, error) {
	app, err := b.getOwnedApp(ctx, appGUID)
	if err != nil {
		return "", err
	}
	metadata := appMetadata(app)
	var routes map[string]routeRecord
	if err := json.Unmarshal([]byte(metadata["cf.routes"]), &routes); err != nil || routes == nil {
		routes = make(map[string]routeRecord)
	}
	portKey := strconv.Itoa(port)
	if existing := routes[portKey]; existing.Host != "" && existing.Domain != "" {
		if err := b.verifyOwnerRoutePolicy(ctx, existing.RouteGUID); err != nil {
			return "", err
		}
		if err := b.verifyDestination(ctx, existing.RouteGUID, appGUID, port); err != nil {
			return "", err
		}
		return existing.Host + "." + existing.Domain, nil
	}
	domain, err := b.findDomain(ctx, b.config.IdentityDomain)
	if err != nil {
		return "", err
	}
	host := shortHost(b.config.AppPrefix, app.GUID) + "-" + strconv.Itoa(port)
	routeBody := map[string]any{
		"host": host,
		"relationships": map[string]any{
			"domain": map[string]any{"data": map[string]string{"guid": domain.GUID}},
			"space":  map[string]any{"data": map[string]string{"guid": b.config.SpaceGUID}},
		},
	}
	var route capiRoute
	if _, _, err := b.client.do(ctx, http.MethodPost, "/v3/routes", routeBody, &route); err != nil {
		return "", fmt.Errorf("create protected route: %w", err)
	}
	cleanupRoute := true
	defer func() {
		if cleanupRoute {
			_ = b.deleteRouteAndPolicy(context.Background(), route.GUID)
		}
	}()
	policyBody := map[string]any{
		"source":        "cf:app:" + b.config.AgentAppGUID,
		"relationships": map[string]any{"route": map[string]any{"data": map[string]string{"guid": route.GUID}}},
	}
	var policy capiPolicy
	if _, _, err := b.client.do(ctx, http.MethodPost, "/v3/route_policies", policyBody, &policy); err != nil {
		return "", fmt.Errorf("create owner route policy: %w", err)
	}
	cleanupPolicy := true
	defer func() {
		if cleanupPolicy {
			_, _, _ = b.client.do(context.Background(), http.MethodDelete, "/v3/route_policies/"+url.PathEscape(policy.GUID), nil, nil)
		}
	}()
	routes[portKey] = routeRecord{RouteGUID: route.GUID, PolicyGUID: policy.GUID, Host: host, Domain: domain.Name}
	routesJSON, _ := json.Marshal(routes)
	metadata["cf.routes"] = string(routesJSON)
	err = b.persistMetadata(ctx, app.GUID, metadata)
	if err != nil {
		return "", err
	}
	if _, _, err := b.client.do(ctx, http.MethodPost, "/v3/routes/"+url.PathEscape(route.GUID)+"/destinations", map[string]any{
		"destinations": []map[string]any{{"app": map[string]string{"guid": app.GUID}, "port": port}},
	}, nil); err != nil {
		return "", fmt.Errorf("map protected route: %w", err)
	}
	cleanupRoute = false
	cleanupPolicy = false
	return host + "." + domain.Name, nil
}

func (b *CAPIBackend) deleteRouteAndPolicy(ctx context.Context, routeGUID string) error {
	if err := b.removeRoutePolicy(ctx, routeGUID); err != nil {
		return err
	}
	var destinations struct {
		Destinations []struct {
			GUID string `json:"guid"`
		} `json:"destinations"`
	}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/routes/"+url.PathEscape(routeGUID)+"/destinations", nil, &destinations); err != nil && !isCAPIStatus(err, http.StatusNotFound) {
		return err
	}
	for _, destination := range destinations.Destinations {
		if _, _, err := b.client.do(ctx, http.MethodDelete, "/v3/routes/"+url.PathEscape(routeGUID)+"/destinations/"+url.PathEscape(destination.GUID), nil, nil); err != nil && !isCAPIStatus(err, http.StatusNotFound) {
			return err
		}
	}
	if _, _, err := b.client.do(ctx, http.MethodDelete, "/v3/routes/"+url.PathEscape(routeGUID), nil, nil); err != nil && !isCAPIStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

func (b *CAPIBackend) verifyOwnerRoutePolicy(ctx context.Context, routeGUID string) error {
	var response struct {
		Resources []struct {
			Source string `json:"source"`
		} `json:"resources"`
	}
	query := url.Values{"route_guids": {routeGUID}, "per_page": {"100"}}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/route_policies?"+query.Encode(), nil, &response); err != nil {
		return err
	}
	want := "cf:app:" + b.config.AgentAppGUID
	found := false
	for _, policy := range response.Resources {
		if policy.Source == want {
			found = true
			continue
		}
		return &BackendError{Status: http.StatusConflict, Code: "FOREIGN_ROUTE_POLICY", Message: "sandbox route has an unexpected caller policy"}
	}
	if !found {
		return &BackendError{Status: http.StatusForbidden, Code: "ROUTE_POLICY_MISSING", Message: "sandbox route is not authorized for its owning agent app"}
	}
	return nil
}

func (b *CAPIBackend) verifyDestination(ctx context.Context, routeGUID, appGUID string, port int) error {
	var response struct {
		Destinations []struct {
			App struct {
				GUID string `json:"guid"`
			} `json:"app"`
			Port int `json:"port"`
		} `json:"destinations"`
	}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/routes/"+url.PathEscape(routeGUID)+"/destinations", nil, &response); err != nil {
		return err
	}
	for _, d := range response.Destinations {
		if d.App.GUID == appGUID && d.Port == port {
			return nil
		}
	}
	return &BackendError{Status: http.StatusConflict, Code: "ROUTE_DESTINATION_MISMATCH", Message: "sandbox route does not target the expected app and port"}
}

type routeRecord struct {
	RouteGUID  string `json:"route_guid"`
	PolicyGUID string `json:"policy_guid"`
	Host       string `json:"host"`
	Domain     string `json:"domain"`
}

func (b *CAPIBackend) getOwnedApp(ctx context.Context, id string) (capiApp, error) {
	var app capiApp
	_, _, err := b.client.do(ctx, http.MethodGet, "/v3/apps/"+url.PathEscape(id)+"?include=space", nil, &app)
	if err != nil {
		return capiApp{}, err
	}
	metadata := appMetadata(app)
	if metadata["cf.app_guid"] != app.GUID || metadata["cf.space_guid"] != b.config.SpaceGUID {
		return capiApp{}, &BackendError{Status: http.StatusNotFound, Code: "SANDBOX_NOT_FOUND", Message: "sandbox not found"}
	}
	return app, nil
}

func (b *CAPIBackend) listApps(ctx context.Context) ([]capiApp, error) {
	query := url.Values{"space_guids": {b.config.SpaceGUID}, "per_page": {"100"}, "order_by": {"-created_at"}}
	var response struct {
		Resources []capiApp `json:"resources"`
	}
	_, _, err := b.client.do(ctx, http.MethodGet, "/v3/apps?"+query.Encode(), nil, &response)
	return response.Resources, err
}

func (b *CAPIBackend) waitForBuild(ctx context.Context, id string) (capiBuild, error) {
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		var build capiBuild
		if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/builds/"+url.PathEscape(id), nil, &build); err != nil {
			return capiBuild{}, err
		}
		switch strings.ToUpper(build.State) {
		case "STAGED":
			return build, nil
		case "FAILED", "EXPIRED", "STAGING_FAILED":
			return capiBuild{}, &BackendError{Status: http.StatusBadGateway, Code: "SANDBOX_BUILD_FAILED", Message: build.Error}
		}
		select {
		case <-ctx.Done():
			return capiBuild{}, ctx.Err()
		case <-deadline.C:
			return capiBuild{}, &BackendError{Status: http.StatusGatewayTimeout, Code: "SANDBOX_BUILD_TIMEOUT", Message: "timed out waiting for Docker app staging"}
		case <-ticker.C:
		}
	}
}

func (b *CAPIBackend) waitForApp(ctx context.Context, id string) (capiApp, error) {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		var app capiApp
		if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/apps/"+url.PathEscape(id), nil, &app); err != nil {
			return capiApp{}, err
		}
		var processes struct {
			Resources []struct {
				GUID string `json:"guid"`
				Type string `json:"type"`
			} `json:"resources"`
		}
		if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/apps/"+url.PathEscape(id)+"/processes", nil, &processes); err != nil {
			return capiApp{}, err
		}
		running := false
		for _, process := range processes.Resources {
			if process.Type != "web" {
				continue
			}
			var stats struct {
				Resources []struct {
					State string `json:"state"`
				} `json:"resources"`
			}
			if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/processes/"+url.PathEscape(process.GUID)+"/stats", nil, &stats); err != nil {
				return capiApp{}, err
			}
			for _, instance := range stats.Resources {
				if instance.State == "RUNNING" {
					running = true
					break
				}
			}
			break
		}
		if running {
			return app, nil
		}
		select {
		case <-ctx.Done():
			return capiApp{}, ctx.Err()
		case <-deadline.C:
			return capiApp{}, &BackendError{Status: http.StatusGatewayTimeout, Code: "SANDBOX_START_TIMEOUT", Message: "timed out waiting for Docker app start"}
		case <-ticker.C:
		}
	}
}

func (b *CAPIBackend) setProcessCommand(ctx context.Context, appGUID string, request CreateRequest) error {
	var response struct {
		Resources []struct {
			GUID   string `json:"guid"`
			Type   string `json:"type"`
			Memory int    `json:"memory_in_mb"`
			Disk   int    `json:"disk_in_mb"`
		} `json:"resources"`
	}
	_, _, err := b.client.do(ctx, http.MethodGet, "/v3/apps/"+url.PathEscape(appGUID)+"/processes", nil, &response)
	if err != nil {
		return err
	}
	processGUID := ""
	processMemory := 1024
	processDisk := b.config.SandboxDiskMB
	if processDisk < 1 {
		processDisk = 4096
	}
	for _, process := range response.Resources {
		if process.Type == "web" {
			processGUID = process.GUID
			if process.Memory > 0 {
				processMemory = process.Memory
			}
			if process.Disk > processDisk {
				processDisk = process.Disk
			}
			break
		}
	}
	if processGUID == "" {
		return errors.New("CAPI did not create the Docker app web process")
	}
	// CF replaces a Docker image's ENTRYPOINT with the configured process
	// command. Run the image bootstrap explicitly so it restores the workspace
	// before launching execd and the requested workload.
	command := append([]string{"/opt/opensandbox/bootstrap"}, request.Entrypoint...)
	_, _, err = b.client.do(ctx, http.MethodPatch, "/v3/processes/"+url.PathEscape(processGUID), map[string]any{"command": shellCommand(command)}, nil)
	if err != nil {
		return err
	}
	memory, disk := processMemory, processDisk
	if raw := request.ResourceLimits["memory"]; raw != "" {
		memory = parseMemoryMB(raw, memory)
	}
	if _, _, err := b.client.do(ctx, http.MethodPost, "/v3/processes/"+url.PathEscape(processGUID)+"/actions/scale", map[string]any{"instances": 1, "memory_in_mb": memory, "disk_in_mb": disk}, nil); err != nil {
		return err
	}
	return nil
}

func parseMemoryMB(raw string, fallback int) int {
	value := strings.TrimSpace(strings.ToLower(raw))
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(value, "gib"):
		multiplier, value = 1024, strings.TrimSuffix(value, "gib")
	case strings.HasSuffix(value, "gb"):
		multiplier, value = 1000, strings.TrimSuffix(value, "gb")
	case strings.HasSuffix(value, "mi"):
		value = strings.TrimSuffix(value, "mi")
	case strings.HasSuffix(value, "mib"):
		value = strings.TrimSuffix(value, "mib")
	case strings.HasSuffix(value, "mb"):
		multiplier, value = 1, strings.TrimSuffix(value, "mb")
	case strings.HasSuffix(value, "gi"):
		multiplier, value = 1024, strings.TrimSuffix(value, "gi")
	case strings.HasSuffix(value, "g"):
		multiplier, value = 1024, strings.TrimSuffix(value, "g")
	case strings.HasSuffix(value, "m"):
		value = strings.TrimSuffix(value, "m")
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 {
		return fallback
	}
	mb := n * multiplier
	if mb > 16384 {
		return 16384
	}
	return int(mb)
}

func sandboxEnvironment(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "VCAP_") || upper == "PORT" || strings.HasPrefix(upper, "CF_INSTANCE_") {
			continue
		}
		result[key] = value
	}
	return result
}

func mergeSandboxEnvironment(base map[string]string, extra map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func shellCommand(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
	}
	return strings.Join(quoted, " ")
}

func (b *CAPIBackend) findDomain(ctx context.Context, name string) (capiDomain, error) {
	var response struct {
		Resources []capiDomain `json:"resources"`
	}
	query := url.Values{"names": {name}, "per_page": {"100"}}
	if _, _, err := b.client.do(ctx, http.MethodGet, "/v3/domains?"+query.Encode(), nil, &response); err != nil {
		return capiDomain{}, err
	}
	for _, domain := range response.Resources {
		if domain.Name == name {
			if domain.Internal || !domain.EnforceRoutePolicies {
				return capiDomain{}, &BackendError{Status: http.StatusFailedDependency, Code: "IDENTITY_DOMAIN_NOT_ENFORCED", Message: "configured identity domain must be external and enforce route policies"}
			}
			return domain, nil
		}
	}
	return capiDomain{}, &BackendError{Status: http.StatusFailedDependency, Code: "IDENTITY_DOMAIN_NOT_FOUND", Message: "configured CF identity-aware route domain not found"}
}

type capiApp struct {
	GUID          string    `json:"guid"`
	Name          string    `json:"name"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
	Relationships struct {
		Space struct {
			Data struct {
				GUID string `json:"guid"`
			} `json:"data"`
		} `json:"space"`
	} `json:"relationships"`
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

type capiPackage struct {
	GUID string `json:"guid"`
}
type capiBuild struct {
	GUID    string `json:"guid"`
	State   string `json:"state"`
	Error   string `json:"error"`
	Droplet struct {
		GUID string `json:"guid"`
	} `json:"droplet"`
}
type capiDomain struct {
	GUID                 string `json:"guid"`
	Name                 string `json:"name"`
	Internal             bool   `json:"internal"`
	EnforceRoutePolicies bool   `json:"enforce_route_policies"`
}
type capiRoute struct {
	GUID string `json:"guid"`
}
type capiPolicy struct {
	GUID string `json:"guid"`
}

func appMetadata(app capiApp) map[string]string {
	if raw := app.Metadata.Annotations["cf.opensandbox/data"]; raw != "" {
		var metadata map[string]string
		if err := json.Unmarshal([]byte(raw), &metadata); err == nil && metadata != nil {
			return metadata
		}
	}
	result := map[string]string{}
	for key, value := range app.Metadata.Annotations {
		if strings.HasPrefix(key, "cf-opensandbox/") {
			result[strings.TrimPrefix(key, "cf-opensandbox/")] = value
		} else {
			result[key] = value
		}
	}
	return result
}

func appToSandbox(app capiApp, image *ImageSpec, entrypoint []string, metadata map[string]string, createdAt time.Time) Sandbox {
	state := "Pending"
	switch strings.ToUpper(app.State) {
	case "STARTED":
		state = "Running"
	case "STOPPED":
		state = "Terminated"
	}
	status := SandboxStatus{State: state, LastTransitionAt: app.CreatedAt}
	if state == "Running" {
		status.Reason = "app_started"
		status.Message = "CF Docker app is started"
	}
	sandbox := Sandbox{ID: app.GUID, Image: image, Status: status, Metadata: publicMetadata(metadata), Entrypoint: entrypoint, CreatedAt: createdAt.UTC()}
	if raw := metadata["cf.expires_at"]; raw != "" {
		if expires, err := time.Parse(time.RFC3339, raw); err == nil {
			sandbox.ExpiresAt = &expires
		}
	}
	if sandbox.Entrypoint == nil {
		sandbox.Entrypoint = []string{"/bin/sh"}
	}
	return sandbox
}

func publicMetadata(metadata map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range metadata {
		if strings.HasPrefix(key, "cf.") {
			continue
		}
		result[key] = value
	}
	return result
}

func annotationMap(metadata map[string]string) map[string]string {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return map[string]string{}
	}
	return map[string]string{"cf.opensandbox/data": string(encoded)}
}

func (b *CAPIBackend) persistMetadata(ctx context.Context, appGUID string, metadata map[string]string) error {
	_, _, err := b.client.do(ctx, http.MethodPatch, "/v3/apps/"+url.PathEscape(appGUID), map[string]any{"metadata": map[string]any{"annotations": annotationMap(metadata)}}, nil)
	return err
}

func cloneMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values)+8)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func appSafeName(prefix string, now time.Time) string {
	prefix = strings.ToLower(strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, prefix), "-"))
	if prefix == "" {
		prefix = "osb-sbx"
	}
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	name := fmt.Sprintf("%s-%x", prefix, now.UnixNano())
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func shortHost(prefix, guid string) string {
	if len(guid) > 12 {
		guid = guid[:12]
	}
	return strings.TrimRight(prefix, "-") + "-" + guid
}
