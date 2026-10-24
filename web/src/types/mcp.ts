export type MCPPermission = 'read' | 'manage' | 'maintain' | 'sensitive';

export interface MCPTool {
  name: string;
  description: string;
  capability: string;
  permission: MCPPermission;
  read_only: boolean;
}

export interface MCPConnection {
  id: number;
  name: string;
  enabled: boolean;
  permissions: MCPPermission[];
  created_at: string;
  last_used_at?: string | null;
}

export interface MCPStatus {
  origin: string;
  endpoint: string;
  enabled: boolean;
  tool_count: number;
  capability_count: number;
  cli_path?: string;
}

export interface MCPConnectionSecret {
  connection: MCPConnection;
  token: string;
}
