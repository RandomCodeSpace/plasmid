// Package outputlimit renders bounded output without splitting UTF-8 or CRLF.
package outputlimit

import (
	"math"
	"strconv"
	"strings"
)

const (
	ReasonBytes      = "bytes"
	ReasonLines      = "lines"
	ReasonLineLength = "line_length"
	ReasonBudget     = "budget"
	ReasonFile       = "file"
)

// Policy controls source content retained in rendered output. Non-positive
// limits are disabled.
type Policy struct {
	MaxBytes     int     `json:"max_bytes"`
	MaxLines     int     `json:"max_lines"`
	MaxLineBytes int     `json:"max_line_bytes"`
	HeadFraction float64 `json:"head_fraction"`
}

// Report describes source content removed from output. Marker bytes and
// aggregate separator bytes are not source content and are excluded.
type Report struct {
	Truncated     bool   `json:"truncated"`
	Reason        string `json:"reason"`
	OriginalBytes int    `json:"original_bytes"`
	OriginalLines int    `json:"original_lines"`
	KeptBytes     int    `json:"kept_bytes"`
	KeptLines     int    `json:"kept_lines"`
}

// Defaults returns the default output policy.
func Defaults() Policy {
	return Policy{
		MaxBytes:     30000,
		MaxLines:     2000,
		MaxLineBytes: 2000,
		HeadFraction: 0.6,
	}
}

// Marker returns the normative output-elision marker. Unknown reasons fail
// soft because marker construction is also used while reading foreign data.
func Marker(reason string, keptBytes, origBytes, keptLines, origLines int) string {
	if !validReason(reason) {
		return ""
	}
	return "[plasmid:truncated reason=" + reason +
		" bytes=" + strconv.Itoa(keptBytes) + "/" + strconv.Itoa(origBytes) +
		" lines=" + strconv.Itoa(keptLines) + "/" + strconv.Itoa(origLines) + "]"
}

func validReason(reason string) bool {
	switch reason {
	case ReasonBytes, ReasonLines, ReasonLineLength, ReasonBudget, ReasonFile:
		return true
	default:
		return false
	}
}

// Apply applies line-body elision before aggregate byte and line selection.
func (p Policy) Apply(input string) (string, Report) {
	lines := splitLines(input)
	prepared := make([]*preparedLine, 0, len(lines))
	processedBytes := 0
	lineElided := false
	for i, line := range lines {
		pl := prepareCompleteLine(i, line.body, line.term, p)
		prepared = append(prepared, pl)
		processedBytes += pl.sourceBytes
		lineElided = lineElided || pl.lineMarker != ""
	}
	return p.renderPrepared(input, prepared, len(input), len(lines), processedBytes, lineElided)
}

// ApplyLines renders logical lines separated by newlines. The first line
// number is reserved for consumers that annotate omitted source ranges.
func (p Policy) ApplyLines(lines []string, firstLineNumber int) (string, Report) {
	_ = firstLineNumber
	return p.Apply(strings.Join(lines, "\n"))
}

type sourceLine struct {
	body string
	term string
}

func splitLines(s string) []sourceLine {
	if s == "" {
		return nil
	}
	lines := make([]sourceLine, 0, strings.Count(s, "\n")+1)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' {
			continue
		}
		bodyEnd, termStart := i, i
		if i > start && s[i-1] == '\r' {
			bodyEnd, termStart = i-1, i-1
		}
		lines = append(lines, sourceLine{body: s[start:bodyEnd], term: s[termStart : i+1]})
		start = i + 1
	}
	if start < len(s) {
		lines = append(lines, sourceLine{body: s[start:]})
	}
	return lines
}

// preparedLine stores the logical source after line-length elision. Prefix and
// suffix samples are sufficient for aggregate selection; fullSource is present
// whenever the whole processed line can survive a Writer's byte ceiling.
type preparedLine struct {
	id           int
	sourceBytes  int
	prefixSource string
	suffixSource string
	fullSource   string
	lineMarker   string
	markerPos    int
}

