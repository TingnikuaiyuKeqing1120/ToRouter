import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { ArrowDown, ArrowDownWideNarrow, ArrowUp, ChevronDown, ChevronRight, CircleHelp, Copy, GripVertical, KeyRound, Layers, MoreHorizontal, Pencil, Plus, Radar, RefreshCw, Search, Settings2, SlidersHorizontal, Tags, Trash2, X } from 'lucide-react';
import { api, channelPayload, discoverFormats, endpointFormats, endpointLabels, errorMessage, oauthRoutes, type Channel, type CredentialItem, type CredentialType, type Endpoint, type EndpointFormat, type DiscoverResult, type ProbeResult, type Settings as SettingsData } from '../api';
import { OAuthAuthorize, OAuthStatusBadge } from '../components/oauth';
import { AddOAuthCredential } from '../components/AddOAuthCredential';
import { OAuthAccounts } from '../components/OAuthAccounts';
import { KeyDiscovery } from '../components/KeyDiscovery';
import { useResource, useResources } from '../hooks';
import { useSortableList } from '../hooks/useSortableList';
import { Badge, Button, Diagnostic, Empty, ErrorBox, Field, Input, Loading, Modal, PageHeader, Select, useToast } from '../components/ui';
import { channelPresets, getPresetById } from '../presets';
import { ProviderIcon } from '../components/ProviderIcons';
import { ChannelIconPicker } from '../components/ChannelIconPicker';
import { useConfirm, useSaveOrDiscard } from '../components/Confirm';
import { NavigationCancelled, useBeforeLeave } from '../components/NavigationGuard';
import { managementLink, managementTarget } from '../services/managementLinks';
import { TagBadge } from '../components/TagBadge';
import { ChannelModelTags } from '../components/ChannelModelTags';
import { CredentialKeyInput } from '../components/CredentialKeyInput';
import { ChannelQuota } from '../components/ChannelQuota';
import { useQuotaSnapshots } from '../hooks/useQuotaSnapshots';
import { usePlugins } from '../hooks/usePlugins';
import type { PluginAccess } from '../types/plugins';
import './Channels.css';
import './ChannelEditor.css';

/** A brand-new channel starts with one endpoint; the owner adds more. */
const emptyChannel: Channel = {
  id: 0,
  name: '',
  api_key: '',
  has_key: false,
  models: [],
  tags: [],
  enabled: true,
  base_url: '',
  endpoints: [{ format: 'openai_chat_completions', base_url: '' }],
  credential_type: 'api_key',
  dispatch_mode: 'failover',
  chain_type: 'unidirectional',
  credentials: [{ id: 'cred_1', label: '主凭据', api_key: '', enabled: true }],
};

/** One card of the OAuth view: a provider's connected accounts, and the
 * credential type the channel list is asked for. Two of the three tracks are
 * not OpenAI-official, and the card says which. */
type OAuthProvider = { type: Exclude<CredentialType, 'api_key'>; title: string; note: string; official: boolean };
const oauthProviders: OAuthProvider[] = [
  { type: 'codex_oauth', title: 'Codex', note: '复用 Codex CLI 的公共客户端身份，请求按原样透传。', official: false },
  { type: 'chatgpt_oauth', title: 'ChatGPT', note: 'OpenAI 官方 Sign in with ChatGPT，仅 Responses 端点。', official: true },
  { type: 'copilot_oauth', title: 'Copilot', note: '设备码授权换取 Copilot 令牌，额度消耗快于官方客户端。', official: true },
  { type: 'claude_oauth', title: 'Claude 订阅', note: '独立授权，使用固定原生请求身份。模型列表为维护目录，实际可用性以账号测试为准。', official: false },
  { type: 'antigravity_oauth', title: 'Antigravity', note: '独立 Google 授权并关联项目，模型权限取决于账号资格。', official: false },
  { type: 'zen_free', title: 'OpenCode Zen Free', note: '公共免费接入。需客户端提供 bash、edit、glob、grep、read 工具；没有适配的额度信息保持未知。', official: false },
  { type: 'devin_oauth', title: 'Devin', note: '独立授权订阅账号。支持文本、函数工具和完整上下文；不支持托管工具或严格结构化输出。', official: false },
  { type: 'plugin:qoder', title: 'Qoder', note: '兼容插件独立授权国际版账号。支持文本、图片和函数工具；不支持严格工具或强制工具选择。', official: false },
  { type: 'plugin:qoder-cn', title: 'Qoder CN', note: '兼容插件独立授权中国版账号，不支持企业 VPC。账号与国际版分开管理。', official: false },
  { type: 'plugin:workbuddy', title: 'WorkBuddy', note: '兼容插件独立授权 WorkBuddy。', official: false },
  { type: 'plugin:workbuddy-ai', title: 'WorkBuddy AI', note: '兼容插件独立授权 WorkBuddy AI 国际版。', official: false },
  { type: 'plugin:minimax-code', title: 'MiniMax Code', note: '兼容插件独立授权 MiniMax Code 中国版。', official: false },
  { type: 'plugin:minimax-code-global', title: 'MiniMax Code Global', note: '兼容插件独立授权 MiniMax Code 国际版。', official: false },
];
const oauthTrackByProvider: Record<string, Exclude<CredentialType, 'api_key'>> = { copilot: 'copilot_oauth', chatgpt: 'chatgpt_oauth', codex: 'codex_oauth', claude: 'claude_oauth', antigravity: 'antigravity_oauth', devin: 'devin_oauth', 'opencode-zen-free': 'zen_free', qoder: 'plugin:qoder', 'qoder-cn': 'plugin:qoder-cn', workbuddy: 'plugin:workbuddy', 'workbuddy-ai': 'plugin:workbuddy-ai', 'minimax-code': 'plugin:minimax-code', 'minimax-code-global': 'plugin:minimax-code-global' };
/** Which track a channel belongs to. The credential type is the handle the
 * console filters on; the credential's own provider is the fallback for a row
 * that predates the field. */
function oauthTrack(channel: Channel): Exclude<CredentialType, 'api_key'> | undefined {
  if (channel.credential_type && channel.credential_type !== 'api_key') return channel.credential_type;
  return channel.oauth?.provider ? oauthTrackByProvider[channel.oauth.provider] : undefined;
}

/** The address an endpoint is reached at: its own, or the channel default when
 * it declares none. The store validates and expands this the same way, so the
 * console's destination comparison and the save agree on what "unchanged"
 * means. */
const endpointUrl = (e: Endpoint, base: string) => (e.base_url.trim() || base.trim()).replace(/\/+$/, '');
const sameDestinations = (formEndpoints: Endpoint[], origEndpoints: Endpoint[], formBase: string, origBase: string) => {
  const normFormBase = formBase.trim().replace(/\/+$/, '');
  const normOrigBase = origBase.trim().replace(/\/+$/, '');
  if (normFormBase !== normOrigBase) return false;
  const knownUrls = new Set([normOrigBase, ...origEndpoints.map(e => endpointUrl(e, normOrigBase))]);
  return formEndpoints.every(e => knownUrls.has(endpointUrl(e, normFormBase)));
};

const formatStandardExamples: Record<EndpointFormat, { standardBase: string; desc: string }> = {
  openai_chat_completions: {
    standardBase: 'https://api.openai.com/v1',
    desc: '生成：/chat/completions · 目录：/models',
  },
  openai_responses: {
    standardBase: 'https://api.openai.com/v1',
    desc: '生成：/responses · 目录：/models',
  },
  anthropic_messages: {
    standardBase: 'https://api.anthropic.com/v1',
    desc: '生成：/messages · 目录：/models',
  },
  gemini_generate_content: {
    standardBase: 'https://generativelanguage.googleapis.com/v1beta',
    desc: '生成：/models/{model}:generateContent · 目录：/models',
  },
};

function previewEndpointUrl(baseUrl: string, format: EndpointFormat, action: 'request' | 'models' = 'request'): string {
  const trimmed = baseUrl.trim().replace(/\/+$/, '');
  if (!trimmed) return '';
  try {
    const url = new URL(trimmed);
    let path = url.pathname.replace(/\/+$/, '');
    const version = format === 'gemini_generate_content' ? '/v1beta' : '/v1';
    if (!path.endsWith(version)) {
      path += version;
    }
    if (action === 'models') {
      path += '/models';
    } else {
      switch (format) {
        case 'openai_chat_completions':
          path += '/chat/completions';
          break;
        case 'openai_responses':
          path += '/responses';
          break;
        case 'anthropic_messages':
          path += '/messages';
          break;
        case 'gemini_generate_content':
          path += '/models/{model}:generateContent';
          break;
      }
    }
    url.pathname = path;
    return url.toString();
  } catch {
    return '';
  }
}

/** The preference a save actually sends: one the channel does not declare — or
 * Responses, which converts to nothing — is sent as unset. The dirty check uses
 * the same rule, so a value the save would drop is never asked about as if it
 * were a change. */
const effectivePreferred = (c: Channel) => c.preferred_endpoint && c.endpoints.some(e => e.format === c.preferred_endpoint) ? c.preferred_endpoint : '';
/** A draft reduced to what the save can carry: every field normalized the way
 * the save normalizes it (trimmed name, an endpoint's effective address with the
 * trailing slash gone, unordered lists as sets, the preference as the value the
 * save sends), so a touch the payload would not carry is not a change. The api
 * key is deliberately not part of this: an empty key field is the normal state,
 * and any text in it is a change on its own. */
function draftFingerprint(c: Channel, preferred: string): string {
  const address = (e: Endpoint) => `${e.format}|${(e.base_url.trim() || (c.base_url || '').trim()).replace(/\/$/, '')}|${e.state || ''}`;
  const set = (list?: string[]) => [...(list || [])].sort().join(',');
  const creds = (c.credentials || []).map(cr => `${cr.id}|${(cr.label || '').trim()}|${cr.enabled !== false}|${(cr.api_key || '').trim()}`).join(';');
  return JSON.stringify([
    c.name.trim(),
    (c.base_url || '').trim(),
    [...c.endpoints].map(address).sort(),
    preferred,
    set(c.models),
    set(c.available_models),
    set(c.manual_models),
    c.enabled, c.tags, c.model_tags,
    c.dispatch_mode || 'failover',
    c.chain_type || 'unidirectional',
    creds,
  ]);
}
/** A channel stored with one shared base carries that base in every endpoint
 * row, because the store expands the default when it saves. The editor reads
 * "this endpoint's address is the channel base" back as "no address of its
 * own": the two describe the same destination, and re-pointing the channel then
 * moves the endpoint with it instead of leaving a stale override behind. */
function inheritDefault(channel: Channel): Channel {
  const base = (channel.base_url || '').trim().replace(/\/$/, '');
  const endpoints = base
    ? channel.endpoints.map(e => e.base_url.trim().replace(/\/$/, '') === base ? { ...e, base_url: '' } : e)
    : channel.endpoints;
  const dispatch_mode = channel.dispatch_mode || 'failover';
  const chain_type = channel.chain_type || 'unidirectional';
  let credentials = channel.credentials ? channel.credentials.map(c => ({ ...c })) : [];
  if (!credentials.length) {
    if ((!channel.credential_type || channel.credential_type === 'api_key')) {
      credentials = [{
        id: 'cred_1',
        label: '默认凭据',
        api_key: '',
        has_key: channel.has_key,
        masked_key: channel.has_key ? '••••已配置' : '',
        enabled: true,
      }];
    }
  } else if (channel.credential_type && channel.credential_type !== 'api_key') {
    credentials = credentials.map((c, i) => ({
      ...c,
      has_oauth: c.has_oauth ?? (i === 0 ? channel.has_oauth : false),
      oauth_info: c.oauth_info || (i === 0 ? channel.oauth : undefined),
    }));
  }
  return {
    ...channel,
    endpoints,
    dispatch_mode,
    chain_type,
    credentials,
  };
}
/** The per-model config and evidence a channel may keep once its catalogue is
 * replaced: a store row may only name models the channel still lists somewhere
 * (catalogue or enabled), and the store prunes the maps when a pull takes a
 * model away. The draft has to drop the same keys or its next save would be
 * refused for referencing a model the pull removed. Known boundary: this mirrors
 * the store's rule rather than asking it — the config of a dropped model is
 * discarded, exactly as the server does, and the editor has no UI to re-enter
 * one. */
