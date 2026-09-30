package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CreateRequest struct {
	Image          *ImageSpec        `json:"image"`
	Timeout        *int64            `json:"timeout"`
	ResourceLimits map[string]string `json:"resourceLimits"`
	Env            map[string]string `json:"env"`
	Metadata       map[string]string `json:"metadata"`
	Entrypoint     []string          `json:"entrypoint"`
	NetworkPolicy  any               `json:"networkPolicy"`
	SnapshotID     string            `json:"snapshotId"`
	TemplateID     string            `json:"templateId"`
	Volumes        any               `json:"volumes"`
	Lifecycle      any               `json:"lifecycle"`
}

type ImageSpec struct {
	URI  string `json:"uri"`
	Auth *struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auth,omitempty"`
}

func (r CreateRequest) Validate() error {
	if r.Image == nil || strings.TrimSpace(r.Image.URI) == "" {
		if r.SnapshotID == "" && r.TemplateID == "" {
			return errors.New("image.uri is required; snapshots and templates are not supported")
		}
		return errors.New("this CF app backend supports only image-based sandboxes")
	}
	if r.SnapshotID != "" || r.TemplateID != "" {
		return errors.New("snapshotId and templateId are not supported")
	}
	if len(r.Entrypoint) == 0 {
		return errors.New("entrypoint is required")
	}
	if r.NetworkPolicy != nil {
		return errors.New("networkPolicy is not supported by the CF app runtime")
	}
	if r.Volumes != nil {
		return errors.New("volumes are not supported")
	}
	if r.Lifecycle != nil {
		return errors.New("lifecycle hooks are not supported")
	}
	if r.Timeout != nil && *r.Timeout < 60 {
		return errors.New("timeout must be at least 60 seconds")
	}
	if r.ResourceLimits["memory"] != "" {
		if parseMemoryMB(r.ResourceLimits["memory"], 0) < 1 {
			return errors.New("resourceLimits.memory must be a positive CF-compatible memory quantity")
		}
	}
	if len(r.ResourceLimits) > 0 {
		for key := range r.ResourceLimits {
			if key != "memory" {
				return fmt.Errorf("unsupported resourceLimits key %q", key)
			}
		}
	}
	for key := range r.Env {
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "VCAP_") || upper == "PORT" || strings.HasPrefix(upper, "CF_INSTANCE_") {
			return fmt.Errorf("environment variable %q is reserved by Cloud Foundry", key)
		}
	}
	metadataBytes, _ := json.Marshal(r.Metadata)
	if len(metadataBytes) > 4096 {
		return errors.New("metadata exceeds the 4096-byte POC limit")
	}
	return nil
}

type Sandbox struct {
	ID         string            `json:"id"`
	Image      *ImageSpec        `json:"image,omitempty"`
	Status     SandboxStatus     `json:"status"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Entrypoint []string          `json:"entrypoint"`
	ExpiresAt  *time.Time        `json:"expiresAt,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
}

type SandboxStatus struct {
	State            string    `json:"state"`
	Reason           string    `json:"reason,omitempty"`
	Message          string    `json:"message,omitempty"`
	LastTransitionAt time.Time `json:"lastTransitionAt,omitempty"`
}

type Endpoint struct {
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

func (s Sandbox) CreateResponse() map[string]any {
	return map[string]any{
		"id": s.ID, "status": s.Status, "metadata": s.Metadata,
		"createdAt": s.CreatedAt, "entrypoint": s.Entrypoint,
		"expiresAt": s.ExpiresAt,
	}
}

type BackendError struct {
	Status  int
	Code    string
	Message string
}

func (e *BackendError) Error() string { return e.Message }
