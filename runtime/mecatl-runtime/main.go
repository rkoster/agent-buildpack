package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stacklok/mecatl/adapters/grpcdriver"
	"github.com/stacklok/mecatl/adapters/redisstore"
	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type serviceBinding struct {
	Name        string            `json:"name"`
	Label       string            `json:"label"`
	Tags        []string          `json:"tags"`
	Credentials map[string]any    `json:"credentials"`
}

type redisBinding struct {
	Address  string
	Username string
	Password string
	TLS      bool
	CACert   string
}

type modelBinding struct {
	Model    string `json:"model"`
	Provider struct {
		Options struct {
			BaseURL string `json:"baseURL"`
			APIKey  string `json:"apiKey"`
		} `json:"options"`
	} `json:"provider"`
}

type application struct {
	URIs []string `json:"application_uris"`
}

type childProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
}

func main() {
	var err error
	if len(os.Args) == 2 && os.Args[1] == "driver" {
		err = runDriver()
	} else if len(os.Args) == 1 {
		err = runApplication()
	} else {
		err = errors.New("usage: mecatl-runtime [driver]")
	}
	if err != nil {
		log.Printf("mecatl runtime: %v", err)
		os.Exit(1)
	}
}

func parseRedisBinding(raw string) (redisBinding, error) {
	var services map[string][]serviceBinding
	if err := json.Unmarshal([]byte(raw), &services); err != nil {
		return redisBinding{}, fmt.Errorf("parse VCAP_SERVICES: %w", err)
	}
	var selected *redisBinding
	for label, bindings := range services {
		for _, binding := range bindings {
			isRedis := strings.EqualFold(label, "redis") || strings.EqualFold(binding.Label, "redis") || binding.Name == "opencode-agent-redis"
			for _, tag := range binding.Tags {
				isRedis = isRedis || strings.EqualFold(tag, "redis")
			}
			if !isRedis {
				continue
			}
			if selected != nil {
				return redisBinding{}, errors.New("multiple Redis bindings; bind one Redis service")
			}
			parsed, err := parseRedisCredentials(binding.Credentials)
			if err != nil {
				return redisBinding{}, fmt.Errorf("Redis binding %q: %w", binding.Name, err)
			}
			selected = &parsed
		}
	}
	if selected == nil {
		return redisBinding{}, errors.New("bind one Redis service (expected opencode-agent-redis or a redis-labeled binding)")
	}
	return *selected, nil
}

func parseRedisCredentials(credentials map[string]any) (redisBinding, error) {
	address := credentialString(credentials, "address")
	if address == "" {
		address = credentialString(credentials, "uri")
	}
	if address == "" {
		address = credentialString(credentials, "url")
	}
	if address == "" {
		host := credentialString(credentials, "host")
		if host == "" {
			host = credentialString(credentials, "hostname")
		}
		port := credentialString(credentials, "port")
		if host == "" || port == "" {
			return redisBinding{}, errors.New("credentials must provide address/uri or host and port")
		}
		address = net.JoinHostPort(host, port)
	}
	username, password := credentialString(credentials, "username"), credentialString(credentials, "password")
	useTLS := strings.EqualFold(credentialString(credentials, "tls"), "true") || credentialString(credentials, "tls_enabled") == "true"
	if strings.Contains(address, "://") {
		parsed, err := url.Parse(address)
		if err != nil {
			return redisBinding{}, errors.New("invalid Redis URI")
		}
		if parsed.Scheme != "redis" && parsed.Scheme != "rediss" {
			return redisBinding{}, errors.New("URI scheme must be redis or rediss")
		}
		if parsed.Hostname() == "" {
			return redisBinding{}, errors.New("URI must contain a Redis host")
		}
		port := parsed.Port()
		if port == "" {
			if parsed.Scheme == "rediss" {
				port = "6380"
			} else {
				port = "6379"
			}
		}
		address = net.JoinHostPort(parsed.Hostname(), port)
		if parsed.User != nil {
			if username == "" {
				username = parsed.User.Username()
			}
			if value, ok := parsed.User.Password(); ok && password == "" {
				password = value
			}
		}
		if parsed.Scheme == "rediss" {
			useTLS = true
		}
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return redisBinding{}, errors.New("Redis address must include a host and port")
	}
	return redisBinding{
		Address: address, Username: username, Password: password,
		TLS: useTLS,
		CACert: credentialString(credentials, "ca_cert"),
	}, nil
}

