// Package sessionstore provides a durable native Google ADK session service.
package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/warning"
)

type Options struct {
	Dir         string
	Fsync       *bool
	NewID       func() string
	Logger      *slog.Logger
	WarningSink warning.Sink
}

type Store struct {
	paths    *paths
	fsync    bool
	newID    func() string
	warnings warning.Sink

	mu        sync.Mutex
	closed    bool
	closeDone chan struct{}
	closeErr  error
	active    sync.WaitGroup
	sessions  map[string]*sessionLock
	apps      map[string]*appLocks

	projectHook     func() error
	journalHook     func() error
	commitHook      func(string)
	dirSyncHook     func(string) error
	appendCloseHook func() error
	closeHook       func() error
	scanEntryHook   func(string)
	inventoryHook   func(string)
	sequenceHook    func() error
}

type sessionLock struct {
	op sync.Mutex
	io sync.RWMutex
}

type appLocks struct {
	order     sync.Mutex
	project   sync.Mutex
	create    sync.Mutex
	pendingMu sync.RWMutex
	pending   map[string]stateRecord
	cacheMu   sync.RWMutex
	app       projectionScope
	users     map[string]projectionScope
	appKnown  bool
	invInit   sync.Mutex
	inventory *recordInventory
}

var _ session.Service = (*Store)(nil)

func Open(dir string) (*Store, error) { return OpenWith(Options{Dir: dir}) }

func OpenWith(o Options) (*Store, error) {
	p, err := openPaths(o.Dir)
	if err != nil {
		return nil, err
	}
	fsync := true
	if o.Fsync != nil {
		fsync = *o.Fsync
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	warnings := o.WarningSink
	if warnings == nil {
		warnings = warning.SlogSink{Logger: logger}
	}
	return &Store{
		paths: p, fsync: fsync, newID: o.NewID, warnings: warnings,
		sessions: make(map[string]*sessionLock), apps: make(map[string]*appLocks),
	}, nil
}

func (s *Store) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.active.Add(1)
	return nil
}

func (s *Store) end() { s.active.Done() }

func (s *Store) locksFor(app, user, id string) (*sessionLock, *appLocks, string, error) {
	name, err := s.identityPath(app, user, id)
	if err != nil {
		return nil, nil, "", err
	}
	encodedApp, err := encodeSegment(app)
	if err != nil {
		return nil, nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := s.sessions[name]
	if sl == nil {
		sl = &sessionLock{}
		s.sessions[name] = sl
	}
	al := s.apps[encodedApp]
	if al == nil {
		al = &appLocks{}
		s.apps[encodedApp] = al
	}
	return sl, al, name, nil
}

func (s *Store) appLocks(app string) (*appLocks, error) {
	encoded, err := encodeSegment(app)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	locks := s.apps[encoded]
	if locks == nil {
		locks = &appLocks{}
		s.apps[encoded] = locks
	}
	return locks, nil
}

type warningBuffer struct {
	values []warning.Warning
	seen   map[string]struct{}
}

func (b *warningBuffer) warn(code, path string, line int, message string) {
	key := fmt.Sprintf("%s\x00%s\x00%d", code, path, line)
	if _, exists := b.seen[key]; exists {
		return
	}
	if b.seen == nil {
		b.seen = make(map[string]struct{})
	}
	b.seen[key] = struct{}{}
	b.values = append(b.values, warning.Warning{Code: code, Source: "sessionstore", Path: path, Line: line, Message: message})
}

func (s *Store) emitWarnings(buffer *warningBuffer) {
	for _, value := range buffer.values {
		s.warnings.Warn(value)
	}
}

func (s *Store) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("create session: request is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.begin(); err != nil {
		return nil, err
	}
	var notices warningBuffer
	defer s.emitWarnings(&notices)
	defer s.end()
	if _, _, err := encodeIdentity(req.AppName, req.UserID); err != nil {
		return nil, err
	}
	stateHash, err := createStateHash(req.State)
	if err != nil {
		return nil, err
	}
	app, err := s.appLocks(req.AppName)
	if err != nil {
		return nil, err
	}
	app.create.Lock()
	defer app.create.Unlock()
	generated := req.SessionID == ""
	id := req.SessionID
	var pending createMarker
	if generated {
		pending, err = s.findPendingCreate(req.AppName, req.UserID, stateHash)
		if err != nil {
			return nil, err
		}
		id = pending.Header.ID
	}
	if id == "" {
		if s.newID != nil {
			id = s.newID()
		} else {
			id = platform.NewUUID(ctx)
		}
	}
	locks, _, name, err := s.locksFor(req.AppName, req.UserID, id)
	if err != nil {
		return nil, err
	}
	locks.op.Lock()
	defer locks.op.Unlock()
	if _, err := s.paths.root.Stat(deleteMarkerName(name)); err == nil {
		return nil, ErrSessionExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	marker, marked, err := s.readCreateMarker(name)
	if err != nil {
		return nil, err
	}
	if marked {
		if marker.StateHash != stateHash || marker.Header.AppName != req.AppName || marker.Header.UserID != req.UserID || marker.Header.ID != id {
			return nil, ErrSessionExists
		}
		return s.resumeCreate(marker, name, locks, app, &notices)
	}
	if _, err := s.paths.root.Stat(name); err == nil {
		return nil, ErrSessionExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := s.readSharedProjection(req.AppName, req.UserID, app, &notices); err != nil {
		return nil, err
	}
	local, appDelta, userDelta := splitState(cloneMap(req.State))
	pendingRecord, err := s.reserveRecord(req.AppName, app, &notices, func(order uint64) stateRecord {
		return stateRecord{ID: sharedRecordID("create", req.UserID, id, order, ""), Order: order, Path: name, Line: 1, UserID: req.UserID, AppDelta: appDelta, UserDelta: userDelta}
	})
	if err != nil {
		return nil, err
	}
	order := pendingRecord.Order
	header := header{ID: id, AppName: req.AppName, UserID: req.UserID, State: local, AppDelta: appDelta, UserDelta: userDelta, CreatedAt: platform.Now(ctx), Incarnation: order}
	marker = createMarker{V: createMarkerVersion, Generated: generated, StateHash: stateHash, Header: header}
	if err := s.paths.ensureParent(name); err != nil {
		s.releaseRecordReservation(app, pendingRecord)
		return nil, err
	}
	if err := s.writeCreateMarker(name, marker); err != nil {
		s.releaseRecordReservation(app, pendingRecord)
		return nil, fmt.Errorf("persist create transaction: %w", err)
	}
	pendingRecords := []stateRecord{pendingRecord}
	data, err := recordLine(record{V: recordVersion, Type: recordSession, Order: order, Session: &header})
	if err != nil {
		s.releaseRecordReservation(app, pendingRecord)
		return nil, err
	}
	locks.io.Lock()
	committed, createErr := s.createLog(name, data)
	locks.io.Unlock()
	if createErr != nil && !committed {
		s.releaseRecordReservation(app, pendingRecord)
		if rollbackErr := s.rollbackCreateMarker(name); rollbackErr != nil {
			return nil, fmt.Errorf("%v; rollback create transaction: %w", createErr, rollbackErr)
		}
		return nil, createErr
	}
	if createErr != nil {
		notices.warn(warning.WarnSessionDurabilityRetry, name, 0, createErr.Error())
		return nil, fmt.Errorf("create session committed but requires retry: %w", createErr)
	}
	projection, err := s.reconcileCommittedShared(req.AppName, req.UserID, name, app, pendingRecords, &notices)
	if err != nil {
		return nil, err
	}
	if err := s.finishCreateMarker(name); err != nil {
		notices.warn(warning.WarnSessionDurabilityRetry, name, 0, err.Error())
		return nil, fmt.Errorf("create session committed but requires retry: %w", err)
	}
	if s.commitHook != nil {
		s.commitHook(id)
	}
	value := newDurableSession(s, header, nil, mergeState(local, projection.App, projection.User), header.CreatedAt)
	return &session.CreateResponse{Session: value}, nil
}

func (s *Store) resumeCreate(marker createMarker, name string, locks *sessionLock, app *appLocks, notices *warningBuffer) (*session.CreateResponse, error) {
	if _, err := s.readSharedProjection(marker.Header.AppName, marker.Header.UserID, app, notices); err != nil {
		return nil, err
	}
	pendingRecord := stateRecord{ID: sharedRecordID("create", marker.Header.UserID, marker.Header.ID, marker.Header.Incarnation, ""), Order: marker.Header.Incarnation, Path: name, Line: 1, UserID: marker.Header.UserID, AppDelta: marker.Header.AppDelta, UserDelta: marker.Header.UserDelta}
	if err := s.validateReservedRecord(marker.Header.AppName, app, notices, pendingRecord); err != nil {
		return nil, err
	}
	data, err := recordLine(record{V: recordVersion, Type: recordSession, Order: marker.Header.Incarnation, Session: &marker.Header})
	if err != nil {
		return nil, err
	}
	locks.io.Lock()
	log, loadErr := loadSessionLog(s.paths, name, marker.Header.AppName, marker.Header.UserID, marker.Header.ID, s.fsync, notices)
	if errors.Is(loadErr, ErrSessionNotFound) {
		_, loadErr = s.createLog(name, data)
		if loadErr == nil {
			log, loadErr = loadSessionLog(s.paths, name, marker.Header.AppName, marker.Header.UserID, marker.Header.ID, s.fsync, notices)
		}
	}
	locks.io.Unlock()
	if loadErr != nil {
		notices.warn(warning.WarnSessionDurabilityRetry, name, 0, loadErr.Error())
		return nil, fmt.Errorf("resume create transaction: %w", loadErr)
	}
	if !reflect.DeepEqual(log.header, marker.Header) {
		return nil, fmt.Errorf("%w: create transaction contradicts transcript", ErrCorruptLog)
	}
	if s.fsync {
		if err := s.syncParentTree(filepath.Dir(name)); err != nil {
			notices.warn(warning.WarnSessionDurabilityRetry, name, 0, err.Error())
			return nil, fmt.Errorf("create session committed but requires retry: %w", err)
		}
	}
	projection, err := s.reconcileCommittedShared(marker.Header.AppName, marker.Header.UserID, name, app, log.stateRecords, notices)
	if err != nil {
		return nil, err
	}
	if err := s.finishCreateMarker(name); err != nil {
		notices.warn(warning.WarnSessionDurabilityRetry, name, 0, err.Error())
		return nil, fmt.Errorf("create session committed but requires retry: %w", err)
	}
	if s.commitHook != nil {
		s.commitHook(marker.Header.ID)
	}
	value := newDurableSession(s, marker.Header, nil, sessionState(log, projection), marker.Header.CreatedAt)
	return &session.CreateResponse{Session: value}, nil
}

func (s *Store) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("get session: request is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.begin(); err != nil {
		return nil, err
	}
	var notices warningBuffer
	defer s.emitWarnings(&notices)
	defer s.end()
	locks, app, name, err := s.locksFor(req.AppName, req.UserID, req.SessionID)
	if err != nil {
		return nil, err
	}
	locks.op.Lock()
	defer locks.op.Unlock()
	scan, err := s.scanApp(req.AppName, &notices)
	if err != nil {
		return nil, err
	}
	log := scan.Logs[name]
	if log == nil {
		return nil, ErrSessionNotFound
	}
	projection, repairErr := s.repairShared(req.AppName, req.UserID, app, scan.Records, &notices)
	if repairErr != nil {
		notices.warn(warning.WarnSessionSnapshotRefresh, name, 0, repairErr.Error())
		projection, err = s.authoritativeFallback(req.AppName, req.UserID, app, scan.Records, &notices)
		if err != nil {
			return nil, fmt.Errorf("reconstruct shared state: %w", err)
		}
	}
	state := sessionState(log, projection)
	events := slices.Clone(log.events)
	if req.NumRecentEvents > 0 {
		events = events[max(len(events)-req.NumRecentEvents, 0):]
	}
	if !req.After.IsZero() {
		filtered := make([]*session.Event, 0, len(events))
		for _, event := range events {
			if !event.Timestamp.Before(req.After) {
				filtered = append(filtered, event)
			}
		}
		events = filtered
	}
	return &session.GetResponse{Session: newDurableSession(s, log.header, events, state, log.updated)}, nil
}

func (s *Store) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("list sessions: request is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.begin(); err != nil {
		return nil, err
	}
	var notices warningBuffer
	defer s.emitWarnings(&notices)
	defer s.end()
	if _, err := encodeSegment(req.AppName); err != nil {
		return nil, err
	}
	if req.UserID != "" {
		if _, err := encodeSegment(req.UserID); err != nil {
			return nil, err
		}
	}
	app, err := s.appLocks(req.AppName)
	if err != nil {
		return nil, err
	}
	scan, err := s.scanApp(req.AppName, &notices)
	if err != nil {
		return nil, err
	}
	users := make(map[string]sharedProjection)
	for _, log := range scan.Logs {
		if _, exists := users[log.header.UserID]; exists {
			continue
		}
		projection, repairErr := s.repairShared(req.AppName, log.header.UserID, app, scan.Records, &notices)
		if repairErr != nil {
			notices.warn(warning.WarnSessionSnapshotRefresh, log.name, 0, repairErr.Error())
			projection, err = s.authoritativeFallback(req.AppName, log.header.UserID, app, scan.Records, &notices)
			if err != nil {
				return nil, fmt.Errorf("reconstruct shared state for user %q: %w", log.header.UserID, err)
			}
		}
		users[log.header.UserID] = projection
	}
	result := make([]session.Session, 0, len(scan.Logs))
	for _, log := range scan.Logs {
		if req.UserID != "" && log.header.UserID != req.UserID {
			continue
		}
		projection := users[log.header.UserID]
		result = append(result, newDurableSession(s, log.header, nil, sessionState(log, projection), log.updated))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID() < result[j].ID() })
	return &session.ListResponse{Sessions: result}, nil
}

