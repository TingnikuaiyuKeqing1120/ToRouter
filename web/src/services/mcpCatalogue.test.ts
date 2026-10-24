import { expect, it } from 'vitest';
import reference from '../../../docs/generated/mcp-tools.json';
import type { MCPTool } from '../types/mcp';
import { mcpCoreTools, mcpToolCategory, mcpToolMatches, mcpToolTitle, mcpToolTitles } from './mcpCatalogue';

it('keeps readable labels and categories aligned with the generated MCP registry', () => {
  const tools = reference.tools as MCPTool[];
  expect(tools.length).toBeGreaterThan(0);
  expect(tools.filter(tool => !mcpToolTitles[tool.name]).map(tool => tool.name)).toEqual([]);
  expect(tools.filter(tool => mcpToolCategory(tool).id === 'other').map(tool => tool.name)).toEqual([]);
  expect(mcpCoreTools.filter(name => !tools.some(tool => tool.name === name))).toEqual([]);
  expect(Object.keys(mcpToolTitles).filter(name => !tools.some(tool => tool.name === name))).toEqual([]);
});

it('assigns automation to its specific category and supports both task words and exact tool names', () => {
  const tool = reference.tools.find(tool => tool.name === 'batch_configure_credential_automation') as MCPTool;
  expect(mcpToolCategory(tool).id).toBe('automation');
  expect(mcpToolMatches(tool, '批量 自动')).toBe(true);
  expect(mcpToolMatches(tool, 'BATCH_CONFIGURE')).toBe(true);
  expect(mcpToolMatches(tool, '价格')).toBe(false);
});

it('still exposes an unknown future operation instead of dropping it from the toolbox', () => {
  const tool: MCPTool = { name: 'future_tool', capability: 'future.task', description: '新的未来功能', permission: 'read', read_only: true };
  expect(mcpToolCategory(tool).id).toBe('other');
  expect(mcpToolTitle(tool)).toBe('future_tool');
  expect(mcpToolMatches(tool, '未来')).toBe(true);
});
