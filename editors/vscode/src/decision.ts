// What the person picked in a change review, and the request body the
// server expects (POST /api/approvals/{id}/changes), with the same rules
// as the web page: a file is applied when it is included and, if it can
// be split, at least one of its hunks is; a split file lists its hunks
// only when some are left out.

export interface ReviewFile {
  path: string;
  splittable: boolean;
  hunks?: string[];
}

export interface Selection {
  /** Per file (same order as the change set): included at all. */
  include: boolean[];
  /** Per file: the selected hunk indexes (splittable files only). */
  hunks: Set<number>[];
}

export function selectAll(files: ReviewFile[]): Selection {
  return {
    include: files.map(() => true),
    hunks: files.map((f) => new Set((f.hunks ?? []).map((_, i) => i))),
  };
}

export function hunkCount(f: ReviewFile): number {
  return f.splittable ? (f.hunks ?? []).length : 0;
}

/** Whether file i is applied (wholly or partly). */
export function fileOn(files: ReviewFile[], sel: Selection, i: number): boolean {
  return sel.include[i] && (!files[i].splittable || sel.hunks[i].size > 0);
}

/** Whether hunk j of file i is applied. Hunks of an unsplittable file follow the file. */
export function hunkOn(files: ReviewFile[], sel: Selection, i: number, j: number): boolean {
  if (!files[i].splittable) {
    return sel.include[i];
  }
  return sel.include[i] && sel.hunks[i].has(j);
}

export function setFile(files: ReviewFile[], sel: Selection, i: number, on: boolean): void {
  sel.include[i] = on;
  if (files[i].splittable) {
    sel.hunks[i] = on ? new Set((files[i].hunks ?? []).map((_, j) => j)) : new Set();
  }
}

/**
 * Sets one hunk. It returns false when the file cannot be split, in which
 * case the whole file is set instead.
 */
export function setHunk(files: ReviewFile[], sel: Selection, i: number, j: number, on: boolean): boolean {
  if (!files[i].splittable) {
    sel.include[i] = on;
    return false;
  }
  if (on) {
    sel.hunks[i].add(j);
  } else {
    sel.hunks[i].delete(j);
  }
  sel.include[i] = sel.hunks[i].size > 0;
  return true;
}

export interface ChangesBody {
  apply: string[];
  hunks: Record<string, number[]>;
}

export function decisionBody(files: ReviewFile[], sel: Selection): ChangesBody {
  const apply: string[] = [];
  const hunks: Record<string, number[]> = {};
  files.forEach((f, i) => {
    if (!fileOn(files, sel, i)) {
      return;
    }
    apply.push(f.path);
    if (f.splittable && sel.hunks[i].size < (f.hunks ?? []).length) {
      hunks[f.path] = Array.from(sel.hunks[i]).sort((a, b) => a - b);
    }
  });
  return { apply, hunks };
}

/** A one-line summary, e.g. "2 of 3 files, 1 partly". */
export function selectionSummary(files: ReviewFile[], sel: Selection): string {
  const b = decisionBody(files, sel);
  const partly = Object.keys(b.hunks).length;
  return `${b.apply.length} of ${files.length} file(s)` + (partly ? `, ${partly} partly` : '');
}
