package application

import (
	"encoding/json"
)

// deadLetterPayload 让输入模式带版本号，使未来重放不依赖原始 HTTP 报文。
// Metadata 与 changes 在持久化前都已通过审计脱敏。
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
