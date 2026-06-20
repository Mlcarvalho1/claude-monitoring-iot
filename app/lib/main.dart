import 'dart:convert';
import 'package:flutter/material.dart';
import 'package:flutter_blue_plus/flutter_blue_plus.dart';

const serviceUuid = "4fafc201-1fb5-459e-8fcc-c5c9c331914b";
const configUuid  = "beb5483e-36e1-4688-b7f5-ea07361b26a8";
const navUuid     = "beb5483e-36e1-4688-b7f5-ea07361b26a9";
const statusUuid  = "beb5483e-36e1-4688-b7f5-ea07361b26aa";

void main() => runApp(const ClaudeMonitorApp());

class ClaudeMonitorApp extends StatelessWidget {
  const ClaudeMonitorApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Claude Monitor',
      debugShowCheckedModeBanner: false,
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(seedColor: const Color(0xFF2563EB)),
        useMaterial3: true,
      ),
      home: const ScanPage(),
    );
  }
}

// ── Scan Page ─────────────────────────────────────────────────────────────────
class ScanPage extends StatefulWidget {
  const ScanPage({super.key});

  @override
  State<ScanPage> createState() => _ScanPageState();
}

class _ScanPageState extends State<ScanPage> {
  final List<ScanResult> _results = [];
  bool _scanning = false;

  void _startScan() async {
    setState(() { _results.clear(); _scanning = true; });
    await FlutterBluePlus.startScan(timeout: const Duration(seconds: 5));
    FlutterBluePlus.scanResults.listen((results) {
      setState(() {
        _results.clear();
        _results.addAll(results.where(
            (r) => r.advertisementData.advName == 'ClaudeMonitor'));
      });
    });
    await Future.delayed(const Duration(seconds: 5));
    setState(() { _scanning = false; });
  }

  void _connect(BluetoothDevice device) async {
    await FlutterBluePlus.stopScan();
    await device.connect();
    if (!mounted) return;
    Navigator.pushReplacement(context,
        MaterialPageRoute(builder: (_) => DevicePage(device: device)));
  }

  @override
  void initState() {
    super.initState();
    _startScan();
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('Claude Monitor'),
        backgroundColor: const Color(0xFF2563EB),
        foregroundColor: Colors.white,
      ),
      body: Column(
        children: [
          if (_scanning) const LinearProgressIndicator(),
          Expanded(
            child: _results.isEmpty
                ? Center(
                    child: Column(
                      mainAxisAlignment: MainAxisAlignment.center,
                      children: [
                        const Icon(Icons.bluetooth_searching,
                            size: 64, color: Colors.grey),
                        const SizedBox(height: 16),
                        Text(
                          _scanning
                              ? 'Procurando ClaudeMonitor...'
                              : 'Nenhum dispositivo encontrado',
                          style: const TextStyle(color: Colors.grey),
                        ),
                      ],
                    ),
                  )
                : ListView.builder(
                    itemCount: _results.length,
                    itemBuilder: (_, i) {
                      final r = _results[i];
                      return ListTile(
                        leading: const Icon(Icons.bluetooth,
                            color: Color(0xFF2563EB)),
                        title: const Text('ClaudeMonitor'),
                        subtitle: Text(r.device.remoteId.str),
                        trailing: const Icon(Icons.chevron_right),
                        onTap: () => _connect(r.device),
                      );
                    },
                  ),
          ),
        ],
      ),
      floatingActionButton: FloatingActionButton.extended(
        onPressed: _scanning ? null : _startScan,
        icon: const Icon(Icons.search),
        label: Text(_scanning ? 'Procurando...' : 'Buscar'),
        backgroundColor: const Color(0xFF2563EB),
        foregroundColor: Colors.white,
      ),
    );
  }
}

// ── Device Page ───────────────────────────────────────────────────────────────
class DevicePage extends StatefulWidget {
  final BluetoothDevice device;
  const DevicePage({super.key, required this.device});

  @override
  State<DevicePage> createState() => _DevicePageState();
}

class _DevicePageState extends State<DevicePage> {
  BluetoothCharacteristic? _configChar;
  BluetoothCharacteristic? _navChar;
  BluetoothCharacteristic? _statusChar;

  double sessionPct = 0;
  double weeklyPct = 0;
  String sessionReset = '--';
  String weeklyReset = '--';
  bool _ready = false;

  @override
  void initState() {
    super.initState();
    _discoverServices();
  }

