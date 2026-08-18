package textmatch

import (
	"fmt"
	"strings"
)

const (
	defaultContextLines = 3
	maxMyersWorkPoints  = 100000
)

type diffKind byte

const (
	diffEqual  diffKind = ' '
	diffDelete diffKind = '-'
	diffInsert diffKind = '+'
)

type diffLine struct {
	text       string
	terminated bool
}

type diffOp struct {
	kind diffKind
	old  diffLine
	new  diffLine
}

type diffHunk struct {
	start int
	end   int
}

func UnifiedDiff(oldText, newText, path string, contextLines int) string {
	return unifiedDiff(diffLines(oldText), diffLines(newText), path, contextLines)
}

// UnifiedDiffExact emits a line diff without normalizing BOMs or line endings.
// It is used when the byte-level replacement itself is the operation being
// reported, rather than a normalized text edit.
func UnifiedDiffExact(oldText, newText, path string, contextLines int) string {
	return unifiedDiff(exactDiffLines(oldText), exactDiffLines(newText), path, contextLines)
}

func unifiedDiff(oldLines, newLines []diffLine, path string, contextLines int) string {
	if contextLines == 0 {
		contextLines = defaultContextLines
	}
	if contextLines < 0 {
		contextLines = 0
	}

	if equalDiffLines(oldLines, newLines) {
		return ""
	}

	ops, ok := myersDiff(oldLines, newLines)
	if !ok {
		ops = wholeFileDiff(oldLines, newLines)
	}
	hunks := groupHunks(ops, contextLines)
	if len(hunks) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	oldPosition, newPosition := 0, 0
	operation := 0
	for _, hunk := range hunks {
		for operation < hunk.start {
			advancePositions(ops[operation], &oldPosition, &newPosition)
			operation++
		}
		oldCount, newCount := hunkCounts(ops[hunk.start:hunk.end])
		oldStart := oldPosition + 1
		newStart := newPosition + 1
		if oldCount == 0 {
			oldStart = oldPosition
		}
		if newCount == 0 {
			newStart = newPosition
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for operation < hunk.end {
			op := ops[operation]
			renderDiffOp(&b, op)
			advancePositions(op, &oldPosition, &newPosition)
			operation++
		}
	}
	return b.String()
}

func exactDiffLines(s string) []diffLine {
	if s == "" {
		return nil
	}
	lines := make([]diffLine, 0, strings.Count(s, "\n")+1)
	for start := 0; start < len(s); {
		relative := strings.IndexByte(s[start:], '\n')
		if relative < 0 {
			lines = append(lines, diffLine{text: s[start:]})
			break
		}
		end := start + relative
		lines = append(lines, diffLine{text: s[start:end], terminated: true})
		start = end + 1
	}
	return lines
}

func diffLines(s string) []diffLine {
	s = normalize(s).content
	logical := splitLogicalLines(s)
	lines := make([]diffLine, len(logical))
	for i, line := range logical {
		lines[i] = diffLine{text: line.text, terminated: line.terminated}
	}
	return lines
}

func equalDiffLines(a, b []diffLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func myersDiff(oldLines, newLines []diffLine) ([]diffOp, bool) {
	workPoints := 0
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
		workPoints++
		if workPoints > maxMyersWorkPoints {
			return nil, false
		}
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
		workPoints++
		if workPoints > maxMyersWorkPoints {
			return nil, false
		}
	}

	oldMiddle := oldLines[prefix : len(oldLines)-suffix]
	newMiddle := newLines[prefix : len(newLines)-suffix]
	middle, ok := myersMiddleWithBudget(oldMiddle, newMiddle, maxMyersWorkPoints-workPoints)
	if !ok {
		return nil, false
	}

	ops := make([]diffOp, 0, prefix+len(middle)+suffix)
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{kind: diffEqual, old: oldLines[i], new: newLines[i]})
	}
	ops = append(ops, middle...)
	for i := len(oldLines) - suffix; i < len(oldLines); i++ {
		newIndex := len(newLines) - (len(oldLines) - i)
		ops = append(ops, diffOp{kind: diffEqual, old: oldLines[i], new: newLines[newIndex]})
	}
	return ops, true
}

func myersMiddle(oldLines, newLines []diffLine) ([]diffOp, bool) {
	return myersMiddleWithBudget(oldLines, newLines, maxMyersWorkPoints)
}

