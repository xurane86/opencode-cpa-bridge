package registry

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The inference list is OpenAI's list shape plus models.dev metadata and an
// `api` object declaring the protocol and base URL for each model.
const openCodeCatalog = `{
  "object": "list",
  "data": [
    {
      "id": "kimi-k3",
      "name": "Kimi K3",
      "limit": { "context": 1000000, "output": 128000 },
      "modalities": { "input": ["text", "image/png"], "output": ["text"] },
      "capabilities": {
        "tools": { "supported": true },
        "reasoning": { "supported": true, "effort": ["low", "medium", "high"] }
      },
      "api": { "messages": { "base_url": "https://opencode.ai/inference/go/anthropic/v1" } }
    },
    {
      "id": "glm-5.3",
      "name": "GLM 5.3",
      "api": { "chat-completions": { "base_url": "https://opencode.ai/inference/go/openai/v1" } }
    },
    {
      "id": "gpt-5.6",
      "name": "GPT 5.6",
      "api": { "responses": { "base_url": "https://opencode.ai/inference/go/openai/v1" } }
    },
    {
      "id": "multi-protocol",
      "api": {
        "responses": { "base_url": "https://opencode.ai/inference/go/openai/v1" },
        "chat-completions": { "base_url": "https://opencode.ai/inference/go/openai/v1" },
        "messages": { "base_url": "https://opencode.ai/inference/go/anthropic/v1" }
      }
    },
    {
      "id": "image-only",
      "api": { "generate-content": { "base_url": "https://opencode.ai/inference/go/google/v1" } }
    },
    {
      "id": "unsafe-base",
      "api": { "messages": { "base_url": "https://user:secret@evil.test/v1" } }
    }
  ]
}`

func openCodeManager() *Manager {
	cfg := testCfg()
	cfg.BaseURL = "https://opencode.ai/inference/go/v1"
	cfg.CatalogURL = "https://opencode.ai/inference/go/v1/models"
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(openCodeCatalog)}}
	return New(cfg, fc)
}

func TestOpenCodeCatalogNormalization(t *testing.T) {
	m := openCodeManager()
	mustRefresh(t, m)
	models := m.Models()
	if len(models) != 4 {
		t.Fatalf("models = %d, want 4 routable (image-only and unsafe-base excluded): %+v", len(models), models)
	}

	kimi := findModel(t, models, "kimi-k3")
	if kimi.DisplayName != "Kimi K3" || kimi.ContextLimit != 1000000 || kimi.OutputLimit != 128000 {
		t.Errorf("kimi metadata = %+v", kimi)
	}
	if len(kimi.InputModes) != 2 || kimi.InputModes[0] != "text" || len(kimi.OutputModes) != 1 {
		t.Errorf("kimi modalities = %v / %v", kimi.InputModes, kimi.OutputModes)
	}
	if kimi.Thinking == nil || len(kimi.Thinking.Levels) != 3 || kimi.Thinking.Levels[0] != "low" {
		t.Errorf("kimi thinking = %+v", kimi.Thinking)
	}
	if kimi.Protocol != RouteMessages || kimi.EndpointPath != "/v1/messages" {
		t.Errorf("kimi route = %s %s", kimi.Protocol, kimi.EndpointPath)
	}
	if kimi.UpstreamBaseURL != "https://opencode.ai/inference/go/anthropic/v1" {
		t.Errorf("kimi base = %q", kimi.UpstreamBaseURL)
	}
	if got := JoinUpstreamURL(kimi.UpstreamBaseURL, kimi.EndpointPath); got != "https://opencode.ai/inference/go/anthropic/v1/messages" {
		t.Errorf("kimi URL = %q", got)
	}
}

func TestOpenCodeAPIRoutePriority(t *testing.T) {
	m := openCodeManager()
	mustRefresh(t, m)
	models := m.Models()
	cases := map[string]Route{
		"kimi-k3":        RouteMessages,
		"glm-5.3":        RouteChatCompletions,
		"gpt-5.6":        RouteResponses,
		"multi-protocol": RouteMessages, // messages wins when several are offered
	}
	for id, want := range cases {
		rec := findModel(t, models, id)
		if rec.Protocol != want {
			t.Errorf("%s route = %s, want %s", id, rec.Protocol, want)
		}
	}
}

func TestOpenCodeCatalogDiagnostics(t *testing.T) {
	m := openCodeManager()
	mustRefresh(t, m)
	unsupported := m.Unsupported()
	if len(unsupported) != 2 {
		t.Fatalf("unsupported = %+v, want 2", unsupported)
	}
	byID := map[string]string{}
	for _, u := range unsupported {
		byID[u.UpstreamID] = u.Reason
	}
	if reason := byID["image-only"]; reason == "" {
		t.Errorf("image-only not excluded with a reason: %+v", byID)
	}
	if reason := byID["unsafe-base"]; reason == "" {
		t.Errorf("unsafe-base not excluded with a reason: %+v", byID)
	}
}

func TestOpenCodeLegacyCatalogStillLoads(t *testing.T) {
	body := `{"data":[{"id":"legacy","display_name":"Legacy Model","context_length":8192,"max_output_tokens":512,"input_modalities":["text"]}]}`
	cfg := testCfg()
	cfg.BaseURL = "https://opencode.ai/inference/go/v1"
	m := New(cfg, &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body)}})
	mustRefresh(t, m)
	rec := findModel(t, m.Models(), "legacy")
	if rec.DisplayName != "Legacy Model" || rec.ContextLimit != 8192 || rec.OutputLimit != 512 {
		t.Errorf("legacy record = %+v", rec)
	}
	// Without an `api` object the model falls back to the configured base-url;
	// UpstreamBaseURL then records that effective value, not an empty string.
	if rec.Protocol != DefaultRoute || rec.UpstreamBaseURL != cfg.BaseURL {
		t.Errorf("legacy route/base = %s / %q, want %s / %q", rec.Protocol, rec.UpstreamBaseURL, DefaultRoute, cfg.BaseURL)
	}
}

func TestSafeUpstreamBase(t *testing.T) {
	cases := []struct {
		url       string
		allowHTTP bool
		want      bool
	}{
		{"https://opencode.ai/inference/go/anthropic/v1", false, true},
		{"http://127.0.0.1:8080/v1", true, true},
		{"http://opencode.ai/v1", false, false},
		{"https://user:pass@opencode.ai/v1", false, false},
		{"https://opencode.ai/v1?x=1", false, false},
		{"ftp://opencode.ai/v1", false, false},
		{"/relative/v1", false, false},
	}
	for _, tc := range cases {
		if got := safeUpstreamBase(tc.url, tc.allowHTTP); got != tc.want {
			t.Errorf("safeUpstreamBase(%q, %t) = %t, want %t", tc.url, tc.allowHTTP, got, tc.want)
		}
	}
}
