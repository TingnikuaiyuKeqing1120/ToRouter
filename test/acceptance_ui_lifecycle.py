#!/usr/bin/env python3
"""Windows built Desktop/pet/CLI lifecycle acceptance with disposable data.

Read-only Win32 observation checks visible HWNDs and saved geometry. This does
not simulate dragging or prove other monitor/DPI combinations. No keys printed.
"""
import argparse
import ctypes
from ctypes import wintypes
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

from acceptance_restart import Gateway, NO_WINDOW, call, free_port, owned_processes, require


def windows_for(pid):
    user = ctypes.WinDLL('user32')
    user.EnumWindows.argtypes = [ctypes.c_void_p, wintypes.LPARAM]
    user.GetWindowThreadProcessId.argtypes = [wintypes.HWND, ctypes.POINTER(wintypes.DWORD)]
    user.GetWindowRect.argtypes = [wintypes.HWND, ctypes.POINTER(wintypes.RECT)]
    user.IsWindowVisible.argtypes = [wintypes.HWND]
    user.GetWindowTextW.argtypes = [wintypes.HWND, wintypes.LPWSTR, ctypes.c_int]
    user.GetClassNameW.argtypes = [wintypes.HWND, wintypes.LPWSTR, ctypes.c_int]
    found = []

    @ctypes.WINFUNCTYPE(wintypes.BOOL, wintypes.HWND, wintypes.LPARAM)
    def inspect(hwnd, _):
        process = wintypes.DWORD()
        user.GetWindowThreadProcessId(hwnd, ctypes.byref(process))
        if process.value == pid:
            cls = ctypes.create_unicode_buffer(128)
            user.GetClassNameW(hwnd, cls, len(cls))
            if cls.value == 'wailsWindow' and user.IsWindowVisible(hwnd):
                rect, title = wintypes.RECT(), ctypes.create_unicode_buffer(256)
                user.GetWindowRect(hwnd, ctypes.byref(rect))
                user.GetWindowTextW(hwnd, title, len(title))
                found.append({'hwnd': hwnd, 'title': title.value,
                              'left': rect.left, 'top': rect.top,
                              'right': rect.right, 'bottom': rect.bottom})
        return True

    user.EnumWindows(inspect, 0)
    return found


