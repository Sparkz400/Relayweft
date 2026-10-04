// Change review in VS Code's diff editor. For each file of a pending
// change set, a read-only virtual document provider serves "before" and
// "after" documents built from the patch the API sends (applied to the
// file in the workspace, or the hunks alone when the file there differs
// from the agent's base). Hunks are accepted or rejected with CodeLens,
// editor title buttons, commands, or the checkboxes in the Review view,
// and the decision is posted to POST /api/approvals/{id}/changes.

import * as fs from 'fs';
import * as path from 'path';
import * as vscode from 'vscode';
import type { SyApi } from './api';
import {
  decisionBody, fileOn, hunkCount, hunkOn, selectAll, selectionSummary, setFile, setHunk, type Selection,
} from './decision';
import { hunkAt, reconstruct, type Reconstructed } from './patch';
import type { ApprovalRequest, ChangeView, FileView } from './types';

export const SCHEME = 'switchyard-review';

type Side = 'before' | 'after';

interface Review {
  req: ApprovalRequest;
  cv: ChangeView;
  sel: Selection;
  docs: Map<number, Reconstructed>;
}

interface Target {
  id: string;
  file: number;
  side: Side;
}

/** switchyard-review:/<id>/<file index>/<side>/<path> (the path gives the language). */
export function reviewUri(id: string, file: number, side: Side, filePath: string): vscode.Uri {
  const clean = filePath.replace(/\\/g, '/').replace(/^\/+/, '');
  return vscode.Uri.from({ scheme: SCHEME, path: `/${encodeURIComponent(id)}/${file}/${side}/${clean}` });
}

export function parseReviewUri(uri: vscode.Uri): Target | undefined {
  if (uri.scheme !== SCHEME) {
    return undefined;
  }
  const m = /^\/([^/]+)\/(\d+)\/(before|after)\//.exec(uri.path);
  if (!m) {
    return undefined;
  }
  return { id: decodeURIComponent(m[1]), file: Number(m[2]), side: m[3] as Side };
}

// --- tree items ----------------------------------------------------------------

export class ReviewItem extends vscode.TreeItem {
  constructor(
    readonly kind: 'set' | 'file' | 'hunk',
    readonly id: string,
    readonly file: number,
    readonly hunk: number,
    label: string,
    collapsible: vscode.TreeItemCollapsibleState,
  ) {
    super(label, collapsible);
  }
}

export class ReviewController implements vscode.TextDocumentContentProvider, vscode.CodeLensProvider, vscode.TreeDataProvider<ReviewItem> {
  private reviews = new Map<string, Review>();
  private api: SyApi | undefined;
  private folder: string | undefined;

  private readonly docChanged = new vscode.EventEmitter<vscode.Uri>();
  readonly onDidChange = this.docChanged.event;
  private readonly lensChanged = new vscode.EventEmitter<void>();
  readonly onDidChangeCodeLenses = this.lensChanged.event;
  private readonly treeChanged = new vscode.EventEmitter<ReviewItem | undefined>();
  readonly onDidChangeTreeData = this.treeChanged.event;

  private readonly rejectedDeco = vscode.window.createTextEditorDecorationType({
    opacity: '0.45',
    textDecoration: 'line-through',
    isWholeLine: true,
    overviewRulerColor: new vscode.ThemeColor('editorOverviewRuler.deletedForeground'),
    overviewRulerLane: vscode.OverviewRulerLane.Right,
  });
  private readonly disposables: vscode.Disposable[] = [];
  view: vscode.TreeView<ReviewItem> | undefined;

  constructor() {
    this.disposables.push(
      this.docChanged, this.lensChanged, this.treeChanged, this.rejectedDeco,
      vscode.window.onDidChangeVisibleTextEditors(() => this.decorate()),
    );
  }

  dispose(): void {
    for (const d of this.disposables) {
      d.dispose();
    }
  }

  attach(api: SyApi | undefined, folder: string | undefined): void {
    this.api = api;
    this.folder = folder && path.resolve(folder);
    if (!api) {
      this.sync([]);
    }
  }

  get pendingIds(): string[] {
    return [...this.reviews.keys()];
  }