function listedOnly(form: Channel, catalogue: string[]): Pick<Channel, 'model_configs' | 'model_evidence' | 'model_catalog_refs'> {
  const listed = new Set([...catalogue, ...form.models]);
  const keep = <T,>(map: Record<string, T> | undefined) => Object.fromEntries(Object.entries(map || {}).filter(([id]) => listed.has(id)));
  return { model_configs: keep(form.model_configs), model_evidence: keep(form.model_evidence), model_catalog_refs: keep(form.model_catalog_refs) };
}

const editorSections = [
  { id: 'quick', label: '快捷配置', Icon: SlidersHorizontal },
  { id: 'models', label: '模型配置', Icon: Layers },
  { id: 'tags', label: '渠道标签', Icon: Tags },
  { id: 'more', label: '更多配置', Icon: Settings2 },
] as const;
type EditorSection = typeof editorSections[number]['id'];

function ChannelEditor({ channel, onClose, onSaved, onChanged, initialReauth = false, initialSection, editorFlow, onEditorFlow, icon, iconBlocked, onIconChange, pluginAccess }: { pluginAccess?: PluginAccess; channel: Channel; allChannels?: Channel[]; icon?: string; iconBlocked?: boolean; onIconChange?: () => void; onClose: () => void; onSaved: () => void; onChanged: () => void; initialReauth?: boolean; initialSection?: 'credentials'; editorFlow?: string; onEditorFlow: (flowID: string | null) => void }) {
  // The shape the editor opened with, kept by reference: a close asks about
  // dropping only what this edit itself changed, measured the way the save
  // would measure it. A model delete is persisted by its own API call and the
  // draft adopts the answer, so the baseline follows it — otherwise closing
  // would ask about a change the server already holds.
  const [section, setSection] = useState<EditorSection>('quick');
  const [prepared, setPrepared] = useState({ models: false, tags: false });
  // Show the selected pane first; prepare expensive contents after a paint.
  // Once prepared, keep it mounted so returning reuses the same local state.
  useEffect(() => {
    if ((section !== 'models' && section !== 'tags') || prepared[section]) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const frame = requestAnimationFrame(() => {
      timer = setTimeout(() => setPrepared(current => ({ ...current, [section]: true })), 0);
    });
    return () => { cancelAnimationFrame(frame); if (timer !== undefined) clearTimeout(timer); };
  }, [section, prepared]);
  const credentialSection = useRef<HTMLElement>(null);
  useEffect(() => {
    if (initialSection !== 'credentials') return;
    const credentialID = new URLSearchParams(window.location.hash.split('?')[1] || '').get('credential_id');
    const exact = credentialID ? Array.from(credentialSection.current?.querySelectorAll<HTMLElement>('[data-credential-id]') || []).find(row => row.dataset.credentialId === credentialID) : undefined;
    const destination = exact || credentialSection.current;
    destination?.scrollIntoView?.({ block: 'start' });
    destination?.focus({ preventScroll: true });
  }, [initialSection]);
  const [initial, setInitial] = useState(() => ({ ...inheritDefault(channel), api_key: '' }));
  const [form, setForm] = useState(initial);
  const [modelSearch, setModelSearch] = useState('');
  const [manualModel, setManualModel] = useState('');
  const [selectedPresetId, setSelectedPresetId] = useState('');
  const selectedPreset = useMemo(() => getPresetById(selectedPresetId), [selectedPresetId]);

  function handlePresetSelect(presetId: string) {
    setSelectedPresetId(presetId);
    if (!presetId) {
      setForm(current => ({
        ...current,
        name: '',
        base_url: '',
        endpoints: [{ format: 'openai_chat_completions', base_url: '' }],
        preferred_endpoint: undefined,
      }));
      return;
    }
    const preset = getPresetById(presetId);
    if (!preset) return;
    setForm(current => ({
      ...current,
      name: preset.name,
      base_url: preset.base_url,
      endpoints: preset.endpoints.map(e => ({ ...e })),
      preferred_endpoint: preset.preferred_endpoint || undefined,
    }));
  }
  const [fetching, setFetching] = useState(false);
  const [discovery, setDiscovery] = useState<DiscoverResult | null>(null);
  const [discoverError, setDiscoverError] = useState('');
  const [probe, setProbe] = useState<{ channel: Channel; model: string } | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState('');
  const editing = !!channel.id;
  const credentialType: CredentialType = form.credential_type || 'api_key';
  const publicAccess = credentialType === 'zen_free';
  const compatibleOAuth = credentialType.startsWith('plugin:') || credentialType.startsWith('market:');
  const oauth = credentialType !== 'api_key' && !publicAccess;
  const networkBlocked = credentialType !== 'api_key' && !pluginAccess?.allowed;
  async function cancelBlockedAuthorization() {
    setReauthorizing(false); setReauthorizingCredIndex(null);
    onEditorFlow(null);
    if (!editorFlow) return;
    const route = credentialType === 'api_key' ? '' : oauthRoutes[credentialType];
    try { await api(credentialType.startsWith('market:') ? `/plugins/market/auth/${encodeURIComponent(editorFlow)}` : `${route}/cancel?flow=${encodeURIComponent(editorFlow)}`, { method: credentialType.startsWith('market:') ? 'DELETE' : 'POST' }); }
    catch { /* Cancellation always clears the local pending flow. */ }
  }
  // Opening the editor on an existing credential with the reauthorize entry
  // already open is how the OAuth view's account cards start a re-flow.
  const canReauthorize = !publicAccess;
  const [reauthorizing, setReauthorizing] = useState(canReauthorize && (initialReauth || !!editorFlow) && channel.has_oauth);
  const [reauthorizingCredIndex, setReauthorizingCredIndex] = useState<number | null>(null);
  const [showAddOAuth, setShowAddOAuth] = useState(false);
  const toast = useToast();
  const confirmAction = useConfirm();
  const saveOrDiscard = useSaveOrDiscard();
  const saving = useRef(false);
  async function copyChannelID() {
    try { await navigator.clipboard.writeText(String(channel.id)); toast('渠道 ID 已复制'); }
    catch { toast('复制失败，请手动选择渠道 ID 复制。'); }
  }

  async function syncNativePool(resultChannelID?: number, loaded?: Channel[]) {
    const list = loaded || await api<Channel[]>('/channels');
    const live = list.find(c => c.id === form.id);
    if (!live) throw new Error('授权已完成，但目标渠道已变化，请重新读取。');
    setForm(current => ({ ...current, has_oauth: live.has_oauth, oauth: live.oauth, generation: live.generation, credentials: [
      ...(current.credentials || []).filter(cred => live.credentials?.some(saved => saved.id === cred.id)).map(cred => ({
        ...live.credentials!.find(saved => saved.id === cred.id)!, label: cred.label, enabled: cred.enabled,
      })),
      ...(live.credentials || []).filter(saved => !current.credentials?.some(cred => cred.id === saved.id)),
    ] }));
    setInitial(current => ({ ...current, has_oauth: live.has_oauth, oauth: live.oauth, generation: live.generation, credentials: live.credentials }));
    if (resultChannelID && resultChannelID !== form.id) throw new Error('授权目标不一致，请重新读取。');
    onChanged();
  }

  function update<K extends keyof Channel>(key: K, value: Channel[K]) { setForm(current => ({ ...current, [key]: value })); }
  const locked = busy || fetching || !!probe || !!removing;
  // The draft catalogue is what the channel lists: the stored catalogue (the
  // fetched entries plus the owner's manual ones) and the enabled models, which
  // a catalogue replacement can take off the shelf without disabling them.
  const inventory = useMemo(() => [...new Set([...(form.available_models || []), ...form.models])], [form.available_models, form.models]);
  const enabledModels = useMemo(() => new Set(form.models), [form.models]);
  const pricedModels = useMemo(() => new Set((channel.associated_prices || []).map(price => price.model)), [channel.associated_prices]);
  const hasFreshKey = (form.credentials || []).some(cr => !!cr.api_key?.trim());
  const isMultiCred = (form.credentials || []).length > 1;
  const needsKey = channel.has_key && !hasFreshKey && !sameDestinations(form.endpoints, channel.endpoints, form.base_url || '', channel.base_url || '');
  const preferredOptions = form.endpoints.map(e => e.format);
  const preferred = effectivePreferred(form);
  const keyNotice = '端点地址已改变；获取模型或测试前请重新输入 API 密钥，不能复用原渠道密钥。';
  function addCredential() {
    const id = `cred_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 6)}`;
    const label = `备用凭据 ${(form.credentials?.length || 0) + 1}`;
    setForm(curr => ({
      ...curr,
      credentials: [...(curr.credentials || []), { id, label, api_key: '', enabled: true }],
    }));
  }
  function updateCredential(index: number, patch: Partial<CredentialItem>) {
    setForm(curr => {
      const list = [...(curr.credentials || [])];
      if (!list[index]) return curr;
      list[index] = { ...list[index], ...patch };
      return { ...curr, credentials: list };
    });
  }
  function removeCredential(index: number) {
    setForm(curr => {
      const list = (curr.credentials || []).filter((_, i) => i !== index);
      return { ...curr, credentials: list };
    });
  }
  function moveCredential(index: number, direction: 'up' | 'down') {
    setForm(curr => {
      const list = [...(curr.credentials || [])];
      const target = direction === 'up' ? index - 1 : index + 1;
      if (target < 0 || target >= list.length) return curr;
      const tmp = list[index];
      list[index] = list[target];
      list[target] = tmp;
      return { ...curr, credentials: list };
    });
  }
  function setEndpoint(format: EndpointFormat, patch: Partial<Endpoint>) {
    setForm(current => ({ ...current, endpoints: current.endpoints.map(e => e.format === format ? { ...e, ...patch } : e) }));
  }
  function toggleFormat(format: EndpointFormat, checked: boolean) {
    setForm(current => {
      const rest = current.endpoints.filter(e => e.format !== format);
      return { ...current, endpoints: checked ? [...rest, { format, base_url: '', state: '' as Endpoint['state'] }] : rest };
    });
  }
  async function discover() {
    if (locked) return;
    setDiscoverError(''); setDiscovery(null);
    if (needsKey) { setDiscoverError(keyNotice); return; }
    setFetching(true);
    try {
      // The pull is applied: the store adopts the fetched models as the auto
      // part of the catalogue (ReplaceCatalogue) and answers with what the
      // channel now lists, so the draft replaces its catalogue with that
      // instead of unioning the fetched ids into whatever it happened to hold.
      // A manual entry survives on both sides — the store keeps it, and so does
      // the draft, including one that is not saved yet.
      const result = await api<DiscoverResult>('/channels/discover', { method: 'POST', body: JSON.stringify({ channel: channelPayload(form), apply: true }) });
      setDiscovery(result);
      const listed = discoverFormats(result).filter(entry => entry.ok).flatMap(entry => (entry.models || []).map(model => model.id));
      // The catalogue that replaces the draft's: the answer's own, or the models
      // this read-only look listed when there is nothing written back yet.
      const catalogue = result.catalogue || (result.ok && listed.length ? listed : null);
      if (catalogue) setForm(current => ({ ...current, available_models: [...new Set([...catalogue, ...(current.manual_models || [])])], ...listedOnly(current, catalogue) }));
      if (editing && result.catalogue) onChanged();
      // A partial answer writes nothing back and changes no inventory: the
      // formats that answered are reported, the draft keeps what it had.
    } catch (e) { setDiscoverError(errorMessage(e)); } finally { setFetching(false); }
  }
  async function discoverOAuth() {
    if (locked || networkBlocked || !form.id) return;
    setFetching(true); setDiscoverError('');
    try {
      const result = await api<{ ok: boolean; detail?: string; catalogue?: string[]; model_formats?: Channel['model_formats'] }>(`/channels/${form.id}/oauth/models`, { method: 'POST', body: '{}' });
      if (!result.ok) throw new Error(result.detail || '获取模型失败');
      const catalogue = result.catalogue || [];
      setForm(current => ({ ...current, model_formats: result.model_formats, available_models: [...new Set([...catalogue, ...(current.manual_models || [])])], ...listedOnly(current, catalogue) }));
      setInitial(current => ({ ...current, model_formats: result.model_formats, available_models: catalogue }));
      onChanged();
      toast('模型目录已更新，新模型默认停用');
    } catch (e) { setDiscoverError(errorMessage(e)); } finally { setFetching(false); }
  }
  function addModel() {
    const model = manualModel.trim();
    if (!model) return;
    // A hand-typed model is marked manual so a later pull keeps it, and joins
    // the catalogue so it can be enabled like any fetched entry.
    setForm(current => ({ ...current, manual_models: [...new Set([...(current.manual_models || []), model])], available_models: [...new Set([...(current.available_models || []), model])] }));
    setManualModel('');
  }
  function toggleModel(model: string) {
    setForm(current => ({
      ...current,
      // The draft catalogue is the union of the two lists, so disabling a model
      // the catalogue never listed keeps it listed as disabled instead of
      // dropping it out of the editor altogether.
      available_models: [...new Set([...(current.available_models || []), ...current.models])],
      models: current.models.includes(model) ? current.models.filter(id => id !== model) : [...current.models, model],
    }));
  }
  async function deleteModel(model: string) {
    if (!form.id) return;
    if (!await confirmAction({ title: `删除渠道模型「${model}」？`, description: '目录条目、启用状态和该模型的单独配置都会移除；已绑定该模型的公开模型将在下次保存时报错。', confirmLabel: '删除渠道模型' })) return;
    setRemoving(model); setError('');
    try {
      // The answer is the channel as it stands after the delete, so the draft
      // adopts its model lists instead of guessing what the store kept.
      const updated = await api<Channel>(`/channels/${encodeURIComponent(form.id)}/models?model=${encodeURIComponent(model)}`, { method: 'DELETE' });
      setForm(current => ({ ...current, models: updated.models || [], available_models: updated.available_models || [], manual_models: updated.manual_models || [], model_configs: updated.model_configs, model_evidence: updated.model_evidence }));
      // The delete is already persisted: the baseline follows the adopted
      // answer so a close afterwards only asks about genuinely unsaved edits.
      setInitial(current => ({ ...current, models: updated.models || [], available_models: updated.available_models || [], manual_models: updated.manual_models || [] }));
      onChanged();
      toast(`已删除模型 ${model}`);
    } catch (e) { setError(errorMessage(e)); } finally { setRemoving(''); }
  }
  function testModel(model: string) {
    setError('');
    if (needsKey) { setError(keyNotice); return; }
    setProbe({ channel: form, model });
  }
  async function saveDraft(): Promise<boolean> {
    if (locked || saving.current) {
      if (fetching) setError('正在获取模型，请完成后再保存。');
      return false;
    }
    setError('');
    // Each endpoint's effective address is its own, or the channel default —
    // exactly what the store validates and the gateway will call.
    const base = (form.base_url || '').trim();
    const addresses = [
      ...(base ? [{ raw: base, section: 'quick' as const, selector: '[aria-label="渠道 Base URL"]' }] : []),
      ...form.endpoints.map(e => ({ raw: e.base_url.trim() || base, section: e.base_url.trim() ? 'more' as const : 'quick' as const, selector: e.base_url.trim() ? `[aria-label="${e.format} 自定义地址"]` : '[aria-label="渠道 Base URL"]' })),
    ];
    for (const address of addresses) {
      try {
        const url = new URL(address.raw);
        if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new Error('invalid URL');
      } catch {
        setSection(address.section);
        setError('请为渠道 Base URL 或该端点填写有效的 HTTP(S) 地址，不要包含账号、密钥、查询参数或片段。');
        requestAnimationFrame(() => {
          const input = document.querySelector<HTMLInputElement>(address.selector);
          const details = input?.closest('details');
          if (details) details.open = true;
          input?.focus();
        });
        return false;
      }
    }
    if (!form.name.trim()) { setError('请输入渠道名称。'); document.querySelector<HTMLInputElement>('.channel-editor-name')?.focus(); return false; }
    if (!form.endpoints.length) { setSection('quick'); setError('请至少选择一种上游协议。'); return false; }
    saving.current = true; setBusy(true);
    try {
      const credsToSave = (form.credentials || []).map(cr => ({
        id: cr.id,
        label: (cr.label || '').trim(),
        api_key: (cr.api_key || '').trim(),
        enabled: cr.enabled !== false,
        source_channel_id: cr.source_channel_id,
      }));
      const answer = await api<Channel & { notice?: string }>(editing ? `/channels/${encodeURIComponent(channel.id)}` : '/channels', {
        method: editing ? 'PUT' : 'POST',
        // 保存不反并：available_models 用草稿目录原样下发，不把当前启用列表并回目录。
        // 启用但不在目录里的模型（上次拉取留下的）保持“启用而未列入目录”，而不是每次
        // 保存都被写回目录；目录 ∪ 启用只用于界面显示（inventory）。
        body: JSON.stringify({
          ...channelPayload(form),
          api_key: '',
          name: form.name.trim(),
          base_url: (form.base_url || '').trim(),
          endpoints: form.endpoints.map(e => ({ ...e, base_url: e.base_url.trim().replace(/\/$/, '') })),
          models: form.models,
          available_models: form.available_models || [],
          preferred_endpoint: preferred,
          dispatch_mode: form.dispatch_mode || 'failover',
          chain_type: form.chain_type || 'unidirectional',
          credentials: credsToSave,
        }),
      });
      // A save can answer with a notice — a re-enabled model the catalogue no
      // longer lists. It rides in the same answer, so it is shown rather than
      // swallowed: the upstream may have taken that model offline.
      const done = editing ? '渠道已保存' : '渠道已创建';
      toast(answer.notice ? `${done}。${answer.notice}` : done);
      return true;
    } catch (e) { setError(errorMessage(e)); return false; } finally { saving.current = false; setBusy(false); }
  }
  async function save(event: FormEvent) {
    event.preventDefault();
    if (await saveDraft()) onSaved();
  }
  /** Whether this edit changed anything a save would carry, against the
   * snapshot the editor opened with. */
  function isDirty() {
    const credsNewKey = (form.credentials || []).some(cr => !!cr.api_key?.trim());
    const credsLengthChanged = (form.credentials || []).length !== (initial.credentials || []).length;
    const credsOrderChanged = (form.credentials || []).map(c => c.id).join(',') !== (initial.credentials || []).map(c => c.id).join(',');
    const credsPropsChanged = (form.credentials || []).some((c, i) => {
      const init = initial.credentials?.[i];
      return !init || c.label !== init.label || c.enabled !== init.enabled;
    });
    return (
      draftFingerprint(form, preferred) !== draftFingerprint(initial, effectivePreferred(initial)) ||
      credsNewKey ||
      credsLengthChanged ||
      credsOrderChanged ||
      credsPropsChanged
    );
  }
  const dirty = useMemo(isDirty, [form, initial, preferred]);
  async function requestClose() {
    if (busy || removing || saving.current) return;
    if (!dirty) { onClose(); return; }
    const decision = await saveOrDiscard();
    if (decision === 'confirm') onClose();
    else if (decision === 'save' && await saveDraft()) onSaved();
  }
  useBeforeLeave(async () => {
    if (busy || removing || saving.current) throw new Error('渠道正在保存，请稍候。');
    if (!dirty) return;
    const decision = await saveOrDiscard();
    if (decision === 'cancel') throw new NavigationCancelled();
    if (decision === 'save') {
      if (!await saveDraft()) throw new NavigationCancelled();
      onSaved();
    }
  });
  const modelList = useMemo(() => {
    if (!prepared.models) return <p role="status" className="channel-editor-note">正在准备模型列表…</p>;
    return (
          <div className="channel-editor-model-list">
            {[true, false].map(enabled => <section key={String(enabled)} aria-label={enabled ? '已启用模型' : '未启用模型'}><h3 className="bg-[var(--color-surface-2)] px-3 py-2 text-xs font-semibold text-[var(--color-text-primary)]">{enabled ? '已启用' : '未启用'} · {inventory.filter(model => enabledModels.has(model) === enabled).length}</h3>{inventory.filter(model => enabledModels.has(model) === enabled && model.toLowerCase().includes(modelSearch.toLowerCase())).map(model => {
              const hasModelConfig = !!form.model_configs?.[model] && Object.keys(form.model_configs[model]).length > 0;
              return <div key={model} className="channel-editor-model-row">
                <label className="flex min-w-0 flex-1 items-center gap-3 text-xs"><input className="checkbox shrink-0" type="checkbox" aria-label={`启用模型 ${model}`} checked={enabledModels.has(model)} onChange={() => toggleModel(model)} /><span className="break-all font-mono">{model}</span></label>
                <Badge tone={enabledModels.has(model) ? 'teal' : 'neutral'}>{enabledModels.has(model) ? '已启用' : '已停用'}</Badge>
                {hasModelConfig && <Badge tone="cyan">已配规格</Badge>}
                <Button type="button" variant="ghost" aria-label={`模型规格 ${model}`} disabled={!editing || dirty} title={!editing || dirty ? '请先保存渠道更改' : '管理此模型规格'} onClick={() => { window.location.hash = managementLink('model-specs', form.id, model); }}>规格 ↗</Button>
                <Badge>{pricedModels.has(model) ? '已配价格' : '价格未配置'}</Badge>
                <Button type="button" variant="ghost" aria-label={`模型价格 ${model}`} disabled={!editing || dirty} title={!editing || dirty ? '请先保存渠道更改' : '管理此模型价格'} onClick={() => { window.location.hash = managementLink('prices', form.id, model); }}>价格 ↗</Button>
                <Button type="button" variant="ghost" aria-label={`测试模型 ${model}`} onClick={() => testModel(model)}>测试</Button>
                {/* The delete goes to the stored channel, so a draft that has nothing stored yet offers none. */}
                {editing && <Button type="button" variant="ghost" aria-label={`删除模型 ${model}`} disabled={removing === model} onClick={() => deleteModel(model)}><Trash2 size={15} /></Button>}
              </div>;
            })}</section>)}
            {!inventory.length && <p className="p-4 text-xs text-[var(--color-text-muted)]">暂无模型，请获取或手动添加。</p>}
            {!!inventory.length && !inventory.some(model => model.toLowerCase().includes(modelSearch.toLowerCase())) && <p className="p-4 text-xs text-[var(--color-text-muted)]">没有匹配的模型。</p>}
          </div>
    );
  }, [prepared.models, inventory, enabledModels, modelSearch, form, channel, dirty, removing, onChanged, confirmAction, toast, needsKey, pricedModels]);
  return <><Modal title={editing ? '编辑渠道' : '添加渠道'} onClose={requestClose} busy={busy || !!removing} className="channel-editor" initialFocus=".channel-editor-name">
    <form onSubmit={save} noValidate className="channel-editor-form">
      <fieldset disabled={locked} className="channel-editor-fields">
        <header className="channel-editor-identity">
          <div className="channel-editor-name-group">
            {editing ? <button type="button" className="channel-editor-icon" aria-label="更换渠道图标" title="更换图标（单独保存）" disabled={!onIconChange || iconBlocked} onClick={onIconChange}><ProviderIcon name={form.name} baseUrl={form.base_url} override={icon} fallbackIcon={pluginAccess?.icon} size={28} /><Pencil size={12} /></button> : <span className="channel-editor-icon"><ProviderIcon name={form.name} baseUrl={form.base_url} fallbackIcon={pluginAccess?.icon} size={28} /></span>}
            <Field label="渠道名称"><Input className="channel-editor-name" required maxLength={100} placeholder="例如：我的 OpenAI" value={form.name} onChange={e => update('name', e.target.value)} /></Field>
          </div>
          <div className="channel-editor-meta">
            {editing && <div className="channel-editor-id"><span>渠道 ID <code>{channel.id}</code></span><Button variant="ghost" aria-label="复制渠道 ID" title="复制渠道 ID" onClick={() => void copyChannelID()}><Copy size={15} /></Button></div>}
            <label className="channel-editor-enabled"><input type="checkbox" className="checkbox" checked={form.enabled} onChange={e => update('enabled', e.target.checked)} />启用此渠道</label>
          </div>
        </header>
        <div className="channel-editor-workspace">
          <nav className="channel-editor-nav" aria-label="渠道配置">
            {editorSections.map(({ id, label, Icon }) => <button key={id} type="button" aria-current={section === id ? 'page' : undefined} aria-controls={`channel-editor-${id}`} onClick={() => setSection(id)}><Icon size={17} /><span>{label}</span></button>)}
          </nav>
          <div className="channel-editor-pages">
            <section id="channel-editor-quick" className="channel-editor-page" hidden={section !== 'quick'} aria-label="快捷配置">
              {!editing && !oauth && <details className="channel-editor-disclosure channel-editor-presets" open><summary>使用渠道预设 <span>{selectedPreset?.name || '自定义'}</span><ChevronDown size={16} /></summary><div>
                <Button variant="secondary" aria-pressed={!selectedPresetId} onClick={() => handlePresetSelect('')}>自定义（空白）</Button>
                {(['cn', 'intl'] as const).map(category => <div key={category}><h4>{category === 'cn' ? '国内厂商' : '国际与海外厂商'}</h4><div className="channel-editor-preset-list">{channelPresets.filter(p => p.category === category).map(preset => <Button key={preset.id} variant="secondary" aria-pressed={selectedPresetId === preset.id} onClick={() => handlePresetSelect(preset.id)}><ProviderIcon name={preset.name} baseUrl={preset.base_url} size={17} />{preset.name}</Button>)}</div></div>)}
                {selectedPreset?.hint && <p className="channel-editor-note">{selectedPreset.hint}</p>}
              </div></details>}
              <Field label="渠道 Base URL" hint={compatibleOAuth ? '由兼容插件维护，与授权地区一致。' : oauth ? '授权时已设置。各协议默认使用此地址，自定义地址仍受平台限制。' : '各协议默认使用此地址，版本与请求路径会自动补齐。'}><Input aria-label="渠道 Base URL" type="url" placeholder="https://api.example.com" readOnly={compatibleOAuth} value={compatibleOAuth ? form.endpoints[0]?.base_url || '' : form.base_url || ''} onChange={e => update('base_url', e.target.value)} /></Field>
              <section ref={credentialSection} tabIndex={-1} className="channel-editor-credentials" aria-label="凭据池与调度策略">
                <div className="channel-editor-section-heading"><h3>凭证池 <span className="channel-editor-count">{form.credentials?.length || 0} 个凭证</span></h3>{publicAccess ? null : !oauth ? <Button variant="secondary" onClick={addCredential}><Plus size={15} />添加凭证</Button> : <Button variant="secondary" aria-label="添加授权凭证" onClick={() => setShowAddOAuth(v => !v)}><Plus size={15} />添加授权凭证</Button>}</div>
                <div className="channel-editor-credential-list">
                  {(form.credentials || []).map((cred, idx) => {
                    const credOAuthInfo = cred.oauth_info || (idx === 0 ? form.oauth : undefined);
                    const hasOAuthCred = cred.has_oauth ?? (idx === 0 ? form.has_oauth : false);
                    return <div key={cred.id || idx} data-credential-id={cred.id} tabIndex={-1} className={`channel-editor-credential${oauth ? ' is-oauth' : ''}`}>
                      <span className="channel-editor-credential-order" title="调用顺序">{idx + 1}</span>
                      <Input aria-label={`凭据 ${idx + 1} 备注`} placeholder="凭证名称" value={cred.label || ''} onChange={e => updateCredential(idx, { label: e.target.value })} />
                      {publicAccess ? <Badge tone="blue">公共访问 · public</Badge> : !oauth ? <CredentialKeyInput channelID={form.id} credential={cred} index={idx} onChange={value => updateCredential(idx, { api_key: value })} /> : <div className="channel-editor-oauth-state">{credOAuthInfo ? <OAuthStatusBadge oauth={{ status: credOAuthInfo.status, status_reason: credOAuthInfo.status_reason }} /> : <Badge tone={hasOAuthCred ? 'neutral' : 'amber'}>{hasOAuthCred ? '已授权' : '待授权'}</Badge>}{canReauthorize && <Button variant="ghost" disabled={networkBlocked} onClick={() => { if (networkBlocked) return; setReauthorizingCredIndex(idx); setReauthorizing(true); }}>{hasOAuthCred ? '重新授权此账号' : '立即授权此账号'}</Button>}</div>}
                      <div className="channel-editor-credential-actions"><label><input type="checkbox" className="checkbox" aria-label={`启用凭据 ${idx + 1}`} checked={cred.enabled !== false} onChange={e => updateCredential(idx, { enabled: e.target.checked })} />启用</label>{(isMultiCred || oauth) && <><Button variant="ghost" disabled={idx === 0} aria-label={`上移凭据 ${idx + 1}`} title="上移" onClick={() => moveCredential(idx, 'up')}><ArrowUp size={16} /></Button><Button variant="ghost" disabled={idx === (form.credentials?.length || 0) - 1} aria-label={`下移凭据 ${idx + 1}`} title="下移" onClick={() => moveCredential(idx, 'down')}><ArrowDown size={16} /></Button><Button variant="ghost" disabled={!oauth && (form.credentials?.length || 0) <= 1} aria-label={`删除凭据 ${idx + 1}`} title="删除此凭据" onClick={() => removeCredential(idx)}><Trash2 size={16} /></Button></>}</div>
                    </div>;
                  })}
                </div>
                {needsKey && !oauth && <p className="channel-editor-note">{keyNotice}</p>}
                {!isMultiCred && !publicAccess && <p className="channel-editor-note">添加新的凭证后，可启用故障转移或轮询，并调整调用顺序。</p>}
                {(isMultiCred || oauth) && <details key={isMultiCred ? 'multiple' : 'single'} className="channel-editor-disclosure" open={isMultiCred}><summary>调用策略 <span>{form.dispatch_mode === 'round_robin' ? '按请求轮询' : '故障转移'} · {form.chain_type === 'cyclic' ? '循环链' : '单向链'}</span><ChevronDown size={16} /></summary><div className="channel-editor-strategy">
                  <div><span className="field-label">调度方式</span><div role="group" aria-label="调度方式" className="segmented"><button type="button" aria-pressed={form.dispatch_mode !== 'round_robin'} className={form.dispatch_mode !== 'round_robin' ? 'active' : ''} onClick={() => update('dispatch_mode', 'failover')}>故障转移</button><button type="button" aria-pressed={form.dispatch_mode === 'round_robin'} className={form.dispatch_mode === 'round_robin' ? 'active' : ''} onClick={() => update('dispatch_mode', 'round_robin')}>按请求轮询</button></div><p className="channel-editor-note">{form.dispatch_mode === 'round_robin' ? '每次请求依次使用下一个可用凭证。' : '调用失败且允许重试时，按顺序尝试备用凭证。'}</p></div>
                  <div><span className="field-label">切换顺序</span><div role="group" aria-label="切换顺序" className="segmented"><button type="button" aria-pressed={form.chain_type !== 'cyclic'} className={form.chain_type !== 'cyclic' ? 'active' : ''} onClick={() => update('chain_type', 'unidirectional')}>单向链</button><button type="button" aria-pressed={form.chain_type === 'cyclic'} className={form.chain_type === 'cyclic' ? 'active' : ''} onClick={() => update('chain_type', 'cyclic')}>循环链</button></div><p className="channel-editor-note">{form.chain_type === 'cyclic' ? '沿顺序循环，恢复后不自动回到首位。' : '按列表次序优先，首选恢复后回到首选。'}</p></div>
                </div></details>}
                {oauth && <p className="channel-editor-note">建议将同规格、同权限的账号放在同一渠道。</p>}{networkBlocked && <p className="channel-editor-plugin-note" role="status">{pluginAccess?.message || '接入插件状态未就绪，暂不可执行联网操作。'} {pluginAccess?.pluginID && <a href={`#/plugins?category=connect&plugin=${encodeURIComponent(pluginAccess.pluginID)}`}>前往插件库</a>}{(editorFlow || reauthorizing) && <Button variant="ghost" onClick={() => void cancelBlockedAuthorization()}>取消授权</Button>}</p>}
                          {oauth && canReauthorize && !networkBlocked && reauthorizing && (form.credentials || []).length > 0 && (
            <div className="space-y-3 pt-1 border-t border-[var(--color-border)]/60">
              <div className="flex items-center justify-between text-xs font-semibold text-[var(--color-text-primary)]">
                <span>
                  {reauthorizingCredIndex !== null && form.credentials?.[reauthorizingCredIndex]
                    ? `正在为凭据 #${reauthorizingCredIndex + 1}「${form.credentials[reauthorizingCredIndex].label || '未命名'}」重新授权`
                    : form.has_oauth
                      ? '重新授权本渠道'
                      : '完成首次授权'}
                </span>
                {form.has_oauth && (
                  <Button
                    type="button"
                    variant="ghost"
                    className="!h-6 !text-xs !p-1"
                    onClick={() => {
                      setReauthorizing(false);
                      setReauthorizingCredIndex(null);
                    }}
                  >
                    取消
                  </Button>
                )}
              </div>
              <OAuthAuthorize
                type={credentialType}
                authMethods={pluginAccess?.authMethods}
                operation="reauth"
                credentialID={form.credentials?.[reauthorizingCredIndex ?? 0]?.id}
                channelID={form.id}
                resumeFlow={editorFlow}
                onStarted={onEditorFlow}
                onResumeGone={() => {
                  onEditorFlow(null);
                  toast('上次未完成的授权已失效（服务可能重启过），请重新开始。');
                }}
                onDone={async result => {
                  setReauthorizing(false);
                  setReauthorizingCredIndex(null);
                  onEditorFlow(null);
                  try { await syncNativePool(result?.status === 'complete' ? result.channel_id : undefined); }
                  catch (cause) { setError(errorMessage(cause)); onChanged(); }
                }}
              />
              <p className="text-xs text-[var(--color-text-secondary)]">重新授权保留本渠道的名称、模型配置和下游绑定。</p>
            </div>
          )}
              </section>
              <section className="channel-editor-protocols" aria-label="协议选择">
                <div className="channel-editor-section-heading"><h3>协议</h3><Button variant="ghost" onClick={() => setSection('more')}>独立配置 <ChevronRight size={15} /></Button></div>
                <p className="channel-editor-note">{compatibleOAuth ? '由插件声明支持的协议，网关按转换策略对接客户端。' : '勾选上游支持的协议，默认共用渠道 Base URL。'}</p>
                <div className="channel-editor-protocol-list">{(compatibleOAuth ? form.endpoints.map(endpoint => endpoint.format) : endpointFormats).map(format => <label key={format}><input className="checkbox" type="checkbox" disabled={compatibleOAuth} aria-label={`启用格式 ${format}`} checked={form.endpoints.some(e => e.format === format)} onChange={e => toggleFormat(format, e.target.checked)} /><span>{endpointLabels[format]}</span>{form.endpoints.find(e => e.format === format)?.base_url && <span className="channel-editor-count">{compatibleOAuth ? '插件维护' : '独立地址'}</span>}</label>)}</div>
                <Field label="偏好协议"><Select aria-label="偏好协议" value={preferred} onChange={e => update('preferred_endpoint', (e.target.value || undefined) as Channel['preferred_endpoint'])}><option value="">按默认顺序</option>{preferredOptions.map(f => <option key={f} value={f}>{endpointLabels[f]}</option>)}</Select></Field>
                <details className="channel-editor-help"><summary><CircleHelp size={15} />协议如何选择</summary><p>优先使用与客户端相同的协议。没有相同协议时，先尝试偏好协议，再按 Chat Completions → Messages → Gemini → Responses 的顺序转换。跨协议支持文本和常规函数工具；特殊内容可能需要原生协议。</p></details>
              </section>
            </section>
            <section id="channel-editor-models" className="channel-editor-page" hidden={section !== 'models'} aria-label="模型配置">        <section className="channel-editor-models" aria-label="模型列表">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h3>模型列表 <span className="channel-editor-count">{form.models.length} 个启用 / {inventory.length} 个模型</span></h3>
            <Button type="button" variant="secondary" disabled={fetching || networkBlocked} onClick={oauth || publicAccess ? discoverOAuth : discover}>{fetching ? '正在获取…' : '从上游获取模型'}</Button>
          </div>
          {oauth && <p className="text-xs leading-5 text-[var(--color-text-secondary)]">在这里或插件渠道的菜单中获取模型目录，再选择启用的模型；测试可能消耗额度，结果不计入业务统计。</p>}
          <p className="text-xs text-[var(--color-text-muted)]">从上游获取仅查询模型目录。规格与价格在各自页面管理；有未保存的连接更改时，请先保存再前往。</p>
          {discoverError && <ErrorBox message={discoverError} />}
          {discovery && (discovery.formats
            // Several formats were asked: each one answered for itself, because
            // the facades of one channel can disagree.
            ? <div className="space-y-2">{discovery.formats.map(entry => <div key={entry.format}>
              <p role="status" className={`text-xs ${entry.ok ? 'text-teal-700 dark:text-teal-400' : 'text-red-700 dark:text-red-400'}`}>{endpointLabels[entry.format]}：{entry.ok ? `可用（${entry.duration_ms} ms）` : '不可用'}{entry.ok && entry.models ? `，${entry.models.length} 个模型` : ''}</p>
              {entry.diagnostic && <Diagnostic diagnostic={entry.diagnostic} />}
            </div>)}</div>
            : discovery.ok
              ? <p role="status" className="text-xs text-teal-700 dark:text-teal-400">已获取 {(discovery.models || []).length} 个模型（{discovery.duration_ms} ms），已保留原有选择；新模型默认停用。</p>
              : <Diagnostic diagnostic={discovery.diagnostic} />)}
          {(discoverError || (discovery && !discovery.ok)) && <p className="text-xs text-[var(--color-text-muted)]">无法获取目录时，仍可手动添加模型 ID 并测试。</p>}

          <div className="channel-editor-model-search">
            <div className="flex-1">
              <Input aria-label="搜索模型" placeholder="搜索模型…" value={modelSearch} onChange={e => setModelSearch(e.target.value)} />
            </div>
            {form.model_formats == null && <div className="flex gap-1.5 shrink-0">
              <Input aria-label="手动模型 ID" placeholder="输入模型 ID" className="channel-editor-manual-model" value={manualModel} onChange={e => setManualModel(e.target.value)} onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addModel(); } }} />
              <Button type="button" variant="secondary" disabled={!manualModel.trim()} onClick={addModel}>手动添加</Button>
            </div>}
          </div>

          {modelList}
        </section></section>
            <section id="channel-editor-tags" className="channel-editor-page" hidden={section !== 'tags'} aria-label="渠道标签">
              {prepared.tags ? <ChannelModelTags channel={form} onChange={setForm} /> : <p role="status" className="channel-editor-note">正在准备标签…</p>}
            </section>
            <section id="channel-editor-more" className="channel-editor-page" hidden={section !== 'more'} aria-label="更多配置">
              <section className="channel-editor-endpoints"><h3>{compatibleOAuth ? '插件协议' : '独立协议配置'}</h3><p className="channel-editor-note">{compatibleOAuth ? '地址与协议由插件维护。' : '仅在协议使用不同地址，或需要声明服务端状态时调整。'}</p>
                {!form.endpoints.length && <p className="channel-editor-note">请先在快捷配置中选择协议。</p>}
                {form.endpoints.map(endpoint => {
                  const format = endpoint.format;
                  const effectiveBase = (endpoint.base_url || form.base_url || '').trim();
                  const standard = formatStandardExamples[format];
                  return <details key={format} className="channel-editor-disclosure" data-endpoint={format}><summary>{endpointLabels[format]}<span>{endpoint.base_url ? '独立地址' : '共用默认地址'}</span><ChevronDown size={16} /></summary><div>
                    <Field label={compatibleOAuth ? '插件端点地址' : '自定义端点地址'} hint={compatibleOAuth ? '由兼容插件维护。' : '留空使用渠道 Base URL。'}><Input aria-label={`${format} 自定义地址`} type="url" readOnly={compatibleOAuth} placeholder={form.base_url || 'https://api.example.com'} value={endpoint.base_url} onChange={e => setEndpoint(format, { base_url: e.target.value })} /></Field>
                    <Field label="服务端状态" hint={compatibleOAuth ? '每次请求携带完整上下文。' : 'Responses 使用会话时，应按上游实际能力声明。'}><Select aria-label={`${format} 服务端状态`} disabled={compatibleOAuth} value={compatibleOAuth ? 'stateless' : endpoint.state || ''} onChange={e => setEndpoint(format, { state: e.target.value as Endpoint['state'] })}><option value="">未知</option><option value="stateless">无状态</option><option value="server_state">有服务端状态</option></Select></Field>
                    {!compatibleOAuth && <dl className="channel-editor-url-preview"><dt>请求地址</dt><dd>{previewEndpointUrl(effectiveBase, format, 'request') || '请填写有效的 Base URL'}</dd><dt>模型目录</dt><dd>{previewEndpointUrl(effectiveBase, format, 'models') || '—'}</dd><dt>标准样例</dt><dd>{standard.standardBase}<span>{standard.desc}</span></dd></dl>}
                  </div></details>;
                })}
                {oauth && <p className="channel-editor-note">OAuth 地址必须属于该平台允许的主机。</p>}
              </section>

            </section>
          </div>
        </div>
      </fieldset>
      {error && <div className="channel-editor-error"><ErrorBox message={error} /></div>}
      <div className="modal-footer"><span className="channel-editor-save-note">{busy ? '正在保存…' : '所有分区统一保存'}</span><Button variant="secondary" onClick={requestClose} disabled={locked}>取消</Button><Button type="submit" disabled={locked}>{busy ? '正在保存…' : editing ? '保存更改' : '创建渠道'}</Button></div>
    </form>
  </Modal>
  {showAddOAuth && <AddOAuthCredential channel={form} access={pluginAccess} onClose={() => setShowAddOAuth(false)} onChanged={() => { void syncNativePool().catch(cause => setError(errorMessage(cause))); }} resumeFlow={editorFlow} onStarted={onEditorFlow} />}
  {probe && <ChannelProbe channel={probe.channel} initialModel={probe.model} onClose={() => setProbe(null)} />}
  </>;
}

