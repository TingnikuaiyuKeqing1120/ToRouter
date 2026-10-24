"""
Automated real-process screenshot generator for README documentation.

Starts a temporary isolated ToRouter.exe daemon with synthetic test fixtures
(deterministic channels, models, and usage logs), launches headless Edge via CDP,
captures pixel-perfect real UI screenshots, and saves them to docs/assets/readme/.
Zero external packages required (Node.js built-in WebSocket & fetch).
"""
import hashlib
import json
import os
import shutil
import sqlite3
import subprocess
import time
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
OUT_DIR = ROOT / "docs/assets/readme"
OUT_DIR.mkdir(parents=True, exist_ok=True)
TEMP_DIR = ROOT / ".local/readme-shots"
TEMP_DIR.mkdir(parents=True, exist_ok=True)
DATA_DIR = TEMP_DIR / "data"
PROFILE_DIR = TEMP_DIR / "edge-profile"

EXE = str(ROOT / "ToRouter.exe")
EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
PORT = 19456
CDP_PORT = 9333
ORIGIN = f"http://127.0.0.1:{PORT}"


def cleanup():
    for d in (DATA_DIR, PROFILE_DIR):
        if d.exists():
            try:
                shutil.rmtree(d)
            except Exception:
                pass


def seed_fixtures():
    cleanup()
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    PROFILE_DIR.mkdir(parents=True, exist_ok=True)

    # 1. Initialize DB and master.key
    key_proc = subprocess.run([EXE, "-data", str(DATA_DIR), "-mode", "cli", "-show-key"],
                              capture_output=True, text=True, check=True)
    admin_key = key_proc.stdout.strip()

    # 2. Seed realistic usage logs into SQLite
    db_path = DATA_DIR / "router.db"
    local = timezone(timedelta(hours=8))
    local_now = datetime.now(local)
    today = local_now.replace(hour=0, minute=0, second=0, microsecond=0)
    profiles = [
        ('coding-agent', 2, 'Claude 官方通道', 42000, .78, 1800),
        ('default-reasoning', 1, 'DeepSeek 官方直连', 96000, .88, 4200),
        ('fast-chat', 4, 'Google Gemini 免费通道', 6500, .40, 950),
        ('review-expert', 3, 'OpenAI / Codex 团队', 26000, .65, 1500),
    ]
    rows = []
    for offset in range(14):
        log_time = today - timedelta(days=13 - offset) + timedelta(hours=14)
        if log_time > local_now:
            log_time = local_now - timedelta(minutes=10)
        for index, (model, ch_id, ch_name, in_size, hit, out_size) in enumerate(profiles):
            count = 6 + ((offset * 11 + index * 5) % 18)
            for serial in range(count):
                total = in_size + ((serial * 713 + offset * 29) % max(1, in_size // 4))
                read = int(total * hit)
                write = int((total - read) * 0.15)
                plain = total - read - write
                output = out_size + serial * 15
                actual_rates = {'input': 0.8, 'output': 3.2, 'cache_read': 0.08, 'cache_write': 1.0}
                official_rates = {'input': 2.0, 'output': 8.0, 'cache_read': 0.2, 'cache_write': 2.5}
                quantities = {'input': plain, 'output': output, 'cache_read': read, 'cache_write': write}
                actual_cost = sum(quantities[k] * actual_rates[k] / 1_000_000 for k in quantities)
                official_val = sum(quantities[k] * official_rates[k] / 1_000_000 for k in quantities)
                record = {
                    'time': log_time.isoformat(),
                    'channel_id': ch_id,
                    'channel_name': ch_name,
                    'model': model,
                    'upstream_model': model + '-upstream',
                    'input_tokens': plain,
                    'output_tokens': output,
                    'cache_read_tokens': read,
                    'cache_write_tokens': write,
                    'estimated': False,
                    'status': 200,
                    'duration_ms': 850 + serial * 25,
                    'ttft_ms': 190,
                    'currency': 'CNY',
                    'official_currency': 'USD',
                    'actual_cost': actual_cost,
                    'official_value': official_val,
                    'actual_pricing': {'method': 'calculated', 'currency': 'CNY', 'unit_tokens': 1_000_000, 'rates': actual_rates},
                    'official_pricing': {'method': 'calculated', 'currency': 'USD', 'unit_tokens': 1_000_000, 'rates': official_rates}
                }
                rows.append((int(log_time.timestamp() * 1000), ch_id, model, 200, json.dumps(record, ensure_ascii=False)))

    with sqlite3.connect(db_path) as db:
        db.executemany('INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,?,?,?,?)', rows)
        rate = {
            'available': True, 'base': 'USD', 'quote': 'CNY', 'rate': 7.23,
            'date': local_now.date().isoformat(), 'attempted_on': local_now.date().isoformat(),
            'checked_at': local_now.isoformat(), 'fetched_at': local_now.isoformat(),
            'stale': False, 'error': '', 'source_name': '实时基准汇率', 'source_id': 'fixture'
        }
        db.execute("INSERT OR REPLACE INTO meta(key,value) VALUES('exchange_usd_cny_v1',?)", (json.dumps(rate),))
        pref = {'theme': 'dark', 'paused': False, 'revision': 1, 'quota_targets': []}
        db.execute("INSERT OR REPLACE INTO meta(key, value) VALUES('companion_preferences', ?)", (json.dumps(pref),))
        db.execute("UPDATE settings SET data=json_set(data,'$.quota_refresh_seconds',0) WHERE id=1")
        db.commit()

    # 3. Boot daemon and configure channels and models via API
    proc = subprocess.Popen([EXE, "-data", str(DATA_DIR), "-mode", "daemon", "-port", str(PORT)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(50):
            try:
                with urllib.request.urlopen(f"{ORIGIN}/api/health", timeout=1):
                    break
            except urllib.error.HTTPError as e:
                if e.code in (200, 401):
                    break
            except Exception:
                time.sleep(0.1)

        login_req = urllib.request.Request(f"{ORIGIN}/api/auth/login", method="POST",
                                          data=json.dumps({"key": admin_key, "remember": True}).encode(),
                                          headers={"Content-Type": "application/json", "X-To-Router": "1"})
        with urllib.request.urlopen(login_req) as res:
            cookie = res.headers.get("Set-Cookie", "")
        session_cookie = cookie.split(";")[0]
        H = {"Cookie": session_cookie, "Content-Type": "application/json", "X-To-Router": "1"}

        def add_ch(name, proto, url, models):
            formats = {"openai": "openai_chat_completions", "anthropic": "anthropic_messages",
                       "gemini": "gemini_generate_content", "responses": "openai_responses"}
            payload = {
                "name": name,
                "api_key": "sk-test-mock-key",
                "endpoints": [{"format": formats[proto], "base_url": url}],
                "models": models,
                "enabled": True
            }
            r = urllib.request.Request(f"{ORIGIN}/api/channels", method="POST",
                                       data=json.dumps(payload).encode(), headers=H)
            with urllib.request.urlopen(r) as resp:
                return json.loads(resp.read().decode())["id"]

        c1 = add_ch("DeepSeek 官方直连", "openai", "https://api.deepseek.com/v1", ["deepseek-chat", "deepseek-reasoner"])
        c2 = add_ch("Claude 官方通道", "anthropic", "https://api.anthropic.com", ["claude-3-7-sonnet-20250219", "claude-3-5-haiku-20241022"])
        c3 = add_ch("OpenAI / Codex 团队", "responses", "https://chatgpt.com/backend-api/codex", ["gpt-4o", "o3-mini", "codex-preview"])
        c4 = add_ch("Google Gemini 免费通道", "gemini", "https://generativelanguage.googleapis.com", ["gemini-2.0-flash"])

        def add_m(mid, bindings):
            payload = {
                "id": mid,
                "enabled": True,
                "fallback": True,
                "bindings": [{"channel_id": cid, "model": m, "priority": p, "enabled": True} for cid, m, p in bindings]
            }
            r = urllib.request.Request(f"{ORIGIN}/api/models", method="POST",
                                       data=json.dumps(payload).encode(), headers=H)
            with urllib.request.urlopen(r):
                pass

        add_m("default-reasoning", [(c1, "deepseek-reasoner", 0), (c3, "o3-mini", 1)])
        add_m("coding-agent", [(c2, "claude-3-7-sonnet-20250219", 0), (c3, "gpt-4o", 1)])
        add_m("fast-chat", [(c4, "gemini-2.0-flash", 0)])

        k_req = urllib.request.Request(f"{ORIGIN}/api/client-keys", method="POST",
                                       data=json.dumps({"name": "Claude Code & NextChat"}).encode(), headers=H)
        with urllib.request.urlopen(k_req):
            pass

    finally:
        proc.terminate()
        proc.wait()


def capture_all():
    # Write runner Node script
    node_script = TEMP_DIR / "capture.mjs"
    node_script.write_text(f"""import fs from 'node:fs';
import path from 'node:path';

const cdpUrl = 'http://127.0.0.1:{CDP_PORT}';
const origin = '{ORIGIN}';
const outDir = path.resolve('{OUT_DIR.as_posix()}');

async function createTab() {{
  const res = await fetch(`${{cdpUrl}}/json/new?about:blank`, {{ method: 'PUT' }});
  return await res.json();
}}

async function closeTab(id) {{
  try {{ await fetch(`${{cdpUrl}}/json/close/${{id}}`); }} catch {{}}
}}

async function capture(tab, hash, filename, width = 1280, height = 800, options = {{}}) {{
  console.log(`[Capture] Navigating to ${{origin}}/${{hash}} -> ${{filename}}...`);
  const ws = new WebSocket(tab.webSocketDebuggerUrl);

  let msgId = 1;
  const pending = new Map();

  function send(method, params = {{}}) {{
    return new Promise((resolve, reject) => {{
      const id = msgId++;
      pending.set(id, {{ resolve, reject }});
      ws.send(JSON.stringify({{ id, method, params }}));
    }});
  }}

  await new Promise(r => ws.onopen = r);

  ws.onmessage = (event) => {{
    const data = JSON.parse(event.data);
    if (data.id && pending.has(data.id)) {{
      const {{ resolve, reject }} = pending.get(data.id);
      pending.delete(data.id);
      if (data.error) reject(data.error);
      else resolve(data.result);
    }}
  }};

  await send('Page.enable');
  await send('DOM.enable');
  await send('Runtime.enable');
  await send('Emulation.setDeviceMetricsOverride', {{
    width,
    height,
    deviceScaleFactor: 1.5,
    mobile: false
  }});

  await send('Page.navigate', {{ url: `${{origin}}/${{hash}}` }});
  await new Promise(r => setTimeout(r, options.waitMs || 2000));

  if (options.evalBefore) {{
    await send('Runtime.evaluate', {{ expression: options.evalBefore }});
    await new Promise(r => setTimeout(r, 600));
  }}

  const {{ data }} = await send('Page.captureScreenshot', {{ format: 'png' }});
  const targetPath = path.join(outDir, filename);
  fs.writeFileSync(targetPath, Buffer.from(data, 'base64'));
  console.log(`[Capture] Wrote ${{targetPath}} (${{data.length}} chars base64)`);

  ws.close();
}}

async function main() {{
  const tab = await createTab();
  try {{
    await capture(tab, '#/models', 'hero-overview.png', 1366, 840, {{
      waitMs: 2000,
      evalBefore: "document.querySelectorAll('details').forEach(d => d.open = true);"
    }});
    await capture(tab, '#/channels', 'scene-channels.png', 1280, 880, {{ waitMs: 2000 }});
    await capture(tab, '#/models', 'scene-models-routing.png', 1280, 850, {{
      waitMs: 2000,
      evalBefore: "document.querySelectorAll('details').forEach(d => d.open = true);"
    }});
    await capture(tab, '#/usage', 'scene-usage-ledger.png', 1280, 850, {{ waitMs: 2500 }});
    await capture(tab, '#/pet', 'scene-pet-companion.png', 460, 480, {{
      waitMs: 1500,
      evalBefore: `
        const btn = document.querySelector('button[aria-label="打开控制坞"]');
        if (btn) btn.click();
        else {{
          const mascot = document.querySelector('.relative.w-\\\\[240px\\\\]') || document.querySelector('svg');
          if (mascot) mascot.click();
        }}
      `
    }});
    console.log('All real screenshots rendered successfully!');
  }} finally {{
    await closeTab(tab.id);
  }}
}}

main().catch(err => {{
  console.error('Capture failed:', err);
  process.exit(1);
}});
""", encoding='utf-8')

    router_proc = subprocess.Popen([EXE, "-data", str(DATA_DIR), "-mode", "daemon", "-port", str(PORT)],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    edge_proc = subprocess.Popen([
        EDGE,
        "--headless=new",
        f"--user-data-dir={PROFILE_DIR}",
        f"--remote-debugging-port={CDP_PORT}",
        "--no-first-run",
        "--no-default-browser-check",
        "about:blank"
    ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    try:
        time.sleep(2)
        subprocess.run(["node", str(node_script)], check=True)
    finally:
        edge_proc.terminate()
        router_proc.terminate()
        edge_proc.wait()
        router_proc.wait()


if __name__ == "__main__":
    print("1. Seeding test fixtures...")
    seed_fixtures()
    print("2. Capturing real application screenshots...")
    capture_all()
    print("3. Done!")
