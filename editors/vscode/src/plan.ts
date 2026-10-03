// Plan approval: a quick pick (approve, edit, reject) and, for edits, an
// untitled JSON document with the plan and CodeLens to approve or reject
// it. The answer goes to POST /api/approvals/{id}/plan.

import * as vscode from 'vscode';
import type { SyApi } from './api';
import { oneLine } from './model';
import type { ApprovalRequest, Plan } from './types';

export class PlanFlow implements vscode.CodeLensProvider {
  /** Open plan editors: document URI -> approval id. */
  private editors = new Map<string, string>();
  private readonly lensChanged = new vscode.EventEmitter<void>();
  readonly onDidChangeCodeLenses = this.lensChanged.event;
  private readonly disposables: vscode.Disposable[] = [];

  constructor(private readonly getApi: () => SyApi | undefined) {
    this.disposables.push(
      this.lensChanged,
      vscode.workspace.onDidCloseTextDocument((d) => this.editors.delete(d.uri.toString())),
      vscode.window.onDidChangeActiveTextEditor((ed) => this.updateContext(ed)),
    );
  }

  dispose(): void {
    for (const d of this.disposables) {
      d.dispose();
    }
  }

  private updateContext(ed = vscode.window.activeTextEditor): void {
    void vscode.commands.executeCommand('setContext', 'switchyard.planEditorActive', !!ed && this.editors.has(ed.document.uri.toString()));
  }

  /** Forgets editors of approvals that are no longer waiting. */
  sync(approvals: ApprovalRequest[]): void {
    const live = new Set(approvals.filter((a) => a.type === 'plan').map((a) => a.id));
    let changed = false;
    for (const [uri, id] of this.editors) {
      if (!live.has(id)) {
        this.editors.delete(uri);
        changed = true;
      }
    }
    if (changed) {
      this.lensChanged.fire();
      this.updateContext();
    }
  }

  provideCodeLenses(doc: vscode.TextDocument): vscode.CodeLens[] {
    const id = this.editors.get(doc.uri.toString());
    if (!id) {
      return [];
    }
    const top = new vscode.Range(0, 0, 0, 0);
    return [
      new vscode.CodeLens(top, { title: '$(check) Approve this plan', command: 'switchyard.plan.approveEdited', arguments: [doc.uri] }),
      new vscode.CodeLens(top, { title: '$(close) Reject (cancel the task)', command: 'switchyard.plan.reject', arguments: [doc.uri] }),
    ];
  }

  /** The quick pick for a plan approval. */
  async ask(req: ApprovalRequest): Promise<void> {
    const plan = req.plan;
    if (!plan) {
      return;
    }
    type Item = vscode.QuickPickItem & { act?: 'approve' | 'edit' | 'reject' };
    const items: Item[] = [
      { label: '$(check) Approve', description: `run ${plan.subtasks.length} subtask(s)`, act: 'approve' },
      { label: '$(edit) Edit the plan…', description: 'as JSON: titles, prompts, files, dependencies, roles', act: 'edit' },
      { label: '$(close) Reject', description: 'cancel the task', act: 'reject' },
      { label: 'Subtasks', kind: vscode.QuickPickItemKind.Separator },
      ...plan.subtasks.map((s): Item => ({
        label: `${s.id}: ${s.title}`,
        description: [s.kind, s.role, s.depends_on?.length ? 'after ' + s.depends_on.join(', ') : ''].filter(Boolean).join(' · '),
        detail: oneLine(s.prompt, 200),
        act: 'edit',
      })),
    ];
    const pick = await vscode.window.showQuickPick(items, {
      title: 'Switchyard plan: ' + oneLine(plan.summary || req.task, 100),
      placeHolder: 'Approve, edit or reject the plan (pick a subtask to edit it)',
      ignoreFocusOut: true,
      matchOnDescription: true,
      matchOnDetail: true,
    });
    switch (pick?.act) {
      case 'approve':
        await this.answer(req.id, plan, true);
        break;
      case 'reject':
        await this.reject(req.id);
        break;
      case 'edit':
        await this.edit(req);
        break;
    }
  }

  private async edit(req: ApprovalRequest): Promise<void> {
    const doc = await vscode.workspace.openTextDocument({ language: 'json', content: JSON.stringify(req.plan, null, 2) + '\n' });
    this.editors.set(doc.uri.toString(), req.id);
    await vscode.window.showTextDocument(doc, { preview: false });
    this.lensChanged.fire();
    this.updateContext();
    void vscode.window.setStatusBarMessage('Switchyard: edit the plan, then use "Approve this plan" at the top of the document', 8000);
  }

  private docFor(uri?: vscode.Uri): vscode.TextDocument | undefined {
    if (uri) {
      return vscode.workspace.textDocuments.find((d) => d.uri.toString() === uri.toString());
    }
    return vscode.window.activeTextEditor?.document;
  }

  async approveEdited(uri?: vscode.Uri): Promise<void> {
    const doc = this.docFor(uri);
    const id = doc && this.editors.get(doc.uri.toString());
    if (!doc || !id) {
      void vscode.window.showWarningMessage('Switchyard: this is not a plan waiting for approval.');
      return;
    }
    let plan: Plan;
    try {
      plan = JSON.parse(doc.getText()) as Plan;
    } catch (e) {
      void vscode.window.showErrorMessage('Switchyard: the plan is not valid JSON: ' + (e as Error).message);
      return;
    }
    if (!plan || !Array.isArray(plan.subtasks)) {
      void vscode.window.showErrorMessage('Switchyard: the plan needs a "subtasks" list.');
      return;
    }
    if (await this.answer(id, plan, true)) {
      await this.close(doc);
    }
  }

  async rejectEdited(uri?: vscode.Uri): Promise<void> {
    const doc = this.docFor(uri);
    const id = doc && this.editors.get(doc.uri.toString());
    if (!doc || !id) {
      return;
    }
    if (await this.reject(id)) {
      await this.close(doc);
    }
  }

  private async close(doc: vscode.TextDocument): Promise<void> {
    this.editors.delete(doc.uri.toString());
    this.lensChanged.fire();
    const ed = vscode.window.visibleTextEditors.find((e) => e.document === doc);
    if (ed) {
      await vscode.window.showTextDocument(doc, ed.viewColumn);
      // An untitled document would ask to be saved; it is not needed any more.
      await vscode.commands.executeCommand('workbench.action.revertAndCloseActiveEditor');
    }
    this.updateContext();
  }

  private async reject(id: string): Promise<boolean> {
    const ok = await vscode.window.showWarningMessage('Reject the plan? The task is cancelled.', { modal: true }, 'Reject');
    if (ok !== 'Reject') {
      return false;
    }
    return this.answer(id, undefined, false);
  }

  private async answer(id: string, plan: Plan | undefined, ok: boolean): Promise<boolean> {
    const api = this.getApi();
    if (!api) {
      void vscode.window.showErrorMessage('Switchyard is not running.');
      return false;
    }
    try {
      const res = await api.call<{ message: string }>('POST', `/api/approvals/${encodeURIComponent(id)}/plan`, { ok, plan: plan ?? { summary: '', subtasks: [] } });
      void vscode.window.showInformationMessage('Switchyard: ' + res.message);
      return true;
    } catch (e) {
      void vscode.window.showErrorMessage('Switchyard: ' + (e as Error).message);
      return false;
    }
  }
}
