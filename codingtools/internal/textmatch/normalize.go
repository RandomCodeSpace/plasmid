package textmatch

import "strings"

const utf8BOM = "\uFEFF"

type normalizedText struct {
	content    string
	lineEnding string
	hadBOM     bool
	trailingLF bool
}

func normalize(s string) normalizedText {
	lineEnding := detectLineEnding(s)
	s, hadBOM := stripBOM(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return normalizedText{
		content:    s,
		lineEnding: lineEnding,
		hadBOM:     hadBOM,
		trailingLF: strings.HasSuffix(s, "\n"),
	}
}

func detectLineEnding(s string) string {
	crlf := strings.Count(s, "\r\n")
	bareLF := strings.Count(s, "\n") - crlf
	if crlf > bareLF {
		return "\r\n"
	}
	return "\n"
}

func stripBOM(s string) (string, bool) {
	if strings.HasPrefix(s, utf8BOM) {
		return strings.TrimPrefix(s, utf8BOM), true
	}
	return s, false
}

func restore(s, lineEnding string, hadBOM, trailingLF bool) string {
	if trailingLF && !strings.HasSuffix(s, "\n") {
		s += "\n"
	} else if !trailingLF {
		s = strings.TrimRight(s, "\n")
	}
	if lineEnding == "\r\n" {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	if hadBOM {
		s = utf8BOM + s
	}
	return s
}
