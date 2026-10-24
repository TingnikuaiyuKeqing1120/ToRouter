// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';
import { channelPresets, getPresetById } from './presets';
import { ProviderIcon } from './components/ProviderIcons';

describe('Channel Presets', () => {
  it('contains expected domestic and international presets', () => {
    const cnPresets = channelPresets.filter(p => p.category === 'cn');
    const intlPresets = channelPresets.filter(p => p.category === 'intl');

    expect(cnPresets.length).toBe(6);
    expect(intlPresets.length).toBe(8);
    expect(channelPresets.length).toBe(14);

    // Verify domestic vendors
    const cnNames = cnPresets.map(p => p.name);
    expect(cnNames).toContain('深度求索');
    expect(cnNames).toContain('月之暗面');
    expect(cnNames).toContain('智谱');
    expect(cnNames).toContain('稀宇科技');
    expect(cnNames).toContain('阶跃星辰');
    expect(cnNames).toContain('小米 MiMo');

    // Verify international vendors
    const intlNames = intlPresets.map(p => p.name);
    expect(intlNames).toContain('Moonshot AI');
    expect(intlNames).toContain('Z.ai');
    expect(intlNames).toContain('MiniMax');
    expect(intlNames).toContain('StepFun');
    expect(intlNames).toContain('OpenAI');
    expect(intlNames).toContain('Anthropic');
    expect(intlNames).toContain('OpenRouter');
    expect(intlNames).toContain('NVIDIA NIM');
  });

  it('ensures each preset has valid URLs and endpoints', () => {
    for (const preset of channelPresets) {
      expect(preset.id).toBeTruthy();
      expect(preset.name).toBeTruthy();
      expect(preset.base_url.startsWith('https://')).toBe(true);
      expect(preset.endpoints.length).toBeGreaterThan(0);
      for (const endpoint of preset.endpoints) {
        expect(['openai_chat_completions', 'anthropic_messages', 'gemini_content', 'openai_responses']).toContain(endpoint.format);
        if (endpoint.base_url) {
          expect(endpoint.base_url.startsWith('https://')).toBe(true);
        }
      }
    }
  });

  it('configures specialized /anthropic paths for MiniMax and Xiaomi MiMo', () => {
    const minimaxCn = getPresetById('minimax-cn');
    expect(minimaxCn).toBeDefined();
    const mmCnAnthropic = minimaxCn!.endpoints.find(e => e.format === 'anthropic_messages');
    expect(mmCnAnthropic?.base_url).toBe('https://api.minimax.cn/anthropic');

    const minimaxIntl = getPresetById('minimax-intl');
    expect(minimaxIntl).toBeDefined();
    const mmIntlAnthropic = minimaxIntl!.endpoints.find(e => e.format === 'anthropic_messages');
    expect(mmIntlAnthropic?.base_url).toBe('https://api.minimax.io/anthropic');

    const xiaomi = getPresetById('xiaomi-mimo');
    expect(xiaomi).toBeDefined();
    const xiaomiAnthropic = xiaomi!.endpoints.find(e => e.format === 'anthropic_messages');
    expect(xiaomiAnthropic?.base_url).toBe('https://api.xiaomimimo.com/anthropic');
  });

  it('retrieves preset by ID correctly', () => {
    expect(getPresetById('deepseek')?.name).toBe('深度求索');
    expect(getPresetById('non-existent')).toBeUndefined();
  });
});

describe('ProviderIcon Component', () => {
  it('renders brand icons based on name matching', () => {
    const testCases = [
      { name: '深度求索生产渠道', expectedFill: '#4D6BFE' },
      { name: 'Moonshot 测试', expectedFill: 'currentColor' },
      { name: '智谱 GLM-4', expectedFill: '#3859FF' },
      { name: '小米 MiMo 渠道', expectedFill: '#FF6900' },
      { name: 'Anthropic Claude', expectedFill: '#D97757' },
      { name: 'NVIDIA NIM', expectedFill: '#74B71B' },
      { name: 'OpenRouter 集成', expectedFill: '#C8FF00' },
    ];

    for (const tc of testCases) {
      const { container } = render(<ProviderIcon name={tc.name} size={24} />);
      const svg = container.querySelector('svg');
      expect(svg).toBeTruthy();
      expect(svg?.getAttribute('width')).toBe('24');
    }
  });

  it('resolves icon via base_url when name is generic', () => {
    const { container: dsContainer } = render(<ProviderIcon name="我的主力渠道" baseUrl="https://api.deepseek.com" size={20} />);
    expect(dsContainer.querySelector('path[fill="#4D6BFE"]')).toBeTruthy();

    const { container: xmContainer } = render(<ProviderIcon name="自用模型" baseUrl="https://api.xiaomimimo.com/v1" size={20} />);
    expect(xmContainer.querySelector('path[fill="#FF6900"]')).toBeTruthy();
  });

  it('renders fallback icon when neither name nor baseUrl matches', () => {
    const { container } = render(<ProviderIcon name="Custom Gateway" baseUrl="https://myproxy.local" size={20} />);
    const svg = container.querySelector('svg');
    expect(svg).toBeTruthy();
    expect(svg?.getAttribute('fill')).toBe('none');
    expect(svg?.getAttribute('stroke')).toBe('currentColor');
  });
});
