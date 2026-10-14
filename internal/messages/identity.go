package messages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func StableID(prefix string, value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return prefix + "-" + hex.EncodeToString(sum[:12])
}

func CloneImageAttachments(src []ImageAttachment) []ImageAttachment {
	if src == nil {
		return nil
	}
	dst := make([]ImageAttachment, len(src))
	for i, attachment := range src {
		dst[i] = attachment
		dst[i].Data = append([]byte(nil), attachment.Data...)
	}
	return dst
}
