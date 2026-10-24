import { useState, useMemo, useRef, useEffect } from 'react';
import { AudioLines, ChevronRight, Download, FileText, GripVertical, Image, Pencil, Plus, RotateCcw, Trash2, Type, Upload, Video } from 'lucide-react';
import { api, configLabel, errorMessage, type Binding, type Channel, type DownstreamModel, type DownstreamModelView, type PublicModelSpecs } from '../api';
import { useResource } from '../hooks';
import { usePlugins } from '../hooks/usePlugins';
import { Badge, Button, Empty, ErrorBox, Field, Input, Loading, Modal, PageHeader, Select, useToast } from '../components/ui';
import { ProviderIcon } from '../components/ProviderIcons';
import { useConfirm } from '../components/Confirm';
import { useUnsavedChanges } from '../hooks/useUnsavedChanges';
import { useSortableList } from '../hooks/useSortableList';
import { enabledChannelModels } from '../services/modelCandidates';
import { managementLink } from '../services/managementLinks';
import { TagBadge } from '../components/TagBadge';
import { TagGroupSelector, TagGroupOptions } from '../components/TagGroupSelector';
import type { TagGroupWithTags } from '../types';
import './Models.css';

const emptyModel: DownstreamModel = { id: '', enabled: true, fallback: true, bindings: [], tags: [] };

function formatContext(spec?: PublicModelSpecs): string {
  const cw = spec?.context_length;
  if (cw == null) return '未知';
  if (cw === 0) return '0';
  if (cw >= 1_000_000) {
    const m = cw / 1_000_000;
    return `${m % 1 === 0 ? m.toFixed(0) : m.toFixed(2)}M`;
  }
  if (cw >= 1_000) {
    const k = cw / 1_000;
    return `${k % 1 === 0 ? k.toFixed(0) : k.toFixed(1)}K`;
  }
  return String(cw);
}

function renderModalities(spec?: PublicModelSpecs) {
  const inputs = spec?.modalities?.input;
  const outputs = spec?.modalities?.output;
  const all = new Set([...(inputs || []), ...(outputs || [])]);
  if (inputs != null && outputs != null && !all.size) return <span className="text-xs text-[var(--color-text-muted)]">空列表</span>;
  if (!all.size) return <span className="text-xs text-[var(--color-text-secondary)]">未知</span>;
  const modalities = [{ id: 'text', label: '文本', Icon: Type }, { id: 'image', label: '图像', Icon: Image }, { id: 'audio', label: '音频', Icon: AudioLines }, { id: 'file', label: '文件', Icon: FileText }, { id: 'video', label: '视频', Icon: Video }];
  return <div className="public-model-modalities">
    {modalities.filter(item => all.has(item.id)).map(({ id, label, Icon }) => <span key={id} title={`${label} · ${[inputs?.includes(id) ? '输入' : '', outputs?.includes(id) ? '输出' : ''].filter(Boolean).join(' / ')}`}><Icon size={16} aria-label={label} /></span>)}
  </div>;
}

function renderThinking(spec?: PublicModelSpecs) {
  const thinking = spec?.thinking;
  if (thinking?.supported === false) return <span className="text-xs text-[var(--color-text-muted)]">不支持</span>;
  const options = thinking?.options;
  if (options == null) return <span className="text-xs text-[var(--color-text-secondary)]">未知</span>;
  if (!options.length) return <span className="text-xs text-[var(--color-text-muted)]">空列表</span>;
  return <span className="inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-mono font-medium bg-purple-500/10 text-purple-600 dark:text-purple-400" title={`支持选项: ${options.join(', ')}`}>{options.length} 挡</span>;
}

function cleanModelPayload(m: DownstreamModelView | DownstreamModel, enabled = m.enabled): DownstreamModel {
  return {
    id: m.id.trim(),
    display_name: m.display_name?.trim() || '',
    enabled,
    fallback: m.fallback,
    catalog_model_ref: m.catalog_model_ref,
    sort_order: m.sort_order,
    tags: m.tags || [],
    bindings: (m.bindings || []).map(b => ({
      channel_id: b.channel_id,
      model: b.model,
      priority: b.priority,
      enabled: b.enabled,
    })),
  };
}

