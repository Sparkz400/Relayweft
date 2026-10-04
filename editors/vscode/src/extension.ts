// Switchyard for VS Code: a thin client for `sy web`. The extension starts
// `sy web --client` for a workspace folder, logs in with the bootstrap sy
// prints (like a browser tab), follows the event stream and drives the
// existing JSON API. Everything stays on 127.0.0.1.

import { spawn } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as vscode from 'vscode';
import { SyApi } from './api';
import { clientArgs, locateSy, userSetting } from './locate';
import { logLine, oneLine, parseFollowUp, TaskModel } from './model';
import { PlanFlow } from './plan';
import { ReviewController, ReviewItem, SCHEME } from './review';
import { SyNotFoundError, SyProcess } from './syProcess';
import { AgentsTree, type AgentsItem } from './tree';
import type { ApprovalRequest, SessionRow, StateView, SubmitResult, SyEvent } from './types';

const INSTALL_URL = 'https://github.com/sparkz400/switchyard#install';

interface Session {
  proc: SyProcess;
  api: SyApi;
  folder: vscode.WorkspaceFolder;
  closeStream: () => void;
  stopping: boolean;
}

let ext: Extension | undefined;

/**
 * What the integration tests (src/test/integration) look at. activate
 * returns it only when VS Code runs the extension's tests.
 */
export interface TestHooks {
  readonly model: TaskModel;
  readonly tree: AgentsTree;
  readonly review: ReviewController;
  /** The pid of the sy this window started, while it runs. */
  syPid(): number | undefined;
  /** Whether a sy session is attached (switchyard.running). */
  running(): boolean;
  /** The lines written to the Switchyard output channel. */
  readonly log: string[];
}

export function activate(context: vscode.ExtensionContext): TestHooks | undefined {
  ext = new Extension(context);
  return context.extensionMode === vscode.ExtensionMode.Test ? ext.hooks() : undefined;
}

export function deactivate(): Promise<void> | undefined {
  const e = ext;
  ext = undefined;
  return e?.stop(4000);
}

class Extension {
  private session: Session | undefined;
  private starting = false;
  private readonly channel = vscode.window.createOutputChannel('Switchyard');
  private readonly logLines: string[] = [];
  private readonly out = {
    appendLine: (l: string) => {
      this.channel.appendLine(l);
      if (this.testing) {
        this.logLines.push(l);
      }
    },
    show: (keep?: boolean) => this.channel.show(keep),
  };
  private readonly testing: boolean;
  private readonly model = new TaskModel();
  private readonly tree = new AgentsTree(this.model);
  private readonly review = new ReviewController();
  private readonly plan = new PlanFlow(() => this.session?.api);
  private readonly status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
  private replaying = false;
  private lastLogged = 0; // ts of the newest event written to the log
  private seenApprovals = new Set<string>();
  private refreshTimer: NodeJS.Timeout | undefined;

