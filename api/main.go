package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
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
	lluEmail     string
	lluPassword  string
	lluPatientID string
)

var (
	lluMu            sync.Mutex
	lluCachedToken   string
	lluTokenExpiry   time.Time
	lluCachedAccount string
	lluBase          = "https://api.libreview.io"
)

type lluLoginResp struct {
	Status int `json:"status"`
	Data   struct {
		AuthTicket struct {
			Token   string `json:"token"`
			Expires int64  `json:"expires"`
		} `json:"authTicket"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Redirect bool   `json:"redirect"`
		Region   string `json:"region"`
	} `json:"data"`
}

type lluGraphPoint struct {
	FactoryTimestamp string  `json:"FactoryTimestamp"`
	Timestamp        string  `json:"Timestamp"`
	Value            float64 `json:"Value"`
}

type lluConnection struct {
	PatientID          string `json:"patientId"`
	GlucoseMeasurement struct {
		FactoryTimestamp string  `json:"FactoryTimestamp"`
		Timestamp        string  `json:"Timestamp"`
		Value            float64 `json:"Value"`
		TrendArrow       int     `json:"TrendArrow"`
	} `json:"glucoseMeasurement"`
}

// lluGraphResp é a resposta de GET /llu/connections/{patientId}/graph,
// que traz o histórico de leituras (a conexão isolada não traz graphData).
type lluGraphResp struct {
	Status int `json:"status"`
	Data   struct {
		GraphData []lluGraphPoint `json:"graphData"`
	} `json:"data"`
}

// lluTimeLayouts são os formatos de timestamp observados na LibreLinkUp API.
var lluTimeLayouts = []string{"1/2/2006 3:04:05 PM", "1/2/2006 15:04:05", "2006-01-02T15:04:05"}

// parseLLUTimestamp prefere factoryTimestamp (sempre UTC); timestamp costuma
// vir no fuso local do leitor e só é usado como fallback.
func parseLLUTimestamp(factoryTimestamp, timestamp string) time.Time {
	raw := factoryTimestamp
	if raw == "" {
		raw = timestamp
	}
	for _, layout := range lluTimeLayouts {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t
		}
	}
	return time.Time{}
}

type lluConnResp struct {
	Status int             `json:"status"`
	Data   json.RawMessage `json:"data"`
}

// parseConnections decodifica lluConnResp.Data, que a LibreLinkUp API retorna
// como array quando a conta segue múltiplos pacientes, ou como objeto único
// quando segue apenas um.
func parseConnections(raw json.RawMessage) ([]lluConnection, error) {
	var list []lluConnection
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var single lluConnection
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("LLU connections decode: %w", err)
	}
	return []lluConnection{single}, nil
}

type GlucoseHistoryPoint struct {
	MinutesAgo int     `json:"m"`
	Value      float64 `json:"v"`
}

type GlucoseResponse struct {
	Value      float64               `json:"value"`
	TrendIcon  string                `json:"trend_icon"`
	MinutesAgo int                   `json:"minutes_ago"`
	Low        bool                  `json:"low"`
	High       bool                  `json:"high"`
	History    []GlucoseHistoryPoint `json:"history"`
}

const glucoseHistoryWindowMin = 6 * 60

func lluSetHeaders(req *http.Request, token, accountIDHash string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("product", "llu.ios")
	req.Header.Set("version", "4.16.0")
	req.Header.Set("User-Agent", "FreeStyle LibreLink Up/4.16.0 CFNetwork/1408.0.4 Darwin/22.5.0")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accountIDHash != "" {
		req.Header.Set("Account-Id", accountIDHash)
	}
}

func lluDoLogin(base string) error {
	body, _ := json.Marshal(map[string]string{"email": lluEmail, "password": lluPassword})
	req, err := http.NewRequest(http.MethodPost, base+"/llu/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	lluSetHeaders(req, "", "")

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
	// A API exige o header Account-Id = sha256(user.id) em todas as chamadas autenticadas.
	sum := sha256.Sum256([]byte(lr.Data.User.ID))
	lluCachedAccount = hex.EncodeToString(sum[:])
	return nil
}

func lluGetToken() (token, accountIDHash string, err error) {
	lluMu.Lock()
	defer lluMu.Unlock()
	if lluCachedToken != "" && time.Now().Before(lluTokenExpiry) {
		return lluCachedToken, lluCachedAccount, nil
	}
	if err := lluDoLogin(lluBase); err != nil {
		return "", "", err
	}
	return lluCachedToken, lluCachedAccount, nil
}

// lluFetchConnectionsRaw faz a chamada a /llu/connections e devolve status + corpo crú,
// sem decodificar — usado tanto pelo fluxo normal quanto pelo endpoint de debug.
func lluFetchConnectionsRaw() (int, []byte, error) {
	token, accountIDHash, err := lluGetToken()
	if err != nil {
		return 0, nil, err
	}

	req, err := http.NewRequest(http.MethodGet, lluBase+"/llu/connections", nil)
	if err != nil {
		return 0, nil, err
	}
	lluSetHeaders(req, token, accountIDHash)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	// Token expirado — força re-login na próxima chamada
	if resp.StatusCode == http.StatusUnauthorized {
		lluMu.Lock()
		lluCachedToken = ""
		lluMu.Unlock()
		return resp.StatusCode, nil, fmt.Errorf("token expirado, tente novamente")
	}

	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// lluFetchGraph busca o histórico de leituras do paciente em
// GET /llu/connections/{patientId}/graph.
func lluFetchGraph(patientID string) ([]lluGraphPoint, error) {
	token, accountIDHash, err := lluGetToken()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, lluBase+"/llu/connections/"+patientID+"/graph", nil)
	if err != nil {
		return nil, err
	}
	lluSetHeaders(req, token, accountIDHash)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("LLU graph retornou status %d: %s", resp.StatusCode, snippet)
	}

	var gr lluGraphResp
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("LLU graph decode: %w", err)
	}
	return gr.Data.GraphData, nil
}

func lluFetchGlucose() (*GlucoseResponse, error) {
	status, body, err := lluFetchConnectionsRaw()
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("LLU connections retornou status %d: %s", status, snippet)
	}

	var cr lluConnResp
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("LLU connections decode: %w", err)
	}

	connections, err := parseConnections(cr.Data)
	if err != nil {
		return nil, err
	}
	if len(connections) == 0 {
		return nil, fmt.Errorf("nenhuma leitura disponível")
	}

	conn := connections[0]
	if len(connections) > 1 {
		// Só exige correspondência exata quando há ambiguidade real.
		if lluPatientID == "" {
			return nil, fmt.Errorf("conta LibreLinkUp segue %d pacientes — defina LLU_PATIENT_ID para escolher qual", len(connections))
		}
		found := false
		for _, c := range connections {
			if c.PatientID == lluPatientID {
				conn = c
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("LLU_PATIENT_ID %q não encontrado entre %d conexões", lluPatientID, len(connections))
		}
	}

	gm := conn.GlucoseMeasurement

	t := parseLLUTimestamp(gm.FactoryTimestamp, gm.Timestamp)
	minutesAgo := 0
	if !t.IsZero() {
		minutesAgo = int(time.Since(t).Minutes())
	}

	icons := map[int]string{1: "vv", 2: "v", 3: "->", 4: "^", 5: "^^"}
	icon := icons[gm.TrendArrow]
	if icon == "" {
		icon = "->"
	}

	// Histórico vem de um endpoint separado — se falhar, devolve a leitura
	// atual mesmo assim (history vazio) em vez de quebrar a rota inteira.
	graphPoints, err := lluFetchGraph(conn.PatientID)
	if err != nil {
		log.Printf("LLU graph fetch falhou: %v", err)
		graphPoints = nil
	}

	history := make([]GlucoseHistoryPoint, 0, len(graphPoints))
	for _, p := range graphPoints {
		pt := parseLLUTimestamp(p.FactoryTimestamp, p.Timestamp)
		if pt.IsZero() {
			continue
		}
		m := int(time.Since(pt).Minutes())
		if m < 0 || m > glucoseHistoryWindowMin {
			continue
		}
		history = append(history, GlucoseHistoryPoint{MinutesAgo: m, Value: p.Value})
	}
	sort.Slice(history, func(i, j int) bool { return history[i].MinutesAgo > history[j].MinutesAgo })

	return &GlucoseResponse{
		Value:      gm.Value,
		TrendIcon:  icon,
		MinutesAgo: minutesAgo,
		Low:        gm.Value < 70,
		High:       gm.Value > 180,
		History:    history,
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
		token := strings.TrimPrefix(auth, "Bearer ")
		if !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare([]byte(token), []byte(apiBearerToken)) != 1 {
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

func handleGlucoseDebug(w http.ResponseWriter, _ *http.Request) {
	if lluEmail == "" || lluPassword == "" {
		jsonError(w, http.StatusNotImplemented, "LLU_EMAIL / LLU_PASSWORD não configurados")
		return
	}
	status, body, err := lluFetchConnectionsRaw()
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		parsed = string(body)
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"status": status,
		"body":   parsed,
	})
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
	lluPatientID = os.Getenv("LLU_PATIENT_ID")

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
	mux.HandleFunc("GET /glucose/debug", bearer(handleGlucoseDebug))
	mux.HandleFunc("GET /debug", bearer(handleDebug))

	log.Println("claude-monitor API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
