package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-cpa-bridge/internal/config"
)

// OpenCode Go exposes one usage endpoint for the whole subscription. It lives
// beside the OpenAI-compatible inference surface under the same authority:
// base-url points at <authority>/inference/go/v1, usage at /inference/go/v1/usage.
// The subscription API key authenticates it (Authorization: Bearer <key>),
// which is what lets one management-center card show one plan's remaining quota.
const (
	usagePath       = "/v1/usage"
	quotaMaxTimeout = 30 * time.Second
)

// quotaRefreshInterval is the background poll period for the whole key pool.
// It is a variable only so tests can shorten it; production always uses one
// minute, which is what the console footer promises.
var quotaRefreshInterval = time.Minute

const (
	// quotaMaxBackoff caps the per-credential retry delay after repeated
	// failures: a revoked key is retried every ten minutes rather than every
	// minute forever.
	quotaMaxBackoff = 10 * time.Minute
	// quotaPassTimeout bounds one full pass over the key pool so a pool of
	// slow accounts cannot make a pass run forever.
	quotaPassTimeout = 4 * time.Minute
)

// quotaWindow is one rate-limit window as rendered by the page. OpenCode Go
// reports percent used (0-100) and status ("ok" / "rate-limited") for three
// meters: rolling (5 hours), weekly, monthly.
type quotaWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resets_at,omitempty"`
}

// quotaUsage is one credential's account snapshot.
type quotaUsage struct {
	Plan        string      `json:"plan,omitempty"`
	Rolling     quotaWindow `json:"rolling"`
	Weekly      quotaWindow `json:"weekly"`
	Monthly     quotaWindow `json:"monthly"`
	RefreshedAt string      `json:"refreshed_at"`
}

type quotaCard struct {
	KeyID string `json:"key_id"`
	Label string `json:"label"`
	Email string `json:"email,omitempty"`
	// Usage is the last snapshot the background loop managed to read. It is
	// kept (and flagged stale) when the newest attempt failed, so an account
	// that rate-limits or goes unreachable still shows its last known quota.
	Usage *quotaUsage `json:"usage,omitempty"`
	Stale bool        `json:"stale,omitempty"`
	Error string      `json:"error,omitempty"`
}

type quotaRequest struct {
	KeyID string `json:"key_id"`
}

// quotaList is the cached view the page polls. Listing never calls upstream:
// the background loop is the only writer, so a poll is a cheap local read and
// an open page follows the poll without adding account traffic.
type quotaList struct {
	Cards           []quotaCard `json:"cards"`
	IntervalSeconds int         `json:"interval_seconds,omitempty"`
	NextRefreshAt   string      `json:"next_refresh_at,omitempty"`
	LastPassAt      string      `json:"last_pass_at,omitempty"`
	Refreshing      bool        `json:"refreshing,omitempty"`
}

// quotaEntry is one credential's cached account snapshot.
type quotaEntry struct {
	Usage quotaUsage
	Label string
	Email string
	At    time.Time
	Err   string
	Failures int
	RetryAt  time.Time
}

// quotaState is the background-refresh cache shared by the loop and the
// management API. Its own mutex keeps a slow upstream away from the Manager's
// config lock. Every method tolerates a nil receiver: a zero-value Manager
// (only reachable by direct construction in tests) must degrade to an empty
// cache rather than panic.
type quotaState struct {
	mu       sync.RWMutex
	entries  map[string]quotaEntry
	lastPass time.Time
	passing  bool
}

func newQuotaState() *quotaState {
	return &quotaState{entries: map[string]quotaEntry{}}
}

func (s *quotaState) get(id string) (quotaEntry, bool) {
	if s == nil {
		return quotaEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	return e, ok
}

func (s *quotaState) put(id string, e quotaEntry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]quotaEntry{}
	}
	s.entries[id] = e
}