func credentialString(credentials map[string]any, key string) string {
	value := credentials[key]
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	case float64:
		return fmt.Sprintf("%.0f", value)
	case bool:
		return fmt.Sprintf("%t", value)
	default:
		return ""
	}
}

func runDriver() error {
	binding, err := parseRedisBinding(os.Getenv("VCAP_SERVICES"))
	if err != nil {
		return err
	}
	if host, port, err := net.SplitHostPort(binding.Address); err == nil && strings.HasSuffix(host, ".apps.internal") {
		host = strings.TrimSuffix(host, ".")
		host = strings.TrimSuffix(host, ".apps.internal")
		host = strings.TrimSuffix(host, ".apps.internal") + ".apps.internal"
		resolved, err := exec.Command("getent", "ahostsv4", host).Output()
		if err != nil {
			return fmt.Errorf("resolve bound Redis service host: %w", err)
		}
		fields := strings.Fields(string(resolved))
		if len(fields) == 0 || net.ParseIP(fields[0]) == nil {
			return errors.New("bound Redis service host resolved to no IPv4 address")
		}
		binding.Address = net.JoinHostPort(fields[0], port)
	}

	tempDir, err := os.MkdirTemp("", "mecatl-redis-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	writeSecret := func(name, value string) (string, error) {
		if value == "" {
			return "", nil
		}
		path := filepath.Join(tempDir, name)
		return path, os.WriteFile(path, []byte(value), 0600)
	}
	usernameFile, err := writeSecret("username", binding.Username)
	if err != nil {
		return fmt.Errorf("write Redis username file: %w", err)
	}
	passwordFile, err := writeSecret("password", binding.Password)
	if err != nil {
		return fmt.Errorf("write Redis password file: %w", err)
	}
	caFile, err := writeSecret("redis-ca.pem", binding.CACert)
	if err != nil {
		return fmt.Errorf("write Redis CA file: %w", err)
	}
	store, err := redisstore.NewWithConfig(redisstore.Config{
		Addr: binding.Address, UsernameFile: usernameFile, PasswordFile: passwordFile,
		CAFile: caFile, TLS: binding.TLS, AllowPlaintext: !binding.TLS && binding.Password == "" && binding.Username == "",
	})
	if err != nil {
		return fmt.Errorf("connect to bound Redis: %w", err)
	}
	defer store.Close()
	addr := "127.0.0.1:19100"
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for loopback Mecatl storage driver: %w", err)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(grpcdriver.MaxSnapshotBytes),
		grpc.MaxSendMsgSize(grpcdriver.MaxSnapshotBytes),
	)
	driverv1.RegisterSessionStoreServiceServer(server, grpcdriver.NewSessionStoreServer(store))
	driverv1.RegisterEventLogServiceServer(server, grpcdriver.NewEventLogServer(store))
	scheduleStore := store.ScheduleStore()
	driverv1.RegisterScheduleStoreServiceServer(server, grpcdriver.NewScheduleStoreServer(scheduleStore))
	reArmer, ok := scheduleStore.(interface {
		ReArmOneShot(context.Context, string, time.Time) error
	})
	if !ok {
		return errors.New("Redis schedule store does not expose one-shot re-arming")
	}
	driverv1.RegisterScheduleOneShotReArmerServiceServer(server, grpcdriver.NewScheduleOneShotReArmerServer(reArmer))
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	log.Printf("Redis-backed Mecatl storage driver listening on %s", addr)
	return server.Serve(listener)
}

