package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxRequestBodyBytes bounds every JSON request body this server accepts
// (spec: "Corpo JSON limitado em tamanho"). These payloads are all small,
// hand-typed objects, so 1 MiB is generous headroom, not a tuned limit.
const maxRequestBodyBytes = 1 << 20

// decodeJSONBody decodes exactly one JSON object into dst, rejecting
// unknown fields and any trailing data after that object (spec: "com campos
// desconhecidos rejeitados"). Every JSON-accepting handler in this package
// uses it, so the three "malformed request" shapes - oversized body,
// unknown field, trailing garbage - are rejected the same way everywhere.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decode request body: unexpected trailing data")
	}
	return nil
}
