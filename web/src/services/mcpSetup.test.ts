import { describe, expect, it } from 'vitest';
import { mcpAgentInstructions, mcpClientSetup, mcpEndpoint } from './mcpSetup';
import type { MCPStatus } from '../types/mcp';

const status: MCPStatus = { origin: 'http://127.0.0.1:49123', endpoint: '/mcp', cli_path: "C:\\Apps with spaces\\to-router\\ToRouter.exe", enabled: true, tool_count: 103, capability_count: 103 };

describe('MCP setup formats', () => {
  it('keeps GUI import and native ZCode config wrappers distinct', () => {
    const v = mcpClientSetup(status, 'zcode', 'synthetic-key');
    const gui = JSON.parse(v.configuration), native = JSON.parse(v.agentConfiguration);
    expect(gui.mcpServers['to-router'].type).toBe('http');
    expect(native.mcp.servers['to-router']).toEqual(gui.mcpServers['to-router']);
    expect(native.mcpServers).toBeUndefined();
    expect(v.nativeTarget).toContain('.agents/mcp.json');
    expect(v.nativeTarget).toContain('不自动启用原先被忽略');
    expect(JSON.parse(v.stdioConfiguration).mcpServers['to-router'].command).toBe(status.cli_path);
  });
  it('makes Codex TOML and Claude PowerShell quoting explicit', () => {
    const token = 'synthetic"quote\\slash';
    const codex = mcpClientSetup(status, 'codex', token);
    expect(codex.configuration).toContain(`[mcp_servers.to-router]\nurl = "${status.origin}/mcp"`);
    expect(codex.configuration).toContain(JSON.stringify(`Bearer ${token}`));
    expect(codex.stdioConfiguration).toContain(JSON.stringify(status.cli_path));
    const claude = mcpClientSetup(status, 'claude', "synthetic'quote");
    expect(claude.configuration).toContain('--transport http --scope user');
    expect(claude.configuration).toContain("'Authorization: Bearer synthetic''quote'");
    expect(JSON.parse(claude.agentConfiguration).type).toBe('http');
    expect(claude.updateConfiguration).toMatch(/^claude mcp remove --scope user to-router\nclaude mcp add/);
    expect(claude.stdioConfiguration).toContain("--transport stdio to-router -- 'C:\\Apps with spaces\\to-router\\ToRouter.exe' mcp");
  });
  it('keeps ordinary instructions token-free and requires real client verification', () => {
    const v = mcpAgentInstructions(status, 'claude');
    expect(v).toContain('<管理连接密钥>');
    expect(v).toContain('不要用占位符测试连接');
    expect(v).toContain('保留其他服务器');
    expect(v).toContain('只查询，不改渠道');
    expect(v).toContain('不用配置解析成功冒充连接成功');
  });
  it('keeps local installation paths out of shared Agent instructions for every client', () => {
    for (const client of ['general', 'codex', 'zcode', 'claude'] as const) {
      for (const token of [undefined, 'synthetic-explicit-token']) {
        const instructions = mcpAgentInstructions(status, client, token);
        expect(instructions).not.toContain(status.cli_path!);
        expect(instructions).not.toContain(JSON.stringify(status.cli_path));
        expect(instructions).not.toContain('Apps with spaces');
        expect(instructions).toContain(`${status.origin}/mcp`);
        if (token) expect(instructions).toContain(token);
      }
    }
    expect(mcpClientSetup(status, 'codex').stdioConfiguration).toContain(JSON.stringify(status.cli_path));
  });
  it('rejects query credentials and changed destinations, and makes a missing CLI path an explicit placeholder', () => {
    expect(mcpEndpoint(status)).toBe(`${status.origin}/mcp`);
    for (const endpoint of ['https://other.invalid/mcp', '/mcp?key=secret', '/api', '/mcp#fragment', 'http://user:pass@127.0.0.1:49123/mcp']) {
      expect(() => mcpEndpoint({ ...status, endpoint })).toThrow();
    }
    expect(mcpClientSetup({ ...status, cli_path: undefined }, 'codex').stdioConfiguration).toContain('<ToRouter.exe 的完整路径>');
  });
});