  /** Takes the server's pending approvals; returns the change sets that are new. */
  sync(approvals: ApprovalRequest[]): ApprovalRequest[] {
    const fresh: ApprovalRequest[] = [];
    const live = new Set<string>();
    for (const a of approvals) {
      if (a.type !== 'changes' || !a.changes) {
        continue;
      }
      live.add(a.id);
      if (!this.reviews.has(a.id)) {
        this.reviews.set(a.id, { req: a, cv: a.changes, sel: selectAll(a.changes.files), docs: new Map() });
        fresh.push(a);
      }
    }
    const gone: string[] = [];
    for (const id of [...this.reviews.keys()]) {
      if (!live.has(id)) {
        this.reviews.delete(id);
        gone.push(id);
      }
    }
    this.ended(gone);
    if (fresh.length || gone.length) {
      this.refresh();
      void vscode.commands.executeCommand('setContext', 'switchyard.reviewPending', this.reviews.size > 0);
    }
    return fresh;
  }

  private refresh(): void {
    this.treeChanged.fire(undefined);
    this.lensChanged.fire();
    this.decorate();
    if (this.view) {
      const n = this.reviews.size;
      this.view.badge = n ? { value: n, tooltip: `${n} change set(s) to review` } : undefined;
    }
  }

  /**
   * Open diffs of reviews that are over (answered here or elsewhere, or the
   * task ended) say so instead of still offering their hunks.
   */
  private ended(ids: string[]): void {
    if (!ids.length) {
      return;
    }
    for (const d of vscode.workspace.textDocuments) {
      const t = parseReviewUri(d.uri);
      if (t && ids.includes(t.id)) {
        this.docChanged.fire(d.uri);
      }
    }
  }

  /** Closes the diff tabs of a review (after it was answered here). */
  private async closeTabs(id: string): Promise<void> {
    const tabs = vscode.window.tabGroups.all.flatMap((g) => g.tabs).filter((tab) => {
      const input = tab.input;
      const uri = input instanceof vscode.TabInputTextDiff ? input.modified : input instanceof vscode.TabInputText ? input.uri : undefined;
      return uri !== undefined && parseReviewUri(uri)?.id === id;
    });
    if (tabs.length) {
      await vscode.window.tabGroups.close(tabs, true);
    }
  }

  // --- documents ---------------------------------------------------------------

  private async docFor(r: Review, i: number): Promise<Reconstructed> {
    let d = r.docs.get(i);
    if (!d) {
      const f = r.cv.files[i];
      let disk: string | undefined;
      if (this.folder && f.status !== 'A') {
        const abs = path.resolve(this.folder, f.path);
        // Never read outside the workspace folder.
        if (abs === this.folder || abs.startsWith(this.folder.endsWith(path.sep) ? this.folder : this.folder + path.sep)) {
          disk = await fs.promises.readFile(abs, 'utf8').catch(() => undefined);
        }
      }
      d = reconstruct(f.patch, f.status, disk);
      r.docs.set(i, d);
    }
    return d;
  }

  async provideTextDocumentContent(uri: vscode.Uri): Promise<string> {
    const t = parseReviewUri(uri);
    const r = t && this.reviews.get(t.id);
    if (!t || !r || !r.cv.files[t.file]) {
      return '(this review has ended: the changes were answered, or the task ended)\n';
    }
    const d = await this.docFor(r, t.file);
    return t.side === 'before' ? d.before : d.after;
  }

  // --- code lenses and decorations ---------------------------------------------

  provideCodeLenses(doc: vscode.TextDocument): vscode.CodeLens[] {
    const t = parseReviewUri(doc.uri);
    const r = t && this.reviews.get(t.id);
    if (!t || !r) {
      return [];
    }
    const f = r.cv.files[t.file];
    const d = r.docs.get(t.file);
    if (!f || !d) {
      return [];
    }
    const out: vscode.CodeLens[] = [];
    const top = new vscode.Range(0, 0, 0, 0);
    const files = r.cv.files;
    const on = fileOn(files, r.sel, t.file);
    out.push(new vscode.CodeLens(top, {
      title: on ? '$(check) file applied' : '$(close) file not applied',
      command: on ? 'switchyard.review.rejectFile' : 'switchyard.review.acceptFile',
      arguments: [{ id: t.id, file: t.file }],
      tooltip: on ? 'Reject this whole file' : 'Accept this whole file',
    }));
    out.push(new vscode.CodeLens(top, { title: `$(git-commit) apply: ${selectionSummary(files, r.sel)}`, command: 'switchyard.review.submit', arguments: [t.id] }));
    if (f.splittable) {
      const ranges = t.side === 'after' ? d.ranges : d.beforeRanges;
      const n = hunkCount(f);
      ranges.forEach((rg, j) => {
        if (j >= n) {
          return;
        }
        const line = Math.min(rg.start, Math.max(0, doc.lineCount - 1));
        const hOn = hunkOn(files, r.sel, t.file, j);
        out.push(new vscode.CodeLens(new vscode.Range(line, 0, line, 0), {
          title: hOn ? `$(check) hunk ${j + 1}/${n} accepted · reject` : `$(close) hunk ${j + 1}/${n} rejected · accept`,
          command: 'switchyard.review.toggleHunk',
          arguments: [{ id: t.id, file: t.file, hunk: j }],
        }));
      });
    }
    return out;
  }

