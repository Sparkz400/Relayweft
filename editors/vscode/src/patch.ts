// Unified diffs as sy web sends them (one file's `git diff` output, the
// hunks numbered like the server's SplitHunks), and the before/after
// documents the diff editor shows.

export interface HunkLine {
  op: ' ' | '-' | '+';
  text: string;
}

export interface Hunk {
  header: string;
  oldStart: number;
  oldLines: number;
  newStart: number;
  newLines: number;
  lines: HunkLine[];
  /** "\ No newline at end of file" after the last old / new line. */
  oldNoEol: boolean;
  newNoEol: boolean;
}

export interface ParsedPatch {
  hunks: Hunk[];
  /** sy cut the patch short (huge file): the hunks are incomplete. */
  truncated: boolean;
}

const TRUNCATED = '... (diff truncated) ...';
const HUNK_RE = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/;

/** Splits text into lines (without "\n"; a "\r" stays) and whether it ends with a newline. */
export function splitLines(text: string): { lines: string[]; eol: boolean } {
  if (text === '') {
    return { lines: [], eol: false };
  }
  const lines = text.split('\n');
  const eol = lines[lines.length - 1] === '';
  if (eol) {
    lines.pop();
  }
  return { lines, eol };
}

export function joinLines(lines: string[], eol: boolean): string {
  if (lines.length === 0) {
    return '';
  }
  return lines.join('\n') + (eol ? '\n' : '');
}

/** Parses one file's unified diff. Lines before the first @@ are the header. */
export function parsePatch(patch: string): ParsedPatch {
  const out: ParsedPatch = { hunks: [], truncated: false };
  let cur: Hunk | undefined;
  let last: HunkLine | undefined;
  const lines = patch.split('\n');
  if (lines[lines.length - 1] === '') {
    lines.pop();
  }
  for (const l of lines) {
    if (l === TRUNCATED) {
      out.truncated = true;
      break;
    }
    if (l.startsWith('@@ ')) {
      // Every "@@ " line starts a hunk, like the server numbers them.
      const m = HUNK_RE.exec(l);
      cur = {
        header: l,
        oldStart: m ? Number(m[1]) : 0,
        oldLines: !m ? 0 : m[2] === undefined ? 1 : Number(m[2]),
        newStart: m ? Number(m[3]) : 0,
        newLines: !m ? 0 : m[4] === undefined ? 1 : Number(m[4]),
        lines: [],
        oldNoEol: false,
        newNoEol: false,
      };
      out.hunks.push(cur);
      last = undefined;
      continue;
    }
    if (!cur) {
      continue; // header
    }
    if (l.startsWith('\\')) {
      if (last) {
        if (last.op !== '+') {
          cur.oldNoEol = true;
        }
        if (last.op !== '-') {
          cur.newNoEol = true;
        }
      }
      continue;
    }
    const op = l[0];
    if (op === ' ' || op === '-' || op === '+') {
      last = { op, text: l.slice(1) };
      cur.lines.push(last);
    } else if (l === '') {
      // Some tools strip the space of an empty context line.
      last = { op: ' ', text: '' };
      cur.lines.push(last);
    }
  }
  // The line counts in the headers are not trusted (where a hunk goes is
  // found by its content): keep them consistent with the lines read.
  for (const h of out.hunks) {
    h.oldLines = h.lines.filter((x) => x.op !== '+').length;
    h.newLines = h.lines.filter((x) => x.op !== '-').length;
  }
  return out;
}

/** A 0-based, end-exclusive line range in the "after" document. */
export interface LineRange {
  start: number;
  end: number;
}

export interface Reconstructed {
  before: string;
  after: string;
  /** Where each hunk is in the "after" document (by hunk index). */
  ranges: LineRange[];
  /** Where each hunk is in the "before" document. */
  beforeRanges: LineRange[];
  /**
   * true: before is the whole file and after the whole result. false: the
   * documents hold only the hunks, with markers for the lines not shown
   * (the file on disk is not the agent's base, or the patch is truncated).
   */
  whole: boolean;
}

function oldSide(h: Hunk): string[] {
  return h.lines.filter((l) => l.op !== '+').map((l) => l.text);
}

function matchesAt(lines: string[], at: number, want: string[]): boolean {
  if (at < 0 || at + want.length > lines.length) {
    return false;
  }
  for (let i = 0; i < want.length; i++) {
    if (lines[at + i] !== want[i]) {
      return false;
    }
  }
  return true;
}

/**
 * Applies the hunks to before. It returns undefined when a hunk's old
 * lines are not found (the file changed since the agent's base). A hunk
 * may have moved (lines added or removed above it): it is looked for near
 * its position first, then anywhere after the previous hunk.
 */
