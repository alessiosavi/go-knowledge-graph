package productobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// ProductID returns the first 32 hex digits of the SHA-256 of the JSON array
// ["product", store, productID], using the source product ID as supplied.
func ProductID(store, productID string) string {
	digest := sha256.Sum256(productIDBytes(store, productID))
	return hex.EncodeToString(digest[:16])
}

func productIDBytes(store, productID string) []byte {
	// The encoder leaves HTML characters unescaped by default. Permitting
	// replacement of invalid UTF-8 makes the string array infallible to encode.
	key, _ := json.Marshal([3]string{"product", store, productID}, jsontext.AllowInvalidUTF8(true))
	return key
}