function ModelEditor({ model, channels, publicSpecs, onClose, onSaved }: { model: DownstreamModel | null; channels: Channel[]; publicSpecs: ReturnType<typeof useResource<{ data: PublicModelSpecs[] }>>; onClose: () => void; onSaved: () => void }) {
  const [form, setForm] = useState<DownstreamModel>(model || emptyModel);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const editing = !!model;
  const toast = useToast();
  const confirmAction = useConfirm();
  const dirty = JSON.stringify(cleanModelPayload(form)) !== JSON.stringify(cleanModelPayload(model || emptyModel));
  const canLeave = useUnsavedChanges({ dirty, busy, save });
  async function requestClose() { if (await canLeave()) onClose(); }

  const upstreamOptions = useMemo(() => {
    const result: { channelId: number; channelName: string; model: string }[] = [];
    const seen = new Set<string>();
    for (const c of channels) {
      const all = enabledChannelModels(c);
      for (const m of all) {
        const key = `${c.id}:${m}`;
        if (!seen.has(key)) {
          seen.add(key);
          result.push({ channelId: c.id, channelName: c.name, model: m });
        }
      }
    }
    return result;
  }, [channels]);

  function setBinding(index: number, patch: Partial<Binding>) {
    setForm(current => ({ ...current, bindings: current.bindings.map((b, i) => i === index ? { ...b, ...patch } : b) }));
  }
  function addBinding() {
    const channel = channels.find(c => enabledChannelModels(c).length > 0);
    if (!channel) return;
    const models = enabledChannelModels(channel);
    setForm(current => ({ ...current, bindings: [...current.bindings, { channel_id: channel.id, model: models[0] || '', priority: current.bindings.length, enabled: true }] }));
  }
  const [activeTab, setActiveTab] = useState<'routes' | 'tags' | 'specs'>('routes');
  const formRef = useRef<HTMLFormElement>(null);
  function invalid(message: string, selector: string) {
    setError(message); setActiveTab('routes');
    requestAnimationFrame(() => formRef.current?.querySelector<HTMLElement>(selector)?.focus());
    return false;
  }

  async function save(): Promise<boolean> {
    if (busy) return false;
    setError('');
    const id = form.id.trim();
    if (!id) return invalid('请填写对外模型 ID。', '[aria-label="对外模型 ID"]');
    if (!form.bindings.length) return invalid('至少需要一个上游绑定。', '[data-add-binding]');
    if (form.bindings.some(b => !b.model)) return invalid('绑定缺少上游模型。', '[aria-label="上游模型"]');
    if (form.bindings.some(b => !Number.isSafeInteger(b.priority))) return invalid('请填写整数优先级。', '[aria-label="优先级"]');
    if (editing && id !== model!.id) {
      const ok = await confirmAction({ title: '修改公开模型 ID？', description: `使用旧 ID「${model!.id}」的客户端将失效。新 ID「${id}」将承接当前官方 API 价，旧 ID 的价格与历史记录会保留。`, confirmLabel: '修改模型 ID', cancelLabel: '继续编辑' });
      if (!ok) return false;
    }
    setBusy(true);
    try {
      // The list view carries read-only fields (channel_exists, effective…);
      // send only the writable binding fields.
      const payload = { ...cleanModelPayload(form), id };
      if (editing && id !== model!.id) {
        const answer = await api<DownstreamModel & { notice?: string }>(`/models/${encodeURIComponent(model!.id)}/rename`, { method: 'POST', body: JSON.stringify(payload) });
        toast(answer.notice ? `模型 ID 已修改为 ${id}。${answer.notice}` : `模型 ID 已修改为 ${id}`);
        onSaved();
      } else {
        const answer = await api<DownstreamModel & { notice?: string }>(editing ? `/models/${encodeURIComponent(id)}` : '/models', { method: editing ? 'PUT' : 'POST', body: JSON.stringify(payload) });
        const done = editing ? '模型已保存' : '模型已创建';
        toast(answer.notice ? `${done}。${answer.notice}` : done);
        onSaved();
      }
      return true;
    } catch (e) { setError(errorMessage(e)); return false; } finally { setBusy(false); }
  }
  return <Modal title={editing ? `编辑公开模型 · ${model!.id}` : '新增公开模型'} description="设置客户端使用的模型 ID，以及对应的上游绑定。" onClose={requestClose} busy={busy} wide className="public-model-editor">
    <form ref={formRef} noValidate onSubmit={event => { event.preventDefault(); void save(); }} className="public-model-form">
      <fieldset disabled={busy} className="public-model-fields">
        <div className="public-model-workspace">
          {/* 左侧垂直索引导航栏 */}
          <nav className="public-model-nav" aria-label="公开模型配置分区">
            {[
              { id: 'routes' as const, label: '调用配置' },
              { id: 'tags' as const, label: '模型标签' },
              { id: 'specs' as const, label: '公开规格' },
            ].map(tab => (
              <button
                key={tab.id}
                type="button"
                aria-current={activeTab === tab.id ? 'page' : undefined}
                onClick={() => setActiveTab(tab.id)}
              >
                {tab.label}
              </button>
            ))}
          </nav>

          {/* 右侧面板内容 */}
          <div className="public-model-pages">
            {/* 1. 路由与绑定 */}
            <div className="public-model-page" hidden={activeTab !== 'routes'}>
              <section className="space-y-3.5" aria-label="基本配置">
                <div className="flex items-center justify-between border-b border-[var(--color-border)]/60 pb-1.5">
                  <h3>模型信息</h3>
                </div>

                {!editing && upstreamOptions.length > 0 && (
                  <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 p-2.5 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)]/50">
                    <div className="text-xs">
                      <span className="font-medium text-[var(--color-text-secondary)]">快捷填入：</span>
                      <span className="text-[11px] text-[var(--color-text-muted)] ml-1">从已启用模型填入 ID 和首个绑定</span>
                    </div>
                    <div className="w-full sm:w-64 shrink-0">
                      <Select
                        aria-label="从上游选择模型"
                        className="!h-8 text-xs"
                        value=""
                        onChange={e => {
                          const val = e.target.value;
                          if (!val) return;
                          const [cIdStr, ...mParts] = val.split(':');
                          const channelId = Number(cIdStr);
                          const modelName = mParts.join(':');
                          setForm(current => ({
                            ...current,
                            id: current.id.trim() ? current.id : modelName,
                            display_name: current.display_name?.trim() ? current.display_name : modelName,
                            bindings: current.bindings.length > 0
                              ? current.bindings
                              : [{ channel_id: channelId, model: modelName, priority: 0, enabled: true }],
                          }));
                        }}
                      >
                        <option value="">-- 选择上游模型快速填入 --</option>
                        {upstreamOptions.map(opt => (
                          <option key={`${opt.channelId}:${opt.model}`} value={`${opt.channelId}:${opt.model}`}>
                            {opt.model} ({opt.channelName})
                          </option>
                        ))}
                      </Select>
                    </div>
                  </div>
                )}

                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="对外模型 ID" hint={editing ? "客户端请求时使用的标识。修改对外 ID 将使原有客户端配置失效。" : "客户端请求时使用的模型名称。"}>
                    <Input
                      aria-label="对外模型 ID"
                      required
                      maxLength={200}
                      placeholder="例如：gpt-5"
                      className="font-mono text-xs"
                      value={form.id}
                      onChange={e => setForm(current => ({ ...current, id: e.target.value }))}
                    />
                    {editing && form.id.trim() !== model!.id && (
                      <p className="text-xs text-amber-600 dark:text-amber-400 mt-1">
                        ⚠️ 警告：修改对外模型 ID 会导致现有调用该 ID 的客户端失效，新 ID 将承接当前官方 API 价，旧 ID 的价格与历史记录会保留。
                      </p>
                    )}
                  </Field>
                  <Field label="显示名" hint="控制台或友好展示名称（选填）。">
                    <Input
                      aria-label="模型显示名"
                      maxLength={200}
                      placeholder="例如：GPT-5 旗舰版"
                      className="text-xs"
                      value={form.display_name || ''}
                      onChange={e => setForm(current => ({ ...current, display_name: e.target.value }))}
                    />
                  </Field>
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 pt-1">
                  <label className={`flex items-start gap-3 p-3 rounded-lg border cursor-pointer transition-all ${
                    form.enabled
                      ? 'border-[var(--brand-blue)]/50 bg-[var(--brand-blue)]/5'
                      : 'border-[var(--color-border)] bg-[var(--color-surface-1)] hover:border-[var(--color-border-strong)]'
                  }`}>
                    <input
                      className="checkbox mt-0.5 shrink-0"
                      type="checkbox"
                      checked={form.enabled}
                      onChange={e => setForm(current => ({ ...current, enabled: e.target.checked }))}
                    />
                    <div>
                      <div className="text-xs font-semibold text-[var(--color-text-primary)]">启用此公开模型</div>
                      <div className="text-[11px] text-[var(--color-text-muted)] mt-0.5">
                        关闭后不参与路由调度，也不在 /v1/models 接口暴露
                      </div>
                    </div>
                  </label>

                  <label className={`flex items-start gap-3 p-3 rounded-lg border cursor-pointer transition-all ${
                    form.fallback
                      ? 'border-[var(--brand-blue)]/50 bg-[var(--brand-blue)]/5'
                      : 'border-[var(--color-border)] bg-[var(--color-surface-1)] hover:border-[var(--color-border-strong)]'
                  }`}>
                    <input
                      className="checkbox mt-0.5 shrink-0"
                      type="checkbox"
                      checked={form.fallback}
                      onChange={e => setForm(current => ({ ...current, fallback: e.target.checked }))}
                    />
                    <div>
                      <div className="text-xs font-semibold text-[var(--color-text-primary)]">允许故障回退</div>
                      <div className="text-[11px] text-[var(--color-text-muted)] mt-0.5">
                        高优先级绑定失败时继续尝试后续可用绑定
                      </div>
                    </div>
                  </label>
                </div>
              </section>

              <section className="space-y-3" aria-label="上游绑定">
                <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-border)]/60 pb-1.5">
                  <div>
                    <h3 className="text-xs font-semibold text-[var(--color-text-primary)]">
                      上游绑定 · {form.bindings.length}
                    </h3>
                    <p className="text-[11px] text-[var(--color-text-muted)]">
                      优先级数字从小到大尝试；同优先级按行序。可单停用某绑定而不删除。
                    </p>
                  </div>
                  <Button
                    type="button"
                    variant="secondary"
                    className="!h-7 !text-xs !py-0 !px-2.5"
                    onClick={addBinding}
                    data-add-binding
                    disabled={!upstreamOptions.length}
                  >
                    <Plus size={14} />
                    添加绑定
                  </Button>
                </div>

                {!upstreamOptions.length && (
                  <p className="text-xs text-amber-700 dark:text-amber-400 p-2 rounded bg-amber-500/10 border border-amber-500/20">
                    暂无已启用的上游模型，请先在上游接入中启用模型。
                  </p>
                )}

                {form.bindings.length > 0 && (
                  <div className="rounded-lg border border-[var(--color-border)] overflow-hidden bg-[var(--color-surface-1)]">
                    <div className="hidden sm:grid sm:grid-cols-[1.2fr_1.2fr_75px_70px_36px] gap-2 px-3 py-1.5 bg-[var(--color-surface-2)]/70 text-[11px] font-semibold text-[var(--color-text-muted)] border-b border-[var(--color-border)]/60">
                      <span>上游渠道</span>
                      <span>上游模型</span>
                      <span className="text-center">优先级</span>
                      <span className="text-center">状态</span>
                      <span className="text-right">操作</span>
                    </div>

                    <div className="divide-y divide-[var(--color-border)]/40">
                      {form.bindings.map((b, index) => {
                        const channel = channels.find(c => c.id === b.channel_id);
                        const models = channel ? enabledChannelModels(channel) : [];
                        return (
                          <div
                            key={index}
                            className="p-3 sm:py-2 sm:px-3 grid gap-2 sm:grid-cols-[1.2fr_1.2fr_75px_70px_36px] items-center hover:bg-[var(--color-surface-2)]/30 transition-colors"
                          >
                            <div>
                              <span className="sm:hidden text-[10px] text-[var(--color-text-muted)] block mb-0.5">提供商</span>
                              <Select
                                aria-label="上游提供商"
                                className="!h-8 text-xs w-full"
                                value={String(b.channel_id)}
                                onChange={e => {
                                  const next = channels.find(c => c.id === Number(e.target.value));
                                  const nextModels = next ? enabledChannelModels(next) : [];
                                  setBinding(index, { channel_id: Number(e.target.value), model: nextModels[0] || '' });
                                }}
                              >
                                {!channel && <option value={b.channel_id}>渠道 {b.channel_id}（已移除）</option>}
                                {channels.filter(c => c.id === b.channel_id || enabledChannelModels(c).length).map(c => <option key={c.id} value={c.id}>{c.name}{c.enabled ? '' : '（渠道已停用）'}</option>)}
                              </Select>
                            </div>

                            <div>
                              <span className="sm:hidden text-[10px] text-[var(--color-text-muted)] block mb-0.5">上游模型</span>
                              <Select
                                aria-label="上游模型"
                                className="!h-8 text-xs font-mono w-full"
                                value={b.model}
                                onChange={e => setBinding(index, { model: e.target.value })}
                              >
                                {!b.model && <option value="">请选择已启用模型</option>}
                                {b.model && !models.includes(b.model) && <option value={b.model}>{b.model}（未启用，保留已有绑定）</option>}
                                {models.map(m => <option key={m} value={m}>{m}</option>)}
                              </Select>
                            </div>

                            <div>
                              <span className="sm:hidden text-[10px] text-[var(--color-text-muted)] block mb-0.5">优先级</span>
                              <Input
                                aria-label="优先级"
                                type="number"
                                step={1}
                                className="!h-8 text-xs font-mono text-center w-full"
                                value={b.priority}
                                onChange={e => setBinding(index, { priority: Number(e.target.value) })}
                              />
                            </div>

                            <div className="flex items-center justify-start sm:justify-center">
                              <label
                                className="flex items-center gap-1.5 cursor-pointer text-xs"
                                title={b.enabled ? '已启用该链路' : '已停用该链路'}
                              >
                                <input
                                  className="checkbox shrink-0"
                                  type="checkbox"
                                  checked={b.enabled}
                                  onChange={e => setBinding(index, { enabled: e.target.checked })}
                                />
                                <span className={`text-[11px] ${b.enabled ? 'text-[var(--color-text-primary)] font-medium' : 'text-[var(--color-text-muted)]'}`}>
                                  {b.enabled ? '启用' : '停用'}
                                </span>
                              </label>
                            </div>

                            <div className="flex items-center justify-end">
                              <Button
                                type="button"
                                variant="ghost"
                                className="!min-h-[30px] !h-7 !w-7 !p-0 text-[var(--color-text-muted)] hover:!text-rose-500 rounded"
                                aria-label={`删除绑定 ${index + 1}`}
                                onClick={() => setForm(current => ({ ...current, bindings: current.bindings.filter((_, i) => i !== index) }))}
                                title="删除此绑定"
                              >
                                <Trash2 size={14} />
                              </Button>
                            </div>
                          </div>
                        );
                      })}
                    </div>
                  </div>
                )}
              </section>
            </div>

            <div className="public-model-page" hidden={activeTab !== 'tags'}>
              <h3>自定义标签</h3>
              <p className="public-model-note">仅用于此公开模型的分类，不继承上游标签。</p>
              <TagGroupSelector selectedTags={form.tags || []} onChange={tags => setForm(f => ({ ...f, tags }))} label="" groupType="user" showEmptyState />
              <p className="public-model-note">模态支持由公开规格生成，可在“公开规格”中查看。</p>
            </div>
            <div className="public-model-page" hidden={activeTab !== 'specs'}>
              <p className="text-xs text-[var(--color-text-muted)]">公开规格由已保存的有效绑定逐字段比较得出。存在差异时建议拆分公开 ID，未知项请到参数配置页补全。</p>
              {publicSpecs.error ? <ErrorBox message={publicSpecs.error} retry={publicSpecs.reload} /> : publicSpecs.loading && !publicSpecs.data ? <Loading /> : <PublicSpecsSummary spec={publicSpecs.data?.data?.find(spec => spec.id === model?.id)} />}
              <div className="flex flex-wrap gap-2">{form.bindings.map((binding, index) => <Button key={index} type="button" variant="secondary" disabled={!editing || dirty} title={dirty || !editing ? '请先保存公开模型更改' : undefined} onClick={() => { window.location.hash = managementLink('model-specs', binding.channel_id, binding.model); }}>规格 · {channels.find(channel => channel.id === binding.channel_id)?.name || binding.channel_id} / {binding.model} ↗</Button>)}</div>
              <Button type="button" variant="secondary" disabled={!editing || dirty} title={dirty || !editing ? '请先保存公开模型更改' : undefined} onClick={() => { window.location.hash = managementLink('prices', 0, form.id); }}>管理官方 API 价 ↗</Button>
              {(!editing || dirty) && <p className="text-xs text-[var(--color-text-muted)]">请先保存公开模型更改后前往规格或价格页。</p>}
            </div>
          </div>
        </div>
      </fieldset>
      {error && <div className="public-model-error"><ErrorBox message={error} /></div>}
      <div className="modal-footer">
        <Button type="button" variant="secondary" onClick={requestClose} disabled={busy}>取消</Button>
        <Button type="submit" disabled={busy}>{busy ? '正在保存…' : editing ? '保存更改' : '创建模型'}</Button>
      </div>
    </form>
  </Modal>;
}

