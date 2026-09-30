package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schema embed.FS

type binding struct {
	Name        string         `json:"name"`
	Label       string         `json:"label"`
	Tags        []string       `json:"tags"`
	Credentials map[string]any `json:"credentials"`
}

func databaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	var services map[string][]binding
	if err := json.Unmarshal([]byte(raw), &services); err != nil {
		return "", fmt.Errorf("parse VCAP_SERVICES: %w", err)
	}
	var selected string
	for label, bindings := range services {
		for _, b := range bindings {
			postgres := strings.EqualFold(label, "postgresql") || strings.EqualFold(b.Label, "postgresql")
			for _, tag := range b.Tags {
				postgres = postgres || strings.EqualFold(tag, "postgresql")
			}
			if !postgres {
				continue
			}
			uri, _ := b.Credentials["uri"].(string)
			if uri == "" {
				return "", fmt.Errorf("PostgreSQL binding %q has no uri", b.Name)
			}
			if !strings.HasPrefix(uri, "postgres://") && !strings.HasPrefix(uri, "postgresql://") {
				return "", fmt.Errorf("PostgreSQL binding %q has an invalid URI scheme", b.Name)
			}
			if selected != "" {
				return "", errors.New("multiple PostgreSQL bindings; bind only one for OpenCode")
			}
			selected = uri
		}
	}
	return selected, nil
}

func run() error {
	uri, err := databaseURL(os.Getenv("VCAP_SERVICES"))
	if err != nil {
		return err
	}
	if uri == "" {
		return nil
	}
	config, err := pgx.ParseConfig(uri)
	if err != nil {
		return fmt.Errorf("invalid PostgreSQL binding URI: %w", err)
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	// CF's libc resolver handles apps.internal service discovery. pgx performs
	// its own hostname lookup before dialing, so override that lookup directly.
	config.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		lookupHost := strings.TrimSuffix(host, ".")
		if strings.HasSuffix(lookupHost, ".apps.internal.apps.internal") {
			lookupHost = strings.TrimSuffix(lookupHost, ".apps.internal")
		}
		if strings.HasSuffix(lookupHost, ".apps.internal") {
			output, err := exec.CommandContext(ctx, "getent", "ahostsv4", lookupHost).Output()
			if err != nil {
				return nil, fmt.Errorf("resolve bound PostgreSQL host %q via getent: %w", lookupHost, err)
			}
			fields := strings.Fields(string(output))
			if len(fields) == 0 || net.ParseIP(fields[0]) == nil {
				return nil, errors.New("getent returned no IP for bound PostgreSQL host")
			}
			return []string{fields[0]}, nil
		}
		return nil, fmt.Errorf("unexpected PostgreSQL host %q", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("connect to bound PostgreSQL: %w", err)
	}
	defer conn.Close(context.Background())
	sql, err := schema.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("initialize OpenCode database schema: %w", err)
	}
	if err := configurePlugin(filepath.Join(os.Getenv("HOME"), ".opencode", "opencode.json")); err != nil {
		return err
	}
	// The startup wrapper captures stdout; do not print credentials to logs.
	pluginURL, err := resolvedPluginURL(ctx, uri)
	if err != nil {
		return err
	}
	fmt.Print(pluginURL)
	return nil
}

func resolvedPluginURL(ctx context.Context, uri string) (string, error) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("invalid PostgreSQL binding URI")
	}
	if !strings.HasSuffix(parsed.Hostname(), ".apps.internal") {
		return uri, nil
	}
	host := parsed.Hostname()
	if strings.HasSuffix(host, ".apps.internal.apps.internal") {
		host = strings.TrimSuffix(host, ".apps.internal")
	}
	output, err := exec.CommandContext(ctx, "getent", "ahostsv4", host).Output()
	if err != nil {
		return "", fmt.Errorf("resolve bound PostgreSQL host %q for plugin: %w", host, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 || net.ParseIP(fields[0]) == nil {
		return "", errors.New("getent returned no IP for bound PostgreSQL host")
	}
	parsed.Host = fields[0]
	if parsed.Port() != "" {
		parsed.Host = net.JoinHostPort(fields[0], parsed.Port())
	}
	return parsed.String(), nil
}

const pluginVersion = "opencode-database-plugin@1.0.12"

func configurePlugin(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read OpenCode config: %w", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse OpenCode config: %w", err)
	}
	plugins, _ := config["plugin"].([]any)
	for _, plugin := range plugins {
		if plugin == pluginVersion {
			return nil
		}
	}
	config["plugin"] = append(plugins, pluginVersion)
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		return fmt.Errorf("write OpenCode config: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