export function getChannelModelFormats(channel: Channel, modelName: string): string[] {
  const allEndpoints = (channel.endpoints || []).map(e => e.format);
  if (!modelName) return allEndpoints;
  const isCatalog = channel.credential_type === 'zen_free' || channel.credential_type === 'copilot_oauth';
  if (isCatalog) {
    if (!channel.model_formats) return [];
    const allowed = channel.model_formats[modelName];
    if (!allowed) return [];
    return allEndpoints.filter(f => allowed.includes(f));
  }
  if (!channel.model_formats) return allEndpoints;
  const allowed = channel.model_formats[modelName];
  if (!allowed) return allEndpoints;
  return allEndpoints.filter(f => allowed.includes(f));
}

function ChannelProbe({ channel, initialModel, onClose }: { channel: Channel; initialModel?: string; onClose: () => void }) {
  const choices = [...new Set([...(initialModel ? [initialModel] : []), ...channel.models])];
  const [selected, setSelected] = useState(initialModel || choices[0] || '');
  const [manual, setManual] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState<ProbeResult | null>(null);
  const actualModel = selected || manual.trim();

  // Advanced selection state
  const [showAdvanced, setShowAdvanced] = useState(false);
  const allEndpoints = channel.endpoints || [];
  const allCredentials = (channel.credentials && channel.credentials.length > 0)
    ? channel.credentials
    : [{ id: 'cred_default', label: '默认凭据', enabled: true }];

  const allowedFormats = useMemo(() => getChannelModelFormats(channel, actualModel), [channel, actualModel]);

  const [selectedFormats, setSelectedFormats] = useState<string[]>(() =>
    allowedFormats.length > 0 ? allowedFormats : (allEndpoints.length > 0 ? [allEndpoints[0].format] : ['openai_chat_completions'])
  );

  useEffect(() => {
    setSelectedFormats(curr => {
      const valid = curr.filter(f => allowedFormats.includes(f));
      return valid.length > 0 ? valid : allowedFormats;
    });
  }, [allowedFormats]);

  const [selectedCreds, setSelectedCreds] = useState<string[]>(() =>
    allCredentials.filter(c => c.enabled !== false).map(c => c.id)
  );

  const stored = channel.id > 0 && (!!channel.has_oauth || channel.credential_type === 'zen_free') && (channel.credential_type || 'api_key') !== 'api_key';
  const batchCount = (selectedFormats.length || 1) * (selectedCreds.length || 1);

  async function run() {
    if (busy || !actualModel) return;
    if (showAdvanced && (selectedFormats.length === 0 || selectedCreds.length === 0)) {
      setError('请至少选择一个测试协议和一个测试凭据。');
      return;
    }
    setBusy(true); setError(''); setResult(null);
    try {
      const payload: any = {
        channel: channelPayload(channel),
        model: actualModel,
        confirmed: true,
      };
      if (showAdvanced) {
        payload.formats = selectedFormats;
        payload.credential_ids = selectedCreds;
      }
      setResult(await api<ProbeResult>(stored ? `/channels/${encodeURIComponent(channel.id)}/probe` : '/channels/probe', {
        method: 'POST',
        body: JSON.stringify(payload),
      }));
    } catch (e) { setError(errorMessage(e)); } finally { setBusy(false); }
  }

  return <Modal title={`测试渠道 · ${channel.name || '未保存草稿'}`} description="验证一个上游模型的文本调用，不会保存或修改渠道配置。" onClose={onClose} busy={busy}>
    <div className="space-y-4">
      <p className="rounded-lg border border-amber-500/25 bg-amber-500/10 p-3 text-xs leading-relaxed text-amber-500 dark:text-amber-400 font-medium">此操作会向所选模型发送 hi 测试请求，可能消耗上游额度。不计入业务统计。仅在点击下方确认按钮后发送。</p>
      <Field label="测试模型"><Select disabled={busy} value={selected} onChange={e => { setSelected(e.target.value); setResult(null); setError(''); }}>{choices.map(model => <option key={model} value={model}>{model}</option>)}<option value="">手动输入实际上游模型 ID</option></Select></Field>
      {!selected && <Field label="实际上游模型 ID" hint="此处直接发送实际 ID。"><Input aria-label="实际上游模型 ID" disabled={busy} value={manual} onChange={e => { setManual(e.target.value); setResult(null); setError(''); }} placeholder="例如：gpt-4.1-mini" /></Field>}
      <p className="break-all text-xs text-[var(--color-text-secondary)]">将调用：{actualModel || '请填写模型 ID'}</p>
      <p className="text-xs leading-5 text-[var(--color-text-muted)]">已有密钥留空时，仅在地址与协议未改变时复用。</p>

      {/* 可展开的高级测试选项 */}
      <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)]/40 p-3">
        <button
          type="button"
          onClick={() => setShowAdvanced(!showAdvanced)}
          className="flex w-full items-center justify-between text-xs font-medium text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)] transition-colors"
        >
          <span className="flex items-center gap-1.5">
            {showAdvanced ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
            高级测试选项（指定协议端点与凭据）
          </span>
          <span className="text-[11px] text-[var(--color-text-muted)]">
            {showAdvanced
              ? `已选 ${selectedFormats.length} 协议 × ${selectedCreds.length} 凭据`
              : '默认走首选端点与默认凭据'}
          </span>
        </button>

        {showAdvanced && (
          <div className="mt-3 space-y-3 pt-3 border-t border-[var(--color-border)]">
            {/* 协议与端点 */}
            <div>
              <div className="flex items-center justify-between mb-1.5">
                <span className="text-xs font-semibold text-[var(--color-text-primary)]">测试协议与端点：</span>
                <div className="flex gap-2 text-[11px]">
                  <button
                    type="button"
                    className="text-[var(--color-accent)] hover:underline"
                    onClick={() => setSelectedFormats(allowedFormats)}
                  >
                    全选
                  </button>
                  <button
                    type="button"
                    className="text-[var(--color-text-muted)] hover:underline"
                    onClick={() => setSelectedFormats([])}
                  >
                    清空
                  </button>
                </div>
              </div>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-1.5">
                {allEndpoints.map(e => {
                  const isAllowed = allowedFormats.includes(e.format);
                  const checked = isAllowed && selectedFormats.includes(e.format);
                  return (
                    <label
                      key={e.format}
                      className={`flex items-center gap-2 p-2 rounded border text-xs select-none transition-colors ${
                        !isAllowed
                          ? 'border-[var(--color-border)] text-[var(--color-text-muted)] opacity-40 cursor-not-allowed bg-[var(--color-surface-2)]/20'
                          : checked
                          ? 'border-[var(--color-accent)] bg-[var(--color-accent)]/10 text-[var(--color-text-primary)] cursor-pointer'
                          : 'border-[var(--color-border)] text-[var(--color-text-muted)] opacity-70 hover:opacity-100 cursor-pointer'
                      }`}
                    >
                      <input
                        type="checkbox"
                        disabled={!isAllowed}
                        checked={checked}
                        onChange={ev => {
                          if (!isAllowed) return;
                          if (ev.target.checked) {
                            setSelectedFormats(curr => [...curr, e.format]);
                          } else {
                            setSelectedFormats(curr => curr.filter(f => f !== e.format));
                          }
                        }}
                        className="rounded border-[var(--color-border)] text-[var(--color-accent)]"
                      />
                      <span className="font-medium truncate">{endpointLabels[e.format] || e.format}</span>
                      {!isAllowed && <span className="text-[10px] text-[var(--color-text-muted)]">（目录未声明）</span>}
                    </label>
                  );
                })}
              </div>
            </div>

            {/* 凭据与账号 */}
            <div>
              <div className="flex items-center justify-between mb-1.5">
                <span className="text-xs font-semibold text-[var(--color-text-primary)]">测试凭据与账号：</span>
                <div className="flex gap-2 text-[11px]">
                  <button
                    type="button"
                    className="text-[var(--color-accent)] hover:underline"
                    onClick={() => setSelectedCreds(allCredentials.map(c => c.id))}
                  >
                    全选
                  </button>
                  <button
                    type="button"
                    className="text-[var(--color-text-muted)] hover:underline"
                    onClick={() => setSelectedCreds([])}
                  >
                    清空
                  </button>
                </div>
              </div>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-1.5">
                {allCredentials.map(cr => {
                  const checked = selectedCreds.includes(cr.id);
                  const displayLabel = cr.label || cr.id;
                  const keyNote = cr.masked_key || (cr.has_oauth ? 'OAuth 账号' : '');
                  return (
                    <label
                      key={cr.id}
                      className={`flex items-center gap-2 p-2 rounded border text-xs cursor-pointer select-none transition-colors ${
                        checked
                          ? 'border-[var(--color-accent)] bg-[var(--color-accent)]/10 text-[var(--color-text-primary)]'
                          : 'border-[var(--color-border)] text-[var(--color-text-muted)] opacity-70 hover:opacity-100'
                      }`}
                    >
                      <input
                        type="checkbox"
                        checked={checked}
                        onChange={ev => {
                          if (ev.target.checked) {
                            setSelectedCreds(curr => [...curr, cr.id]);
                          } else {
                            setSelectedCreds(curr => curr.filter(id => id !== cr.id));
                          }
                        }}
                        className="rounded border-[var(--color-border)] text-[var(--color-accent)]"
                      />
                      <div className="min-w-0 flex-1">
                        <div className="font-medium truncate">{displayLabel}</div>
                        {keyNote && <div className="text-[10px] text-[var(--color-text-muted)] truncate">{keyNote}</div>}
                      </div>
                    </label>
                  );
                })}
              </div>
            </div>
          </div>
        )}
      </div>

      {error && <ErrorBox message={error} />}
      {result && (result.formats
        ? <div className="space-y-2 max-h-64 overflow-y-auto pr-1">
            {result.formats.map((entry, idx) => (
              <div key={idx} className="p-2.5 rounded border border-[var(--color-border)] bg-[var(--color-surface)] text-xs space-y-1">
                <p role="status" className={`text-sm ${entry.ok ? 'text-teal-700 dark:text-teal-400' : 'text-red-700 dark:text-red-400'}`}>
                  {entry.credential_label ? `[${entry.credential_label}] ` : ''}{endpointLabels[entry.format] || entry.format}：{entry.ok ? '测试成功' : '测试失败'} · {entry.duration_ms} ms{entry.upstream_status != null && ` · 上游 HTTP ${entry.upstream_status}`}
                </p>
                {entry.diagnostic && <Diagnostic diagnostic={entry.diagnostic} />}
              </div>
            ))}
          </div>
        : <><p role="status" className="text-sm">{result.ok ? '测试成功' : '测试失败'} · {result.model} · {result.duration_ms} ms{result.upstream_status != null && ` · 上游 HTTP ${result.upstream_status}`}</p>{result.diagnostic && <Diagnostic diagnostic={result.diagnostic} />}</>)}
    </div>
    <div className="modal-footer">
      <Button type="button" variant="secondary" disabled={busy} onClick={onClose}>关闭</Button>
      <Button type="button" disabled={busy || !actualModel} onClick={run}>
        {busy ? '正在测试…' : showAdvanced && batchCount > 1 ? `确认并批量测试 (共 ${batchCount} 项)` : '确认并测试（可能收费）'}
      </Button>
    </div>
  </Modal>;
}

