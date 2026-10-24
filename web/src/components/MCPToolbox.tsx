import { useEffect, useRef, useState } from 'react';
import { Activity, ArrowRight, BookOpen, Boxes, ChartNoAxesCombined, ChevronDown, CircleDollarSign, KeyRound, Layers, PackageOpen, Search, ShieldCheck, Sparkles, WandSparkles, Wrench, X } from 'lucide-react';
import { errorMessage } from '../api';
import { mcp } from '../services/mcp';
import { mcpCategories, mcpCoreTools, mcpToolCategory, mcpToolMatches, mcpToolTitle, mcpToolSummary, type MCPCategory } from '../services/mcpCatalogue';
import type { MCPPermission, MCPTool } from '../types/mcp';
import { Button, ErrorBox, Input, Loading, Modal } from './ui';
import './MCPToolbox.css';

const icons = { channels: KeyRound, models: Layers, automation: WandSparkles, plugins: PackageOpen, pricing: CircleDollarSign, usage: ChartNoAxesCombined, access: ShieldCheck, system: Activity, browse: BookOpen, other: Boxes };
const permissionLabels: Record<MCPPermission, string> = { read: '查看', manage: '日常管理', maintain: '维护', sensitive: '敏感读取' };

export function MCPToolbox({ cachedTools, onLoad, onClose }: { cachedTools: MCPTool[] | null; onLoad: (tools: MCPTool[]) => void; onClose: () => void }) {
  const [tools, setTools] = useState(cachedTools), [error, setError] = useState('');
  const [attempt, setAttempt] = useState(0), [loading, setLoading] = useState(!cachedTools);
  const [selection, setSelection] = useState<'core' | 'all' | MCPCategory['id']>('core');
  const [query, setQuery] = useState('');
  const list = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (cachedTools) return;
    const controller = new AbortController();
    setLoading(true); setError('');
    void mcp.tools(controller.signal).then(value => {
      if (controller.signal.aborted) return;
      setTools(value); setLoading(false); onLoad(value);
    }).catch(e => { if (!controller.signal.aborted) setError(`暂时无法读取功能列表：${errorMessage(e)}`); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [cachedTools, onLoad, attempt]);

  const all = tools || [], capabilityCount = new Set(all.map(tool => tool.capability)).size;
  const core = mcpCoreTools.flatMap(name => all.filter(tool => tool.name === name));
  const searching = !!query.trim();
  const category = mcpCategories.find(item => item.id === selection);
  const selected = selection === 'core' ? core : selection === 'all' ? all : all.filter(tool => mcpToolCategory(tool).id === selection);
  // Search always spans all tools, including those outside the current category.
  const shown = searching ? all.filter(tool => mcpToolMatches(tool, query)) : selected;
  const title = searching ? '搜索结果' : selection === 'core' ? '把这些日常工作交给 Agent' : category?.label || '全部功能';
  function choose(value: typeof selection) { setSelection(value); setQuery(''); if (list.current) list.current.scrollTop = 0; }
  function search(value: string) { setQuery(value); if (list.current) list.current.scrollTop = 0; }

  return <Modal title="MCP 功能工具箱" description="了解 Agent 能为你做什么。客户端可用范围取决于连接授权。" onClose={onClose} wide className="mcp-toolbox" initialFocus="#mcp-tool-search">
    <div className="mcp-toolbox-toolbar">
      <label className="mcp-toolbox-search"><Search size={18} aria-hidden="true" /><Input id="mcp-tool-search" type="search" aria-label="搜索MCP功能" placeholder="搜索功能、用途或工具名" value={query} onChange={event => search(event.target.value)} />{query && <Button variant="ghost" aria-label="清空功能搜索" onClick={() => search('')}><X size={16} /></Button>}</label>
      <span className="mcp-toolbox-total">{tools ? <><strong>{all.length}</strong> 个工具 <span>·</span> <strong>{capabilityCount}</strong> 项能力</> : '读取当前功能目录'}</span>
    </div>
    <div className="mcp-toolbox-layout">
      <nav className="mcp-toolbox-nav" aria-label="MCP功能分类">
        <button type="button" aria-pressed={!searching && selection === 'core'} onClick={() => choose('core')}><Sparkles size={18} /><span>常用功能</span><small>{core.length}</small></button>
        <button type="button" aria-pressed={!searching && selection === 'all'} onClick={() => choose('all')}><Boxes size={18} /><span>全部功能</span><small>{all.length}</small></button>
        <div className="mcp-toolbox-nav-divider" />
        {mcpCategories.map(item => {
          const count = all.filter(tool => mcpToolCategory(tool).id === item.id).length;
          if (!count) return null;
          const Icon = icons[item.id];
          return <button type="button" key={item.id} className={`mcp-tone-${item.tone}`} aria-pressed={!searching && selection === item.id} onClick={() => choose(item.id)}><Icon size={18} /><span>{item.label}</span><small>{count}</small></button>;
        })}
      </nav>
      <div className="mcp-toolbox-list" ref={list}>
        {loading && !tools ? <Loading /> : error ? <ErrorBox message={error} retry={() => setAttempt(value => value + 1)} /> : <>
          <header className="mcp-toolbox-list-heading"><div><p className="mcp-toolbox-eyebrow">{searching ? '快速查找' : selection === 'core' ? '常用功能' : '功能目录'}</p><h3>{title}</h3>{!searching && <p>{category?.hint || (selection === 'core' ? '渠道、模型、用量和自动化，直接用自然语言提出需求。' : '来自当前网关的工具注册表，展开查看功能说明。')}</p>}</div><span className="mcp-toolbox-result-count" role="status">{shown.length} 个工具</span></header>
          {shown.length ? <div className="mcp-toolbox-grid">{shown.map(tool => <ToolCard key={tool.name} tool={tool} />)}</div> : <div className="mcp-toolbox-empty"><Search size={30} /><h4>{searching ? '没有匹配的功能' : '暂无可展示的工具'}</h4><p>{searching ? '试试“额度”“模型”“价格”，或输入工具名。' : '刷新 MCP 状态后重新打开工具箱。'}</p>{searching && <Button variant="secondary" onClick={() => search('')}>清空搜索</Button>}</div>}
          {selection === 'core' && !searching && all.length > core.length && <button type="button" className="mcp-toolbox-explore" onClick={() => choose('all')}>浏览全部 {all.length} 个工具 <ArrowRight size={16} /></button>}
        </>}
      </div>
    </div>
    <div className="mcp-toolbox-footer"><Wrench size={15} /><span>工具是 Agent 的操作入口；多个工具可以共同完成一项能力。</span></div>
  </Modal>;
}

function ToolCard({ tool }: { tool: MCPTool }) {
  const category = mcpToolCategory(tool), Icon = icons[category.id];
  const summary = mcpToolSummary(tool);
  return <details className={`mcp-tool-card mcp-tone-${category.tone}`}>
    <summary><div className="mcp-tool-card-heading"><span className="mcp-tool-card-icon"><Icon size={19} /></span><h4>{mcpToolTitle(tool)}</h4><ChevronDown size={15} className="mcp-tool-card-chevron" /></div><p>{summary}</p><div className="mcp-tool-card-meta"><span>{category.label}</span><span className={`mcp-tool-permission mcp-permission-${tool.permission}`} title={`基础权限：${permissionLabels[tool.permission]}`}>{permissionLabels[tool.permission]}</span></div></summary>
    <div className="mcp-tool-card-detail"><p>{tool.description}</p><dl><div><dt>工具</dt><dd><code>{tool.name}</code></dd></div><div><dt>能力</dt><dd><code>{tool.capability}</code></dd></div></dl></div>
  </details>;
}
