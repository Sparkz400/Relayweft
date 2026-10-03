// The `sy web --client` child process: started without a shell (the
// arguments go to the program as they are, nothing is quoted by hand),
// talked to through JSON lines on stdin/stdout, and stopped by closing
// its stdin (sy then cancels the running task and stops its agents like
// on Ctrl+C), with a kill as the last resort.

import { spawn, type ChildProcessWithoutNullStreams } from 'child_process';
import { parseHello } from './locate';
import type { ClientHello } from './types';

export class SyNotFoundError extends Error {}

const HELLO_TIMEOUT_MS = 60_000;
const REPLY_TIMEOUT_MS = 10_000;

export class SyProcess {
  readonly hello: ClientHello;
  private waiting: ((line: string) => void)[] = [];
  private exited = false;
  private exitWaiters: (() => void)[] = [];
  private exitListeners: ((code: number | null, signal: string | null) => void)[] = [];

  private constructor(private readonly child: ChildProcessWithoutNullStreams, hello: ClientHello, lines: LineReader) {
    this.hello = hello;
    lines.onLine = (l) => {
      const w = this.waiting.shift();
      if (w) {
        w(l);
      }
    };
    child.on('exit', (code, signal) => {
      this.exited = true;
      for (const w of this.waiting.splice(0)) {
        w('');
      }
      for (const w of this.exitWaiters.splice(0)) {
        w();
      }
      for (const l of this.exitListeners) {
        l(code, signal);
      }
    });
  }

  get pid(): number | undefined {
    return this.child.pid;
  }

  get running(): boolean {
    return !this.exited;
  }

  onExit(fn: (code: number | null, signal: string | null) => void): void {
    this.exitListeners.push(fn);
  }

  /**
   * Starts sy and waits for its hello line. stderr lines go to onStderr
   * (sy's human-readable messages and warnings).
   */
  static start(exe: string, args: string[], cwd: string, onStderr: (line: string) => void): Promise<SyProcess> {
    return new Promise((resolve, reject) => {
      let child: ChildProcessWithoutNullStreams;
      try {
        child = spawn(exe, args, { cwd, shell: false, windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'], env: process.env });
      } catch (err) {
        reject(err);
        return;
      }
      const errTail: string[] = [];
      const errLines = new LineReader((l) => {
        errTail.push(l);
        if (errTail.length > 20) {
          errTail.shift();
        }
        onStderr(l);
      });
      child.stderr.setEncoding('utf8');
      child.stderr.on('data', (d: string) => errLines.push(d));
      let settled = false;
      const outLines = new LineReader(() => undefined);
      const fail = (e: Error) => {
        if (settled) {
          return;
        }
        settled = true;
        clearTimeout(timer);
        if (child.exitCode === null && !child.killed) {
          child.kill();
        }
        reject(e);
      };
      const timer = setTimeout(() => fail(new Error('sy did not start within a minute')), HELLO_TIMEOUT_MS);
      outLines.onLine = (line) => {
        if (settled) {
          return;
        }
        let hello: ClientHello;
        try {
          hello = parseHello(line);
        } catch (e) {
          fail(e as Error);
          return;
        }
        settled = true;
        clearTimeout(timer);
        resolve(new SyProcess(child, hello, outLines));
      };
      child.stdout.setEncoding('utf8');
      child.stdout.on('data', (d: string) => outLines.push(d));
      child.on('error', (err: NodeJS.ErrnoException) => {
        if (err.code === 'ENOENT') {
          fail(new SyNotFoundError(`could not start ${exe}: not found`));
        } else {
          fail(new Error(`could not start ${exe}: ${err.message}`));
        }
      });
      child.on('exit', (code) => {
        errLines.flush();
        const why = errTail.filter((l) => l.trim()).slice(-6).join('\n');
        fail(new Error(`sy exited (code ${String(code)}) before it was ready${why ? ':\n' + why : ''}`));
      });
      // An error writing to a dead process must not crash the extension host.
      child.stdin.on('error', () => undefined);
    });
  }

  /** Sends a command ("link" or "bootstrap") and returns sy's JSON answer. */
  async request(cmd: 'link' | 'bootstrap'): Promise<Record<string, string>> {
    if (this.exited) {
      throw new Error('sy is not running');
    }
    const line = await new Promise<string>((resolve, reject) => {
      const timer = setTimeout(() => {
        const i = this.waiting.indexOf(done);
        if (i >= 0) {
          this.waiting.splice(i, 1);
        }
        reject(new Error('sy did not answer'));
      }, REPLY_TIMEOUT_MS);
      const done = (l: string) => {
        clearTimeout(timer);
        resolve(l);
      };
      this.waiting.push(done);
      this.child.stdin.write(cmd + '\n');
    });
    if (!line) {
      throw new Error('sy stopped');
    }
    const v = JSON.parse(line) as Record<string, string>;
    if (v.error) {
      throw new Error(v.error);
    }
    return v;
  }

  /**
   * Stops sy: closes its stdin (a clean stop: the running task is
   * cancelled and its agents stopped) and kills it if it is still running
   * after graceMs.
   */
  stop(graceMs = 10_000): Promise<void> {
    if (this.exited) {
      return Promise.resolve();
    }
    const done = new Promise<void>((resolve) => this.exitWaiters.push(resolve));
    this.child.stdin.end();
    const timer = setTimeout(() => {
      if (!this.exited) {
        this.child.kill();
      }
    }, graceMs);
    return done.finally(() => clearTimeout(timer));
  }
}

/** Splits a text stream into lines. */
export class LineReader {
  private buf = '';
  constructor(public onLine: (line: string) => void) {}

  push(chunk: string): void {
    this.buf += chunk;
    let i: number;
    while ((i = this.buf.indexOf('\n')) >= 0) {
      const line = this.buf.slice(0, i).replace(/\r$/, '');
      this.buf = this.buf.slice(i + 1);
      this.onLine(line);
    }
  }

  flush(): void {
    if (this.buf) {
      const l = this.buf;
      this.buf = '';
      this.onLine(l);
    }
  }
}
