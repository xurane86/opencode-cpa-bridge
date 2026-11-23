package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-cpa-bridge/internal/config"
)

func TestManagementRegistration(t *testing.T) {
	m := NewManager(nil)
	var got struct {
		Routes    []struct{ Method, Path string }            `json:"routes"`
		Resources []struct{ Path, Menu, Description string } `json:"resources"`
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodManagementRegister, []byte(`{}`)), &got)
	foundQuota := false
	for _, r := range got.Routes {
		if r.Path == "/plugins/"+pluginName+"/quota-usage" {
			foundQuota = true
		}
	}
	if !foundQuota {
		t.Fatalf("routes = %+v", got.Routes)
	}
	if len(got.Resources) != 1 || got.Resources[0].Path != "/console" || got.Resources[0].Menu != "OpenCode Go" {
		t.Fatalf("resources = %+v", got.Resources)
	}
	var registration registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &registration)
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("registration did not advertise management_api")
	}
}

func TestQuotaListDoesNotCallHost(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{APIKeys: []config.APIKey{{Value: "quota-key-a"}, {Value: "quota-key-b"}}}
	var got quotaList
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 2 || got.Cards[0].Label != "OpenCode Go credential "+mustHashPrefix("quota-key-a") || got.Cards[1].Label != "OpenCode Go credential "+mustHashPrefix("quota-key-b") {
		t.Fatalf("cards = %+v", got.Cards)
	}
	if strings.Contains(string(resp.Body), "quota-key-") || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("list leaked key or called upstream: %s calls=%v", resp.Body, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
	if got.Cards[0].Email != "" || got.Cards[1].Email != "" {
		t.Fatalf("unsynced list cards must not carry an email: %+v", got.Cards)
	}
}

func TestQuotaListSurfacesStoredEmail(t *testing.T) {
	const key = "quota-list-email-secret"
	id, _ := quotaIdentity(key)
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			t.Fatal("list must not call upstream")
		}
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
				ID: id, Name: id + ".json", Email: "dev@example.test", Source: "file",
			}}}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{APIKeys: []config.APIKey{{Value: key}}}
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got quotaList
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 1 || got.Cards[0].KeyID != id || got.Cards[0].Email != "dev@example.test" || got.Cards[0].Label != "dev@example.test" {
		t.Fatalf("cards = %+v", got.Cards)
	}
	if got.Cards[0].Usage != nil {
		t.Fatal("list must not fetch usage for an already-synced account")
	}
}

