package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeBoundModelProviderPreservesBuildpackToolSettings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("models request path=%s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "unsloth/model", "name": "DGX Runtime Model", "context_length": 131072, "top_provider": map[string]any{"context_length": 131072, "max_completion_tokens": 32768}},
			{"id": "second/model", "name": "Second Model", "context_length": 65536},
		}})
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	vcapServices := fmt.Sprintf(`{"user-provided":[{"name":"dgx-spark-model","credentials":{"model":"dgxSpark/unsloth/model","provider":{"id":"dgxSpark","name":"DGX Spark","npm":"@ai-sdk/openai-compatible","options":{"baseURL":%q},"models":{"stale-model":{"name":"Stale Model"}}}}}]}`, server.URL+"/v1")
	t.Setenv("VCAP_SERVICES", vcapServices)

	configDir := filepath.Join(home, ".opencode")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, []byte(`{"tools":{"bash":false,"read":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mergeBoundModelProvider(); err != nil {
		t.Fatal(err)
	}

	var config map[string]any
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["model"] != "dgxSpark/unsloth/model" {
		t.Fatalf("model=%v", config["model"])
	}
	tools := config["tools"].(map[string]any)
	if tools["bash"] != false || tools["read"] != false {
		t.Fatalf("buildpack tool settings overwritten: %v", tools)
	}
	provider := config["provider"].(map[string]any)["dgxSpark"].(map[string]any)
	if provider["name"] != "DGX Spark" {
		t.Fatalf("provider config=%v", provider)
	}
	models := provider["models"].(map[string]any)
	if len(models) != 2 || models["stale-model"] != nil {
		t.Fatalf("injected models should match the DGX /v1/models catalog: %v", models)
	}
	model := models["unsloth/model"].(map[string]any)
	limits := model["limit"].(map[string]any)
	if model["name"] != "DGX Runtime Model" || limits["context"] != float64(131072) || limits["output"] != float64(32768) {
		t.Fatalf("runtime model metadata=%v", model)
	}
	if second := models["second/model"].(map[string]any)["limit"].(map[string]any); second["context"] != float64(65536) || second["output"] != float64(65536) {
		t.Fatalf("fallback output limit=%v", second)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions=%o want 600", info.Mode().Perm())
	}
}
