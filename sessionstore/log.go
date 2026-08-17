package sessionstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/plasmid-dev/plasmid/loop"
)

// sessionLog is a replayed projection of one append-only log. Its caller owns
// synchronization; records are appended only after they have been marshalled.
type sessionLog struct {
	name         string
	header       header
	events       []loop.Event
	sidecars     map[string][]byte
	stateRecords []stateRecord
	updated      time.Time
}

type stateRecord struct {
	Order     uint64
	Path      string
	Line      int
	UserID    string
	AppDelta  map[string]any
	UserDelta map[string]any
}

func loadSessionLog(p *paths, name, app, user, id string, logger warningLogger) (*sessionLog, error) {
	if err := p.ensureParent(name); err != nil {
		return nil, err
	}
	if err := p.root.Chmod(name, fileMode); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("set session log mode: %w", err)
	}
	data, err := p.root.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read session log: %w", err)
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		cut := bytes.LastIndexByte(data, '\n')
		if cut < 0 {
			cut = 0
		} else {
			cut++
		}
		file, openErr := p.root.OpenFile(name, os.O_WRONLY, fileMode)
		if openErr != nil {
			return nil, fmt.Errorf("open torn session log: %w", openErr)
		}
		truncateErr := file.Truncate(int64(cut))
		closeErr := file.Close()
		if truncateErr != nil {
			return nil, fmt.Errorf("truncate torn session log: %w", truncateErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close torn session log: %w", closeErr)
		}
		logger.warn(loop.WarnSessionLogTornTail, name, 0, "discarded final unterminated record")
		data = data[:cut]
	}

	log := &sessionLog{name: name, sidecars: make(map[string][]byte)}
	line := 0
	for len(data) > 0 {
		line++
		end := bytes.IndexByte(data, '\n')
		if end < 0 { // The preceding repair makes this unreachable.
			return nil, corruptLog(name, line, io.ErrUnexpectedEOF, logger)
		}
		rawLine := data[:end]
		data = data[end+1:]
		if len(bytes.TrimSpace(rawLine)) == 0 {
			return nil, corruptLog(name, line, errors.New("empty record"), logger)
		}
		record, warning, decodeErr := decodeRecord(rawLine)
		if decodeErr != nil {
			return nil, corruptLog(name, line, decodeErr, logger)
		}
		if warning != "" {
			logger.warn(warning, name, line, "skipped forward-compatible record")
			continue
		}
		if line == 1 {
			if record.Type != recordSession || record.Session == nil || record.Session.ID != id || record.Session.AppName != app || record.Session.UserID != user {
				return nil, corruptLog(name, line, errors.New("invalid session header"), logger)
			}
			log.header = cloneHeader(*record.Session)
			_, appDelta, userDelta := splitState(record.Session.State)
			log.stateRecords = append(log.stateRecords, stateRecord{Order: record.Order, Path: name, Line: line, UserID: user, AppDelta: appDelta, UserDelta: userDelta})
			continue
		}
		if log.header.ID == "" {
			return nil, corruptLog(name, line, errors.New("first record is not a session header"), logger)
		}
		switch record.Type {
		case recordEvent:
			event, eventErr := record.Event.event()
			if eventErr != nil || event.ID == "" || event.SessionID != id {
				if eventErr == nil {
					eventErr = errors.New("invalid event record")
				}
				return nil, corruptLog(name, line, eventErr, logger)
			}
			log.events = append(log.events, cloneEvent(event))
			log.updated = event.Timestamp
			_, appDelta, userDelta := splitState(event.StateDelta)
			log.stateRecords = append(log.stateRecords, stateRecord{Order: record.Order, Path: name, Line: line, UserID: user, AppDelta: appDelta, UserDelta: userDelta})
		case recordSidecar:
			if record.Sidecar.Kind == "" || !jsonValue(record.Sidecar.Data) {
				return nil, corruptLog(name, line, errors.New("invalid sidecar record"), logger)
			}
			log.sidecars[record.Sidecar.Kind] = append([]byte(nil), record.Sidecar.Data...)
		case recordSession:
			return nil, corruptLog(name, line, errors.New("duplicate session header"), logger)
		}
	}
	if line == 0 || log.header.ID == "" {
		return nil, corruptLog(name, 1, errors.New("missing session header"), logger)
	}
	return log, nil
}

func corruptLog(name string, line int, cause error, logger warningLogger) error {
	logger.warn(loop.WarnSessionLogCorruptMiddle, name, line, cause.Error())
	return fmt.Errorf("%w: %s line %d: %v", ErrCorruptLog, name, line, cause)
}

func (log *sessionLog) appendBytes(p *paths, data []byte, sync bool) error {
	file, err := p.root.OpenFile(log.name, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("open session log for append: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("append session log: %w", err)
	}
	if sync {
		if err := file.Sync(); err != nil {
			file.Close()
			return fmt.Errorf("sync session log: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close session log: %w", err)
	}
	return nil
}

func marshalRecord(value record) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode session record: %w", err)
	}
	return append(data, '\n'), nil
}

type warningLogger interface {
	warn(code, path string, line int, message string)
}

func jsonValue(data []byte) bool {
	return len(data) > 0 && strings.TrimSpace(string(data)) != "" && json.Valid(data)
}
