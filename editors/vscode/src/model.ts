// The agent tree and the activity log, folded from sy web's event stream
// the way the web page does it (internal/web/static/app.js: fold).

import type { SyEvent, StateView } from './types';

export type AgentStatus = 'queued' | 'running' | 'ok' | 'failed' | 'killed';

export interface AgentNode {
  id: string;
  parent: string;
  role: string;
  title: string;
  provider: string;
  model: string;
  status: AgentStatus;
  last: string;
  lastErr: boolean;
  files: string[];
  merge: string;
  mergeOK: boolean;
  tokens: number;
  started: number;
  ended: number;
}

export const MAIN = 'main';
export const REVIEWER = 'reviewer';
export const JUDGE = 'judge';

function newNode(id: string, role = '', title = ''): AgentNode {
  return {
    id, parent: id === MAIN ? '' : MAIN, role, title, provider: '', model: '', status: 'queued',
    last: '', lastErr: false, files: [], merge: '', mergeOK: false, tokens: 0, started: 0, ended: 0,
  };
}

function freshTokens(t: SyEvent['tokens']): number {
  if (!t) {
    return 0;
  }
  return (t.input ?? 0) - (t.cached ?? 0) + (t.output ?? 0);
}

export function oneLine(s: string | undefined, n = 0): string {
  const t = (s ?? '').split(/\s+/).filter(Boolean).join(' ');
  return n > 0 && t.length > n ? t.slice(0, Math.max(0, n - 1)) + '…' : t;
}

export class TaskModel {
  nodes = new Map<string, AgentNode>();
  /** Agent ids in the order they appeared (main, reviewer and judge excluded). */
  order: string[] = [];
  state: StateView | undefined;
  taskText = '';

  constructor() {
    this.reset();
  }

  reset(): void {
    this.nodes = new Map([[MAIN, newNode(MAIN, 'planner', 'main agent')]]);
    this.order = [];
    this.taskText = '';
  }

  node(id: string): AgentNode {
    let n = this.nodes.get(id);
    if (!n) {
      n = newNode(id, id === REVIEWER ? 'reviewer' : id === JUDGE ? 'judge' : '');
      this.nodes.set(id, n);
      if (id !== MAIN && id !== REVIEWER && id !== JUDGE) {
        this.order.push(id);
      }
    }
    return n;
  }

  /** The children of id, in order (reviewer and judge last). */
  children(id: string): AgentNode[] {
    const out: AgentNode[] = [];
    for (const k of this.order) {
      const n = this.nodes.get(k);
      if (n && this.parentOf(n) === id) {
        out.push(n);
      }
    }
    if (id === MAIN) {
      for (const k of [REVIEWER, JUDGE]) {
        const n = this.nodes.get(k);
        if (n) {
          out.push(n);
        }
      }
    }
    return out;
  }

  private parentOf(n: AgentNode): string {
    // Nest under the parent the orchestrator named when it is in the tree.
    return n.parent && n.parent !== n.id && this.nodes.has(n.parent) ? n.parent : MAIN;
  }

  /** Applies one event. It returns the activity log line for it, if any. */
  fold(e: SyEvent): string | undefined {
    const k = e.kind;
    switch (k) {
      case 'task_start': {
        this.reset();
        this.taskText = e.text ?? '';
        this.nodes.get(MAIN)!.title = e.text ?? '';
        return 'task: ' + (e.text ?? '');
      }
      case 'task_done': {
        const ts = Date.parse(e.ts) || Date.now();
        for (const n of this.nodes.values()) {
          if (n.status === 'queued' || n.status === 'running') {
            if (n.id === MAIN && e.ok) {
              continue;
            }
            n.status = 'killed';
            n.ended ||= ts;
          }
        }
        const main = this.nodes.get(MAIN)!;
        if (e.ok) {
          main.status = 'ok';
        } else if (main.status !== 'killed') {
          main.status = 'failed';
        }
        main.ended = ts;
        return (e.ok ? 'done: ' : 'failed: ') + (e.text ?? '');
      }
      case 'phase':
        return 'phase: ' + (e.text ?? '');
      case 'log':
        return (e.agent_id ? e.agent_id + ': ' : '') + (e.text ?? '');
      case 'provider':
        return `${e.provider ?? ''}: ${e.text ?? ''}`;
      case 'quota':
        return undefined;
      case 'route': {
        const d = e.decision;
        if (!d) {
          return undefined;
        }
        const label = d.provider + ':' + d.model + (d.effort ? '@' + d.effort : '');
        return `${e.agent_id ?? ''} → ${label}  [${d.rule} ${Number(d.confidence || 0).toFixed(2)}] ${d.reason || ''}${d.fallback ? ' (limit fallback)' : ''}`;
      }
      case 'checkpoint':
        this.node(REVIEWER);
        return `reviewer: ${e.ok ? 'approved' : 'changes requested'} · ${oneLine(e.text)}`;
      case 'merge': {
        if (!e.agent_id) {
          return undefined;
        }
        const n = this.node(e.agent_id);
        n.merge = e.text ?? '';
        n.mergeOK = !!e.ok;
        return `${e.agent_id}: merge ${e.ok ? '✓' : '✗'} ${e.text ?? ''}`;
      }
    }
    if (!e.agent_id) {
      return undefined;
    }
    return this.nodeEvent(this.node(e.agent_id), e);
  }

