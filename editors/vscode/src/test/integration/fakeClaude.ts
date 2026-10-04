// A scripted stand-in for the Claude Code CLI (`claude -p --output-format
// stream-json`), so the integration test drives a real, non-demo sy (git,
// worktrees, change review) without spending any quota. sy starts it
// through a .cmd shim (fake-claude.cmd, written by runTest.ts) as
//
//   node fakeClaude.js <state dir> <claude args...>
//
// with the prompt on stdin. It recognises sy's prompt markers like
// internal/runner/fake.go does and writes one line per call to
// <state dir>/calls.log.

import * as fs from 'fs';
import * as path from 'path';

import { EDITED_LINES, edited, HANG, NOTES, PLAN_TEXT } from './fakeClaude.consts';

const stateDir = process.argv[2];
const args = process.argv.slice(3);
const prompt = fs.readFileSync(0, 'utf8');
const cwd = process.cwd();
const sid = `fake-${process.pid}`;
const resumeAt = args.indexOf('--resume');
const resumed = resumeAt >= 0 ? args[resumeAt + 1] : '';
// Read-only agents (explorer, planner, reviewers) never write.
const readOnly = args.includes('dontAsk');

function out(v: unknown): void {
  process.stdout.write(JSON.stringify(v) + '\n');
}

function note(what: string): void {
  fs.appendFileSync(path.join(stateDir, 'calls.log'), JSON.stringify({ what, pid: process.pid, sid, resumed, cwd, prompt }) + '\n');
}

function result(text: string): void {
  out({ type: 'result', subtype: 'success', is_error: false, result: text, session_id: sid, usage: { input_tokens: 120, output_tokens: 12 } });
}

function write(rel: string, content: string): void {
  const p = path.join(cwd, rel);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, content);
  // Real CLIs report absolute paths.
  out({ type: 'assistant', message: { content: [{ type: 'tool_use', name: 'Write', input: { file_path: p } }] } });
}

out({ type: 'system', subtype: 'init', session_id: sid, model: 'fake' });
out({ type: 'assistant', message: { content: [{ type: 'text', text: 'Reading the task' }] } });

const has = (m: string) => prompt.includes(m);
if (resumed) {
  // A follow-up (@agent message): sy resumes the agent's session.
  note('followup');
  result('followed up');
} else if (has('[SY:PLAN-REVIEW]') || has('[SY:FINAL-REVIEW]') || has('[SY:ERROR-REVIEW]')) {
  note('review');
  result('{"approve": true, "advice": "ok", "issues": []}');
} else if (has('[SY:JUDGE]')) {
  note('judge');
  result('A');
} else if (has('[SY:PLAN]')) {
  note('plan');
  const plan = {
    summary: 'Look around, then edit the notes and add a file.',
    subtasks: [
      { id: 'look', title: 'look around', kind: 'explore', prompt: 'List the top-level files.', files: [] },
      {
        id: 'edit', title: 'edit the notes', kind: 'edit',
        prompt: `Change ${NOTES} and write <<added.txt>> containing <<${PLAN_TEXT}>>`,
        files: [NOTES, 'added.txt'], depends_on: ['look'],
      },
    ],
  };
  result('```json\n' + JSON.stringify(plan, null, 2) + '\n```');
} else if (has(HANG)) {
  note('hang');
  fs.writeFileSync(path.join(stateDir, 'hang.pid'), String(process.pid));
  // Wait to be killed (sy cancels the task or stops).
  setInterval(() => undefined, 60_000);
} else {
  const m = /write <<([^>]+)>> containing <<([^>]+)>>/.exec(prompt);
  if (m && !readOnly) {
    note('edit ' + m[2]);
    const notes = path.join(cwd, NOTES);
    if (fs.existsSync(notes)) {
      // Keep the file's line ends, as an editing agent does (CRLF in a
      // core.autocrlf=true checkout on Windows).
      const text = fs.readFileSync(notes, 'utf8');
      const eol = text.includes('\r\n') ? '\r\n' : '\n';
      const lines = text.split(eol);
      for (const n of EDITED_LINES) {
        lines[n - 1] = edited(n);
      }
      write(NOTES, lines.join(eol));
    }
    write(m[1], m[2] + '\n');
    result('Edited the notes and wrote ' + m[1]);
  } else {
    note('explore');
    result('The repository has notes.txt and README.md.');
  }
}
