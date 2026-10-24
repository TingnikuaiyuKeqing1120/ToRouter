// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import Logs from './Logs';
import type { UsageLog } from '../types';

const response = (value: unknown) =>
  new Response(JSON.stringify(value), { headers: { 'Content-Type': 'application/json' } });

beforeEach(() => {
  sessionStorage.clear();
  localStorage.clear();
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', {
    configurable: true,
    value: function (this: HTMLDialogElement) {
      this.setAttribute('open', '');
    },
  });
  Object.defineProperty(HTMLDialogElement.prototype, 'close', {
    configurable: true,
    value: function (this: HTMLDialogElement) {
      this.removeAttribute('open');
    },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const baseLog: UsageLog = {
  id: 101,
  time: '2026-10-09T08:00:00Z',
  channel_id: 1,
  channel_name: 'Main-Channel',
  model: 'gpt-4o',
  upstream_model: 'gpt-4o',
  inbound: 'openai_chat_completions',
  outbound: 'openai_chat_completions',
  input_tokens: 100,
  output_tokens: 200,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  estimated: false,
  duration_ms: 1250,
  ttft_ms: 200,
  status: 200,
  error: '',
};

describe('Logs - Request Attempt & Switch Logs (T046)', () => {
  it('shows plan usage without a false per-token price and retains independent API value', async () => {
    const log = { ...baseLog, actual_cost: null, official_value: 0.5, official_currency: 'USD', actual_pricing: { method: 'request', calculated_at: baseLog.time, resolution: { status: 'resolved', layer: 'plan', channel_id: 1, model: baseLog.model }, rates: { input: null, output: null, cache_read: null, cache_write: null }, currency: '', unit_tokens: 0, matched_rules: [], missing_reason: 'plan_unallocated' } };
    vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(response(url.startsWith('/api/logs') ? [log] : []))));
    render(<Logs />);
    expect(await screen.findByText('套餐内')).toBeTruthy();
    expect(screen.getByText('$0.50 USD')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '查看请求 101 详情' }));
    expect(screen.getByText('实际支出 · 套餐内')).toBeTruthy();
    expect(screen.getByRole('link', { name: '查看套餐价格 ↗', hidden: true }).getAttribute('href')).toBe('#/prices?view=plan&channel_id=1');
  });
  it('renders attempt count badge in table row for multi-attempt logs, with amber for success and red for failure', async () => {
    const logs: UsageLog[] = [
      {
        ...baseLog,
        id: 1,
        status: 200,
        attempts: [
          {
            attempt_index: 1,
            channel_id: 1,
            channel_name: 'Key-1',
            upstream_model: 'gpt-4o',
            status_code: 429,
            duration_ms: 120,
            action_taken: 'cooling_switched',
          },
          {
            attempt_index: 2,
            channel_id: 1,
            channel_name: 'Key-2',
            upstream_model: 'gpt-4o',
            status_code: 200,
            duration_ms: 450,
            action_taken: 'success',
          },
        ],
      },
      {
        ...baseLog,
        id: 2,
        status: 502,
        error: 'All channels exhausted',
        attempts: [
          {
            attempt_index: 1,
            channel_id: 1,
            channel_name: 'Chan-1',
            upstream_model: 'gpt-4o',
            status_code: 500,
            duration_ms: 300,
            action_taken: 'exhausted_fallback',
          },
          {
            attempt_index: 2,
            channel_id: 2,
            channel_name: 'Chan-2',
            upstream_model: 'gpt-4o',
            status_code: 502,
            duration_ms: 310,
            action_taken: 'exhausted_fallback',
          },
          {
            attempt_index: 3,
            channel_id: 3,
            channel_name: 'Chan-3',
            upstream_model: 'gpt-4o',
            status_code: 502,
            duration_ms: 290,
          },
        ],
      },
      {
        ...baseLog,
        id: 3,
        status: 200,
        attempts: [
          {
            attempt_index: 1,
            channel_id: 1,
            channel_name: 'Single-Attempt',
            upstream_model: 'gpt-4o',
            status_code: 200,
            duration_ms: 200,
            action_taken: 'success',
          },
        ],
      },
      {
        ...baseLog,
        id: 4,
        status: 200,
        // no attempts array
      },
    ];

    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) => {
      if (url.startsWith('/api/logs?')) return Promise.resolve(response(logs));
      if (url.startsWith('/api/channels')) return Promise.resolve(response([]));
      return Promise.resolve(response([]));
    }));

    render(<Logs />);
    await screen.findByRole('button', { name: '查看请求 1 详情' });

    const rows = screen.getAllByRole('row');
    // Header is row 0. Data rows are 1..4
    expect(rows).toHaveLength(5);

    // Row 1 (id: 1, success with 2 attempts): badge "尝试 2 次" with amber tone
    const cell1Status = within(rows[1]).getAllByRole('cell')[6];
    const badge1 = within(cell1Status).getByText('尝试 2 次');
    expect(badge1.className).toContain('badge-amber');

    // Row 2 (id: 2, failure 502 with 3 attempts): badge "尝试 3 次" with red tone
    const cell2Status = within(rows[2]).getAllByRole('cell')[6];
    const badge2 = within(cell2Status).getByText('尝试 3 次');
    expect(badge2.className).toContain('badge-red');

    // Row 3 (id: 3, single attempt): no "尝试 1 次" badge
    const cell3Status = within(rows[3]).getAllByRole('cell')[6];
    expect(within(cell3Status).queryByText(/尝试/)).toBeNull();

    // Row 4 (id: 4, no attempts array): no "尝试" badge
    const cell4Status = within(rows[4]).getAllByRole('cell')[6];
    expect(within(cell4Status).queryByText(/尝试/)).toBeNull();
  });

  it('renders Attempt Trace in detail modal with chronological order, badges, actions, and error reasons', async () => {
    const multiAttemptLog: UsageLog = {
      ...baseLog,
      id: 88,
      status: 200,
      attempts: [
        // Intentionally provide out-of-order attempts to verify sorting
        {
          attempt_index: 2,
          channel_id: 1,
          channel_name: 'Key-Pool',
          credential_id: 'cred-2',
          credential_mask_key: 'sk-...9999',
          upstream_model: 'gpt-4o-2024-08-06',
          status_code: 200,
          duration_ms: 560,
          action_taken: 'success',
        },
        {
          attempt_index: 1,
          channel_id: 1,
          channel_name: 'Key-Pool',
          credential_id: 'cred-1',
          credential_mask_key: 'sk-...1111',
          upstream_model: 'gpt-4o',
          status_code: 429,
          duration_ms: 150,
          error_reason: '429: rate limited (Retry-After: 15s)',
          action_taken: 'cooling_switched',
        },
      ],
    };

    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) => {
      if (url.startsWith('/api/logs?')) return Promise.resolve(response([multiAttemptLog]));
      if (url.startsWith('/api/channels')) return Promise.resolve(response([]));
      return Promise.resolve(response([]));
    }));

    render(<Logs />);
    const detailBtn = await screen.findByRole('button', { name: '查看请求 88 详情' });
    fireEvent.click(detailBtn);

    // Modal dialog is open
    const traceSection = screen.getByRole('region', { name: '尝试与切换轨迹' });
    expect(traceSection).toBeTruthy();
    expect(within(traceSection).getByText(/共 2 次尝试/)).toBeTruthy();

    // Verify chronological order: #1 must appear before #2
    const seqBadges = within(traceSection).getAllByText(/^#[12]$/);
    expect(seqBadges.map(b => b.textContent)).toEqual(['#1', '#2']);

    // Attempt #1 checks
    expect(within(traceSection).getByText('sk-...1111', { exact: false })).toBeTruthy();
    expect(within(traceSection).getByText('429: rate limited (Retry-After: 15s)')).toBeTruthy();
    expect(within(traceSection).getByText('冷却切换')).toBeTruthy();
    expect(within(traceSection).getByText('150 ms')).toBeTruthy();

    // Attempt #1 status 429 badge tone
    const status429Badge = within(traceSection).getByText('429');
    expect(status429Badge.className).toContain('badge-amber');

    // Attempt #2 checks
    expect(within(traceSection).getByText('sk-...9999', { exact: false })).toBeTruthy();
    expect(within(traceSection).getByText('gpt-4o-2024-08-06')).toBeTruthy();
    expect(within(traceSection).getByText('成功')).toBeTruthy();
    expect(within(traceSection).getByText('560 ms')).toBeTruthy();

    // Attempt #2 status 200 badge tone
    const status200Badge = within(traceSection).getByText('200');
    expect(status200Badge.className).toContain('badge-teal');
  });

  it('correctly maps all action labels and status code tones in attempt trace', async () => {
    const actionTestingLog: UsageLog = {
      ...baseLog,
      id: 99,
      status: 500,
      attempts: [
        {
          attempt_index: 1,
          channel_id: 1,
          channel_name: 'Ch-Fallback',
          upstream_model: 'm1',
          status_code: 502,
          duration_ms: 100,
          action_taken: 'exhausted_fallback',
          error_reason: '502 bad gateway',
        },
        {
          attempt_index: 2,
          channel_id: 2,
          channel_name: 'Ch-Retry',
          upstream_model: 'm1',
          status_code: 503,
          duration_ms: 200,
          action_taken: 'retry',
        },
        {
          attempt_index: 3,
          channel_id: 2,
          channel_name: 'Ch-Cancelled',
          upstream_model: 'm1',
          status_code: 499,
          duration_ms: 300,
          action_taken: 'client_cancelled',
        },
        {
          attempt_index: 4,
          channel_id: 3,
          channel_name: 'Ch-Custom',
          upstream_model: 'm1',
          status_code: 500,
          duration_ms: 400,
          action_taken: 'custom_action',
        },
      ],
    };

    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) => {
      if (url.startsWith('/api/logs?')) return Promise.resolve(response([actionTestingLog]));
      if (url.startsWith('/api/channels')) return Promise.resolve(response([]));
      return Promise.resolve(response([]));
    }));

    render(<Logs />);
    const detailBtn = await screen.findByRole('button', { name: '查看请求 99 详情' });
    fireEvent.click(detailBtn);

    const traceSection = screen.getByRole('region', { name: '尝试与切换轨迹' });

    // Action mappings
    expect(within(traceSection).getByText('本次尝试结束')).toBeTruthy();
    expect(within(traceSection).getByText('单凭据重试')).toBeTruthy();
    expect(within(traceSection).getByText('客户端取消')).toBeTruthy();
    expect(within(traceSection).getByText('custom_action')).toBeTruthy();

    // 5xx status codes should have red badges
    const status502Badge = within(traceSection).getByText('502');
    expect(status502Badge.className).toContain('badge-red');
    const status503Badge = within(traceSection).getByText('503');
    expect(status503Badge.className).toContain('badge-red');
  });
});


it('marks not-sent records without zero token or price claims in list and detail', async () => {
  vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(response(url.startsWith('/api/logs') ? [{ ...baseLog, not_sent: true, status: 503, input_tokens: 0, output_tokens: 0, official_value: 0, actual_cost: 0, official_currency: 'USD', currency: 'USD', error: '全部冷却' }] : []))));
  render(<Logs />);
  const row = (await screen.findByText('未发送')).closest('tr')!;
  const cells = within(row).getAllByRole('cell');
  expect(cells[2].textContent).toBe('— / —');
  expect(cells[3].textContent).toBe('—');
  expect(cells[4].textContent).toBe('—');
  fireEvent.click(screen.getByRole('button', { name: '查看请求 101 详情' }));
  const dialog = await screen.findByRole('dialog');
  expect(within(dialog).getByText('503 · 未发送')).toBeTruthy();
  expect(within(dialog).getByText('未发送，不计入上游调用')).toBeTruthy();
  for (const title of ['输入 Tokens', '输出 Tokens', '缓存读取 Tokens', '缓存写入 Tokens', '实际支出', '等价API价值']) expect(within(dialog).getByText(title).nextElementSibling?.textContent).toBe('—');
});