func (s *Store) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if req == nil {
		return fmt.Errorf("delete session: request is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.begin(); err != nil {
		return err
	}
	var notices warningBuffer
	defer s.emitWarnings(&notices)
	defer s.end()
	locks, app, name, err := s.locksFor(req.AppName, req.UserID, req.SessionID)
	if err != nil {
		return err
	}
	locks.op.Lock()
	defer locks.op.Unlock()
	locks.io.Lock()
	log, loadErr := loadSessionLog(s.paths, name, req.AppName, req.UserID, req.SessionID, s.fsync, &notices)
	locks.io.Unlock()
	marker, marked, err := s.readDeleteMarker(name)
	if err != nil {
		return err
	}
	createTransaction, createPending, err := s.readCreateMarker(name)
	if err != nil {
		return err
	}
	if marked {
		if loadErr == nil && log.header.Incarnation != marker {
			return s.finishDeleteMarker(name)
		}
		if loadErr != nil && !errors.Is(loadErr, ErrSessionNotFound) {
			return loadErr
		}
		if loadErr == nil {
			locks.io.Lock()
			err = s.paths.root.Remove(name)
			locks.io.Unlock()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := s.removeCreateMarker(name); err != nil {
			return err
		}
		return s.finishDeleteMarker(name)
	}
	if errors.Is(loadErr, ErrSessionNotFound) {
		if createPending {
			markerData, err := json.Marshal(createTransaction.Header.Incarnation)
			if err != nil {
				return err
			}
			if err := writeFileAtomic(s.paths.root, deleteMarkerName(name), markerData, s.fsync); err != nil {
				return fmt.Errorf("persist delete marker: %w", err)
			}
			if err := s.removeCreateMarker(name); err != nil {
				return err
			}
			return s.finishDeleteMarker(name)
		}
		return nil
	}
	if loadErr != nil {
		return loadErr
	}
	scan, err := s.scanApp(req.AppName, &notices)
	if err != nil {
		return err
	}
	if _, err := s.repairShared(req.AppName, req.UserID, app, scan.Records, &notices); err != nil {
		return fmt.Errorf("repair shared state before delete: %w", err)
	}
	markerData, err := json.Marshal(log.header.Incarnation)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.paths.root, deleteMarkerName(name), markerData, s.fsync); err != nil {
		return fmt.Errorf("persist delete marker: %w", err)
	}
	locks.io.Lock()
	err = s.paths.root.Remove(name)
	locks.io.Unlock()
	if err != nil {
		return fmt.Errorf("delete session log: %w", err)
	}
	if err := s.removeCreateMarker(name); err != nil {
		return err
	}
	return s.finishDeleteMarker(name)
}

