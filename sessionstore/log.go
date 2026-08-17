package sessionstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/warning"
)

type sessionLog struct {
	name         string
	header       header
	events       []*session.Event
	sidecars     map[string][]byte
	stateRecords []stateRecord
	updated      time.Time
}

type stateRecord struct {
	ID        string
	Order     uint64
	Path      string
	Line      int
	UserID    string
	AppDelta  map[string]any
	UserDelta map[string]any
}

func loadSessionLog(p *paths, name, app, user, id string, sync bool, logger warningLogger) (*sessionLog, error) {
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
		cut := bytes.LastIndexByte(data, '\n') + 1
		file, openErr := p.root.OpenFile(name, os.O_WRONLY, fileMode)
		if openErr != nil {
			return nil, fmt.Errorf("open torn session log: %w", openErr)
		}
		truncateErr := file.Truncate(int64(cut))
		if truncateErr == nil && sync {
			truncateErr = file.Sync()
		}
		closeErr := file.Close()
		if truncateErr != nil {
			return nil, fmt.Errorf("truncate torn session log: %w", truncateErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close torn session log: %w", closeErr)
		}
		logger.warn(warning.WarnSessionLogTornTail, name, 0, "discarded final unterminated record")
		data = data[:cut]
	}

	log := &sessionLog{name: name, sidecars: make(map[string][]byte)}
	seenEvents := make(map[string]struct{})
	line := 0
	for len(data) > 0 {
		line++
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			return nil, corruptLog(name, line, io.ErrUnexpectedEOF, logger)
		}
		rawLine := data[:end]
		data = data[end+1:]
		if len(bytes.TrimSpace(rawLine)) == 0 {
			return nil, corruptLog(name, line, errors.New("empty record"), logger)
		}
		rec, warningCode, decodeErr := decodeRecord(rawLine)
		if decodeErr != nil {
			return nil, corruptLog(name, line, decodeErr, logger)
		}
		if warningCode != "" {
			logger.warn(warningCode, name, line, "skipped forward-compatible record")
			if rec.Order > 0 {
				log.stateRecords = append(log.stateRecords, stateRecord{ID: sharedRecordID("forward", user, id, log.header.Incarnation, fmt.Sprintf("%d:%d", line, rec.Order)), Order: rec.Order, Path: name, Line: line, UserID: user})
			}
			continue
		}
		if line == 1 {
			if rec.Type != recordSession || rec.Session == nil || rec.Session.ID != id || rec.Session.AppName != app || rec.Session.UserID != user || rec.Order == 0 || rec.Session.Incarnation != rec.Order || rec.Session.CreatedAt.IsZero() {
				return nil, corruptLog(name, line, errors.New("invalid session header"), logger)
			}
			log.header = cloneHeader(*rec.Session)
			log.updated = rec.Session.CreatedAt
			log.stateRecords = append(log.stateRecords, stateRecord{ID: sharedRecordID("create", user, id, rec.Session.Incarnation, ""), Order: rec.Order, Path: name, Line: line, UserID: user, AppDelta: maps.Clone(rec.Session.AppDelta), UserDelta: maps.Clone(rec.Session.UserDelta)})
			continue
		}
		if log.header.ID == "" {
			return nil, corruptLog(name, line, errors.New("first record is not a session header"), logger)
		}
		switch rec.Type {
		case recordEvent:
			if rec.Event.ID == "" || rec.Order == 0 {
				return nil, corruptLog(name, line, errors.New("invalid event record"), logger)
			}
			if _, exists := seenEvents[rec.Event.ID]; exists {
				return nil, corruptLog(name, line, errors.New("duplicate event id"), logger)
			}
			seenEvents[rec.Event.ID] = struct{}{}
			event := cloneEvent(rec.Event)
			log.events = append(log.events, event)
			log.updated = event.Timestamp
			_, appDelta, userDelta := splitState(event.Actions.StateDelta)
			log.stateRecords = append(log.stateRecords, stateRecord{ID: sharedRecordID("event", user, id, log.header.Incarnation, event.ID), Order: rec.Order, Path: name, Line: line, UserID: user, AppDelta: appDelta, UserDelta: userDelta})
		case recordSidecar:
			if rec.Sidecar.Kind == "" || !jsonValue(rec.Sidecar.Data) {
				return nil, corruptLog(name, line, errors.New("invalid sidecar record"), logger)
			}
			log.sidecars[rec.Sidecar.Kind] = append([]byte(nil), rec.Sidecar.Data...)
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
	logger.warn(warning.WarnSessionLogCorruptMiddle, name, line, cause.Error())
	return fmt.Errorf("%w: %s line %d: %v", ErrCorruptLog, name, line, cause)
}

func (log *sessionLog) appendBytes(p *paths, data []byte, sync bool, closeHook func() error) (bool, error) {
	file, err := p.root.OpenFile(log.name, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return false, fmt.Errorf("open session log for append: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return false, fmt.Errorf("append session log: %w", err)
	}
	if sync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return false, fmt.Errorf("sync session log: %w", err)
		}
	}
	closeErr := file.Close()
	if closeErr == nil && closeHook != nil {
		closeErr = closeHook()
	}
	if closeErr != nil {
		return true, fmt.Errorf("close session log: %w", closeErr)
	}
	return true, nil
}

type warningLogger interface {
	warn(code, path string, line int, message string)
}

func jsonValue(data []byte) bool {
	return len(data) > 0 && strings.TrimSpace(string(data)) != "" && json.Valid(data)
}
