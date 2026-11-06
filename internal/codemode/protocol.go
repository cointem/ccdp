// Package codemode executes bounded JavaScript programs in isolated workers.
// The worker never executes a tool: all authority remains with the host callback.
package codemode

import (
	"bytes"
	"ccdp/internal/protocol"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
)

const WorkerArgument = "--internal-code-worker-v1"
const MaxFrame = 1 << 20
const MaxCode = 64 << 10
const MaxResult = 512 << 10
const MaxCalls = 128
const MaxQueue = 32
const MaxResults = 8 << 20

type Result struct {
	Process       *protocol.ProcessOutput `json:"-"`
	CompletionIDs []string                `json:"-"`
	CallID        string                  `json:"call_id,omitempty"`
	Status        string                  `json:"status"`
	Text          string                  `json:"text"`
	Data          json.RawMessage         `json:"data,omitempty"`
	Truncated     bool                    `json:"truncated"`
	DataError     string                  `json:"data_error,omitempty"`
	OutputID      string                  `json:"output_id,omitempty"`
}

// ValidateData prevents a precise host JSON number from being silently rounded
// when the worker parses it into a JavaScript Number. Text remains available,
// and callers can use a direct tool call for values outside this range.
func ValidateData(data json.RawMessage) error {
	if len(data) == 0 {
		return nil
	}
	if !json.Valid(data) {
		return fmt.Errorf("structured data is not valid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var check func(any) error
	check = func(value any) error {
		switch value := value.(type) {
		case json.Number:
			number, err := value.Float64()
			if err != nil || math.IsInf(number, 0) || math.Abs(number) > 9007199254740991 {
				return fmt.Errorf("structured data contains a number outside JavaScript's exact integer range; use the saved output or a direct tool call")
			}
		case map[string]any:
			for _, child := range value {
				if err := check(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := check(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return check(value)
}

type frame struct {
	Type    string          `json:"type"`
	Version int             `json:"version,omitempty"`
	ID      int             `json:"id,omitempty"`
	Code    string          `json:"code,omitempty"`
	Names   []string        `json:"names,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    map[string]any  `json:"args"`
	Value   json.RawMessage `json:"value,omitempty"`
	Result  *Result         `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func readFrame(r io.Reader) (frame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame{}, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxFrame {
		return frame{}, fmt.Errorf("invalid worker frame length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return frame{}, err
	}
	var f frame
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(&f); err != nil {
		return frame{}, err
	}
	return f, nil
}
func writeFrame(w io.Writer, f frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return writeFrameBytes(w, b)
}

// Worker frames are already JSON. Preserve their encoding so UTF-8 limits
// checked by the bootstrap also describe the bytes written on the pipe.
func writeFrameBytes(w io.Writer, b []byte) error {
	if len(b) > MaxFrame {
		return fmt.Errorf("worker frame exceeds %d bytes", MaxFrame)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(b)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}
