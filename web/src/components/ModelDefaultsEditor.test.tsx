// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ModelDefaultsEditor } from './ModelDefaultsEditor';

afterEach(cleanup);
describe('global request defaults editor', () => {
  it('preserves explicit zero and false while editing unrelated fields', () => {
    const onChange = vi.fn();
    render(<ModelDefaultsEditor value={{ max_output_tokens: 0, temperature: 0, thinking: { mode: 'enabled', enabled: false } }} onChange={onChange} />);
    expect(screen.getByLabelText('默认思考开关')).toHaveProperty('value', 'false');
    fireEvent.change(screen.getByLabelText('默认 temperature'), { target: { value: '1' } });
    expect(onChange.mock.lastCall![0]).toEqual({ max_output_tokens: 0, temperature: 1, thinking: { mode: 'enabled', enabled: false } });
  });
  it('removes blank defaults and makes mode changes independent', () => {
    const onChange = vi.fn();
    const { rerender } = render(<ModelDefaultsEditor value={{ temperature: 0, thinking: { mode: 'effort', effort: 'custom' } }} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText('默认思考模式'), { target: { value: 'budget' } });
    expect(onChange.mock.lastCall![0]).toEqual({ temperature: 0, thinking: { mode: 'budget' } });
    rerender(<ModelDefaultsEditor value={onChange.mock.lastCall![0]} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText('默认思考预算'), { target: { value: '0' } });
    expect(onChange.mock.lastCall![0]).toEqual({ temperature: 0, thinking: { mode: 'budget', budget: 0 } });
    fireEvent.change(screen.getByLabelText('默认思考模式'), { target: { value: '' } });
    expect(onChange.mock.lastCall![0]).toEqual({ temperature: 0 });
    fireEvent.change(screen.getByLabelText('默认 temperature'), { target: { value: '' } });
    expect(onChange.mock.lastCall![0]).not.toHaveProperty('temperature');
  });
  it('never exposes or stores model capability ranges', () => {
    const onChange = vi.fn();
    render(<ModelDefaultsEditor value={{}} onChange={onChange} />);
    expect(screen.queryByLabelText('设置支持的思考选项')).toBeNull();
    expect(screen.queryByLabelText('上下文总窗口')).toBeNull();
    fireEvent.change(screen.getByLabelText('默认最大输出'), { target: { value: '12' } });
    expect(onChange.mock.lastCall![0]).toEqual({ max_output_tokens: 12 });
  });
});