// snapshot copies the cache so a caller can render without holding the lock.
func (s *quotaState) snapshot() (map[string]quotaEntry, time.Time, bool) {
	out := map[string]quotaEntry{}
	if s == nil {
		return out, time.Time{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, e := range s.entries {
		out[id] = e
	}
	return out, s.lastPass, s.passing
}

func (s *quotaState) setPassing(v bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passing = v
}

func (s *quotaState) setLastPass(t time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPass = t
}

// prune drops snapshots of credentials that no longer exist, so a deleted key
// cannot keep showing a card's quota forever.
func (s *quotaState) prune(keep map[string]bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.entries {
		if !keep[id] {
			delete(s.entries, id)
		}
	}
}

// ---- account API response shapes (official OpenCode Go usage) ------------

type accountUsageMeter struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

type accountUsageResponse struct {
	Usage struct {
		Rolling accountUsageMeter `json:"rolling"`
		Weekly  accountUsageMeter `json:"weekly"`
		Monthly accountUsageMeter `json:"monthly"`
	} `json:"usage"`
}

func quotaIdentity(key string) (id, label string) {
	hash := authKeyHash(key)
	return authRecordIDFromHash(hash), "OpenCode Go credential " + hash[:12]
}

// quotaKey is one credential the quota page can refresh: the auth-record id,
// a display label, and the resolved API key.
type quotaKey struct {
	ID    string
	Label string
	Key   string
}

// quotaKeys resolves every Go credential from the YAML keys and the host auth
// records, de-duplicated by id. The key material never leaves this process
// except as the upstream Authorization header.
func (m *Manager) quotaKeys(ctx context.Context) []quotaKey {
	out := []quotaKey{}
	seen := map[string]bool{}
	add := func(id, label, key string) {
		if id == "" || key == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, quotaKey{ID: id, Label: label, Key: key})
	}
	m.mu.RLock()
	cfgKeys := append([]config.APIKey(nil), m.cfg.APIKeys...)
	m.mu.RUnlock()
	for _, k := range cfgKeys {
		id, label := quotaIdentity(k.Value)
		add(id, label, k.Value)
	}
	if entries, err := m.authEntries(ctx); err == nil {
		for _, e := range entries {
			id := strings.TrimSpace(e.ID)
			if id == "" {
				id = strings.TrimSuffix(strings.TrimSpace(e.Name), ".json")
			}
			label := strings.TrimSpace(e.Label)
			if label == "" {
				label = id
			}
			key := ""
			if path := strings.TrimSpace(e.Path); path != "" {
				key = readKeyFromFile(path)
			}
			add(id, label, key)
		}
	}
	return out
}

// quotaAPI serves the per-credential quota cards. An empty key_id lists the
// cached cards without any upstream call; a key_id force-refreshes that one
// card. Neither path is how the page stays current — the background loop is —
// so a page left open follows the loop instead of polling the account API.
func (m *Manager) quotaAPI(ctx context.Context, bodyBytes []byte) pluginapi.ManagementResponse {
	var body quotaRequest
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
	}
	keys := m.quotaKeys(ctx)
	if body.KeyID == "" {
		return m.quotaListResponse(ctx, keys)
	}
	cfg := m.currentConfig()
	for _, key := range keys {
		if key.ID != body.KeyID {
			continue
		}
		// An explicit refresh is the operator saying "now": ignore the retry
		// backoff and re-read this credential's meters immediately.
		entry := m.refreshQuotaEntry(ctx, cfg, key, true)
		emails := m.accountEmails(ctx)
		return quotaJSON(m.quotaCard(key, entry, emails[key.ID]))
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown quota key"})
}

// quotaListResponse renders the cached snapshots. It is a local read: the
// mailbox falls back to the auth file, and usage comes from the loop's last
// successful attempt.
func (m *Manager) quotaListResponse(ctx context.Context, keys []quotaKey) pluginapi.ManagementResponse {
	snaps, lastPass, passing := m.quota.snapshot()
	emails := m.accountEmails(ctx)
	cards := make([]quotaCard, 0, len(keys))
	for _, key := range keys {
		entry, ok := snaps[key.ID]
		if !ok {
			entry = quotaEntry{}
		}
		cards = append(cards, m.quotaCard(key, entry, emails[key.ID]))
	}
	out := quotaList{
		Cards:           cards,
		IntervalSeconds: int(quotaRefreshInterval / time.Second),
		Refreshing:      passing,
	}
	if !lastPass.IsZero() {
		out.LastPassAt = lastPass.UTC().Format(time.RFC3339)
		out.NextRefreshAt = lastPass.Add(quotaRefreshInterval).UTC().Format(time.RFC3339)
	}
	return quotaJSON(out)
}

// quotaCard merges a credential's cache entry with the mailbox known from its
// auth file into one card. A failed attempt keeps the last good snapshot and
// flags it stale, so a transient outage degrades the card instead of blanking
// it.
func (m *Manager) quotaCard(key quotaKey, entry quotaEntry, fileEmail string) quotaCard {
	card := quotaCard{KeyID: key.ID}
	email := entry.Email
	if email == "" {
		email = strings.TrimSpace(fileEmail)
	}
	card.Email = email
	switch {
	case entry.Label != "":
		card.Label = entry.Label
	case email != "":
		card.Label = email
	default:
		card.Label = key.Label
	}
	if !entry.At.IsZero() {
		usage := entry.Usage
		card.Usage = &usage
	}
	if entry.Err != "" {
		card.Error = entry.Err
		card.Stale = card.Usage != nil
	}
	return card
}

func quotaJSON(v any) pluginapi.ManagementResponse {
	body, err := json.Marshal(v)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "quota response encoding failed"})
	}
	return pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body}
}