function EffectiveSummary({ view }: { view: DownstreamModelView }) {
  const active = view.bindings.filter(b => b.enabled);
  if (!active.length) return <p className="text-xs text-[var(--color-text-muted)]">没有启用的绑定。</p>;
  return <div className="space-y-2.5"><p className="text-xs text-[var(--color-text-muted)]">各绑定的具体上游模型规格。请求兜底统一在系统设置管理。</p>{active.map((b, index) => <div key={index} className="rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3 text-xs">
    <div className="flex flex-wrap items-center gap-2">
      <span className="font-mono text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-surface-1)] border border-[var(--color-border)] text-[var(--color-text-muted)]">优先级 {b.priority}</span>
      <p className="font-medium text-[var(--color-text-primary)]">
        {b.channel_name || `渠道 ${b.channel_id}`} · <span className="font-mono text-[var(--brand-blue)] font-semibold">{b.model}</span>
        {!b.channel_exists && <span className="ml-2 text-rose-500 font-normal">渠道已删除，该绑定无效</span>}
      </p>
    </div>
    {b.effective && <dl className="mt-2.5 grid gap-x-6 gap-y-1.5 sm:grid-cols-2 border-t border-[var(--color-border)]/60 pt-2">{Object.entries(b.effective.config).map(([key, value]) => <div key={key} className="flex items-center gap-2"><dt className="text-[var(--color-text-muted)]">{configLabel(key)}</dt><dd className="font-mono text-[var(--color-text-primary)]">{Array.isArray(value) ? value.join('/') || '（空）' : typeof value === 'object' ? JSON.stringify(value) : String(value)}<span className="ml-2 text-[var(--color-text-muted)] text-[11px]">来源：{b.effective!.sources[key] || '具体上游模型'}</span></dd></div>)}</dl>}
  </div>)}</div>;
}

