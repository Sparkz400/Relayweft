package io.github.sparkz400.relayweft

import io.github.sparkz400.relayweft.core.LineRange
import io.github.sparkz400.relayweft.core.applyHunks
import io.github.sparkz400.relayweft.core.documentText
import io.github.sparkz400.relayweft.core.hunkAt
import io.github.sparkz400.relayweft.core.parsePatch
import io.github.sparkz400.relayweft.core.reconstruct
import io.github.sparkz400.relayweft.core.splitLines
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

// The same cases as editors/vscode/src/test/patch.test.ts, plus the IDE's
// LF-only documents.
class PatchTest {
    private val before = listOf(
        "package main", "", "import \"fmt\"", "", "func a() {", "\tfmt.Println(\"a\")", "}", "", "func b() {", "\tfmt.Println(\"b\")", "}", "",
        "func c() {", "\tfmt.Println(\"c\")", "}", "",
    ).joinToString("\n")

    private val patch = """diff --git a/x.go b/x.go
index 1111111..2222222 100644
--- a/x.go
+++ b/x.go
@@ -3,5 +3,5 @@ package main
 import "fmt"

 func a() {
-	fmt.Println("a")
+	fmt.Println("A")
 }
@@ -12,4 +12,5 @@ func b() {

 func c() {
 	fmt.Println("c")
+	fmt.Println("C")
 }
"""

    private val after = before.replace("\"a\"", "\"A\"").replace("\tfmt.Println(\"c\")\n", "\tfmt.Println(\"c\")\n\tfmt.Println(\"C\")\n")

    @Test
    fun parsePatchReadsHunksLikeTheServerSplitsThem() {
        val p = parsePatch(patch)
        assertFalse(p.truncated)
        assertEquals(2, p.hunks.size)
        val h = p.hunks[0]
        assertEquals(listOf(3, 5, 3, 5), listOf(h.oldStart, h.oldLines, h.newStart, h.newLines))
        assertEquals(1, p.hunks[1].lines.count { it.op == '+' })
    }

    @Test
    fun reconstructAppliesThePatchToTheFileOnDisk() {
        val r = reconstruct(patch, "M", before)
        assertTrue(r.whole)
        assertEquals(before, r.before)
        assertEquals(after, r.after)
        assertEquals(listOf(LineRange(2, 7), LineRange(11, 16)), r.ranges)
        assertEquals(listOf(LineRange(2, 7), LineRange(11, 15)), r.beforeRanges)
    }

    @Test
    fun aHunkIsFoundWhenLinesMovedAboveIt() {
        val shifted = "// header\n// more\n$before"
        val r = reconstruct(patch, "M", shifted)
        assertTrue(r.whole)
        assertTrue(r.after.startsWith("// header\n// more\npackage main"))
        assertTrue(r.after.contains("\"A\"") && r.after.contains("\"C\""))
        assertEquals(LineRange(4, 9), r.ranges[0])
    }

    @Test
    fun aFileThatDiffersFromTheBaseFallsBackToTheHunks() {
        val r = reconstruct(patch, "M", before.replace("\"a\"", "\"x\""))
        assertFalse(r.whole)
        assertTrue(r.before.contains("lines 1–2 not shown"))
        assertTrue(r.after.contains("\"A\""))
        assertEquals(2, r.ranges.size)
        val a = r.after.split("\n")
        assertEquals("", a[r.ranges[1].start])
        assertEquals("\tfmt.Println(\"C\")", a[r.ranges[1].end - 2])
    }

    @Test
    fun addedAndDeletedFiles() {
        val add = "diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,2 @@\n+one\n+two\n"
        val a = reconstruct(add, "A", null)
        assertEquals(listOf("", "one\ntwo\n"), listOf(a.before, a.after))
        assertTrue(a.whole)
        val del = "diff --git a/n.txt b/n.txt\ndeleted file mode 100644\n--- a/n.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-one\n-two\n"
        val d = reconstruct(del, "D", null)
        assertEquals(listOf("one\ntwo\n", ""), listOf(d.before, d.after))
        assertTrue(d.whole)
    }

    @Test
    fun noNewlineAtEndOfFile() {
        val p = "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n one\n-two\n\\ No newline at end of file\n+TWO\n"
        val r = reconstruct(p, "M", "one\ntwo")
        assertTrue(r.whole)
        assertEquals("one\nTWO\n", r.after)
        // The disk file has a final newline but the base did not: not the base.
        assertFalse(reconstruct(p, "M", "one\ntwo\n").whole)
    }

