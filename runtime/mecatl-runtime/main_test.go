package main

import "testing"

func TestModelSelector(t *testing.T) {
	for _, test := range []struct {
		model string
		want  string
	}{
		{model: "dgxSpark/unsloth/Qwen3.6", want: "unsloth/Qwen3.6"},
		{model: "openai/gpt-5", want: "gpt-5"},
		{model: "unsloth-studio/unsloth/Qwen3.6", want: "unsloth-studio/unsloth/Qwen3.6"},
		{model: "model-without-provider", want: "model-without-provider"},
	} {
		t.Run(test.model, func(t *testing.T) {
			if got := modelSelector(test.model); got != test.want {
				t.Fatalf("modelSelector(%q) = %q, want %q", test.model, got, test.want)
			}
		})
	}
}

func TestParseRedisCredentials(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string]any
		want        redisBinding
		wantErr     bool
	}{
		{
			name:        "host and numeric port",
			credentials: map[string]any{"host": "redis.apps.internal", "port": float64(6379)},
			want:        redisBinding{Address: "redis.apps.internal:6379"},
		},
		{
			name:        "redis URI with credentials",
			credentials: map[string]any{"uri": "redis://demo:secret@redis.internal:6379/0"},
			want:        redisBinding{Address: "redis.internal:6379", Username: "demo", Password: "secret"},
		},
		{
			name:        "TLS URI",
			credentials: map[string]any{"uri": "rediss://redis.internal:6380"},
			want:        redisBinding{Address: "redis.internal:6380", TLS: true},
		},
		{
			name:        "invalid scheme",
			credentials: map[string]any{"uri": "http://redis.internal:6379"},
			wantErr:     true,
		},
		{
			name:        "missing port",
			credentials: map[string]any{"host": "redis.internal"},
			wantErr:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRedisCredentials(test.credentials)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseRedisCredentials() error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("parseRedisCredentials() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseRedisBindingRequiresExactlyOneRedis(t *testing.T) {
	for _, test := range []struct {
		name    string
		services string
		wantErr bool
	}{
		{
			name:     "missing binding",
			services: `{"user-provided":[{"name":"model","credentials":{}}]}`,
			wantErr:  true,
		},
		{
			name:     "one named Redis binding",
			services: `{"user-provided":[{"name":"opencode-agent-redis","credentials":{"host":"redis.apps.internal","port":6379}}]}`,
		},
		{
			name:     "multiple Redis bindings",
			services: `{"redis":[{"name":"one","credentials":{"address":"redis.apps.internal:6379"}},{"name":"two","credentials":{"address":"redis.apps.internal:6379"}}]}`,
			wantErr:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseRedisBinding(test.services)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseRedisBinding() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