function ImportModal({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const [mode, setMode] = useState<'merge' | 'overwrite'>('merge');
  const [jsonText, setJsonText] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  const parsed = useMemo<{ models: DownstreamModel[]; valid: boolean; errorMsg?: string }>(() => {
    const trimmed = jsonText.trim();
    if (!trimmed) return { models: [], valid: false };
    try {
      const data = JSON.parse(trimmed);
      let list: unknown[] = [];
      if (Array.isArray(data)) {
        list = data;
      } else if (data && typeof data === 'object' && Array.isArray((data as { models?: unknown[] }).models)) {
        list = (data as { models: unknown[] }).models;
      } else {
        return { models: [], valid: false, errorMsg: 'JSON 必须是模型数组或包含 models 数组的备份对象。' };
      }

      if (!Array.isArray(data) && 'prices' in data && data.prices !== null && !Array.isArray(data.prices)) return { models: [], valid: false, errorMsg: '备份 prices 必须是价格数组。' };
      const validModels: DownstreamModel[] = [];
      for (let i = 0; i < list.length; i++) {
        const item = list[i] as Record<string, unknown>;
        if (!item || typeof item !== 'object' || typeof item.id !== 'string' || !item.id.trim()) {
          return { models: [], valid: false, errorMsg: `第 ${i + 1} 个模型缺少合法的 id 字段。` };
        }
        validModels.push({
          id: item.id.trim(),
          display_name: typeof item.display_name === 'string' ? item.display_name.trim() : '',
          enabled: item.enabled !== false,
          fallback: item.fallback !== false,
          sort_order: typeof item.sort_order === 'number' ? item.sort_order : i,
          catalog_model_ref: item.catalog_model_ref as DownstreamModel['catalog_model_ref'],
          tags: Array.isArray(item.tags) ? item.tags.filter((tag): tag is string => typeof tag === 'string') : [],
          bindings: Array.isArray(item.bindings) ? (item.bindings as Binding[]) : [],
        });
      }
      return { models: validModels, valid: true };
    } catch (e) {
      return { models: [], valid: false, errorMsg: 'JSON 格式解析错误：' + (e as Error).message };
    }
  }, [jsonText]);

  const handleFileUpload = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = evt => {
      setJsonText(String(evt.target?.result || ''));
      setError('');
    };
    reader.onerror = () => setError('读取文件失败');
    reader.readAsText(file);
  };

  const handleImport = async (): Promise<boolean> => {
    if (busy) return false;
    if (!parsed.valid || !parsed.models.length) {
      setError(parsed.errorMsg || '请先提供有效的模型配置 JSON');
      return false;
    }
    setBusy(true);
    setError('');
    try {
      await api('/models/import', {
        method: 'POST',
        body: JSON.stringify({
          models: parsed.models,
          mode,
          ...(Array.isArray(JSON.parse(jsonText)?.prices) ? { prices: JSON.parse(jsonText).prices } : {}),
        }),
      });
      toast(`成功导入 ${parsed.models.length} 个模型配置`);
      onSaved();
      return true;
    } catch (err) {
      setError(errorMessage(err));
      return false;
    } finally {
      setBusy(false);
    }
  };
  const canLeave = useUnsavedChanges({ dirty: !!jsonText.trim(), busy, save: handleImport, prompt: { title: '导入当前配置？', description: mode === 'overwrite' ? '将按已选择的覆盖模式导入；关闭此提示可继续检查。' : '将合并当前配置；关闭此提示可继续检查。', confirmLabel: '放弃导入', saveLabel: '确认导入' } });
  async function requestClose() { if (await canLeave()) onClose(); }

  return (
    <Modal
      title="恢复 / 导入公开模型配置"
      description="从全量 JSON 备份导入公开模型与绑定关系。"
      onClose={requestClose}
      busy={busy}
      wide
    >
      <form onSubmit={e => { e.preventDefault(); void handleImport(); }} className="space-y-4">
        {error && <ErrorBox message={error} />}
        <fieldset disabled={busy} className="space-y-4">
          <Field label="导入模式">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 pt-1">
              <label
                className={`flex items-start gap-3 p-3 rounded-lg border cursor-pointer transition-all ${
                  mode === 'merge'
                    ? 'border-[var(--brand-blue)] bg-[var(--brand-blue)]/5'
                    : 'border-[var(--color-border)] hover:border-[var(--color-border-strong)]'
                }`}
              >
                <input
                  type="radio"
                  name="importMode"
                  value="merge"
                  checked={mode === 'merge'}
                  onChange={() => setMode('merge')}
                  className="mt-0.5"
                />
                <div>
                  <div className="text-sm font-semibold text-[var(--color-text-primary)]">增量合并 (Merge)</div>
                  <div className="text-xs text-[var(--color-text-muted)] mt-0.5">
                    保留未在文件中的现有模型，相同 ID 的模型按导入文件覆盖更新。
                  </div>
                </div>
              </label>

              <label
                className={`flex items-start gap-3 p-3 rounded-lg border cursor-pointer transition-all ${
                  mode === 'overwrite'
                    ? 'border-rose-500 bg-rose-500/5'
                    : 'border-[var(--color-border)] hover:border-[var(--color-border-strong)]'
                }`}
              >
                <input
                  type="radio"
                  name="importMode"
                  value="overwrite"
                  checked={mode === 'overwrite'}
                  onChange={() => setMode('overwrite')}
                  className="mt-0.5"
                />
                <div>
                  <div className="text-sm font-semibold text-rose-500">覆盖还原 (Overwrite)</div>
                  <div className="text-xs text-[var(--color-text-muted)] mt-0.5">
                    清空系统当前所有公开模型，完全替换为导入文件中的配置。
                  </div>
                </div>
              </label>
            </div>
          </Field>

          <Field label="选择备份文件 (.json)" hint="支持直接选取此前导出的 JSON 文件">
            <input
              type="file"
              accept=".json,application/json"
              className="input file:mr-3 file:py-1 file:px-3 file:rounded-md file:border-0 file:text-xs file:font-semibold file:bg-[var(--color-surface-2)] file:text-[var(--color-text-primary)] hover:file:bg-[var(--color-surface-3)] cursor-pointer"
              onChange={handleFileUpload}
            />
          </Field>

          <Field
            label="或直接粘贴 JSON 内容"
            hint={
              parsed.valid
                ? `已成功识别 ${parsed.models.length} 个模型配置`
                : parsed.errorMsg
                ? parsed.errorMsg
                : '格式支持标准备份包格式或模型对象数组'
            }
          >
            <textarea
              rows={6}
              className="input font-mono text-xs w-full py-2"
              placeholder="[ { &quot;id&quot;: &quot;gpt-4o&quot;, ... } ] 或 { &quot;models&quot;: [...] }"
              value={jsonText}
              onChange={e => {
                setJsonText(e.target.value);
                setError('');
              }}
            />
          </Field>
        </fieldset>

        <div className="modal-footer">
          <Button type="button" variant="secondary" onClick={requestClose} disabled={busy}>
            取消
          </Button>
          <Button
            type="submit"
            variant={mode === 'overwrite' ? 'danger' : 'primary'}
            disabled={busy || !parsed.valid || !parsed.models.length}
          >
            {busy ? '正在导入…' : mode === 'overwrite' ? `确认覆盖导入 (${parsed.models.length} 个模型)` : `确认合并导入 (${parsed.models.length} 个模型)`}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

function publicSpecValue(field: string, value: unknown): string {
  if (value == null) return '未知';
  if (typeof value === 'boolean') return field === 'thinking_mandatory' ? value ? '必需' : '可关闭' : value ? '支持' : '不支持';
  if (Array.isArray(value)) return value.length ? value.map(item => typeof item === 'object' ? JSON.stringify(item) : String(item)).join(' / ') : '空列表';
  return typeof value === 'object' ? JSON.stringify(value) : String(value);
}

function PublicSpecsSummary({ spec }: { spec?: PublicModelSpecs }) {
  if (!spec) return <p className="text-xs text-[var(--color-text-muted)]">尚无公开规格；停用的模型不会公开。</p>;
  const fields = { context_window: spec.context_length, max_input_tokens: spec.max_input_tokens, max_output_tokens: spec.max_output_tokens, streaming: spec.streaming, input_modalities: spec.modalities?.input, output_modalities: spec.modalities?.output, thinking_efforts: spec.thinking?.options };
  const coveredMessages = new Set(['bindings have differing or unknown specifications', 'Split differing specifications into separate public model IDs; complete unknown upstream model specifications.']);
  const messages = [...new Set([...(spec.warnings || []), spec.recommendation].filter((message): message is string => !!message && !coveredMessages.has(message)))];
  return <section className="space-y-3" aria-label="只读公开规格"><dl className="form-grid">{Object.entries(fields).map(([key, value]) => <div key={key} className="text-xs"><dt className="text-[var(--color-text-muted)]">{configLabel(key)}</dt><dd className="font-mono pt-1">{publicSpecValue(key, value)}</dd></div>)}</dl>{!!spec.differences?.length && <details className="advanced"><summary>绑定规格差异 · {spec.differences.length}</summary>{spec.differences.map(diff => <p key={diff.field} className="text-xs py-1">{configLabel(diff.field)}：{diff.bindings.map(binding => `${binding.name} / ${binding.model} = ${binding.known ? publicSpecValue(diff.field, binding.value) : '未知'}`).join('；')}</p>)}</details>}{messages.map(message => <p key={message} className="text-xs text-amber-600">{message}</p>)}</section>;
}

export type ModelSortOption = 'default' | 'id_asc' | 'id_desc' | 'bindings_desc' | 'enabled_first';

export const modelSortOptions: { value: ModelSortOption; label: string }[] = [
  { value: 'default', label: '默认顺序' },
  { value: 'id_asc', label: '模型 ID 升序 (A-Z)' },
  { value: 'id_desc', label: '模型 ID 降序 (Z-A)' },
  { value: 'bindings_desc', label: '绑定上游数 (多到少)' },
  { value: 'enabled_first', label: '启用状态优先' },
];

export default function Models() {
  const { data, setData, loading, error, reload: reloadModels } = useResource<DownstreamModelView[]>('/models');
  const publicSpecs = useResource<{ data: PublicModelSpecs[] }>('/models/specs');
  const specsByID = useMemo(() => new Map((publicSpecs.data?.data || []).map(spec => [spec.id, spec])), [publicSpecs.data]);
  function reload() { reloadModels(); publicSpecs.reload(); }
  const { data: channels } = useResource<Channel[]>('/channels');
  const channelDisplay = useResource<{ icons: Record<string, string> }>('/channels/display');
  const plugins = usePlugins();
  const [editing, setEditing] = useState<DownstreamModel | null>(null);
  const [creating, setCreating] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [search, setSearch] = useState('');
  const [sortBy, setSortBy] = useState<ModelSortOption>('default');
  const [expandedIds, setExpandedIds] = useState<Set<string>>(() => new Set());
  const [busyId, setBusyId] = useState('');
  const [actionError, setActionError] = useState('');
  const toast = useToast();

  // 多选与批量操作
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() => new Set());

  // 5秒无感撤销 (Undo Snackbar)
  const [pendingDeletion, setPendingDeletion] = useState<{
    ids: string[];
    label: string;
    remaining: number;
  } | null>(null);

  const pendingDeletionRef = useRef<{
    ids: string[];
    label: string;
    timeoutId: ReturnType<typeof setTimeout>;
    intervalId: ReturnType<typeof setInterval>;
  } | null>(null);

  useEffect(() => {
    return () => {
      if (pendingDeletionRef.current) {
        clearTimeout(pendingDeletionRef.current.timeoutId);
        clearInterval(pendingDeletionRef.current.intervalId);
        const { ids } = pendingDeletionRef.current;
        if (ids.length === 1) {
          api(`/models/${encodeURIComponent(ids[0])}`, { method: 'DELETE' }).catch(() => {});
        } else {
          api('/models/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) }).catch(() => {});
        }
      }
    };
  }, []);

  const [selectedTags, setSelectedTags] = useState<string[]>([]);
  const [filtersExpanded, setFiltersExpanded] = useState(true);
  const tagResource = useResource<TagGroupWithTags[]>('/tag-groups');
  const tagGroups = useMemo(() => (Array.isArray(tagResource.data) ? tagResource.data : []).filter(group => group.type === 'user' || group.id === 'modality'), [tagResource.data]);
  const tagByID = useMemo(() => new Map(tagGroups.flatMap(group => group.tags).map(tag => [tag.id, tag])), [tagGroups]);

  // 前端即时筛选与排序：绝不改变底层 payload 与 bindings 的 fallback 顺序
  const rows = useMemo(() => {
    let list = (data || []).filter(m => `${m.id} ${m.display_name || ''}`.toLowerCase().includes(search.toLowerCase()));
    const filters = tagGroups.map(group => group.tags.filter(tag => selectedTags.includes(tag.id)).map(tag => tag.id)).filter(tags => tags.length);
    list = list.filter(model => filters.every(tags => tags.some(tag => model.effective_tags?.includes(tag))));
    if (sortBy === 'default') return list;
    return [...list].sort((a, b) => {
      switch (sortBy) {
        case 'id_asc':
          return a.id.localeCompare(b.id);
        case 'id_desc':
          return b.id.localeCompare(a.id);
        case 'bindings_desc': {
          const diff = (b.bindings?.length || 0) - (a.bindings?.length || 0);
          return diff !== 0 ? diff : a.id.localeCompare(b.id);
        }
        case 'enabled_first': {
          const diff = (b.enabled ? 1 : 0) - (a.enabled ? 1 : 0);
          return diff !== 0 ? diff : a.id.localeCompare(b.id);
        }
        default:
          return 0;
      }
    });
  }, [data, search, sortBy, selectedTags, tagGroups]);

  // 过滤当前处于 5 秒可撤回等待期的模型
  const visibleRows = useMemo(() => {
    if (!pendingDeletion) return rows;
    return rows.filter(m => !pendingDeletion.ids.includes(m.id));
  }, [rows, pendingDeletion]);

  useEffect(() => {
    const visible = new Set(visibleRows.map(row => row.id));
    setSelectedIds(current => {
      const next = new Set([...current].filter(id => visible.has(id)));
      return next.size === current.size ? current : next;
    });
  }, [visibleRows]);

  const canReorder = sortBy === 'default' && !search.trim() && !selectedTags.length && !pendingDeletion && !loading && !error;
  const sortRevision = useMemo(() => JSON.stringify([data?.map(model => [model.id, model.enabled, model.display_name, model.bindings, model.tags]), [...expandedIds]]), [data, expandedIds]);
  const sortable = useSortableList({ ids: visibleRows.map(model => model.id), disabled: !canReorder || !!busyId, revision: sortRevision, onCommit: saveModelOrder });

  const allSelected = visibleRows.length > 0 && visibleRows.every(r => selectedIds.has(r.id));
  const someSelected = visibleRows.some(r => selectedIds.has(r.id)) && !allSelected;

  function toggleExpand(id: string) {
    setExpandedIds(prev => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function toggleSelect(id: string) {
    setSelectedIds(prev => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function toggleSelectAll() {
    if (allSelected) {
      setSelectedIds(new Set());
    } else {
      setSelectedIds(new Set(visibleRows.map(r => r.id)));
    }
  }

  async function toggle(model: DownstreamModelView) {
    setBusyId(model.id);
    setActionError('');
    try {
      await api(`/models/${encodeURIComponent(model.id)}`, {
        method: 'PUT',
        body: JSON.stringify(cleanModelPayload(model, !model.enabled)),
      });
      reload();
    } catch (e) {
      setActionError(errorMessage(e));
    } finally {
      setBusyId('');
    }
  }

  async function handleBatchStatus(enabled: boolean) {
    if (selectedIds.size === 0) return;
    const ids = Array.from(selectedIds);
    setActionError('');
    try {
      await api('/models/batch/status', {
        method: 'PUT',
        body: JSON.stringify({ ids, enabled }),
      });
      toast(`已批量${enabled ? '启用' : '停用'} ${ids.length} 个模型`);
      setSelectedIds(new Set());
      reload();
    } catch (e) {
      setActionError(errorMessage(e));
    }
  }

  function handleBatchDelete() {
    if (selectedIds.size === 0) return;
    const ids = Array.from(selectedIds);
    requestDelete(ids, `${ids.length} 个模型`);
  }

  function commitDeletion(ids: string[]) {
    if (pendingDeletionRef.current) {
      clearTimeout(pendingDeletionRef.current.timeoutId);
      clearInterval(pendingDeletionRef.current.intervalId);
      pendingDeletionRef.current = null;
    }
    setPendingDeletion(null);
    const promise = ids.length === 1
      ? api(`/models/${encodeURIComponent(ids[0])}`, { method: 'DELETE' })
      : api('/models/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) });

    promise
      .then(() => reload())
      .catch(err => {
        setActionError(errorMessage(err));
        reload();
      });
  }

  function requestDelete(ids: string[], label: string) {
    if (pendingDeletionRef.current) {
      commitDeletion(pendingDeletionRef.current.ids);
    }

    setSelectedIds(prev => {
      const next = new Set(prev);
      ids.forEach(id => next.delete(id));
      return next;
    });

    const duration = 5;
    setPendingDeletion({ ids, label, remaining: duration });

    const intervalId = setInterval(() => {
      setPendingDeletion(curr => {
        if (!curr) return null;
        if (curr.remaining <= 1) return null;
        return { ...curr, remaining: curr.remaining - 1 };
      });
    }, 1000);

    const timeoutId = setTimeout(() => {
      clearInterval(intervalId);
      pendingDeletionRef.current = null;
      setPendingDeletion(null);
      const promise = ids.length === 1
        ? api(`/models/${encodeURIComponent(ids[0])}`, { method: 'DELETE' })
        : api('/models/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) });

      promise
        .then(() => {
          toast(ids.length === 1 ? '模型已删除' : `已批量删除 ${ids.length} 个模型`);
          reload();
        })
        .catch(err => {
          setActionError(errorMessage(err));
          reload();
        });
    }, duration * 1000);

    pendingDeletionRef.current = { ids, label, timeoutId, intervalId };
  }

  function handleUndo() {
    if (pendingDeletionRef.current) {
      clearTimeout(pendingDeletionRef.current.timeoutId);
      clearInterval(pendingDeletionRef.current.intervalId);
      pendingDeletionRef.current = null;
    }
    setPendingDeletion(null);
    toast('已撤销删除');
  }

  function handleCommitDeletionNow() {
    if (pendingDeletionRef.current) {
      commitDeletion(pendingDeletionRef.current.ids);
      toast('删除已生效');
    }
  }

  function handleExport() {
    if (!data || !data.length) {
      toast('当前没有可导出的公开模型');
      return;
    }
    const payload = {
      version: 2,
      exported_at: new Date().toISOString(),
      models: data.map(m => cleanModelPayload(m)),
      prices: data.flatMap(m => m.associated_prices || []),
    };
    const blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    const dateStr = new Date().toISOString().slice(0, 10);
    a.download = `to-router-models-backup-${dateStr}.json`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
    toast('已导出模型备份配置');
  }

  async function saveModelOrder(order: string[]) {
    setBusyId('reorder'); setActionError('');
    try {
      await api('/models/reorder', { method: 'PUT', body: JSON.stringify({ order }) });
      setData(current => {
        if (!current) return current;
        const positions = new Map(order.map((id, index) => [id, index]));
        return [...current].sort((a, b) => (positions.get(a.id) ?? current.length) - (positions.get(b.id) ?? current.length));
      });
      toast('显示顺序已保存'); reload();
    } catch (e) { setActionError(errorMessage(e)); throw e; } finally { setBusyId(''); }
  }

  async function moveModel(id: string, direction: number) {
    if (!canReorder || busyId || !data || sortable.dragging || sortable.pending) return;
    const list = [...data];
    const from = list.findIndex(model => model.id === id);
    const to = from + direction;
    if (from < 0 || to < 0 || to >= list.length) return;
    [list[from], list[to]] = [list[to], list[from]];
    try { await saveModelOrder(list.map(model => model.id)); } catch { /* The shared save displays the failure. */ }
  }

  if (loading && !data) return <Loading />;
  if (error && !data) return <ErrorBox message={error} retry={reload} />;

  return (
    <div className="public-models space-y-4">
      <PageHeader
        eyebrow="PUBLIC MODELS"
        title="公开模型"
        description="管理客户端使用的模型 ID、上游绑定和调用顺序。"
        actions={
          <>
            <Button variant="secondary" onClick={handleExport} title="备份当前所有公开模型为 JSON 文件">
              <Download size={15} />
              备份配置
            </Button>
            <Button variant="secondary" onClick={() => setImportOpen(true)} title="从 JSON 文件恢复或合并模型配置">
              <Upload size={15} />
              恢复配置
            </Button>
            <Button onClick={() => { setEditing(null); setCreating(true); }}>
              <Plus size={16} />
              新增公开模型
            </Button>
          </>
        }
      />
      {actionError && <ErrorBox message={actionError} />}
      {error && <ErrorBox message={`更新失败，当前显示上次结果：${error}`} retry={reload} />}

      <div className="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-3">
        <div className="flex-1">
          <Input aria-label="搜索模型" placeholder="搜索名称或 ID…" value={search} onChange={e => setSearch(e.target.value)} />
        </div>
        <div className="flex items-center gap-3 shrink-0 flex-wrap">
          <div className="flex items-center gap-1.5">
            <span className="text-xs text-[var(--color-text-muted)] whitespace-nowrap">排序：</span>
            <div className="w-48">
              <Select
                aria-label="排序模型"
                value={sortBy}
                onChange={e => setSortBy(e.target.value as ModelSortOption)}
              >
                {modelSortOptions.map(opt => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </Select>
            </div>
          </div>
        </div>
      </div>

      <section className="public-model-filters" aria-label="按标签筛选">
        <div className="public-model-filter-heading" data-expanded={filtersExpanded}>
          <button type="button" className="public-model-filter-toggle" aria-expanded={filtersExpanded} aria-controls="public-model-tag-options" onClick={() => setFiltersExpanded(expanded => !expanded)}><ChevronRight size={16} className={filtersExpanded ? 'rotate-90' : ''} />标签筛选{selectedTags.length > 0 && <span className="public-model-note">已选 {selectedTags.length} 项</span>}</button>
          {filtersExpanded && <span className="public-model-note">同组满足任一，跨组同时满足</span>}
          {selectedTags.length > 0 && <Button variant="ghost" onClick={() => setSelectedTags([])}>清除标签筛选 · {selectedTags.length}</Button>}
        </div>
        <div id="public-model-tag-options" hidden={!filtersExpanded}>{tagResource.error ? <ErrorBox message="标签读取失败" retry={tagResource.reload} /> : tagResource.loading && !tagResource.data ? <p className="public-model-note">正在读取标签…</p> : <TagGroupOptions groups={tagGroups} selectedTags={selectedTags} onChange={setSelectedTags} />}</div>
      </section>

      {publicSpecs.error && <ErrorBox message={`公开规格加载失败：${publicSpecs.error}`} retry={publicSpecs.reload} />}
      {!data?.length && (
        <Empty
          title="暂无公开模型"
          description="添加一个对外模型 ID，并把它绑定到渠道中的上游模型。"
          action={<Button onClick={() => { setEditing(null); setCreating(true); }}><Plus size={16} />新增公开模型</Button>}
        />
      )}
      {Boolean(data?.length && !visibleRows.length) && (
        <Empty title="未找到匹配的模型" description="可以减少筛选条件，或清除后查看全部。" action={<Button variant="secondary" onClick={() => { setSearch(''); setSelectedTags([]); }}>清除筛选</Button>} />
      )}

      <div className="space-y-2" {...sortable.listProps}>
        {visibleRows.length > 0 && <div className="public-model-list-heading">
          <div className="public-model-identity"><div className="public-model-row-controls"><label className="public-model-select" title="全选 / 取消全选"><input type="checkbox" className="checkbox" aria-label="全选所有模型" checked={allSelected} ref={el => { if (el) el.indeterminate = someSelected; }} onChange={toggleSelectAll} /></label><span aria-hidden="true" /></div><span>公开模型</span></div>
          <span>上游绑定</span><div className="public-model-stats"><span>上下文</span><span>模态</span><span>思考</span></div><span className="text-right">状态与操作</span>
        </div>}
        {visibleRows.map(model => {
          const spec = model.enabled ? specsByID.get(model.id) : undefined;
          const context = model.enabled ? formatContext(spec) : '未公开';
          const expanded = expandedIds.has(model.id);
          const names = [...new Set(model.bindings.map(b => b.channel_name || '渠道 ' + b.channel_id))];
          return <article key={model.id} {...sortable.itemProps(model.id)} className="card overflow-hidden border">
            <div className="public-model-row">
              <div className="public-model-identity">
                <div className="public-model-row-controls"><label className="public-model-select"><input type="checkbox" className="checkbox" aria-label={'选择模型 ' + model.id} checked={selectedIds.has(model.id)} onChange={() => toggleSelect(model.id)} /></label><button type="button" {...sortable.handleProps(model.id)} className="public-model-handle" aria-disabled={!canReorder || !!busyId} aria-label={'拖拽调整模型 ' + model.id + ' 顺序'} title={canReorder ? '拖拽或使用上下方向键调整顺序' : '请切回默认排序以自定义调整顺序'} onKeyDown={e => { if (canReorder && (e.key === 'ArrowUp' || e.key === 'ArrowDown')) { e.preventDefault(); void moveModel(model.id, e.key === 'ArrowUp' ? -1 : 1); } }}><GripVertical size={16} /></button></div>
                <ProviderIcon {...(() => { const channel = model.bindings.map(binding => channels?.find(item => item.id === binding.channel_id)).find(Boolean); if (!channel) return { name: model.display_name || model.id }; const address = (channel.endpoints?.[0]?.base_url?.trim() || channel.base_url?.trim() || '').replace(/\/+$/, ''); return { name: channel.name, baseUrl: address, override: channelDisplay.data?.icons?.[String(channel.id)], fallbackIcon: plugins.data?.items?.find(plugin => plugin.credential_type === channel.credential_type)?.icon }; })()} size={24} />
                <div className="public-model-name">
                  <h2><button type="button" aria-expanded={expanded} onClick={() => toggleExpand(model.id)}>{model.display_name || model.id}</button></h2>
                  {model.display_name && model.display_name !== model.id && <p className="public-model-id">{model.id}</p>}
                  <div className="public-model-tags">
                    {!model.enabled && <Badge tone="neutral">已停用</Badge>}
                    {(model.tags || []).map(id => <TagBadge key={id} tag={tagByID.get(id)} tagId={id} size="sm" />)}
                  </div>
                </div>
              </div>
              <div className="public-model-bindings"><span title={names.join('、')}>{names.join('、') || '暂无绑定'}</span><span className="public-model-note">{model.bindings.filter(b => b.enabled).length}/{model.bindings.length} 个绑定启用{model.fallback ? ' · 允许回退' : ''}</span></div>
              <div className="public-model-stats">
                <div title={'上下文：' + context}><span className="public-model-stat-label">上下文</span>{context}</div>
                <div><span className="public-model-stat-label">模态</span>{renderModalities(spec)}</div>
                <div><span className="public-model-stat-label">思考</span>{renderThinking(spec)}</div>
              </div>
              <div className="public-model-actions">
                <button type="button" role="switch" aria-checked={model.enabled} aria-label={(model.enabled ? '停用 ' : '启用 ') + model.id} disabled={!!busyId || loading || !!error}
                  className={'switch ' + (model.enabled ? 'switch-on ' : '') + 'relative before:absolute before:-inset-y-1.5 before:inset-x-0 cursor-pointer'} onClick={() => void toggle(model)}><span /></button>
                <button type="button" className="btn btn-ghost !min-h-[32px] !h-8 !w-8 !p-0" aria-label={'编辑模型 ' + model.id} disabled={!!busyId || loading || !!error} onClick={() => { setEditing(model); setCreating(true); }}><Pencil size={16} /></button>
                <button type="button" className="btn btn-ghost !min-h-[32px] !h-8 !w-8 !p-0" aria-label={'删除模型 ' + model.id} disabled={!!busyId || loading || !!error} onClick={() => requestDelete([model.id], model.display_name || model.id)}><Trash2 size={16} /></button>
                <button type="button" className="btn btn-ghost !min-h-[32px] !h-8 !w-8 !p-0" aria-label={(expanded ? '折叠模型 ' : '展开模型 ') + model.id + ' 详情'} aria-expanded={expanded} onClick={() => toggleExpand(model.id)}><ChevronRight size={16} className={expanded ? 'rotate-90' : ''} /></button>
              </div>
            </div>
            <details open={expanded} className="border-t border-[var(--color-border)]" onToggle={e => { if (e.currentTarget.open !== expanded) toggleExpand(model.id); }}>
              <summary className="public-model-details-summary">绑定规格与来源</summary>
              <div className="p-4 pt-2"><EffectiveSummary view={model} /></div>
            </details>
          </article>;
        })}
      </div>

      {/* 浮动批量操作条 */}
      {selectedIds.size > 0 && (
        <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-30 flex items-center gap-3 px-4 py-2.5 rounded-xl border border-[var(--color-border)] bg-[var(--color-surface-1)]/95 shadow-2xl backdrop-blur-md">
          <span className="text-xs font-medium text-[var(--color-text-primary)] whitespace-nowrap">
            已选择 <span className="font-mono font-bold text-[var(--brand-blue)]">{selectedIds.size}</span> 项
          </span>
          <div className="h-4 w-px bg-[var(--color-border)]" />
          <Button
            variant="ghost"
            className="!text-xs !py-1 !px-2.5 !h-7"
            onClick={() => setSelectedIds(new Set())}
          >
            取消选择
          </Button>
          <Button
            variant="secondary"
            className="!text-xs !py-1 !px-2.5 !h-7"
            onClick={() => handleBatchStatus(true)}
          >
            批量启用
          </Button>
          <Button
            variant="secondary"
            className="!text-xs !py-1 !px-2.5 !h-7"
            onClick={() => handleBatchStatus(false)}
          >
            批量停用
          </Button>
          <Button
            variant="danger"
            className="!text-xs !py-1 !px-2.5 !h-7"
            onClick={handleBatchDelete}
          >
            批量删除
          </Button>
        </div>
      )}

      {/* 5秒无感撤销 Snackbar */}
      {pendingDeletion && (
        <div className="fixed bottom-6 right-6 z-40 flex items-center gap-3 px-4 py-3 rounded-xl border border-[var(--color-border)] bg-[var(--color-surface-1)] shadow-2xl backdrop-blur-md">
          <div className="flex items-center gap-2 text-xs text-[var(--color-text-primary)]">
            <span className="inline-block w-2 h-2 rounded-full bg-amber-500 animate-ping" />
            <span>已移除「{pendingDeletion.label}」</span>
            <span className="font-mono text-[var(--color-text-muted)]">({pendingDeletion.remaining}s)</span>
          </div>
          <Button
            variant="primary"
            className="!text-xs !py-1 !px-3 !h-7 gap-1"
            onClick={handleUndo}
          >
            <RotateCcw size={13} />
            撤回
          </Button>
          <Button
            variant="ghost"
            className="!text-xs !py-1 !px-2 !h-7 text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
            onClick={handleCommitDeletionNow}
            title="立即永久删除"
          >
            立即生效
          </Button>
        </div>
      )}

      {creating && (
        <ModelEditor
          model={editing}
          publicSpecs={publicSpecs}
          channels={channels || []}
          onClose={() => { setCreating(false); setEditing(null); }}
          onSaved={() => { setCreating(false); setEditing(null); reload(); }}
        />
      )}

      {importOpen && (
        <ImportModal
          onClose={() => setImportOpen(false)}
          onSaved={() => { setImportOpen(false); reload(); }}
        />
      )}

    </div>
  );
}