  private nodeEvent(n: AgentNode, e: SyEvent): string | undefined {
    const ts = Date.parse(e.ts) || Date.now();
    if (e.parent_id && n.id !== MAIN) {
      n.parent = e.parent_id;
    }
    switch (e.kind) {
      case 'queued':
        if (e.text && n.id !== MAIN) {
          n.title = e.text;
        }
        if (e.role && !n.role) {
          n.role = e.role;
        }
        if (e.provider) {
          n.provider = e.provider;
          n.model = e.model ?? '';
          n.role = e.role || n.role;
          n.status = 'queued';
        }
        return undefined;
      case 'started':
        n.status = 'running';
        n.started = ts;
        n.ended = 0;
        n.provider = e.provider ?? n.provider;
        n.model = e.model ?? n.model;
        if (e.role) {
          n.role = e.role;
        }
        break;
      case 'thinking':
      case 'tool':
      case 'message':
        if (e.text) {
          n.last = e.text;
          n.lastErr = false;
        }
        break;
      case 'edit':
        if (e.text && !n.files.includes(e.text)) {
          n.files.push(e.text);
        }
        n.last = 'edit ' + (e.text ?? '');
        n.lastErr = false;
        break;
      case 'usage':
        n.tokens += freshTokens(e.tokens);
        return undefined;
      case 'error':
        n.last = e.text ?? '';
        n.lastErr = true;
        break;
      case 'limit':
        n.last = 'usage limit: ' + (e.text ?? '');
        n.lastErr = true;
        break;
      case 'done':
        n.ended = ts;
        n.status = e.ok ? 'ok' : /killed/.test(e.text ?? '') ? 'killed' : 'failed';
        if (e.text) {
          n.last = e.text;
          n.lastErr = !e.ok;
        }
        break;
    }
    let text = e.text ?? '';
    if (e.kind === 'done' && !text.trim()) {
      text = n.status;
    }
    if ((e.kind === 'thinking' || e.kind === 'message') && !text.trim()) {
      return undefined;
    }
    const prov = e.provider ? ` (${e.provider})` : '';
    return `${n.id}${prov} ${e.kind}: ${text}`;
  }
}

/** Formats a log line with the event's local time. */
export function logLine(e: SyEvent, text: string): string {
  const d = new Date(Date.parse(e.ts) || Date.now());
  const p = (x: number) => String(x).padStart(2, '0');
  return `[${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}] ${text}`;
}

/**
 * Parses "@agent message" or "@ message" (the newest agent) like the
 * server does; undefined when text is not a follow-up.
 */
export function parseFollowUp(text: string): { agent: string; message: string } | undefined {
  if (!text.startsWith('@')) {
    return undefined;
  }
  const rest = text.slice(1);
  if (rest === '' || /^\s/.test(rest)) {
    return { agent: '', message: rest.trim() };
  }
  const m = /^(\S+)(?:\s+([\s\S]*))?$/.exec(rest);
  if (!m || !/^[a-z0-9_-]+$/.test(m[1])) {
    return undefined;
  }
  return { agent: m[1] === 'last' ? '' : m[1], message: (m[2] ?? '').trim() };
}
