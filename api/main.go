package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

var (
	claudeSessionKey string
	claudeOrgID      string
	apiBearerToken   string
	pollTimeout      int
	usageURL         string
)

type UsageResponse struct {
	SessionPct      float64 `json:"session_pct"`
	SessionResetsIn string  `json:"session_resets_in"`
	WeeklyPct       float64 `json:"weekly_pct"`
	WeeklyResetsAt  string  `json:"weekly_resets_at"`
}

type claudeResp struct {
	FiveHour *usageWindow `json:"five_hour"`
	SevenDay *usageWindow `json:"seven_day"`
}

type usageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

func timeUntil(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		if len(iso) >= 16 {
			return iso[:16]
		}
		return iso
	}
	diff := time.Until(t)
	if diff <= 0 {
		return "now"
	}
	h := int(diff.Hours())
	m := int(diff.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func formatResetsAt(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		if len(iso) >= 16 {
			return iso[:16]
		}
		return iso
	}
	return t.Local().Format("Mon 15:04")
}

func newTLSClient() (tls_client.HttpClient, error) {
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(pollTimeout),
		tls_client.WithClientProfile(profiles.Chrome_120),
	}
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
}

func fetchClaude() (*claudeResp, int, []byte, error) {
	client, err := newTLSClient()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("tls client: %w", err)
	}

	req, err := fhttp.NewRequest(fhttp.MethodGet, usageURL, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("anthropic-client-platform", "web_claude_ai")
	req.Header.Set("anthropic-client-version", "1.0.0")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("referer", "https://claude.ai/settings")
	req.Header.Set("Cookie", "sessionKey="+claudeSessionKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, body, nil
	}

	var cr claudeResp
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, resp.StatusCode, body, fmt.Errorf("unmarshal: %w", err)
	}
	return &cr, resp.StatusCode, body, nil
}

// ── middleware ────────────────────────────────────────────────────────────────

func bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != apiBearerToken {
			jsonError(w, http.StatusUnauthorized, "Invalid token")
			return
		}
		next(w, r)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func jsonWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonWrite(w, status, map[string]string{"detail": msg})
}

// ── handlers ──────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	jsonWrite(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleUsage(w http.ResponseWriter, _ *http.Request) {
	cr, status, _, err := fetchClaude()
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	switch status {
	case http.StatusUnauthorized:
		jsonError(w, http.StatusBadGateway, "claude.ai session expired — update CLAUDE_SESSION_KEY")
		return
	case http.StatusForbidden:
		jsonError(w, http.StatusBadGateway, "claude.ai returned 403 — check CLAUDE_ORG_ID and CLAUDE_SESSION_KEY")
		return
	default:
		if status != http.StatusOK {
			jsonError(w, http.StatusBadGateway, fmt.Sprintf("claude.ai returned %d", status))
			return
		}
	}
	if cr.FiveHour == nil && cr.SevenDay == nil {
		jsonError(w, http.StatusBadGateway, "unexpected response shape — use /debug")
		return
	}

	out := UsageResponse{}
	if cr.FiveHour != nil {
		out.SessionPct = cr.FiveHour.Utilization
		out.SessionResetsIn = timeUntil(cr.FiveHour.ResetsAt)
	}
	if cr.SevenDay != nil {
		out.WeeklyPct = cr.SevenDay.Utilization
		out.WeeklyResetsAt = formatResetsAt(cr.SevenDay.ResetsAt)
	}
	jsonWrite(w, http.StatusOK, out)
}

func handleDebug(w http.ResponseWriter, _ *http.Request) {
	_, status, body, err := fetchClaude()
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		s := string(body)
		if len(s) > 1000 {
			s = s[:1000]
		}
		parsed = s
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"url":    usageURL,
		"status": status,
		"body":   parsed,
	})
}

// ── bootstrap ─────────────────────────────────────────────────────────────────

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %q is not set", key)
	}
	return v
}

func main() {
	claudeSessionKey = mustEnv("CLAUDE_SESSION_KEY")
	claudeOrgID = mustEnv("CLAUDE_ORG_ID")
	apiBearerToken = mustEnv("API_BEARER_TOKEN")

	pollTimeout = 10
	if s := os.Getenv("POLL_TIMEOUT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			pollTimeout = n
		}
	}

	usageURL = "https://claude.ai/api/organizations/" + claudeOrgID + "/usage"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /usage", bearer(handleUsage))
	mux.HandleFunc("GET /debug", bearer(handleDebug))

	log.Println("claude-monitor API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