  private decorate(): void {
    for (const ed of vscode.window.visibleTextEditors) {
      const t = parseReviewUri(ed.document.uri);
      if (!t) {
        continue;
      }
      const r = this.reviews.get(t.id);
      const d = r?.docs.get(t.file);
      const ranges: vscode.Range[] = [];
      if (r && d) {
        const f = r.cv.files[t.file];
        const rs = t.side === 'after' ? d.ranges : d.beforeRanges;
        if (!fileOn(r.cv.files, r.sel, t.file) && t.side === 'after') {
          ranges.push(new vscode.Range(0, 0, Math.max(0, ed.document.lineCount - 1), 0));
        } else if (f.splittable && t.side === 'after') {
          rs.forEach((rg, j) => {
            if (!hunkOn(r.cv.files, r.sel, t.file, j) && rg.end > rg.start) {
              ranges.push(new vscode.Range(rg.start, 0, rg.end - 1, 0));
            }
          });
        }
      }
      ed.setDecorations(this.rejectedDeco, ranges);
    }
  }

  // --- tree ----------------------------------------------------------------------

  getTreeItem(e: ReviewItem): vscode.TreeItem {
    return e;
  }

  getChildren(e?: ReviewItem): ReviewItem[] {
    if (!e) {
      return [...this.reviews.entries()].map(([id, r]) => {
        const it = new ReviewItem('set', id, -1, -1, `${r.cv.step_id}: ${r.cv.title}`, vscode.TreeItemCollapsibleState.Expanded);
        it.description = `round ${r.cv.round} · ${selectionSummary(r.cv.files, r.sel)}`;
        it.tooltip = new vscode.MarkdownString(`**${escapeMd(r.cv.title)}**\n\n${escapeMd(r.cv.summary ?? '')}`);
        it.iconPath = new vscode.ThemeIcon('git-pull-request');
        it.contextValue = 'reviewSet';
        return it;
      });
    }
    const r = this.reviews.get(e.id);
    if (!r) {
      return [];
    }
    if (e.kind === 'set') {
      return r.cv.files.map((f, i) => {
        const n = hunkCount(f);
        const it = new ReviewItem('file', e.id, i, -1, f.path, n > 0 ? vscode.TreeItemCollapsibleState.Collapsed : vscode.TreeItemCollapsibleState.None);
        const sel = n > 0 ? ` · ${[...r.sel.hunks[i]].length}/${n} hunks` : '';
        it.description = `${statusWord(f)} +${f.added} −${f.deleted}${sel}${f.binary ? ' · binary' : ''}`;
        it.checkboxState = fileOn(r.cv.files, r.sel, i) ? vscode.TreeItemCheckboxState.Checked : vscode.TreeItemCheckboxState.Unchecked;
        it.resourceUri = vscode.Uri.file(f.path);
        it.contextValue = 'reviewFile';
        it.command = { command: 'switchyard.review.openFile', title: 'Open Diff', arguments: [{ id: e.id, file: i }] };
        return it;
      });
    }
    if (e.kind === 'file') {
      const f = r.cv.files[e.file];
      return (f.hunks ?? []).map((h, j) => {
        const head = h.split('\n')[0] ?? '';
        const it = new ReviewItem('hunk', e.id, e.file, j, `hunk ${j + 1}`, vscode.TreeItemCollapsibleState.None);
        it.description = head.replace(/^@@[^@]*@@\s?/, '') || head;
        it.tooltip = new vscode.MarkdownString('```diff\n' + h.slice(0, 4000) + '\n```');
        it.checkboxState = hunkOn(r.cv.files, r.sel, e.file, j) ? vscode.TreeItemCheckboxState.Checked : vscode.TreeItemCheckboxState.Unchecked;
        it.contextValue = 'reviewHunk';
        it.command = { command: 'switchyard.review.openFile', title: 'Open Diff', arguments: [{ id: e.id, file: e.file, hunk: j }] };
        return it;
      });
    }
    return [];
  }

