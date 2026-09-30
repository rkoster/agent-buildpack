package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, services, want, errorPart string
	}{
		{"no services", "", "", ""},
		{"other binding", `{"user-provided":[{"name":"model","credentials":{"uri":"postgres://ignored"}}]}`, "", ""},
		{"nested model credentials", `{"user-provided":[{"name":"model","credentials":{"provider":{"id":"spark"}}}],"PostgreSQL":[{"name":"db","credentials":{"uri":"postgres://u:p@db/postgres","port":5432}}]}`, "postgres://u:p@db/postgres", ""},
		{"broker binding", `{"PostgreSQL":[{"name":"agent-db","tags":["database","postgresql"],"credentials":{"uri":"postgres://user:pass@db.apps.internal:5432/postgres?sslmode=disable"}}]}`, "postgres://user:pass@db.apps.internal:5432/postgres?sslmode=disable", ""},
		{"tagged binding", `{"user-provided":[{"name":"db","tags":["postgresql"],"credentials":{"uri":"postgresql://u:p@db/postgres"}}]}`, "postgresql://u:p@db/postgres", ""},
		{"missing uri", `{"postgresql":[{"name":"db","credentials":{}}]}`, "", "no uri"},
		{"multiple", `{"postgresql":[{"credentials":{"uri":"postgres://a/db"}},{"credentials":{"uri":"postgres://b/db"}}]}`, "", "multiple PostgreSQL"},
		{"invalid json", "{", "", "parse VCAP_SERVICES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := databaseURL(tc.services)
			if got != tc.want || (tc.errorPart == "" && err != nil) || (tc.errorPart != "" && (err == nil || !strings.Contains(err.Error(), tc.errorPart))) {
				t.Fatalf("databaseURL() = %q, %v; want %q, error containing %q", got, err, tc.want, tc.errorPart)
			}
		})
	}
}

func TestConfigurePlugin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(path, []byte(`{"tools":{"bash":false},"provider":{"model":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := configurePlugin(path); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	plugins := config["plugin"].([]any)
	if len(plugins) != 1 || plugins[0] != "opencode-database-plugin@1.0.12" || config["provider"] == nil || config["tools"].(map[string]any)["bash"] != false {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestResolvedPluginURLWithoutInternalDNS(t *testing.T) {
	uri := "postgres://user:password@localhost:5432/database?sslmode=disable"
	got, err := resolvedPluginURL(context.Background(), uri)
	if err != nil || got != uri {
		t.Fatalf("resolvedPluginURL() = %q, %v", got, err)
	}
}