func runApplication() error {
	runtimeKind := strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_RUNTIME")))
	if runtimeKind == "" || runtimeKind == "opencode" {
		return errors.New("mecatl-runtime invoked while AGENT_RUNTIME is not mecatl")
	}
	if runtimeKind != "mecatl" {
		return fmt.Errorf("unsupported AGENT_RUNTIME %q (choose opencode or mecatl)", runtimeKind)
	}
	if os.Getenv("PORT") == "" {
		return errors.New("PORT must be set by Cloud Foundry")
	}
	publicURL := strings.TrimSpace(os.Getenv("STUDIO_PUBLIC_URL"))
	if publicURL == "" {
		var app application
		if err := json.Unmarshal([]byte(os.Getenv("VCAP_APPLICATION")), &app); err != nil || len(app.URIs) == 0 {
			return errors.New("set STUDIO_PUBLIC_URL or map a route to the CF app")
		}
		scheme := envOr("STUDIO_PUBLIC_SCHEME", "https")
		publicURL = scheme + "://" + app.URIs[0]
	}
	model, err := parseModelBinding(os.Getenv("VCAP_SERVICES"))
	if err != nil {
		return err
	}
	if model.Provider.Options.BaseURL == "" || model.Model == "" {
		return errors.New("DGX Spark binding must provide provider.options.baseURL and model")
	}
	modelID := modelSelector(model.Model)
	if strings.TrimSpace(modelID) == "" {
		return errors.New("DGX Spark binding selected an empty model")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dependencyDir := strings.TrimSpace(os.Getenv("AGENT_DEPS_DIR"))
	if dependencyDir == "" {
		return errors.New("AGENT_DEPS_DIR must identify the staged agent dependency directory")
	}
	apiToken := strings.TrimSpace(os.Getenv("MECATL_API_TOKEN"))
	if apiToken == "" {
		return errors.New("MECATL_API_TOKEN must be set to enable the protected gRPC route")
	}
	studioDir := filepath.Join(filepath.Dir(dependencyDir), "mecatl-studio")
	baseEnv := selectedEnvironment(os.Environ(), "HOME", "PATH", "TMPDIR", "LANG", "LC_ALL", "USER", "LOGNAME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME")

	children := make([]*childProcess, 0, 3)
	start := func(name string, command *exec.Cmd) error {
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Start(); err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		child := &childProcess{command: command, done: make(chan struct{})}
		children = append(children, child)
		go func() {
			child.err = command.Wait()
			close(child.done)
		}()
		log.Printf("started %s", name)
		return nil
	}
	if err := start("Redis driver", exec.Command(self, "driver")); err != nil {
		return err
	}
	if err := waitForTCP(ctx, "127.0.0.1:19100", children); err != nil {
		stopChildren(children)
		return err
	}
	apiKey := envOr("OPENCODE_API_KEY", envOr("DGX_SPARK_API_KEY", model.Provider.Options.APIKey))
	if apiKey == "" {
		apiKey = "cf-dgx-spark-no-key"
	}
	mecatedEnv := append(append([]string{}, baseEnv...),
		"OPENCODE_API_KEY="+apiKey,
		"MECATL_AUTH_TOKEN="+apiToken,
	)
	mecatedArgs := mecatedArguments(os.Getenv("HOME"), modelID, model.Provider.Options.BaseURL)
	mecated := exec.Command(filepath.Join(dependencyDir, "mecated"), mecatedArgs...)
	mecated.Env = mecatedEnv
	if err := start("mecated", mecated); err != nil {
		stopChildren(children)
		return err
	}
	if err := waitForTCP(ctx, "127.0.0.1:19101", children); err != nil {
		stopChildren(children)
		return err
	}
	studioEnv := append(append([]string{}, baseEnv...),
		"MECATL_BASE_URL=http://127.0.0.1:19101", "MECATL_AUTH_TOKEN="+apiToken, "STUDIO_IMAGE=1", "STUDIO_ALLOW_UNAUTHENTICATED=1",
		"STUDIO_PUBLIC_URL="+publicURL,
		"STUDIO_HOST=0.0.0.0", "STUDIO_PORT="+os.Getenv("PORT"),
		"STUDIO_WEB_DIST="+filepath.Join(studioDir, "app", "web", "dist"),
	)
	studio := exec.Command(filepath.Join(studioDir, "usr", "bin", "node"), filepath.Join(studioDir, "app", "dist", "index.js"))
	studio.Dir = filepath.Join(studioDir, "app")
	studio.Env = studioEnv
	if err := start("Mecatl Studio", studio); err != nil {
		stopChildren(children)
		return err
	}

	if err := waitForHTTP(ctx, "http://127.0.0.1:"+os.Getenv("PORT")+"/api/health", children); err != nil {
		stopChildren(children)
		return err
	}
	log.Printf("Mecatl Studio is healthy on CF port %s", os.Getenv("PORT"))
	result := make(chan error, len(children))
	for _, child := range children {
		go func(process *childProcess) {
			<-process.done
			result <- process.err
		}(child)
	}
	select {
	case <-ctx.Done():
		stopChildren(children)
		return nil
	case childErr := <-result:
		stopChildren(children)
		if childErr == nil {
			return errors.New("a Mecatl runtime process exited unexpectedly")
		}
		return fmt.Errorf("a Mecatl runtime process exited: %w", childErr)
	}
}

func mecatedArguments(workspace, modelID, baseURL string) []string {
	return []string{
		// Gorouter receives the route's HTTP/2 mode and forwards prior-knowledge
		// h2c to this listener. Client TLS terminates at the router.
		"serve", "--grpc-addr=0.0.0.0:19101", "--http-addr=", "--metrics-addr=",
		"--workspace=" + workspace, "--default-provider=opencode", "--model=" + modelID,
		"--opencode-base-url=" + baseURL,
		"--session-store-url=127.0.0.1:19100", "--event-log-url=127.0.0.1:19100",
		"--schedule-store-url=127.0.0.1:19100", "--product-metrics=false",
		"--no-user-model", "--websearch=off", "--toolhive=false", "--enable-parallel=false",
		"--enable-teams=false", "--flight-recorder=false",
	}
}

func parseModelBinding(raw string) (modelBinding, error) {
	var services map[string][]struct {
		Name        string          `json:"name"`
		Credentials json.RawMessage `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(raw), &services); err != nil {
		return modelBinding{}, fmt.Errorf("parse model service binding: %w", err)
	}
	var selected *modelBinding
	for _, bindings := range services {
		for _, binding := range bindings {
			if binding.Name != "dgx-spark-model" {
				continue
			}
			if selected != nil {
				return modelBinding{}, errors.New("multiple dgx-spark-model bindings")
			}
			var model modelBinding
			if err := json.Unmarshal(binding.Credentials, &model); err != nil {
				return modelBinding{}, fmt.Errorf("parse DGX Spark binding: %w", err)
			}
			selected = &model
		}
	}
	if selected == nil {
		return modelBinding{}, errors.New("bind dgx-spark-model to select a Mecatl model")
	}
	return *selected, nil
}

func modelSelector(model string) string {
	provider, suffix, ok := strings.Cut(model, "/")
	if ok && (provider == "dgxSpark" || provider == "openai") {
		return suffix
	}
	return model
}

func selectedEnvironment(env []string, selected ...string) []string {
	allowed := make(map[string]bool, len(selected))
	for _, key := range selected {
		allowed[key] = true
	}
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			result = append(result, entry)
		}
	}
	return result
}

func waitForHTTP(ctx context.Context, endpoint string, children []*childProcess) error {
	client := &http.Client{Timeout: time.Second}
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		response, err := client.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		for _, child := range children {
			select {
			case <-child.done:
				return fmt.Errorf("runtime process exited before Studio became healthy: %w", child.err)
			default:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("Mecatl Studio did not become healthy within 90 seconds")
		case <-tick.C:
		}
	}
}

func waitForTCP(ctx context.Context, address string, children []*childProcess) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		for _, child := range children {
			select {
			case <-child.done:
				return fmt.Errorf("runtime process exited before Redis driver became ready: %w", child.err)
			default:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("Redis-backed Mecatl driver did not become ready within 30 seconds")
		case <-tick.C:
		}
	}
}

func stopChildren(children []*childProcess) {
	for _, child := range children {
		if child.command.Process != nil {
			_ = child.command.Process.Signal(syscall.SIGTERM)
		}
	}
	for _, child := range children {
		if child.command.Process != nil {
			select {
			case <-child.done:
			case <-time.After(10 * time.Second):
				_ = child.command.Process.Kill()
				<-child.done
			}
		}
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
