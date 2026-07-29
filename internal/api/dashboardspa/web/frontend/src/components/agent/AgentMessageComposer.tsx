import { useState } from 'react';
import { errorMessage } from 'gas-city-dashboard-shared';
import { Button } from '../Button';

const MAX_MESSAGE_LENGTH = 16 * 1024;

export function AgentMessageComposer({
  enabled,
  disabledReason,
  onSend,
}: {
  enabled: boolean;
  disabledReason: string;
  onSend: (message: string) => Promise<{ request_id: string }>;
}) {
  const [message, setMessage] = useState('');
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [receipt, setReceipt] = useState<string | null>(null);
  const trimmed = message.trim();
  const disabled = !enabled || sending || trimmed.length === 0;

  const send = async () => {
    if (disabled) return;
    setSending(true);
    setError(null);
    setReceipt(null);
    try {
      const accepted = await onSend(trimmed);
      setMessage('');
      setReceipt(accepted.request_id);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSending(false);
    }
  };

  return (
    <section className="mt-12" aria-labelledby="agent-direct-message-heading">
      <header className="flex items-baseline justify-between mb-4">
        <h2
          id="agent-direct-message-heading"
          className="text-label uppercase tracking-wider text-fg-faint"
        >
          Direct message
        </h2>
        {!enabled && <span className="text-label text-fg-faint">{disabledReason}</span>}
      </header>
      <div className="space-y-3">
        <textarea
          aria-label="Message agent"
          value={message}
          onChange={(event) => setMessage(event.target.value)}
          rows={4}
          maxLength={MAX_MESSAGE_LENGTH}
          disabled={!enabled || sending}
          title={!enabled ? disabledReason : undefined}
          className="w-full bg-surface-tint border border-rule rounded-sm px-3 py-2 text-body text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent/40 resize-y disabled:opacity-50"
        />
        <div className="flex items-baseline justify-between gap-4">
          <div aria-live="polite" className="text-label text-fg-muted">
            {error !== null ? (
              <span className="text-accent" role="alert">
                {error}
              </span>
            ) : receipt !== null ? (
              <span>
                Message accepted · <code>{receipt}</code>
              </span>
            ) : (
              <span>Delivered through the session controller.</span>
            )}
          </div>
          <Button tone="accent" onClick={() => void send()} disabled={disabled}>
            {sending ? 'Sending' : 'Send'}
          </Button>
        </div>
      </div>
    </section>
  );
}
