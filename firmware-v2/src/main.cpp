#include <Arduino.h>
#include <U8g2lib.h>
#include <WiFi.h>
#include <WebServer.h>
#include <HTTPClient.h>
#include <WiFiClientSecure.h>
#include <ArduinoJson.h>
#include <LittleFS.h>
#include <NimBLEDevice.h>

// ── Hardware ──────────────────────────────────────────────────────────────────
#define BTN_NEXT    4
#define BTN_PREV    5
#define SDA_PIN     8
#define SCL_PIN     9

U8G2_SSD1306_128X64_NONAME_F_SW_I2C display(U8G2_R0, SCL_PIN, SDA_PIN, U8X8_PIN_NONE);

// ── BLE UUIDs ─────────────────────────────────────────────────────────────────
#define BLE_SERVICE_UUID  "4fafc201-1fb5-459e-8fcc-c5c9c331914b"
#define BLE_CONFIG_UUID   "beb5483e-36e1-4688-b7f5-ea07361b26a8"
#define BLE_NAV_UUID      "beb5483e-36e1-4688-b7f5-ea07361b26a9"
#define BLE_STATUS_UUID   "beb5483e-36e1-4688-b7f5-ea07361b26aa"

// ── Config ────────────────────────────────────────────────────────────────────
struct Config {
    char ssid[64];
    char password[64];
    char api_url[128];
    char api_token[128];
    uint32_t poll_interval_ms;
};

Config cfg;
const char* CONFIG_FILE = "/config.json";

// ── State ─────────────────────────────────────────────────────────────────────
enum Screen { SCREEN_SESSION, SCREEN_WEEKLY, SCREEN_GLUCOSE, SCREEN_IP, SCREEN_COUNT };
Screen currentScreen = SCREEN_SESSION;

float  sessionPct   = 0;
String sessionReset = "--";
float  weeklyPct    = 0;
String weeklyReset  = "--";
bool   dataOk       = false;
int    lastHttpCode = 0;

float  glucoseVal   = 0;
String glucoseTrend = "->";
int    glucoseMin   = 0;
bool   glucoseLow   = false;
bool   glucoseHigh  = false;
bool   glucoseOk    = false;
unsigned long lastGlucoseFetch = 0;
const unsigned long GLUCOSE_INTERVAL_MS = 5UL * 60UL * 1000UL; // 5 minutos

unsigned long lastFetch   = 0;
unsigned long lastBtnNext = 0;
unsigned long lastBtnPrev = 0;
const unsigned long DEBOUNCE_MS = 200;

WebServer server(80);
NimBLECharacteristic* pStatusChar = nullptr;

// ── LittleFS ──────────────────────────────────────────────────────────────────
bool mountFS() {
    if (LittleFS.begin(false)) return true;
    LittleFS.format();
    return LittleFS.begin(false);
}

bool loadConfig() {
    if (!mountFS()) return false;
    File f = LittleFS.open(CONFIG_FILE, "r");
    if (!f) return false;
    JsonDocument doc;
    if (deserializeJson(doc, f)) { f.close(); return false; }
    f.close();
    strlcpy(cfg.ssid,      doc["ssid"]     | "", sizeof(cfg.ssid));
    strlcpy(cfg.password,  doc["password"] | "", sizeof(cfg.password));
    strlcpy(cfg.api_url,   doc["api_url"]  | "", sizeof(cfg.api_url));
    strlcpy(cfg.api_token, doc["api_token"]| "", sizeof(cfg.api_token));
    cfg.poll_interval_ms = doc["poll_interval_ms"] | 60000;
    return strlen(cfg.ssid) > 0;
}

