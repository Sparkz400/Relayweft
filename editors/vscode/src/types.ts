// The parts of sy web's JSON API the extension uses (internal/web).

/** The line `sy web --client` prints first (web.ClientHello). */
export interface ClientHello {
  switchyard: 'web-client';
  protocol: number;
  version: string;
  url: string;
  addr: string;
  bootstrap: string;
  dir: string;
  pid: number;
  demo?: boolean;
}

export interface TokenUsage {
  input?: number;
  cached?: number;
  output?: number;
  reasoning?: number;
  cost_usd?: number;
}

export interface Decision {
  step_id: string;
  role: string;
  provider: string;
  model: string;
  effort?: string;
  rule: string;
  reason: string;
  confidence: number;
  fallback?: boolean;
}

/** event.Event as JSON; kind is the event kind's name. */
export interface SyEvent {
  agent_id?: string;
  parent_id?: string;
  provider?: string;
  model?: string;
  role?: string;
  kind: string;
  text?: string;
  tokens?: TokenUsage;
  ts: string;
  ok?: boolean;
  decision?: Decision;
  cost?: { cost_usd?: number };
  until?: string;
}

export interface Subtask {
  id: string;
  title: string;
  kind: string;
  prompt: string;
  files: string[] | null;
  depends_on: string[] | null;
  role?: string;
  repo?: string;
}

export interface Plan {
  summary: string;
  subtasks: Subtask[];
  repos?: string[];
}

export interface FileView {
  path: string;
  status: string; // A, M, D
  added: number;
  deleted: number;
  binary?: boolean;
  patch: string;
  splittable: boolean;
  header?: string;
  hunks?: string[];
}

export interface ChangeView {
  step_id: string;
  title: string;
  summary?: string;
  round: number;
  files: FileView[];
}

export interface BudgetView {
  text: string;
  hint: string;
}

export interface ApprovalRequest {
  id: string;
  type: 'plan' | 'changes' | 'budget';
  task?: string;
  plan?: Plan;
  changes?: ChangeView;
  budget?: BudgetView;
  created: string;
}

export interface ResultView {
  ok: boolean;
  text: string;
  cost?: string;
  took?: string;
}

export interface StateView {
  version: string;
  dir: string;
  project: string;
  demo: boolean;
  running: boolean;
  cancelling: boolean;
  paused: boolean;
  phase: string;
  task?: string;
  queue: { id: number; label: string; kind: string; at?: string }[];
  approvals: ApprovalRequest[];
  running_agents: string[] | null;
  last?: ResultView;
}

export interface SessionRow {
  agent: string;
  role?: string;
  provider?: string;
  model?: string;
  title?: string;
  running: boolean;
}

export interface SubmitResult {
  status: string;
  message: string;
  job_id?: number;
}