  Future<void> _discoverServices() async {
    final services = await widget.device.discoverServices();
    for (final s in services) {
      if (s.uuid.str128.toLowerCase() == serviceUuid) {
        for (final c in s.characteristics) {
          final uuid = c.uuid.str128.toLowerCase();
          if (uuid == configUuid) _configChar = c;
          if (uuid == navUuid) _navChar = c;
          if (uuid == statusUuid) _statusChar = c;
        }
      }
    }

    if (_statusChar != null) {
      await _statusChar!.setNotifyValue(true);
      _statusChar!.lastValueStream.listen((value) {
        if (value.isEmpty) return;
        try {
          final data = jsonDecode(utf8.decode(value));
          setState(() {
            sessionPct = (data['session_pct'] ?? 0).toDouble();
            weeklyPct = (data['weekly_pct'] ?? 0).toDouble();
            sessionReset = data['session_resets_in'] ?? '--';
            weeklyReset = data['weekly_resets_at'] ?? '--';
          });
        } catch (_) {}
      });
      await _statusChar!.read();
    }

    setState(() { _ready = true; });
  }

  Future<void> _sendNav(String cmd) async {
    await _navChar?.write(utf8.encode(cmd), withoutResponse: false);
  }

  Future<void> _sendConfig(Map<String, dynamic> cfg) async {
    await _configChar?.write(utf8.encode(jsonEncode(cfg)),
        withoutResponse: false);
  }

  void _disconnect() async {
    await widget.device.disconnect();
    if (!mounted) return;
    Navigator.pushReplacement(context,
        MaterialPageRoute(builder: (_) => const ScanPage()));
  }

  @override
  Widget build(BuildContext context) {
    return DefaultTabController(
      length: 2,
      child: Scaffold(
        appBar: AppBar(
          title: const Text('Claude Monitor'),
          backgroundColor: const Color(0xFF2563EB),
          foregroundColor: Colors.white,
          actions: [
            IconButton(
              icon: const Icon(Icons.settings),
              onPressed: () => Navigator.push(context,
                  MaterialPageRoute(builder: (_) => ConfigPage(onSave: _sendConfig))),
            ),
            IconButton(
                icon: const Icon(Icons.bluetooth_disabled),
                onPressed: _disconnect),
          ],
          bottom: const TabBar(
            labelColor: Colors.white,
            unselectedLabelColor: Colors.white60,
            indicatorColor: Colors.white,
            tabs: [
              Tab(icon: Icon(Icons.monitor_heart), text: 'Monitor'),
              Tab(icon: Icon(Icons.sports_esports), text: 'Jogo'),
            ],
          ),
        ),
        body: !_ready
            ? const Center(child: CircularProgressIndicator())
            : TabBarView(
                children: [
                  // ── Aba Monitor ──
                  Padding(
                    padding: const EdgeInsets.all(24),
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.stretch,
                      children: [
                        _UsageCard(
                          title: 'Sessão atual',
                          pct: sessionPct,
                          subtitle: 'Reset: $sessionReset',
                        ),
                        const SizedBox(height: 16),
                        _UsageCard(
                          title: 'Semanal',
                          pct: weeklyPct,
                          subtitle: weeklyReset,
                        ),
                        const SizedBox(height: 32),
                        const Text('Navegar no display',
                            style: TextStyle(
                                fontWeight: FontWeight.bold,
                                fontSize: 14,
                                color: Colors.grey)),
                        const SizedBox(height: 12),
                        Row(
                          children: [
                            Expanded(
                              child: OutlinedButton.icon(
                                onPressed: () => _sendNav('P'),
                                icon: const Icon(Icons.arrow_back),
                                label: const Text('Anterior'),
                              ),
                            ),
                            const SizedBox(width: 12),
                            Expanded(
                              child: OutlinedButton.icon(
                                onPressed: () => _sendNav('N'),
                                icon: const Icon(Icons.arrow_forward),
                                label: const Text('Próxima'),
                              ),
                            ),
                          ],
                        ),
                      ],
                    ),
                  ),
                  // ── Aba Jogo ──
                  GameTab(onJump: () => _sendNav('J')),
                ],
              ),
      ),
    );
  }
}

class _UsageCard extends StatelessWidget {
  final String title;
  final double pct;
  final String subtitle;

  const _UsageCard(
      {required this.title, required this.pct, required this.subtitle});

  Color get _color {
    if (pct >= 90) return Colors.red;
    if (pct >= 70) return Colors.orange;
    return const Color(0xFF2563EB);
  }

  @override
  Widget build(BuildContext context) {
    return Card(
      elevation: 2,
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(title,
                style: const TextStyle(fontSize: 13, color: Colors.grey)),
            const SizedBox(height: 4),
            Text('${pct.toStringAsFixed(1)}%',
                style: const TextStyle(
                    fontSize: 32, fontWeight: FontWeight.bold)),
            const SizedBox(height: 8),
            LinearProgressIndicator(
              value: pct / 100,
              backgroundColor: Colors.grey.shade200,
              valueColor: AlwaysStoppedAnimation(_color),
              minHeight: 8,
              borderRadius: BorderRadius.circular(4),
            ),
            const SizedBox(height: 6),
            Text(subtitle,
                style:
                    const TextStyle(fontSize: 12, color: Colors.grey)),
          ],
        ),
      ),
    );
  }
}

// ── Game Tab ──────────────────────────────────────────────────────────────────
class GameTab extends StatefulWidget {
  final VoidCallback onJump;
  const GameTab({super.key, required this.onJump});

