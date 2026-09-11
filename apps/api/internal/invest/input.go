package invest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// InvestigationInput is the POST /investigate-equivalent payload: the
// single united exception envelope. One field only — the future cognitive
// side takes no other input channel, so there is no second slot for a
// prompt, a hint, an override, or any other stage-foreign key to arrive
// through. Strict decoding rejects all of those.
type InvestigationInput struct {
	Exception UnresolvedException `json:"exception"`
}

// ValidateInput checks the payload standalone: exactly the united
// envelope, validated.
func ValidateInput(in InvestigationInput) error {
	if err := Validate(in.Exception); err != nil {
		return fmt.Errorf("invest: input: %v", err)
	}
	return nil
}

// MarshalInput renders the canonical payload bytes: validate first (fail
// closed), then encode the canonicalized envelope with encoding/json v1.
func MarshalInput(in InvestigationInput) ([]byte, error) {
	if err := ValidateInput(in); err != nil {
		return nil, err
	}
	c := canonicalize(in.Exception)
	payload := InvestigationInput{Exception: c}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("invest: marshal input: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// DecodeInput parses and validates one payload with strict field
// checking: unknown fields — including prompt, hints, overrides,
// agreed_raw, confidence, or any future stage-foreign key — are rejected;
// trailing data is rejected; the result must pass ValidateInput.
func DecodeInput(data []byte) (InvestigationInput, error) {
	var in InvestigationInput
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return InvestigationInput{}, fmt.Errorf("invest: decode input: %v", err)
	}
	// Trailing-data check: a second decode must hit EOF.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return InvestigationInput{}, fmt.Errorf("invest: decode input: trailing data")
	}
	normalizeNil(&in.Exception)
	if err := ValidateInput(in); err != nil {
		return InvestigationInput{}, err
	}
	return in, nil
}
