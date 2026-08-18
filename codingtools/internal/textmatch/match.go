package textmatch

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrEmptyOld       = errors.New("old text is empty")
	ErrNoOpEdit       = errors.New("old text and new text are identical")
	ErrNoMatch        = errors.New("old text did not match content")
	ErrAmbiguousMatch = errors.New("old text matched multiple locations")
)

type Request struct {
	Content    string
	Old        string
	New        string
	ReplaceAll bool
}

// Range is a half-open byte range in the normalized, BOM-free source content.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Result struct {
	Content      string
	Replacements int
	Tier         Tier
	Ranges       []Range
	LineEnding   string
	HadBOM       bool
}

type Tier int

const (
	TierExact Tier = iota
	TierTrailingWhitespace
	TierIndentation
)

func (t Tier) String() string {
	switch t {
	case TierExact:
		return "exact"
	case TierTrailingWhitespace:
		return "whitespace"
	case TierIndentation:
		return "indentation"
	default:
		return "unknown"
	}
}

type AmbiguityError struct {
	Count int
	Lines []int
}

func (e *AmbiguityError) Error() string {
	lines := e.Lines
	if len(lines) > 5 {
		lines = lines[:5]
	}
	return fmt.Sprintf("old text matched %d locations at lines %v; add surrounding context or set replace_all", e.Count, lines)
}

func (e *AmbiguityError) Unwrap() error {
	return ErrAmbiguousMatch
}

type match struct {
	start int
	end   int
	line  int
	delta string
}

type logicalLine struct {
	text       string
	start      int
	end        int
	fullEnd    int
	terminated bool
}

func Apply(req Request) (Result, error) {
	if req.Old == "" {
		return Result{}, ErrEmptyOld
	}
	if req.Old == req.New {
		return Result{}, ErrNoOpEdit
	}

	source := normalize(req.Content)
	oldText := normalize(req.Old).content
	newText := normalize(req.New).content
	if oldText == "" {
		return Result{}, ErrEmptyOld
	}
	if oldText == newText {
		return Result{}, ErrNoOpEdit
	}

	tier := TierExact
	matches := exactMatches(source.content, oldText)
	if len(matches) == 0 {
		tier = TierTrailingWhitespace
		matches = lineMatches(source.content, oldText, false)
	}
	if len(matches) == 0 {
		tier = TierIndentation
		matches = lineMatches(source.content, oldText, true)
	}
	if len(matches) == 0 {
		return Result{}, ErrNoMatch
	}
	if len(matches) > 1 && !req.ReplaceAll {
		lines := make([]int, len(matches))
		for i, candidate := range matches {
			lines[i] = candidate.line
		}
		return Result{}, &AmbiguityError{Count: len(matches), Lines: lines}
	}
	if !req.ReplaceAll {
		matches = matches[:1]
	}

	var b strings.Builder
	b.Grow(len(source.content) + len(matches)*(len(newText)-len(oldText)))
	previous := 0
	ranges := make([]Range, 0, len(matches))
	for _, candidate := range matches {
		b.WriteString(source.content[previous:candidate.start])
		replacement := newText
		if tier == TierIndentation {
			replacement = indentReplacement(replacement, candidate.delta)
		}
		b.WriteString(replacement)
		ranges = append(ranges, Range{Start: candidate.start, End: candidate.end})
		previous = candidate.end
	}
	b.WriteString(source.content[previous:])

	content := restore(b.String(), source.lineEnding, source.hadBOM, source.trailingLF)
	return Result{
		Content:      content,
		Replacements: len(matches),
		Tier:         tier,
		Ranges:       ranges,
		LineEnding:   source.lineEnding,
		HadBOM:       source.hadBOM,
	}, nil
}

func exactMatches(content, oldText string) []match {
	var matches []match
	for offset := 0; offset <= len(content)-len(oldText); {
		relative := strings.Index(content[offset:], oldText)
		if relative < 0 {
			break
		}
		start := offset + relative
		end := start + len(oldText)
		matches = append(matches, match{start: start, end: end, line: lineNumber(content, start)})
		offset = end
	}
	return matches
}

func lineMatches(content, oldText string, allowIndentation bool) []match {
	contentLines := splitLogicalLines(content)
	oldLines := splitLogicalLines(oldText)
	if len(oldLines) == 0 || len(oldLines) > len(contentLines) {
		return nil
	}
	oldTrailingLF := strings.HasSuffix(oldText, "\n")
	var matches []match
	for start := 0; start+len(oldLines) <= len(contentLines); {
		endLine := start + len(oldLines) - 1
		if oldTrailingLF && !contentLines[endLine].terminated {
			start++
			continue
		}
		delta, ok := compareLineWindow(contentLines[start:start+len(oldLines)], oldLines, allowIndentation)
		if !ok {
			start++
			continue
		}
		end := contentLines[endLine].end
		if oldTrailingLF {
			end = contentLines[endLine].fullEnd
		}
		matches = append(matches, match{
			start: contentLines[start].start,
			end:   end,
			line:  start + 1,
			delta: delta,
		})
		start += len(oldLines)
	}
	return matches
}

func compareLineWindow(content, old []logicalLine, allowIndentation bool) (string, bool) {
	delta := ""
	deltaSet := false
	for i := range old {
		contentLine := strings.TrimRight(content[i].text, " \t")
		oldLine := strings.TrimRight(old[i].text, " \t")
		if !allowIndentation {
			if contentLine != oldLine {
				return "", false
			}
			continue
		}
		if contentLine == "" && oldLine == "" {
			continue
		}
		if !strings.HasSuffix(contentLine, oldLine) {
			return "", false
		}
		candidate := strings.TrimSuffix(contentLine, oldLine)
		if candidate == "" || strings.Trim(candidate, " \t") != "" {
			return "", false
		}
		if !deltaSet {
			delta = candidate
			deltaSet = true
		} else if candidate != delta {
			return "", false
		}
	}
	if !allowIndentation {
		return "", true
	}
	if !deltaSet {
		return "", false
	}
	return delta, true
}

func splitLogicalLines(s string) []logicalLine {
	if s == "" {
		return nil
	}
	lines := make([]logicalLine, 0, strings.Count(s, "\n")+1)
	for start := 0; start < len(s); {
		relative := strings.IndexByte(s[start:], '\n')
		if relative < 0 {
			lines = append(lines, logicalLine{text: s[start:], start: start, end: len(s), fullEnd: len(s)})
			break
		}
		end := start + relative
		lines = append(lines, logicalLine{text: s[start:end], start: start, end: end, fullEnd: end + 1, terminated: true})
		start = end + 1
	}
	return lines
}

func indentReplacement(s, delta string) string {
	if s == "" || delta == "" {
		return s
	}
	lines := splitLogicalLines(s)
	var b strings.Builder
	b.Grow(len(s) + len(lines)*len(delta))
	for _, line := range lines {
		b.WriteString(delta)
		b.WriteString(line.text)
		if line.terminated {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func lineNumber(s string, offset int) int {
	return strings.Count(s[:offset], "\n") + 1
}
