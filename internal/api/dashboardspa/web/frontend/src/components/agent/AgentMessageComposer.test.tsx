import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AgentMessageComposer } from './AgentMessageComposer';

describe('AgentMessageComposer', () => {
  afterEach(() => {
    cleanup();
  });

  it('sends a trimmed message and surfaces the async request receipt', async () => {
    const onSend = vi.fn(async () => ({ request_id: 'req-42' }));
    render(<AgentMessageComposer enabled disabledReason="" onSend={onSend} />);

    fireEvent.change(screen.getByRole('textbox', { name: 'Message agent' }), {
      target: { value: '  continue the review  ' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => {
      expect(onSend).toHaveBeenCalledWith('continue the review');
    });
    expect(await screen.findByText(/req-42/)).toBeTruthy();
    expect(
      (screen.getByRole('textbox', { name: 'Message agent' }) as HTMLTextAreaElement).value,
    ).toBe('');
  });

  it('keeps the write control disabled for a non-operator or stopped session', () => {
    render(
      <AgentMessageComposer
        enabled={false}
        disabledReason="Switch to operator identity to send."
        onSend={vi.fn()}
      />,
    );

    expect(
      (screen.getByRole('textbox', { name: 'Message agent' }) as HTMLTextAreaElement).disabled,
    ).toBe(true);
    expect((screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText('Switch to operator identity to send.')).toBeTruthy();
  });

  it('preserves the draft and reports a failed send', async () => {
    const onSend = vi.fn(async () => {
      throw new Error('proxy rejected write');
    });
    render(<AgentMessageComposer enabled disabledReason="" onSend={onSend} />);

    const input = screen.getByRole('textbox', { name: 'Message agent' }) as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: 'retry me' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    expect((await screen.findByRole('alert')).textContent).toContain('proxy rejected write');
    expect(input.value).toBe('retry me');
  });
});