void saveConfig() {
    mountFS();
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

// ── Display ───────────────────────────────────────────────────────────────────
void dispMsg(const char* line0, const char* line1 = "") {
    display.clearBuffer();
    display.setFont(u8g2_font_6x10_tf);
    display.drawStr(0, 10, line0);
    display.drawStr(0, 24, line1);
    display.sendBuffer();
}

void drawBar(int pct, int y, int h = 8) {
    display.drawFrame(0, y, 128, h);
    int fill = 126 * pct / 100;
    if (fill > 0) display.drawBox(1, y + 1, fill, h - 2);
}

void drawScreen() {
    display.clearBuffer();
    display.setFont(u8g2_font_6x10_tf);

    if (currentScreen == SCREEN_GLUCOSE) {
        if (!glucoseOk) {
            display.drawStr(0, 10, "Glicose");
            display.drawStr(0, 24, "Aguardando...");
            display.sendBuffer();
            return;
        }
        char valStr[16];
        snprintf(valStr, sizeof(valStr), "%.0f %s", glucoseVal, glucoseTrend.c_str());
        display.drawStr(0, 10, "Glicose mg/dL");
        display.setFont(u8g2_font_ncenB14_tr);
        display.drawStr(0, 32, valStr);
        display.setFont(u8g2_font_6x10_tf);

        // Barra de range (verde 70-180, vermelho fora)
        display.drawFrame(0, 38, 128, 8);
        // faixa normal (70-180) em destaque
        int lo = 128 * 70 / 400;
        int hi = 128 * 180 / 400;
        display.drawBox(lo, 39, hi - lo, 6);   // zona alvo
        // posição atual
        int pos = constrain((int)(128 * glucoseVal / 400), 0, 127);
        display.setDrawColor(0);
        display.drawBox(pos - 1, 38, 3, 8);
        display.setDrawColor(1);
        display.drawVLine(pos, 38, 8);

        char minStr[24];
        if (glucoseLow)       snprintf(minStr, sizeof(minStr), "BAIXO  ha %d min", glucoseMin);
        else if (glucoseHigh) snprintf(minStr, sizeof(minStr), "ALTO   ha %d min", glucoseMin);
        else                  snprintf(minStr, sizeof(minStr), "Normal  ha %d min", glucoseMin);
        display.drawStr(0, 56, minStr);
        display.sendBuffer();
        return;
    }

    if (currentScreen == SCREEN_IP) {
        display.drawStr(0, 10, "Claude Monitor");
        display.drawStr(0, 24, "IP:");
        display.drawStr(0, 36, WiFi.localIP().toString().c_str());
        display.drawStr(0, 50, cfg.ssid);
        display.sendBuffer();
        return;
    }

    if (!dataOk) {
        if (lastHttpCode != 0) {
            char err[24];
            snprintf(err, sizeof(err), "HTTP %d", lastHttpCode);
            display.drawStr(0, 10, "Erro API:");
            display.drawStr(0, 24, err);
        } else {
            display.drawStr(0, 10, "Aguardando dados...");
        }
        display.sendBuffer();
        return;
    }

    if (currentScreen == SCREEN_SESSION) {
        char pctStr[12];
        snprintf(pctStr, sizeof(pctStr), "%4.1f%%", sessionPct);
        display.drawStr(0, 10, "Sessao atual");
        display.setFont(u8g2_font_ncenB14_tr);
        display.drawStr(0, 30, pctStr);
        display.setFont(u8g2_font_6x10_tf);
        drawBar((int)sessionPct, 36, 8);
        char reset[32];
        snprintf(reset, sizeof(reset), "Reset: %s", sessionReset.c_str());
        display.drawStr(0, 56, reset);
    } else {
        char pctStr[12];
        snprintf(pctStr, sizeof(pctStr), "%4.1f%%", weeklyPct);
        display.drawStr(0, 10, "Semanal");
        display.setFont(u8g2_font_ncenB14_tr);
        display.drawStr(0, 30, pctStr);
        display.setFont(u8g2_font_6x10_tf);
        drawBar((int)weeklyPct, 36, 8);
        String wr = weeklyReset.length() > 21 ? weeklyReset.substring(0, 21) : weeklyReset;
        display.drawStr(0, 56, wr.c_str());
    }

    display.sendBuffer();
}

// ── HTTP fetch ────────────────────────────────────────────────────────────────
void fetchGlucose() {
    if (WiFi.status() != WL_CONNECTED) return;

    // Deriva URL de glicose a partir da api_url (troca /usage por /glucose)
    String url = String(cfg.api_url);
    int idx = url.lastIndexOf("/usage");
    if (idx < 0) return;
    url = url.substring(0, idx) + "/glucose";

    HTTPClient http;
    WiFiClientSecure secureClient;
    secureClient.setInsecure();

    if (url.startsWith("https://"))
        http.begin(secureClient, url);
    else
        http.begin(url);

    http.addHeader("Authorization", String("Bearer ") + cfg.api_token);
    int code = http.GET();
    if (code == 200) {
        JsonDocument doc;
        if (!deserializeJson(doc, http.getString())) {
            glucoseVal   = doc["value"]       | 0.0f;
            glucoseTrend = (const char*)(doc["trend_icon"] | "->");
            glucoseMin   = doc["minutes_ago"] | 0;
            glucoseLow   = doc["low"]         | false;
            glucoseHigh  = doc["high"]        | false;
            glucoseOk    = true;
        }
    }
    http.end();
}

void fetchUsage() {
    if (WiFi.status() != WL_CONNECTED) return;

    HTTPClient http;
    WiFiClientSecure secureClient;
    secureClient.setInsecure();

    String url = String(cfg.api_url);
    if (url.startsWith("https://"))
        http.begin(secureClient, url);
    else
        http.begin(url);

    http.addHeader("Authorization", String("Bearer ") + cfg.api_token);
    int code = http.GET();
    lastHttpCode = code;
    if (code == 200) {
        JsonDocument doc;
        if (!deserializeJson(doc, http.getString())) {
            sessionPct   = doc["session_pct"]       | 0.0f;
            sessionReset = (const char*)(doc["session_resets_in"] | "--");
            weeklyPct    = doc["weekly_pct"]         | 0.0f;
            weeklyReset  = (const char*)(doc["weekly_resets_at"]  | "--");
            dataOk = true;
        }
    }
    http.end();

    if (pStatusChar && dataOk) {
        JsonDocument doc;
        doc["session_pct"]       = sessionPct;
        doc["weekly_pct"]        = weeklyPct;
        doc["session_resets_in"] = sessionReset;
        doc["weekly_resets_at"]  = weeklyReset;
        String out;
        serializeJson(doc, out);
        pStatusChar->setValue(out.c_str());
        pStatusChar->notify();
    }
}

// ── BLE callbacks ─────────────────────────────────────────────────────────────
class ConfigCallback : public NimBLECharacteristicCallbacks {
    void onWrite(NimBLECharacteristic* pChar) override {
        std::string val = pChar->getValue();
        JsonDocument doc;
        if (deserializeJson(doc, val.c_str())) return;

        if (doc["ssid"].is<const char*>())
            strlcpy(cfg.ssid, doc["ssid"], sizeof(cfg.ssid));
        if (doc["password"].is<const char*>())
            strlcpy(cfg.password, doc["password"], sizeof(cfg.password));
        if (doc["api_url"].is<const char*>())
            strlcpy(cfg.api_url, doc["api_url"], sizeof(cfg.api_url));
        if (doc["api_token"].is<const char*>())
            strlcpy(cfg.api_token, doc["api_token"], sizeof(cfg.api_token));
        if (doc["poll_interval_ms"].is<uint32_t>())
            cfg.poll_interval_ms = doc["poll_interval_ms"];

        saveConfig();
        dispMsg("Config salva!", "Reiniciando...");
        delay(2000);
        ESP.restart();
    }
};

class NavCallback : public NimBLECharacteristicCallbacks {
    void onWrite(NimBLECharacteristic* pChar) override {
        std::string val = pChar->getValue();
        if (val == "N") {
            currentScreen = (Screen)((currentScreen + 1) % SCREEN_COUNT);
            drawScreen();
        } else if (val == "P") {
            currentScreen = (Screen)((currentScreen + SCREEN_COUNT - 1) % SCREEN_COUNT);
            drawScreen();
        }
    }
};

void setupBLE() {
    NimBLEDevice::init("ClaudeMonitor");
    NimBLEServer*  pServer  = NimBLEDevice::createServer();
    NimBLEService* pService = pServer->createService(BLE_SERVICE_UUID);

    NimBLECharacteristic* pConfigChar = pService->createCharacteristic(
        BLE_CONFIG_UUID, NIMBLE_PROPERTY::WRITE);
    pConfigChar->setCallbacks(new ConfigCallback());

    NimBLECharacteristic* pNavChar = pService->createCharacteristic(
        BLE_NAV_UUID, NIMBLE_PROPERTY::WRITE);
    pNavChar->setCallbacks(new NavCallback());

    pStatusChar = pService->createCharacteristic(
        BLE_STATUS_UUID, NIMBLE_PROPERTY::READ | NIMBLE_PROPERTY::NOTIFY);

    pService->start();
    NimBLEAdvertising* pAdv = NimBLEDevice::getAdvertising();
    pAdv->addServiceUUID(BLE_SERVICE_UUID);
    pAdv->start();
}

// ── Setup portal (AP fallback) ────────────────────────────────────────────────
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
  <label>WiFi Senha<input name="password" type="password"></label>
  <label>URL da API<input name="api_url" required value="%URL%"></label>
  <label>Bearer Token<input name="api_token" required></label>
  <label>Intervalo poll (segundos)<input name="poll_interval" type="number" min="10" value="%INTERVAL%"></label>
  <button type="submit">Salvar e Reiniciar</button>
</form></body></html>
)rawliteral";