func prepareCompleteLine(id int, body, term string, p Policy) *preparedLine {
	if p.MaxLineBytes <= 0 || len(body) <= p.MaxLineBytes {
		source := body + term
		return &preparedLine{
			id:           id,
			sourceBytes:  len(source),
			prefixSource: source,
			suffixSource: source,
			fullSource:   source,
		}
	}
	headLimit, tailLimit := limitSplit(p.MaxLineBytes, normalizedFraction(p.HeadFraction))
	headEnd := inwardHead(body, headLimit)
	tailStart := inwardTail(body, tailLimit)
	if tailStart < headEnd {
		tailStart = headEnd
	}
	head, tail := body[:headEnd], body[tailStart:]
	source := head + tail + term
	return &preparedLine{
		id:           id,
		sourceBytes:  len(source),
		prefixSource: source,
		suffixSource: source,
		fullSource:   source,
		lineMarker:   Marker(ReasonLineLength, len(head)+len(tail), len(body), 1, 1),
		markerPos:    len(head),
	}
}

func (p Policy) renderPrepared(input string, lines []*preparedLine, originalBytes, originalLines, processedBytes int, lineElided bool) (string, Report) {
	return p.renderPreparedCandidates(input, lines, lines, originalBytes, originalLines, processedBytes, lineElided)
}

func (p Policy) renderPreparedCandidates(input string, headCandidates, tailCandidates []*preparedLine, originalBytes, originalLines, processedBytes int, lineElided bool) (string, Report) {
	reason := p.truncationReason(processedBytes, originalLines)
	if reason == "" {
		return renderCompleteSelection(headCandidates, originalBytes, originalLines, processedBytes, lineElided)
	}

	head, tail := p.selectOuterEdges(headCandidates, tailCandidates)
	return renderTruncatedSelection(input, reason, head, tail, originalBytes, originalLines)
}

func (p Policy) truncationReason(processedBytes, originalLines int) string {
	if p.MaxBytes > 0 && processedBytes > p.MaxBytes {
		return ReasonBytes
	}
	if p.MaxLines > 0 && originalLines > p.MaxLines {
		return ReasonLines
	}
	return ""
}

func renderCompleteSelection(lines []*preparedLine, originalBytes, originalLines, processedBytes int, lineElided bool) (string, Report) {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line.renderFull())
	}
	report := Report{
		OriginalBytes: originalBytes,
		OriginalLines: originalLines,
		KeptBytes:     processedBytes,
		KeptLines:     originalLines,
	}
	if lineElided {
		report.Truncated = true
		report.Reason = ReasonLineLength
	}
	return b.String(), report
}

func (p Policy) selectOuterEdges(headCandidates, tailCandidates []*preparedLine) ([]lineSelection, []lineSelection) {
	fraction := normalizedFraction(p.HeadFraction)
	_, tailBytes := limitSplit(p.MaxBytes, fraction)
	_, tailLines := limitSplit(p.MaxLines, fraction)
	tail := selectSuffix(tailCandidates, tailBytes, tailLines, p.MaxBytes > 0, p.MaxLines > 0)

	// A byte split and a line split can constrain opposite sides. Redistribute
	// only unused allowance so neither constraint strands source that can still
	// be retained at an outer edge.
	headByteLimit := p.MaxBytes
	if p.MaxBytes > 0 {
		headByteLimit -= selectionBytes(tail)
	}
	head := selectPrefixWithin(
		headCandidates,
		headByteLimit,
		p.MaxLines,
		p.MaxBytes > 0,
		p.MaxLines > 0,
		tail,
	)
	tailByteLimit := p.MaxBytes
	if p.MaxBytes > 0 {
		tailByteLimit -= selectionBytes(head)
	}
	tail = selectSuffixWithin(
		tailCandidates,
		tailByteLimit,
		p.MaxLines,
		p.MaxBytes > 0,
		p.MaxLines > 0,
		head,
	)
	return head, tail
}

