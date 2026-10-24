#!/usr/bin/env python3
"""Real Windows single-EXE upgrade and rollback, with disposable signed releases.

Only synthetic data and keys are used. No vendor calls or production releases.
Requires current web/dist (run build.ps1 first). Evidence remains under .local.
"""
import argparse
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import shutil
import subprocess
import threading
import time
import traceback

from acceptance_restart import Gateway, NO_WINDOW, call, require, powershell
from acceptance_ui_lifecycle import visible, windows_for

REPO = Path(__file__).resolve().parents[1]
POWERSHELL = shutil.which('pwsh') or shutil.which('powershell')


def run(argv, log, timeout=180):
    with log.open('wb') as output:
        result = subprocess.run(argv, cwd=REPO, stdout=output, stderr=subprocess.STDOUT,
                                creationflags=NO_WINDOW, timeout=timeout)
    require(result.returncode == 0, 'fixture command failed; see isolated build log')


def until(test, label, timeout=100):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            value = test()
            if value:
                return value
        except (ConnectionError, OSError, TimeoutError, ValueError):
            pass
        time.sleep(.1)
    raise AssertionError(label)


class Feed(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.path == '/feed.json':
            body = self.server.manifest.read_bytes()
            if self.server.bad_signature:
                value = json.loads(body)
                value['signature'] = base64.b64encode(bytes(64)).decode()
                body = json.dumps(value).encode()
        elif self.path == '/ToRouter.exe':
            body = self.server.artifact.read_bytes()
            if self.server.tamper:
                body = bytes([body[0] ^ 1]) + body[1:]
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (ConnectionError, OSError):
            pass


def mcp_proxy(gateway):
    status, _, body = gateway.request('/api/mcp/connections', {'name': 'Synthetic update observer', 'permissions': ['read']}, 'POST')
    require(status == 201, 'cannot create isolated management connection')
    env = dict(os.environ, TOROUTER_MCP_URL=gateway.origin+'/mcp', TOROUTER_MCP_TOKEN=json.loads(body)['token'])
    proc = subprocess.Popen([str(gateway.exe), 'mcp'], env=env, stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, creationflags=NO_WINDOW)
    lines = queue.Queue()
    def read():
        for line in iter(proc.stdout.readline, b''):
            lines.put(line)
    threading.Thread(target=read, daemon=True).start()
    request = {'jsonrpc':'2.0','id':1,'method':'initialize','params':{'protocolVersion':'2025-03-26','capabilities':{},'clientInfo':{'name':'synthetic-update-check','version':'1'}}}
    proc.stdin.write(json.dumps(request).encode()+b'\n')
    proc.stdin.flush()
    response = json.loads(lines.get(timeout=15))
    require(response.get('id') == 1 and 'result' in response, 'MCP proxy did not initialize')
    proc.stdin.write(b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n')
    proc.stdin.flush()
    return proc


def native_processes(exe, data, terminate=False):
    quote = lambda value: "'" + str(value).replace("'", "''") + "'"
    script = "$fixtureExe="+quote(exe)+"; $fixtureData="+quote(data)+"; "
    script += ("$owned=@(Get-CimInstance Win32_Process -Filter \"Name='ToRouter.exe'\" | Where-Object { "
               "$_.ExecutablePath -eq $fixtureExe -and $_.CommandLine -and "
               "$_.CommandLine.IndexOf($fixtureData,[StringComparison]::OrdinalIgnoreCase) -ge 0 -and "
               "$_.CommandLine -match '(?:^|\\s)-mode\\s+(?:desktop|pet)(?:\\s|$)' }); ")
    if terminate:
        script += "$owned | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }; "
    script += "ConvertTo-Json -Compress -InputObject @($owned | Select-Object ProcessId)"
    return json.loads(powershell(script) or '[]')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--evidence-dir', type=Path, required=True)
    args = parser.parse_args()
    require(os.name == 'nt', 'Windows acceptance required')
    root = args.evidence_dir.resolve()
    require(root.is_relative_to(REPO/'.local'), 'evidence must be under the ignored project .local directory')
    require(not root.exists(), 'use a new evidence directory')
    root.mkdir(parents=True)
    os.environ['WEBVIEW2_USER_DATA_FOLDER'] = str(root/'webview')
    keys = root/'keys.go'
    keys.write_text('''package main
import("crypto/ed25519";"crypto/rand";"encoding/base64";"os";"path/filepath")
func main(){p,k,e:=ed25519.GenerateKey(rand.Reader);if e!=nil{panic(e)};d:=os.Args[1];if e=os.WriteFile(filepath.Join(d,"private.key"),[]byte(base64.StdEncoding.EncodeToString(k)),0600);e!=nil{panic(e)};if e=os.WriteFile(filepath.Join(d,"public.key"),[]byte(base64.StdEncoding.EncodeToString(p)),0600);e!=nil{panic(e)}}
''', encoding='utf-8')
    run(['go','run',str(keys),str(root)], root/'keys.log')
    public = (root/'public.key').read_text()
    feed = ThreadingHTTPServer(('127.0.0.1',0), Feed)
    feed.manifest = root/'release.json'
    feed.artifact = root/'v2'/'ToRouter.exe'
    feed.bad_signature = False
    feed.tamper = False
    feed_url = f'http://127.0.0.1:{feed.server_port}'
    threading.Thread(target=feed.serve_forever, daemon=True).start()
    gateway = None
    followers = []
    results = {}
    try:
        for folder,version in [('v1','0.0.1'),('v2','0.0.2')]:
            require(POWERSHELL, 'PowerShell is required for the isolated build')
            run([POWERSHELL,'-NoProfile','-File',str(REPO/'build.ps1'),'-UsePrebuiltWeb','-OutputDir',str(root/folder),'-Version',version,'-UpdateFeedURL',feed_url+'/feed.json','-UpdatePublicKey',public], root/(folder+'-build.log'))
        notes = root/'notes.txt'
        notes.write_text('Synthetic signed release for isolated update acceptance.', encoding='utf-8')
        def sign(version, artifact, name):
            dest = root/name
            run(['go','run','./scripts/release-manifest','-exe',str(artifact),'-version',version,'-url',feed_url+'/ToRouter.exe','-key',str(root/'private.key'),'-notes',str(notes),'-output',str(dest)], root/(name+'.log'))
            feed.artifact = artifact
            feed.manifest = dest
            return json.loads(dest.read_text())['payload']
        release = sign('0.0.2', root/'v2'/'ToRouter.exe', 'release-good.json')
        binaries = root/'running'
        binaries.mkdir()
        shutil.copy2(root/'v1'/'ToRouter.exe', binaries/'ToRouter.exe')
        require([p.name for p in binaries.iterdir()] == ['ToRouter.exe'], 'initial executable directory contains extra binaries')
        data = root/'data'
        data.mkdir()
        gateway = Gateway(binaries, data, True, 90)
        gateway.ready(gateway.port, starting=True)
        _,_,body = gateway.request('/api/settings')
        settings = json.loads(body)
        settings.pop('prices',None)
        settings['default_launch_mode'] = 'desktop'
        settings['max_attempts'] = 4
        results['phase'] = 'seed_settings'
        require(gateway.request('/api/settings',settings,'PUT')[0] == 200, 'cannot seed launch preference')
        channel = {'name':'Synthetic retained channel','base_url':'https://example.invalid/v1','endpoints':[{'format':'openai_chat_completions','base_url':'https://example.invalid/v1'}], 'api_key':'synthetic-update-credential','models':['synthetic-model'],'enabled':True}
        results['phase'] = 'seed_channel'
        require(gateway.request('/api/channels',channel,'POST')[0] == 200, 'cannot seed synthetic channel')
        feed.bad_signature = True
        results['phase'] = 'reject_bad_signature'
        require(gateway.request('/api/app-updates/check',{},'POST')[0] == 502, 'forged release was accepted')
        require(gateway.parent.poll() is None, 'check failure stopped gateway')
        feed.bad_signature = False
        results['phase'] = 'check_good_signature'
        require(gateway.request('/api/app-updates/check',{},'POST')[0] == 200, 'signed release was not accepted')
        feed.tamper = True
        results['phase'] = 'reject_tampered_download'
        require(call(gateway.origin,'/api/app-updates/install',{'version':'0.0.2','sha256':release['sha256']},'POST',gateway.headers,timeout=60)[0] == 409, 'tampered download was accepted')
        require(gateway.parent.poll() is None, 'download failure stopped gateway')
        feed.tamper = False
        results['invalid_signature_and_download_preserve_service'] = True
        cli = subprocess.Popen([str(gateway.exe),'-mode','cli','-data',str(data),'-port',str(gateway.port)],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,creationflags=NO_WINDOW)
        followers.append(cli)
        results['phase'] = 'mcp_follower'
        proxy = mcp_proxy(gateway)
        followers.append(proxy)
        for mode in ('desktop','pet'):
            proc = subprocess.Popen([str(gateway.exe),'-mode',mode,'-data',str(data),'-port',str(gateway.port)],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,creationflags=NO_WINDOW)
            followers.append(proc)
            until(lambda: visible(proc), 'native '+mode+' did not become visible', 20)
        require(cli.poll() is None and proxy.poll() is None, 'followers did not stay active before upgrade')
        results['phase'] = 'install_good_release'
        status,_,body = call(gateway.origin,'/api/app-updates/install',{'version':'0.0.2','sha256':release['sha256'],'open_mode':'desktop'},'POST',gateway.headers,timeout=60)
        require(status == 202 and json.loads(body)['requested'], 'update was not accepted')
        gateway.parent.wait(timeout=30)
        def outcome(version,state):
            status,_,body=call(gateway.origin,'/_console/ready',timeout=1)
            if status!=200 or json.loads(body).get('version')!=version: return None
            status,_,body=gateway.request('/api/app-updates')
            if status!=200: return None
            value=json.loads(body)
            return value if value.get('last_result',{}).get('state')==state else None
        success = until(lambda: outcome('0.0.2','updated'), 'actual signed EXE update failed')
        require(all(proc.poll() is not None for proc in followers), 'old view or MCP process still holds previous executable')
        def restored_views():
            current = native_processes(gateway.exe, data)
            observations = [{'pid':proc['ProcessId'], 'visible_windows':windows_for(proc['ProcessId'])} for proc in current]
            results['native_observation'] = observations
            return len(current) == 2 and all(len(item['visible_windows']) == 1 for item in observations)
        until(restored_views, 'Desktop and pet were not restored after update', 25)
        results['upgrade'] = {'from':'0.0.1','to':'0.0.2','result':success['last_result'],'old_parent_exited':True,'cli_exited':cli.poll() is not None,'mcp_exited':proxy.poll() is not None}
        results['upgrade']['desktop_and_pet_restored'] = True
        _,_,body = gateway.request('/api/settings')
        kept = json.loads(body)
        require(kept['default_launch_mode']=='desktop' and kept['max_attempts']==4, 'update lost settings')
        _,_,body=gateway.request('/api/channels')
        require(len(json.loads(body))==1, 'update lost synthetic channel')
        key_before=(data/'master.key').read_bytes()
        bad=root/'bad-release.go'
        bad.write_text('''package main
import("os";"path/filepath";"torouter/internal/store";"torouter/internal/updater")
func main(){a:=os.Args;d:="";for i:=1;i<len(a)-1;i++{if a[i]=="-data"{d=a[i+1]}};if d==""{os.Exit(2)};if e:=updater.CheckLaunch(d);e!=nil{os.Exit(3)};s,e:=store.Open(d);if e!=nil{os.Exit(4)};v,e:=s.GetSettings();if e!=nil{os.Exit(5)};v.DefaultLaunchMode="cli";v.MaxAttempts=9;if e=s.SaveSettings(v);e!=nil{os.Exit(6)};s.Close();os.WriteFile(filepath.Join(d,"master.key"),make([]byte,32),0600)}
''',encoding='utf-8')
        bad_exe=root/'bad-release.exe'
        run(['go','build','-trimpath','-ldflags','-s -w -H windowsgui','-o',str(bad_exe),str(bad)], root/'bad-build.log')
        bad_release=sign('0.0.3',bad_exe,'release-bad.json')
        require(gateway.request('/api/app-updates/check',{},'POST')[0]==200,'bad-health fixture metadata not accepted')
        require(call(gateway.origin,'/api/app-updates/install',{'version':'0.0.3','sha256':bad_release['sha256'],'open_mode':'desktop'},'POST',gateway.headers,timeout=60)[0]==202,'rollback fixture not accepted')
        rollback=until(lambda: outcome('0.0.2','rolled_back'),'old EXE/data did not recover',120)
        _,_,body=gateway.request('/api/settings')
        kept=json.loads(body)
        require(kept['default_launch_mode']=='desktop' and kept['max_attempts']==4,'failed candidate settings were not rolled back')
        require((data/'master.key').read_bytes()==key_before,'master key was not restored with database')
        _,_,body=gateway.request('/api/channels')
        require(len(json.loads(body))==1,'credential-bearing channel not readable after rollback')
        until(restored_views, 'Desktop and pet were not restored after rollback', 25)
        results['rollback']={'running_version':'0.0.2','result':rollback['last_result'],'settings_restored':True,'master_key_restored':True,'channel_readable':True}
        results['single_executable'] = True
        results.pop('phase', None)
        results.pop('native_observation', None)
        results['ok'] = True
    except Exception as error:
        (root/'failure-trace.txt').write_text(traceback.format_exc(), encoding='utf-8')
        results['ok'] = False
        results['failure'] = str(error) if isinstance(error,AssertionError) else type(error).__name__
        raise
    finally:
        for proc in followers:
            if proc.poll() is None: proc.terminate()
            proc.wait(timeout=5)
            for stream in (proc.stdin,proc.stdout,proc.stderr):
                if stream: stream.close()
        if gateway:
            gateway.cleanup()
            native_processes(gateway.exe, gateway.data, terminate=True)
        feed.shutdown()
        feed.server_close()
        (root/'results.json').write_text(json.dumps(results,indent=2)+'\n',encoding='utf-8')
        print(json.dumps(results),flush=True)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'ok':False,'error_type':type(error).__name__}),flush=True)
        raise SystemExit(1)