// UsageAPIBase derives the usage API root from the configured inference
// base-url by trimming the trailing /v1. The host is preserved so staging and
// self-hosted bases keep working, and no second config key is needed.
func UsageAPIBase(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("usage API base unavailable: base-url is not an absolute URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("usage API base unavailable: unsupported scheme %q", u.Scheme)
	}
	path := strings.TrimSuffix(u.Path, "/")
	path = strings.TrimSuffix(path, "/v1")
	return u.Scheme + "://" + u.Host + path, nil
}

// fetchQuota reads one credential's subscription meters from the official
// usage endpoint. It is a single request per credential per poll: the response
// already carries all three windows and the status of each.
func fetchQuota(ctx context.Context, bridge *HostBridge, baseURL string, timeout time.Duration, key string) (quotaUsage, error) {
	if bridge == nil {
		return quotaUsage{}, fmt.Errorf("account bridge unavailable")
	}
	base, err := UsageAPIBase(baseURL)
	if err != nil {
		return quotaUsage{}, err
	}
	if timeout <= 0 || timeout > quotaMaxTimeout {
		timeout = quotaMaxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := bridge.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    base + usagePath,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + key},
			"Accept":        []string{"application/json"},
		},
	})
	if err != nil {
		return quotaUsage{}, fmt.Errorf("usage request failed")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return quotaUsage{}, fmt.Errorf("usage request rejected (401): key invalid or expired")
	}
	if resp.StatusCode == http.StatusForbidden {
		return quotaUsage{}, fmt.Errorf("usage request rejected (403): active OpenCode Go subscription required")
	}
	if resp.StatusCode != http.StatusOK {
		return quotaUsage{}, fmt.Errorf("usage request rejected (%d)", resp.StatusCode)
	}
	var payload accountUsageResponse
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return quotaUsage{}, fmt.Errorf("usage response invalid: %v", err)
	}
	meters := payload.Usage
	if meters.Rolling.Status == "" && meters.Weekly.Status == "" && meters.Monthly.Status == "" {
		return quotaUsage{}, fmt.Errorf("usage response missing meters")
	}
	return quotaUsage{
		Plan:        "OpenCode Go",
		Rolling:     windowFromMeter(meters.Rolling),
		Weekly:      windowFromMeter(meters.Weekly),
		Monthly:     windowFromMeter(meters.Monthly),
		RefreshedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// windowFromMeter normalizes one upstream meter into the wire shape the page
// renders: status defaults to ok, percent clamps to 0..100, and resetsAt is
// converted to RFC3339 UTC when parseable.
func windowFromMeter(m accountUsageMeter) quotaWindow {
	out := quotaWindow{Status: strings.TrimSpace(m.Status), Percent: m.Percent}
	if out.Status == "" {
		out.Status = "ok"
	}
	if out.Percent < 0 {
		out.Percent = 0
	}
	if out.Percent > 100 {
		out.Percent = 100
	}
	if raw := strings.TrimSpace(m.ResetsAt); raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			out.ResetsAt = t.UTC().Format(time.RFC3339)
		} else {
			out.ResetsAt = raw
		}
	}
	return out
}

