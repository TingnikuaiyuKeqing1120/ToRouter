import { api } from './client';
import type { MCPConnection, MCPConnectionSecret, MCPPermission, MCPStatus, MCPTool } from '../types/mcp';

const connectionPath = (id: number) => `/mcp/connections/${encodeURIComponent(String(id))}`;
export const mcp = {
  status: (signal?: AbortSignal) => api<MCPStatus>('/mcp/status', { signal }),
  tools: (signal?: AbortSignal) => api<MCPTool[]>('/mcp/tools', { signal }),
  connections: (signal?: AbortSignal) => api<MCPConnection[]>('/mcp/connections', { signal }),
  create: (value: { name: string; permissions: MCPPermission[] }) => api<MCPConnectionSecret>('/mcp/connections', { method: 'POST', body: JSON.stringify(value) }),
  update: (id: number, value: { name?: string; enabled?: boolean; permissions?: MCPPermission[] }) => api<MCPConnection>(connectionPath(id), { method: 'PATCH', body: JSON.stringify(value) }),
  revoke: (id: number) => api<void>(connectionPath(id), { method: 'DELETE' }),
  rotate: (id: number) => api<MCPConnectionSecret>(`${connectionPath(id)}/rotate`, { method: 'POST' }),
};
