package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

var (
	claudeSessionKey string
	claudeOrgID      string
	apiBearerToken   string
	extraCookies     string
	pollTimeout      int
	usageURL         string
)

// ── LibreLinkUp ───────────────────────────────────────────────────────────────

var (
	lluEmail    string
	lluPassword string
)

var (
	lluMu          sync.Mutex
	lluCachedToken string
	lluTokenExpiry time.Time
	lluBase        = "https://api.libreview.io"
)

type lluLoginResp struct {
	Status int `json:"status"`
	Data   struct {
		AuthTicket struct {
			Token   string `json:"token"`
			Expires int64  `json:"expires"`
		} `json:"authTicket"`
		Redirect bool   `json:"redirect"`
		Region   string `json:"region"`
	} `json:"data"`
}

type lluConnResp struct {
	Status int `json:"status"`
	Data   []struct {
		GlucoseMeasurement struct {
			Timestamp  string  `json:"Timestamp"`
			Value      float64 `json:"Value"`
			TrendArrow int     `json:"TrendArrow"`
		} `json:"glucoseMeasurement"`
	} `json:"data"`
}

type GlucoseResponse struct {
	Value      float64 `json:"value"`
	TrendIcon  string  `json:"trend_icon"`
	MinutesAgo int     `json:"minutes_ago"`
	Low        bool    `json:"low"`
	High       bool    `json:"high"`
}

func lluSetHeaders(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("product", "llu.ios")
	req.Header.Set("version", "4.7.0")
	req.Header.Set("User-Agent", "FreeStyle LibreLink Up/4.7.0 CFNetwork/1408.0.4 Darwin/22.5.0")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func lluDoLogin(base string) error {
	body, _ := json.Marshal(map[string]string{"email": lluEmail, "password": lluPassword})
	req, err := http.NewRequest(http.MethodPost, base+"/llu/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	lluSetHeaders(req, "")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var lr lluLoginResp
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return fmt.Errorf("LLU login decode: %w", err)
	}

	// API pode redirecionar para região específica (ex: api-eu.libreview.io)
	if lr.Data.Redirect && lr.Data.Region != "" {
		lluBase = "https://api-" + lr.Data.Region + ".libreview.io"
		return lluDoLogin(lluBase)
	}

	if lr.Data.AuthTicket.Token == "" {
		return fmt.Errorf("LLU login falhou: status %d", lr.Status)
	}

	lluCachedToken = lr.Data.AuthTicket.Token
	lluTokenExpiry = time.Unix(lr.Data.AuthTicket.Expires, 0).Add(-5 * time.Minute)
	return nil
}

func lluGetToken() (string, error) {
	lluMu.Lock()
	defer lluMu.Unlock()
	if lluCachedToken != "" && time.Now().Before(lluTokenExpiry) {
		return lluCachedToken, nil
	}
	if err := lluDoLogin(lluBase); err != nil {
		return "", err
	}
	return lluCachedToken, nil
}

func lluFetchGlucose() (*GlucoseResponse, error) {
	token, err := lluGetToken()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, lluBase+"/llu/connections", nil)
	if err != nil {
		return nil, err
	}
	lluSetHeaders(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Token expirado — força re-login na próxima chamada
	if resp.StatusCode == http.StatusUnauthorized {
		lluMu.Lock()
		lluCachedToken = ""
		lluMu.Unlock()
		return nil, fmt.Errorf("token expirado, tente novamente")
	}

	var cr lluConnResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("LLU connections decode: %w", err)
	}
	if len(cr.Data) == 0 {
		return nil, fmt.Errorf("nenhuma leitura disponível")
	}

	gm := cr.Data[0].GlucoseMeasurement

	// Tenta múltiplos formatos de timestamp que a API retorna
	var t time.Time
	for _, layout := range []string{"1/2/2006 3:04:05 PM", "1/2/2006 15:04:05", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, gm.Timestamp, time.UTC); err == nil {
			t = parsed
			break
		}
	}
	minutesAgo := 0
	if !t.IsZero() {
		minutesAgo = int(time.Since(t).Minutes())
	}

	icons := map[int]string{1: "vv", 2: "v", 3: "->", 4: "^", 5: "^^"}
	icon := icons[gm.TrendArrow]
	if icon == "" {
		icon = "->"
	}

	return &GlucoseResponse{
		Value:      gm.Value,
		TrendIcon:  icon,
		MinutesAgo: minutesAgo,
		Low:        gm.Value < 70,
		High:       gm.Value > 180,
	}, nil
}

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
		tls_client.WithClientProfile(profiles.Chrome_131),
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
	// Header order matches Chrome 131 fetch() — Cloudflare inspects ordering
	req.Header.Set("sec-ch-ua", `"Google Chrome";v="131", "Chromium";v="131", "Not-A.Brand";v="24"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("anthropic-client-platform", "web_claude_ai")
	req.Header.Set("anthropic-client-version", "1.0.0")
	req.Header.Set("referer", "https://claude.ai/settings")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("priority", "u=1, i")
	cookie := "sessionKey=" + claudeSessionKey
	if extraCookies != "" {
		cookie += "; " + extraCookies
	}
	req.Header.Set("Cookie", cookie)

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

func handleGlucose(w http.ResponseWriter, _ *http.Request) {
	if lluEmail == "" || lluPassword == "" {
		jsonError(w, http.StatusNotImplemented, "LLU_EMAIL / LLU_PASSWORD não configurados")
		return
	}
	gr, err := lluFetchGlucose()
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, gr)
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
	extraCookies = os.Getenv("EXTRA_COOKIES")
	lluEmail = os.Getenv("LLU_EMAIL")
	lluPassword = os.Getenv("LLU_PASSWORD")

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
	mux.HandleFunc("GET /glucose", bearer(handleGlucose))
	mux.HandleFunc("GET /debug", bearer(handleDebug))

	log.Println("claude-monitor API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
