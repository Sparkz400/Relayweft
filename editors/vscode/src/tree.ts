// The Agents view: the task's status, waiting approvals, the queue and the
// agent tree, live from the event stream.

import * as vscode from 'vscode';
import { MAIN, oneLine, type AgentNode, type TaskModel } from './model';
import type { ApprovalRequest } from './types';

export type AgentsItem =
  | { kind: 'status' }
  | { kind: 'approvals' }
  | { kind: 'approval'; req: ApprovalRequest }
  | { kind: 'queue' }
  | { kind: 'job'; id: number; label: string; at?: string }
  | { kind: 'agent'; id: string };

const STATUS_ICON: Record<string, [string, string?]> = {
  queued: ['circle-outline', 'disabledForeground'],
  running: ['sync~spin', 'charts.blue'],
  ok: ['pass', 'testing.iconPassed'],
  failed: ['error', 'testing.iconFailed'],
  killed: ['circle-slash', 'disabledForeground'],
};

export class AgentsTree implements vscode.TreeDataProvider<AgentsItem> {
  private readonly changed = new vscode.EventEmitter<AgentsItem | undefined>();
  readonly onDidChangeTreeData = this.changed.event;
  connected = false;
  error = '';

  constructor(private readonly model: TaskModel) {}

  refresh(): void {
    this.changed.fire(undefined);
  }

  dispose(): void {
    this.changed.dispose();
  }

  getChildren(e?: AgentsItem): AgentsItem[] {
    const st = this.model.state;
    if (!e) {
      const out: AgentsItem[] = [{ kind: 'status' }];
      if (st?.approvals?.length) {
        out.push({ kind: 'approvals' });
      }
      if (st?.queue?.length) {
        out.push({ kind: 'queue' });
      }
      if (this.model.taskText || this.model.order.length) {
        out.push({ kind: 'agent', id: MAIN });
      }
      return out;
    }
    switch (e.kind) {
      case 'approvals':
        return (st?.approvals ?? []).map((req) => ({ kind: 'approval', req }));
      case 'queue':
        return (st?.queue ?? []).map((j) => ({ kind: 'job', id: j.id, label: j.label, at: j.at }));
      case 'agent':
        return this.model.children(e.id).map((n) => ({ kind: 'agent', id: n.id }));
    }
    return [];
  }

