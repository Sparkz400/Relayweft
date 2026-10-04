import * as assert from 'node:assert/strict';
import { test } from 'node:test';
import { applyHunks, hunkAt, parsePatch, reconstruct, splitLines } from '../patch';

const before = ['package main', '', 'import "fmt"', '', 'func a() {', '\tfmt.Println("a")', '}', '', 'func b() {', '\tfmt.Println("b")', '}', '', 'func c() {', '\tfmt.Println("c")', '}', ''].join('\n');

const patch = `diff --git a/x.go b/x.go
index 1111111..2222222 100644
--- a/x.go
+++ b/x.go
@@ -3,5 +3,5 @@ package main
 import "fmt"

 func a() {
-\tfmt.Println("a")
+\tfmt.Println("A")
 }
@@ -12,4 +12,5 @@ func b() {

 func c() {
 \tfmt.Println("c")
+\tfmt.Println("C")
 }
`;

test('parsePatch reads hunks like the server splits them', () => {
  const p = parsePatch(patch);
  assert.equal(p.truncated, false);
  assert.equal(p.hunks.length, 2);
  assert.deepEqual([p.hunks[0].oldStart, p.hunks[0].oldLines, p.hunks[0].newStart, p.hunks[0].newLines], [3, 5, 3, 5]);
  assert.equal(p.hunks[1].lines.filter((l) => l.op === '+').length, 1);
});

test('reconstruct applies the patch to the file on disk', () => {
  const r = reconstruct(patch, 'M', before);
  assert.equal(r.whole, true);
  assert.equal(r.before, before);
  assert.equal(r.after, before.replace('"a"', '"A"').replace('\tfmt.Println("c")\n', '\tfmt.Println("c")\n\tfmt.Println("C")\n'));
  assert.deepEqual(r.ranges, [{ start: 2, end: 7 }, { start: 11, end: 16 }]);
  assert.deepEqual(r.beforeRanges, [{ start: 2, end: 7 }, { start: 11, end: 15 }]);
});

test('a hunk is found when lines moved above it', () => {
  const shifted = '// header\n// more\n' + before;
  const r = reconstruct(patch, 'M', shifted);
  assert.equal(r.whole, true);
  assert.ok(r.after.startsWith('// header\n// more\npackage main'));
  assert.ok(r.after.includes('"A"') && r.after.includes('"C"'));
  assert.deepEqual(r.ranges[0], { start: 4, end: 9 });
});

test('a file that differs from the base falls back to the hunks', () => {
  const r = reconstruct(patch, 'M', before.replace('"a"', '"x"'));
  assert.equal(r.whole, false);
  assert.match(r.before, /lines 1–2 not shown/);
  assert.match(r.after, /"A"/);
  assert.equal(r.ranges.length, 2);
  const after = r.after.split('\n');
  assert.equal(after[r.ranges[1].start], '');
  assert.equal(after[r.ranges[1].end - 2], '\tfmt.Println("C")');
});

test('added and deleted files', () => {
  const add = 'diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,2 @@\n+one\n+two\n';
  const a = reconstruct(add, 'A', undefined);
  assert.deepEqual([a.before, a.after, a.whole], ['', 'one\ntwo\n', true]);
  const del = 'diff --git a/n.txt b/n.txt\ndeleted file mode 100644\n--- a/n.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-one\n-two\n';
  const d = reconstruct(del, 'D', undefined);
  assert.deepEqual([d.before, d.after, d.whole], ['one\ntwo\n', '', true]);
});

test('no newline at end of file', () => {
  const p = '--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n one\n-two\n\\ No newline at end of file\n+TWO\n';
  const r = reconstruct(p, 'M', 'one\ntwo');
  assert.equal(r.whole, true);
  assert.equal(r.after, 'one\nTWO\n');
  // The disk file has a final newline but the base did not: not the base.
  assert.equal(reconstruct(p, 'M', 'one\ntwo\n').whole, false);
});

test('CRLF files keep their line endings', () => {
  const p = '--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n one\r\n-two\r\n+TWO\r\n';
  const r = reconstruct(p, 'M', 'one\r\ntwo\r\n');
  assert.equal(r.whole, true);
  assert.equal(r.after, 'one\r\nTWO\r\n');
});

test('core.autocrlf: an LF patch applies to a CRLF checkout (whole file, not hunks only)', () => {
  // Found in a real VS Code on Windows: git diffs the normalized (LF)
  // content while the working tree has CRLF.
  const disk = before.replace(/\n/g, '\r\n');
  const r = reconstruct(patch, 'M', disk);
  assert.equal(r.whole, true);
  assert.equal(r.before, before);
  assert.equal(r.after, before.replace('"a"', '"A"').replace('\tfmt.Println("c")\n', '\tfmt.Println("c")\n\tfmt.Println("C")\n'));
  assert.deepEqual(r.ranges, [{ start: 2, end: 7 }, { start: 11, end: 16 }]);
  assert.equal(hunkAt(r.ranges, 12), 1);
});

test('a truncated patch shows the hunks it has', () => {
  const p = patch + '... (diff truncated) ...';
  const r = reconstruct(p, 'M', before);
  assert.equal(r.whole, false);
  assert.match(r.after, /too large/);
});

test('applyHunks refuses a file it cannot match', () => {
  assert.equal(applyHunks(splitLines('nothing alike\n'), parsePatch(patch).hunks), undefined);
});

test('hunkAt picks the hunk at or nearest a line', () => {
  const rs = [{ start: 2, end: 7 }, { start: 11, end: 16 }];
  assert.equal(hunkAt(rs, 0), 0);
  assert.equal(hunkAt(rs, 4), 0);
  assert.equal(hunkAt(rs, 12), 1);
  assert.equal(hunkAt(rs, 30), 1);
  assert.equal(hunkAt([], 3), -1);
});

test('header line counts are not trusted, the hunks are kept and numbered like the server', () => {
  const p = '--- a/f\n+++ b/f\n@@ -1,9 +1,9 @@\n a\n-b\n+B\n c\n@@ bogus @@\n x\n';
  const parsed = parsePatch(p);
  assert.equal(parsed.hunks.length, 2);
  assert.deepEqual([parsed.hunks[0].oldLines, parsed.hunks[0].newLines], [3, 3]);
  const r = reconstruct('--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n', 'M', 'a\nb\nc\nd\n');
  assert.equal(r.after, 'a\nB\nc\nd\n');
  const add = reconstruct('@@ -0,0 +1,5 @@\n+one\n+two\n', 'A', undefined);
  assert.deepEqual([add.after, add.whole], ['one\ntwo\n', true]);
});
