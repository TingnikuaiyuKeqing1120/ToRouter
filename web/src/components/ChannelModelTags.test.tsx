// @vitest-environment jsdom
import { useState } from 'react';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import type { Channel, TagGroupWithTags } from '../api';
import { ChannelModelTags } from './ChannelModelTags';

const groups = ['billing', 'privacy', 'team'].map((id, index) => ({ id, name: ['计费', '隐私', '团队'][index], type: index === 2 ? 'user' : 'builtin', tags: ['unknown', 'a', 'b'].map(value => ({ id: `${id}:${value}`, group_id: id, name: `${id}-${value}`, color: 'slate' })) })) as TagGroupWithTags[];
const channel = { id: 1, name: '测试', models: ['alpha', 'beta'], available_models: ['alpha', 'beta', 'disabled'], tags: ['billing:a', 'privacy:a'], model_tags: { disabled: ['billing:b'] } } as unknown as Channel;
function mount() {
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(JSON.stringify(groups)))));
  function Wrapper() { const [draft, setDraft] = useState(channel); return <><ChannelModelTags channel={draft} onChange={setDraft} /><output data-testid="draft">{JSON.stringify(draft)}</output></>; }
  render(<Wrapper />);
}
const draft = () => JSON.parse(screen.getByTestId('draft').textContent!) as Channel;
async function openModel(id: string) {
  const summary = await screen.findByText(id, { selector: 'summary > span:first-child' });
  const details = summary.closest('details')!;
  details.open = true; fireEvent(details, new Event('toggle'));
  return details;
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('uses per-group inheritance, explicit unknown and restores the latest channel default', async () => {
  mount(); const model = await openModel('alpha');
  fireEvent.click(within(model).getByRole('button', { name: 'privacy-unknown' }));
  expect(draft().model_tags?.alpha).toEqual(['privacy:unknown']);
  expect(within(model).getByText('跟随渠道 · billing-a')).toBeTruthy();
  const defaults = screen.getByRole('heading', { name: '渠道默认标签' }).closest('section')!;
  fireEvent.click(within(defaults).getByRole('button', { name: 'privacy-b' }));
  expect(draft().model_tags?.alpha).toEqual(['privacy:unknown']);
  fireEvent.click(within(model).getByRole('button', { name: '恢复跟随' }));
  expect(draft().model_tags?.alpha).toBeUndefined();
  expect(within(model).getByText('跟随渠道 · privacy-b')).toBeTruthy();
  expect(draft().model_tags?.disabled).toEqual(['billing:b']);
});

it('applies bulk changes only to visible selected enabled models and keeps disabled overrides editable', async () => {
  mount(); await screen.findByRole('heading', { name: '模型标签' });
  fireEvent.click(screen.getByRole('checkbox', { name: '选择可见已启用模型标签' }));
  fireEvent.change(screen.getByRole('textbox', { name: '搜索模型标签' }), { target: { value: 'alpha' } });
  fireEvent.click(screen.getByRole('button', { name: '应用到这 1 个模型' }));
  expect(draft().model_tags?.alpha).toEqual(['billing:unknown']);
  expect(draft().model_tags?.beta).toBeUndefined();
  expect(draft().model_tags?.disabled).toEqual(['billing:b']);
  fireEvent.change(screen.getByRole('textbox', { name: '搜索模型标签' }), { target: { value: '' } });
  fireEvent.click(screen.getByRole('checkbox', { name: '显示停用模型' }));
  const model = await openModel('disabled（已停用）');
  fireEvent.click(within(model).getByRole('button', { name: 'billing-a' }));
  expect(draft().model_tags?.disabled).toEqual(['billing:a']);
});
