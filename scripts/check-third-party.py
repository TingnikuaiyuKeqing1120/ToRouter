#!/usr/bin/env python3
"""Generate/check bundled third-party notices, offline and without extra packages.

Requires Go and the exact production npm packages installed by npm ci. Never
downloads modules: populate the ordinary build caches before running this tool.
"""
import argparse
import hashlib
import html
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / 'web/public/third-party'
LEGAL = re.compile(r'(^|[-_.])(licen[cs]e|notice|copying|copyright|patents|authors)([-_.]|$)', re.I)
ENTRIES = ['cmd/torouter']


def sha(data):
    return hashlib.sha256(data).hexdigest()


def encode(value):
    return (json.dumps(value, ensure_ascii=False, indent=2) + '\n').encode()


def run(*args):
    # A check must never resolve a cache miss by making a network request.
    env = dict(os.environ, GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local',
               GOOS='windows', GOARCH='amd64')
    result = subprocess.run(args, cwd=ROOT, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, check=False)
    if result.returncode:
        raise RuntimeError(f'{args[0]} command failed; prepare the locked build dependencies first')
    return result.stdout.decode('utf-8')


def json_stream(text):
    decoder = json.JSONDecoder()
    while text.strip():
        value, end = decoder.raw_decode(text.lstrip())
        yield value
        text = text.lstrip()[end:]


def legal_files(directory):
    found = []
    for base, dirs, files in os.walk(directory):
        dirs[:] = sorted(d for d in dirs if d not in {'node_modules', '.git'})
        for name in sorted(files):
            if LEGAL.search(name) and not name.endswith(('.go', '.js', '.ts', '.map', '.json')):
                path = Path(base) / name
                if path.is_file():
                    found.append(path)
    return sorted(found)


