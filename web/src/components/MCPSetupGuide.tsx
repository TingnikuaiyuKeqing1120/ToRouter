import { Copy, ExternalLink } from 'lucide-react';
import { useState } from 'react';
import { Button } from './ui';
import { mcpAgentInstructions, mcpClients, mcpClientSetup, mcpTestPrompt, type MCPClient } from '../services/mcpSetup';
import type { MCPStatus } from '../types/mcp';

export function MCPClientChoice({ value, onChange }: { value: MCPClient; onChange: (value: MCPClient) => void }) {
  return <div className="mcp-client-choice" role="group" aria-label="选择接入工具">{mcpClients.map(client => <button type="button" key={client.id} aria-pressed={value === client.id} onClick={() => onChange(client.id)}>{client.name}</button>)}</div>;
}

export function MCPSetupGuide({ status, client, onClientChange, onCreate, canCreate, copy, copyMessage }: {
  status: MCPStatus | null; client: MCPClient; onClientChange: (value: MCPClient) => void;
  onCreate: () => void; canCreate: boolean; copy: (value: string, label: string) => Promise<void>; copyMessage: string;
}) {
  const [mode, setMode] = useState<'human' | 'agent'>('human');
  let setup: ReturnType<typeof mcpClientSetup> | null = null;
  let agentText = '';
  try { if (status) { setup = mcpClientSetup(status, client); agentText = mcpAgentInstructions(status, client); } } catch { /* Wait for a valid instance URL. */ }
  return <details className="panel mcp-guide" open>
    <summary>连接指引 <span className="mcp-secondary">从添加连接到第一次使用</span></summary>
    <div className="mcp-guide-content">
      <MCPClientChoice value={client} onChange={onClientChange} />
      <div className="mcp-guide-mode" role="group" aria-label="选择配置方式">
        <button type="button" aria-pressed={mode === 'human'} onClick={() => setMode('human')}>自己配置</button>
        <button type="button" aria-pressed={mode === 'agent'} onClick={() => setMode('agent')}>交给 Agent</button>
      </div>
      {mode === 'human' && client === 'general' ? <div className="mcp-overview">
        <section><h3>MCP 能帮你做什么？</h3><p>MCP 是让 AI 工具调用外部功能的连接方式。这里由 to-router 提供查询和管理工具，你在客户端聊天中提出要求，Agent 就能按授权查询用量、查看模型或调整配置。</p><p className="mcp-overview-flow">你提出要求 → Agent 调用工具 → to-router 执行并返回结果</p></section>
        <section><h3>连接时，各部分负责什么？</h3><dl><div><dt>服务端</dt><dd>to-router 提供工具和数据。MCP 已内置，使用时保持程序运行，无需另外安装服务端。</dd></div><div><dt>客户端</dt><dd>你使用的 AI 工具负责读取工具说明，由 Agent 根据任务选择调用。先在它的 MCP 设置中添加 to-router。</dd></div><div><dt>地址与密钥</dt><dd>地址告诉客户端连接哪里；管理连接密钥证明身份，并决定可以查看、修改哪些内容。为不同客户端分别创建连接，便于单独停用或轮换。</dd></div></dl></section>
        <section><h3>怎样判断能不能接入？</h3><p>先在工具设置或官方说明中找 MCP 入口。支持 Streamable HTTP 并允许填写 Authorization 请求头时，使用本页地址与管理密钥；如果只支持 stdio，则使用下方 CLI 备用资料。找不到入口或不确定支持情况，可以切到“交给 Agent”让它检查。</p></section>
        <section><h3>通常只需配置一次</h3><p>启动 to-router → 创建管理连接 → 按客户端要求填写地址和密钥 → 重载连接并发起一次查询。上方已有对应工具时，可直接查看它的具体步骤。</p><p>例如：“查看本月用量，并列出消耗最多的渠道。”看到实际工具调用和返回数据，才算连通。之后直接在聊天中说明目标即可；密钥轮换或地址变化时，再更新客户端配置。</p></section>
        {setup && <details><summary>查看通用连接资料</summary><p>{setup.existing}</p><pre tabIndex={0}>{setup.configuration}</pre><Button variant="secondary" onClick={() => void copy(setup!.configuration, '通用连接资料')}><Copy size={15} />复制连接资料</Button></details>}
        <details><summary>它与模型调用的 URL、Key 有什么区别？</summary><p>模型调用连接让客户端通过网关使用上游模型；MCP 管理连接让 Agent 操作网关中的配置和数据。两者分别配置：MCP 使用这里创建的管理连接密钥，模型调用使用“客户端接入”页的 Key。</p><p>默认允许查看和日常管理；只想查询可只选“查看”，维护与敏感读取按需另授。你仍可在本页随时停用某个管理连接。</p></details>
      </div> : mode === 'human' ? <ol className="mcp-setup-steps">
        <li><strong>在这里创建管理连接</strong><p>按客户端命名，例如“Codex”。通常保留默认的查看和日常管理，复制刚生成的密钥；维护与敏感读取按需授权。</p><Button variant="secondary" disabled={!canCreate} onClick={onCreate}>创建管理连接</Button><p className="mcp-secondary">已有密钥可直接使用。旧密钥不能再次查看；遗失时可在该连接的更多操作中轮换。</p></li>
        <li><strong>添加到 {setup?.name || '所选工具'}</strong>{setup ? <><p>{setup.destination}</p><p>{setup.action}</p><details className="mcp-config-block"><summary>查看{setup.format}</summary><p>把 <code>&lt;管理连接密钥&gt;</code> 换成刚复制的值；只添加 to-router 条目，保留其他设置。</p><pre tabIndex={0}>{setup.configuration}</pre><Button variant="secondary" onClick={() => void copy(setup!.configuration, `${setup!.name} 配置模板`)}><Copy size={15} />复制配置模板</Button></details><details><summary>已有连接或其他 MCP？</summary><p>{setup.existing}</p>{setup.updateConfiguration && <><pre tabIndex={0}>{setup.updateConfiguration}</pre><Button variant="secondary" onClick={() => void copy(setup!.updateConfiguration!, '已有连接的更新命令')}><Copy size={15} />复制更新命令</Button></>}</details></> : <p>读取到有效网关地址后显示对应配置。</p>}</li>
        <li><strong>重新加载连接</strong><p>{setup?.reconnect || '保存配置后，在客户端重新加载 MCP。'}</p><p className="mcp-secondary">本机地址只连接这台电脑。云端、远端或 WSL 中运行的 Agent，需要先确认能访问这里。</p></li>
        <li><strong>发一句话，确认真正连通</strong><blockquote>{mcpTestPrompt}</blockquote><Button variant="secondary" onClick={() => void copy(mcpTestPrompt, '验证用语')}><Copy size={15} />复制验证用语</Button><p>看到实际工具调用和网关返回的数据，才算连通。保存配置或显示“已启用”只代表已登记。</p></li>
      </ol> : <div className="mcp-agent-guide">
        <p>{client === 'general' ? '把这份说明交给 Agent，让它先识别你使用的工具和运行环境，检查是否支持 MCP；能接入时按实际要求配置并验证，不能接入时说明原因和下一步。' : '把配置说明复制到所选工具的聊天里，让 Agent 帮你找到配置文件、保留原设置并验证连接。'}</p>
        <Button disabled={!setup} onClick={() => void copy(agentText, 'Agent 配置说明')}><Copy size={16} />复制给 Agent</Button>
        <p className="mcp-secondary">这份说明不带密钥。创建连接后的领取窗口可复制含密钥版本；粘贴给哪个 Agent，就把该连接的管理权限交给它。</p>
        <details><summary>预览配置说明</summary><pre tabIndex={0}>{agentText || '正在读取网关地址…'}</pre></details>
      </div>}
      <p className="mcp-copy-status mcp-secondary" role="status">{copyMessage}</p>
      <details><summary>连接失败时怎么排查？</summary><ul className="mcp-help-list"><li>连接被拒绝／超时：先确认 to-router 正在运行，地址与本页相同。</li><li>401／无效凭证：使用这里创建的管理密钥，并确认连接已启用；轮换后要更新客户端。</li><li>配置已保存但没有工具：重载 MCP 或新建会话，查看客户端的具体连接错误。</li><li>提示缺少权限：只为所需操作调整这条连接的授权。</li></ul></details>
      {setup && <details><summary>其他连接方式：stdio</summary><p>HTTP 是上面的推荐接法。需要本地命令方式时，可使用随程序提供的 CLI 连接同一网关；它也要求网关已启动。</p><p className="mcp-secondary">此配置可能包含当前电脑的安装路径，请用于本机配置；分享说明时可使用上方“复制给 Agent”。</p><pre tabIndex={0}>{setup.stdioConfiguration}</pre><Button variant="secondary" onClick={() => void copy(setup!.stdioConfiguration, `${setup!.name} stdio 模板`)}><Copy size={15} />复制 stdio 模板</Button></details>}
      {setup?.source && <a className="mcp-source-link" href={setup.source} target="_blank" rel="noreferrer">{setup.name} 官方接入说明 <ExternalLink size={13} /></a>}
    </div>
  </details>;
}
