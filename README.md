# Claude Monitor

Monitor físico de uso do Claude.ai Pro usando Raspberry Pi Pico 2W + LCD 16x2 I2C.

## Arquitetura

```
claude.ai ←→ [API Docker no Dokploy] ←→ [Pico 2W] → LCD
```

---

## 1. API Docker (Dokploy)

### Pré-requisitos
- Servidor com Docker e Dokploy
- Conta Claude.ai Pro

### Obter as credenciais do claude.ai

**sessionKey:**
1. Abra [claude.ai](https://claude.ai) no browser
2. DevTools → Application → Cookies → `claude.ai`
3. Copie o valor do cookie `sessionKey`

**CLAUDE_ORG_ID:**
- Opção A: DevTools → Network → filtre por `organizations` → copie o UUID da URL
- Opção B: DevTools → Application → Cookies → copie o cookie `lastActiveOrg`

> Os cookies expiram periodicamente. Quando o display parar de atualizar, renove `CLAUDE_SESSION_KEY`.

### Deploy no Dokploy

1. Crie um novo serviço **Docker Compose** apontando para este repositório
2. Configure as variáveis de ambiente:

```env
CLAUDE_SESSION_KEY=sk-ant-sid02-...
CLAUDE_ORG_ID=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
API_BEARER_TOKEN=gere-com-openssl-rand-hex-32
POLL_TIMEOUT=10
```

3. O serviço sobe na porta `8080`

### Testar a API

```bash
# Health check
curl https://seu-dominio.com/health

# Debug — inspeciona quais endpoints o claude.ai usa
curl -H "Authorization: Bearer SEU_TOKEN" https://seu-dominio.com/debug

# Usage
curl -H "Authorization: Bearer SEU_TOKEN" https://seu-dominio.com/usage
```

Resposta esperada:
```json
{
  "session_pct": 3.0,
  "session_resets_in": "4h 33m",
  "weekly_pct": 7.0,
  "weekly_resets_at": "Tue 10:59 PM"
}
```

> Se `/usage` falhar, use `/debug` para ver as respostas brutas do claude.ai e abra uma issue com o resultado.

---

## 2. Firmware Pico 2W

### Hardware necessário

| Componente | Detalhes |
|-----------|---------|
| Raspberry Pi Pico 2W | Com WiFi |
| LCD 16x2 I2C | Endereço 0x27 ou 0x3F |
| 2x botões | GP14 (próximo) e GP15 (anterior) |
| Resistores 10kΩ | Pull-down para os botões (opcional — firmware usa INPUT_PULLUP) |

### Conexão I2C

| LCD | Pico 2W |
|-----|---------|
| SDA | GP4 |
| SCL | GP5 |
| VCC | 3.3V |
| GND | GND |

### Build e flash

1. Instale [PlatformIO](https://platformio.org/)
2. `cd firmware`
3. `pio run --target upload`

### Configuração inicial

No primeiro boot (ou se não houver config salva):

1. O Pico abre um AP WiFi: **ClaudeMonitor** / senha: `setup1234`
2. Conecte ao AP e acesse `http://192.168.4.1`
3. Preencha: SSID, senha WiFi, URL da API, Bearer Token, intervalo (segundos)
4. Salve — o Pico reinicia e conecta

### Navegação pelos botões

| Botão (GP) | Ação |
|-----------|------|
| GP14 | Próxima tela |
| GP15 | Tela anterior |

### Telas disponíveis

| Tela | Linha 0 | Linha 1 |
|------|---------|---------|
| Sessão atual | `Sessao: 3.0%` | `Reset: 4h 33m` |
| Limite semanal | `Semanal: 7.0%` | data/hora do reset |

---

## Troubleshooting

| Sintoma | Causa provável | Solução |
|---------|---------------|---------|
| Display mostra "Aguardando..." | Sem dados ainda | Aguardar o primeiro poll |
| "WiFi falhou!" | SSID/senha errados | Reconectar ao AP e reconfigurar |
| API retorna 502 "session expired" | Cookie expirou | Atualizar `CLAUDE_SESSION_KEY` |
| API retorna 502 "shape not recognized" | claude.ai mudou o endpoint | Usar `/debug` e abrir issue |
