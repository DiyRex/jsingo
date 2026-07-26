package jsingo

import (
	"encoding/json"
	"fmt"
)

// Codec serialises call arguments and results.
//
// The wire layer moves opaque bytes, so the codec is a clean swap: CBOR or
// protobuf can be dropped in without touching framing, multiplexing or
// transport. Whatever a Runtime uses, its sidecar must use the same one.
type Codec interface {
	// Name identifies the codec to the sidecar.
	Name() string
	// Encode serialises a call argument or result.
	Encode(v any) ([]byte, error)
	// Decode deserialises into a pointer.
	Decode(data []byte, v any) error
}

// JSONCodec is the default.
//
// JSON is not the fastest option in Go, but it is decoded natively in C++ by
// both JavaScript runtimes, where protobuf would run in interpreted JS. Across
// the pair it is usually the faster choice, and it needs no schema, no
// generated types and no build step. Revisit against a benchmark, not
// intuition.
type JSONCodec struct{}

// Name implements Codec.
func (JSONCodec) Name() string { return "json" }

// Encode implements Codec.
func (JSONCodec) Encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("jsingo: encode request: %w", err)
	}
	return b, nil
}

// Decode implements Codec.
func (JSONCodec) Decode(data []byte, v any) error {
	// A handler returning nothing sends "null"; leaving v at its zero value is
	// the right reading, not an error.
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("jsingo: decode reply: %w", err)
	}
	return nil
}

// defaultCodec is used when no WithCodec option is given.
var defaultCodec Codec = JSONCodec{}