  onCheckbox(changes: readonly (readonly [ReviewItem, vscode.TreeItemCheckboxState])[]): void {
    for (const [it, st] of changes) {
      const r = this.reviews.get(it.id);
      if (!r) {
        continue;
      }
      const on = st === vscode.TreeItemCheckboxState.Checked;
      if (it.kind === 'file') {
        setFile(r.cv.files, r.sel, it.file, on);
      } else if (it.kind === 'hunk') {
        setHunk(r.cv.files, r.sel, it.file, it.hunk, on);
      }
    }
    this.refresh();
  }

  // --- commands --------------------------------------------------------------------

  /** Resolves the review a command is about: an argument, the active diff, or a pick. */
  private async pickReview(arg?: unknown): Promise<string | undefined> {
    if (typeof arg === 'string' && this.reviews.has(arg)) {
      return arg;
    }
    if (arg instanceof ReviewItem && this.reviews.has(arg.id)) {
      return arg.id;
    }
    const t = this.activeTarget();
    if (t && this.reviews.has(t.id)) {
      return t.id;
    }
    const ids = this.pendingIds;
    if (ids.length <= 1) {
      if (!ids.length) {
        void vscode.window.showInformationMessage('Switchyard: no changes are waiting for review.');
      }
      return ids[0];
    }
    const pick = await vscode.window.showQuickPick(
      ids.map((id) => {
        const r = this.reviews.get(id)!;
        return { label: `${r.cv.step_id}: ${r.cv.title}`, description: `${r.cv.files.length} file(s)`, id };
      }),
      { placeHolder: 'Which change set?' },
    );
    return pick?.id;
  }

  private activeTarget(): (Target & { line: number }) | undefined {
    const ed = vscode.window.activeTextEditor;
    const t = ed && parseReviewUri(ed.document.uri);
    return t && ed ? { ...t, line: ed.selection.active.line } : undefined;
  }

  /** Opens the change set: the first file's diff (or the one picked). */
  async openReview(id?: string): Promise<void> {
    const rid = await this.pickReview(id);
    const r = rid && this.reviews.get(rid);
    if (!rid || !r) {
      return;
    }
    await vscode.commands.executeCommand('switchyard.review.focus');
    if (r.cv.files.length === 1) {
      await this.openFile({ id: rid, file: 0 });
      return;
    }
    const pick = await vscode.window.showQuickPick(
      r.cv.files.map((f, i) => ({ label: f.path, description: `${statusWord(f)} +${f.added} −${f.deleted}`, i })),
      { placeHolder: `${r.cv.step_id}: ${r.cv.title} — open which file?` },
    );
    if (pick) {
      await this.openFile({ id: rid, file: pick.i });
    }
  }

  async openFile(arg: { id: string; file: number; hunk?: number } | ReviewItem): Promise<void> {
    const id = arg.id;
    const i = arg.file;
    const hunk = arg instanceof ReviewItem ? (arg.hunk >= 0 ? arg.hunk : undefined) : arg.hunk;
    const r = this.reviews.get(id);
    const f = r?.cv.files[i];
    if (!r || !f) {
      return;
    }
    if (f.binary) {
      void vscode.window.showInformationMessage(`${f.path} is a binary file: accept or reject it as a whole in the Review view.`);
      return;
    }
    const d = await this.docFor(r, i);
    const left = reviewUri(id, i, 'before', f.path);
    const right = reviewUri(id, i, 'after', f.path);
    const rg = hunk !== undefined ? d.ranges[hunk] : d.ranges[0];
    const opts: vscode.TextDocumentShowOptions = { preview: false };
    if (rg) {
      opts.selection = new vscode.Range(rg.start, 0, rg.start, 0);
    }
    const note = d.whole ? '' : ' (hunks only)';
    await vscode.commands.executeCommand('vscode.diff', left, right, `${path.basename(f.path)} — ${r.cv.step_id} review${note}`, opts);
    if (!d.whole) {
      void vscode.window.setStatusBarMessage('Switchyard: the file in your folder is not the agent\'s base (or the patch is truncated): showing the hunks only', 6000);
    }
    this.decorate();
  }

  private change(fn: () => void): void {
    fn();
    this.refresh();
  }

