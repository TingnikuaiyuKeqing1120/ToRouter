import { useEffect, useState } from 'react';
import { api, errorMessage, type ClientKeyPreview, type ClientKeyScope, type DownstreamModelView, type TagGroupWithTags } from '../api';
import { useResource } from '../hooks';
import { editableTagGroups } from '../services/tagAssignment';
import { TagGroupOptions } from './TagGroupSelector';
import { Button, ErrorBox, Input } from './ui';

export function ClientKeyScopeEditor({ scope, onChange, models, modelsError, reloadModels }: { scope: ClientKeyScope; onChange: (scope: ClientKeyScope) => void; models: DownstreamModelView[]; modelsError?: string; reloadModels: () => void }) {
  const tags = useResource<TagGroupWithTags[]>('/tag-groups');
  const [search, setSearch] = useState('');
  const [preview, setPreview] = useState<ClientKeyPreview | null>(null);
  const [previewError, setPreviewError] = useState('');
  const [loading, setLoading] = useState(true);
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const serialized = JSON.stringify(scope);
  useEffect(() => {
    let active = true;
    setLoading(true); setPreviewError(''); setPreview(null);
    const timer = setTimeout(() => {
      api<ClientKeyPreview>('/client-keys/preview', { method: 'POST', body: JSON.stringify({ scope: JSON.parse(serialized) }) })
        .then(result => { if (active) setPreview(result); })
        .catch(error => { if (active) setPreviewError(errorMessage(error)); })
        .finally(() => { if (active) setLoading(false); });
    }, 250);
    return () => { active = false; clearTimeout(timer); };
  }, [serialized, attempt]);
  const groups = editableTagGroups(tags.data || []);
  const modelsByID = new Map(models.map(model => [model.id, model]));
  const choices = [...new Set([...models.filter(model => model.enabled).map(model => model.id), ...scope.model_ids])].filter(id => id.toLowerCase().includes(search.toLowerCase()));
  function setRule(id: string, values?: string[]) {
    const next = { ...scope.tag_rules };
    if (values === undefined) delete next[id]; else next[id] = values;
    onChange({ ...scope, tag_rules: next });
  }
  return <div className="client-scope-editor">
    <section className="client-scope-section"><h3>公开模型范围</h3>
      <div className="client-scope-modes" role="group" aria-label="公开模型范围">
        <Button type="button" variant="secondary" aria-pressed={scope.model_mode === 'all'} onClick={() => onChange({ ...scope, model_mode: 'all', model_ids: [] })}>全部公开模型</Button>
        <Button type="button" variant="secondary" aria-pressed={scope.model_mode === 'selected'} onClick={() => onChange({ ...scope, model_mode: 'selected' })}>指定公开模型</Button>
      </div>
      <p className="client-scope-note">{scope.model_mode === 'all' ? '包含今后新增的公开模型。' : scope.model_ids.length ? `已指定 ${scope.model_ids.length} 个公开模型；今后新增项不会自动纳入。` : '未选择任何公开模型：此 Key 不允许访问任何模型。'}</p>
      {scope.model_mode === 'selected' && <>
        <Input aria-label="搜索公开模型" placeholder="搜索公开模型 ID…" value={search} onChange={event => setSearch(event.target.value)} />
        {modelsError && <ErrorBox message={modelsError} retry={reloadModels} />}
        <div className="client-model-choices">{choices.map(id => <label key={id}><input type="checkbox" checked={scope.model_ids.includes(id)} onChange={() => onChange({ ...scope, model_ids: scope.model_ids.includes(id) ? scope.model_ids.filter(value => value !== id) : [...scope.model_ids, id] })} /><span>{id}{!modelsByID.has(id) ? '（已移除）' : !modelsByID.get(id)?.enabled ? '（已停用）' : ''}</span></label>)}{!choices.length && <p className="client-scope-note">没有匹配的公开模型</p>}</div>
      </>}
    </section>
    <section className="client-scope-section"><h3>上游标签条件</h3><p className="client-scope-note">同组允许任一，组间同时满足。判断上游模型最终标签；未知仅在明确允许或该组不限时通过。</p>
      {tags.error && <ErrorBox message={tags.error} retry={tags.reload} />}
      {tags.loading && !tags.data && <p role="status" className="client-scope-note">加载标签组中…</p>}
      {groups.map(group => {
        const restricted = Object.prototype.hasOwnProperty.call(scope.tag_rules, group.id), allowed = scope.tag_rules[group.id] || [];
        return <div className="client-rule" key={group.id}>
          <div className="client-rule-heading"><strong>{group.name}</strong><div className="client-scope-modes"><Button type="button" variant="secondary" aria-pressed={!restricted} onClick={() => setRule(group.id)}>不限制</Button><Button type="button" variant="secondary" aria-pressed={restricted} onClick={() => setRule(group.id, allowed)}>只允许所选</Button></div></div>
          {restricted && <><TagGroupOptions groups={[group]} selectedTags={allowed} onChange={values => setRule(group.id, values)} />{!allowed.length && <p className="client-scope-note client-scope-warning">此组限制为空：没有上游符合规则。</p>}{allowed.filter(id => !group.tags.some(tag => tag.id === id)).map(id => <Button key={id} type="button" variant="secondary" onClick={() => setRule(group.id, allowed.filter(value => value !== id))}>{id}（缺失，点击移除）</Button>)}</>}
        </div>;
      })}
      {!!tags.data && Object.keys(scope.tag_rules).filter(id => !groups.some(group => group.id === id)).map(id => <div className="client-rule" key={id}><strong>缺失的标签组：{id}</strong><p className="client-scope-note">当前规则保留，无法匹配。请处理引用后保存。</p><Button type="button" variant="secondary" onClick={() => setRule(id)}>取消此组限制</Button></div>)}
    </section>
    <section className="client-scope-section client-scope-preview" aria-label="范围匹配预览"><div className="client-rule-heading"><h3>范围匹配</h3><Button type="button" variant="ghost" disabled={loading} onClick={() => setAttempt(value => value + 1)}>刷新匹配</Button></div>
      <div className="client-preview-status">{loading ? <p role="status" className="client-scope-note">正在计算符合规则的模型…</p> : previewError ? <ErrorBox message={`匹配预览失败：${previewError}`} retry={() => setAttempt(value => value + 1)} /> : preview && <><p role="status"><strong>{preview.allowed_models}</strong> / {preview.total_models} 个公开模型符合规则</p><p className="client-scope-note">按静态绑定匹配，不代表上游实时可用性。</p></>}</div>
      <details open={detailsOpen} onToggle={event => setDetailsOpen(event.currentTarget.open)}><summary>查看模型与不匹配原因</summary><div className="client-preview-models">{loading ? <p className="client-scope-note">正在计算当前范围…</p> : previewError ? <p className="client-scope-note">请重试匹配预览。</p> : preview?.models.map(model => <div key={model.model_id}><strong>{model.model_id}</strong><span>{model.allowed ? '符合规则' : model.reason || '不符合规则'}</span>{model.bindings.map(binding => <p key={`${binding.channel_id}/${binding.model_id}`}>{binding.channel_name} · {binding.model_id}：{binding.allowed ? '符合规则' : binding.reasons.join('；') || '不符合规则'}</p>)}</div>)}</div></details>
    </section>
  </div>;
}