func renderTruncatedSelection(input, reason string, head, tail []lineSelection, originalBytes, originalLines int) (string, Report) {
	keptBytes := selectionBytes(head) + selectionBytes(tail)
	keptLines := distinctSelectionLines(head, tail)

	var b strings.Builder
	if prefix := renderSelections(head); prefix != "" {
		b.WriteString(prefix)
		if !strings.HasSuffix(prefix, "\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString(Marker(reason, keptBytes, originalBytes, keptLines, originalLines))
	if suffix := renderSelections(tail); suffix != "" {
		b.WriteByte('\n')
		b.WriteString(suffix)
	} else if strings.HasSuffix(input, "\r\n") {
		b.WriteString("\r\n")
	} else if strings.HasSuffix(input, "\n") {
		b.WriteByte('\n')
	}
	return b.String(), Report{
		Truncated:     true,
		Reason:        reason,
		OriginalBytes: originalBytes,
		OriginalLines: originalLines,
		KeptBytes:     keptBytes,
		KeptLines:     keptLines,
	}
}

type lineSelection struct {
	line       *preparedLine
	start, end int
}

type selectionBudget struct {
	remainingBytes int
	remainingLines int
	useBytes       bool
	useLines       bool
	occupied       map[int]struct{}
}

func newSelectionBudget(byteLimit, lineLimit int, useBytes, useLines bool, occupied []lineSelection) (selectionBudget, bool) {
	budget := selectionBudget{
		remainingBytes: byteLimit,
		remainingLines: lineLimit,
		useBytes:       useBytes,
		useLines:       useLines,
		occupied:       selectionLineIDs(occupied),
	}
	if useLines {
		budget.remainingLines -= len(budget.occupied)
	}
	return budget, budget.remainingLines >= 0
}

func (b selectionBudget) canSelect(line *preparedLine) bool {
	_, occupied := b.occupied[line.id]
	if b.useLines && b.remainingLines == 0 && !occupied {
		return false
	}
	return !b.useBytes || b.remainingBytes != 0 || line.sourceBytes == 0
}

func (b *selectionBudget) consume(line *preparedLine, kept int) {
	if b.useBytes {
		b.remainingBytes -= kept
	}
	if _, occupied := b.occupied[line.id]; b.useLines && !occupied {
		b.remainingLines--
	}
}

func selectPrefixWithin(lines []*preparedLine, byteLimit, lineLimit int, useBytes, useLines bool, occupied []lineSelection) []lineSelection {
	budget, valid := newSelectionBudget(byteLimit, lineLimit, useBytes, useLines, occupied)
	if !valid {
		return nil
	}
	selected := make([]lineSelection, 0)
	for _, line := range lines {
		if !budget.canSelect(line) {
			break
		}
		end := line.sourceBytes
		if useBytes && end > budget.remainingBytes {
			end = line.safePrefix(budget.remainingBytes)
		}
		if end == 0 && line.sourceBytes != 0 {
			break
		}
		selected = append(selected, lineSelection{line: line, end: end})
		budget.consume(line, end)
		if end != line.sourceBytes {
			break
		}
	}
	return selected
}

func selectSuffix(lines []*preparedLine, byteLimit, lineLimit int, useBytes, useLines bool) []lineSelection {
	return selectSuffixWithin(lines, byteLimit, lineLimit, useBytes, useLines, nil)
}

func selectSuffixWithin(lines []*preparedLine, byteLimit, lineLimit int, useBytes, useLines bool, occupied []lineSelection) []lineSelection {
	budget, valid := newSelectionBudget(byteLimit, lineLimit, useBytes, useLines, occupied)
	if !valid {
		return nil
	}
	reversed := make([]lineSelection, 0)
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !budget.canSelect(line) {
			break
		}
		start := 0
		if useBytes && line.sourceBytes > budget.remainingBytes {
			start = line.safeSuffixStart(budget.remainingBytes)
		}
		kept := line.sourceBytes - start
		if kept == 0 && line.sourceBytes != 0 {
			break
		}
		reversed = append(reversed, lineSelection{line: line, start: start, end: line.sourceBytes})
		budget.consume(line, kept)
		if start != 0 {
			break
		}
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func (line *preparedLine) safePrefix(limit int) int {
	if limit <= 0 {
		return 0
	}
	if limit >= line.sourceBytes {
		return line.sourceBytes
	}
	return inwardHead(line.prefixSource, limit)
}

func (line *preparedLine) safeSuffixStart(limit int) int {
	if limit <= 0 {
		return line.sourceBytes
	}
	if limit >= line.sourceBytes {
		return 0
	}
	local := inwardTail(line.suffixSource, limit)
	return line.sourceBytes - (len(line.suffixSource) - local)
}

func (line *preparedLine) renderFull() string {
	if line.lineMarker == "" {
		return line.fullSource
	}
	return line.fullSource[:line.markerPos] + line.lineMarker + line.fullSource[line.markerPos:]
}

func (line *preparedLine) renderRange(start, end int) string {
	if start == 0 && end == line.sourceBytes {
		return line.renderFull()
	}
	var source string
	switch {
	case start == 0:
		source = line.prefixSource[:end]
	case end == line.sourceBytes:
		kept := end - start
		source = line.suffixSource[len(line.suffixSource)-kept:]
	default:
		panic("outputlimit: internal non-edge line selection")
	}
	if line.lineMarker == "" || line.markerPos <= start || line.markerPos >= end {
		return source
	}
	markerOffset := line.markerPos - start
	return source[:markerOffset] + line.lineMarker + source[markerOffset:]
}

func renderSelections(selections []lineSelection) string {
	var b strings.Builder
	for _, selection := range selections {
		b.WriteString(selection.line.renderRange(selection.start, selection.end))
	}
	return b.String()
}

func selectionBytes(selections []lineSelection) int {
	total := 0
	for _, selection := range selections {
		total += selection.end - selection.start
	}
	return total
}

func distinctSelectionLines(groups ...[]lineSelection) int {
	return len(selectionLineIDs(groups...))
}

func selectionLineIDs(groups ...[]lineSelection) map[int]struct{} {
	seen := make(map[int]struct{})
	for _, selections := range groups {
		for _, selection := range selections {
			seen[selection.line.id] = struct{}{}
		}
	}
	return seen
}

func normalizedFraction(fraction float64) float64 {
	if math.IsNaN(fraction) {
		return 0.5
	}
	if fraction < 0 {
		return 0
	}
	if fraction > 1 {
		return 1
	}
	return fraction
}

func limitSplit(limit int, fraction float64) (int, int) {
	if limit <= 0 {
		return 0, 0
	}
	if fraction <= 0 {
		return 0, limit
	}
	if fraction >= 1 {
		return limit, 0
	}
	head := int(math.Floor(float64(limit) * fraction))
	if head < 0 {
		head = 0
	} else if head > limit {
		head = limit
	}
	return head, limit - head
}

func inwardHead(s string, limit int) int {
	if limit <= 0 {
		return 0
	}
	if limit >= len(s) {
		return len(s)
	}
	end := limit
	for end > 0 && s[end]&0xc0 == 0x80 {
		end--
	}
	if end > 0 && end < len(s) && s[end-1] == '\r' && s[end] == '\n' {
		end--
	}
	return end
}

func inwardTail(s string, limit int) int {
	if limit <= 0 {
		return len(s)
	}
	if limit >= len(s) {
		return 0
	}
	start := len(s) - limit
	for start < len(s) && s[start]&0xc0 == 0x80 {
		start++
	}
	if start > 0 && start < len(s) && s[start-1] == '\r' && s[start] == '\n' {
		start++
	}
	return start
}