func myersMiddleWithBudget(oldLines, newLines []diffLine, workBudget int) ([]diffOp, bool) {
	if len(oldLines) == 0 {
		if len(newLines) > workBudget {
			return nil, false
		}
		ops := make([]diffOp, len(newLines))
		for i, line := range newLines {
			ops[i] = diffOp{kind: diffInsert, new: line}
		}
		return ops, true
	}
	if len(newLines) == 0 {
		if len(oldLines) > workBudget {
			return nil, false
		}
		ops := make([]diffOp, len(oldLines))
		for i, line := range oldLines {
			ops[i] = diffOp{kind: diffDelete, old: line}
		}
		return ops, true
	}

	maxDistance := len(oldLines) + len(newLines)
	frontier := map[int]int{1: 0}
	trace := make([]map[int]int, 0, 448)
	workPoints := 0
	for distance := 0; distance <= maxDistance; distance++ {
		trace = append(trace, cloneFrontier(frontier))
		for diagonal := -distance; diagonal <= distance; diagonal += 2 {
			workPoints++
			if workPoints > workBudget {
				return nil, false
			}
			var x int
			if diagonal == -distance || (diagonal != distance && frontier[diagonal-1] < frontier[diagonal+1]) {
				x = frontier[diagonal+1]
			} else {
				x = frontier[diagonal-1] + 1
			}
			y := x - diagonal
			for x < len(oldLines) && y < len(newLines) && oldLines[x] == newLines[y] {
				workPoints++
				if workPoints > workBudget {
					return nil, false
				}
				x++
				y++
			}
			frontier[diagonal] = x
			if x >= len(oldLines) && y >= len(newLines) {
				return backtrackMyers(trace, oldLines, newLines, distance), true
			}
		}
	}
	return nil, false
}

func cloneFrontier(source map[int]int) map[int]int {
	clone := make(map[int]int, len(source))
	for diagonal, x := range source {
		clone[diagonal] = x
	}
	return clone
}

func backtrackMyers(trace []map[int]int, oldLines, newLines []diffLine, distance int) []diffOp {
	x, y := len(oldLines), len(newLines)
	reversed := make([]diffOp, 0, x+y)
	for d := distance; d > 0; d-- {
		frontier := trace[d]
		diagonal := x - y
		var previousDiagonal int
		if diagonal == -d || (diagonal != d && frontier[diagonal-1] < frontier[diagonal+1]) {
			previousDiagonal = diagonal + 1
		} else {
			previousDiagonal = diagonal - 1
		}
		previousX := frontier[previousDiagonal]
		previousY := previousX - previousDiagonal
		for x > previousX && y > previousY {
			reversed = append(reversed, diffOp{kind: diffEqual, old: oldLines[x-1], new: newLines[y-1]})
			x--
			y--
		}
		if x == previousX {
			reversed = append(reversed, diffOp{kind: diffInsert, new: newLines[y-1]})
			y--
		} else {
			reversed = append(reversed, diffOp{kind: diffDelete, old: oldLines[x-1]})
			x--
		}
	}
	for x > 0 && y > 0 {
		reversed = append(reversed, diffOp{kind: diffEqual, old: oldLines[x-1], new: newLines[y-1]})
		x--
		y--
	}
	for x > 0 {
		reversed = append(reversed, diffOp{kind: diffDelete, old: oldLines[x-1]})
		x--
	}
	for y > 0 {
		reversed = append(reversed, diffOp{kind: diffInsert, new: newLines[y-1]})
		y--
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func wholeFileDiff(oldLines, newLines []diffLine) []diffOp {
	ops := make([]diffOp, 0, len(oldLines)+len(newLines))
	for _, line := range oldLines {
		ops = append(ops, diffOp{kind: diffDelete, old: line})
	}
	for _, line := range newLines {
		ops = append(ops, diffOp{kind: diffInsert, new: line})
	}
	return ops
}

func groupHunks(ops []diffOp, contextLines int) []diffHunk {
	var hunks []diffHunk
	for index, op := range ops {
		if op.kind == diffEqual {
			continue
		}
		start := max(0, index-contextLines)
		end := min(len(ops), index+contextLines+1)
		if len(hunks) > 0 && start <= hunks[len(hunks)-1].end {
			if end > hunks[len(hunks)-1].end {
				hunks[len(hunks)-1].end = end
			}
			continue
		}
		hunks = append(hunks, diffHunk{start: start, end: end})
	}
	return hunks
}

func hunkCounts(ops []diffOp) (int, int) {
	oldCount, newCount := 0, 0
	for _, op := range ops {
		switch op.kind {
		case diffEqual:
			oldCount++
			newCount++
		case diffDelete:
			oldCount++
		case diffInsert:
			newCount++
		}
	}
	return oldCount, newCount
}

func advancePositions(op diffOp, oldPosition, newPosition *int) {
	switch op.kind {
	case diffEqual:
		*oldPosition++
		*newPosition++
	case diffDelete:
		*oldPosition++
	case diffInsert:
		*newPosition++
	}
}

func renderDiffOp(b *strings.Builder, op diffOp) {
	b.WriteByte(byte(op.kind))
	switch op.kind {
	case diffEqual:
		b.WriteString(op.old.text)
		b.WriteByte('\n')
		if !op.old.terminated || !op.new.terminated {
			b.WriteString("\\ No newline at end of file\n")
		}
	case diffDelete:
		b.WriteString(op.old.text)
		b.WriteByte('\n')
		if !op.old.terminated {
			b.WriteString("\\ No newline at end of file\n")
		}
	case diffInsert:
		b.WriteString(op.new.text)
		b.WriteByte('\n')
		if !op.new.terminated {
			b.WriteString("\\ No newline at end of file\n")
		}
	}
}