/** The channel page's state that outlives a route switch: the view, the editor
 * modal, and the flow each entry point has in flight. A half-finished
 * authorization is the expensive one to lose — the console keeps its flows
 * answerable for minutes — so switching to another page and back resumes
 * polling instead of starting a second authorization. Editor form drafts are
 * deliberately not kept: only what the server still holds is restored. */
const channelsStateKey = 'to-router.channels.v1';
type ChannelSortMode = 'name' | 'manual';
type ChannelDisplay = { sort_mode: ChannelSortMode; order: number[]; icons: Record<string, string> };
type ChannelDisplayPatch = { sort_mode?: ChannelSortMode; order?: number[]; icon?: { channel_id: number; value: string } };
type ChannelsState = { view: 'api_key' | 'oauth'; search: string; editor: { channelID: number; reauth: boolean } | null; flows: Record<string, string> };
const emptyChannelsState: ChannelsState = { view: 'api_key', search: '', editor: null, flows: {} };
function loadChannelsState(): ChannelsState {
  try {
    const raw = sessionStorage.getItem(channelsStateKey);
    if (!raw) return emptyChannelsState;
    const parsed = JSON.parse(raw) as Partial<ChannelsState>;
    return {
      view: parsed.view === 'oauth' ? 'oauth' : 'api_key',
      search: typeof parsed.search === 'string' ? parsed.search : '',
      editor: parsed.editor && typeof parsed.editor.channelID === 'number' ? { channelID: parsed.editor.channelID, reauth: !!parsed.editor.reauth } : null,
      flows: parsed.flows && typeof parsed.flows === 'object' ? parsed.flows : {},
    };
  } catch { return emptyChannelsState; }
}
function saveChannelsState(state: ChannelsState) {
  try { sessionStorage.setItem(channelsStateKey, JSON.stringify(state)); } catch { /* storage may be disabled */ }
}

