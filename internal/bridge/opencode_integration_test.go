package bridge

import (
	"testing"

	"opencode-cpa-bridge/internal/config"
	"opencode-cpa-bridge/internal/registry"
)

func TestExecutionURLPrefersModelBase(t *testing.T) {
	cfg := config.Config{BaseURL: "https://opencode.ai/inference/go/v1"}
	rec := registry.ModelRecord{
		EndpointPath:    "/v1/messages",
		UpstreamBaseURL: "https://opencode.ai/inference/go/anthropic/v1",
	}
	if got, want := executionURL(cfg, rec), "https://opencode.ai/inference/go/anthropic/v1/messages"; got != want {
		t.Errorf("model base URL = %q, want %q", got, want)
	}
	rec.UpstreamBaseURL = ""
	if got, want := executionURL(cfg, rec), "https://opencode.ai/inference/go/v1/messages"; got != want {
		t.Errorf("configured base URL = %q, want %q", got, want)
	}
}
