// A minimal server-sent events parser (the subset rw web sends: event,
// data, retry and comments), fed with decoded text chunks of any size.

export interface SseMessage {
  event: string;
  data: string;
}

export class SseParser {
  private buf = '';
  private event = '';
  private data: string[] = [];
  /** The last retry interval the server asked for, in milliseconds. */
  retry: number | undefined;

  constructor(private readonly onMessage: (m: SseMessage) => void) {}

  push(chunk: string): void {
    this.buf += chunk;
    for (;;) {
      const i = this.buf.search(/\r\n|\r|\n/);
      if (i < 0) {
        break;
      }
      // A lone \r at the very end may be the first half of \r\n.
      if (this.buf[i] === '\r' && i === this.buf.length - 1) {
        break;
      }
      const line = this.buf.slice(0, i);
      const eol = this.buf.startsWith('\r\n', i) ? 2 : 1;
      this.buf = this.buf.slice(i + eol);
      this.line(line);
    }
  }

  /** Drops a partly received message (the connection ended). */
  reset(): void {
    this.buf = '';
    this.event = '';
    this.data = [];
  }

  private line(line: string): void {
    if (line === '') {
      if (this.data.length > 0) {
        this.onMessage({ event: this.event || 'message', data: this.data.join('\n') });
      }
      this.event = '';
      this.data = [];
      return;
    }
    if (line.startsWith(':')) {
      return; // comment (keep-alive ping)
    }
    const c = line.indexOf(':');
    const field = c < 0 ? line : line.slice(0, c);
    let value = c < 0 ? '' : line.slice(c + 1);
    if (value.startsWith(' ')) {
      value = value.slice(1);
    }
    switch (field) {
      case 'event':
        this.event = value;
        break;
      case 'data':
        this.data.push(value);
        break;
      case 'retry': {
        const n = Number(value);
        if (Number.isInteger(n) && n >= 0) {
          this.retry = n;
        }
        break;
      }
    }
  }
}