  @override
  State<GameTab> createState() => _GameTabState();
}

class _GameTabState extends State<GameTab> {
  bool _playing = false;

  void _handleTap() {
    setState(() { _playing = true; });
    widget.onJump();
  }

  @override
  Widget build(BuildContext context) {
    return GestureDetector(
      onTap: _handleTap,
      child: Container(
        color: Colors.black,
        child: Stack(
          children: [
            // instrução no topo
            Positioned(
              top: 24,
              left: 0,
              right: 0,
              child: Text(
                _playing ? 'Toque para pular!' : 'Toque para iniciar',
                textAlign: TextAlign.center,
                style: const TextStyle(
                  color: Colors.white70,
                  fontSize: 14,
                  letterSpacing: 1.2,
                ),
              ),
            ),

            // botão de pulo central
            Center(
              child: Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  Container(
                    width: 120,
                    height: 120,
                    decoration: BoxDecoration(
                      shape: BoxShape.circle,
                      color: const Color(0xFF2563EB).withAlpha(200),
                      border: Border.all(color: Colors.white24, width: 2),
                      boxShadow: [
                        BoxShadow(
                          color: const Color(0xFF2563EB).withAlpha(120),
                          blurRadius: 24,
                          spreadRadius: 4,
                        )
                      ],
                    ),
                    child: const Icon(Icons.keyboard_arrow_up,
                        size: 64, color: Colors.white),
                  ),
                  const SizedBox(height: 16),
                  const Text(
                    'PULAR',
                    style: TextStyle(
                      color: Colors.white,
                      fontSize: 16,
                      fontWeight: FontWeight.bold,
                      letterSpacing: 3,
                    ),
                  ),
                ],
              ),
            ),

            // dica no rodapé
            const Positioned(
              bottom: 24,
              left: 0,
              right: 0,
              child: Text(
                'O jogo roda no display do ESP32',
                textAlign: TextAlign.center,
                style: TextStyle(color: Colors.white38, fontSize: 12),
              ),
            ),
          ],
        ),
      ),
    );
  }
}

// ── Config Page ───────────────────────────────────────────────────────────────
class ConfigPage extends StatefulWidget {
  final Future<void> Function(Map<String, dynamic>) onSave;
  const ConfigPage({super.key, required this.onSave});

  @override
  State<ConfigPage> createState() => _ConfigPageState();
}

class _ConfigPageState extends State<ConfigPage> {
  final _ssid     = TextEditingController();
  final _password = TextEditingController();
  final _apiUrl   = TextEditingController();
  final _apiToken = TextEditingController();
  final _interval = TextEditingController(text: '60');
  bool _saving = false;

  Future<void> _save() async {
    setState(() { _saving = true; });
    await widget.onSave({
      'ssid':             _ssid.text,
      'password':         _password.text,
      'api_url':          _apiUrl.text,
      'api_token':        _apiToken.text,
      'poll_interval_ms': (int.tryParse(_interval.text) ?? 60) * 1000,
    });
    if (!mounted) return;
    ScaffoldMessenger.of(context).showSnackBar(const SnackBar(
        content: Text('Config enviada! Dispositivo vai reiniciar.')));
    Navigator.pop(context);
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('Configuração'),
        backgroundColor: const Color(0xFF2563EB),
        foregroundColor: Colors.white,
      ),
      body: ListView(
        padding: const EdgeInsets.all(24),
        children: [
          _field(_ssid,     'WiFi SSID',          Icons.wifi),
          _field(_password, 'WiFi Senha',          Icons.lock,    obscure: true),
          _field(_apiUrl,   'URL da API',           Icons.link),
          _field(_apiToken, 'Bearer Token',         Icons.vpn_key, obscure: true),
          _field(_interval, 'Intervalo (segundos)', Icons.timer,
              keyboard: TextInputType.number),
          const SizedBox(height: 32),
          FilledButton.icon(
            onPressed: _saving ? null : _save,
            icon: _saving
                ? const SizedBox(
                    width: 18, height: 18,
                    child: CircularProgressIndicator(
                        strokeWidth: 2, color: Colors.white))
                : const Icon(Icons.save),
            label: Text(_saving ? 'Enviando...' : 'Salvar e Reiniciar'),
            style: FilledButton.styleFrom(
                backgroundColor: const Color(0xFF2563EB),
                padding: const EdgeInsets.symmetric(vertical: 14)),
          ),
        ],
      ),
    );
  }

  Widget _field(TextEditingController ctrl, String label, IconData icon,
      {bool obscure = false,
      TextInputType keyboard = TextInputType.text}) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 16),
      child: TextField(
        controller: ctrl,
        obscureText: obscure,
        keyboardType: keyboard,
        decoration: InputDecoration(
          labelText: label,
          prefixIcon: Icon(icon),
          border: const OutlineInputBorder(),
        ),
      ),
    );
  }
}