    @Test
    fun crlfFilesKeepTheirLineEndings() {
        val p = "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n one\r\n-two\r\n+TWO\r\n"
        val r = reconstruct(p, "M", "one\r\ntwo\r\n")
        assertTrue(r.whole)
        assertEquals("one\r\nTWO\r\n", r.after)
        // The IDE's documents hold LF only; the lines (and so the hunk ranges) stay.
        assertEquals("one\nTWO\n", documentText(r.after))
    }

    @Test
    fun autocrlfAnLfPatchAppliesToACrlfCheckoutWholeFileNotHunksOnly() {
        // Found in a real VS Code on Windows (ROADMAP, "Verified for real"):
        // git diffs the normalized (LF) content while the working tree has
        // CRLF. The diff must show the whole file, not the hunks only.
        val disk = before.replace("\n", "\r\n")
        val r = reconstruct(patch, "M", disk)
        assertTrue(r.whole)
        assertEquals(before, r.before)
        assertEquals(after, r.after)
        assertEquals(listOf(LineRange(2, 7), LineRange(11, 16)), r.ranges)
        assertEquals(1, hunkAt(r.ranges, 12))
    }

    @Test
    fun autocrlfWithAMissingFinalNewline() {
        val p = "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n one\n-two\n\\ No newline at end of file\n+TWO\n\\ No newline at end of file\n"
        val r = reconstruct(p, "M", "one\r\ntwo")
        assertTrue(r.whole)
        assertEquals("one\nTWO", r.after)
    }

    @Test
    fun documentTextKeepsLineNumbersWithALoneCarriageReturn() {
        // A lone CR is not a line break in git's diff: turning it into one
        // would move every hunk below it by a line.
        val t = "a\r\nb\rc\nd\n"
        val doc = documentText(t)
        assertFalse(doc.contains('\r'))
        assertEquals(listOf("a", "b␍c", "d"), splitLines(doc).lines)
        val p = "--- a/f\n+++ b/f\n@@ -2,2 +2,2 @@\n b\rc\n-d\n+D\n"
        val r = reconstruct(p, "M", "a\nb\rc\nd\n")
        assertTrue(r.whole)
        assertEquals(listOf(LineRange(1, 3)), r.ranges)
        assertEquals("D", documentText(r.after).split("\n")[2])
    }

    @Test
    fun aTruncatedPatchShowsTheHunksItHas() {
        val r = reconstruct("$patch... (diff truncated) ...", "M", before)
        assertFalse(r.whole)
        assertTrue(r.after.contains("too large"))
    }

    @Test
    fun applyHunksRefusesAFileItCannotMatch() {
        assertNull(applyHunks(splitLines("nothing alike\n"), parsePatch(patch).hunks))
    }

    @Test
    fun hunkAtPicksTheHunkAtOrNearestALine() {
        val rs = listOf(LineRange(2, 7), LineRange(11, 16))
        assertEquals(0, hunkAt(rs, 0))
        assertEquals(0, hunkAt(rs, 4))
        assertEquals(1, hunkAt(rs, 12))
        assertEquals(1, hunkAt(rs, 30))
        assertEquals(-1, hunkAt(emptyList(), 3))
        // A pure deletion leaves an empty range in the after side.
        assertEquals(0, hunkAt(listOf(LineRange(5, 5)), 5))
    }

    @Test
    fun headerLineCountsAreNotTrustedAndHunksAreNumberedLikeTheServer() {
        val p = "--- a/f\n+++ b/f\n@@ -1,9 +1,9 @@\n a\n-b\n+B\n c\n@@ bogus @@\n x\n"
        val parsed = parsePatch(p)
        assertEquals(2, parsed.hunks.size)
        assertEquals(listOf(3, 3), listOf(parsed.hunks[0].oldLines, parsed.hunks[0].newLines))
        val r = reconstruct("--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n", "M", "a\nb\nc\nd\n")
        assertEquals("a\nB\nc\nd\n", r.after)
        val add = reconstruct("@@ -0,0 +1,5 @@\n+one\n+two\n", "A", null)
        assertEquals("one\ntwo\n", add.after)
        assertTrue(add.whole)
    }
}
