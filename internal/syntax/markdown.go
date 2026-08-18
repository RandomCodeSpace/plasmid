package syntax

import (
	"sort"
	"strings"
)

// CodeRegionKind identifies inline and fenced Markdown code.
type CodeRegionKind string

const (
	CodeRegionInline CodeRegionKind = "inline"
	CodeRegionFence  CodeRegionKind = "fence"
)

// CodeRegion contains half-open byte offsets. Start and End include Markdown
// delimiters; ContentStart and ContentEnd exclude them.
type CodeRegion struct {
	Kind         CodeRegionKind `json:"kind"`
	Start        int            `json:"start"`
	End          int            `json:"end"`
	ContentStart int            `json:"content_start"`
	ContentEnd   int            `json:"content_end"`
}

// ScanCodeRegions returns deterministic non-overlapping Markdown code spans.
// It recognizes CommonMark-style backtick and tilde fences with up to three
// leading spaces and backtick inline code spans outside fences.
func ScanCodeRegions(source string) []CodeRegion {
	fences := scanFences(source)
	regions := append([]CodeRegion(nil), fences...)
	cursor := 0
	for _, fence := range fences {
		regions = append(regions, scanInlineCode(source, cursor, fence.Start)...)
		cursor = fence.End
	}
	regions = append(regions, scanInlineCode(source, cursor, len(source))...)
	sort.Slice(regions, func(i, j int) bool { return regions[i].Start < regions[j].Start })
	return regions
}

// IsCodeOffset reports whether an offset is inside a code region, including
// its delimiters.
func IsCodeOffset(regions []CodeRegion, offset int) bool {
	index := sort.Search(len(regions), func(index int) bool { return regions[index].End > offset })
	return index < len(regions) && regions[index].Start <= offset
}

func scanFences(source string) []CodeRegion {
	var regions []CodeRegion
	for lineStart := 0; lineStart < len(source); {
		lineEnd, nextLine := markdownLineEnd(source, lineStart)
		marker, count, ok := openingFence(source[lineStart:lineEnd])
		if !ok {
			lineStart = nextLine
			continue
		}
		contentStart := nextLine
		closeStart, closeEnd := len(source), len(source)
		for candidate := nextLine; candidate < len(source); {
			candidateEnd, candidateNext := markdownLineEnd(source, candidate)
			if closingFence(source[candidate:candidateEnd], marker, count) {
				closeStart, closeEnd = candidate, candidateNext
				break
			}
			candidate = candidateNext
		}
		regions = append(regions, CodeRegion{
			Kind: CodeRegionFence, Start: lineStart, End: closeEnd,
			ContentStart: contentStart, ContentEnd: closeStart,
		})
		lineStart = closeEnd
	}
	return regions
}

func openingFence(line string) (byte, int, bool) {
	indent := leadingSpaces(line)
	if indent > 3 || indent >= len(line) || (line[indent] != '`' && line[indent] != '~') {
		return 0, 0, false
	}
	marker := line[indent]
	count := markerRun(line, indent, marker)
	if count < 3 {
		return 0, 0, false
	}
	if marker == '`' && strings.ContainsRune(line[indent+count:], '`') {
		return 0, 0, false
	}
	return marker, count, true
}

func closingFence(line string, marker byte, minimum int) bool {
	indent := leadingSpaces(line)
	if indent > 3 || indent >= len(line) || line[indent] != marker {
		return false
	}
	count := markerRun(line, indent, marker)
	return count >= minimum && strings.TrimSpace(line[indent+count:]) == ""
}

func scanInlineCode(source string, start, end int) []CodeRegion {
	var regions []CodeRegion
	for index := start; index < end; {
		if source[index] != '`' || escapedMarkdownByte(source, index) {
			index++
			continue
		}
		count := markerRun(source[:end], index, '`')
		close := findInlineClose(source, index+count, end, count)
		if close < 0 {
			index += count
			continue
		}
		regions = append(regions, CodeRegion{
			Kind: CodeRegionInline, Start: index, End: close + count,
			ContentStart: index + count, ContentEnd: close,
		})
		index = close + count
	}
	return regions
}

func findInlineClose(source string, start, end, count int) int {
	for index := start; index < end; {
		if source[index] != '`' || escapedMarkdownByte(source, index) {
			index++
			continue
		}
		run := markerRun(source[:end], index, '`')
		if run == count {
			return index
		}
		index += run
	}
	return -1
}

func markdownLineEnd(source string, start int) (int, int) {
	if newline := strings.IndexByte(source[start:], '\n'); newline >= 0 {
		end := start + newline
		return end, end + 1
	}
	return len(source), len(source)
}

func leadingSpaces(value string) int {
	index := 0
	for index < len(value) && value[index] == ' ' {
		index++
	}
	return index
}

func markerRun(value string, start int, marker byte) int {
	index := start
	for index < len(value) && value[index] == marker {
		index++
	}
	return index - start
}

func escapedMarkdownByte(source string, index int) bool {
	backslashes := 0
	for index--; index >= 0 && source[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}
