export type PluginCapabilityID = 'connect' | 'discover_api_key';

export interface PluginCapability {
  id: PluginCapabilityID;
  enabled: boolean;
  available: boolean;
  unavailable_reason?: string;
  affected_channel_ids: number[];
}

export interface PluginInfo {
  id: string;
  name: string;
  description: string;
  icon: string;
  implementation: 'native' | 'compatible';
  featured: boolean;
  version: string;
  credential_type?: string;
  package?: string;
  community?: boolean;
  auth_methods?: MarketAuthMethod[];
  capabilities: PluginCapability[];
}

export interface PluginAccess {
  allowed: boolean;
  enabled?: boolean;
  icon?: string;
  pluginID?: string;
  message?: string;
  authMethods?: MarketAuthMethod[];
}

export interface MarketPrompt {
  type: string;
  key: string;
  label: string;
  placeholder?: string;
  options?: { label: string; value: string; hint?: string }[];
  condition?: { key: string; op: string; value: string };
}

export interface MarketAuthMethod {
  index: number;
  type: 'api' | 'oauth';
  label: string;
  prompts?: MarketPrompt[];
}

export interface MarketProvider {
  id: string;
  name: string;
  credential_type: `market:${string}`;
  api: string;
  npm: string;
  methods: MarketAuthMethod[];
}

export interface MarketItem {
  id: string;
  package: string;
  name: string;
  description: string;
  icon: string;
  community: boolean;
  version?: string;
  installed_version?: string;
  installed: boolean;
  update_available: boolean;
  providers: string[];
  installed_providers: MarketProvider[];
}

export interface MarketCatalog {
  items: MarketItem[];
  refreshing: boolean;
  warning?: string;
}

export interface MarketAuthorizationStatus {
  flow_id: string;
  status: 'pending' | 'success' | 'error' | 'cancelled';
  url?: string;
  instructions?: string;
  method?: 'auto' | 'code';
  channel_id?: number;
  error?: string;
}

export interface PluginList {
  items: PluginInfo[];
  version: string;
  compatible?: { installed: boolean; version?: string; reason?: string };
}
