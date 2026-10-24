import type { PluginList } from '../types/plugins';

// Existing OAuth scenarios explicitly exercise already enabled integrations.
// This is a synthetic fixture, not the production new-install defaults.
export const enabledPlugins: PluginList = {
  version: 'fixture-v1',
  items: [
    ['codex', 'Codex', 'codex_oauth'],
    ['chatgpt', 'ChatGPT', 'chatgpt_oauth'],
    ['copilot', 'Copilot', 'copilot_oauth'],
    ['claude', 'Claude 订阅', 'claude_oauth'],
    ['antigravity', 'Antigravity', 'antigravity_oauth'],
    ['opencode-zen-free', 'OpenCode Zen Free', 'zen_free'],
    ['devin', 'Devin', 'devin_oauth'],
  ].map(([id, name, credential_type]) => ({
    id, name, credential_type, description: 'Synthetic connection fixture', icon: id,
    implementation: 'native', featured: true, version: '1',
    capabilities: [{ id: 'connect', enabled: true, available: true, affected_channel_ids: [] }],
  })),
};
