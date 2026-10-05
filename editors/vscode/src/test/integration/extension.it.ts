/// <reference types="mocha" />
// The extension in a real VS Code, driving a real `rw web --client` (built
// from this repo) whose agent is the scripted CLI in fakeClaude.ts. The
// tests run in order and share one workspace (a git repo made by
// runTest.ts): start, a task with plan editing and hunk review, undo,
// follow-up, cancel, stop, rw crashing, and a bad rw path.
//
// Dialogs (quick picks, input boxes, messages) are answered by replacing
// the vscode.window functions: the test file belongs to the extension's
// own folder, so it shares the extension's vscode API object.

import * as assert from 'assert';
import * as fs from 'fs';
import * as path from 'path';
import * as vscode from 'vscode';
import type { TestHooks } from '../../extension';
import type { ReviewItem } from '../../review';
import type { AgentsItem } from '../../tree';
import type { StateView } from '../../types';
import { EDITED_LINES, EDITED_PLAN_TEXT, edited, HANG, NOTES, NOTES_LINES, PLAN_TEXT } from './fakeClaude.consts';

const REPO = process.env.RW_TEST_REPO!;
const STATE = process.env.RW_TEST_STATE!;
const RW = process.env.RW_TEST_RW!;
const TASK = 'Please edit the notes file in this repository and add a new file next to it with a greeting';

// --- dialogs ------------------------------------------------------------------------

type Pick = (items: readonly vscode.QuickPickItem[], title: string) => vscode.QuickPickItem | undefined;

interface Said {
  level: 'info' | 'warn' | 'error';
  text: string;
  items: string[];
  modal: boolean;
}

const ui = {
  said: [] as Said[],
  picks: [] as Pick[],
  inputs: [] as (string | undefined)[],
  /** Answers a message that has buttons; undefined dismisses it. */
  answer: (_m: Said): string | undefined => undefined,
  opened: [] as string[],
  unexpected: [] as string[],
};

const win = vscode.window as unknown as Record<string, unknown>;
const saved: Record<string, unknown> = {};

function stub(name: string, fn: unknown): void {
  saved[name] = win[name];
  win[name] = fn;
}

function message(level: Said['level']) {
  return async (text: string, ...rest: unknown[]) => {
    let modal = false;
    if (rest.length && typeof rest[0] === 'object' && rest[0] !== null && !('title' in (rest[0] as object))) {
      modal = !!(rest.shift() as vscode.MessageOptions).modal;
    }
    const items = rest.map((r) => (typeof r === 'string' ? r : (r as vscode.MessageItem).title));
    const m: Said = { level, text, items, modal };
    ui.said.push(m);
    return items.length ? ui.answer(m) : undefined;
  };
}

function installStubs(): void {
  stub('showInformationMessage', message('info'));
  stub('showWarningMessage', message('warn'));
  stub('showErrorMessage', message('error'));
  stub('showQuickPick', async (items: readonly vscode.QuickPickItem[] | Thenable<readonly vscode.QuickPickItem[]>, opts?: vscode.QuickPickOptions) => {
    const list = await items;
    const title = opts?.title ?? opts?.placeHolder ?? '';
    const next = ui.picks.shift();
    if (!next) {
      ui.unexpected.push('quick pick: ' + title);
      return undefined;
    }
    return next(list, title);
  });
  stub('showInputBox', async (opts?: vscode.InputBoxOptions) => {
    if (!ui.inputs.length) {
      ui.unexpected.push('input box: ' + (opts?.title ?? ''));
      return undefined;
    }
    return ui.inputs.shift();
  });
  const env = vscode.env as unknown as Record<string, unknown>;
  saved.openExternal = env.openExternal;
  env.openExternal = async (u: vscode.Uri) => {
    ui.opened.push(u.toString(true));
    return true;
  };
}

