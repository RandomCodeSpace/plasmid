package sessionstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	data, err := readSessionLog(p, name, sync, logger)
	if err != nil {
		return nil, err
	}
	parser := sessionLogParser{
		log:        &sessionLog{name: name, sidecars: make(map[string][]byte)},
		seenEvents: make(map[string]struct{}),
		name:       name,
		app:        app,
		user:       user,
		id:         id,
		logger:     logger,
	}
	return parser.parse(data)
}

func readSessionLog(p *paths, name string, sync bool, logger warningLogger) ([]byte, error) {
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
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data, nil
	}
	return truncateTornFile(p, name, data, sync, logger)
}

func truncateTornFile(p *paths, name string, data []byte, sync bool, logger warningLogger) ([]byte, error) {
	cut := bytes.LastIndexByte(data, '\n') + 1
	file, err := p.root.OpenFile(name, os.O_WRONLY, fileMode)
	if err != nil {
		return nil, fmt.Errorf("open torn durable log: %w", err)
	}
	truncateErr := file.Truncate(int64(cut))
	if truncateErr == nil && sync {
		truncateErr = file.Sync()
	}
	closeErr := file.Close()
	if truncateErr != nil {
		return nil, fmt.Errorf("truncate torn durable log: %w", truncateErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close torn durable log: %w", closeErr)
	}
	logger.warn(warning.WarnSessionLogTornTail, name, 0, "discarded final unterminated record")
	return data[:cut], nil
}

type sessionLogParser struct {
	log        *sessionLog
	seenEvents map[string]struct{}
	name       string
	app        string
	user       string
	id         string
	logger     warningLogger
}

func (p *sessionLogParser) parse(data []byte) (*sessionLog, error) {
	line := 0
	for len(data) > 0 {
		line++
		end := bytes.IndexByte(data, '\n')
		rawLine := data[:end]
		data = data[end+1:]
		if err := p.parseLine(rawLine, line); err != nil {
			return nil, err
		}
	}
	if line == 0 || p.log.header.ID == "" {
		return nil, corruptLog(p.name, 1, errors.New("missing session header"), p.logger)
	}
	return p.log, nil
}

func (p *sessionLogParser) parseLine(rawLine []byte, line int) error {
	if len(bytes.TrimSpace(rawLine)) == 0 {
		return corruptLog(p.name, line, errors.New("empty record"), p.logger)
	}
	rec, warningCode, err := decodeRecord(rawLine)
	if err != nil {
		return corruptLog(p.name, line, err, p.logger)
	}
	if warningCode != "" {
		p.acceptForwardRecord(rec, warningCode, line)
		return nil
	}
	if line == 1 {
		return p.acceptHeader(rec, line)
	}
	return p.acceptBodyRecord(rec, line)
}

func (p *sessionLogParser) acceptForwardRecord(rec record, warningCode string, line int) {
	p.logger.warn(warningCode, p.name, line, "skipped forward-compatible record")
	if rec.Order == 0 {
		return
	}
	p.log.stateRecords = append(p.log.stateRecords, stateRecord{
		ID:    sharedRecordID("forward", p.user, p.id, p.log.header.Incarnation, fmt.Sprintf("%d:%d", line, rec.Order)),
		Order: rec.Order, Path: p.name, Line: line, UserID: p.user,
	})
}

func (p *sessionLogParser) acceptHeader(rec record, line int) error {
	if !p.validHeader(rec) {
		return corruptLog(p.name, line, errors.New("invalid session header"), p.logger)
	}
	p.log.header = cloneHeader(*rec.Session)
	p.log.updated = rec.Session.CreatedAt
	p.log.stateRecords = append(p.log.stateRecords, stateRecord{
		ID:    sharedRecordID("create", p.user, p.id, rec.Session.Incarnation, ""),
		Order: rec.Order, Path: p.name, Line: line, UserID: p.user,
		AppDelta: maps.Clone(rec.Session.AppDelta), UserDelta: maps.Clone(rec.Session.UserDelta),
	})
	return nil
}

func (p *sessionLogParser) validHeader(rec record) bool {
	return rec.Type == recordSession && rec.Session != nil && rec.Session.ID == p.id &&
		rec.Session.AppName == p.app && rec.Session.UserID == p.user && rec.Order > 0 &&
		rec.Session.Incarnation == rec.Order && !rec.Session.CreatedAt.IsZero()
}

func (p *sessionLogParser) acceptBodyRecord(rec record, line int) error {
	if rec.Type == recordSession {
		return corruptLog(p.name, line, errors.New("duplicate session header"), p.logger)
	}
	if rec.Type == recordSidecar {
		return p.acceptSidecar(rec, line)
	}
	return p.acceptEvent(rec, line)
}

func (p *sessionLogParser) acceptEvent(rec record, line int) error {
	if rec.Event.ID == "" || rec.Order == 0 {
		return corruptLog(p.name, line, errors.New("invalid event record"), p.logger)
	}
	if _, exists := p.seenEvents[rec.Event.ID]; exists {
		return corruptLog(p.name, line, errors.New("duplicate event id"), p.logger)
	}
	p.seenEvents[rec.Event.ID] = struct{}{}
	event := cloneEvent(rec.Event)
	p.log.events = append(p.log.events, event)
	p.log.updated = event.Timestamp
	_, appDelta, userDelta := splitState(event.Actions.StateDelta)
	p.log.stateRecords = append(p.log.stateRecords, stateRecord{
		ID:    sharedRecordID("event", p.user, p.id, p.log.header.Incarnation, event.ID),
		Order: rec.Order, Path: p.name, Line: line, UserID: p.user, AppDelta: appDelta, UserDelta: userDelta,
	})
	return nil
}

func (p *sessionLogParser) acceptSidecar(rec record, line int) error {
	if rec.Sidecar.Kind == "" || !jsonValue(rec.Sidecar.Data) {
		return corruptLog(p.name, line, errors.New("invalid sidecar record"), p.logger)
	}
	p.log.sidecars[rec.Sidecar.Kind] = append([]byte(nil), rec.Sidecar.Data...)
	return nil
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
	committed, err := writeAndClose(file, data, sync)
	if err == nil && closeHook != nil {
		err = closeHook()
	}
	if err != nil {
		return committed, fmt.Errorf("persist session log: %w", err)
	}
	return true, nil
}

type warningLogger interface {
	warn(code, path string, line int, message string)
}

func jsonValue(data []byte) bool {
	return len(data) > 0 && strings.TrimSpace(string(data)) != "" && json.Valid(data)
}
