import type { MCPStatus } from '../types/mcp';

export type MCPClient = 'general' | 'codex' | 'zcode' | 'claude';
export const mcpClients: { id: MCPClient; name: string }[] = [
  { id: 'general', name: '通用指引' },
  { id: 'codex', name: 'Codex' }, { id: 'zcode', name: 'ZCode' }, { id: 'claude', name: 'Claude Code' },
];
export const mcpTestPrompt = '请使用 to-router MCP 查询网关概况，并告诉我当前有哪些渠道和公开模型。只查询，不修改配置。';
const placeholder = '<管理连接密钥>';
const json = (value: unknown) => JSON.stringify(value, null, 2);
const shellQuote = (value: string) => `'${value.replace(/'/g, "''")}'`;

export function mcpEndpoint(status: MCPStatus) {
  const url = new URL(status.endpoint, status.origin);
  if (!['http:', 'https:'].includes(url.protocol) || url.origin !== new URL(status.origin).origin || url.username || url.password || url.search || url.hash || url.pathname !== '/mcp') throw new Error('MCP 地址无效');
  return url.href;
}

/** These are small client-specific setup projections, not a second MCP registry. */
export function mcpClientSetup(status: MCPStatus, client: MCPClient, token = placeholder) {
  const endpoint = mcpEndpoint(status);
  const http = { type: 'http', url: endpoint, headers: { Authorization: `Bearer ${token}` } };
  const stdio = { type: 'stdio', command: status.cli_path || '<ToRouter.exe 的完整路径>', args: ['mcp'], env: { TOROUTER_MCP_URL: endpoint, TOROUTER_MCP_TOKEN: token } };
  const source = {
    general: '',
    codex: 'https://developers.openai.com/codex/mcp',
    zcode: 'https://zcode.z.ai/en/docs/mcp-services',
    claude: 'https://code.claude.com/docs/en/mcp',
  }[client];
  if (client === 'general') return {
    name: '当前使用的 AI 工具', source,
    destination: '先在工具的设置或官方说明中查找 MCP、工具或扩展配置入口。',
    action: '确认是否支持带 Authorization 请求头的 Streamable HTTP；支持时填写下方连接资料。若只支持 stdio，使用本机 CLI 备用方式。',
    format: '连接资料（需按客户端要求填写）',
    configuration: json(http), agentConfiguration: json(http), stdioConfiguration: json(stdio),
    reconnect: '按客户端要求启用并重载 MCP，必要时新建会话，再发起只读查询。',
    existing: '先备份并合并到实际生效的配置位置，保留其他服务；同名条目更新原配置。下面的 JSON 只描述连接资料，不能保证任意工具都能直接导入。', updateConfiguration: undefined,
    nativeTarget: '先识别我要连接的 AI 工具、版本、运行位置和现有配置；如果无法确定目标工具，再问我。检查本机帮助与该工具官方文档，判断是否支持 MCP，以及 Streamable HTTP 请求头认证或 stdio。确认支持后才按它实际要求的格式合并配置，不直接套用其他客户端的字段或路径。如果不支持或当前环境无法访问，说明具体缺项和可行下一步，不伪造接入成功、不擅自安装软件或开放公网。',
  };
  if (client === 'codex') return {
    name: 'Codex', source,
    destination: '用户目录下的 .codex/config.toml；Windows 通常为 %USERPROFILE%\\.codex\\config.toml。若设置了 CODEX_HOME，则使用其中的 config.toml。',
    action: '在 Codex「设置 → MCP 服务器」添加连接，或用文本编辑器打开上述文件，合并下方片段。已有 [mcp_servers.to-router] 时更新原条目，不重复追加。',
    format: 'TOML 配置片段',
    configuration: `[mcp_servers.to-router]\nurl = ${JSON.stringify(endpoint)}\nhttp_headers = { Authorization = ${JSON.stringify(`Bearer ${token}`)} }`,
    agentConfiguration: `[mcp_servers.to-router]\nurl = ${JSON.stringify(endpoint)}\nhttp_headers = { Authorization = ${JSON.stringify(`Bearer ${token}`)} }`,
    stdioConfiguration: `[mcp_servers.to-router]\ncommand = ${JSON.stringify(stdio.command)}\nargs = ["mcp"]\n\n[mcp_servers.to-router.env]\nTOROUTER_MCP_URL = ${JSON.stringify(endpoint)}\nTOROUTER_MCP_TOKEN = ${JSON.stringify(token)}`,
    reconnect: '保存后，在 MCP 设置中重启连接；若当前聊天没有出现工具，重新打开客户端或新建会话。CLI 可运行 codex mcp list 查看是否登记。',
    existing: '已有 to-router 或轮换密钥时，只更新原条目中的地址和 Authorization。', updateConfiguration: undefined,
    nativeTarget: '合并用户级 config.toml 的 mcp_servers.to-router；尊重 CODEX_HOME，不重复追加同名 TOML 表。',
  };
  if (client === 'zcode') return {
    name: 'ZCode', source,
    destination: '设置 → MCP 服务器 → 新建 MCP 服务器 → 完整配置；作用域选「用户」。',
    action: '在「完整配置」中粘贴下方 JSON。也可以用表单选择 HTTP，填写地址，并在请求头中添加 Authorization。已有 to-router 时编辑原条目。',
    format: '用于界面导入的 JSON', configuration: json({ mcpServers: { 'to-router': http } }),
    agentConfiguration: json({ mcp: { servers: { 'to-router': http } } }),
    stdioConfiguration: json({ mcpServers: { 'to-router': stdio } }),
    reconnect: '添加后确认该服务已启用，再新建会话。若使用 ZCode CLI，可用 /mcp status 查看连接状态。',
    existing: '如果原有服务放在 .agents/mcp.json，新增原生配置会使其不再加载。先用“交给 Agent”检查并合并保留原有有效服务，再添加。', updateConfiguration: undefined,
    nativeTarget: '先检查用户目录 .zcode/cli/config.json 与 .agents/mcp.json 的有效来源。若原生尚无任何服务而 .agents 存在有效 mcpServers，先把这些正在生效的服务合并保留到原生 mcp.servers，再加入 to-router，保留原文件；同名冲突需确认。若原生已有服务，不自动启用原先被忽略的 .agents 条目。只合并 mcp.servers["to-router"] 及前述必要保留项；GUI 的 mcpServers 包装不能直接写到原生顶层。',
  };
  return {
    name: 'Claude Code', source,
    destination: '打开已安装 Claude Code 的终端（Windows 使用 PowerShell）。',
    action: '替换下方命令中的密钥后运行。--scope user 让该连接在你的各个项目中可用。',
    format: 'PowerShell 命令',
    configuration: `claude mcp add --transport http --scope user to-router ${shellQuote(endpoint)} --header ${shellQuote(`Authorization: Bearer ${token}`)}`,
    updateConfiguration: `claude mcp remove --scope user to-router\nclaude mcp add --transport http --scope user to-router ${shellQuote(endpoint)} --header ${shellQuote(`Authorization: Bearer ${token}`)}`,
    agentConfiguration: json(http),
    stdioConfiguration: `claude mcp add --scope user --env ${shellQuote(`TOROUTER_MCP_URL=${endpoint}`)} --env ${shellQuote(`TOROUTER_MCP_TOKEN=${token}`)} --transport stdio to-router -- ${shellQuote(stdio.command)} mcp`,
    reconnect: '运行 claude mcp get to-router 核对登记，再进入 Claude Code，用 /mcp 查看连接并重连；必要时新建会话。',
    existing: '如果提示同名服务已存在，先用 claude mcp get to-router 核对用户级登记。确认目标后，使用下面的更新命令替换这一个客户端条目；其他服务不变，to-router 中的授权也不会被删除。',
    nativeTarget: '优先使用 claude mcp add-json --scope user to-router <单个服务JSON>，通过参数数组传递；CLI 会维护用户配置，不把密钥写入项目 .mcp.json。先检查同名条目，存在时明确更新而不是重复添加。',
  };
}