  getTreeItem(e: AgentsItem): vscode.TreeItem {
    const st = this.model.state;
    const None = vscode.TreeItemCollapsibleState.None;
    switch (e.kind) {
      case 'status': {
        const it = new vscode.TreeItem(st?.project || 'Relayweft', None);
        if (!this.connected) {
          it.description = this.error ? 'disconnected: ' + this.error : 'connecting…';
          it.iconPath = new vscode.ThemeIcon('debug-disconnect');
        } else if (st?.running) {
          it.description = `${st.cancelling ? 'cancelling' : st.phase}${st.paused ? ' · paused' : ''}`;
          it.iconPath = new vscode.ThemeIcon('loading~spin');
        } else if (st?.last) {
          it.description = `idle · last task ${st.last.ok ? 'done' : 'failed'}${st.last.took ? ' in ' + st.last.took : ''}`;
          it.iconPath = new vscode.ThemeIcon(st.last.ok ? 'pass' : 'error');
        } else {
          it.description = 'idle';
          it.iconPath = new vscode.ThemeIcon('circle-large-outline');
        }
        const md = new vscode.MarkdownString();
        md.appendMarkdown(`**Relayweft ${st?.version ?? ''}**${st?.demo ? ' (demo)' : ''}\n\n`);
        md.appendText(`${st?.dir ?? ''}\n`);
        if (st?.task) {
          md.appendText(`\ntask: ${oneLine(st.task, 300)}\n`);
        }
        if (st?.last && !st.running) {
          md.appendText(`\n${st.last.ok ? 'done' : 'failed'}: ${oneLine(st.last.text, 400)}\n`);
          if (st.last.cost) {
            md.appendText(`${st.last.cost}\n`);
          }
        }
        it.tooltip = md;
        it.contextValue = 'status';
        return it;
      }
      case 'approvals': {
        const n = st?.approvals?.length ?? 0;
        const it = new vscode.TreeItem(`Waiting for you (${n})`, vscode.TreeItemCollapsibleState.Expanded);
        it.iconPath = new vscode.ThemeIcon('bell-dot', new vscode.ThemeColor('notificationsWarningIcon.foreground'));
        return it;
      }
      case 'approval': {
        const r = e.req;
        let label = '';
        let icon = 'question';
        switch (r.type) {
          case 'plan':
            label = `Approve plan: ${r.plan?.subtasks?.length ?? 0} subtask(s)`;
            icon = 'checklist';
            break;
          case 'changes':
            label = `Review ${r.changes?.conflict ? 'conflict resolution of ' : ''}${r.changes?.step_id ?? ''}: ${r.changes?.files.length ?? 0} file(s)`;
            icon = r.changes?.conflict ? 'git-merge' : 'diff';
            break;
          case 'budget':
            label = 'Budget reached';
            icon = 'credit-card';
            break;
          case 'conflict':
            label = `Merge conflict: ${r.conflict?.step_id ?? ''}`;
            icon = 'git-merge';
            break;
        }
        const it = new vscode.TreeItem(label, None);
        it.description = oneLine(r.type === 'budget' ? r.budget?.text : r.type === 'conflict' ? r.conflict?.text : r.type === 'changes' ? r.changes?.title : r.plan?.summary || r.task, 80);
        it.iconPath = new vscode.ThemeIcon(icon);
        it.contextValue = 'approval.' + r.type;
        it.command = { command: 'relayweft.answerApproval', title: 'Answer', arguments: [r.id] };
        return it;
      }
      case 'queue': {
        const it = new vscode.TreeItem(`Queue (${st?.queue?.length ?? 0})`, vscode.TreeItemCollapsibleState.Collapsed);
        it.iconPath = new vscode.ThemeIcon('list-ordered');
        it.tooltip = 'Queued tasks run unattended (no approvals) after the current one.';
        return it;
      }
      case 'job': {
        const it = new vscode.TreeItem(oneLine(e.label, 80), None);
        it.description = e.at ? 'at ' + new Date(e.at).toLocaleTimeString() : '#' + e.id;
        it.iconPath = new vscode.ThemeIcon(e.at ? 'clock' : 'circle-small');
        return it;
      }
      case 'agent':
        return this.agentItem(this.model.nodes.get(e.id));
    }
  }

  private agentItem(n: AgentNode | undefined): vscode.TreeItem {
    if (!n) {
      return new vscode.TreeItem('?');
    }
    const kids = this.model.children(n.id).length;
    const label = n.id === MAIN ? oneLine(this.model.taskText || 'main agent', 60) : n.id;
    const it = new vscode.TreeItem(label, kids ? vscode.TreeItemCollapsibleState.Expanded : vscode.TreeItemCollapsibleState.None);
    const route = n.provider ? `${n.provider}:${n.model}` : '';
    const parts = [n.id === MAIN ? 'main' : n.title ? oneLine(n.title, 40) : '', n.role, route, n.status].filter(Boolean);
    it.description = parts.join(' · ');
    const [icon, color] = STATUS_ICON[n.status] ?? ['circle-outline'];
    it.iconPath = new vscode.ThemeIcon(icon, color ? new vscode.ThemeColor(color) : undefined);
    const md = new vscode.MarkdownString();
    md.appendMarkdown(`**${n.id}**`);
    md.appendText(` ${n.role ? '(' + n.role + ')' : ''} ${route}\n`);
    if (n.title && n.id !== MAIN) {
      md.appendText(n.title + '\n');
    }
    md.appendText(`status: ${n.status}${n.tokens ? ` · ${n.tokens} fresh tokens` : ''}\n`);
    if (n.last) {
      md.appendText(`\n${n.lastErr ? 'error: ' : ''}${oneLine(n.last, 400)}\n`);
    }
    if (n.files.length) {
      md.appendText(`\nfiles: ${n.files.slice(0, 20).join(', ')}${n.files.length > 20 ? ' …' : ''}\n`);
    }
    if (n.merge) {
      md.appendText(`\nmerge ${n.mergeOK ? '✓' : '✗'} ${n.merge}\n`);
    }
    it.tooltip = md;
    it.contextValue = 'agent.' + n.status;
    it.id = 'agent:' + n.id;
    return it;
  }
}
