import presetData from '../../internal/catalog/channel-presets.json';
import type { Endpoint, EndpointFormat } from './api';

export interface ChannelPreset {
  id: string;
  name: string;
  label: string;
  category: 'cn' | 'intl';
  base_url: string;
  endpoints: Endpoint[];
  preferred_endpoint?: EndpointFormat;
  hint?: string;
}

export const channelPresets: ChannelPreset[] = presetData as ChannelPreset[];

export function getPresetById(id: string): ChannelPreset | undefined {
  return channelPresets.find(p => p.id === id);
}
