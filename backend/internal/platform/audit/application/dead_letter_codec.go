package application

import (
	"encoding/json"
)

// deadLetterPayload keeps the input schema versioned so future replay does not depend on a raw
// HTTP body. Metadata and changes have already passed audit redaction before persistence.
type deadLetterPayload struct {
	Version int        `json:"version"`
	Event   EventInput `json:"event"`
}

func marshalDeadLetterEvent(event EventInput) ([]byte, error) {
	return json.Marshal(deadLetterPayload{Version: 1, Event: event})
}

func unmarshalDeadLetterEvent(raw []byte) (EventInput, error) {
	var payload deadLetterPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return EventInput{}, err
	}
	if payload.Version != 1 {
		return EventInput{}, domainError("unsupported dead-letter payload version")
	}
	return payload.Event, nil
}

type domainError string

func (err domainError) Error() string { return string(err) }