  constructor(ctx: vscode.ExtensionContext) {
    this.testing = ctx.extensionMode === vscode.ExtensionMode.Test;
    const agentsView = vscode.window.createTreeView('switchyard.agents', { treeDataProvider: this.tree, showCollapseAll: true });
    const reviewView = vscode.window.createTreeView('switchyard.review', { treeDataProvider: this.review, manageCheckboxStateManually: true });
    this.review.view = reviewView;
    this.status.command = 'switchyard.agents.focus';
    const reg = (id: string, fn: (...args: any[]) => unknown) =>
      vscode.commands.registerCommand(id, async (...args: unknown[]) => {
        try {
          await fn(...args);
        } catch (e) {
          void vscode.window.showErrorMessage('Switchyard: ' + (e as Error).message);
        }
      });
    ctx.subscriptions.push(
      this.channel, this.tree, this.review, this.plan, this.status, agentsView, reviewView,
      vscode.workspace.registerTextDocumentContentProvider(SCHEME, this.review),
      vscode.languages.registerCodeLensProvider({ scheme: SCHEME }, this.review),
      vscode.languages.registerCodeLensProvider({ scheme: 'untitled', language: 'json' }, this.plan),
      reviewView.onDidChangeCheckboxState((e) => this.review.onCheckbox(e.items)),
      vscode.workspace.onDidChangeWorkspaceFolders((e) => {
        const s = this.session;
        if (s && e.removed.some((f) => f.uri.toString() === s.folder.uri.toString())) {
          this.out.appendLine('The folder was closed: stopping sy.');
          void this.stop();
        }
      }),
      reg('switchyard.start', () => this.start()),
      reg('switchyard.stop', () => this.stop()),
      reg('switchyard.runTask', () => this.runTask()),
      reg('switchyard.cancelTask', () => this.cancelTask()),
      reg('switchyard.approvePlan', (arg?: AgentsItem) => this.approvePlan(arg)),
      reg('switchyard.answerApproval', (id?: string | AgentsItem) => this.answerApproval(id)),
      reg('switchyard.followUp', (arg?: AgentsItem) => this.followUp(arg)),
      reg('switchyard.undoLastTask', () => this.undoLastTask()),
      reg('switchyard.openInBrowser', () => this.openInBrowser()),
      reg('switchyard.showLog', () => this.out.show(true)),
      reg('switchyard.reviewChanges', (arg?: AgentsItem) => this.review.openReview(arg && 'req' in arg ? arg.req.id : undefined)),
      reg('switchyard.review.openFile', (arg: { id: string; file: number; hunk?: number } | ReviewItem) => this.review.openFile(arg)),
      reg('switchyard.review.acceptHunk', () => this.review.setHunkAtCursor(true)),
      reg('switchyard.review.rejectHunk', () => this.review.setHunkAtCursor(false)),
      reg('switchyard.review.toggleHunk', (arg: { id: string; file: number; hunk: number }) => this.review.toggleHunk(arg)),
      reg('switchyard.review.acceptFile', (arg?: { id: string; file: number } | ReviewItem) => this.review.setWholeFile(arg, true)),
      reg('switchyard.review.rejectFile', (arg?: { id: string; file: number } | ReviewItem) => this.review.setWholeFile(arg, false)),
      reg('switchyard.review.submit', (arg?: unknown) => this.review.submit(arg)),
      reg('switchyard.review.rejectAll', (arg?: unknown) => this.review.rejectAll(arg)),
      reg('switchyard.review.feedback', (arg?: unknown) => this.review.feedback(arg)),
      reg('switchyard.plan.approveEdited', (uri?: vscode.Uri) => this.plan.approveEdited(uri)),
      reg('switchyard.plan.reject', (uri?: vscode.Uri) => this.plan.rejectEdited(uri)),
    );
    this.setContext();
  }

  hooks(): TestHooks {
    return {
      model: this.model,
      tree: this.tree,
      review: this.review,
      syPid: () => this.session?.proc.pid,
      running: () => !!this.session,
      log: this.logLines,
    };
  }

  // --- process -------------------------------------------------------------------

  private async pickFolder(): Promise<vscode.WorkspaceFolder | undefined> {
    const folders = vscode.workspace.workspaceFolders ?? [];
    if (folders.length === 0) {
      void vscode.window.showErrorMessage('Switchyard: open a folder first (sy works in a project folder).');
      return undefined;
    }
    if (folders.length === 1) {
      return folders[0];
    }
    return vscode.window.showWorkspaceFolderPick({ placeHolder: 'Run Switchyard in which folder?' });
  }

  /** The sy executable, or undefined after telling the person how to fix it. */
  private findSy(): string | undefined {
    // Only the user's own value: a repository's settings must not pick the
    // program sy is (userSetting).
    const configured = userSetting(vscode.workspace.getConfiguration('switchyard').inspect<string>('path'), '');
    const found = locateSy(configured, {
      platform: process.platform,
      env: process.env,
      home: os.homedir(),
      isFile: (p) => {
        try {
          return fs.statSync(p).isFile();
        } catch {
          return false;
        }
      },
    });
    if (found.path) {
      return found.path;
    }
    this.out.appendLine('sy was not found. Looked at:\n  ' + found.tried.join('\n  '));
    const what = configured
      ? `Switchyard: "${configured}" (setting switchyard.path) is not an executable file.`
      : `Switchyard: could not find sy${process.platform === 'win32' ? '.exe' : ''} on PATH. Install Switchyard, or set switchyard.path to the sy executable.`;
    void vscode.window.showErrorMessage(what, 'Open Settings', 'How to Install', 'Show Details').then((a) => {
      if (a === 'Open Settings') {
        void vscode.commands.executeCommand('workbench.action.openSettings', 'switchyard.path');
      } else if (a === 'How to Install') {
        void vscode.env.openExternal(vscode.Uri.parse(INSTALL_URL));
      } else if (a === 'Show Details') {
        this.out.show(true);
      }
    });
    return undefined;
  }

