package openai

import "encoding/base64"

func encodeBase64(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}