function removeStubs(): void {
  for (const [k, v] of Object.entries(saved)) {
    if (k === 'openExternal') {
      (vscode.env as unknown as Record<string, unknown>).openExternal = v;
    } else {
      win[k] = v;
    }
  }
}

const byLabel = (re: RegExp): Pick => (items) => {
  const it = items.find((i) => re.test(i.label));
  if (!it) {
    throw new Error(`no item ${re} in ${items.map((i) => i.label).join(' | ')}`);
  }
  return it;
};

// --- helpers --------------------------------------------------------------------------

async function waitFor<T>(what: string, fn: () => T | undefined | false | Promise<T | undefined | false>, ms = 60_000): Promise<T> {
  const end = Date.now() + ms;
  let last: unknown;
  for (;;) {
    try {
      const v = await fn();
      if (v !== undefined && v !== false) {
        return v;
      }
    } catch (e) {
      last = e;
    }
    if (Date.now() > end) {
      throw new Error(`timed out waiting for ${what}${last ? ': ' + String(last) : ''}`);
    }
    await new Promise((r) => setTimeout(r, 150));
  }
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function calls(): { what: string; pid: number; sid: string; resumed: string; cwd: string; prompt: string }[] {
  const f = path.join(STATE, 'calls.log');
  if (!fs.existsSync(f)) {
    return [];
  }
  return fs.readFileSync(f, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
}

function readRepo(rel: string): string | undefined {
  try {
    return fs.readFileSync(path.join(REPO, rel), 'utf8').replace(/\r\n/g, '\n');
  } catch {
    return undefined;
  }
}

function reviewTabs(): vscode.Tab[] {
  return vscode.window.tabGroups.all.flatMap((g) => g.tabs).filter((t) => t.input instanceof vscode.TabInputTextDiff && t.input.modified.scheme === 'relayweft-review');
}

const origNotes = Array.from({ length: NOTES_LINES }, (_, i) => `line ${i + 1}`);

describe('Relayweft in a real VS Code', () => {
  let hooks: TestHooks;
  let leaveRunning = false;
  const st = (): StateView | undefined => hooks.model.state;
  const errors = () => ui.said.filter((m) => m.level === 'error').map((m) => m.text);

  before(async () => {
    installStubs();
    assert.ok(REPO && STATE && RW, 'run through runTest.js (RW_TEST_* are not set)');
  });

  afterEach(function () {
    if (this.currentTest?.state === 'failed') {
      console.log('--- Relayweft output channel ---\n' + hooks?.log.join('\n'));
      console.log('--- messages ---\n' + ui.said.map((m) => `${m.level}: ${m.text}`).join('\n'));
      console.log('--- fake agent calls ---\n' + calls().map((c) => JSON.stringify(c)).join('\n'));
    }
    assert.deepStrictEqual(ui.unexpected, [], 'dialogs nobody answered');
  });

  after(async () => {
    try {
      if (!leaveRunning) {
        await vscode.commands.executeCommand('relayweft.stop');
      }
    } finally {
      removeStubs();
    }
  });

  it('activates in a trusted workspace without errors', async () => {
    const ext = vscode.extensions.getExtension<TestHooks>('sparkz400.relayweft');
    assert.ok(ext, 'extension not found');
    hooks = (await ext.activate())!;
    assert.ok(hooks, 'activate returned no test hooks (extensionMode is not Test?)');
    assert.strictEqual(vscode.workspace.isTrusted, true);
    const cmds = await vscode.commands.getCommands(true);
    for (const c of ['relayweft.start', 'relayweft.stop', 'relayweft.runTask', 'relayweft.review.submit', 'relayweft.agents.focus']) {
      assert.ok(cmds.includes(c), 'missing command ' + c);
    }
    assert.strictEqual(hooks.running(), false);
    // A command that needs rw says so (and the stubs see the extension's dialogs).
    await vscode.commands.executeCommand('relayweft.cancelTask');
    assert.match(errors().pop() ?? '', /not running/);
  });

  it('starts rw web --client for the folder and connects', async () => {
    await vscode.commands.executeCommand('relayweft.start');
    assert.ok(hooks.running(), 'no session after start: ' + hooks.log.join('\n'));
    const pid = hooks.rwPid()!;
    assert.ok(pid && alive(pid));
    const s = await waitFor('the first state', () => st());
    assert.strictEqual(path.resolve(s.dir).toLowerCase(), path.resolve(REPO).toLowerCase());
    assert.strictEqual(s.demo, false);
    await waitFor('the event stream', () => hooks.tree.connected);
    const root = hooks.tree.getChildren();
    assert.strictEqual(root[0].kind, 'status');
    const status = hooks.tree.getTreeItem(root[0]);
    assert.strictEqual(status.description, 'idle');
    // Starting again does not start a second rw.
    await vscode.commands.executeCommand('relayweft.start');
    assert.strictEqual(hooks.rwPid(), pid);
    assert.match(ui.said.at(-1)?.text ?? '', /already running/);
  });

  it('opens the web UI with a single-use link', async () => {
    await vscode.commands.executeCommand('relayweft.openInBrowser');
    assert.match(ui.opened.pop() ?? '', /^http:\/\/127\.0\.0\.1:\d+\/#b=[0-9a-f]{64}$/);
  });

  it('runs a task; the plan waits and shows in the agent tree', async () => {
    ui.inputs.push(TASK);
    await vscode.commands.executeCommand('relayweft.runTask');
    assert.match(ui.said.find((m) => m.text.startsWith('Relayweft: ') && /start|queued|running/i.test(m.text))?.text ?? '', /Relayweft: /);
    const plan = await waitFor('a plan approval', () => st()?.approvals?.find((a) => a.type === 'plan'));
    assert.strictEqual(plan.plan?.subtasks.length, 2);
    await waitFor('the tree to show it', () => {
      const root = hooks.tree.getChildren();
      const ap = root.find((r) => r.kind === 'approvals');
      const kids = ap ? hooks.tree.getChildren(ap) : [];
      return kids.length === 1 && /Approve plan: 2 subtask/.test(String(hooks.tree.getTreeItem(kids[0]).label)) && root.some((r) => r.kind === 'agent');
    });
    // The notification offers to review it.
    assert.ok(ui.said.some((m) => /approve the plan \(2 subtasks\)/.test(m.text) && m.items.includes('Review Plan')));
  });

  it('edits the plan as JSON and approves the edited plan', async () => {
    ui.picks.push(byLabel(/Edit the plan/));
    await vscode.commands.executeCommand('relayweft.approvePlan');
    const ed = await waitFor('the plan editor', () => {
      const e = vscode.window.activeTextEditor;
      return e && e.document.uri.scheme === 'untitled' && e.document.getText().includes(PLAN_TEXT) ? e : undefined;
    });
    assert.strictEqual(ed.document.languageId, 'json');
    const text = ed.document.getText();
    const at = text.indexOf(PLAN_TEXT);
    const ok = await ed.edit((b) => b.replace(new vscode.Range(ed.document.positionAt(at), ed.document.positionAt(at + PLAN_TEXT.length)), EDITED_PLAN_TEXT));
    assert.ok(ok);
    await vscode.commands.executeCommand('relayweft.plan.approveEdited');
    await waitFor('the plan to be answered', () => !st()?.approvals?.some((a) => a.type === 'plan'));
    assert.match(ui.said.map((m) => m.text).join('\n'), /plan approved: 2 subtasks/);
    // The plan document was closed without a save prompt.
    await waitFor('the plan editor to close', () => !vscode.workspace.textDocuments.some((d) => d.uri.toString() === ed.document.uri.toString() && !d.isClosed && vscode.window.visibleTextEditors.some((e) => e.document === d)));
  });

  it('shows the agents in the tree while they work', async () => {
    const kids = await waitFor('two subtask agents under main', () => {
      const main = hooks.tree.getChildren().find((r): r is Extract<AgentsItem, { kind: 'agent' }> => r.kind === 'agent');
      const ks = main ? hooks.tree.getChildren(main).filter((k) => k.kind === 'agent') : [];
      return ks.length >= 2 ? ks : undefined;
    });
    const items = kids.map((k) => hooks.tree.getTreeItem(k));
    assert.ok(items.some((i) => /edit the notes/.test(String(i.description))), items.map((i) => `${i.label} ${i.description}`).join(' | '));
    assert.ok(items.every((i) => /^agent\./.test(i.contextValue ?? '')));
  });

  it('reviews hunks in the diff editor and applies only the accepted ones', async () => {
    const req = await waitFor('a change review', () => st()?.approvals?.find((a) => a.type === 'changes'), 120_000);
    const files = req.changes!.files.map((f) => f.path).sort();
    assert.deepStrictEqual(files, ['added.txt', NOTES]);
    const notes = req.changes!.files.find((f) => f.path === NOTES)!;
    assert.ok(notes.splittable, 'notes.txt should be splittable');
    assert.strictEqual(notes.hunks?.length, 2);
    await waitFor('the review view', () => hooks.review.pendingIds.includes(req.id));

    ui.picks.push(byLabel(new RegExp('^' + NOTES.replace('.', '\\.') + '$')));
    await vscode.commands.executeCommand('relayweft.reviewChanges');
    const ed = await waitFor('the diff editor', () => {
      const e = vscode.window.activeTextEditor;
      return e && e.document.uri.scheme === 'relayweft-review' && /\/after\//.test(e.document.uri.path) ? e : undefined;
    });
    // The whole file, not only the hunks (the base is the file on disk).
    const tab = vscode.window.tabGroups.activeTabGroup.activeTab;
    assert.ok(tab && tab.input instanceof vscode.TabInputTextDiff, 'not a diff tab');
    assert.doesNotMatch(tab.label, /hunks only/, 'the diff fell back to hunks only: ' + tab.label);
    const after = ed.document.getText().replace(/\r\n/g, '\n').split('\n');
    assert.strictEqual(after[EDITED_LINES[0] - 1], edited(EDITED_LINES[0]));
    assert.strictEqual(after[EDITED_LINES[1] - 1], edited(EDITED_LINES[1]));
    const before = await vscode.workspace.openTextDocument((tab.input as vscode.TabInputTextDiff).original);
    assert.strictEqual(before.getText().replace(/\r\n/g, '\n'), origNotes.join('\n') + '\n');

    const setFile = () => hooks.review.getChildren().find((r: ReviewItem) => r.kind === 'set')!;
    // Reject the second hunk with the cursor in it, toggle the first off and on.
    const line2 = EDITED_LINES[1] - 1;
    ed.selection = new vscode.Selection(line2, 0, line2, 0);
    await vscode.commands.executeCommand('relayweft.review.rejectHunk');
    assert.match(String(setFile().description), /2 of 2 file\(s\), 1 partly/);
    const line1 = EDITED_LINES[0] - 1;
    ed.selection = new vscode.Selection(line1, 0, line1, 0);
    await vscode.commands.executeCommand('relayweft.review.rejectHunk');
    assert.match(String(setFile().description), /1 of 2 file\(s\)/);
    await vscode.commands.executeCommand('relayweft.review.acceptHunk');
    assert.match(String(setFile().description), /2 of 2 file\(s\), 1 partly/);
    // The Review view's hunk checkboxes agree.
    const fileItem = hooks.review.getChildren(setFile()).find((f) => f.label === NOTES)!;
    const hunkStates = hooks.review.getChildren(fileItem).map((h) => h.checkboxState);
    assert.deepStrictEqual(hunkStates, [vscode.TreeItemCheckboxState.Checked, vscode.TreeItemCheckboxState.Unchecked]);
    // CodeLens over the hunks says the same.
    const lenses = await vscode.commands.executeCommand<vscode.CodeLens[]>('vscode.executeCodeLensProvider', ed.document.uri);
    const titles = lenses.map((l) => l.command?.title ?? '');
    assert.ok(titles.some((t) => /hunk 1\/2 accepted/.test(t)) && titles.some((t) => /hunk 2\/2 rejected/.test(t)), titles.join(' | '));

    await vscode.commands.executeCommand('relayweft.review.submit');
    assert.match(ui.said.map((m) => m.text).join('\n'), /applying 2 file\(s\), 1 of them partly/);
    await waitFor('the review to end', () => !hooks.review.pendingIds.length);
    const s = await waitFor('the task to finish', () => (st() && !st()!.running && st()!.last ? st() : undefined), 120_000);
    assert.ok(s.last!.ok, 'task failed: ' + s.last!.text);

    const want = [...origNotes];
    want[EDITED_LINES[0] - 1] = edited(EDITED_LINES[0]);
    assert.strictEqual(readRepo(NOTES), want.join('\n') + '\n', 'only the accepted hunk is in notes.txt');
    assert.strictEqual(readRepo('added.txt'), EDITED_PLAN_TEXT + '\n', 'the edited plan reached the agent');
    // The answered review's diff closed; a document of it says it ended.
    assert.ok(!reviewTabs().length, 'the review diff is still open');
    const doc = await vscode.workspace.openTextDocument(ed.document.uri);
    await waitFor('the document to say the review ended', () => /review has ended/.test(doc.getText()), 10_000);
  });

  it('undoes the last task after showing what changes', async () => {
    ui.answer = (m) => (m.modal && /Undo the last Relayweft task/.test(m.text) ? 'Undo' : undefined);
    try {
      await vscode.commands.executeCommand('relayweft.undoLastTask');
    } finally {
      ui.answer = () => undefined;
    }
    const asked = ui.said.find((m) => m.modal && /Undo the last Relayweft task/.test(m.text));
    assert.ok(asked, 'no undo confirmation: ' + ui.said.slice(-3).map((m) => m.text).join(' | '));
    assert.strictEqual(readRepo(NOTES), origNotes.join('\n') + '\n');
    assert.strictEqual(readRepo('added.txt'), undefined);
    assert.ok(!errors().some((e) => /undo failed/.test(e)), errors().join(' | '));
  });

  it('follows up with the agent that edited', async () => {
    const editCall = calls().find((c) => c.what.startsWith('edit '));
    assert.ok(editCall, 'no edit call');
    const n = calls().length;
    ui.picks.push((items) => {
      const it = items.find((i) => i.label === '@edit');
      if (!it) {
        throw new Error('no @edit agent in ' + items.map((i) => `${i.label} ${i.detail ?? ''}`).join(' | '));
      }
      return it;
    });
    ui.inputs.push('please also double-check line three');
    await vscode.commands.executeCommand('relayweft.followUp');
    const fu = await waitFor('the follow-up call', () => calls().slice(n).find((c) => c.prompt.includes('double-check line three')));
    // rw resumes the edit agent's session in the folder it ran in: the repo,
    // or its pool worktree (ROADMAP 2.5; Claude keys sessions by folder).
    assert.strictEqual(fu.resumed, editCall.sid, 'the follow-up did not resume the session of the edit agent');
    assert.strictEqual(path.resolve(fu.cwd).toLowerCase(), path.resolve(editCall.cwd).toLowerCase(), 'the follow-up ran in another folder');
    // (The fake call above means the follow-up task runs.)
    await waitFor('the follow-up to finish', () => st() && !st()!.running && hooks.model.nodes.get('edit')?.status === 'ok');
    assert.match(hooks.model.taskText, /follow-up to edit/);
  });

  it('sends changes back with feedback, then rejects them all', async () => {
    ui.inputs.push(TASK + ' (second round)');
    await vscode.commands.executeCommand('relayweft.runTask');
    await waitFor('a plan approval', () => st()?.approvals?.find((a) => a.type === 'plan'));
    ui.picks.push(byLabel(/Approve$/));
    await vscode.commands.executeCommand('relayweft.approvePlan');
    const first = await waitFor('a change review', () => st()?.approvals?.find((a) => a.type === 'changes'), 120_000);
    assert.strictEqual(first.changes!.round, 1);
    await waitFor('the review view', () => hooks.review.pendingIds.includes(first.id));
    ui.inputs.push('use a friendlier greeting');
    await vscode.commands.executeCommand('relayweft.review.feedback');
    assert.match(ui.said.map((m) => m.text).join('\n'), /sent back to the agent with your feedback/);
    const second = await waitFor('the second round', () => st()?.approvals?.find((a) => a.type === 'changes' && a.id !== first.id), 120_000);
    assert.strictEqual(second.changes!.round, 2);
    assert.ok(calls().some((c) => c.prompt.includes('use a friendlier greeting')), 'the agent did not get the feedback');
    await waitFor('the review view', () => hooks.review.pendingIds.length === 1 && hooks.review.pendingIds[0] === second.id);
    ui.answer = (m) => (m.modal && /Reject every change/.test(m.text) ? 'Reject All' : undefined);
    try {
      await vscode.commands.executeCommand('relayweft.review.rejectAll');
    } finally {
      ui.answer = () => undefined;
    }
    assert.match(ui.said.map((m) => m.text).join('\n'), /all changes rejected/);
    await waitFor('the task to finish', () => (st() && !st()!.running && st()!.last ? st() : undefined), 120_000);
    assert.strictEqual(readRepo(NOTES), origNotes.join('\n') + '\n', 'rejected changes must not land');
    assert.strictEqual(readRepo('added.txt'), undefined);
  });

  it('rejects a plan from the plan editor (the task is cancelled)', async () => {
    const n = calls().length;
    ui.inputs.push(TASK + ' (third time)');
    await vscode.commands.executeCommand('relayweft.runTask');
    await waitFor('a plan approval', () => st()?.approvals?.find((a) => a.type === 'plan'));
    ui.picks.push(byLabel(/Edit the plan/));
    await vscode.commands.executeCommand('relayweft.approvePlan');
    const ed = await waitFor('the plan editor', () => {
      const e = vscode.window.activeTextEditor;
      return e && e.document.uri.scheme === 'untitled' && e.document.getText().includes(PLAN_TEXT) ? e : undefined;
    });
    const lenses = await vscode.commands.executeCommand<vscode.CodeLens[]>('vscode.executeCodeLensProvider', ed.document.uri);
    const reject = lenses.find((l) => /Reject/.test(l.command?.title ?? ''));
    assert.ok(reject?.command, 'no Reject CodeLens: ' + lenses.map((l) => l.command?.title).join(' | '));
    ui.answer = (m) => (m.modal && /Reject the plan/.test(m.text) ? 'Reject' : undefined);
    try {
      await vscode.commands.executeCommand(reject.command.command, ...(reject.command.arguments ?? []));
    } finally {
      ui.answer = () => undefined;
    }
    await waitFor('the task to end', () => (st() && !st()!.running && st()!.last ? st() : undefined), 60_000);
    assert.ok(!calls().slice(n).some((c) => c.what.startsWith('edit') || c.what === 'explore'), 'an agent ran after the plan was rejected');
    assert.ok(!vscode.window.visibleTextEditors.some((e) => e.document.uri.toString() === ed.document.uri.toString()), 'the plan editor is still open');
  });

  it('cancels a running task (its agent is killed)', async () => {
    fs.rmSync(path.join(STATE, 'hang.pid'), { force: true });
    ui.inputs.push(HANG + ' wait here');
    await vscode.commands.executeCommand('relayweft.runTask');
    const pid = await waitFor('the hanging agent', () => {
      const f = path.join(STATE, 'hang.pid');
      return fs.existsSync(f) ? Number(fs.readFileSync(f, 'utf8')) : undefined;
    });
    await waitFor('the task to run', () => st()?.running);
    await vscode.commands.executeCommand('relayweft.cancelTask');
    await waitFor('the task to stop', () => st() && !st()!.running, 30_000);
    await waitFor('the agent process to exit', () => !alive(pid), 15_000);
    assert.ok(hooks.running(), 'rw itself keeps running');
  });

  it('stops rw: no rw or agent process is left', async () => {
    fs.rmSync(path.join(STATE, 'hang.pid'), { force: true });
    ui.inputs.push(HANG + ' and again');
    await vscode.commands.executeCommand('relayweft.runTask');
    const agent = await waitFor('the hanging agent', () => {
      const f = path.join(STATE, 'hang.pid');
      return fs.existsSync(f) ? Number(fs.readFileSync(f, 'utf8')) : undefined;
    });
    const rw = hooks.rwPid()!;
    await vscode.commands.executeCommand('relayweft.stop');
    assert.strictEqual(hooks.running(), false);
    assert.strictEqual(alive(rw), false, 'rw still runs after stop');
    await waitFor('the agent to exit with rw', () => !alive(agent), 15_000);
    assert.strictEqual(hooks.tree.connected, false);
    assert.ok(!ui.said.some((m) => /stopped unexpectedly/.test(m.text)), 'a requested stop is not a crash');
  });

  it('notices when rw dies, and starts again', async () => {
    await vscode.commands.executeCommand('relayweft.start');
    const pid = hooks.rwPid()!;
    await waitFor('the event stream', () => hooks.tree.connected);
    let restart = false;
    ui.answer = (m) => {
      if (/stopped unexpectedly/.test(m.text) && !restart) {
        restart = true;
        return 'Restart';
      }
      return undefined;
    };
    process.kill(pid); // TerminateProcess on Windows: a crash
    await waitFor('the crash message', () => ui.said.some((m) => m.level === 'error' && /rw stopped unexpectedly/.test(m.text)));
    // "Restart" starts a new rw.
    const again = await waitFor('the restarted rw', () => (hooks.running() && hooks.rwPid() !== pid ? hooks.rwPid() : undefined), 30_000);
    assert.ok(alive(again));
    await waitFor('the event stream again', () => hooks.tree.connected);
    ui.answer = () => undefined;
    await vscode.commands.executeCommand('relayweft.stop');
    assert.strictEqual(alive(again), false);
  });

  it('explains a relayweft.path that is not rw', async () => {
    const cfg = vscode.workspace.getConfiguration('relayweft');
    await cfg.update('path', path.join(REPO, 'no-such-rw.exe'), vscode.ConfigurationTarget.Global);
    try {
      await vscode.commands.executeCommand('relayweft.start');
      assert.strictEqual(hooks.running(), false);
      assert.match(errors().pop() ?? '', /is not an executable file/);
    } finally {
      await cfg.update('path', RW, vscode.ConfigurationTarget.Global);
    }
  });

  it('leaves rw running with a working agent for the shutdown check', async () => {
    // runTest.ts checks after VS Code exits that closing the window took
    // rw and its agent along (deactivate, or rw's stdin closing).
    await vscode.commands.executeCommand('relayweft.start');
    fs.rmSync(path.join(STATE, 'hang.pid'), { force: true });
    ui.inputs.push(HANG + ' until the window closes');
    await vscode.commands.executeCommand('relayweft.runTask');
    const agent = await waitFor('the hanging agent', () => {
      const f = path.join(STATE, 'hang.pid');
      return fs.existsSync(f) ? Number(fs.readFileSync(f, 'utf8')) : undefined;
    });
    fs.writeFileSync(path.join(STATE, 'shutdown.json'), JSON.stringify({ rw: hooks.rwPid(), agent }));
    leaveRunning = true;
  });
});