  async start(): Promise<void> {
    if (this.session) {
      void vscode.window.showInformationMessage(`Switchyard is already running for ${this.session.folder.name}.`);
      await vscode.commands.executeCommand('switchyard.agents.focus');
      return;
    }
    if (this.starting) {
      return;
    }
    const folder = await this.pickFolder();
    if (!folder) {
      return;
    }
    if (folder.uri.scheme !== 'file') {
      void vscode.window.showErrorMessage('Switchyard: the folder must be on this machine (a file: folder).');
      return;
    }
    const exe = this.findSy();
    if (!exe) {
      return;
    }
    // As with switchyard.path, workspace values are ignored (userSetting).
    const extra = userSetting(vscode.workspace.getConfiguration('switchyard').inspect<string[]>('args'), []);
    const dir = folder.uri.fsPath;
    const args = clientArgs(dir, Array.isArray(extra) ? extra : []);
    this.starting = true;
    this.out.appendLine(`starting: ${exe} ${args.map((a) => JSON.stringify(a)).join(' ')}`);
    try {
      await vscode.window.withProgress({ location: vscode.ProgressLocation.Window, title: 'Starting Switchyard…' }, async () => {
        const proc = await SyProcess.start(exe, args, dir, (l) => this.out.appendLine('sy: ' + l));
        const api = new SyApi(proc.hello.url, async () => {
          const r = await proc.request('bootstrap');
          return r.bootstrap;
        });
        try {
          await api.login(proc.hello.bootstrap);
        } catch (e) {
          await proc.stop(2000);
          throw e;
        }
        const s: Session = { proc, api, folder, closeStream: () => undefined, stopping: false };
        this.session = s;
        this.out.appendLine(`Switchyard ${proc.hello.version} for ${proc.hello.dir} on ${proc.hello.url} (pid ${proc.hello.pid})${proc.hello.demo ? ' — demo mode' : ''}`);
        proc.onExit((code, signal) => {
          if (this.session !== s) {
            return;
          }
          this.detach();
          if (!s.stopping) {
            const why = signal ? `signal ${signal}` : `code ${String(code)}`;
            this.out.appendLine(`sy stopped unexpectedly (${why}).`);
            void vscode.window.showErrorMessage(`Switchyard: sy stopped unexpectedly (${why}).`, 'Show Log', 'Restart').then((a) => {
              if (a === 'Show Log') {
                this.out.show(true);
              } else if (a === 'Restart') {
                void vscode.commands.executeCommand('switchyard.start');
              }
            });
          }
        });
        this.review.attach(api, dir);
        s.closeStream = api.stream((m) => this.onMessage(m.event, m.data), (ok, err) => this.onStream(ok, err));
        this.setContext();
      });
      await vscode.commands.executeCommand('switchyard.agents.focus');
    } catch (e) {
      const msg = (e as Error).message;
      this.out.appendLine('could not start sy: ' + msg);
      if (e instanceof SyNotFoundError) {
        this.findSy();
      } else {
        void vscode.window.showErrorMessage('Switchyard: ' + msg, 'Show Log').then((a) => a && this.out.show(true));
      }
    } finally {
      this.starting = false;
    }
  }

  /** Stops sy (closing its stdin lets it cancel the task and stop agents). */
  async stop(graceMs = 10_000): Promise<void> {
    const s = this.session;
    if (!s) {
      return;
    }
    s.stopping = true;
    this.detach();
    this.out.appendLine('stopping sy…');
    await s.proc.stop(graceMs);
    this.out.appendLine('sy stopped.');
  }

  private detach(): void {
    const s = this.session;
    if (!s) {
      return;
    }
    s.closeStream();
    this.session = undefined;
    this.model.reset();
    this.model.state = undefined;
    this.tree.connected = false;
    this.review.attach(undefined, undefined);
    this.plan.sync([]);
    this.seenApprovals.clear();
    this.lastLogged = 0;
    this.setContext();
    this.tree.refresh();
  }