String buildPage() {
    String page = SETUP_HTML;
    page.replace("%SSID%",     cfg.ssid);
    page.replace("%URL%",      cfg.api_url);
    page.replace("%INTERVAL%", String(cfg.poll_interval_ms / 1000));
    return page;
}

void handleRoot() { server.send(200, "text/html", buildPage()); }

void handleSave() {
    strlcpy(cfg.ssid,      server.arg("ssid").c_str(),      sizeof(cfg.ssid));
    strlcpy(cfg.password,  server.arg("password").c_str(),  sizeof(cfg.password));
    strlcpy(cfg.api_url,   server.arg("api_url").c_str(),   sizeof(cfg.api_url));
    strlcpy(cfg.api_token, server.arg("api_token").c_str(), sizeof(cfg.api_token));
    cfg.poll_interval_ms = server.arg("poll_interval").toInt() * 1000;
    saveConfig();
    server.send(200, "text/html",
        "<html><body><p>Salvo! Reiniciando em 3s...</p>"
        "<script>setTimeout(()=>location.href='/',3000)</script></body></html>");
    delay(3000);
    ESP.restart();
}

void startAP() {
    dispMsg("Sem config", "BLE: ClaudeMonitor");
    WiFi.mode(WIFI_AP);
    WiFi.softAP("ClaudeMonitor", "setup1234");
    server.on("/",     handleRoot);
    server.on("/save", HTTP_POST, handleSave);
    server.begin();
    while (true) { server.handleClient(); delay(10); }
}

