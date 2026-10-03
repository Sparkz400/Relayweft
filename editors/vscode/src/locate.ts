// Finding the sy executable and reading the `sy web --client` hello line.
// Pure (no vscode import) so it can be unit tested.

import * as path from 'path';
import type { ClientHello } from './types';

export interface LocateEnv {
  platform: NodeJS.Platform;
  env: Record<string, string | undefined>;
  home: string;
  isFile: (p: string) => boolean;
}

export interface Located {
  path?: string;
  /** Where it looked, for the error message. */
  tried: string[];
}

/**
 * Resolves the sy executable: the configured path (a bare name is looked
 * up on PATH), else "sy" on PATH, else Go's and Scoop's install folders.
 * On Windows only .exe files count: sy is started without a shell, and a
 * .cmd/.bat shim cannot be started that way.
 */
export function locateSy(configured: string, le: LocateEnv): Located {
  const win = le.platform === 'win32';
  const p = win ? path.win32 : path.posix;
  const tried: string[] = [];
  const names = (base: string) => (win && !/\.exe$/i.test(base) ? [base + '.exe'] : [base]);
  const check = (cand: string) => {
    tried.push(cand);
    return le.isFile(cand);
  };
  const expand = (s: string) => (s === '~' || s.startsWith('~/') || s.startsWith('~\\') ? p.join(le.home, s.slice(1)) : s);

  const want = expand(configured.trim());
  if (want && (want.includes('/') || want.includes('\\'))) {
    for (const n of names(want)) {
      if (check(n)) {
        return { path: n, tried };
      }
    }
    return { tried };
  }
  const base = want || 'sy';
  const pathVar = le.env.PATH ?? le.env.Path ?? le.env.path ?? '';
  for (const dir of pathVar.split(win ? ';' : ':')) {
    const d = dir.trim().replace(/^"(.*)"$/, '$1');
    if (!d) {
      continue;
    }
    for (const n of names(base)) {
      if (check(p.join(d, n))) {
        return { path: p.join(d, n), tried };
      }
    }
  }
  if (!want) {
    const extra: string[] = [];
    if (le.env.GOBIN) {
      extra.push(le.env.GOBIN);
    }
    extra.push(p.join(le.env.GOPATH || p.join(le.home, 'go'), 'bin'));
    if (win) {
      extra.push(p.join(le.env.SCOOP ?? p.join(le.home, 'scoop'), 'shims'));
      if (le.env.LOCALAPPDATA) {
        extra.push(p.join(le.env.LOCALAPPDATA, 'Microsoft', 'WinGet', 'Links'));
      }
    } else {
      extra.push('/usr/local/bin', p.join(le.home, '.local', 'bin'));
    }
    for (const d of extra) {
      for (const n of names('sy')) {
        if (check(p.join(d, n))) {
          return { path: p.join(d, n), tried };
        }
      }
    }
  }
  return { tried };
}

/** The arguments for `sy web --client` in dir, then the user's extra ones. */
export function clientArgs(dir: string, extra: readonly string[]): string[] {
  return ['web', '--client', '--dir', dir, ...extra.filter((a) => typeof a === 'string' && a !== '')];
}

export const PROTOCOL = 1;

/** Parses the hello line; it throws with a readable reason. */
export function parseHello(line: string): ClientHello {
  let v: unknown;
  try {
    v = JSON.parse(line);
  } catch {
    throw new Error(`sy printed something that is not the client hello: ${line.slice(0, 200)} (is this sy older than \`sy web --client\`?)`);
  }
  const h = v as Partial<ClientHello>;
  if (!h || h.switchyard !== 'web-client') {
    throw new Error('sy printed an unexpected first line (not a web-client hello)');
  }
  if (h.protocol !== PROTOCOL) {
    throw new Error(`sy speaks client protocol ${String(h.protocol)}, this extension speaks ${PROTOCOL}: update one of them`);
  }
  if (typeof h.url !== 'string' || !/^http:\/\/(127\.0\.0\.1|localhost|\[::1\]):\d+$/.test(h.url)) {
    throw new Error('sy announced a non-loopback address: ' + String(h.url));
  }
  if (typeof h.bootstrap !== 'string' || !/^[0-9a-f]{64}$/.test(h.bootstrap)) {
    throw new Error('sy announced no valid bootstrap');
  }
  return h as ClientHello;
}