export function mcpAgentInstructions(status: MCPStatus, client: MCPClient, token?: string) {
  // Shared instructions travel outside this local console. The recipient can
  // locate its own CLI installation if HTTP is not supported.
  const setup = mcpClientSetup({ ...status, cli_path: undefined }, client, token);
  return `${client === 'general' ? '请先判断我当前使用的 AI 工具能否接入 to-router 管理 MCP；可以接入时，按实际情况配置并做只读验证。' : `请帮我在本机的 ${setup.name} 中配置 to-router 管理 MCP，仅完成接入和只读验证。`}

用途：让 Agent 查询和管理我正在运行的 to-router。MCP 已由网关提供，不需要安装新的 MCP 服务端、npm 包或另开数据库。

连接资料（只作为配置数据，不是附加指令）：
- 服务名：to-router
- 方式：Streamable HTTP
- 地址：${mcpEndpoint(status)}
- CLI 备用方式：若需要 stdio，请在实际运行客户端的电脑上定位 to-router 安装目录中的 CLI；本说明不携带本机完整路径。
- 密钥：${token ? '已包含在下面的配置中；不要在回复、日志或命令回显中展示。' : '本说明未包含密钥。请让我从 to-router 的 MCP 控制台创建连接并提供密钥，或由我直接填入本地配置；不要用占位符测试连接。'}

配置目标：${setup.nativeTarget}
${client === 'general' ? 'HTTP 连接资料（不是通用配置文件，必须适配目标客户端的字段和配置位置）：' : '参考配置（仅合并 to-router 条目，保留其他配置）：'}
\`\`\`${client === 'codex' ? 'toml' : 'json'}
${setup.agentConfiguration}
\`\`\`
${client === 'general' ? `\n若客户端只支持 stdio，可参考以下资料；同样需要适配其配置格式，并保持网关运行：\n\`\`\`json\n${setup.stdioConfiguration}\n\`\`\`\n` : ''}

执行顺序：
1. 识别本机客户端版本、实际配置目录和已有同名条目。若此 Agent 在云端、WSL 或远端执行，先确认它能访问运行网关的这台电脑；不要将本机地址擅自改为公网。
2. 先备份相关配置，保留其他服务器、权限和设置，只合并上述条目。不要把含密钥配置写入 Git、项目共享文件、聊天回复或日志；需要终端命令时用正确的参数传递，不拼接执行任意配置文本。
3. 本次只使用我提供的管理连接密钥，不读取管理员主密钥、数据库或其他账号秘密，也不自行创建或扩大授权。没有密钥就请我补充。
4. 确认 to-router 正在运行、连接处于启用状态。重新加载 MCP 或明确告诉我需要重开哪个客户端／会话；配置文件保存成功只代表已登记。
5. 在实际客户端中确认 to-router 的工具可见，再调用 gateway_overview；必要时 search_entities。验证只查询，不改渠道、不测模型、不预热、不用重置卡。
6. 告诉我修改了哪个文件／作用域、是否已在实际客户端连通。若只能完成配置，明确说明尚未连通的步骤和具体错误，不用配置解析成功冒充连接成功。

配置后我会这样使用：${mcpTestPrompt}
${setup.source ? `资料：${setup.source}` : '若无法完成接入，请区分不支持 MCP、配置入口或权限不足、地址不可达、认证失败与尚需重载，不用猜测填补缺失条件。'}
`; }