func (s *Store) removeCreateMarker(name string) error {
	if err := s.paths.root.Remove(createMarkerName(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete create transaction: %w", err)
	}
	return nil
}

func deleteMarkerName(name string) string { return name + ".delete-pending" }

const createMarkerVersion = 1

type createMarker struct {
	V         int    `json:"v"`
	Generated bool   `json:"generated"`
	StateHash string `json:"stateHash"`
	Header    header `json:"header"`
}

func createMarkerName(name string) string { return name + ".create-pending" }

func createStateHash(state map[string]any) (string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("encode initial session state: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (s *Store) writeCreateMarker(name string, marker createMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.paths.root, createMarkerName(name), data, s.fsync)
}

func (s *Store) readCreateMarker(name string) (createMarker, bool, error) {
	data, err := s.paths.root.ReadFile(createMarkerName(name))
	if errors.Is(err, os.ErrNotExist) {
		return createMarker{}, false, nil
	}
	if err != nil {
		return createMarker{}, false, err
	}
	var marker createMarker
	if json.Unmarshal(data, &marker) != nil || marker.V != createMarkerVersion || marker.StateHash == "" || marker.Header.ID == "" || marker.Header.AppName == "" || marker.Header.UserID == "" || marker.Header.Incarnation == 0 {
		return createMarker{}, false, fmt.Errorf("%w: invalid create transaction", ErrCorruptLog)
	}
	return marker, true, nil
}

func (s *Store) findPendingCreate(app, user, stateHash string) (createMarker, error) {
	dir, err := s.paths.sessionDir(app, user)
	if err != nil {
		return createMarker{}, err
	}
	entries, err := readDir(s.paths.root, dir)
	if errors.Is(err, os.ErrNotExist) {
		return createMarker{}, nil
	}
	if err != nil {
		return createMarker{}, err
	}
	const suffix = ".jsonl.create-pending"
	var found createMarker
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		id, err := decodeSegment(strings.TrimSuffix(entry.Name(), suffix))
		if err != nil {
			continue
		}
		name, err := s.paths.sessionLog(app, user, id)
		if err != nil {
			return createMarker{}, err
		}
		marker, exists, err := s.readCreateMarker(name)
		if err != nil {
			return createMarker{}, err
		}
		if !exists || !marker.Generated || marker.Header.AppName != app || marker.Header.UserID != user {
			continue
		}
		if marker.StateHash != stateHash {
			return createMarker{}, fmt.Errorf("pending generated session has different initial state")
		}
		if found.Header.ID != "" {
			return createMarker{}, fmt.Errorf("multiple matching pending generated sessions")
		}
		found = marker
	}
	return found, nil
}

func (s *Store) rollbackCreateMarker(name string) error {
	if err := s.paths.root.Remove(createMarkerName(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.fsync {
		return s.syncParentTree(filepath.Dir(name))
	}
	return nil
}

func (s *Store) finishCreateMarker(name string) error {
	markerName := createMarkerName(name)
	markerData, err := s.paths.root.ReadFile(markerName)
	if err != nil {
		return err
	}
	if err := s.paths.root.Remove(markerName); err != nil {
		return err
	}
	if s.fsync {
		if err := s.syncParentTree(filepath.Dir(name)); err != nil {
			if restoreErr := writeFileAtomic(s.paths.root, markerName, markerData, true); restoreErr != nil {
				return fmt.Errorf("sync create-marker removal: %v; restore retry marker: %w", err, restoreErr)
			}
			return fmt.Errorf("sync create-marker removal: %w", err)
		}
	}
	return nil
}

func (s *Store) readDeleteMarker(name string) (uint64, bool, error) {
	data, err := s.paths.root.ReadFile(deleteMarkerName(name))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var incarnation uint64
	if json.Unmarshal(data, &incarnation) != nil || incarnation == 0 {
		return 0, false, fmt.Errorf("invalid delete marker")
	}
	return incarnation, true, nil
}

func (s *Store) finishDeleteMarker(name string) error {
	dir := filepath.Dir(name)
	if s.fsync {
		if err := s.syncDirectory(dir); err != nil {
			return fmt.Errorf("sync deleted session directory: %w", err)
		}
	}
	markerName := deleteMarkerName(name)
	markerData, readErr := s.paths.root.ReadFile(markerName)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := s.paths.root.Remove(markerName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.fsync {
		if err := s.syncDirectory(dir); err != nil {
			if readErr == nil {
				if restoreErr := writeFileAtomic(s.paths.root, markerName, markerData, true); restoreErr != nil {
					return fmt.Errorf("sync delete-marker removal: %v; restore retry marker: %w", err, restoreErr)
				}
			}
			return fmt.Errorf("sync delete-marker removal: %w", err)
		}
	}
	return nil
}

func (s *Store) AppendEvent(ctx context.Context, current session.Session, event *session.Event) error {
	if current == nil {
		return fmt.Errorf("append event: session is nil")
	}
	if event == nil {
		return fmt.Errorf("append event: event is nil")
	}
	durable, ok := current.(*durableSession)
	if !ok || durable == nil || durable.store != s {
		return fmt.Errorf("%w: foreign session handle", ErrInvalidEvent)
	}
	if event.Partial {
		return nil
	}
	if event.ID == "" {
		return ErrInvalidEvent
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.begin(); err != nil {
		return err
	}
	var notices warningBuffer
	defer s.emitWarnings(&notices)
	defer s.end()
	locks, app, name, err := s.locksFor(current.AppName(), current.UserID(), current.ID())
	if err != nil {
		return err
	}
	_, cacheKnown := app.cachedProjection(current.UserID())
	if !cacheKnown {
		if _, err := s.readSharedProjection(current.AppName(), current.UserID(), app, &notices); err != nil {
			return fmt.Errorf("establish shared state baseline: %w", err)
		}
	}
	locks.op.Lock()
	defer locks.op.Unlock()
	locks.io.Lock()
	log, err := loadSessionLog(s.paths, name, current.AppName(), current.UserID(), current.ID(), s.fsync, &notices)
	if err != nil {
		locks.io.Unlock()
		return err
	}
	if durable.header.Incarnation != log.header.Incarnation {
		locks.io.Unlock()
		return fmt.Errorf("%w: stale session incarnation", ErrInvalidEvent)
	}
	stored := cloneEvent(event)
	stored.Actions.StateDelta = withoutTemporaryState(stored.Actions.StateDelta)
	for _, prior := range log.events {
		if prior.ID == event.ID {
			locks.io.Unlock()
			if !sameEvent(prior, stored) {
				return ErrInvalidEvent
			}
			projection, err := s.reconcileCommittedShared(current.AppName(), current.UserID(), name, app, log.stateRecords, &notices)
			if err != nil {
				notices.warn(warning.WarnSessionSnapshotRefresh, name, 0, err.Error())
				if errors.Is(err, errLogicalRecordConflict) {
					return err
				}
			}
			durable.ensureCommitted(prior, projection)
			return nil
		}
	}
	locks.io.Unlock()
	_, appDelta, userDelta := splitState(stored.Actions.StateDelta)
	pendingRecord, err := s.reserveRecord(current.AppName(), app, &notices, func(order uint64) stateRecord {
		return stateRecord{ID: sharedRecordID("event", current.UserID(), current.ID(), log.header.Incarnation, stored.ID), Order: order, Path: name, Line: len(log.events) + 2, UserID: current.UserID(), AppDelta: appDelta, UserDelta: userDelta}
	})
	if err != nil {
		return err
	}
	order := pendingRecord.Order
	data, err := recordLine(record{V: recordVersion, Type: recordEvent, Order: order, Event: stored})
	committed := false
	if err == nil {
		locks.io.Lock()
		committed, err = log.appendBytes(s.paths, data, s.fsync, s.appendCloseHook)
		locks.io.Unlock()
	}
	if err != nil {
		if !committed {
			s.releaseRecordReservation(app, pendingRecord)
		}
		if committed {
			return fmt.Errorf("append event committed before close failed: %w", err)
		}
		return err
	}
	if s.commitHook != nil {
		s.commitHook(current.ID())
	}
	local := durable
	pendingRecords := slices.Clone(log.stateRecords)
	pendingRecords = append(pendingRecords, pendingRecord)
	projection, err := s.reconcileCommittedShared(current.AppName(), current.UserID(), name, app, pendingRecords, &notices)
	if err != nil {
		notices.warn(warning.WarnSessionSnapshotRefresh, name, 0, err.Error())
		if errors.Is(err, errLogicalRecordConflict) {
			return err
		}
	}
	local.appendCommitted(stored, projection)
	return nil
}

func (s *Store) authoritativeFallback(app, user string, locks *appLocks, pending []stateRecord, notices *warningBuffer) (sharedProjection, error) {
	locks.project.Lock()
	defer locks.project.Unlock()
	return s.authoritativeFallbackLocked(app, user, locks, pending, notices)
}

func (s *Store) authoritativeFallbackLocked(app, user string, locks *appLocks, pending []stateRecord, notices *warningBuffer) (sharedProjection, error) {
	fallback := func(cause error) (sharedProjection, error) {
		if errors.Is(cause, errLogicalRecordConflict) {
			return sharedProjection{}, cause
		}
		known, ok := locks.cachedProjection(user)
		if !ok {
			return sharedProjection{}, cause
		}
		projection, err := mergeKnownSharedRecords(known, pending, user)
		if err != nil {
			return sharedProjection{}, err
		}
		locks.rememberProjection(user, projection)
		return projection, cause
	}
	appName, err := s.paths.appJournal(app)
	if err != nil {
		return fallback(err)
	}
	userName, err := s.paths.userJournal(app, user)
	if err != nil {
		return fallback(err)
	}
	appSnapshot, err := s.paths.appState(app)
	if err != nil {
		return fallback(err)
	}
	userSnapshot, err := s.paths.userState(app, user)
	if err != nil {
		return fallback(err)
	}
	for _, pair := range [][2]string{{appName, appSnapshot}, {userName, userSnapshot}} {
		if err := s.requireJournalWhenSnapshotExists(pair[0], pair[1]); err != nil {
			return fallback(err)
		}
	}
	appJournal, err := s.loadJournal(appName, notices)
	if err != nil {
		return fallback(err)
	}
	userJournal, err := s.loadJournal(userName, notices)
	if err != nil {
		return fallback(err)
	}
	appByID := make(map[string]stateJournalRecord, len(appJournal))
	appByOrder := make(map[uint64]stateJournalRecord, len(appJournal))
	userByID := make(map[string]stateJournalRecord, len(userJournal))
	userByOrder := make(map[uint64]stateJournalRecord, len(userJournal))
	for _, record := range appJournal {
		if _, err := addJournalRecord(appByID, appByOrder, record); err != nil {
			return fallback(err)
		}
	}
	for _, record := range userJournal {
		if _, err := addJournalRecord(userByID, userByOrder, record); err != nil {
			return fallback(err)
		}
	}
	for _, record := range pending {
		entry := stateJournalRecord{V: 1, ID: record.ID, Order: record.Order, Delta: maps.Clone(record.AppDelta)}
		exists, err := addJournalRecord(appByID, appByOrder, entry)
		if err != nil {
			return fallback(err)
		}
		if !exists {
			appJournal = append(appJournal, entry)
		}
		if record.UserID == user {
			entry = stateJournalRecord{V: 1, ID: record.ID, Order: record.Order, Delta: maps.Clone(record.UserDelta)}
			exists, err = addJournalRecord(userByID, userByOrder, entry)
			if err != nil {
				return fallback(err)
			}
			if !exists {
				userJournal = append(userJournal, entry)
			}
		}
	}
	appState, appVersions, err := projectJournal(appJournal)
	if err != nil {
		return fallback(err)
	}
	userState, userVersions, err := projectJournal(userJournal)
	if err != nil {
		return fallback(err)
	}
	projection := sharedProjection{App: appState, User: userState, AppVersions: appVersions, UserVersions: userVersions, Records: cloneJournalRecords(appJournal)}
	locks.rememberProjection(user, projection)
	return projection, nil
}

func (s *Store) requireJournalWhenSnapshotExists(journal, snapshot string) error {
	if _, err := s.paths.root.Stat(journal); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := s.paths.root.Stat(snapshot); err == nil {
		return fmt.Errorf("authoritative journal %q is missing while derived snapshot exists", journal)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) reconcileCommittedShared(app, user, path string, locks *appLocks, pending []stateRecord, notices *warningBuffer) (sharedProjection, error) {
	locks.project.Lock()
	defer locks.project.Unlock()
	locks.pendingMu.Lock()
	if locks.pending == nil {
		locks.pending = make(map[string]stateRecord)
	}
	for _, record := range pending {
		locks.pending[record.ID] = record
	}
	pending = pendingRecords(locks.pending)
	locks.pendingMu.Unlock()
	scan, scanErr := s.scanApp(app, notices)
	if scanErr != nil {
		notices.warn(warning.WarnSessionSnapshotRefresh, path, 0, scanErr.Error())
		return s.authoritativeFallbackLocked(app, user, locks, pending, notices)
	}
	projection, repairErr := s.repairSharedLocked(app, user, locks, scan.Records, notices)
	if repairErr != nil {
		notices.warn(warning.WarnSessionSnapshotRefresh, path, 0, repairErr.Error())
		all := append(slices.Clone(scan.Records), pending...)
		return s.authoritativeFallbackLocked(app, user, locks, all, notices)
	}
	locks.pendingMu.Lock()
	for id, record := range locks.pending {
		if record.UserID == user || len(record.UserDelta) == 0 {
			delete(locks.pending, id)
		}
	}
	locks.pendingMu.Unlock()
	return projection, nil
}

func mergeKnownSharedRecords(known sharedProjection, records []stateRecord, user string) (sharedProjection, error) {
	result := cloneSharedProjection(known)
	if result.App == nil {
		result.App = make(map[string]any)
	}
	if result.User == nil {
		result.User = make(map[string]any)
	}
	if result.AppVersions == nil {
		result.AppVersions = make(map[string]keyVersion)
	}
	if result.UserVersions == nil {
		result.UserVersions = make(map[string]keyVersion)
	}
	byID := make(map[string]stateJournalRecord, len(result.Records)+len(records))
	byOrder := make(map[uint64]stateJournalRecord, len(result.Records)+len(records))
	for _, record := range result.Records {
		if _, err := addJournalRecord(byID, byOrder, record); err != nil {
			return sharedProjection{}, err
		}
	}
	records = slices.Clone(records)
	sortStateRecords(records)
	for _, record := range records {
		journalRecord := stateJournalRecord{V: 1, ID: record.ID, Order: record.Order, Delta: maps.Clone(record.AppDelta)}
		exists, err := addJournalRecord(byID, byOrder, journalRecord)
		if err != nil {
			return sharedProjection{}, err
		}
		if !exists {
			result.Records = append(result.Records, journalRecord)
		}
		if err := applyOrderedDelta(result.App, result.AppVersions, record.ID, record.Order, record.AppDelta); err != nil {
			return sharedProjection{}, err
		}
		if record.UserID == user {
			if err := applyOrderedDelta(result.User, result.UserVersions, record.ID, record.Order, record.UserDelta); err != nil {
				return sharedProjection{}, err
			}
		}
	}
	return result, nil
}

func pendingRecords(pending map[string]stateRecord) []stateRecord {
	result := make([]stateRecord, 0, len(pending))
	for _, record := range pending {
		result = append(result, record)
	}
	sortStateRecords(result)
	return result
}

func (s *Store) readSharedProjection(app, user string, locks *appLocks, notices *warningBuffer) (sharedProjection, error) {
	locks.project.Lock()
	defer locks.project.Unlock()
	appName, err := s.paths.appJournal(app)
	if err != nil {
		return sharedProjection{}, err
	}
	userName, err := s.paths.userJournal(app, user)
	if err != nil {
		return sharedProjection{}, err
	}
	appJournal, err := s.loadJournal(appName, notices)
	if err != nil {
		return sharedProjection{}, err
	}
	userJournal, err := s.loadJournal(userName, notices)
	if err != nil {
		return sharedProjection{}, err
	}
	appState, appVersions, err := projectJournal(appJournal)
	if err != nil {
		return sharedProjection{}, err
	}
	userState, userVersions, err := projectJournal(userJournal)
	if err != nil {
		return sharedProjection{}, err
	}
	projection := sharedProjection{App: appState, User: userState, AppVersions: appVersions, UserVersions: userVersions, Records: cloneJournalRecords(appJournal)}
	locks.rememberProjection(user, projection)
	return projection, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closeDone != nil {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.closeDone = make(chan struct{})
	done := s.closeDone
	s.mu.Unlock()
	s.active.Wait()
	var hookErr error
	if s.closeHook != nil {
		hookErr = s.closeHook()
	}
	pathErr := s.paths.close()
	if hookErr == nil {
		hookErr = pathErr
	}
	s.mu.Lock()
	s.closeErr = hookErr
	close(done)
	s.mu.Unlock()
	return hookErr
}

func (s *Store) createLog(name string, data []byte) (bool, error) {
	if err := s.paths.ensureParent(name); err != nil {
		return false, err
	}
	file, err := s.paths.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if errors.Is(err, os.ErrExist) {
		return false, ErrSessionExists
	}
	if err != nil {
		return false, fmt.Errorf("create session log: %w", err)
	}
	if _, err = file.Write(data); err == nil && s.fsync {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		_ = s.paths.root.Remove(name)
		if err != nil {
			return false, fmt.Errorf("write session header: %w", err)
		}
		return false, fmt.Errorf("close session header: %w", closeErr)
	}
	if s.fsync {
		if err := s.syncParentTree(filepath.Dir(name)); err != nil {
			return true, err
		}
	}
	return true, nil
}

type recordInventory struct {
	byID    map[string]reservedRecord
	byOrder map[uint64]reservedRecord
	maximum uint64
}

type reservedRecord struct {
	ID          string
	Order       uint64
	Fingerprint string
	Complete    bool
}

func newRecordInventory() *recordInventory {
	return &recordInventory{byID: make(map[string]reservedRecord), byOrder: make(map[uint64]reservedRecord)}
}

func (inventory *recordInventory) add(record stateRecord) error {
	fingerprint, err := recordFingerprint(record)
	if err != nil {
		return err
	}
	reserved := reservedRecord{ID: record.ID, Order: record.Order, Fingerprint: fingerprint, Complete: true}
	if existing, exists := inventory.byID[record.ID]; exists {
		if !existing.Complete || existing != reserved {
			return fmt.Errorf("%w: %w: logical record %q has contradictory identity or state", ErrCorruptLog, errLogicalRecordConflict, record.ID)
		}
		return nil
	}
	if existing, exists := inventory.byOrder[record.Order]; exists {
		return fmt.Errorf("%w: %w: logical order %d is claimed by records %q and %q", ErrCorruptLog, errLogicalRecordConflict, record.Order, existing.ID, record.ID)
	}
	inventory.byID[record.ID] = reserved
	inventory.byOrder[record.Order] = reserved
	inventory.maximum = max(inventory.maximum, record.Order)
	return nil
}

func (inventory *recordInventory) addIncomplete(record stateRecord) error {
	reserved := reservedRecord{ID: record.ID, Order: record.Order}
	if existing, exists := inventory.byID[record.ID]; exists {
		if existing.ID != reserved.ID || existing.Order != reserved.Order {
			return fmt.Errorf("%w: %w: logical record %q has contradictory retained identity", ErrCorruptLog, errLogicalRecordConflict, record.ID)
		}
		return nil
	}
	if existing, exists := inventory.byOrder[record.Order]; exists {
		return fmt.Errorf("%w: %w: logical order %d is claimed by records %q and %q", ErrCorruptLog, errLogicalRecordConflict, record.Order, existing.ID, record.ID)
	}
	inventory.byID[record.ID] = reserved
	inventory.byOrder[record.Order] = reserved
	inventory.maximum = max(inventory.maximum, record.Order)
	return nil
}

func (inventory *recordInventory) remove(record stateRecord) {
	fingerprint, err := recordFingerprint(record)
	if err != nil {
		return
	}
	existing, exists := inventory.byID[record.ID]
	if !exists || !existing.Complete || existing.Order != record.Order || existing.Fingerprint != fingerprint {
		return
	}
	delete(inventory.byID, record.ID)
	delete(inventory.byOrder, record.Order)
}

func recordFingerprint(record stateRecord) (string, error) {
	appDelta := record.AppDelta
	if appDelta == nil {
		appDelta = map[string]any{}
	}
	userDelta := record.UserDelta
	if userDelta == nil {
		userDelta = map[string]any{}
	}
	data, err := json.Marshal(struct {
		UserID    string         `json:"userId"`
		AppDelta  map[string]any `json:"appDelta"`
		UserDelta map[string]any `json:"userDelta"`
	}{UserID: record.UserID, AppDelta: appDelta, UserDelta: userDelta})
	if err != nil {
		return "", fmt.Errorf("encode logical record fingerprint: %w", err)
	}
	return string(data), nil
}

type inventoryPart struct {
	record  stateRecord
	appSet  bool
	userSet bool
}

type inventoryBuilder struct {
	byID    map[string]*inventoryPart
	byOrder map[uint64]string
}

func newInventoryBuilder() *inventoryBuilder {
	return &inventoryBuilder{byID: make(map[string]*inventoryPart), byOrder: make(map[uint64]string)}
}

func (builder *inventoryBuilder) claim(id string, order uint64) (*inventoryPart, error) {
	if id == "" || order == 0 {
		return nil, fmt.Errorf("%w: logical record has empty identity or order", ErrCorruptLog)
	}
	if existing, exists := builder.byOrder[order]; exists && existing != id {
		return nil, fmt.Errorf("%w: %w: logical order %d is claimed by records %q and %q", ErrCorruptLog, errLogicalRecordConflict, order, existing, id)
	}
	part := builder.byID[id]
	if part == nil {
		part = &inventoryPart{record: stateRecord{ID: id, Order: order}}
		builder.byID[id] = part
		builder.byOrder[order] = id
		return part, nil
	}
	if part.record.Order != order {
		return nil, fmt.Errorf("%w: %w: logical record %q claims orders %d and %d", ErrCorruptLog, errLogicalRecordConflict, id, part.record.Order, order)
	}
	return part, nil
}

func sameRecordDelta(left, right map[string]any) (bool, error) {
	if len(left) == 0 && len(right) == 0 {
		return true, nil
	}
	leftData, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	rightData, err := json.Marshal(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftData, rightData), nil
}

func (builder *inventoryBuilder) addApp(record stateJournalRecord) error {
	part, err := builder.claim(record.ID, record.Order)
	if err != nil {
		return err
	}
	if part.appSet {
		equal, err := sameRecordDelta(part.record.AppDelta, record.Delta)
		if err != nil {
			return err
		}
		if !equal {
			return fmt.Errorf("%w: %w: logical record %q has contradictory app state", ErrCorruptLog, errLogicalRecordConflict, record.ID)
		}
		return nil
	}
	part.record.AppDelta = maps.Clone(record.Delta)
	part.appSet = true
	return nil
}

func (builder *inventoryBuilder) addUser(user string, record stateJournalRecord) error {
	part, err := builder.claim(record.ID, record.Order)
	if err != nil {
		return err
	}
	if part.userSet {
		equal, err := sameRecordDelta(part.record.UserDelta, record.Delta)
		if err != nil {
			return err
		}
		if part.record.UserID != user || !equal {
			return fmt.Errorf("%w: %w: logical record %q has contradictory user state", ErrCorruptLog, errLogicalRecordConflict, record.ID)
		}
		return nil
	}
	part.record.UserID = user
	part.record.UserDelta = maps.Clone(record.Delta)
	part.userSet = true
	return nil
}

func (builder *inventoryBuilder) addFull(record stateRecord) error {
	part, err := builder.claim(record.ID, record.Order)
	if err != nil {
		return err
	}
	if err := builder.addApp(stateJournalRecord{ID: record.ID, Order: record.Order, Delta: record.AppDelta}); err != nil {
		return err
	}
	if err := builder.addUser(record.UserID, stateJournalRecord{ID: record.ID, Order: record.Order, Delta: record.UserDelta}); err != nil {
		return err
	}
	part.record.Path = record.Path
	part.record.Line = record.Line
	return nil
}

func (builder *inventoryBuilder) inventory() (*recordInventory, error) {
	inventory := newRecordInventory()
	for _, part := range builder.byID {
		if !part.appSet || !part.userSet {
			if err := inventory.addIncomplete(part.record); err != nil {
				return nil, err
			}
			continue
		}
		if err := inventory.add(part.record); err != nil {
			return nil, err
		}
	}
	return inventory, nil
}

func (s *Store) buildRecordInventory(app string, locks *appLocks, notices *warningBuffer) (*recordInventory, error) {
	builder := newInventoryBuilder()
	locks.cacheMu.RLock()
	if !locks.appKnown {
		locks.cacheMu.RUnlock()
		return nil, errors.New("shared state record inventory is not established")
	}
	retained := cloneJournalRecords(locks.app.Records)
	locks.cacheMu.RUnlock()
	for _, record := range retained {
		if err := builder.addApp(record); err != nil {
			return nil, err
		}
	}
	userJournals, err := s.scanUserJournals(app, notices)
	if err != nil {
		return nil, err
	}
	for user, journal := range userJournals {
		for _, record := range journal {
			if err := builder.addUser(user, record); err != nil {
				return nil, err
			}
		}
	}
	transcriptRecords, err := s.scanTranscriptRecordInventory(app)
	if err != nil {
		return nil, err
	}
	for _, record := range transcriptRecords {
		if err := builder.addFull(record); err != nil {
			return nil, err
		}
	}
	locks.pendingMu.RLock()
	pending := pendingRecords(locks.pending)
	locks.pendingMu.RUnlock()
	for _, record := range pending {
		if err := builder.addFull(record); err != nil {
			return nil, err
		}
	}
	markers, err := s.scanCreateMarkers(app)
	if err != nil {
		return nil, err
	}
	for _, record := range markers {
		if err := builder.addFull(record); err != nil {
			return nil, err
		}
	}
	return builder.inventory()
}

func (s *Store) scanUserJournals(app string, notices *warningBuffer) (map[string][]stateJournalRecord, error) {
	encoded, err := encodeSegment(app)
	if err != nil {
		return nil, err
	}
	base := filepath.Join("apps", encoded, "users")
	users, err := readDir(s.paths.root, base)
	if errors.Is(err, os.ErrNotExist) {
		return map[string][]stateJournalRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string][]stateJournalRecord)
	for _, entry := range users {
		if !entry.IsDir() {
			continue
		}
		user, err := decodeSegment(entry.Name())
		if err != nil {
			continue
		}
		name, err := s.paths.userJournal(app, user)
		if err != nil {
			return nil, err
		}
		journal, err := s.loadJournal(name, notices)
		if err != nil {
			return nil, err
		}
		result[user] = journal
	}
	return result, nil
}

// scanTranscriptRecordInventory recovers every identifiable logical record
// from active transcripts without requiring the transcript to be usable as a
// session. Reservation must account for valid committed records after a crash,
// but an unrelated corrupt transcript remains the projection layer's problem.
func (s *Store) scanTranscriptRecordInventory(app string) ([]stateRecord, error) {
	encoded, err := encodeSegment(app)
	if err != nil {
		return nil, err
	}
	base := filepath.Join("apps", encoded, "users")
	users, err := readDir(s.paths.root, base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []stateRecord
	for _, userEntry := range users {
		if !userEntry.IsDir() {
			continue
		}
		user, err := decodeSegment(userEntry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(base, userEntry.Name(), "sessions")
		entries, err := readDir(s.paths.root, dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			id, err := decodeSegment(strings.TrimSuffix(entry.Name(), ".jsonl"))
			if err != nil {
				continue
			}
			locks, _, name, err := s.locksFor(app, user, id)
			if err != nil {
				return nil, err
			}
			if s.inventoryHook != nil {
				s.inventoryHook(name)
			}
			locks.io.RLock()
			data, err := s.paths.root.ReadFile(name)
			locks.io.RUnlock()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read session record inventory: %w", err)
			}
			records = append(records, transcriptRecordInventory(data, name, app, user, id)...)
		}
	}
	return records, nil
}

func transcriptRecordInventory(data []byte, name, app, user, id string) []stateRecord {
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	var records []stateRecord
	var incarnation uint64
	for line, raw := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		if len(raw) == 0 {
			continue
		}
		rec, _, err := decodeRecord(raw)
		if err != nil || rec.Order == 0 {
			continue
		}
		state := stateRecord{Order: rec.Order, Path: name, Line: line + 1, UserID: user}
		switch {
		case line == 0 && rec.Type == recordSession && rec.Session != nil && rec.Session.ID == id && rec.Session.AppName == app && rec.Session.UserID == user && rec.Session.Incarnation == rec.Order:
			incarnation = rec.Session.Incarnation
			state.ID = sharedRecordID("create", user, id, incarnation, "")
			state.AppDelta = maps.Clone(rec.Session.AppDelta)
			state.UserDelta = maps.Clone(rec.Session.UserDelta)
		case rec.Type == recordEvent && rec.Event != nil && rec.Event.ID != "" && incarnation != 0:
			state.ID = sharedRecordID("event", user, id, incarnation, rec.Event.ID)
			_, state.AppDelta, state.UserDelta = splitState(rec.Event.Actions.StateDelta)
		case rec.Type == "" && incarnation != 0:
			state.ID = sharedRecordID("forward", user, id, incarnation, fmt.Sprintf("%d:%d", line+1, rec.Order))
		default:
			// A parseable order without enough valid session identity still
			// reserves its sequence number. Its synthetic identity is stable
			// for this path and line, so exact rescans remain idempotent.
			state.ID = sharedRecordID("unresolved", user, id, 0, fmt.Sprintf("%s:%d:%d", name, line+1, rec.Order))
		}
		records = append(records, state)
	}
	return records
}

func (s *Store) scanCreateMarkers(app string) ([]stateRecord, error) {
	encoded, err := encodeSegment(app)
	if err != nil {
		return nil, err
	}
	base := filepath.Join("apps", encoded, "users")
	users, err := readDir(s.paths.root, base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	const suffix = ".jsonl.create-pending"
	var records []stateRecord
	for _, userEntry := range users {
		if !userEntry.IsDir() {
			continue
		}
		user, err := decodeSegment(userEntry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(base, userEntry.Name(), "sessions")
		entries, err := readDir(s.paths.root, dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
				continue
			}
			id, err := decodeSegment(strings.TrimSuffix(entry.Name(), suffix))
			if err != nil {
				continue
			}
			name, err := s.paths.sessionLog(app, user, id)
			if err != nil {
				return nil, err
			}
			marker, exists, err := s.readCreateMarker(name)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			if marker.Header.AppName != app || marker.Header.UserID != user || marker.Header.ID != id {
				return nil, fmt.Errorf("%w: create transaction identity contradicts its path", ErrCorruptLog)
			}
			records = append(records, stateRecord{ID: sharedRecordID("create", user, id, marker.Header.Incarnation, ""), Order: marker.Header.Incarnation, Path: name, Line: 1, UserID: user, AppDelta: marker.Header.AppDelta, UserDelta: marker.Header.UserDelta})
		}
	}
	return records, nil
}

func (s *Store) ensureRecordInventory(app string, locks *appLocks, notices *warningBuffer) error {
	locks.order.Lock()
	ready := locks.inventory != nil
	locks.order.Unlock()
	if ready {
		return nil
	}
	locks.invInit.Lock()
	defer locks.invInit.Unlock()
	locks.order.Lock()
	ready = locks.inventory != nil
	locks.order.Unlock()
	if ready {
		return nil
	}
	locks.project.Lock()
	inventory, err := s.buildRecordInventory(app, locks, notices)
	if err != nil {
		locks.project.Unlock()
		return err
	}
	locks.order.Lock()
	locks.inventory = inventory
	locks.order.Unlock()
	locks.project.Unlock()
	return nil
}

func (s *Store) releaseRecordReservation(locks *appLocks, record stateRecord) {
	locks.order.Lock()
	defer locks.order.Unlock()
	if locks.inventory != nil {
		locks.inventory.remove(record)
	}
}

func (s *Store) reserveRecord(app string, locks *appLocks, notices *warningBuffer, candidate func(uint64) stateRecord) (stateRecord, error) {
	if err := s.ensureRecordInventory(app, locks, notices); err != nil {
		return stateRecord{}, err
	}
	locks.order.Lock()
	defer locks.order.Unlock()
	name, err := s.paths.appSequence(app)
	if err != nil {
		return stateRecord{}, err
	}
	if err := s.paths.ensureParent(name); err != nil {
		return stateRecord{}, err
	}
	current, _, readErr := readUint64File(s.paths.root, name)
	current = max(current, locks.inventory.maximum)
	if readErr != nil {
		notices.warn(warning.WarnSessionSnapshotRefresh, name, 0, "repaired invalid shared order sequence: "+readErr.Error())
	}
	if current == math.MaxUint64 {
		return stateRecord{}, fmt.Errorf("session order exhausted")
	}
	current++
	record := candidate(current)
	if err := locks.inventory.add(record); err != nil {
		return stateRecord{}, err
	}
	if err := s.writeSequence(name, current); err != nil {
		locks.inventory.remove(record)
		return stateRecord{}, err
	}
	return record, nil
}

func (s *Store) validateReservedRecord(app string, locks *appLocks, notices *warningBuffer, record stateRecord) error {
	if err := s.ensureRecordInventory(app, locks, notices); err != nil {
		return err
	}
	locks.order.Lock()
	defer locks.order.Unlock()
	if err := locks.inventory.add(record); err != nil {
		return err
	}
	name, err := s.paths.appSequence(app)
	if err != nil {
		return err
	}
	if err := s.paths.ensureParent(name); err != nil {
		return err
	}
	current, exists, readErr := readUint64File(s.paths.root, name)
	needsWrite := !exists || readErr != nil || current < locks.inventory.maximum
	current = max(current, locks.inventory.maximum)
	if readErr != nil {
		notices.warn(warning.WarnSessionSnapshotRefresh, name, 0, "repaired invalid shared order sequence: "+readErr.Error())
	}
	if needsWrite {
		return s.writeSequence(name, current)
	}
	return nil
}

func (s *Store) writeSequence(name string, value uint64) error {
	if s.sequenceHook != nil {
		if err := s.sequenceHook(); err != nil {
			return fmt.Errorf("write shared order sequence: %w", err)
		}
	}
	return writeUint64File(s.paths.root, name, value, s.fsync)
}

type appScan struct {
	Logs    map[string]*sessionLog
	Records []stateRecord
}

func (s *Store) scanApp(app string, notices *warningBuffer) (appScan, error) {
	encoded, err := encodeSegment(app)
	if err != nil {
		return appScan{}, err
	}
	base := filepath.Join("apps", encoded, "users")
	users, err := readDir(s.paths.root, base)
	if errors.Is(err, os.ErrNotExist) {
		return appScan{Logs: make(map[string]*sessionLog)}, nil
	}
	if err != nil {
		return appScan{}, err
	}
	out := appScan{Logs: make(map[string]*sessionLog)}
	for _, userEntry := range users {
		if !userEntry.IsDir() {
			continue
		}
		user, err := decodeSegment(userEntry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(base, userEntry.Name(), "sessions")
		entries, err := readDir(s.paths.root, dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return appScan{}, err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			id, err := decodeSegment(entry.Name()[:len(entry.Name())-6])
			if err != nil {
				continue
			}
			locks, _, name, err := s.locksFor(app, user, id)
			if err != nil {
				return appScan{}, err
			}
			if s.scanEntryHook != nil {
				s.scanEntryHook(name)
			}
			locks.io.Lock()
			log, err := loadSessionLog(s.paths, name, app, user, id, s.fsync, notices)
			locks.io.Unlock()
			if errors.Is(err, ErrSessionNotFound) {
				continue
			}
			if err != nil {
				return appScan{}, err
			}
			out.Logs[name] = log
			out.Records = append(out.Records, log.stateRecords...)
		}
	}
	sortStateRecords(out.Records)
	for index := 1; index < len(out.Records); index++ {
		if out.Records[index].Order == out.Records[index-1].Order {
			r := out.Records[index]
			notices.warn(warning.WarnSessionOrderDuplicate, r.Path, r.Line, "duplicate positive record order")
			return appScan{}, fmt.Errorf("%w: duplicate order %d", ErrCorruptLog, r.Order)
		}
	}
	return out, nil
}

type sharedProjection struct {
	App          map[string]any
	User         map[string]any
	AppVersions  map[string]keyVersion
	UserVersions map[string]keyVersion
	Records      []stateJournalRecord
}

type keyVersion struct {
	Order    uint64 `json:"order"`
	RecordID string `json:"recordId"`
}

type projectionScope struct {
	State    map[string]any
	Versions map[string]keyVersion
	Records  []stateJournalRecord
}

func applyOrderedDelta(state map[string]any, versions map[string]keyVersion, recordID string, order uint64, delta map[string]any) error {
	for key, value := range delta {
		current, exists := versions[key]
		if exists && order < current.Order {
			continue
		}
		if exists && order == current.Order {
			if current.RecordID != recordID || !reflect.DeepEqual(state[key], value) {
				return fmt.Errorf("shared state key %q has conflicting records at order %d", key, order)
			}
			continue
		}
		state[key] = value
		versions[key] = keyVersion{Order: order, RecordID: recordID}
	}
	return nil
}

func cloneSharedProjection(value sharedProjection) sharedProjection {
	return sharedProjection{
		App:          maps.Clone(value.App),
		User:         maps.Clone(value.User),
		AppVersions:  maps.Clone(value.AppVersions),
		UserVersions: maps.Clone(value.UserVersions),
		Records:      cloneJournalRecords(value.Records),
	}
}

func cloneProjectionScope(value projectionScope) projectionScope {
	return projectionScope{State: maps.Clone(value.State), Versions: maps.Clone(value.Versions), Records: cloneJournalRecords(value.Records)}
}

func cloneJournalRecords(records []stateJournalRecord) []stateJournalRecord {
	cloned := make([]stateJournalRecord, len(records))
	for index, record := range records {
		record.Delta = maps.Clone(record.Delta)
		cloned[index] = record
	}
	return cloned
}

// rememberProjection records the store-wide recovery baseline.
func (l *appLocks) rememberProjection(user string, projection sharedProjection) {
	l.cacheMu.Lock()
	defer l.cacheMu.Unlock()
	l.app = projectionScope{State: maps.Clone(projection.App), Versions: maps.Clone(projection.AppVersions), Records: cloneJournalRecords(projection.Records)}
	l.appKnown = true
	if l.users == nil {
		l.users = make(map[string]projectionScope)
	}
	l.users[user] = projectionScope{State: maps.Clone(projection.User), Versions: maps.Clone(projection.UserVersions)}
}

// cachedProjection returns only a baseline established from healthy authority.
func (l *appLocks) cachedProjection(user string) (sharedProjection, bool) {
	l.cacheMu.RLock()
	defer l.cacheMu.RUnlock()
	userProjection, userKnown := l.users[user]
	if !l.appKnown || !userKnown {
		return sharedProjection{}, false
	}
	appProjection := cloneProjectionScope(l.app)
	userProjection = cloneProjectionScope(userProjection)
	return sharedProjection{
		App:          appProjection.State,
		User:         userProjection.State,
		AppVersions:  appProjection.Versions,
		UserVersions: userProjection.Versions,
		Records:      appProjection.Records,
	}, true
}

func sortStateRecords(records []stateRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].Order != records[j].Order {
			return records[i].Order < records[j].Order
		}
		if records[i].Path != records[j].Path {
			return records[i].Path < records[j].Path
		}
		return records[i].Line < records[j].Line
	})
}

func (s *Store) repairShared(app, user string, locks *appLocks, records []stateRecord, notices *warningBuffer) (sharedProjection, error) {
	locks.project.Lock()
	defer locks.project.Unlock()
	return s.repairSharedLocked(app, user, locks, records, notices)
}

func (s *Store) repairSharedLocked(app, user string, locks *appLocks, records []stateRecord, notices *warningBuffer) (sharedProjection, error) {
	if s.projectHook != nil {
		if err := s.projectHook(); err != nil {
			return sharedProjection{}, err
		}
	}
	sortStateRecords(records)
	appName, err := s.paths.appJournal(app)
	if err != nil {
		return sharedProjection{}, err
	}
	userName, err := s.paths.userJournal(app, user)
	if err != nil {
		return sharedProjection{}, err
	}
	appSnapshot, err := s.paths.appState(app)
	if err != nil {
		return sharedProjection{}, err
	}
	userSnapshot, err := s.paths.userState(app, user)
	if err != nil {
		return sharedProjection{}, err
	}
	for _, pair := range [][2]string{{appName, appSnapshot}, {userName, userSnapshot}} {
		if err := s.requireJournalWhenSnapshotExists(pair[0], pair[1]); err != nil {
			return sharedProjection{}, err
		}
	}
	appJournal, appChanged, err := s.repairJournal(appName, records, func(r stateRecord) map[string]any { return r.AppDelta }, func(stateRecord) bool { return true }, true, notices)
	if err != nil {
		return sharedProjection{}, err
	}
	userJournal, userChanged, err := s.repairJournal(userName, records, func(r stateRecord) map[string]any { return r.UserDelta }, func(r stateRecord) bool { return r.UserID == user }, true, notices)
	if err != nil {
		return sharedProjection{}, err
	}
	appState, appVersions, err := projectJournal(appJournal)
	if err != nil {
		return sharedProjection{}, err
	}
	userState, userVersions, err := projectJournal(userJournal)
	if err != nil {
		return sharedProjection{}, err
	}
	projection := sharedProjection{App: appState, User: userState, AppVersions: appVersions, UserVersions: userVersions, Records: cloneJournalRecords(appJournal)}
	through := maxJournalOrder(appJournal, userJournal)
	stale, err := s.projectionStale(app, user, projection, through)
	if err != nil {
		return projection, err
	}
	if appChanged || userChanged || stale {
		if err := s.writeProjection(app, user, projection, through); err != nil {
			return projection, err
		}
	}
	locks.rememberProjection(user, projection)
	return projection, nil
}

func (s *Store) projectionStale(app, user string, projection sharedProjection, through uint64) (bool, error) {
	appName, err := s.paths.appState(app)
	if err != nil {
		return false, err
	}
	userName, err := s.paths.userState(app, user)
	if err != nil {
		return false, err
	}
	want := []projectionScope{
		{State: projection.App, Versions: projection.AppVersions},
		{State: projection.User, Versions: projection.UserVersions},
	}
	for index, name := range []string{appName, userName} {
		data, err := s.paths.root.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		var snapshot struct {
			Through  uint64                `json:"through"`
			State    map[string]any        `json:"state"`
			Versions map[string]keyVersion `json:"versions"`
		}
		if json.Unmarshal(data, &snapshot) != nil || snapshot.Through != through || !reflect.DeepEqual(snapshot.State, want[index].State) || !reflect.DeepEqual(snapshot.Versions, want[index].Versions) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) repairJournal(name string, records []stateRecord, delta func(stateRecord) map[string]any, include func(stateRecord) bool, keepEmpty bool, notices *warningBuffer) ([]stateJournalRecord, bool, error) {
	journal, err := s.loadJournal(name, notices)
	if err != nil {
		return nil, false, err
	}
	changed := false
	byID := make(map[string]stateJournalRecord, len(journal))
	byOrder := make(map[uint64]stateJournalRecord, len(journal))
	for _, record := range journal {
		if _, err := addJournalRecord(byID, byOrder, record); err != nil {
			return nil, false, err
		}
	}
	var additions []stateJournalRecord
	for _, record := range records {
		value := delta(record)
		if !include(record) || len(value) == 0 && !keepEmpty {
			continue
		}
		entry := stateJournalRecord{V: 1, ID: record.ID, Order: record.Order, Delta: maps.Clone(value)}
		exists, err := addJournalRecord(byID, byOrder, entry)
		if err != nil {
			return nil, false, err
		}
		if exists {
			continue
		}
		additions = append(additions, entry)
	}
	for _, entry := range additions {
		if s.journalHook != nil {
			if err := s.journalHook(); err != nil {
				return nil, false, err
			}
		}
		data, err := marshalJournalRecord(entry)
		if err != nil {
			return nil, false, err
		}
		if err := s.appendFile(name, data); err != nil {
			return nil, false, err
		}
		journal = append(journal, entry)
		changed = true
	}
	sort.Slice(journal, func(i, j int) bool { return journal[i].Order < journal[j].Order })
	return journal, changed, nil
}

var errLogicalRecordConflict = errors.New("logical record identity conflict")

// addJournalRecord validates the global identity of a logical record before it
// is projected or appended. Exact replay is idempotent; an ID or order reused
// by a different record is corruption even when both deltas are empty.
func addJournalRecord(byID map[string]stateJournalRecord, byOrder map[uint64]stateJournalRecord, record stateJournalRecord) (bool, error) {
	if existing, exists := byID[record.ID]; exists {
		if existing.Order != record.Order || !reflect.DeepEqual(existing.Delta, record.Delta) {
			return false, fmt.Errorf("%w: %w: state journal record %q contradicts logical record", ErrCorruptLog, errLogicalRecordConflict, record.ID)
		}
		return true, nil
	}
	if existing, exists := byOrder[record.Order]; exists {
		return false, fmt.Errorf("%w: %w: logical order %d is claimed by records %q and %q", ErrCorruptLog, errLogicalRecordConflict, record.Order, existing.ID, record.ID)
	}
	byID[record.ID] = record
	byOrder[record.Order] = record
	return false, nil
}

// maxJournalOrder returns the greatest committed order retained by the
// authoritative journals, including records with empty state deltas.
func maxJournalOrder(journals ...[]stateJournalRecord) uint64 {
	var maximum uint64
	for _, journal := range journals {
		for _, record := range journal {
			maximum = max(maximum, record.Order)
		}
	}
	return maximum
}

func (s *Store) loadJournal(name string, notices *warningBuffer) ([]stateJournalRecord, error) {
	data, err := s.paths.root.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state journal: %w", err)
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		cut := bytes.LastIndexByte(data, '\n') + 1
		file, openErr := s.paths.root.OpenFile(name, os.O_WRONLY, fileMode)
		if openErr != nil {
			return nil, openErr
		}
		err = file.Truncate(int64(cut))
		if err == nil && s.fsync {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		notices.warn(warning.WarnSessionLogTornTail, name, 0, "discarded final unterminated record")
		data = data[:cut]
	}
	var result []stateJournalRecord
	seenIDs := make(map[string]struct{})
	seenOrders := make(map[uint64]struct{})
	for line, raw := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		if len(raw) == 0 {
			continue
		}
		var record stateJournalRecord
		if err := json.Unmarshal(raw, &record); err != nil || record.V != 1 || record.ID == "" || record.Order == 0 {
			if err == nil {
				err = errors.New("invalid state journal record")
			}
			return nil, fmt.Errorf("%w: %s line %d: %v", ErrCorruptLog, name, line+1, err)
		}
		if _, exists := seenIDs[record.ID]; exists {
			return nil, fmt.Errorf("%w: %s line %d: duplicate journal id %q", ErrCorruptLog, name, line+1, record.ID)
		}
		if _, exists := seenOrders[record.Order]; exists {
			return nil, fmt.Errorf("%w: %s line %d: duplicate journal order %d", ErrCorruptLog, name, line+1, record.Order)
		}
		seenIDs[record.ID] = struct{}{}
		seenOrders[record.Order] = struct{}{}
		result = append(result, record)
	}
	return result, nil
}

func (s *Store) appendFile(name string, data []byte) error {
	if err := s.paths.ensureParent(name); err != nil {
		return err
	}
	file, err := s.paths.root.OpenFile(name, os.O_WRONLY|os.O_APPEND|os.O_CREATE, fileMode)
	if err != nil {
		return fmt.Errorf("open append-only state journal: %w", err)
	}
	if err := s.paths.root.Chmod(name, fileMode); err != nil {
		_ = file.Close()
		return fmt.Errorf("set state journal permissions: %w", err)
	}
	if _, err = file.Write(data); err == nil && s.fsync {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("append state journal: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close state journal: %w", closeErr)
	}
	if s.fsync {
		return s.syncParentTree(filepath.Dir(name))
	}
	return nil
}

func projectJournal(records []stateJournalRecord) (map[string]any, map[string]keyVersion, error) {
	records = slices.Clone(records)
	sort.Slice(records, func(i, j int) bool { return records[i].Order < records[j].Order })
	state := make(map[string]any)
	versions := make(map[string]keyVersion)
	byID := make(map[string]stateJournalRecord, len(records))
	byOrder := make(map[uint64]stateJournalRecord, len(records))
	for _, record := range records {
		if _, err := addJournalRecord(byID, byOrder, record); err != nil {
			return nil, nil, err
		}
		if err := applyOrderedDelta(state, versions, record.ID, record.Order, record.Delta); err != nil {
			return nil, nil, err
		}
	}
	return state, versions, nil
}

func (s *Store) writeProjection(app, user string, projection sharedProjection, through uint64) error {
	appName, err := s.paths.appState(app)
	if err != nil {
		return err
	}
	userName, err := s.paths.userState(app, user)
	if err != nil {
		return err
	}
	value := func(state map[string]any, versions map[string]keyVersion) ([]byte, error) {
		return json.Marshal(struct {
			Through  uint64                `json:"through"`
			State    map[string]any        `json:"state"`
			Versions map[string]keyVersion `json:"versions"`
		}{Through: through, State: state, Versions: versions})
	}
	appData, err := value(projection.App, projection.AppVersions)
	if err != nil {
		return err
	}
	userData, err := value(projection.User, projection.UserVersions)
	if err != nil {
		return err
	}
	if err := s.paths.ensureParent(appName); err != nil {
		return err
	}
	if err := s.paths.ensureParent(userName); err != nil {
		return err
	}
	if err := writeFileAtomic(s.paths.root, appName, appData, s.fsync); err != nil {
		return err
	}
	return writeFileAtomic(s.paths.root, userName, userData, s.fsync)
}

func (s *Store) syncParentTree(name string) error {
	for current := name; current != "."; current = filepath.Dir(current) {
		if err := s.syncDirectory(current); err != nil {
			return err
		}
	}
	return s.syncDirectory(".")
}

func (s *Store) syncDirectory(name string) error {
	if s.dirSyncHook != nil {
		return s.dirSyncHook(name)
	}
	dir, err := s.paths.root.Open(name)
	if err != nil {
		return fmt.Errorf("open storage directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync storage directory: %w", err)
	}
	return nil
}

func (s *Store) identityPath(app, user, id string) (string, error) {
	if _, _, err := encodeIdentity(app, user); err != nil {
		return "", err
	}
	if _, err := encodeSegment(id); err != nil {
		return "", err
	}
	return s.paths.sessionLog(app, user, id)
}

func sessionState(log *sessionLog, projection sharedProjection) map[string]any {
	local := cloneMap(log.header.State)
	if local == nil {
		local = make(map[string]any)
	}
	for _, event := range log.events {
		eventLocal, _, _ := splitState(event.Actions.StateDelta)
		maps.Copy(local, eventLocal)
	}
	return mergeState(local, projection.App, projection.User)
}

func cloneHeader(value header) header {
	value.State = cloneMap(value.State)
	value.AppDelta = cloneMap(value.AppDelta)
	value.UserDelta = cloneMap(value.UserDelta)
	return value
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return maps.Clone(value)
	}
	var clone map[string]any
	if json.Unmarshal(data, &clone) != nil {
		return maps.Clone(value)
	}
	return clone
}

func cloneEvent(value *session.Event) *session.Event {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		clone := *value
		return &clone
	}
	var clone session.Event
	if json.Unmarshal(data, &clone) != nil {
		clone = *value
	}
	return &clone
}

func sameEvent(left, right *session.Event) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func readDir(root *os.Root, name string) ([]os.DirEntry, error) {
	dir, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

type durableSession struct {
	mu      sync.RWMutex
	store   *Store
	header  header
	state   map[string]any
	events  []*session.Event
	updated time.Time
}

func newDurableSession(store *Store, header header, events []*session.Event, state map[string]any, updated time.Time) *durableSession {
	cloned := make([]*session.Event, len(events))
	for index, event := range events {
		cloned[index] = cloneEvent(event)
	}
	return &durableSession{store: store, header: cloneHeader(header), state: cloneMap(state), events: cloned, updated: updated}
}

func (s *durableSession) ID() string             { return s.header.ID }
func (s *durableSession) AppName() string        { return s.header.AppName }
func (s *durableSession) UserID() string         { return s.header.UserID }
func (s *durableSession) State() session.State   { return sessionStateView{session: s} }
func (s *durableSession) Events() session.Events { return sessionEventsView{session: s} }
func (s *durableSession) LastUpdateTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updated
}

func (s *durableSession) appendCommitted(event *session.Event, projection sharedProjection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendCommittedLocked(event, projection)
}

func (s *durableSession) appendCommittedLocked(event *session.Event, projection sharedProjection) {
	local, _, _ := splitState(event.Actions.StateDelta)
	maps.Copy(s.state, local)
	for key := range s.state {
		if len(key) > 4 && key[:4] == "app:" || len(key) > 5 && key[:5] == "user:" {
			delete(s.state, key)
		}
	}
	maps.Copy(s.state, mergeState(nil, projection.App, projection.User))
	s.events = append(s.events, cloneEvent(event))
	s.updated = event.Timestamp
}

func (s *durableSession) ensureCommitted(event *session.Event, projection sharedProjection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, current := range s.events {
		if current.ID == event.ID {
			s.replaceSharedLocked(projection)
			return
		}
	}
	s.appendCommittedLocked(event, projection)
}

func (s *durableSession) replaceSharedLocked(projection sharedProjection) {
	for key := range s.state {
		if len(key) > 4 && key[:4] == "app:" || len(key) > 5 && key[:5] == "user:" {
			delete(s.state, key)
		}
	}
	maps.Copy(s.state, mergeState(nil, projection.App, projection.User))
}

type sessionStateView struct{ session *durableSession }

func (v sessionStateView) Get(key string) (any, error) {
	v.session.mu.RLock()
	defer v.session.mu.RUnlock()
	value, exists := v.session.state[key]
	if !exists {
		return nil, session.ErrStateKeyNotExist
	}
	return value, nil
}

func (v sessionStateView) Set(key string, value any) error {
	v.session.mu.Lock()
	defer v.session.mu.Unlock()
	if v.session.state == nil {
		v.session.state = make(map[string]any)
	}
	v.session.state[key] = value
	return nil
}

func (v sessionStateView) All() iter.Seq2[string, any] {
	v.session.mu.RLock()
	clone := maps.Clone(v.session.state)
	v.session.mu.RUnlock()
	return func(yield func(string, any) bool) {
		for key, value := range clone {
			if !yield(key, value) {
				return
			}
		}
	}
}

type sessionEventsView struct{ session *durableSession }

func (v sessionEventsView) All() iter.Seq[*session.Event] {
	v.session.mu.RLock()
	events := slices.Clone(v.session.events)
	v.session.mu.RUnlock()
	return slices.Values(events)
}

func (v sessionEventsView) Len() int {
	v.session.mu.RLock()
	defer v.session.mu.RUnlock()
	return len(v.session.events)
}

func (v sessionEventsView) At(index int) *session.Event {
	v.session.mu.RLock()
	defer v.session.mu.RUnlock()
	if index < 0 || index >= len(v.session.events) {
		return nil
	}
	return v.session.events[index]
}

var _ = time.Time{}
