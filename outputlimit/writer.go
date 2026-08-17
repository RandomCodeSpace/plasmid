package outputlimit

import (
	"sync"
	"unicode/utf8"
)

// Writer retains bounded head and tail samples from a byte stream.
type Writer struct {
	mu sync.RWMutex

	policy    Policy
	max       int
	unlimited bool
	head      []byte
	tail      []byte
	tailAt    int
	total     int

	newlines int
	prevByte byte
	lastByte byte
	hasLast  bool

	headBoundary    [utf8.UTFMax]byte
	headBoundaryLen int
	beforeTail      byte
	hasBeforeTail   bool
}

// NewWriter constructs a writer whose dynamic buffers never exceed twice the
// policy's aggregate byte limit when MaxBytes is positive. A zero MaxBytes
// preserves the policy's unlimited-output meaning.
func NewWriter(policy Policy) (*Writer, error) {
	if policy.MaxBytes < 0 {
		return nil, ErrInvalidLimit
	}
	if policy.MaxBytes == 0 {
		return &Writer{policy: policy, unlimited: true}, nil
	}
	return &Writer{
		policy: policy,
		max:    policy.MaxBytes,
		head:   make([]byte, 0, policy.MaxBytes),
		tail:   make([]byte, 0, policy.MaxBytes),
	}, nil
}

// Write implements io.Writer. Counter overflow rejects the whole chunk.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > maxIntValue()-w.total {
		return 0, ErrCounterOverflow
	}
	for _, value := range p {
		if value == '\n' {
			w.newlines++
		}
		if w.hasLast {
			w.prevByte = w.lastByte
		}
		w.lastByte = value
		w.hasLast = true
		if w.unlimited || len(w.head) < w.max {
			w.head = append(w.head, value)
		} else if w.headBoundaryLen < len(w.headBoundary) {
			w.headBoundary[w.headBoundaryLen] = value
			w.headBoundaryLen++
		}
		if !w.unlimited {
			w.appendTail(value)
		}
	}
	w.total += len(p)
	return len(p), nil
}

func (w *Writer) appendTail(value byte) {
	if len(w.tail) < w.max {
		w.tail = append(w.tail, value)
		return
	}
	w.beforeTail = w.tail[w.tailAt]
	w.hasBeforeTail = true
	w.tail[w.tailAt] = value
	w.tailAt++
	if w.tailAt == len(w.tail) {
		w.tailAt = 0
	}
}

// String renders the captured stream and its truncation report.
func (w *Writer) String() (string, Report) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.unlimited || w.total <= w.max {
		value := string(w.head[:w.total])
		lines := logicalLineCount(w.newlines, w.total, w.lastByte)
		return value, Report{
			OriginalBytes: w.total,
			OriginalLines: lines,
			KeptBytes:     w.total,
			KeptLines:     lines,
		}
	}

	fraction := normalizedFraction(w.policy.HeadFraction)
	_, tailLimit := limitSplit(w.max, fraction)
	headSample := make([]byte, 0, len(w.head)+w.headBoundaryLen)
	headSample = append(headSample, w.head...)
	headSample = append(headSample, w.headBoundary[:w.headBoundaryLen]...)
	orderedTail := w.orderedTail()
	suffix := w.tailWithin(orderedTail, tailLimit)
	prefix := string(w.head[:inwardHead(string(headSample), w.max-len(suffix))])
	suffix = w.tailWithin(orderedTail, w.max-len(prefix))
	keptBytes := len(prefix) + len(suffix)
	originalLines := logicalLineCount(w.newlines, w.total, w.lastByte)
	keptLines := selectedLineCount(prefix, suffix, w.newlines)

	result := prefix
	if result != "" && result[len(result)-1] != '\n' {
		result += "\n"
	}
	result += Marker(ReasonBytes, keptBytes, w.total, keptLines, originalLines)
	if suffix != "" {
		result += "\n" + suffix
	} else if w.lastByte == '\n' {
		if w.prevByte == '\r' {
			result += "\r\n"
		} else {
			result += "\n"
		}
	}
	return result, Report{
		Truncated:     true,
		Reason:        ReasonBytes,
		OriginalBytes: w.total,
		OriginalLines: originalLines,
		KeptBytes:     keptBytes,
		KeptLines:     keptLines,
	}
}

func (w *Writer) tailWithin(ordered []byte, limit int) string {
	start := inwardTail(string(ordered), limit)
	for start < len(ordered) && ordered[start]&0xc0 == 0x80 {
		start++
	}
	if start == 0 && w.hasBeforeTail && w.beforeTail == '\r' && len(ordered) > 0 && ordered[0] == '\n' {
		start++
	}
	return string(ordered[start:])
}

// Result is an alias for String.
func (w *Writer) Result() (string, Report) { return w.String() }

func (w *Writer) orderedTail() []byte {
	if len(w.tail) < w.max || w.tailAt == 0 {
		return append([]byte(nil), w.tail...)
	}
	ordered := make([]byte, 0, len(w.tail))
	ordered = append(ordered, w.tail[w.tailAt:]...)
	ordered = append(ordered, w.tail[:w.tailAt]...)
	return ordered
}

func logicalLineCount(newlines, total int, last byte) int {
	if total == 0 {
		return 0
	}
	if last == '\n' {
		return newlines
	}
	return newlines + 1
}

func selectedLineCount(prefix, suffix string, totalNewlines int) int {
	count := len(splitLines(prefix)) + len(splitLines(suffix))
	omittedNewlines := totalNewlines - countByte(prefix, '\n') - countByte(suffix, '\n')
	if prefix != "" && prefix[len(prefix)-1] != '\n' && suffix != "" && omittedNewlines == 0 {
		count--
	}
	return count
}

func countByte(value string, target byte) int {
	count := 0
	for i := 0; i < len(value); i++ {
		if value[i] == target {
			count++
		}
	}
	return count
}

func maxIntValue() int { return int(^uint(0) >> 1) }

// Reset discards the captured stream and accounting state.
func (w *Writer) Reset() {
	w.mu.Lock()
	w.head = w.head[:0]
	w.tail = w.tail[:0]
	w.tailAt = 0
	w.total = 0
	w.newlines = 0
	w.prevByte = 0
	w.lastByte = 0
	w.hasLast = false
	w.headBoundary = [utf8.UTFMax]byte{}
	w.headBoundaryLen = 0
	w.beforeTail = 0
	w.hasBeforeTail = false
	w.mu.Unlock()
}
