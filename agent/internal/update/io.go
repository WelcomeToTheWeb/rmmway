package update

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func decodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}

// decodeJSON decodes one JSON value from r (erroring on trailing garbage).
func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after JSON document")
	}
	return nil
}

// mustKeyID renders a key id for logs; "?" if the key is malformed.
func mustKeyID(pub string) string {
	id, err := keyID(pub)
	if err != nil {
		return "?"
	}
	return id
}

// KeyID is the exported form of mustKeyID (for the agent CLI's log line).
func KeyID(pub string) string { return mustKeyID(pub) }
