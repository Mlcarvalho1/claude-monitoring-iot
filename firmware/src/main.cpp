#include <Arduino.h>
#include <Wire.h>
#include <LiquidCrystal_I2C.h>
#include <WiFi.h>
#include <WebServer.h>
#include <HTTPClient.h>
#include <WiFiClientSecure.h>
#include <ArduinoJson.h>
#include <LittleFS.h>

// ── Hardware ────────────────────────────────────────────────────────────────
#define BTN_NEXT   14   // GP14 — next screen
#define BTN_PREV   15   // GP15 — previous screen
#define SDA_PIN     4   // GP4
#define SCL_PIN     5   // GP5

LiquidCrystal_I2C lcd(0x27, 16, 2);

// ── Config (persisted in LittleFS) ──────────────────────────────────────────
struct Config {
    char ssid[64];
    char password[64];
    char api_url[128];    // e.g. https://myserver.com/usage
    char api_token[128];  // Bearer token
    uint32_t poll_interval_ms;  // milliseconds between fetches
};

Config cfg;
const char* CONFIG_FILE = "/config.json";

// ── State ────────────────────────────────────────────────────────────────────
enum Screen { SCREEN_SESSION, SCREEN_WEEKLY, SCREEN_COUNT };
Screen currentScreen = SCREEN_SESSION;

float sessionPct   = 0;
String sessionReset = "--";
float weeklyPct    = 0;
String weeklyReset  = "--";
bool  dataOk       = false;

unsigned long lastFetch = 0;
unsigned long lastBtnNext = 0;
unsigned long lastBtnPrev = 0;
const unsigned long DEBOUNCE_MS = 200;

WebServer server(80);

// ── LittleFS helpers ─────────────────────────────────────────────────────────
bool loadConfig() {
    if (!LittleFS.begin()) return false;
    File f = LittleFS.open(CONFIG_FILE, "r");
    if (!f) return false;
    JsonDocument doc;
    if (deserializeJson(doc, f)) { f.close(); return false; }
    f.close();
    strlcpy(cfg.ssid,           doc["ssid"]     | "", sizeof(cfg.ssid));
    strlcpy(cfg.password,       doc["password"] | "", sizeof(cfg.password));
    strlcpy(cfg.api_url,        doc["api_url"]  | "", sizeof(cfg.api_url));
    strlcpy(cfg.api_token,      doc["api_token"]| "", sizeof(cfg.api_token));
    cfg.poll_interval_ms = doc["poll_interval_ms"] | 60000;
    return strlen(cfg.ssid) > 0;
}

void saveConfig() {
    LittleFS.begin();
    File f = LittleFS.open(CONFIG_FILE, "w");
    if (!f) return;
    JsonDocument doc;
    doc["ssid"]             = cfg.ssid;
    doc["password"]         = cfg.password;
    doc["api_url"]          = cfg.api_url;
    doc["api_token"]        = cfg.api_token;
    doc["poll_interval_ms"] = cfg.poll_interval_ms;
    serializeJson(doc, f);
    f.close();
}

// ── LCD helpers ──────────────────────────────────────────────────────────────
void lcdPrint(const char* line0, const char* line1) {
    lcd.clear();
    lcd.setCursor(0, 0); lcd.print(line0);
    lcd.setCursor(0, 1); lcd.print(line1);
}

void drawScreen() {
    char l0[17], l1[17];
    if (!dataOk) {
        lcdPrint("Aguardando...", "");
        return;
    }
    if (currentScreen == SCREEN_SESSION) {
        snprintf(l0, sizeof(l0), "Sessao: %4.1f%%", sessionPct);
        snprintf(l1, sizeof(l1), "Reset: %s", sessionReset.c_str());
    } else {
        snprintf(l0, sizeof(l0), "Semanal:%4.1f%%", weeklyPct);
        snprintf(l1, sizeof(l1), "%s", weeklyReset.length() > 16 ? weeklyReset.substring(0,16).c_str() : weeklyReset.c_str());
    }
    lcdPrint(l0, l1);
}

// ── HTTP fetch ───────────────────────────────────────────────────────────────
void fetchUsage() {
    if (WiFi.status() != WL_CONNECTED) return;

    HTTPClient http;
    WiFiClientSecure secureClient;
    secureClient.setInsecure();  // skip cert validation — personal/LAN use

    String url = String(cfg.api_url);
    if (url.startsWith("https://")) {
        http.begin(secureClient, url);
    } else {
        http.begin(url);
    }

    http.addHeader("Authorization", String("Bearer ") + cfg.api_token);
    int code = http.GET();
    if (code == 200) {
        JsonDocument doc;
        if (!deserializeJson(doc, http.getString())) {
            sessionPct   = doc["session_pct"]   | 0.0f;
            sessionReset = (const char*)(doc["session_resets_in"] | "--");
            weeklyPct    = doc["weekly_pct"]    | 0.0f;
            weeklyReset  = (const char*)(doc["weekly_resets_at"]  | "--");
            dataOk = true;
        }
    }
    http.end();
}