export function applyHunks(
  before: { lines: string[]; eol: boolean },
  hunks: Hunk[],
): { lines: string[]; eol: boolean; ranges: LineRange[]; beforeRanges: LineRange[] } | undefined {
  const out: string[] = [];
  const ranges: LineRange[] = [];
  const beforeRanges: LineRange[] = [];
  let pos = 0; // next unread line of before
  let shift = 0; // where hunks were found relative to their header
  let eol = before.eol;
  for (const h of hunks) {
    const want = oldSide(h);
    const nominal = (h.oldLines === 0 ? h.oldStart : h.oldStart - 1) + shift;
    let at = -1;
    if (nominal >= pos && matchesAt(before.lines, nominal, want)) {
      at = nominal;
    } else {
      for (let d = 1; d <= before.lines.length && at < 0; d++) {
        for (const c of [nominal - d, nominal + d]) {
          if (c >= pos && matchesAt(before.lines, c, want)) {
            at = c;
            break;
          }
        }
        if (nominal - d < pos && nominal + d > before.lines.length) {
          break;
        }
      }
    }
    if (at < 0) {
      return undefined;
    }
    shift = at - (h.oldLines === 0 ? h.oldStart : h.oldStart - 1);
    out.push(...before.lines.slice(pos, at));
    const start = out.length;
    for (const l of h.lines) {
      if (l.op !== '-') {
        out.push(l.text);
      }
    }
    ranges.push({ start, end: out.length });
    beforeRanges.push({ start: at, end: at + want.length });
    pos = at + want.length;
    if (pos >= before.lines.length) {
      // The hunk reaches the end of the file: it decides the final newline.
      if (h.oldNoEol === before.eol && h.oldLines > 0) {
        return undefined; // the base's final newline differs from the disk's
      }
      eol = !h.newNoEol;
    }
  }
  out.push(...before.lines.slice(pos));
  if (out.length === 0) {
    eol = false;
  }
  return { lines: out, eol, ranges, beforeRanges };
}

/** Builds before/after documents from the hunks alone. */
export function fragments(hunks: Hunk[], truncated: boolean): Reconstructed {
  const b: string[] = [];
  const a: string[] = [];
  const ranges: LineRange[] = [];
  const beforeRanges: LineRange[] = [];
  let next = 1; // next old line number not shown yet
  const gap = (from: number, to: number) => {
    const m = to > from ? `⋯ lines ${from}–${to} not shown ⋯` : `⋯ line ${from} not shown ⋯`;
    b.push(m);
    a.push(m);
  };
  for (const h of hunks) {
    const first = h.oldLines === 0 ? h.oldStart + 1 : h.oldStart;
    if (first > next) {
      gap(next, first - 1);
    }
    const bStart = b.length;
    for (const l of h.lines) {
      if (l.op !== '+') {
        b.push(l.text);
      }
    }
    beforeRanges.push({ start: bStart, end: b.length });
    const start = a.length;
    for (const l of h.lines) {
      if (l.op !== '-') {
        a.push(l.text);
      }
    }
    ranges.push({ start, end: a.length });
    next = first + h.oldLines;
  }
  const tail = truncated ? '⋯ the rest of the diff was too large to show; see the patch in sy web ⋯' : '⋯ rest of the file not shown ⋯';
  if (hunks.length === 0 || truncated || next > 1) {
    b.push(tail);
    a.push(tail);
  }
  return { before: joinLines(b, true), after: joinLines(a, true), ranges, beforeRanges, whole: false };
}

/**
 * Builds the before/after documents for one file of a change set.
 * status is A (added), M (modified) or D (deleted); disk is the file's
 * current content in the workspace (undefined when there is none).
 */
export function reconstruct(patch: string, status: string, disk: string | undefined): Reconstructed {
  const p = parsePatch(patch);
  if (!p.truncated) {
    let base: { lines: string[]; eol: boolean } | undefined;
    if (status === 'A') {
      base = { lines: [], eol: false };
    } else if (disk !== undefined) {
      base = splitLines(disk);
    }
    if (base) {
      const r = applyHunks(base, p.hunks);
      if (r) {
        return { before: joinLines(base.lines, base.eol), after: joinLines(r.lines, r.eol), ranges: r.ranges, beforeRanges: r.beforeRanges, whole: true };
      }
      // core.autocrlf=true (Git for Windows' default): the file on disk has
      // CRLF, but git diffs the normalized (LF) content, so no hunk matches
      // as it is. Compare and show both sides with LF line ends then.
      if (disk !== undefined && disk.includes('\r\n')) {
        const lf = splitLines(disk.replace(/\r\n/g, '\n'));
        const hunks = p.hunks.map((h) => ({ ...h, lines: h.lines.map((l) => ({ op: l.op, text: l.text.replace(/\r$/, '') })) }));
        const r2 = applyHunks(lf, hunks);
        if (r2) {
          return { before: joinLines(lf.lines, lf.eol), after: joinLines(r2.lines, r2.eol), ranges: r2.ranges, beforeRanges: r2.beforeRanges, whole: true };
        }
      }
    }
    if (status === 'D' && p.hunks.length === 1 && p.hunks[0].oldStart <= 1) {
      // A deletion's patch holds the whole old file.
      const h = p.hunks[0];
      const old = oldSide(h);
      return { before: joinLines(old, !h.oldNoEol), after: '', ranges: [{ start: 0, end: 0 }], beforeRanges: [{ start: 0, end: old.length }], whole: true };
    }
  }
  return fragments(p.hunks, p.truncated);
}

/** The index of the hunk whose range holds line (0-based), or the nearest one. */
export function hunkAt(ranges: LineRange[], line: number): number {
  let best = -1;
  let dist = Infinity;
  ranges.forEach((r, i) => {
    const d = line < r.start ? r.start - line : line >= Math.max(r.end, r.start + 1) ? line - Math.max(r.end - 1, r.start) : 0;
    if (d < dist) {
      best = i;
      dist = d;
    }
  });
  return best;
}
