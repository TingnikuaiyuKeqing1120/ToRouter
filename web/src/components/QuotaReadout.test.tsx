// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { QuotaReadout } from './QuotaReadout';
import { ChannelQuota } from './ChannelQuota';
import { CompanionQuotaDock } from './CompanionQuotaDock';
import type { QuotaSnapshot } from '../types/quota';
const json = (body: unknown) => new Response(JSON.stringify(body));
const item: QuotaSnapshot = { channel_id: 5, credential_id: 'fixture-primary', channel_name: '示例渠道', credential_name: '工作凭证', kind: 'balance', enabled: true, balances: [{ currency: 'CNY', total: '0.00' }, { currency: 'USD', total: '1.25' }], windows: [], checked_at: '2026-10-19T00:00:00Z', stale: false };
beforeEach(() => { vi.spyOn(document, 'hidden', 'get').mockReturnValue(false); Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' }); });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
it('keeps original currencies and zero distinct from unknown, marks retained expired or failed data', () => {
  const view = render(<QuotaReadout item={item} />);
  expect(screen.getByText('0.00')).toBeTruthy(); expect(screen.getByText('1.25')).toBeTruthy();
  expect(screen.getByText('CNY')).toBeTruthy(); expect(screen.getByText('USD')).toBeTruthy();
  view.rerender(<QuotaReadout item={{ ...item, balances: [], stale: true }} />);
  expect(screen.getByText('余额 —')).toBeTruthy(); expect(screen.getByText('已过期')).toBeTruthy();
  view.rerender(<QuotaReadout item={{ ...item, stale: true, error: '合成查询失败' }} />);
  expect(screen.getByText('更新失败')).toBeTruthy(); expect(screen.getByText('0.00')).toBeTruthy();
});
it('shows Codex unknown percentages and window reset time without inventing a zero', () => {
  render(<QuotaReadout item={{ ...item, kind: 'windows', balances: [], windows: [{ id: 'primary', label: '5h', remaining_percent: null, resets_at: '2026-10-20T00:00:00Z' }, { id: 'secondary', label: '7d', remaining_percent: 0 }] }} />);
  expect(screen.getByText('—')).toBeTruthy(); expect(screen.getByText('0%')).toBeTruthy(); expect(screen.getAllByText(/重置/)).toHaveLength(2);
});
it('keeps multiple credential balances collapsed and separate by neutral names without key or account identity', async () => {
  const second = { ...item, credential_id: 'fixture-secondary', credential_name: '备用凭证', balances: [{ currency: 'CNY', total: '9.00' }] };
  const fetcher = vi.fn(() => Promise.resolve(json({ items: [item, second], refresh_seconds: 0 })));
  vi.stubGlobal('fetch', fetcher);
  render(<ChannelQuota items={[item, second]} />);
  expect(screen.queryByText('9.00')).toBeNull(); expect(fetcher).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: /凭证余额/ }));
  await screen.findByText('9.00'); expect(screen.getByText('工作凭证')).toBeTruthy(); expect(screen.getByText('备用凭证')).toBeTruthy();
  expect(document.body.textContent).not.toContain('fixture-primary'); expect(document.body.textContent).not.toContain('fixture-secondary');
  expect(document.body.textContent).not.toContain('9.00 +');
});
it('shows three followed rows then expands and opens only the exact target without navigating the pet', async () => {
  const items = Array.from({ length: 4 }, (_, index) => ({ ...item, credential_id: `fixture-${index}`, credential_name: `凭证${index}` }));
  const fetcher = vi.fn((url: string) => Promise.resolve(json(url.endsWith('/quotas') ? { items, refresh_seconds: 0 } : { requested: true })));
  vi.stubGlobal('fetch', fetcher); const before = window.location.hash;
  render(<CompanionQuotaDock targets={items} />);
  await screen.findByRole('button', { name: '查看渠道 示例渠道 凭证0' });
  expect(screen.queryByRole('button', { name: '查看渠道 示例渠道 凭证3' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: /展开更多/ }));
  fireEvent.click(await screen.findByRole('button', { name: '查看渠道 示例渠道 凭证3' }));
  await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url.endsWith('/desktop/open-channel'))).toBe(true));
  const write = fetcher.mock.calls.find(([url]) => url.endsWith('/desktop/open-channel'))! as unknown as [string, RequestInit];
  expect(JSON.parse(String(write[1].body))).toEqual({ channel_id: 5, credential_id: 'fixture-3' });
  expect(window.location.hash).toBe(before);
  await act(async () => {});
});
