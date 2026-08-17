package sessionstore

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/warning"
)

func sharedRecordID(kind, userID, sessionID string, incarnation uint64, eventID string) string {
	digest := sha256.New()
	for _, value := range []string{kind, userID, sessionID, strconv.FormatUint(incarnation, 10), eventID} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write([]byte(value))
	}
	return kind + ":" + hex.EncodeToString(digest.Sum(nil))
}

const (
	recordVersion = 2
	recordSession = "session"
	recordEvent   = "event"
	recordSidecar = "sidecar"
)

type record struct {
	V       int            `json:"v"`
	Type    string         `json:"type"`
	Order   uint64         `json:"order,omitempty"`
	Session *header        `json:"session,omitempty"`
	Event   *session.Event `json:"event,omitempty"`
	Sidecar *sidecar       `json:"sidecar,omitempty"`
}

type header struct {
	ID          string         `json:"id"`
	AppName     string         `json:"appName"`
	UserID      string         `json:"userId"`
	State       map[string]any `json:"state,omitempty"`
	AppDelta    map[string]any `json:"appDelta,omitempty"`
	UserDelta   map[string]any `json:"userDelta,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	Incarnation uint64         `json:"incarnation"`
}

type sidecar struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type stateJournalRecord struct {
	V     int            `json:"v"`
	ID    string         `json:"id"`
	Order uint64         `json:"order"`
	Delta map[string]any `json:"delta"`
}

func marshalRecord(value record) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode session record: %w", err)
	}
	return append(data, '\n'), nil
}

func marshalJournalRecord(value stateJournalRecord) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode state journal record: %w", err)
	}
	return append(data, '\n'), nil
}

func decodeRecord(data []byte) (record, string, error) {
	var envelope struct {
		V     int    `json:"v"`
		Type  string `json:"type"`
		Order uint64 `json:"order"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return record{}, "", fmt.Errorf("decode session record: %w", err)
	}
	if envelope.V != recordVersion {
		return record{Order: envelope.Order}, warning.WarnSessionRecordUnsupportedVersion, nil
	}
	switch envelope.Type {
	case recordSession, recordEvent, recordSidecar:
	default:
		return record{Order: envelope.Order}, warning.WarnSessionRecordUnknown, nil
	}
	var decoded record
	if err := json.Unmarshal(data, &decoded); err != nil {
		return record{}, "", fmt.Errorf("decode session record: %w", err)
	}
	switch decoded.Type {
	case recordSession:
		if decoded.Session == nil {
			return record{}, "", fmt.Errorf("session record has no session")
		}
	case recordEvent:
		if decoded.Event == nil {
			return record{}, "", fmt.Errorf("event record has no event")
		}
	case recordSidecar:
		if decoded.Sidecar == nil {
			return record{}, "", fmt.Errorf("sidecar record has no sidecar")
		}
	}
	return decoded, "", nil
}

func recordLine(value record) ([]byte, error) {
	return marshalRecord(value)
}
