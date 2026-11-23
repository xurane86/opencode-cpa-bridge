// Package registry fetches the live OpenCode Go /v1/models catalog,
// normalizes metadata, assigns routes, and serves the last good snapshot.
//
// Route resolution priority follows arch spec: user route override >
// built-in compatibility table > unsupported (excluded from the routable set,
// kept in diagnostics).
//
// No scheduling lives here; wires the refresh cadence.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-cpa-bridge/internal/config"
)

// catalogBudgetFloor keeps an undersized max-response-bytes knob from
// freezing stale serving forever: catalogs grow larger than typical
// completion bodies, so the fetch budget never drops below 16 MiB while
// max-response-bytes stays meaningful for completions elsewhere.
const catalogBudgetFloor = 16 << 20

// Route is the upstream protocol a model is served through.
type Route string

const (
	RouteChatCompletions Route = "chat-completions"
	RouteMessages        Route = "messages"
	RouteResponses       Route = "responses"
)

// EndpointPath returns the upstream endpoint path for the route.
// routeEndpoints maps each route to its upstream endpoint path.
var routeEndpoints = map[Route]string{
	RouteChatCompletions: "/v1/chat/completions",
	RouteMessages:        "/v1/messages",
	RouteResponses:       "/v1/responses",
}

// EndpointPath returns the route's upstream endpoint path, or "" for
// unknown routes.
func (r Route) EndpointPath() string {
	return routeEndpoints[r]
}

func routeFromString(s string) (Route, bool) {
	if _, ok := routeEndpoints[Route(s)]; ok {
		return Route(s), true
	}
	return "", false
}

// ModelRecord is one routable normalized catalog entry.
type ModelRecord struct {
	PublicID     string
	UpstreamID   string
	DisplayName  string
	Protocol     Route
	EndpointPath string
	// UpstreamBaseURL is the model-specific base_url advertised in the
	// catalog's `api` entry. Empty means use the configured base-url.
	UpstreamBaseURL string
	ContextLimit int64
	OutputLimit  int64
	InputModes   []string
	OutputModes  []string
	Thinking     *pluginapi.ThinkingSupport
}

// UnsupportedModel is a discovered model excluded from the routable set,
// retained for diagnostics ( open questions).
type UnsupportedModel struct {
	UpstreamID string
	Reason     string
}