export default function Channels() {
  // The view decides which slice of the channel list is fetched: the API key
  // view asks for the api_key type (a legacy row without one reads back as
  // it), the OAuth view asks once per provider so a card never waits on
  // accounts it does not show.
  const [restored] = useState(loadChannelsState);
  const pendingEditorRestore = useRef(restored.editor);
  const [pluginTarget] = useState(() => new URLSearchParams(window.location.hash.split('?')[1] || '').get('plugin'));
  const [discoverSourceID] = useState(() => new URLSearchParams(window.location.hash.split('?')[1] || '').get('source') || undefined);
  const [discoveryOpen, setDiscoveryOpen] = useState(() => new URLSearchParams(window.location.hash.split('?')[1] || '').get('discover') === '1');
  const plugins = usePlugins();
  const providers = useMemo<OAuthProvider[]>(() => [...oauthProviders, ...(plugins.data?.items || []).filter(item => item.credential_type?.startsWith('market:')).map(item => ({ type: item.credential_type as Exclude<CredentialType, 'api_key'>, title: item.name, note: item.description, official: false }))], [plugins.data]);
  const pluginAccess = useMemo(() => Object.fromEntries(providers.map(provider => {
    const plugin = plugins.data?.items?.find(item => item.credential_type === provider.type);
    const capability = plugin?.capabilities.find(item => item.id === 'connect');
    const allowed = !!plugins.data && !plugins.error && !!capability?.enabled && !!capability.available;
    const message = plugins.error ? `插件状态读取失败，已暂停 OAuth 联网操作：${plugins.error}` : !plugins.data ? '正在读取接入插件状态，暂不可执行联网操作。' : !capability?.enabled ? '接入插件未启用，账号与配置已保留' : !capability.available ? capability.unavailable_reason || '接入插件当前不可用，账号与配置已保留' : undefined;
    return [provider.type, { allowed, enabled: plugins.data ? capability?.enabled === true : undefined, icon: plugin?.icon, pluginID: plugin?.id, authMethods: plugin?.auth_methods, message } satisfies PluginAccess];
  })), [plugins.data, plugins.error, providers]);
  const targetProvider = pluginTarget ? plugins.data?.items?.find(item => item.id === pluginTarget)?.credential_type : undefined;
  const [target] = useState(managementTarget);
  const [locating, setLocating] = useState(target.channelID != null && target.channelID > 0);
  const [view, setView] = useState<'api_key' | 'oauth'>(pluginTarget ? 'oauth' : restored.view);
  // Plugin metadata may refresh independently; only refetch channels when the
  // requested type set changes, preserving loaded rows and action availability.
  const channelPathsKey = JSON.stringify(locating ? ['/channels'] : view === 'oauth' ? providers.map(provider => `/channels?credential_type=${provider.type}`) : ['/channels?credential_type=api_key']);
  const paths = useMemo<string[]>(() => JSON.parse(channelPathsKey), [channelPathsKey]);
  const { data: resourceData, setData: setResourceData, loadedKey, loading, error, reload } = useResources<Channel>(paths);
  // Keep visited views visible while their next read is pending. These rows
  // live only for this mounted page; authorized collections are not global cache data.
  const viewSnapshots = useRef(new Map<string, Channel[]>());
  useEffect(() => { if (resourceData && loadedKey) viewSnapshots.current.set(loadedKey, resourceData); }, [resourceData, loadedKey]);
  const data = loadedKey === JSON.stringify(paths) ? resourceData : viewSnapshots.current.get(JSON.stringify(paths)) ?? null;
  const quotaCatalog = useQuotaSnapshots([], view === 'api_key');
  const settings = useResource<SettingsData>('/settings');
  const display = useResource<ChannelDisplay>('/channels/display');
  const [search, setSearch] = useState(restored.search);
  const [editor, setEditor] = useState<{ channel: Channel; reauth?: boolean; section?: 'credentials' } | null>(null);
  const [selectedProvider, setSelectedProvider] = useState<OAuthProvider>();
  const [creatingProvider, setCreatingProvider] = useState<OAuthProvider>();
  const [newProviderName, setNewProviderName] = useState('');
  const [creatingChannel, setCreatingChannel] = useState(false);
  const [createError, setCreateError] = useState('');
  async function createProviderChannel(event: FormEvent) {
    event.preventDefault(); if (!creatingProvider || creatingChannel || !newProviderName.trim()) return;
    setCreatingChannel(true); setCreateError('');
    try { await api('/channels', { method: 'POST', body: JSON.stringify({ name: newProviderName.trim(), credential_type: creatingProvider.type, enabled: true }) }); setCreatingProvider(undefined); reload(); }
    catch (cause) { setCreateError(errorMessage(cause)); }
    finally { setCreatingChannel(false); }
  }
  const [deleting, setDeleting] = useState<Channel | null>(null);
  const [testing, setTesting] = useState<Channel | null>(null);
  const [deleteError, setDeleteError] = useState('');
  const [busy, setBusy] = useState<number | null>(null);
  const [actionError, setActionError] = useState('');
  const [displayActionError, setDisplayActionError] = useState('');
  const [savingDisplay, setSavingDisplay] = useState(false);
  const writingDisplay = useRef(false);
  const [iconChannel, setIconChannel] = useState<Channel | null>(null);
  // The flow each entry point has in flight, keyed by place: a provider card's
  // add flow, the editor's re-authorization, the editor's added account.
  const [flows, setFlows] = useState<Record<string, string>>(restored.flows);
  const [autoOpen, setAutoOpen] = useState(true);
  const toast = useToast();
  useEffect(() => {
    if (!locating || !data) return;
    const channel = data.find(row => row.id === target.channelID);
    if (channel) {
      setView(oauthTrack(channel) ? 'oauth' : 'api_key');
      setSearch('');
      const credentialID = new URLSearchParams(window.location.hash.split('?')[1] || '').get('credential_id');
      if (credentialID && !channel.credentials?.some(credential => credential.id === credentialID)) setActionError('关注凭证已不存在，请重新选择。');
      setEditor({ channel, section: credentialID ? 'credentials' : undefined });
    } else setActionError('此渠道已不存在，请刷新渠道列表。');
    setLocating(false);
  }, [data, locating, target.channelID]);
  useEffect(() => { if (settings.data) setAutoOpen(settings.data.oauth_auto_open !== false); }, [settings.data]);
  useEffect(() => { saveChannelsState({ view, search, editor: editor ? { channelID: editor.channel.id, reauth: !!editor.reauth } : null, flows }); }, [view, search, editor, flows]);
  // The editor comes back with the channel row it was editing, once the list
  // has it: the row is the only stored copy of that channel.
  useEffect(() => {
    if (!data) return;
    const pending = pendingEditorRestore.current;
    pendingEditorRestore.current = null;
    if (editor || pluginTarget || target.channelID || !pending) return;
    const found = data.find(channel => channel.id === pending.channelID);
    if (found) setEditor({ channel: found, reauth: pending.reauth });
  }, [data, editor, target.channelID, pluginTarget]);
  async function toggleAutoOpen(next: boolean) {
    if (!settings.data) return;
    setAutoOpen(next);
    try {
      await api('/settings/oauth-auto-open', { method: 'PUT', body: JSON.stringify({ oauth_auto_open: next }) });
      toast(next ? '授权时将自动打开浏览器' : '授权时不再自动打开；复制授权链接到任意浏览器登录');
    } catch (e) { setAutoOpen(!next); setActionError(errorMessage(e)); }
  }
  const query = search.trim().toLowerCase();
  const orderedChannels = useMemo(() => {
    const order = display.data?.sort_mode === 'manual' ? display.data.order : [];
    const ranks = new Map(order.map((id, index) => [id, index]));
    return [...(data || [])].sort((a, b) => (ranks.get(a.id) ?? order.length) - (ranks.get(b.id) ?? order.length) || a.name.localeCompare(b.name) || a.id - b.id);
  }, [data, display.data]);
  const channels = useMemo(() => orderedChannels.filter(c => `${c.name} #${c.id} ${c.base_url || ''} ${(c.endpoints || []).map(e => `${endpointLabels[e.format]} ${e.base_url}`).join(' ')} ${(c.models || []).join(' ')}`.toLowerCase().includes(query)), [orderedChannels, query]);
  const oauthPools = useMemo(() => providers.map(provider => ({ provider, channels: (data || []).filter(channel => oauthTrack(channel) === provider.type).sort((a, b) => a.name.localeCompare(b.name)) })), [data, providers]);
  function openEditor(channel: Channel, reauth = false, section?: 'credentials') { setEditor({ channel, reauth, section }); }
  /** Remembers (or forgets) the flow an entry point has in flight, so a route
   * switch can resume polling it. */
  function trackFlow(key: string, flowID: string | null) {
    setFlows(current => {
      const next = { ...current };
      if (flowID) next[key] = flowID; else delete next[key];
      return next;
    });
  }
  function retainSavedCredentialOrder(channelID: number, ids: string[]) {
    setResourceData(current => current?.map(channel => {
      if (channel.id !== channelID) return channel;
      const credentials = channel.credentials || [];
      const byID = new Map(credentials.map(credential => [credential.id, credential]));
      // A concurrent channel edit may have replaced the credential pool.
      if (credentials.length !== ids.length || new Set(ids).size !== ids.length || !ids.every(id => byID.has(id))) return channel;
      return { ...channel, credentials: ids.map(id => byID.get(id)!) };
    }) ?? current);
  }
  async function copyAddress(channel: Channel) {
    setActionError('');
    try { await navigator.clipboard.writeText(channel.endpoints[0] ? endpointUrl(channel.endpoints[0], channel.base_url || '') : channel.base_url || ''); toast('上游地址已复制'); }
    catch { setActionError('复制失败，请手动选择上游地址复制。'); }
  }
  async function toggle(channel: Channel) {
    setBusy(channel.id); setActionError('');
    try { await api(`/channels/${encodeURIComponent(channel.id)}`, { method: 'PUT', body: JSON.stringify({ ...channelPayload(channel), api_key: '', enabled: !channel.enabled }) }); toast(channel.enabled ? '渠道已停用' : '渠道已启用'); reload(); }
    catch (e) { setActionError(errorMessage(e)); } finally { setBusy(null); }
  }
  async function remove() {
    if (!deleting) return;
    setBusy(deleting.id); setDeleteError('');
    try { await api(`/channels/${encodeURIComponent(deleting.id)}`, { method: 'DELETE' }); toast('渠道已删除'); setDeleting(null); reload(); display.reload(); }
    catch (e) { setDeleteError(errorMessage(e)); } finally { setBusy(null); }
  }
  const apiBlocked = busy != null || loading || !!error || savingDisplay;
  const displayBlocked = apiBlocked || !display.data || display.loading || !!display.error;
  const sortMode = display.data?.sort_mode || 'name';

  async function saveDisplay(patch: ChannelDisplayPatch) {
    if (displayBlocked || writingDisplay.current) throw new Error('显示设置尚未就绪，请稍后重试。');
    writingDisplay.current = true; setSavingDisplay(true); setDisplayActionError('');
    try {
      const next = await api<ChannelDisplay>('/channels/display', { method: 'PUT', body: JSON.stringify(patch) });
      display.setData(next);
    } finally { writingDisplay.current = false; setSavingDisplay(false); }
  }
  async function saveOrder(order: number[]) {
    try { await saveDisplay({ sort_mode: 'manual', order }); }
    catch (cause) { setDisplayActionError(`未能保存显示顺序：${errorMessage(cause)}`); throw cause; }
  }
  async function switchSortMode(mode: ChannelSortMode) {
    if (mode === sortMode || displayBlocked || writingDisplay.current) return;
    clearDrag();
    try { await saveDisplay({ sort_mode: mode }); }
    catch (cause) { setDisplayActionError(`未能切换排序方式：${errorMessage(cause)}`); }
  }
  function moveChannel(from: number, to: number) {
    if (displayBlocked || writingDisplay.current || from === to) return;
    const visible = channels.map(channel => channel.id);
    const fromIndex = visible.indexOf(from), toIndex = visible.indexOf(to);
    if (fromIndex < 0 || toIndex < 0) return;
    visible.splice(toIndex, 0, visible.splice(fromIndex, 1)[0]);
    void saveVisibleOrder(visible.map(String)).catch(() => {});
  }
  async function saveVisibleOrder(ids: string[]) {
    // Replace only visible slots so searching cannot move a hidden channel.
    const visibleIDs = new Set(ids.map(Number));
    let index = 0;
    await saveOrder(orderedChannels.map(channel => visibleIDs.has(channel.id) ? Number(ids[index++]) : channel.id));
  }
  const sortable = useSortableList({
    ids: channels.map(channel => String(channel.id)),
    disabled: displayBlocked || view !== 'api_key',
    revision: JSON.stringify([view, search, data, display.data]),
    onCommit: saveVisibleOrder,
  });
  function clearDrag() { sortable.cancel(); }
  function refreshChannels() {
    if (writingDisplay.current) return;
    setDisplayActionError(''); reload(); display.reload(); void plugins.reload();
  }
  return <div className="upstream-access">
    <PageHeader eyebrow="UPSTREAM ACCESS" title="上游接入" description="管理上游服务的连接、凭据与模型目录。" actions={<><a className="btn btn-ghost" href="#/plugins">插件库</a><Button variant="secondary" onClick={refreshChannels} disabled={loading || display.loading || savingDisplay} aria-label="刷新渠道"><RefreshCw size={16} className={loading || display.loading ? 'animate-spin' : ''} />刷新</Button><Button variant="secondary" onClick={() => setDiscoveryOpen(true)}><Radar size={16} />发现凭证</Button><Button disabled={savingDisplay || view === 'oauth' && (!selectedProvider || selectedProvider.type.startsWith('market:'))} onClick={() => { if (view === 'oauth' && selectedProvider) { setCreatingProvider(selectedProvider); const count = (data || []).filter(channel => channel.credential_type === selectedProvider.type).length; setNewProviderName(`${selectedProvider.title}渠道${count ? count + 1 : ''}`); setCreateError(''); } else setEditor({ channel: { ...emptyChannel } }); }}><Plus size={17} />添加渠道</Button></>} />
    {plugins.error && view === 'oauth' && <ErrorBox message={`插件状态读取失败，已暂停 OAuth 联网操作：${plugins.error}`} retry={() => void plugins.reload()} />}
    <section className={`panel ${view === 'oauth' ? '!overflow-visible' : 'upstream-panel'}`}>
      <div className="panel-toolbar upstream-toolbar">
        <div className="flex flex-wrap items-center gap-3">
          <div className="segmented" aria-label="渠道视图"><button type="button" disabled={savingDisplay} aria-pressed={view === 'api_key'} className={view === 'api_key' ? 'active' : ''} onClick={() => { clearDrag(); setView('api_key'); }}>API Key 渠道</button><button type="button" disabled={savingDisplay} aria-pressed={view === 'oauth'} className={view === 'oauth' ? 'active' : ''} onClick={() => { clearDrag(); setView('oauth'); }}>OAuth 账号渠道</button></div>
        </div>
        <div className="search-field upstream-search"><Search size={16} /><Input aria-label="搜索渠道或模型" placeholder="搜索名称、模型或地址" value={search} onChange={e => { clearDrag(); setSearch(e.target.value); }} />{search && <button type="button" aria-label="清除搜索" onClick={() => { clearDrag(); setSearch(''); }}><X size={15} /></button>}</div>
      </div>
      {view === 'api_key' && <div className="upstream-list-summary"><h2>{data ? query ? `${channels.length} 个匹配 · 共 ${data.length} 个渠道` : `${data.length} 个渠道 · ${data.filter(c => c.enabled).length} 个启用` : 'API Key 渠道'}</h2><div className="upstream-order-controls"><span role="status">{savingDisplay ? '正在保存显示设置…' : loading && data ? '正在刷新…' : display.loading ? '正在读取显示设置…' : display.error ? display.data ? '显示设置未刷新' : '显示顺序未加载' : <ArrowDownWideNarrow size={14} />}</span><div className="segmented upstream-sort-mode" role="group" aria-label="渠道排序方式"><button type="button" disabled={displayBlocked} aria-pressed={sortMode === 'name'} className={sortMode === 'name' ? 'active' : ''} onClick={() => void switchSortMode('name')}>按名称</button><button type="button" disabled={displayBlocked} aria-pressed={sortMode === 'manual'} className={sortMode === 'manual' ? 'active' : ''} onClick={() => void switchSortMode('manual')}>手动排列</button></div></div></div>}
      {error && <div className="p-5"><ErrorBox message={data ? view === 'api_key' ? `刷新失败，仍显示上次数据。${error}` : `刷新失败，已保留当前数据。${error}` : error} retry={reload} /></div>}
      {actionError && <div className="px-5 pt-4"><ErrorBox message={actionError} /></div>}
      {view === 'api_key' && display.error && <div className="px-5 pt-4"><ErrorBox message={`${display.data ? '显示设置刷新失败，保留上次设置' : '显示设置未加载，暂按名称展示'}：${display.error}`} retry={display.reload} /></div>}
      {view === 'api_key' && displayActionError && <div className="px-5 pt-4"><ErrorBox message={displayActionError} retry={refreshChannels} /></div>}
      {view === 'oauth'
        ? <OAuthAccounts onProviderSelected={setSelectedProvider} search={query} autoOpen={autoOpen} autoOpenDisabled={!settings.data} onAutoOpenChange={toggleAutoOpen} pluginAccess={pluginAccess} initialProvider={targetProvider} pools={oauthPools} loading={loading} flows={flows} onFlowStarted={trackFlow} onReauthorize={channel => { if (pluginAccess[oauthTrack(channel) || '']?.allowed) openEditor(channel, true); }} onEdit={(channel, section) => openEditor(channel, false, section)} onAdded={reload} onCredentialOrderSaved={retainSavedCredentialOrder} onTest={channel => { if (pluginAccess[oauthTrack(channel) || '']?.allowed) setTesting({ ...channel, api_key: '' }); }} onToggle={toggle} busy={busy} onDelete={channel => { setDeleting(channel); setDeleteError(''); }} />
        : !data && loading ? <Loading /> : data && channels.length === 0 ? <Empty title={query ? '未找到匹配渠道' : '连接你的第一个渠道'} description={query ? '试试其他名称、模型或地址。' : '添加一个上游服务，即可开始通过统一入口调用模型。'} action={query ? <Button variant="secondary" onClick={() => setSearch('')}>清除搜索</Button> : <Button onClick={() => setEditor({ channel: { ...emptyChannel } })}><Plus size={16} />添加渠道</Button>} /> : data && <div className="upstream-list" aria-busy={loading} {...sortable.listProps}>{channels.map((channel, rowIndex) => {
          const address = channel.endpoints[0] ? endpointUrl(channel.endpoints[0], channel.base_url || '') : channel.base_url || '';
          const addressCount = new Set(channel.endpoints.map(endpoint => endpointUrl(endpoint, channel.base_url || '')).filter(Boolean)).size;
          const credentials = channel.credentials || [];
          const credentialCount = credentials.length || (channel.has_key || channel.has_oauth ? 1 : 0);
          const enabledCredentials = credentials.length ? credentials.filter(credential => credential.enabled).length : credentialCount;
          const models = channel.models || [];
          const matchedModels = query ? models.filter(model => model.toLowerCase().includes(query)) : [];
          const previewModels = (matchedModels.length ? matchedModels : models).slice(0, 3);
          return <article key={channel.id} data-channel-id={channel.id} className="upstream-row" aria-label={`渠道 ${channel.name}`} {...sortable.itemProps(String(channel.id))}>
            <div className="upstream-identity">
              <button type="button" className="upstream-drag-handle" aria-label={`拖动排序 ${channel.name}`} title="拖动改变显示顺序，也可用上下方向键移动" aria-keyshortcuts="ArrowUp ArrowDown" disabled={displayBlocked || channels.length < 2} {...sortable.handleProps(String(channel.id))} onKeyDown={event => { const target = event.key === 'ArrowUp' ? channels[rowIndex - 1] : event.key === 'ArrowDown' ? channels[rowIndex + 1] : undefined; if (target) { event.preventDefault(); moveChannel(channel.id, target.id); } }}><GripVertical size={17} /></button>
              <button type="button" className="upstream-provider" aria-label={`更换 ${channel.name} 图标`} title="更换图标" disabled={displayBlocked} onClick={() => setIconChannel(channel)}><ProviderIcon name={channel.name} baseUrl={address} override={display.data?.icons?.[String(channel.id)]} size={24} /></button>
              <div className="upstream-title"><h3><button type="button" className="upstream-name" aria-label={`编辑渠道名称 ${channel.name}`} disabled={apiBlocked} onClick={() => openEditor(channel)}>{channel.name}</button></h3>{!!channel.tags?.length && <div className="upstream-tags">{channel.tags.map(tag => <TagBadge key={tag} tagId={tag} size="sm" />)}</div>}</div>
            </div>
            <div className="upstream-actions">
              <div className="upstream-enabled" title="渠道启停配置，不代表上游可用性"><span>{channel.enabled ? '已启用' : '已停用'}</span><button role="switch" aria-checked={channel.enabled} aria-label={`${channel.enabled ? '停用' : '启用'} ${channel.name}`} disabled={apiBlocked} className={`switch ${channel.enabled ? 'switch-on' : ''}`} onClick={() => toggle(channel)}><span /></button></div>
              <Button variant="secondary" aria-label={`编辑 ${channel.name}`} disabled={apiBlocked} onClick={() => openEditor(channel)}><Pencil size={15} />编辑</Button>
              <Button variant="ghost" aria-label={`测试渠道 ${channel.name}`} disabled={apiBlocked} onClick={() => setTesting({ ...channel, api_key: '' })}>测试</Button>
              <details className="upstream-more" onKeyDown={e => { if (e.key === 'Escape') { e.currentTarget.open = false; e.currentTarget.querySelector('summary')?.focus(); } }} onBlur={e => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) e.currentTarget.open = false; }}><summary aria-label={`更多操作 ${channel.name}`} title="更多操作"><MoreHorizontal size={18} /></summary><div><Button variant="ghost" aria-label={`上移 ${channel.name}`} disabled={displayBlocked || rowIndex === 0} onClick={() => moveChannel(channel.id, channels[rowIndex - 1].id)}><ArrowUp size={15} />上移</Button><Button variant="ghost" aria-label={`下移 ${channel.name}`} disabled={displayBlocked || rowIndex === channels.length - 1} onClick={() => moveChannel(channel.id, channels[rowIndex + 1].id)}><ArrowDown size={15} />下移</Button><Button variant="ghost" aria-label={`删除 ${channel.name}`} disabled={apiBlocked} onClick={e => { e.currentTarget.closest('details')?.removeAttribute('open'); setDeleting(channel); setDeleteError(''); }}><Trash2 size={15} />删除渠道</Button></div></details>
            </div>
            <div className="upstream-connection">
              <div className="upstream-address"><span className="upstream-label">首选地址</span><span className="upstream-url" title={address}>{address || '未配置地址'}</span>{address && <Button variant="ghost" aria-label={`复制 ${channel.name} 上游地址`} title="复制首选上游地址" onClick={() => copyAddress(channel)}><Copy size={14} /></Button>}{addressCount > 1 && <span className="upstream-secondary" title={channel.endpoints.map(endpoint => `${endpointLabels[endpoint.format]}：${endpointUrl(endpoint, channel.base_url || '')}`).join('\n')}>另 {addressCount - 1} 个地址</span>}<button type="button" className="upstream-credential-link" aria-label={`配置 ${channel.name} 凭据`} disabled={apiBlocked} onClick={() => openEditor(channel, false, 'credentials')}><KeyRound size={13} />{credentialCount ? `${credentialCount} 个凭据${enabledCredentials !== credentialCount ? ` · ${enabledCredentials} 个启用` : ''}` : '未配置凭据'}</button></div>
              <ChannelQuota items={quotaCatalog.items.filter(item => item.channel_id === channel.id && item.kind === 'balance')} />
              <div className="upstream-configuration"><div className="upstream-protocols">{channel.endpoints.map(endpoint => <span key={endpoint.format}>{endpointLabels[endpoint.format]}{endpoint.state === 'server_state' ? ' · 有状态' : ''}</span>)}</div>{credentialCount > 1 && <span>{channel.dispatch_mode === 'round_robin' ? '轮询' : '故障转移'}{channel.chain_type === 'cyclic' ? '（循环）' : '（单向）'}</span>}</div>
            </div>
            <div className="upstream-models">{models.length ? <details><summary aria-label={`查看 ${channel.name} 全部模型`}><Layers size={14} /><span>{models.length} 个启用模型</span><span className="upstream-model-preview">{previewModels.join(' · ')}{models.length > previewModels.length ? ' …' : ''}</span><ChevronDown size={15} /></summary><div className="upstream-model-list">{models.map(model => <span key={model} title={model}>{model}</span>)}</div></details> : <span className="upstream-no-models"><Layers size={14} />尚未启用模型</span>}</div>
          </article>;
        })}</div>}
    </section>
    {view === 'api_key' && <p className="upstream-list-note">切换排序方式会保留手动顺序；拖动或上下移将切到手动排列。点击图标可更换，调用顺序在公开模型中设置。</p>}
    {creatingProvider && <Modal title="添加渠道" onClose={() => setCreatingProvider(undefined)} busy={creatingChannel}><form onSubmit={createProviderChannel}><p className="mb-3 text-sm">{creatingProvider.title} · {creatingProvider.type === 'zen_free' ? '公共访问，无需账号登录。' : '创建后在渠道中添加账号 / 工作区。'}</p><Field label="渠道名称"><Input aria-label="渠道名称" autoFocus value={newProviderName} disabled={creatingChannel} onChange={event => setNewProviderName(event.target.value)} /></Field>{createError && <ErrorBox message={createError} />}<div className="modal-footer"><Button variant="secondary" disabled={creatingChannel} onClick={() => setCreatingProvider(undefined)}>取消</Button><Button type="submit" disabled={creatingChannel || !newProviderName.trim()}>{creatingChannel ? '正在创建…' : '创建渠道'}</Button></div></form></Modal>}
    {discoveryOpen && <KeyDiscovery initialSourceID={discoverSourceID} onChanged={reload} onClose={() => setDiscoveryOpen(false)} />}
    {editor && <ChannelEditor pluginAccess={pluginAccess[oauthTrack(editor.channel) || '']} channel={editor.channel} icon={display.data?.icons?.[String(editor.channel.id)]} iconBlocked={displayBlocked} onIconChange={() => setIconChannel(editor.channel)} allChannels={data || []} onChanged={reload} initialReauth={editor.reauth} initialSection={editor.section} editorFlow={flows[`editor:${editor.channel.id}`]} onEditorFlow={flowID => trackFlow(`editor:${editor.channel.id}`, flowID)} onClose={() => setEditor(null)} onSaved={() => { setEditor(null); reload(); }} />}
    {iconChannel && <ChannelIconPicker name={iconChannel.name} fallbackIcon={pluginAccess[oauthTrack(iconChannel) || '']?.icon} baseUrl={iconChannel.endpoints[0] ? endpointUrl(iconChannel.endpoints[0], iconChannel.base_url || '') : iconChannel.base_url} value={display.data?.icons?.[String(iconChannel.id)]} onClose={() => setIconChannel(null)} onSelect={value => saveDisplay({ icon: { channel_id: iconChannel.id, value } })} />}
    {testing && (!oauthTrack(testing) || pluginAccess[oauthTrack(testing)!]?.allowed) && <ChannelProbe channel={testing} onClose={() => setTesting(null)} />}
    {deleting && <Modal title="删除渠道？" description={`将删除「${deleting.name}」及其连接配置，此操作无法撤销。`} onClose={() => setDeleting(null)} busy={!!busy}>{deleteError && <ErrorBox message={deleteError} />}<div className="modal-footer"><Button variant="secondary" disabled={!!busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" disabled={!!busy} onClick={remove}>{busy ? '正在删除…' : '确认删除'}</Button></div></Modal>}
  </div>;
}
