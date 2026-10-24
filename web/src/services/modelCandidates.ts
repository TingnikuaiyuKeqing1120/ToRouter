import type { Channel } from '../api';

// Configuration candidates follow model selection, independently of channel pause.
// Discovery inventory belongs to channel management, not new model selections.
export function enabledChannelModels(channel: Pick<Channel, 'models'>): string[] {
  return [...new Set(channel.models)];
}
