package messages

import (
	"unicode/utf8"
)

func TruncateUTF8(value string, maxBytes int) string {
	if maxBytes < 0 || len(value) <= maxBytes {
		return value
	}
	if maxBytes == 0 {
		return ""
	}
	if maxBytes >= len(value) {
		return value
	}
	cut := value[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		_, size := utf8.DecodeLastRuneInString(cut)
		if size <= 0 || size > len(cut) {
			cut = cut[:len(cut)-1]
		} else {
			cut = cut[:len(cut)-size]
		}
	}
	return cut
}