def until(predicate, label, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(.05)
    raise AssertionError(label)


def visible(process):
    require(process.poll() is None, 'native follower exited before visibility')
    result = windows_for(process.pid)
    if not result:
        return None
    require(len(result) == 1, 'native follower has multiple main windows')
    return result[0]


def restart(gateway, target):
    status, _, body = call(gateway.origin, '/_console/ready')
    require(status == 200, 'original gateway identity missing')
    before = json.loads(body)['instance_id']
    if target != gateway.port:
        gateway.save_port(target)
    gateway.restart()

    def successor():
        try:
            status, _, body = call(f'http://127.0.0.1:{target}', '/_console/ready', timeout=.5)
            value = json.loads(body)
            return status == 200 and value.get('instance_id') != before
        except (OSError, ValueError):
            return False

    until(successor, 'restart has no identified successor', 20)
    gateway.ready(target)


def run_case(binaries, root, abrupt):
    data = root / ('abrupt-data' if abrupt else 'normal-data')
    data.mkdir()
    service = Gateway(binaries, data, True, 20)
    processes, outputs = [], []
    environment = os.environ.copy()
    environment['WEBVIEW2_USER_DATA_FOLDER'] = str(root / ('abrupt-webview' if abrupt else 'normal-webview'))

    def launch(name, args):
        output = (data / f'{name}-{len(processes)}.log').open('wb')
        outputs.append(output)
        exe = binaries / 'ToRouter.exe'
        mode = 'desktop' if name == 'gui' else name
        process = subprocess.Popen([str(exe), '-mode', mode, '-data', str(data), *args],
                                   stdout=output, stderr=subprocess.STDOUT,
                                   stdin=subprocess.DEVNULL, creationflags=NO_WINDOW, env=environment)
        processes.append(process)
        return process

    try:
        service.ready(service.port, starting=True)
        cache = data / '.pet-position.json'
        expected = {'left': 130, 'bottom': 560}
        cache.write_text(json.dumps(expected))
        pet = launch('pet', [])
        first = until(lambda: visible(pet), 'pet did not become visible')
        require({key: first[key] for key in expected} == expected, 'built pet did not restore cached point: '+json.dumps(first))
        status, _, _ = call(service.origin, '/api/desktop/close-pet', method='POST', headers=service.headers)
        require(status == 200, 'pet close was not accepted')
        require(pet.wait(timeout=5) == 0, 'pet close failed')
        require(json.loads(cache.read_text()) == expected, 'normal close did not retain idle point')
        pet = launch('pet', [])
        reopened = until(lambda: visible(pet), 'pet did not reopen')
        require({key: reopened[key] for key in expected} == expected, 'reopened pet lost saved point')
        gui = launch('gui', [])
        gui_window = until(lambda: visible(gui), 'Desktop did not become visible')
        cli = launch('cli', [])

        def cli_ready():
            require(cli.poll() is None, 'CLI follower exited at startup')
            try:
                with (data / '.cli-view.lock').open('rb') as lease:
                    lease.seek(1)
                    return json.load(lease).get('pid') == cli.pid
            except (OSError, ValueError):
                return False

        until(cli_ready, 'CLI lease not ready')
        if not abrupt:
            restart(service, service.port)
            restart(service, free_port())
            until(lambda: service.origin.removeprefix('http://') in visible(pet)['title'], 'pet did not follow changed port')
            require(visible(gui)['hwnd'] == gui_window['hwnd'], 'restart replaced Desktop window')
            require(visible(pet)['hwnd'] == reopened['hwnd'], 'restart replaced pet window')
            # Also cover the first 25s watch heartbeat after reconnect; merely
            # staying alive for a few seconds could hide a 30s grace expiry.
            deadline = time.monotonic() + 32
            while time.monotonic() < deadline:
                require(all(p.poll() is None for p in (pet, gui, cli)), 'restart follower exceeded its recovery budget')
                time.sleep(.1)
            status, _, _ = service.request('/api/shutdown', {}, 'POST')
            require(status == 200, 'shutdown was not acknowledged')
            exit_limit = 5
        else:
            owned_processes(service.exe, service.data, terminate=True)
            exit_limit = 38
        started = time.monotonic()
        until(lambda: all(p.poll() is not None for p in (pet, gui, cli)), 'followers did not exit with gateway', exit_limit)
        elapsed = round(time.monotonic() - started, 3)
        require(all(p.returncode == 0 for p in (pet, gui, cli)), 'follower reported an abnormal exit')
        require(not owned_processes(service.exe, service.data), 'UI follower auto-started a stopped daemon')
        with (data / '.cli-view.lock').open('rb') as lease:
            require(lease.read() == b'\0', 'CLI lease metadata was not released')
        return {'stop_mode': 'abrupt' if abrupt else 'graceful', 'followers_exit_seconds': elapsed,
                'saved_point': expected, 'pet_reopen_restored': True,
                'same_and_changed_port_restart': not abrupt,
                'restart_survived_first_heartbeat': not abrupt, 'owned_daemons_remaining': 0}
    finally:
        for process in processes:
            if process.poll() is None:
                process.terminate()
            process.wait(timeout=5)
        service.cleanup()
        for output in outputs:
            output.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin-dir', type=Path, required=True)
    parser.add_argument('--evidence-dir', type=Path, required=True)
    args = parser.parse_args()
    require(os.name == 'nt', 'this acceptance requires Windows')
    # The production cache uses physical screen coordinates. An unaware Python
    # observer would get GetWindowRect's virtualized coordinates at 150% DPI.
    user = ctypes.WinDLL('user32')
    user.SetProcessDpiAwarenessContext.argtypes = [ctypes.c_void_p]
    user.SetProcessDpiAwarenessContext.restype = wintypes.BOOL
    require(user.SetProcessDpiAwarenessContext(ctypes.c_void_p(-4)), 'observer DPI awareness could not be established')
    source = args.bin_dir.resolve(strict=True)
    with tempfile.TemporaryDirectory(prefix='torouter-ui-lifecycle-') as temporary:
        root = Path(temporary).resolve()
        require(root.parent == Path(tempfile.gettempdir()).resolve() and root.name.startswith('torouter-ui-lifecycle-'),
                'temporary cleanup boundary invalid')
        binaries = root / 'bin'
        binaries.mkdir()
        for name in ('ToRouter.exe',):
            shutil.copy2(source / name, binaries / name)
        results = [run_case(binaries, root, False), run_case(binaries, root, True)]
        evidence = args.evidence_dir.resolve()
        evidence.mkdir(parents=True, exist_ok=True)
        (evidence / 'ui-lifecycle.json').write_text(json.dumps(results, indent=2) + '\n')
        print(json.dumps({'ok': True, 'cases': results}))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'ok': False, 'error_type': type(error).__name__,
                          'error': str(error) if isinstance(error, AssertionError) else 'isolated lifecycle acceptance failed'}))
        raise SystemExit(1)
