// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ThinkingEffortPicker } from './ThinkingEffortPicker';

afterEach(() => {
  cleanup();
});

describe('ThinkingEffortPicker', () => {
  it('renders preset rows and selects all items in a row with the row checkbox', () => {
    const onChange = vi.fn();
    render(<ThinkingEffortPicker efforts={[]} onChange={onChange} />);

    expect(screen.getByText('开关控制')).toBeTruthy();
    expect(screen.getByText('常规档位')).toBeTruthy();
    expect(screen.getByText('极高档位')).toBeTruthy();
    expect(screen.getByText('极简档位')).toBeTruthy();

    // Check row select all for '开关控制' (disable, enable)
    const rowCheckbox = screen.getByLabelText('全选 开关控制 行');
    fireEvent.click(rowCheckbox);
    expect(onChange).toHaveBeenCalledWith(['disable', 'enable'], undefined);
  });

  it('deselects all items in a row when clicking checked row checkbox', () => {
    const onChange = vi.fn();
    render(<ThinkingEffortPicker efforts={['disable', 'enable', 'low']} defaultEffort="low" onChange={onChange} />);

    const rowCheckbox = screen.getByLabelText('全选 开关控制 行');
    fireEvent.click(rowCheckbox);
    // Should remove disable and enable, leaving low
    expect(onChange).toHaveBeenCalledWith(['low'], 'low');
  });

  it('supports adding and deleting custom effort tags', () => {
    const onChange = vi.fn();
    render(<ThinkingEffortPicker efforts={['custom-one']} onChange={onChange} />);

    // Custom tag rendered with delete button
    expect(screen.getAllByText('custom-one').length).toBeGreaterThan(0);
    const removeBtn = screen.getByLabelText('删除自创档位 custom-one');
    fireEvent.click(removeBtn);
    expect(onChange).toHaveBeenCalledWith([], undefined);

    // Add new custom tag
    const input = screen.getByPlaceholderText(/自创新档位/);
    fireEvent.change(input, { target: { value: 'custom-two' } });
    fireEvent.click(screen.getByRole('button', { name: '添加自创档位' }));
    expect(onChange).toHaveBeenCalledWith(['custom-one', 'custom-two'], undefined);
  });

  it('updates default effort when selected', () => {
    const onChange = vi.fn();
    render(
      <ThinkingEffortPicker
        efforts={['low', 'medium', 'high']}
        defaultEffort="low"
        onChange={onChange}
      />
    );

    const select = screen.getByLabelText('默认思考档位') as HTMLSelectElement;
    expect(select.value).toBe('low');
    fireEvent.change(select, { target: { value: 'high' } });
    expect(onChange).toHaveBeenCalledWith(['low', 'medium', 'high'], 'high');
  });

  it('clears a removed default without choosing another or losing an explicit empty range', () => {
    const onChange = vi.fn();
    const { rerender } = render(<ThinkingEffortPicker efforts={['low', 'high']} defaultEffort="low" onChange={onChange} />);
    fireEvent.click(screen.getByLabelText('low'));
    expect(onChange).toHaveBeenLastCalledWith(['high'], undefined);
    rerender(<ThinkingEffortPicker efforts={['high']} defaultEffort="high" onChange={onChange} />);
    fireEvent.click(screen.getByLabelText('high'));
    expect(onChange).toHaveBeenLastCalledWith([], undefined);
    rerender(<ThinkingEffortPicker efforts={[]} onChange={onChange} />);
    expect(screen.getByLabelText('默认思考档位')).toBeTruthy();
  });

  it('retains an existing unknown default while editing the range and allows clearing it', () => {
    const onChange = vi.fn();
    render(<ThinkingEffortPicker efforts={['low']} defaultEffort="old-custom" onChange={onChange} />);
    expect((screen.getByLabelText('默认思考档位') as HTMLSelectElement).value).toBe('old-custom');
    fireEvent.click(screen.getByLabelText('high'));
    expect(onChange).toHaveBeenLastCalledWith(['low', 'high'], 'old-custom');
    fireEvent.change(screen.getByLabelText('默认思考档位'), { target: { value: '' } });
    expect(onChange).toHaveBeenLastCalledWith(['low'], undefined);
  });

  it('uses updated custom choices and blocks defaults without a confirmed binding', () => {
    const onChange = vi.fn();
    const { rerender } = render(<ThinkingEffortPicker efforts={['first-custom']} onChange={onChange} />);
    rerender(<ThinkingEffortPicker efforts={['second-custom']} onChange={onChange} canSelectDefault={() => false} />);
    expect(screen.queryByLabelText('删除自创档位 first-custom')).toBeNull();
    expect(screen.getByLabelText('删除自创档位 second-custom')).toBeTruthy();
    expect((screen.getByRole('option', { name: /second-custom/ }) as HTMLOptionElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText('默认思考档位'), { target: { value: 'second-custom' } });
    expect(onChange).not.toHaveBeenCalled();
  });

  it('can edit a published range without offering a request default', () => {
    const onChange = vi.fn();
    render(<ThinkingEffortPicker efforts={['low']} showDefault={false} onChange={onChange} />);
    expect(screen.queryByLabelText('默认思考档位')).toBeNull();
    fireEvent.click(screen.getByLabelText('high'));
    expect(onChange).toHaveBeenLastCalledWith(['low', 'high'], undefined);
  });

});
