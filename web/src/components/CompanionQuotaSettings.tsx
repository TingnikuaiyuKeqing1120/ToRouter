import { useEffect, useState } from 'react';
import { useCompanionPreferences } from '../companion/preferences';
import { useQuotaSnapshots } from '../hooks/useQuotaSnapshots';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { quotaKey } from '../services/quotas';
import type { QuotaTarget } from '../types/quota';
import { Button, ErrorBox } from './ui';

export function CompanionQuotaSettings() {
  const preferences = useCompanionPreferences(true);
  const quotas = useQuotaSnapshots([]);
  const [draft, setDraft] = useState<QuotaTarget[]>([]);
  const [baseline, setBaseline] = useState<QuotaTarget[]>([]);
  const [saving, setSaving] = useState(false);
  const dirty = JSON.stringify(draft) !== JSON.stringify(baseline);
  useEffect(() => {
    if (!dirty && !saving) { setDraft(preferences.value.quota_targets); setBaseline(preferences.value.quota_targets); }
  }, [preferences.value.quota_targets, dirty, saving]);
  async function save() {
    if (saving || preferences.value.revision < 0) return false;
    setSaving(true);
    const ok = await preferences.update({ quota_targets: draft });
    if (ok) setBaseline(draft);
    setSaving(false);
    return ok;
  }
  const discard = () => { setDraft(preferences.value.quota_targets); setBaseline(preferences.value.quota_targets); };
  useUnsavedChanges({ dirty, busy: saving, save, discard });
  const selected = new Set(draft.map(quotaKey));
  const groups = [...new Set(quotas.items.map(item => item.channel_id))];
  function toggle(targets: QuotaTarget[]) {
    const all = targets.every(target => selected.has(quotaKey(target)));
    const keys = new Set(targets.map(quotaKey));
    setDraft(current => all ? current.filter(target => !keys.has(quotaKey(target))) : [...current, ...targets.filter(target => !selected.has(quotaKey(target))).map(({ channel_id, credential_id }) => ({ channel_id, credential_id }))]);
  }
  return <section className="rounded-2xl border border-[var(--color-border)] bg-[var(--color-surface-1)] p-5 space-y-3" aria-label="关注额度设置">
    <div><h2 className="text-sm font-semibold">关注额度</h2><p className="mt-1 text-xs text-[var(--color-text-muted)]">选择在桌宠控制坞查看的凭证。仅支持已接入额度查询的渠道。</p></div>
    {(preferences.error || quotas.error) && <ErrorBox message={preferences.error || quotas.error} />}
    {!groups.length && <p className="text-xs text-[var(--color-text-muted)]">{quotas.loaded ? '暂无支持额度查询的凭证。' : '正在读取凭证…'}</p>}
    {groups.map(id => {
      const items = quotas.items.filter(item => item.channel_id === id);
      return <fieldset key={id} className="rounded-xl border border-[var(--color-border)] p-3 space-y-2" disabled={saving || preferences.value.revision < 0}>
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" aria-label={`关注渠道 ${items[0].channel_name} 全部凭证`} checked={items.every(item => selected.has(quotaKey(item)))} onChange={() => toggle(items)} />{items[0].channel_name}</label>
        <div className="flex flex-wrap gap-x-5 gap-y-2 pl-5">{items.map(item => <label key={quotaKey(item)} className="flex items-center gap-2 text-xs"><input type="checkbox" aria-label={`关注 ${item.channel_name} ${item.credential_name}`} checked={selected.has(quotaKey(item))} onChange={() => toggle([item])} />{item.credential_name}{!item.enabled && <span className="text-[var(--color-text-muted)]">已停用</span>}</label>)}</div>
      </fieldset>;
    })}
    {draft.some(target => !quotas.items.some(item => quotaKey(item) === quotaKey(target))) && <div className="text-xs text-[var(--color-text-muted)]">部分关注凭证已不可用。<Button variant="ghost" disabled={saving} onClick={() => setDraft(current => current.filter(target => quotas.items.some(item => quotaKey(item) === quotaKey(target))))}>移除不可用项</Button></div>}
    <div className="flex justify-end gap-2"><Button variant="secondary" disabled={!dirty || saving} onClick={discard}>取消更改</Button><Button disabled={!dirty || saving || preferences.value.revision < 0} onClick={() => void save()}>{saving ? '保存中…' : '保存关注'}</Button></div>
  </section>;
}