// ---- background refresh -------------------------------------------------

// startQuotaLoop polls the usage endpoint for the whole key pool every
// quotaRefreshInterval. This is what makes the console's quota cards update on
// their own: before it existed the page only fetched while it was being opened,
// so a card froze the moment the operator stopped clicking.
//
// The first tick is deliberately one full interval away rather than immediate.
// Registration already spends its startup budget on the catalog, and a page
// that opens later fetches any card the loop has not filled yet through the
// per-key endpoint, so a cold cache is covered without adding work to startup.
func (m *Manager) startQuotaLoop(stop, done chan struct{}) {
	if m.bridge == nil || m.quota == nil {
		close(done)
		return
	}
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		defer close(done)
		defer cancel()
		ticker := time.NewTicker(quotaRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							_ = m.bridge.Log("error", "quota refresh panicked", nil)
						}
					}()
					ctx, passCancel := context.WithTimeout(stopCtx, quotaPassTimeout)
					defer passCancel()
					m.refreshQuotaPass(ctx)
				}()
			}
		}
	}()
}

// refreshQuotaPass refreshes every credential once. Credentials are walked
// sequentially: the account API is not documented as burst-tolerant, and a
// minute is plenty for a handful of subscriptions.
func (m *Manager) refreshQuotaPass(ctx context.Context) {
	if m.bridge == nil {
		return
	}
	cfg := m.currentConfig()
	keys := m.quotaKeys(ctx)
	keep := make(map[string]bool, len(keys))
	for _, key := range keys {
		keep[key.ID] = true
	}
	m.quota.prune(keep)
	if len(keys) == 0 {
		m.quota.setLastPass(time.Now())
		return
	}
	m.quota.setPassing(true)
	defer m.quota.setPassing(false)
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		m.refreshQuotaEntry(ctx, cfg, key, false)
	}
	m.quota.setLastPass(time.Now())
}

// refreshQuotaEntry updates one credential's cache entry. force ignores the
// retry backoff and is what a manual refresh uses.
//
// A failure never discards the last good snapshot: it keeps the usage, records
// the reason, and pushes the next attempt out by a backoff that grows with
// consecutive failures. A revoked key therefore costs one request every ten
// minutes instead of one a minute, while the card keeps showing its last known
// numbers with the failure beside them.
func (m *Manager) refreshQuotaEntry(ctx context.Context, cfg config.Config, key quotaKey, force bool) quotaEntry {
	now := time.Now()
	prior, had := m.quota.get(key.ID)
	if !force && had && now.Before(prior.RetryAt) {
		return prior
	}
	if ctx.Err() != nil {
		return prior
	}
	usage, err := fetchQuota(ctx, m.bridge, cfg.BaseURL, cfg.RequestTimeout, key.Key)
	if err != nil {
		entry := prior
		// A cancelled pass (reconfigure / shutdown) says nothing about the
		// account, so it must not be recorded as a failure.
		if ctx.Err() != nil {
			return entry
		}
		entry.Err = err.Error()
		entry.Failures = prior.Failures + 1
		backoff := time.Duration(entry.Failures) * quotaRefreshInterval
		if backoff > quotaMaxBackoff {
			backoff = quotaMaxBackoff
		}
		entry.RetryAt = now.Add(backoff)
		m.quota.put(key.ID, entry)
		_ = m.bridge.Log("warn", "quota refresh failed", map[string]any{
			"credential": key.ID, "reason": err.Error(), "failures": entry.Failures,
		})
		return entry
	}

	entry := prior
	entry.Usage = usage
	entry.Err = ""
	entry.Failures = 0
	entry.RetryAt = time.Time{}
	entry.At = now
	if !had {
		if emails := m.accountEmails(ctx); emails != nil {
			entry.Email = strings.TrimSpace(emails[key.ID])
		}
	}
	if entry.Label == "" {
		entry.Label = key.Label
	}
	m.quota.put(key.ID, entry)
	return entry
}
