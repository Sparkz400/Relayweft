import * as assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as path from 'node:path';
import { test } from 'node:test';
import { decisionBody, selectAll, setFile, setHunk, type ReviewFile } from '../decision';
import { clientArgs, locateRw, parseHello, userSetting } from '../locate';
import { parseFollowUp, TaskModel } from '../model';
import { SseParser, type SseMessage } from '../sse';
import { LineReader } from '../rwProcess';

test('SSE parser handles split chunks, CRLF, comments and retry', () => {
  const got: SseMessage[] = [];
  const p = new SseParser((m) => got.push(m));
  p.push('retry: 1500\n\n: ping\n\nevent: st');
  p.push('ate\ndata: {"a":1}\r');
  p.push('\n\r\nevent: ev\ndata: line1\ndata: line2\n\ndata: plain\n\n');
  assert.equal(p.retry, 1500);
  assert.deepEqual(got, [
    { event: 'state', data: '{"a":1}' },
    { event: 'ev', data: 'line1\nline2' },
    { event: 'message', data: 'plain' },
  ]);
});

test('LineReader splits lines across chunks', () => {
  const lines: string[] = [];
  const r = new LineReader((l) => lines.push(l));
  r.push('{"a"');
  r.push(':1}\r\n{"b":2}\npart');
  r.flush();
  assert.deepEqual(lines, ['{"a":1}', '{"b":2}', 'part']);
});

const hex = 'ab'.repeat(32);

test('parseHello accepts the client hello and refuses anything else', () => {
  const h = parseHello(JSON.stringify({ relayweft: 'web-client', protocol: 1, version: 'v1', url: 'http://127.0.0.1:1234', addr: '127.0.0.1:1234', bootstrap: hex, dir: '/x', pid: 9 }));
  assert.equal(h.url, 'http://127.0.0.1:1234');
  assert.throws(() => parseHello('Relayweft web UI on http://127.0.0.1:1'), /not the client hello/);
  assert.throws(() => parseHello(JSON.stringify({ relayweft: 'web-client', protocol: 2, url: 'http://127.0.0.1:1', bootstrap: hex })), /protocol 2/);
  assert.throws(() => parseHello(JSON.stringify({ relayweft: 'web-client', protocol: 1, url: 'http://10.0.0.1:1', bootstrap: hex })), /non-loopback/);
  assert.throws(() => parseHello(JSON.stringify({ relayweft: 'web-client', protocol: 1, url: 'http://127.0.0.1:1', bootstrap: 'x' })), /bootstrap/);
});

test('clientArgs passes arguments as they are', () => {
  assert.deepEqual(clientArgs('C:\\My Projects\\a b', ['--threads', '2', '', '--route', 'worker=codex:gpt "x"']), [
    'web', '--client', '--dir', 'C:\\My Projects\\a b', '--threads', '2', '--route', 'worker=codex:gpt "x"',
  ]);
});

test('locateRw on Windows wants an .exe on PATH, then the Go and Scoop folders', () => {
  const files = new Set(['C:\\tools\\rw.cmd', 'C:\\Users\\me\\go\\bin\\rw.exe']);
  const le = { platform: 'win32' as const, env: { Path: 'C:\\tools;"C:\\Program Files\\x"' }, home: 'C:\\Users\\me', isFile: (p: string) => files.has(p) };
  assert.equal(locateRw('', le).path, 'C:\\Users\\me\\go\\bin\\rw.exe');
  files.add('C:\\Program Files\\x\\rw.exe');
  assert.equal(locateRw('', le).path, 'C:\\Program Files\\x\\rw.exe');
  assert.equal(locateRw('D:\\rw\\rw', le).path, undefined);
  files.add('D:\\rw\\rw.exe');
  assert.equal(locateRw('D:\\rw\\rw', le).path, 'D:\\rw\\rw.exe');
  assert.equal(locateRw('~\\go\\bin\\rw.exe', le).path, 'C:\\Users\\me\\go\\bin\\rw.exe');
});

test('locateRw on Unix', () => {
  const files = new Set(['/home/me/go/bin/rw']);
  const le = { platform: 'linux' as const, env: { PATH: '/usr/bin:/bin' }, home: '/home/me', isFile: (p: string) => files.has(p) };
  const r = locateRw('', le);
  assert.equal(r.path, path.posix.join('/home/me/go/bin', 'rw'));
  assert.ok(r.tried.includes('/usr/bin/rw'));
  assert.equal(locateRw('/opt/rw', le).path, undefined);
});

