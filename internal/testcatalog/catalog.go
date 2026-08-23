package testcatalog

import (
	"strings"
	"unicode"
)

func NormalizeSQL(statement string) string {
	runes := []rune(statement)
	var normalized strings.Builder
	normalized.Grow(len(statement))
	var quote rune
	pendingSpace := false
	for i := 0; i < len(runes); i++ {
		current := runes[i]
		if quote != 0 {
			normalized.WriteRune(current)
			if current != quote {
				continue
			}
			if quote != ']' && i+1 < len(runes) && runes[i+1] == quote {
				i++
				normalized.WriteRune(runes[i])
				continue
			}
			quote = 0
			continue
		}
		if unicode.IsSpace(current) {
			if normalized.Len() > 0 {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			normalized.WriteByte(' ')
			pendingSpace = false
		}
		normalized.WriteRune(current)
		switch current {
		case '\'', '"', '`':
			quote = current
		case '[':
			quote = ']'
		}
	}
	return normalized.String()
}
