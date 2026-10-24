#!/usr/bin/env python3
"""Small offline privacy guard. Reports locations, never matched values.

This is not a complete secret scanner. Images, account names and release scope
still require review. --history inspects reachable objects, not remote caches.
"""
import argparse
import hashlib
import json
import pathlib
import re
import subprocess
import sys

PROFILES = (
    re.compile(r'(?i)[A-Z]:[\\/]+Users[\\/]+([^\\/\r\n"<>`]+)'),
    re.compile(r'(?<![A-Za-z0-9:/\\])/(?:Users|home)/([^/\s"<>`]+)'),
)
EXAMPLE_PROFILES = {'example-user', 'test-user', 'fixture-user', 'example user', 'test user', 'fixture user', 'public'}
PATTERNS = {
    'private-key': re.compile(r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----'),
    'token-candidate': re.compile(r'\b(?:sk-(?:proj-|ant-)?[A-Za-z0-9_-]{24,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,}|tr_(?:mcp_[A-Za-z0-9_-]{30,}|[A-Za-z0-9_-]{43,})|AKIA[A-Z0-9]{16})\b'),
    'jwt-candidate': re.compile(r'\beyJ[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{15,}\b'),
    'messaging-profile': re.compile(r'\bwxid_[A-Za-z0-9_-]{8,}'),
    'personal-email-candidate': re.compile(r'\b[\w.+-]+@(?:qq|163|126|gmail|hotmail|outlook|icloud)\.com\b', re.I),
}
STATE_NAMES = {'master.key', 'router.db', 'router.db-wal', 'router.db-shm', '.env'}
NOTICE_MANIFEST = 'web/public/third-party/manifest.json'
NOTICE_ROOT = 'web/public/third-party/'


def attributed_notices(raw):
    """Allow public copyright emails only in the exact inventoried legal text.

    This is not a directory exclusion: secrets and personal paths still fail,
    and the release notice check independently verifies upstream provenance.
    """
    try:
        manifest = json.loads(raw)
        if manifest.get('schema') != 1:
            return {}
        result = {}
        for component in manifest.get('components', []):
            if not component.get('name') or not component.get('version') or not str(component.get('source', '')).startswith('https://'):
                continue
            for notice in component.get('notices', []):
                path = pathlib.PurePosixPath(notice.get('path', ''))
                digest = notice.get('sha256', '')
                if path.is_absolute() or '..' in path.parts or not path.parts or path.parts[0] != 'licenses':
                    continue
                if not re.search(r'(^|[-_.])(licen[cs]e|notice|copying|copyright|patents|authors)([-_.]|$)', path.name, re.I):
                    continue
                if re.fullmatch(r'[a-f0-9]{64}', digest):
                    result[NOTICE_ROOT + path.as_posix()] = digest
        return result
    except (ValueError, TypeError, AttributeError):
        return {}


def findings(data):
    if b'\0' in data[:8192]:
        return []
    text = data.decode('utf-8', 'replace')
    result = set()
    for regex in PROFILES:
        for match in regex.finditer(text):
            if match[1].strip().casefold() not in EXAMPLE_PROFILES:
                result.add(('personal-home', text.count('\n', 0, match.start()) + 1))
    for kind, regex in PATTERNS.items():
        for match in regex.finditer(text):
            result.add((kind, text.count('\n', 0, match.start()) + 1))
    return sorted(result)


def safe_location(value):
    for pattern in PROFILES:
        value = pattern.sub('[HOME]', value)
    for pattern in PATTERNS.values():
        value = pattern.sub('[REDACTED]', value)
    return value


def git(*args):
    return subprocess.check_output(['git', '-c', 'core.quotepath=false', *args], stderr=subprocess.PIPE)


def staged_contents():
    """Read the index through one Git process, rather than one per file."""
    process = subprocess.Popen(['git', 'cat-file', '--batch'], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    try:
        for record in git('ls-files', '--stage', '-z').split(b'\0'):
            if not record:
                continue
            metadata, name = record.split(b'\t', 1)
            _, oid, stage = metadata.split()
            if stage != b'0':
                raise RuntimeError('Resolve index conflicts before checking privacy')
            process.stdin.write(oid + b'\n'); process.stdin.flush()
            header = process.stdout.readline().split()
            if len(header) != 3 or header[1] != b'blob':
                raise RuntimeError('Unexpected staged object')
            data = process.stdout.read(int(header[2])); process.stdout.read(1)
            yield name.decode('utf-8'), data
    finally:
        process.stdin.close(); process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--staged', action='store_true', help='Check index contents instead of working files')
    parser.add_argument('--history', action='store_true', help='Also inspect all locally reachable history blobs and commit messages')
    parser.add_argument('--identity', action='store_true', help='Reject non-noreply/non-example author and committer email')
    parser.add_argument('--binaries', nargs='*', default=[], help='Check binary files for this machine\'s home path bytes')
    args = parser.parse_args()
    count = 0
    try:
        manifest = git('show', ':' + NOTICE_MANIFEST) if args.staged else pathlib.Path(NOTICE_MANIFEST).read_bytes()
    except (OSError, subprocess.CalledProcessError):
        manifest = b'{}'
    notices = attributed_notices(manifest)
    retained_attributions = 0
    def report(location, data, notice_path=None):
        nonlocal count
        nonlocal retained_attributions
        attributed = notice_path in notices and hashlib.sha256(data).hexdigest() == notices[notice_path]
        for kind, line in findings(data):
            if kind == 'personal-email-candidate' and attributed:
                retained_attributions += 1
                continue
            print(f'{safe_location(location)}:{line}: {kind}')
            count += 1

    names = set(git('ls-files', '-z').decode('utf-8').split('\0')) - {''}
    if not args.staged:
        names.update(set(git('ls-files', '--others', '--exclude-standard', '-z').decode('utf-8').split('\0')) - {''})
    image_count = 0
    checked_count = 0
    contents = staged_contents() if args.staged else ((name, pathlib.Path(name).read_bytes()) for name in sorted(names) if pathlib.Path(name).is_file())
    for name, data in contents:
        path = pathlib.Path(name)
        checked_count += 1
        report(name, name.encode('utf-8'))
        if path.name in STATE_NAMES or (path.name.startswith('.env.') and path.name != '.env.example') or path.suffix.lower() in {'.db', '.sqlite', '.sqlite3', '.p12', '.pfx', '.pem', '.log'}:
            print(f'{safe_location(name)}: local-state-file'); count += 1
        if path.suffix.lower() in {'.png', '.jpg', '.jpeg', '.webp', '.gif'}:
            image_count += 1
        report(name, data, name)
    if args.history:
        objects = git('rev-list', '--objects', '--all').splitlines()
        process = subprocess.Popen(['git', 'cat-file', '--batch'], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        try:
            for record in objects:
                oid, _, rawpath = record.partition(b' ')
                process.stdin.write(oid + b'\n'); process.stdin.flush()
                header = process.stdout.readline().split()
                if len(header) != 3: raise RuntimeError('Unexpected Git object response')
                data = process.stdout.read(int(header[2])); process.stdout.read(1)
                if header[1] == b'blob':
                    report(f'history/{oid[:12].decode()}/filename', rawpath)
                    report(f'history/{oid[:12].decode()}/{rawpath.decode("utf-8", "replace")}', data, rawpath.decode('utf-8', 'replace'))
            report('history/commit-messages', git('log', '--all', '--format=%B'))
        finally:
            process.stdin.close(); process.wait()
        print('History author names/emails and unreachable/remote objects need separate review.')
    if args.identity:
        for kind in ('GIT_AUTHOR_IDENT', 'GIT_COMMITTER_IDENT'):
            value = git('var', kind).decode('utf-8', 'replace')
            match = re.search(r'<([^<>]+)>', value)
            if not match or not re.search(r'@(?:users\.noreply\.github\.com|[^@\s]+\.invalid)$', match[1], re.I):
                print(f'{kind}: public identity must be confirmed; use an approved noreply or project identity')
                count += 1
    home = str(pathlib.Path.home())
    variants = {home, home.replace('\\', '/'), home.replace('\\', '\\\\')}
    for name in args.binaries:
        data = pathlib.Path(name).read_bytes()
        if any(value.encode(encoding) in data for value in variants for encoding in ('utf-8', 'utf-16le')):
            print(f'{safe_location(pathlib.Path(name).name)}: embedded-home-path'); count += 1
    print(f'Checked {checked_count} paths; {count} finding(s). {image_count} images require visual/OCR review before publication.')
    if retained_attributions:
        print(f'Retained {retained_attributions} public email attribution line(s) in source-linked, hash-matched legal notices.')
    return 1 if count else 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, subprocess.CalledProcessError, RuntimeError):
        print('Privacy check could not finish; verify repository and file access.', file=sys.stderr)
        sys.exit(2)