test('parseFollowUp matches the server', () => {
  assert.deepEqual(parseFollowUp('@s1 fix the test'), { agent: 's1', message: 'fix the test' });
  assert.deepEqual(parseFollowUp('@ again'), { agent: '', message: 'again' });
  assert.deepEqual(parseFollowUp('@last more'), { agent: '', message: 'more' });
  assert.equal(parseFollowUp('@types/node is missing'), undefined);
  assert.equal(parseFollowUp('plain task'), undefined);
});

test('decision body mirrors the web page', () => {
  const files: ReviewFile[] = [
    { path: 'a.go', splittable: true, hunks: ['h0', 'h1', 'h2'] },
    { path: 'b.go', splittable: false },
    { path: 'c.go', splittable: true, hunks: ['h0', 'h1'] },
  ];
  const sel = selectAll(files);
  assert.deepEqual(decisionBody(files, sel), { apply: ['a.go', 'b.go', 'c.go'], hunks: {} });
  setHunk(files, sel, 0, 1, false);
  assert.equal(setHunk(files, sel, 1, 0, false), false);
  setHunk(files, sel, 2, 0, false);
  setHunk(files, sel, 2, 1, false);
  assert.deepEqual(decisionBody(files, sel), { apply: ['a.go'], hunks: { 'a.go': [0, 2] } });
  setFile(files, sel, 2, true);
  assert.deepEqual(decisionBody(files, sel), { apply: ['a.go', 'c.go'], hunks: { 'a.go': [0, 2] } });
});

test('TaskModel folds events into a tree and log lines', () => {
  const m = new TaskModel();
  const ts = new Date().toISOString();
  assert.equal(m.fold({ kind: 'task_start', text: 'do it', ts }), 'task: do it');
  m.fold({ kind: 'started', agent_id: 'main', provider: 'claude', model: 'opus', ts });
  m.fold({ kind: 'done', agent_id: 'main', ok: true, ts });
  m.fold({ kind: 'queued', agent_id: 's1', parent_id: 'main', role: 'worker', text: 'step one', ts });
  m.fold({ kind: 'started', agent_id: 's1', provider: 'codex', model: 'gpt-5', ts });
  m.fold({ kind: 'started', agent_id: 'reviewer', provider: 'claude', model: 'opus', ts });
  m.fold({ kind: 'usage', agent_id: 's1', tokens: { input: 100, cached: 40, output: 10 }, ts });
  assert.match(m.fold({ kind: 'edit', agent_id: 's1', text: 'a.go', ts }) ?? '', /s1.*edit: a\.go/);
  assert.equal(m.nodes.get('s1')?.status, 'running');
  assert.equal(m.nodes.get('s1')?.tokens, 70);
  assert.deepEqual(m.children('main').map((n) => n.id), ['s1', 'reviewer']);
  m.fold({ kind: 'done', agent_id: 's1', ok: true, ts });
  m.fold({ kind: 'task_done', ok: false, text: 'failed', ts });
  assert.equal(m.nodes.get('s1')?.status, 'ok');
  assert.equal(m.nodes.get('reviewer')?.status, 'killed');
  assert.equal(m.nodes.get('main')?.status, 'failed');
});

test('rw path and args come only from user settings, never a workspace', () => {
  // A repository's .vscode/settings.json must not choose what program runs.
  const ws = { defaultValue: '', workspaceValue: '/repo/evil', workspaceFolderValue: '/repo/evil' };
  assert.equal(userSetting(ws, 'x'), '');
  assert.equal(userSetting({ ...ws, globalValue: '/usr/bin/rw' }, ''), '/usr/bin/rw');
  assert.deepEqual(userSetting({ defaultValue: [], workspaceValue: ['--evil'] }, ['x']), []);
  assert.deepEqual(userSetting(undefined, ['x']), ['x']);
  const pkg = JSON.parse(readFileSync(path.join(__dirname, '..', '..', 'package.json'), 'utf8'));
  const props = pkg.contributes.configuration.properties;
  for (const k of ['relayweft.path', 'relayweft.args']) {
    assert.equal(props[k].scope, 'machine', k);
  }
  assert.equal(pkg.capabilities?.untrustedWorkspaces?.supported, false);
});