def generate():
    outputs = {}
    components = []

    def component(name, version, group, source, directory=None, files=None, **extra):
        identifier = sha(f'{group}:{name}@{version}'.encode())[:16]
        notices = []
        paths = files if files is not None else legal_files(directory)
        if not paths:
            raise RuntimeError(f'No license/notice text found for {name}@{version}')
        for path in paths:
            relative = path.relative_to(directory).as_posix() if directory else path.name
            data = path.read_bytes()
            # A binary named LICENSE is not usable as a legal notice.
            data.decode('utf-8')
            dest = f'licenses/{identifier}/{relative}'
            outputs[dest] = data
            notices.append({'file': relative, 'path': dest, 'sha256': sha(data)})
        value = dict(name=name, version=version, group=group, source=source,
                     notices=notices, **extra)
        components.append(value)
        return value

    build_by_module = {}
    for entry in ENTRIES:
        for line in run('go', 'list', '-deps', '-tags', 'production', '-f',
                        '{{if .Module}}{{.Module.Path}}{{end}}', './' + entry).splitlines():
            if line and line != 'torouter':
                build_by_module.setdefault(line, []).append(entry)
    modules = list(json_stream(run('go', 'list', '-m', '-json', 'all')))
    go_sums = {}
    for line in (ROOT / 'go.sum').read_text(encoding='utf-8').splitlines():
        name, version, checksum = line.split()
        go_sums[name, version] = checksum
    go_inventory = []
    for module in modules:
        if module.get('Main'):
            continue
        name, version = module['Path'], module['Version']
        if module.get('Replace'):
            raise RuntimeError(f'Module replacement needs explicit source review: {name}')
        go_inventory.append({'name': name, 'version': version,
                             'release_entries': build_by_module.get(name, []),
                             'sum': go_sums.get((name, version)),
                             'go_mod_sum': go_sums.get((name, version + '/go.mod'))})
        if name not in build_by_module:
            continue
        directory = module.get('Dir')
        if not directory or not Path(directory).is_dir():
            raise RuntimeError(f'Missing module cache: {name}@{version}')
        component(name, version, 'Go release', f'https://pkg.go.dev/{name}@{version}',
                  Path(directory), release_entries=build_by_module[name])
    if set(build_by_module) != {m['name'] for m in go_inventory if m['release_entries']}:
        raise RuntimeError('Go release module inventory is incomplete')

    goroot = Path(run('go', 'env', 'GOROOT').strip())
    # Go's source tree contains additional licenses for vendored/system code.
    go_legal = [p for p in legal_files(goroot / 'src')]
    go_legal += [p for p in goroot.iterdir() if p.is_file() and LEGAL.search(p.name)]
    component('Go runtime and standard library', run('go', 'env', 'GOVERSION').strip(),
              'Go toolchain', 'https://go.dev/LICENSE', goroot, sorted(go_legal))

    lock = json.loads((ROOT / 'web/package-lock.json').read_text(encoding='utf-8'))
    npm_inventory = []
    for location, package in sorted(lock['packages'].items()):
        if not location:
            continue
        name = location.rsplit('node_modules/', 1)[-1]
        release = not package.get('dev', False)
        npm_inventory.append({'name': name, 'version': package['version'],
                              'location': location, 'release_graph': release,
                              'license': package.get('license'),
                              'integrity': package.get('integrity')})
        if not release:
            continue
        directory = ROOT / 'web' / location
        metadata_path = directory / 'package.json'
        if not metadata_path.is_file():
            raise RuntimeError(f'Missing installed npm package: {location}; run npm ci in web')
        metadata = json.loads(metadata_path.read_text(encoding='utf-8'))
        if metadata['version'] != package['version'] or metadata['name'] != name:
            raise RuntimeError(f'Installed npm package differs from lock: {location}')
        # Nested copies may have equal versions; one license copy covers both.
        if any(c['group'] == 'npm production graph' and c['name'] == name and
               c['version'] == package['version'] for c in components):
            continue
        component(name, package['version'], 'npm production graph',
                  f'https://www.npmjs.com/package/{name}/v/{package["version"]}',
                  directory, license=metadata.get('license', package.get('license')))

    refs = json.loads((ROOT / 'third_party/references.json').read_text(encoding='utf-8'))
    for reference in refs:
        directory = ROOT / reference['directory']
        for file in reference['files']:
            if sha((directory / file['file']).read_bytes()) != file['sha256']:
                raise RuntimeError(f'Reference legal text changed: {reference["name"]}')
        component(reference['name'], reference['version'], 'Research and design references',
                  reference['source'], directory,
                  files=[directory / f['file'] for f in reference['files']],
                  scope=reference['scope'])

    component('@magpie-community/opencode-qoder-auth', '0.2.19 + to-router 1',
              'Directly used code',
              'https://github.com/magpie-community/plugins/tree/d34f78b69a303a620307f1202a77e171a13ecf8f/packages/qoder',
              ROOT / 'internal/pluginhost/packages/qoder',
              files=[ROOT / 'internal/pluginhost/packages/qoder' / name
                     for name in ['LICENSE', 'LICENSE-CLIProxyAPI']],
              scope='Optional compatible provider package; modified request contract and stream terminal/tool handling. See internal/pluginhost/packages/qoder/SOURCE.md.')

    for package, version in [('workbuddy', '0.1.25'), ('minimax', '0.1.2')]:
        component(f'@magpie-community/opencode-{package}-auth', f'{version} + to-router 1',
                  'Directly used code',
                  f'https://github.com/magpie-community/plugins/tree/d34f78b69a303a620307f1202a77e171a13ecf8f/packages/{package}',
                  ROOT / f'internal/pluginhost/packages/{package}',
                  files=[ROOT / f'internal/pluginhost/packages/{package}/LICENSE'],
                  scope=f'Optional compatible provider; independent authorization, durable state, unchanged context and checked stream termination. See internal/pluginhost/packages/{package}/SOURCE.md.')

    icon_dir = ROOT / 'web/src/assets/provider-icons'
    icon_catalog = json.loads((icon_dir / 'catalog.json').read_text(encoding='utf-8'))
    icon_license = ROOT / icon_catalog['license_file']
    if sha(icon_license.read_bytes()) != icon_catalog['license_sha256']:
        raise RuntimeError('Provider icon license changed; verify the fixed upstream source')
    icon_originals = []
    icon_ids = set()
    for icon in icon_catalog['icons']:
        name = icon['file']
        if Path(name).name != name or Path(name).suffix not in ('.svg', '.png') or icon['id'] in icon_ids:
            raise RuntimeError('Invalid or duplicate provider icon catalog entry')
        icon_ids.add(icon['id'])
        data = (icon_dir / name).read_bytes()
        if sha(data) != icon['sha256']:
            raise RuntimeError(f'Provider icon source changed: {name}')
        dest = 'sources/provider-icons/' + name
        outputs[dest] = data
        original = {'file': name, 'path': dest, 'sha256': sha(data),
                    'upstream_path': icon['upstream_path']}
        if icon.get('repository'):
            legal = ROOT / icon['license_file']
            if sha(legal.read_bytes()) != icon['license_sha256']:
                raise RuntimeError(f'Provider icon license changed: {name}')
            component(icon['label'] + ' icon', icon['revision'], 'Directly used artwork',
                      icon['repository'] + '/tree/' + icon['revision'], legal.parent,
                      files=[legal], license='MIT', originals=[original],
                      modifications='Original artwork copied unchanged; rendered locally.')
        else:
            icon_originals.append(original)
    outputs['sources/provider-icons/catalog.json'] = (icon_dir / 'catalog.json').read_bytes()
    component(icon_catalog['name'], icon_catalog['revision'], 'Directly used artwork',
              icon_catalog['repository'] + '/tree/' + icon_catalog['revision'],
              icon_license.parent, files=[icon_license], license=icon_catalog['license'],
              originals=icon_originals, modifications=icon_catalog['modifications'],
              scope='Curated static provider/tool SVGs, bundled locally without CDN requests. '
                    'Brand names and logos identify their respective owners; inclusion does not imply endorsement.')

    assets = ROOT / 'scripts/companion/assets'
    asset_sources = json.loads((assets / 'SOURCES.json').read_text(encoding='utf-8'))
    original_files = []
    for file in asset_sources['files']:
        data = (assets / file['file']).read_bytes()
        git_blob = hashlib.sha1(f'blob {len(data)}\0'.encode() + data).hexdigest()
        if git_blob != file['git_blob']:
            raise RuntimeError(f'Original Clawd asset changed: {file["file"]}')
        path = 'sources/companion/assets/' + file['file']
        original_files.append({'file': file['file'], 'path': path,
                               'sha256': sha(data), 'git_blob': git_blob})
    for name in ['index.template.html', 'build.mjs']:
        outputs['sources/companion/' + name] = (assets.parent / name).read_bytes()
    for name in ['deepsleep.svg', 'deepwork.svg', 'deepwake.svg']:
        outputs['sources/companion/assets/' + name] = (assets / name).read_bytes()
    # Keep the preferred editable inputs next to the build script's expected paths.
    for file in asset_sources['files']:
        outputs['sources/companion/assets/' + file['file']] = (assets / file['file']).read_bytes()
    outputs['sources/companion/assets/CLAWD-LICENSE.txt'] = (assets / 'CLAWD-LICENSE.txt').read_bytes()
    outputs['sources/companion/assets/SOURCES.json'] = (assets / 'SOURCES.json').read_bytes()
    component('Clawd SVG animations', 'source Git blobs in SOURCES.json', 'Directly used artwork',
              asset_sources['repository'], assets, files=[assets / 'CLAWD-LICENSE.txt'],
              license='AGPL-3.0 (upstream root LICENSE)', originals=original_files,
              modifications='2026-10-06 onward: SVG/CSS identifier scoping, scene composition, timing overrides; see supplied template/build script.',
              scope='Embedded SVG animation code/artwork; not merely a design reference.')

    manifest = {'schema': 1, 'target': 'windows/amd64; production build tag; unified Go entry',
                'components': components, 'go_build_list': go_inventory,
                'npm_lock_inventory': npm_inventory,
                'inputs': {p: sha((ROOT / p).read_bytes()) for p in
                           ['go.mod', 'go.sum', 'web/package.json', 'web/package-lock.json',
                            'third_party/references.json', 'scripts/check-third-party.py',
                            'web/src/assets/provider-icons/catalog.json']},
                'unresolved': [
                    'User-supplied blue-whale SVGs are described by the user as online circulating derivative artwork based on the official DeepSeek logo; derivative author, original URL and redistribution permission remain unknown.',
                    'No project-wide LICENSE selected. Third-party notices do not license to-router or clear AGPL obligations.',
                    'Research references are documented; a repository-wide copied-code provenance audit has not been performed.',
                    'OS-provided Microsoft WebView2 Runtime is not redistributed by this build; separate redistribution terms must be checked if bundled later.'
                ]}
    outputs['manifest.json'] = encode(manifest)
    esc = html.escape
    sections = []
    for group in dict.fromkeys(c['group'] for c in components):
        items = []
        for c in components:
            if c['group'] != group:
                continue
            links = ' · '.join(f'<a href="{esc(n["path"], quote=True)}">{esc(n["file"])}</a>' for n in c['notices'])
            scope = '<p>' + esc(c['scope']) + '</p>' if c.get('scope') else ''
            if c.get('originals'):
                scope += '<p>原始 SVG：' + ' · '.join(
                    f'<a href="{esc(n["path"], quote=True)}" download>{esc(n["file"])}</a>'
                    for n in c['originals']) + '</p>'
            items.append(f'<li><strong>{esc(c["name"])}</strong> <code>{esc(c["version"])}</code> '
                         f'<a href="{esc(c["source"], quote=True)}">上游来源</a>{scope}<p>{links}</p></li>')
        sections.append(f'<h2>{esc(group)}</h2><ul>{"".join(items)}</ul>')
    outputs['notices.html'] = ('''<!doctype html><html lang="zh-CN"><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>第三方声明 · to-router</title>
<style>body{font:16px/1.6 system-ui,sans-serif;max-width:1000px;margin:2rem auto;padding:0 1rem;color:#172033;background:#fafbfc}a{color:#245ac4}li{margin:1rem 0;overflow-wrap:anywhere}code{font-size:.85em}p{margin:.3rem 0}h2{margin-top:2rem}</style>
<h1>第三方声明</h1><p>以下保留上游许可、版权与 NOTICE 原文；各组件以各自条款为准，无担保声明见许可原文。本页不替 to-router 选择整体许可证，也不代表发布许可已经齐备。</p>
<p>覆盖 Windows 三入口实际 Go 依赖与前端生产依赖图；图中可能包含经打包器移除的类型包或未采用代码。模块内附带的许可保守保留，不声称每个子组件均已使用。开发／测试依赖完整锁定清单见 <a href="manifest.json">机器可读清单与 SHA256</a>。</p>
<h2>素材与发布待确认</h2><p>Clawd 动画直接内嵌，来自 clawd-on-desk，许可为 GNU AGPL v3。修改包括命名隔离、场景组合和节奏调整（2026-10-06 起）。可按该许可行使相应权利，许可含无担保条款；<a href="sources/companion/index.template.html" download>修改源模板</a>、<a href="sources/companion/build.mjs" download>生成脚本</a>及原始 SVG 入口见清单。这里提供的素材生成源文件不等于已满足组合程序的全部对应源代码义务。</p>
<p>蓝鲸来源为用户提供的 deepsleep.svg、deepwork.svg、deepwake.svg；用户说明其为网络流传二创图，原型为 DeepSeek 官方图标。二创作者、具体原始链接与再分发授权仍未知，不声称获得许可或官方背书。项目整体许可证及与 AGPL 的组合／对应源代码提供方式仍待确定；当前声明不能作为公开发布许可证明。</p>
<p>AGPL 第 2 节允许私人运行及不对外传播的修改；第 4–6 节对分发要求保留许可、版权与修改说明并按要求提供对应源代码，第 13 节另约束修改版的远程网络交互。第 5 节区分独立作品聚合，不能仅凭同仓库或同一 EXE 判断整个项目的许可范围。提供对应源代码不要求公开真实密钥、个人数据或旧 Git 历史。条款以随附许可原文为准。</p>
<p>参考实现按已记录研究版本保留许可。本轮未做全仓复制代码溯源审计，不能据此断言所有代码均仅借鉴概念。Microsoft WebView2 系统运行时不随当前包分发，未来捆绑时须另核其条款。</p>
''' + ''.join(sections) + '</html>\n').encode()
    return outputs


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true', help='Do not write; fail on missing, changed or stale generated files')
    args = parser.parse_args()
    outputs = generate()
    existing = {p.relative_to(OUT).as_posix() for p in OUT.rglob('*') if p.is_file()}
    changed = [name for name, data in outputs.items()
               if not (OUT / name).is_file() or (OUT / name).read_bytes() != data]
    stale = sorted(existing - outputs.keys())
    if args.check:
        if changed or stale:
            print(f'Third-party notices drift: {len(changed)} changed/missing, {len(stale)} stale; run python scripts/check-third-party.py')
            return 1
    else:
        for name, data in outputs.items():
            path = OUT / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        for name in stale:
            (OUT / name).unlink()
    manifest = json.loads(outputs['manifest.json'])
    print(f'Third-party notices {"checked" if args.check else "generated"}: {len(manifest["components"])} components, {len(outputs)} files; unresolved publication limits remain in manifest.')
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, RuntimeError) as error:
        # OS error text can contain personal cache paths. Keep diagnostics scoped.
        print('Third-party notices failed: ' + (str(error) if isinstance(error, RuntimeError) else type(error).__name__), file=sys.stderr)
        sys.exit(2)