// ── Setup portal (AP mode) ───────────────────────────────────────────────────
const char SETUP_HTML[] PROGMEM = R"rawliteral(
<!DOCTYPE html><html><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Claude Monitor Setup</title>
<style>
  body{font-family:sans-serif;max-width:420px;margin:40px auto;padding:0 16px}
  h1{font-size:1.2rem;margin-bottom:1.5rem}
  label{display:block;margin-top:12px;font-size:.9rem;color:#555}
  input{width:100%;padding:8px;margin-top:4px;border:1px solid #ccc;border-radius:4px;box-sizing:border-box}
  button{margin-top:20px;width:100%;padding:10px;background:#2563eb;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:1rem}
  button:hover{background:#1d4ed8}
</style></head><body>
<h1>Claude Monitor — Setup</h1>
<form action="/save" method="POST">
  <label>WiFi SSID<input name="ssid" required value="%SSID%"></label>
  <label>WiFi Senha<input name="password" type="password" value="%PASS%"></label>
  <label>URL da API (ex: https://meu-servidor.com/usage)<input name="api_url" required value="%URL%"></label>
  <label>Bearer Token<input name="api_token" required value="%TOKEN%"></label>
  <label>Intervalo de poll (segundos)<input name="poll_interval" type="number" min="10" value="%INTERVAL%"></label>
  <button type="submit">Salvar e Reiniciar</button>
</form></body></html>
)rawliteral";

String buildPage() {
    String page = SETUP_HTML;
    page.replace("%SSID%",     cfg.ssid);
    page.replace("%PASS%",     "");
    page.replace("%URL%",      cfg.api_url);
    page.replace("%TOKEN%",    "");
    page.replace("%INTERVAL%", String(cfg.poll_interval_ms / 1000));
    return page;
}

void handleRoot() { server.send(200, "text/html", buildPage()); }

void handleSave() {
    strlcpy(cfg.ssid,      server.arg("ssid").c_str(),         sizeof(cfg.ssid));
    strlcpy(cfg.password,  server.arg("password").c_str(),     sizeof(cfg.password));
    strlcpy(cfg.api_url,   server.arg("api_url").c_str(),      sizeof(cfg.api_url));
    strlcpy(cfg.api_token, server.arg("api_token").c_str(),    sizeof(cfg.api_token));
    cfg.poll_interval_ms = server.arg("poll_interval").toInt() * 1000;
    saveConfig();
    server.send(200, "text/html",
        "<html><body><p>Configuracao salva! Reiniciando em 3s...</p>"
        "<script>setTimeout(()=>location.href='/',3000)</script></body></html>");
    delay(3000);
    rp2040.reboot();
}

void startAP() {
    lcdPrint("Setup WiFi:", "192.168.4.1");
    WiFi.mode(WIFI_AP);
    WiFi.softAP("ClaudeMonitor", "setup1234");
    server.on("/", handleRoot);
    server.on("/save", HTTP_POST, handleSave);
    server.begin();
    while (true) { server.handleClient(); delay(10); }
}

// ── Main ─────────────────────────────────────────────────────────────────────
void setup() {
    Serial.begin(115200);
    Wire.setSDA(SDA_PIN);
    Wire.setSCL(SCL_PIN);
    Wire.begin();

    // LCD init with I2C address fallback
    if (![]{ Wire.beginTransmission(0x27); return Wire.endTransmission() == 0; }()) {
        lcd = LiquidCrystal_I2C(0x3F, 16, 2);
    }
    lcd.init();
    lcd.backlight();
    lcdPrint("Claude Monitor", "Iniciando...");

    pinMode(BTN_NEXT, INPUT_PULLUP);
    pinMode(BTN_PREV, INPUT_PULLUP);

    if (!loadConfig() || strlen(cfg.ssid) == 0) {
        startAP();  // never returns
    }

    lcdPrint("Conectando WiFi", cfg.ssid);
    WiFi.begin(cfg.ssid, cfg.password);

    unsigned long t = millis();
    while (WiFi.status() != WL_CONNECTED && millis() - t < 15000) {
        delay(500);
    }
    if (WiFi.status() != WL_CONNECTED) {
        lcdPrint("WiFi falhou!", "Modo setup...");
        delay(2000);
        startAP();  // never returns
    }

    lcdPrint("WiFi OK!", WiFi.localIP().toString().c_str());
    delay(1500);
    fetchUsage();
    drawScreen();
}

void loop() {
    unsigned long now = millis();

    // Poll
    if (now - lastFetch >= cfg.poll_interval_ms) {
        lastFetch = now;
        fetchUsage();
        drawScreen();
    }

    // Button: next screen
    if (digitalRead(BTN_NEXT) == LOW && now - lastBtnNext > DEBOUNCE_MS) {
        lastBtnNext = now;
        currentScreen = (Screen)((currentScreen + 1) % SCREEN_COUNT);
        drawScreen();
    }

    // Button: previous screen
    if (digitalRead(BTN_PREV) == LOW && now - lastBtnPrev > DEBOUNCE_MS) {
        lastBtnPrev = now;
        currentScreen = (Screen)((currentScreen + SCREEN_COUNT - 1) % SCREEN_COUNT);
        drawScreen();
    }

    delay(50);
}