  toggleHunk(arg: { id: string; file: number; hunk: number }): void {
    const r = this.reviews.get(arg.id);
    if (!r) {
      return;
    }
    this.change(() => setHunk(r.cv.files, r.sel, arg.file, arg.hunk, !hunkOn(r.cv.files, r.sel, arg.file, arg.hunk)));
  }

  /** Accepts or rejects the hunk at the cursor of the active review diff. */
  async setHunkAtCursor(on: boolean): Promise<void> {
    const t = this.activeTarget();
    const r = t && this.reviews.get(t.id);
    if (!t || !r) {
      void vscode.window.showInformationMessage('Switchyard: put the cursor in a review diff first (open one from the Review view).');
      return;
    }
    const f = r.cv.files[t.file];
    const d = await this.docFor(r, t.file);
    if (!f.splittable) {
      this.change(() => setFile(r.cv.files, r.sel, t.file, on));
      void vscode.window.setStatusBarMessage(`Switchyard: ${f.path} cannot be split into hunks: the whole file is ${on ? 'accepted' : 'rejected'}`, 5000);
      return;
    }
    const j = hunkAt(t.side === 'after' ? d.ranges : d.beforeRanges, t.line);
    if (j < 0) {
      return;
    }
    this.change(() => setHunk(r.cv.files, r.sel, t.file, j, on));
    void vscode.window.setStatusBarMessage(`Switchyard: hunk ${j + 1} ${on ? 'accepted' : 'rejected'} · ${selectionSummary(r.cv.files, r.sel)}`, 4000);
  }

  setWholeFile(arg: { id: string; file: number } | ReviewItem | undefined, on: boolean): void {
    let target: { id: string; file: number } | undefined = arg;
    if (!target) {
      target = this.activeTarget();
    }
    const r = target && this.reviews.get(target.id);
    if (!target || !r) {
      return;
    }
    const fileIdx = target.file;
    this.change(() => setFile(r.cv.files, r.sel, fileIdx, on));
  }

  async submit(arg?: unknown): Promise<void> {
    const id = await this.pickReview(arg);
    const r = id && this.reviews.get(id);
    if (!id || !r || !this.api) {
      return;
    }
    const body = decisionBody(r.cv.files, r.sel);
    if (body.apply.length === 0) {
      const ok = await vscode.window.showWarningMessage(
        'Nothing is selected: reject all changes of ' + r.cv.step_id + '? They are kept on a branch.', { modal: true }, 'Reject All');
      if (ok !== 'Reject All') {
        return;
      }
    }
    await this.post(id, body);
  }

  async rejectAll(arg?: unknown): Promise<void> {
    const id = await this.pickReview(arg);
    const r = id && this.reviews.get(id);
    if (!id || !r) {
      return;
    }
    const ok = await vscode.window.showWarningMessage(`Reject every change of ${r.cv.step_id}? They are kept on a branch.`, { modal: true }, 'Reject All');
    if (ok === 'Reject All') {
      await this.post(id, { apply: [], hunks: {} });
    }
  }

  async feedback(arg?: unknown): Promise<void> {
    const id = await this.pickReview(arg);
    const r = id && this.reviews.get(id);
    if (!id || !r) {
      return;
    }
    const text = await vscode.window.showInputBox({
      title: `Send ${r.cv.step_id} back to its agent`,
      prompt: 'What should the agent change? Its current changes stay in its worktree and come back for review.',
      ignoreFocusOut: true,
    });
    if (text && text.trim()) {
      await this.post(id, { feedback: text.trim() });
    }
  }

  private async post(id: string, body: unknown): Promise<void> {
    if (!this.api) {
      void vscode.window.showErrorMessage('Switchyard is not running.');
      return;
    }
    try {
      const res = await this.api.call<{ message: string }>('POST', `/api/approvals/${encodeURIComponent(id)}/changes`, body);
      void vscode.window.showInformationMessage('Switchyard: ' + res.message);
      this.reviews.delete(id);
      this.ended([id]);
      this.refresh();
      await this.closeTabs(id).catch(() => undefined);
      void vscode.commands.executeCommand('setContext', 'switchyard.reviewPending', this.reviews.size > 0);
    } catch (e) {
      void vscode.window.showErrorMessage('Switchyard: ' + (e as Error).message);
    }
  }
}

function statusWord(f: FileView): string {
  switch (f.status) {
    case 'A':
      return 'added';
    case 'D':
      return 'deleted';
  }
  return 'modified';
}

function escapeMd(s: string): string {
  return s.replace(/[\\`*_{}[\]()#+\-.!|<>]/g, '\\$&');
}
