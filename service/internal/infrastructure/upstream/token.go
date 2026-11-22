package upstream

// Dialog page-token codec: opaque to callers, base64-encoded JSON
// internally ({"o": top_message_id offset}). Invalid tokens are caller
// input errors; versioned for future extension.

import (
	"encoding/base64"
	"encoding/json"
)

type dialogTokenV1 struct {
	OffsetID int32 `json:"o"`
}

func encodeDialogToken(offsetID int64) string {
	b, _ := json.Marshal(dialogTokenV1{OffsetID: int32(offsetID)})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeDialogToken(token string) (int32, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, err
	}
	var t dialogTokenV1
	if err := json.Unmarshal(raw, &t); err != nil {
		return 0, err
	}
	return t.OffsetID, nil
}