// HostClient issues HTTP requests through the host (pluginapi.HTTPDo
// semantics); 's host callback satisfies it directly.
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// rawModel tolerantly decodes one OpenCode Go catalog entry. The inference
// list uses OpenAI's list shape plus models.dev fields: name, limit.context /
// limit.output, modalities.input / .output, capabilities.reasoning, and an
// `api` object keyed by the protocol the model speaks (messages,
// chat-completions, responses, ...). Legacy flat fields are kept as fallbacks
// so a self-hosted catalog using the older shape still loads.
type rawModel struct {
	ID       string `json:"id"`
	DisplayName string `json:"display_name"`
	Name     string `json:"name"`
	ContextLength   int64 `json:"context_length"`
	MaxOutputTokens int64 `json:"max_output_tokens"`
	Limit struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	InputModes       []string `json:"input_modes"`
	OutputModes      []string `json:"output_modes"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Thinking     *rawThinking `json:"thinking"`
	Capabilities struct {
		Reasoning *struct {
			Supported bool     `json:"supported"`
			Effort    []string `json:"effort"`
		} `json:"reasoning"`
	} `json:"capabilities"`
	API map[string]struct {
		BaseURL string `json:"base_url"`
	} `json:"api"`
}

// normalize folds the OpenCode fields into the flat fields the snapshot
// builder reads. Explicit legacy values win when present.
func (e rawModel) normalize() rawModel {
	if e.DisplayName == "" {
		e.DisplayName = e.Name
	}
	if e.ContextLength == 0 {
		e.ContextLength = e.Limit.Context
	}
	if e.MaxOutputTokens == 0 {
		e.MaxOutputTokens = e.Limit.Output
	}
	if len(e.InputModes) == 0 {
		e.InputModes = e.InputModalities
	}
	if len(e.InputModes) == 0 {
		e.InputModes = e.Modalities.Input
	}
	if len(e.OutputModes) == 0 {
		e.OutputModes = e.OutputModalities
	}
	if len(e.OutputModes) == 0 {
		e.OutputModes = e.Modalities.Output
	}
	if e.Thinking == nil && e.Capabilities.Reasoning != nil && e.Capabilities.Reasoning.Supported {
		e.Thinking = &rawThinking{Levels: e.Capabilities.Reasoning.Effort}
	}
	return e
}

// rawThinking tolerantly decodes the optional per-model thinking object;
// pluginapi.ThinkingSupport itself is untagged, so snake_case upstream keys
// are decoded here and mapped field-by-field (partial objects decode to a
// partial struct).
type rawThinking struct {
	Min            *int     `json:"min"`
	Max            *int     `json:"max"`
	ZeroAllowed    *bool    `json:"zero_allowed"`
	DynamicAllowed *bool    `json:"dynamic_allowed"`
	Levels         []string `json:"levels"`
}

// DefaultRoute is the fallback when a catalog entry carries no `api` protocol
// metadata. OpenCode Go publishes each model's native protocol in `api`, so
// resolve normally picks the route from there; the fallback keeps older or
// hand-built catalogs routable.
const DefaultRoute = RouteChatCompletions

// Manager owns the catalog snapshot. All accessors are safe for
// concurrent use alongside Refresh.
type Manager struct {
	cfg    config.Config
	client HostClient

	mu sync.Mutex
	// raw retains the decoded upstream entries behind the current snapshot
	// so SeedFrom can rebuild records against a NEW config (route overrides,
	// protocol flags, and prefix settings may all have changed since the
	// snapshot was built).
	raw    []rawModel
	models []ModelRecord
	// index maps both PublicID and UpstreamID to their record so ID
	// resolution is O(1); rebuilt atomically with models on every swap.
	index map[string]ModelRecord
	unsup []UnsupportedModel
	warns []string
}

// New returns a Manager serving cfg through the host client.
func New(cfg config.Config, client HostClient) *Manager {
	return &Manager{cfg: cfg, client: client}
}

// SeedFrom republishes prev's last-good snapshot into m by REBUILDING every
// record from prev's raw upstream entries through the same
// resolution/validation path a refresh applies, but against m's OWN cfg:
// routes re-resolve (a removed override drops its model), the protocolEnabled
// gate re-applies, PublicIDs recompute under m's prefix, endpoints re-check
// against m's base-url, and collisions/diagnostics regenerate.
//
// m keeps its OWN cfg: subsequent Refresh calls use m's base-url/catalog-url/
// protocols/prefix ( a failed-refresh reconfigure must adopt the new
// manager rather than keep ticking the old URL). Because seeding rebuilds
// instead of copying, a config change between snapshots takes effect at seed
// time — e.g. a protocol kill-switch flipped during a catalog outage demotes
// its routes immediately instead of serving stale routable records until the
// outage ends.
func (m *Manager) SeedFrom(prev *Manager) {
	prev.mu.Lock()
	raw := prev.raw
	prev.mu.Unlock()
	m.swap(raw)
}

// Refresh fetches and swaps the catalog snapshot. On failure it returns a
// classified, key-free error; the previous snapshot keeps serving while
// catalog.stale-while-unavailable is enabled, otherwise the routable set
// is cleared until the next success.
func (m *Manager) Refresh(ctx context.Context, apiKey string) error {
	if m.client == nil {
		// Only direct construction can produce a nil host client
		// (production always wires the bridge); fail loudly through the
		// classified path instead of panicking inside Do.
		return m.fail("host client unavailable")
	}
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    m.cfg.CatalogURL,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + apiKey},
			"Accept":        []string{"application/json"},
		},
	}
	resp, err := m.client.Do(ctx, req)
	if err != nil {
		return m.fail("network error")
	}
	budget := max(m.cfg.MaxResponseBytes, catalogBudgetFloor)
	if int64(len(resp.Body)) > budget {
		return m.fail("response exceeds max-response-bytes")
	}
	if resp.StatusCode != http.StatusOK {
		return m.fail(fmt.Sprintf("http %d", resp.StatusCode))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return m.fail("invalid json")
	}
	var entries []rawModel
	var warns []string
	if env.Data == nil {
		// Key ABSENCE is upstream shape drift, never an intended clear:
		// clear like an empty plan (swap(nil)) but leave a diagnostic.
		// A present-but-empty array stays silent (tested-intended state).
		warns = append(warns, `upstream catalog response missing "data" field`)
	} else if err := json.Unmarshal(env.Data, &entries); err != nil {
		return m.fail("invalid json")
	}
	m.swap(entries, warns...)
	return nil
}

// fail applies the stale policy and returns the classified error.
func (m *Manager) fail(category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Catalog.StaleWhileUnavailable {
		// Clear the index too — Lookup must stop resolving IDs whose
		// records are gone. Raw entries go with them: a cleared
		// snapshot has nothing to seed.
		m.raw, m.models, m.index, m.unsup, m.warns = nil, nil, nil, nil, nil
	}
	return fmt.Errorf("catalog refresh failed: %s", category)
}

// swap builds the new snapshot from decoded entries and installs it
// atomically under the mutex (dedup, route priority).
// extraWarns are caller-supplied snapshot diagnostics (e.g. decode-level
// shape-drift notices) recorded alongside the per-entry ones.
func (m *Manager) swap(entries []rawModel, extraWarns ...string) {
	models := make([]ModelRecord, 0, len(entries))
	index := make(map[string]ModelRecord, len(entries)*2)
	var unsup []UnsupportedModel
	var warns []string
	warns = append(warns, extraWarns...)
	seen := make(map[string]bool, len(entries))
	for _, raw := range entries {
		e := raw.normalize()
		if e.ID == "" {
			unsup = append(unsup, UnsupportedModel{Reason: "missing id"})
			continue
		}
		if seen[e.ID] {
			warns = append(warns, fmt.Sprintf("duplicate model %q ignored; keeping first occurrence", e.ID))
			continue
		}
		seen[e.ID] = true
		route, endpoint, upstreamBase, ok, warn := m.resolve(e)
		if warn != "" {
			warns = append(warns, warn)
		}
		if !ok {
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: "no supported route determined"})
			continue
		}
		effectiveBase := m.cfg.BaseURL
		if upstreamBase != "" {
			effectiveBase = strings.TrimRight(upstreamBase, "/")
			if !safeUpstreamBase(effectiveBase, m.cfg.AllowHTTP) {
				unsup = append(unsup, UnsupportedModel{
					UpstreamID: e.ID,
					Reason:     "catalog api.base_url is not a safe upstream URL",
				})
				continue
			}
		}
		if !m.protocolEnabled(route) {
			// Flags apply at snapshot build; a stale snapshot kept per
			// reflects the flags it was built with until the next
			// successful refresh.
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     fmt.Sprintf("protocol %s disabled by config", route),
			})
			continue
		}
		if endpoint != "" && endpointEscapesBase(effectiveBase, endpoint) {
			// The joined URL must stay on the configured upstream
			// authority. Override endpoints flow through here, so they
			// cannot rewrite the URL host — e.g. "/v1@evil.com/x"
			// against a bare-host base-url turns the base into userinfo
			// and would send credentials/prompts to an attacker host
			// (trust boundary).
			unsup = append(unsup, UnsupportedModel{
				UpstreamID: e.ID,
				Reason:     "resolved endpoint escapes the configured upstream host",
			})
			continue
		}
		if endpoint == "" {
			endpoint = route.EndpointPath()
		}
		inputModes := firstNonEmpty(e.InputModes, e.InputModalities)
		outputModes := firstNonEmpty(e.OutputModes, e.OutputModalities)
		display := e.DisplayName
		if display == "" {
			display = e.ID
		}
		rec := ModelRecord{
			PublicID:        config.PublicID(m.cfg, e.ID),
			UpstreamID:      e.ID,
			DisplayName:     display,
			Protocol:        route,
			EndpointPath:    endpoint,
			UpstreamBaseURL: effectiveBase,
			ContextLimit: e.ContextLength,
			OutputLimit:  e.MaxOutputTokens,
			InputModes:   inputModes,
			OutputModes:  outputModes,
			Thinking:     normalizeThinking(e.Thinking),
		}
		// With a prefix enabled, one record's PublicID can equal another
		// record's UpstreamID (upstream "foo" and "opencode-go/foo" both
		// claim index key "<prefix>/foo"); last-write-wins would silently
		// misroute. First in catalog order wins (same dedup rule as
		// duplicate IDs); the later record is excluded. Same-role clashes
		// are impossible — distinct upstream IDs and an injective public
		// mapping — so the slot owner always holds the key via its other
		// role, and a record's own public==upstream rewrite is not a clash.
		if other, ok := index[rec.PublicID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", rec.PublicID, other.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		if other, ok := index[rec.UpstreamID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", other.PublicID, rec.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		models = append(models, rec)
		index[rec.PublicID] = rec
		index[rec.UpstreamID] = rec
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw, m.models, m.index, m.unsup, m.warns = entries, models, index, unsup, warns
}

// protocolEnabled reports whether the resolved route's protocol flag is on
// flags default to true.
func (m *Manager) protocolEnabled(r Route) bool {
	switch r {
	case RouteChatCompletions:
		return m.cfg.Protocols.ChatCompletions
	case RouteMessages:
		return m.cfg.Protocols.Messages
	case RouteResponses:
		return m.cfg.Protocols.Responses
	}
	return true
}

// resolve applies route priority strictly: user override, then the protocol
// advertised in the catalog's `api` object, then the chat-completions fallback.
// It returns the route, an optional endpoint override, and the model-specific
// upstream base URL (empty means use the configured one).
func (m *Manager) resolve(e rawModel) (Route, string, string, bool, string) {
	if o, ok := m.cfg.RouteOverrides[e.ID]; ok {
		// An override MUST be visible in diagnostics. Load
		// validation guarantees a known protocol, so the
		// map lookup above is the only decision point here.
		return Route(o.Protocol), o.Endpoint, "", true, fmt.Sprintf(
			"model %q routed via user override (%s %s)", e.ID, o.Protocol, o.Endpoint)
	}
	// Prefer Anthropic Messages when offered: it preserves thinking blocks
	// across all three downstream client formats. Chat-completions and
	// responses are equally valid translations of the same upstream model.
	for _, candidate := range []struct {
		key   string
		route Route
	}{
		{"messages", RouteMessages},
		{"chat-completions", RouteChatCompletions},
		{"responses", RouteResponses},
	} {
		if api, ok := e.API[candidate.key]; ok {
			return candidate.route, "", strings.TrimSpace(api.BaseURL), true, ""
		}
	}
	if len(e.API) > 0 {
		keys := make([]string, 0, len(e.API))
		for k := range e.API {
			keys = append(keys, k)
		}
		return "", "", "", false, fmt.Sprintf("model %q advertises no supported API protocol (%s)", e.ID, strings.Join(keys, ", "))
	}
	return DefaultRoute, "", "", true, ""
}

// safeUpstreamBase validates a base URL advertised by the upstream catalog
// before it is used for inference. An attacker-controlled catalog entry must
// not be able to redirect a credential-bearing request to an arbitrary host
// or scheme.
func safeUpstreamBase(raw string, allowHTTP bool) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return allowHTTP
	default:
		return false
	}
}

// JoinUpstreamURL joins a base URL (configured default or a model-specific
// api.base_url) onto an endpoint path and is the single source of truth for
// that join. The base already carries a version segment (default
// https://opencode.ai/inference/go/v1, catalog at {base}/models), so a leading
// "/v1" on the endpoint is dropped instead of doubled. The authority-escape
// check (endpointEscapesBase) must parse exactly the URL this produces — two
// independent join formulas would be free to drift apart and validate one URL
// while sending another.
func JoinUpstreamURL(baseURL, endpoint string) string {
	return strings.TrimSuffix(baseURL, "/") + strings.TrimPrefix(endpoint, "/v1")
}

// endpointEscapesBase reports whether joining baseURL with endpoint via
// JoinUpstreamURL would send the request to a different authority or scheme
// than the configured base-url. Any parse failure counts as escaping (fail
// closed).
func endpointEscapesBase(baseURL, endpoint string) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	got, err := url.Parse(JoinUpstreamURL(baseURL, endpoint))
	if err != nil {
		return true
	}
	return !strings.EqualFold(got.Host, base.Host) || got.Scheme != base.Scheme
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// normalizeThinking converts the decoded raw thinking object into its
// pluginapi form; absent objects yield nil, partial ones a partial struct.
func normalizeThinking(rt *rawThinking) *pluginapi.ThinkingSupport {
	if rt == nil {
		return nil
	}
	t := &pluginapi.ThinkingSupport{}
	if rt.Min != nil {
		t.Min = *rt.Min
	}
	if rt.Max != nil {
		t.Max = *rt.Max
	}
	if rt.ZeroAllowed != nil {
		t.ZeroAllowed = *rt.ZeroAllowed
	}
	if rt.DynamicAllowed != nil {
		t.DynamicAllowed = *rt.DynamicAllowed
	}
	if len(rt.Levels) > 0 {
		t.Levels = append([]string(nil), rt.Levels...)
	}
	return t
}

// Models returns the routable snapshot (identity fields). The slice
// is a shallow copy; records are rebuilt fresh on every swap and consumers
// treat them as read-only, so nested slices/thinking are shared safely.
func (m *Manager) Models() []ModelRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelRecord, len(m.models))
	copy(out, m.models)
	return out
}

// Lookup resolves an ID to its routable record, accepting the public ID
// (with or without prefix) or the bare upstream ID: the PublicID
// key wins over UpstreamID when both exist.
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.index[id]
	return rec, ok
}

// Unsupported returns models excluded from the routable set with reasons.
func (m *Manager) Unsupported() []UnsupportedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsupportedModel, len(m.unsup))
	copy(out, m.unsup)
	return out
}

// Warnings returns dedup and override diagnostics.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.warns))
	copy(out, m.warns)
	return out
}