  private setContext(): void {
    const st = this.model.state;
    void vscode.commands.executeCommand('setContext', 'switchyard.running', !!this.session);
    void vscode.commands.executeCommand('setContext', 'switchyard.taskRunning', !!st?.running);
    if (!this.session) {
      this.status.hide();
      return;
    }
    const pending = st?.approvals?.length ?? 0;
    if (pending) {
      this.status.text = `$(bell-dot) Switchyard: ${pending} waiting`;
      this.status.backgroundColor = new vscode.ThemeColor('statusBarItem.warningBackground');
    } else {
      this.status.text = st?.running ? `$(sync~spin) Switchyard: ${st.phase}` : '$(circle-large-outline) Switchyard';
      this.status.backgroundColor = undefined;
    }
    this.status.tooltip = st?.task ? 'Switchyard: ' + oneLine(st.task, 200) : 'Switchyard: idle';
    this.status.show();
  }

  // --- event stream ---------------------------------------------------------------

  private onStream(connected: boolean, err?: string): void {
    this.tree.connected = connected;
    this.tree.error = err ?? '';
    if (!connected && err) {
      this.out.appendLine('event stream: ' + err + ' (reconnecting)');
    }
    this.scheduleRefresh();
  }

  private onMessage(name: string, data: string): void {
    let v: unknown;
    try {
      v = JSON.parse(data);
    } catch {
      return;
    }
    switch (name) {
      case 'state':
        this.onState(v as StateView);
        return;
      case 'reset':
        this.replaying = true;
        this.model.reset();
        break;
      case 'synced':
        this.replaying = false;
        break;
      case 'ev': {
        const e = v as SyEvent;
        const line = this.model.fold(e);
        const ts = Date.parse(e.ts) || 0;
        // A reconnect replays what the log already shows: skip it.
        if (line !== undefined && (!this.replaying || ts > this.lastLogged)) {
          this.out.appendLine(logLine(e, line));
        }
        if (ts > this.lastLogged) {
          this.lastLogged = ts;
        }
        break;
      }
      case 'notice': {
        const n = v as { level: string; text: string };
        if (n.level === 'warn' || n.level === 'error') {
          void vscode.window.setStatusBarMessage('Switchyard: ' + n.text, 6000);
        }
        return;
      }
    }
    this.scheduleRefresh();
  }

  private onState(st: StateView): void {
    this.model.state = st;
    const approvals = st.approvals ?? [];
    this.review.sync(approvals);
    this.plan.sync(approvals);
    this.notifyApprovals(approvals);
    this.setContext();
    this.scheduleRefresh();
  }

