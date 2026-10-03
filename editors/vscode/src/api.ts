// sy web's API, spoken like a browser tab speaks it: a single-use
// bootstrap is traded for a session (POST /api/session), and every /api
// request carries the session in X-Switchyard-Session. The client sends
// no Origin (it is not a web page) and no cookies; sy checks the Host
// header and the session on every request. Only 127.0.0.1 is ever
// contacted, with node's http module (no proxy, no fetch).

import * as http from 'http';
import { SseParser, type SseMessage } from './sse';

export const SESSION_HEADER = 'X-Switchyard-Session';

export class ApiError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
  }
}

interface Raw {
  status: number;
  body: string;
}

export class SyApi {
  private session = '';
  private readonly host: string;
  private readonly port: number;

  /**
   * base is http://127.0.0.1:PORT. reauth returns a fresh bootstrap (sy
   * mints one on request), used when the session was dropped.
   */
  constructor(base: string, private readonly reauth: () => Promise<string>) {
    const u = new URL(base);
    if (u.protocol !== 'http:' || !['127.0.0.1', 'localhost', '[::1]'].includes(u.hostname)) {
      throw new Error('refusing a non-loopback sy address: ' + base);
    }
    this.host = u.hostname.replace(/^\[(.*)\]$/, '$1');
    this.port = Number(u.port);
  }

  get hasSession(): boolean {
    return this.session !== '';
  }

  /** Trades a bootstrap for a session. */
  async login(bootstrap: string): Promise<void> {
    const r = await this.raw('POST', '/api/session', JSON.stringify({ bootstrap }), false);
    if (r.status !== 200) {
      throw new ApiError(r.status, errorText(r));
    }
    const v = JSON.parse(r.body) as { session?: string };
    if (!v.session) {
      throw new Error('sy returned no session');
    }
    this.session = v.session;
  }

  private relogin: Promise<void> | undefined;

  private async refreshSession(): Promise<void> {
    this.relogin ??= (async () => {
      try {
        await this.login(await this.reauth());
      } finally {
        this.relogin = undefined;
      }
    })();
    return this.relogin;
  }

  /** A JSON request; it logs in again once when the session was dropped. */
  async call<T>(method: 'GET' | 'POST', path: string, body?: unknown): Promise<T> {
    const data = body === undefined ? (method === 'POST' ? '{}' : undefined) : JSON.stringify(body);
    let r = await this.raw(method, path, data, true);
    if (r.status === 401) {
      await this.refreshSession();
      r = await this.raw(method, path, data, true);
    }
    if (r.status < 200 || r.status >= 300) {
      throw new ApiError(r.status, errorText(r));
    }
    return (r.body ? JSON.parse(r.body) : undefined) as T;
  }

  private headers(withSession: boolean, json: boolean): http.OutgoingHttpHeaders {
    const h: http.OutgoingHttpHeaders = { Accept: 'application/json' };
    if (json) {
      h['Content-Type'] = 'application/json';
    }
    if (withSession && this.session) {
      h[SESSION_HEADER] = this.session;
    }
    return h;
  }

  private raw(method: string, path: string, body: string | undefined, withSession: boolean): Promise<Raw> {
    return new Promise((resolve, reject) => {
      const req = http.request(
        { host: this.host, port: this.port, method, path, headers: this.headers(withSession, body !== undefined), timeout: 30_000, agent: false },
        (res) => {
          res.setEncoding('utf8');
          let buf = '';
          res.on('data', (d: string) => (buf += d));
          res.on('end', () => resolve({ status: res.statusCode ?? 0, body: buf }));
          res.on('error', reject);
        },
      );
      req.on('timeout', () => req.destroy(new Error('sy did not answer in time')));
      req.on('error', reject);
      if (body !== undefined) {
        req.write(body);
      }
      req.end();
    });
  }

  /**
   * Follows the event stream until the returned function is called. It
   * reconnects after a dropped connection (sy then sends a reset and the
   * replay again) and logs in again when the session was dropped.
   */
  stream(onMessage: (m: SseMessage) => void, onStatus: (connected: boolean, err?: string) => void): () => void {
    let closed = false;
    let req: http.ClientRequest | undefined;
    let timer: NodeJS.Timeout | undefined;
    let refused = 0; // 401s in a row
    const parser = new SseParser(onMessage);
    const again = (err?: string) => {
      if (closed) {
        return;
      }
      onStatus(false, err);
      parser.reset();
      timer = setTimeout(connect, parser.retry ?? 1500);
    };
    const connect = () => {
      if (closed) {
        return;
      }
      // Each connection retries once, whichever of its events comes first.
      let over = false;
      const retry = (err?: string) => {
        if (!over) {
          over = true;
          again(err);
        }
      };
      req = http.request(
        { host: this.host, port: this.port, method: 'GET', path: '/api/events', headers: { ...this.headers(true, false), Accept: 'text/event-stream' }, agent: false },
        (res) => {
          if (res.statusCode === 401) {
            over = true;
            res.resume();
            // Log in again; reconnect at once the first time, then wait.
            this.refreshSession().then(
              () => (refused++ === 0 ? connect() : again('the session was refused again')),
              (e: Error) => again('login failed: ' + e.message),
            );
            return;
          }
          if (res.statusCode !== 200) {
            res.resume();
            retry(`event stream: HTTP ${String(res.statusCode)}`);
            return;
          }
          refused = 0;
          onStatus(true);
          res.setEncoding('utf8');
          res.on('data', (d: string) => parser.push(d));
          res.on('end', () => retry());
          res.on('close', () => retry());
          res.on('error', (e) => retry(e.message));
        },
      );
      req.on('error', (e) => retry(e.message));
      req.end();
    };
    connect();
    return () => {
      closed = true;
      if (timer) {
        clearTimeout(timer);
      }
      req?.destroy();
    };
  }
}

function errorText(r: Raw): string {
  try {
    const v = JSON.parse(r.body) as { error?: string };
    if (v.error) {
      return v.error;
    }
  } catch {
    // not JSON
  }
  return `HTTP ${r.status}${r.body ? ': ' + r.body.trim().slice(0, 200) : ''}`;
}
