"""HTTP acceptance against preview_credential_operations.py's isolated EXE.

Only a verified synthetic fixture is accepted. Tests never target user data.
"""
import argparse
from http.cookiejar import CookieJar
import json
from pathlib import Path
import sqlite3
import urllib.error
import urllib.request
import uuid


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--state-file', type=Path, required=True)
    p.add_argument('--control-file', type=Path, required=True)
    p.add_argument('--report', type=Path, required=True)
    args = p.parse_args()
    state = json.loads(args.state_file.read_text())
    assert state['synthetic'] is True and Path(state['data_dir']).name.startswith('torouter-t102-')
    origin = state['origin']
    assert origin.startswith('http://127.0.0.1:')
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(CookieJar()))
    checks = []

    def call(path, method='GET', body=None, status=200):
        request = urllib.request.Request(origin+'/api'+path, method=method,
            data=None if body is None else json.dumps(body).encode(), headers={'X-To-Router':'1', 'Content-Type':'application/json'})
        try:
            response = opener.open(request, timeout=40)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            assert response.status == status, (path, response.status, raw[:200])
            assert b'synthetic-access' not in raw and b'synthetic-refresh' not in raw
            return json.loads(raw)

    def control(**values):
        args.control_file.write_text(json.dumps(values), encoding='utf-8')

    def observed(path):
        file = args.state_file.with_suffix('.calls.json')
        return [x for x in json.loads(file.read_text()) if x['path']==path] if file.exists() else []

    call('/auth/local','POST',{})
    channels = call('/channels')
    assert isinstance(channels,list) and len(channels)==6
    assert len(observed('/responses'))+len(observed('/credits/consume'))==0
    checks.append('automatic actions off by default')
    # Locate by stable fixture ID, not provider/name sorting.
    initial = next(c for c in channels if c['id']==1)
    ids = [c['id'] for c in initial['credentials']]
    quota_path = '/oauth/codex/usage?channel_id=1&credential_id=demo_2'
    before_quota = call(quota_path)
    call('/channels/1/credential-order','PUT',{'credential_ids':list(reversed(ids))})
    call('/channels/1/credentials/demo_2/enabled','PUT',{'enabled':False})
    assert call(quota_path)['captured_at']==before_quota['captured_at']
    changed = next(c for c in call('/channels') if c['id']==1)
    assert [c['id'] for c in changed['credentials']]==list(reversed(ids))
    assert next(c for c in changed['credentials'] if c['id']=='demo_2')['enabled'] is False
    call('/channels/1/credential-order','PUT',{'credential_ids':['demo_1','demo_1']},400)
    assert next(c for c in call('/channels') if c['id']==1)==changed
    call('/channels/1/credentials/demo_2/enabled','PUT',{'enabled':True})
    call('/channels/1/credential-order','PUT',{'credential_ids':ids})
    checks.append('exact order and enable writes; invalid order atomic; account quota retained')
    base = '/channels/1/credentials/demo_2/operations'
    report = call(base)
    identity = report['identity']
    config = report['config']
    config['warmup']['model']='coding-model'
    config['auto_reset']['rules']=[{'window_seconds':604800,'remaining_below':15,'expires_within_days':2,'join':'and'},
                                   {'window_seconds':18000,'remaining_below':10,'expires_within_days':None,'join':'or'}]
    if state.get('workbench'):
        assert config['warmup']['daily_times']==['09:00'] and 'daily_time' not in config['warmup']
        checks.append('load migration preserves legacy daily time before typed readers')
    config['warmup']['daily_times']=['08:30','17:45']
    report = call(base,'PUT',{'expected_identity':identity,'warmup':config['warmup']})
    report = call(base,'PUT',{'expected_identity':identity,'auto_reset':config['auto_reset']})
    assert call(base)['config']==config
    call(base,'PUT',{'expected_identity':'old-account','warmup':config['warmup']},409)
    call(base,'PUT',{'expected_identity':identity,'config':config},400)
    call(base,'PUT',{'expected_identity':identity,'warmup':dict(config['warmup'],daily_time='09:00')},400)
    call(base,'PUT',{'expected_identity':identity,'warmup':dict(config['warmup'],daily_times=['08:30','08:30'])},400)
    assert call(base)['config']==config
    changed_model = call(base,'PUT',{'expected_identity':identity,'warmup_model':'review-model'})
    assert changed_model['config']['auto_reset']==config['auto_reset']
    assert changed_model['config']['warmup']['daily_times']==['08:30','17:45']
    call(base,'PUT',{'expected_identity':identity,'warmup_model':'coding-model'})
    checks.append('independent multi-time/rule/model patches preserve sibling settings; stale identity and retired/invalid fields rejected atomically')
    current_generation=call(base)['channel_generation']
    toggled=call('/channels/1/credentials/demo_2/enabled','PUT',{'enabled':False,'expected_generation':current_generation})
    call('/channels/1/credentials/demo_2/enabled','PUT',{'enabled':True,'expected_generation':current_generation},409)
    call('/channels/1/credentials/demo_2/enabled','PUT',{'enabled':True,'expected_generation':toggled['generation']})
    checks.append('checked batch toggles accept own returned generation and reject stale generation')
    if state.get('workbench'):
        weekly=call('/channels/1/credentials/demo_3/operations')
        for mode in ('reset','daily'):
            call('/channels/1/credentials/demo_3/operations','PUT',{'expected_identity':weekly['identity'],
                 'warmup':dict(config['warmup'],enabled=True,mode=mode)},400)
        checks.append('weekly-only credential cannot enable either automatic warmup mode')
    control()
    request_id = str(uuid.uuid4())
    payload = {'expected_identity':identity,'request_id':request_id}
    for _ in range(2):
        assert call(base+'/warmup','POST',payload)['last_warmup']['state']=='succeeded'
    assert len(observed('/responses'))==1
    assert observed('/responses')[0]['account']=='account-1-1'
    assert observed('/responses')[0]['model']=='coding-model'
    checks.append('warmup exact credential/model; repeated UUID executes once')
    snapshot = call(base+'/refresh-reset-cards','POST',{'expected_identity':identity})['reset_cards']
    control(cards_status=503,consume_status=503)
    failed = call(base+'/refresh-reset-cards','POST',{'expected_identity':identity})
    assert failed['reset_cards']==snapshot and failed['reset_cards_error']
    payload['request_id']=str(uuid.uuid4())
    for _ in range(2):
        assert call(base+'/consume-reset','POST',payload)['last_reset']['state']=='unknown'
    assert len(observed('/credits/consume'))==1
    call(base+'/consume-reset','POST',dict(payload,request_id=str(uuid.uuid4())),409)
    assert call(base+'/acknowledge','POST',{'expected_identity':identity,'kind':'reset'})['last_reset']['state']=='acknowledged'
    control(card_count=2)
    assert call(base+'/consume-reset','POST',dict(payload,request_id=str(uuid.uuid4())))['last_reset']['state']=='succeeded'
    assert len(observed('/credits/consume'))==2
    assert call(base)['reset_cards']['available_count']==2
    checks.append('failed read retains cache; unknown does not retry; explicit acknowledge enables next action')
    with sqlite3.connect(Path(state['data_dir'])/'router.db') as db:
        record = json.loads(db.execute('SELECT value FROM meta WHERE key=?',('codex_credential_operations:1:demo_2',)).fetchone()[0])
        history = record['data']['history']
        assert len(history)==3 and sorted(x['state'] for x in history.values())==['acknowledged','succeeded','succeeded']
        stored = json.loads(db.execute('SELECT data FROM channels WHERE id=1').fetchone()[0])
        assert next(c for c in stored['credentials'] if c['id']=='demo_2')['operations']==config
    checks.append('configuration and all action receipts committed to SQLite')
    result = {'passed':True,'exe_sha256':state['exe_sha256'],'checks':checks,
              'warmup_posts':len(observed('/responses')),'consume_posts':len(observed('/credits/consume')),
              'evidence_boundary':'real local EXE and SQLite with synthetic upstream; no real account effects'}
    args.report.write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding='utf-8')
    print(json.dumps(result,ensure_ascii=False,indent=2))


if __name__=='__main__':
    main()