  private scheduleRefresh(): void {
    if (this.refreshTimer) {
      return;
    }
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = undefined;
      this.tree.refresh();
    }, 120);
  }

  private notifyApprovals(approvals: ApprovalRequest[]): void {
    const live = new Set(approvals.map((a) => a.id));
    for (const id of [...this.seenApprovals]) {
      if (!live.has(id)) {
        this.seenApprovals.delete(id);
      }
    }
    if (!vscode.workspace.getConfiguration('switchyard').get<boolean>('notifyApprovals', true)) {
      approvals.forEach((a) => this.seenApprovals.add(a.id));
      return;
    }
    for (const a of approvals) {
      if (this.seenApprovals.has(a.id)) {
        continue;
      }
      this.seenApprovals.add(a.id);
      let msg = '';
      let action = '';
      switch (a.type) {
        case 'plan':
          msg = `Switchyard: approve the plan (${a.plan?.subtasks.length ?? 0} subtasks) for "${oneLine(a.task, 80)}"`;
          action = 'Review Plan';
          break;
        case 'changes':
          msg = `Switchyard: review the changes of ${a.changes?.step_id ?? ''} (${a.changes?.files.length ?? 0} file(s))`;
          action = 'Review Changes';
          break;
        case 'budget':
          msg = 'Switchyard: ' + (a.budget?.text ?? 'budget reached');
          action = 'Decide';
          break;
      }
      void vscode.window.showInformationMessage(msg, action).then((x) => {
        if (x) {
          void this.answerApproval(a.id);
        }
      });
    }
  }

  // --- commands -------------------------------------------------------------------

  private need(): Session {
    if (!this.session) {
      throw new Error('Switchyard is not running: run "Switchyard: Start" first.');
    }
    return this.session;
  }

  /** Commands that need sy start it first (after asking). */
  private async ensure(): Promise<Session | undefined> {
    if (!this.session) {
      const a = await vscode.window.showInformationMessage('Switchyard is not running for this workspace.', 'Start');
      if (a === 'Start') {
        await this.start();
      }
    }
    return this.session;
  }

  private async runTask(): Promise<void> {
    const s = await this.ensure();
    if (!s) {
      return;
    }
    const text = await vscode.window.showInputBox({
      title: 'Switchyard: run a task',
      prompt: this.model.state?.running
        ? 'A task is running: this one is queued and runs unattended after it. "@agent message" follows up.'
        : 'Describe the task. "@agent message" follows up with a finished agent.',
      placeHolder: 'e.g. add a --strict flag to the parser, with tests',
      ignoreFocusOut: true,
    });
    if (!text || !text.trim()) {
      return;
    }
    await this.submit(s, text.trim());
  }

  private async submit(s: Session, text: string): Promise<void> {
    const r = await s.api.call<SubmitResult>('POST', '/api/task', { text });
    void vscode.window.showInformationMessage('Switchyard: ' + r.message);
  }

  private async cancelTask(): Promise<void> {
    const s = this.need();
    const r = await s.api.call<{ message: string }>('POST', '/api/cancel');
    void vscode.window.showInformationMessage('Switchyard: ' + r.message);
  }

  private approvals(type?: ApprovalRequest['type']): ApprovalRequest[] {
    return (this.model.state?.approvals ?? []).filter((a) => !type || a.type === type);
  }

  private async approvePlan(arg?: AgentsItem): Promise<void> {
    this.need();
    if (arg && arg.kind === 'approval' && arg.req.type === 'plan') {
      await this.plan.ask(arg.req);
      return;
    }
    const plans = this.approvals('plan');
    if (!plans.length) {
      void vscode.window.showInformationMessage('Switchyard: no plan is waiting for approval.');
      return;
    }
    let req = plans[0];
    if (plans.length > 1) {
      const pick = await vscode.window.showQuickPick(plans.map((p) => ({ label: oneLine(p.plan?.summary || p.task, 100), req: p })), { placeHolder: 'Which plan?' });
      if (!pick) {
        return;
      }
      req = pick.req;
    }
    await this.plan.ask(req);
  }

  private async answerApproval(arg?: string | AgentsItem): Promise<void> {
    const id = typeof arg === 'string' ? arg : arg && arg.kind === 'approval' ? arg.req.id : undefined;
    const req = this.approvals().find((a) => a.id === id);
    if (!req) {
      void vscode.window.showInformationMessage('Switchyard: that question is no longer waiting.');
      return;
    }
    switch (req.type) {
      case 'plan':
        await this.plan.ask(req);
        break;
      case 'changes':
        await this.review.openReview(req.id);
        break;
      case 'budget':
        await this.answerBudget(req);
        break;
    }
  }

  private async answerBudget(req: ApprovalRequest): Promise<void> {
    const s = this.need();
    const pick = await vscode.window.showQuickPick(
      [
        { label: '$(debug-continue) Go on', description: 'past the limit, until this task ends', ok: true },
        { label: '$(debug-stop) Stop the task', description: 'finished work is kept', ok: false },
      ],
      { title: 'Switchyard: ' + (req.budget?.text ?? 'budget reached'), placeHolder: req.budget?.hint, ignoreFocusOut: true },
    );
    if (!pick) {
      return;
    }
    const r = await s.api.call<{ message: string }>('POST', `/api/approvals/${encodeURIComponent(req.id)}/budget`, { ok: pick.ok });
    void vscode.window.showInformationMessage('Switchyard: ' + r.message);
  }

  private async followUp(arg?: AgentsItem): Promise<void> {
    const s = this.need();
    let agent: string | undefined = arg && arg.kind === 'agent' ? arg.id : undefined;
    if (agent === undefined) {
      const rows = await s.api.call<SessionRow[]>('GET', '/api/sessions');
      const items = [
        { label: '@ (newest agent)', description: 'the agent that finished last', agent: '' },
        ...rows.map((r) => ({
          label: '@' + r.agent,
          description: [r.running ? 'running: the message is delivered when its turn ends' : r.role, r.provider && `${r.provider}:${r.model ?? ''}`].filter(Boolean).join(' · '),
          detail: r.title ? oneLine(r.title, 120) : undefined,
          agent: r.agent,
        })),
      ];
      const pick = await vscode.window.showQuickPick(items, { placeHolder: 'Follow up with which agent? (it keeps its session and context)' });
      if (!pick) {
        return;
      }
      agent = pick.agent;
    }
    const msg = await vscode.window.showInputBox({
      title: `Switchyard: follow up with ${agent ? '@' + agent : 'the newest agent'}`,
      prompt: 'Your message to the agent',
      ignoreFocusOut: true,
    });
    if (!msg || !msg.trim()) {
      return;
    }
    const text = `@${agent} ${msg.trim()}`;
    if (!parseFollowUp(text)) {
      throw new Error(`"${agent}" is not an agent id`);
    }
    await this.submit(s, text);
  }

  private async openInBrowser(): Promise<void> {
    const s = this.need();
    const r = await s.proc.request('link');
    // The link holds a single-use bootstrap: the browser tab gets its own session.
    await vscode.env.openExternal(vscode.Uri.parse(r.link));
  }

  /** Runs a one-shot sy command (no shell; stdin closed so it never waits). */
  private runSy(exe: string, args: string[], cwd: string): Promise<{ code: number | null; out: string }> {
    return new Promise((resolve, reject) => {
      const child = spawn(exe, args, { cwd, shell: false, windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'] });
      let out = '';
      child.stdout.setEncoding('utf8');
      child.stderr.setEncoding('utf8');
      child.stdout.on('data', (d: string) => (out += d));
      child.stderr.on('data', (d: string) => (out += d));
      child.on('error', reject);
      child.on('close', (code) => resolve({ code, out }));
      child.stdin.on('error', () => undefined);
      child.stdin.end();
      setTimeout(() => child.kill(), 120_000).unref();
    });
  }

  private async undoLastTask(): Promise<void> {
    if (this.model.state?.running) {
      void vscode.window.showWarningMessage('Switchyard: a task is running. Cancel it or wait for it to finish before undoing.');
      return;
    }
    const folder = this.session?.folder ?? (await this.pickFolder());
    const exe = folder && this.findSy();
    if (!folder || !exe) {
      return;
    }
    const dir = folder.uri.fsPath;
    // Without --yes, sy undo prints what it would do and, with no answer
    // on stdin, changes nothing.
    const preview = await this.runSy(exe, ['undo', '--dir', dir], dir);
    const text = preview.out.replace(/\n?Undo these changes\? \[y\/N\]\s*/, '\n').replace(/Nothing changed\.\s*$/, '').trim();
    this.out.appendLine('--- sy undo (preview) ---\n' + text);
    if (preview.code !== 0 || !/^This changes \d+ file/m.test(text)) {
      void vscode.window.showInformationMessage('Switchyard undo: ' + oneLine(text, 300));
      return;
    }
    const detail = text.length > 1800 ? text.slice(0, 1800) + '\n… (see the Switchyard output)' : text;
    const ok = await vscode.window.showWarningMessage('Undo the last Switchyard task?', { modal: true, detail }, 'Undo');
    if (ok !== 'Undo') {
      return;
    }
    const res = await this.runSy(exe, ['undo', '--yes', '--dir', dir], dir);
    this.out.appendLine('--- sy undo ---\n' + res.out.trim());
    if (res.code === 0) {
      const said = oneLine(res.out.split('\n').filter((l) => /done|redo/i.test(l)).join(' '), 200);
      void vscode.window.showInformationMessage('Switchyard: ' + (said || 'undone'));
    } else {
      void vscode.window.showErrorMessage('Switchyard undo failed: ' + oneLine(res.out, 300), 'Show Log').then((a) => a && this.out.show(true));
    }
  }
}