// TestQuotaRefresh pins the official OpenCode Go usage shape: one request to
// /v1/usage carrying rolling/weekly/monthly meters, and no account lookups.
func TestQuotaRefresh(t *testing.T) {
	const key = "quota-refresh-secret"
	seen := map[string]int{}
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Method != http.MethodGet || wire.Headers.Get("Authorization") != "Bearer "+key || wire.Headers.Get("Accept") != "application/json" {
			t.Fatalf("bad quota request: %s", payload)
		}
		seen[wire.URL]++
		if wire.URL != "https://quota.test/v1/usage" {
			t.Fatalf("unexpected quota URL %q", wire.URL)
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
			`{"usage":{"rolling":{"status":"ok","percent":30,"resetsAt":"2026-11-21T15:00:00.000Z"},` +
				`"weekly":{"status":"rate-limited","percent":100,"resetsAt":"2026-11-23T00:00:00.000Z"},` +
				`"monthly":{"status":"ok","percent":50,"resetsAt":"2026-12-01T00:00:00.000Z"}}}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}
	id, _ := quotaIdentity(key)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got quotaCard
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "" || got.Usage == nil {
		t.Fatalf("card = %+v", got)
	}
	if got.Usage.Plan != "OpenCode Go" {
		t.Errorf("plan = %q", got.Usage.Plan)
	}
	if got.Usage.Rolling.Percent != 30 || got.Usage.Rolling.Status != "ok" || got.Usage.Rolling.ResetsAt != "2026-11-21T15:00:00Z" {
		t.Errorf("rolling = %+v", got.Usage.Rolling)
	}
	if got.Usage.Weekly.Percent != 100 || got.Usage.Weekly.Status != "rate-limited" || got.Usage.Weekly.ResetsAt != "2026-11-23T00:00:00Z" {
		t.Errorf("weekly = %+v", got.Usage.Weekly)
	}
	if got.Usage.Monthly.Percent != 50 || got.Usage.Monthly.Status != "ok" {
		t.Errorf("monthly = %+v", got.Usage.Monthly)
	}
	if got.Usage.RefreshedAt == "" {
		t.Error("refreshed_at missing")
	}
	if len(seen) != 1 || seen["https://quota.test/v1/usage"] != 1 {
		t.Errorf("endpoints called = %v, want exactly one usage call", seen)
	}
	if strings.Contains(string(resp.Body), key) {
		t.Fatal("quota response leaked the api key")
	}
}

func TestUsageAPIBase(t *testing.T) {
	cases := map[string]string{
		"https://opencode.ai/inference/go/v1":         "https://opencode.ai/inference/go",
		"https://opencode.ai/inference/go/v1/":        "https://opencode.ai/inference/go",
		"https://staging.example.com/inference/go/v1": "https://staging.example.com/inference/go",
		"https://gateway.test/code/v1":                "https://gateway.test/code",
		"https://gateway.test":                        "https://gateway.test",
	}
	for in, want := range cases {
		got, err := UsageAPIBase(in)
		if err != nil || got != want {
			t.Errorf("UsageAPIBase(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "not a url", "/v1", "ftp://opencode.ai/inference/go/v1"} {
		if got, err := UsageAPIBase(bad); err == nil {
			t.Errorf("UsageAPIBase(%q) = %q, want an error", bad, got)
		}
	}
}

func TestWindowFromMeter(t *testing.T) {
	cases := []struct {
		name string
		in   accountUsageMeter
		want quotaWindow
	}{
		{"normal", accountUsageMeter{Status: "ok", Percent: 42, ResetsAt: "2026-11-21T15:00:00.000Z"}, quotaWindow{Status: "ok", Percent: 42, ResetsAt: "2026-11-21T15:00:00Z"}},
		{"rate limited", accountUsageMeter{Status: "rate-limited", Percent: 100}, quotaWindow{Status: "rate-limited", Percent: 100}},
		{"default status", accountUsageMeter{Percent: 10}, quotaWindow{Status: "ok", Percent: 10}},
		{"clamp high", accountUsageMeter{Status: "ok", Percent: 140}, quotaWindow{Status: "ok", Percent: 100}},
		{"clamp low", accountUsageMeter{Status: "ok", Percent: -5}, quotaWindow{Status: "ok", Percent: 0}},
		{"unparseable reset kept verbatim", accountUsageMeter{Status: "ok", Percent: 1, ResetsAt: "soon"}, quotaWindow{Status: "ok", Percent: 1, ResetsAt: "soon"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := windowFromMeter(tc.in)
			if got != tc.want {
				t.Errorf("windowFromMeter(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestQuotaUnknownKeyAndResource(t *testing.T) {
	const key = "quota-known-secret"
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{APIKeys: []config.APIKey{{Value: key}}}
	unknown := "opencodego-key-unknown"
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + unknown + `"}`)})
	if err != nil || resp.StatusCode != http.StatusNotFound || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), unknown) || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("unknown response = %+v err=%v calls=%v", resp, err, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
	resource, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/console"})
	if err != nil || (resource.StatusCode != 0 && resource.StatusCode != http.StatusOK) || resource.Headers.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(string(resource.Body), "OpenCode Go") || strings.Contains(string(resource.Body), key) {
		t.Fatalf("resource response = %+v err=%v", resource, err)
	}
}

// Quota failures surface on the card with a redacted reason: one unreachable
// account must not blank the page, and no key material may appear in the
// response.
func TestQuotaErrorsAreRedacted(t *testing.T) {
	const key = "quota-error-secret"
	id, _ := quotaIdentity(key)
	for name, responder := range map[string]func(string, []byte) ([]byte, error){
		"bridge": func(string, []byte) ([]byte, error) { return nil, context.Canceled },
		"status": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(key + " upstream body")}), nil
		},
		"json": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(key + " not json")}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{responder: responder}
			m := NewManager(NewHostBridge(f.call))
			m.cfg = config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}
			resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
			if err != nil || resp.StatusCode != 0 || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), "upstream body") {
				t.Fatalf("response = %+v err=%v", resp, err)
			}
			var got quotaCard
			if err := json.Unmarshal(resp.Body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Error == "" || got.Usage != nil {
				t.Fatalf("card = %+v, want an error and no usage", got)
			}
			if got.Label != "OpenCode Go credential "+mustHashPrefix(key) {
				t.Errorf("label = %q, want the credential fallback label", got.Label)
			}
		})
	}
}

func mustHashPrefix(key string) string {
	_, label := quotaIdentity(key)
	return strings.TrimPrefix(label, "OpenCode Go credential ")
}