// ── Main ──────────────────────────────────────────────────────────────────────
void setup() {
    display.begin();
    dispMsg("Claude Monitor", "Iniciando...");

    pinMode(BTN_NEXT, INPUT_PULLUP);
    pinMode(BTN_PREV, INPUT_PULLUP);

    setupBLE();
    dispMsg("Claude Monitor", "BLE ativo");

    if (!loadConfig() || strlen(cfg.ssid) == 0) {
        startAP();
    }

    dispMsg("Conectando WiFi", cfg.ssid);
    WiFi.begin(cfg.ssid, cfg.password);

    unsigned long t = millis();
    while (WiFi.status() != WL_CONNECTED && millis() - t < 15000)
        delay(500);

    if (WiFi.status() != WL_CONNECTED) {
        dispMsg("WiFi falhou!", "Modo AP...");
        delay(2000);
        startAP();
    }

    dispMsg("WiFi OK!", WiFi.localIP().toString().c_str());
    delay(1500);

    server.on("/",     handleRoot);
    server.on("/save", HTTP_POST, handleSave);
    server.begin();

    fetchUsage();
    drawScreen();
}

void loop() {
    server.handleClient();

    unsigned long now = millis();

    if (now - lastFetch >= cfg.poll_interval_ms) {
        lastFetch = now;
        fetchUsage();
        if (currentScreen != SCREEN_GLUCOSE) drawScreen();
    }

    if (now - lastGlucoseFetch >= GLUCOSE_INTERVAL_MS) {
        lastGlucoseFetch = now;
        fetchGlucose();
        if (currentScreen == SCREEN_GLUCOSE) drawScreen();
    }

    if (digitalRead(BTN_NEXT) == LOW && now - lastBtnNext > DEBOUNCE_MS) {
        lastBtnNext = now;
        currentScreen = (Screen)((currentScreen + 1) % SCREEN_COUNT);
        drawScreen();
    }

    if (digitalRead(BTN_PREV) == LOW && now - lastBtnPrev > DEBOUNCE_MS) {
        lastBtnPrev = now;
        currentScreen = (Screen)((currentScreen + SCREEN_COUNT - 1) % SCREEN_COUNT);
        drawScreen();
    }

    delay(50);
}
