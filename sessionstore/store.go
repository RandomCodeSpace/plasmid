package sessionstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/plasmid-dev/plasmid/loop"
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
	mu       sync.Mutex
	closed   bool
	locks    map[string]*sync.Mutex
}

var _ loop.SessionStore = (*Store)(nil)

func Open(dir string) (*Store, error) { return OpenWith(Options{Dir: dir}) }
func OpenWith(o Options) (*Store, error) {
	p, e := openPaths(o.Dir)
	if e != nil {
		return nil, e
	}
	f := true
	if o.Fsync != nil {
		f = *o.Fsync
	}
	n := o.NewID
	if n == nil {
		n = newUUIDv4
	}
	l := o.Logger
	if l == nil {
		l = slog.Default()
	}
	w := o.WarningSink
	if w == nil {
		w = warning.SlogSink{Logger: l}
	}
	return &Store{paths: p, fsync: f, newID: n, warnings: w, locks: map[string]*sync.Mutex{}}, nil
}
func (s *Store) open() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}
func (s *Store) appLock(app string) (*sync.Mutex, error) {
	key, e := encodeSegment(app)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.locks[key]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[key] = m
	}
	return m, nil
}
func (s *Store) warn(code, path string, line int, message string) {
	s.warnings.Warn(warning.Warning{Code: code, Source: "sessionstore", Path: path, Line: line, Message: message})
}

func (s *Store) Create(ctx context.Context, r loop.CreateSessionRequest) (loop.SessionRef, error) {
	if e := ctx.Err(); e != nil {
		return loop.SessionRef{}, e
	}
	if e := s.open(); e != nil {
		return loop.SessionRef{}, e
	}
	if _, _, e := encodeIdentity(r.AppName, r.UserID); e != nil {
		return loop.SessionRef{}, e
	}
	id := r.SessionID
	generated := id == ""
	if generated {
		id = s.newID()
	}
	if _, e := encodeSegment(id); e != nil {
		return loop.SessionRef{}, e
	}
	if generated && !isCanonicalUUIDv4(id) {
		return loop.SessionRef{}, ErrInvalidID
	}
	local, app, user := splitState(cloneMap(r.State))
	all := mergeState(local, app, user)
	h := header{ID: id, AppName: r.AppName, UserID: r.UserID, State: all}
	prepared, e := prepareHeaderRecord(h)
	if e != nil {
		return loop.SessionRef{}, e
	}
	m, e := s.appLock(r.AppName)
	if e != nil {
		return loop.SessionRef{}, e
	}
	m.Lock()
	defer m.Unlock()
	scan, e := s.scanAppLocked(r.AppName)
	if e != nil {
		return loop.SessionRef{}, e
	}
	order, e := s.reserveOrderLocked(r.AppName, scan.MaxOrder)
	if e != nil {
		return loop.SessionRef{}, e
	}
	name, e := s.paths.sessionLog(r.AppName, r.UserID, id)
	if e != nil {
		return loop.SessionRef{}, e
	}
	if e = s.paths.ensureParent(name); e != nil {
		return loop.SessionRef{}, e
	}
	f, e := s.paths.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if errors.Is(e, os.ErrExist) {
		return loop.SessionRef{}, ErrSessionExists
	}
	if e != nil {
		return loop.SessionRef{}, fmt.Errorf("create session log: %w", e)
	}
	data := prepared.bytes(order)
	if _, e = f.Write(data); e == nil && s.fsync {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		_ = s.paths.root.Remove(name)
		if e != nil {
			return loop.SessionRef{}, fmt.Errorf("write session header: %w", e)
		}
		return loop.SessionRef{}, fmt.Errorf("close session header: %w", closeErr)
	}
	log := &sessionLog{name: name, header: cloneHeader(h), sidecars: map[string][]byte{}, stateRecords: []stateRecord{{Order: order, Path: name, Line: 1, UserID: r.UserID, AppDelta: app, UserDelta: user}}}
	scan.Logs[name] = log
	scan.Records = append(scan.Records, log.stateRecords[0])
	scan.MaxOrder = order
	p, e := s.rebuildSharedLocked(r.AppName, r.UserID, scan, false)
	if e != nil {
		s.warn(warning.WarnSessionSnapshotRefresh, name, 0, e.Error())
		p = sharedProjection{App: stateCheckpoint{State: map[string]any{}}, User: stateCheckpoint{State: map[string]any{}}}
	}
	return s.reference(log, r.AppName, r.UserID, p.App.State, p.User.State), nil
}

func (s *Store) Get(ctx context.Context, app, user, id string) (loop.SessionRef, []loop.Event, error) {
	if e := ctx.Err(); e != nil {
		return loop.SessionRef{}, nil, e
	}
	if e := s.open(); e != nil {
		return loop.SessionRef{}, nil, e
	}
	name, e := s.identityPath(app, user, id)
	if e != nil {
		return loop.SessionRef{}, nil, e
	}
	m, e := s.appLock(app)
	if e != nil {
		return loop.SessionRef{}, nil, e
	}
	m.Lock()
	defer m.Unlock()
	scan, e := s.scanAppLocked(app)
	if e != nil {
		return loop.SessionRef{}, nil, e
	}
	log := scan.Logs[name]
	if log == nil {
		return loop.SessionRef{}, nil, ErrSessionNotFound
	}
	p, e := s.rebuildSharedLocked(app, user, scan, false)
	if e != nil {
		s.warn(warning.WarnSessionSnapshotRefresh, name, 0, e.Error())
		p = s.projectShared(app, user, scan)
	}
	events := make([]loop.Event, len(log.events))
	for i := range log.events {
		events[i] = cloneEvent(log.events[i])
	}
	return s.reference(log, app, user, p.App.State, p.User.State), events, nil
}

func (s *Store) List(ctx context.Context, app, user string) ([]loop.SessionRef, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := s.open(); e != nil {
		return nil, e
	}
	if _, e := encodeSegment(app); e != nil {
		return nil, e
	}
	if user != "" {
		if _, e := encodeSegment(user); e != nil {
			return nil, e
		}
	}
	m, e := s.appLock(app)
	if e != nil {
		return nil, e
	}
	m.Lock()
	defer m.Unlock()
	scan, e := s.scanAppLocked(app)
	if e != nil {
		return nil, e
	}
	var refs []loop.SessionRef
	for _, log := range scan.Logs {
		if user != "" && log.header.UserID != user {
			continue
		}
		p, e := s.rebuildSharedLocked(app, log.header.UserID, scan, false)
		if e != nil {
			s.warn(warning.WarnSessionSnapshotRefresh, log.name, 0, e.Error())
			p = s.projectShared(app, log.header.UserID, scan)
		}
		ref := s.reference(log, app, log.header.UserID, p.App.State, p.User.State)
		ref.State = nil
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

func (s *Store) Delete(ctx context.Context, app, user, id string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := s.open(); e != nil {
		return e
	}
	name, e := s.identityPath(app, user, id)
	if e != nil {
		return e
	}
	m, e := s.appLock(app)
	if e != nil {
		return e
	}
	m.Lock()
	defer m.Unlock()
	scan, e := s.scanAppLocked(app)
	if e != nil {
		return e
	}
	if scan.Logs[name] == nil {
		return nil
	}
	if _, e = s.rebuildSharedLocked(app, user, scan, true); e != nil {
		return e
	}
	if e = s.paths.root.Remove(name); e != nil {
		return fmt.Errorf("delete session log: %w", e)
	}
	return nil
}

func (s *Store) Append(ctx context.Context, ref loop.SessionRef, event loop.Event) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if event.ID == "" || event.SessionID != ref.ID || event.Kind == loop.EventTextDelta {
		return ErrInvalidEvent
	}
	prepared, er, e := prepareEventRecord(event)
	if e != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, e)
	}
	if e = s.open(); e != nil {
		return e
	}
	name, e := s.identityPath(ref.AppName, ref.UserID, ref.ID)
	if e != nil {
		return e
	}
	m, e := s.appLock(ref.AppName)
	if e != nil {
		return e
	}
	m.Lock()
	defer m.Unlock()
	scan, e := s.scanAppLocked(ref.AppName)
	if e != nil {
		return e
	}
	log := scan.Logs[name]
	if log == nil {
		return ErrSessionNotFound
	}
	order, e := s.reserveOrderLocked(ref.AppName, scan.MaxOrder)
	if e != nil {
		return e
	}
	if e = log.appendBytes(s.paths, prepared.bytes(order), s.fsync); e != nil {
		return e
	}
	committed, _ := er.event()
	log.events = append(log.events, cloneEvent(committed))
	log.updated = committed.Timestamp
	_, a, u := splitState(committed.StateDelta)
	sr := stateRecord{Order: order, Path: name, Line: len(log.events) + 1, UserID: ref.UserID, AppDelta: a, UserDelta: u}
	log.stateRecords = append(log.stateRecords, sr)
	scan.Records = append(scan.Records, sr)
	scan.MaxOrder = order
	if _, e = s.rebuildSharedLocked(ref.AppName, ref.UserID, scan, false); e != nil {
		s.warn(warning.WarnSessionSnapshotRefresh, name, 0, e.Error())
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.paths.close()
}
func (s *Store) identityPath(app, user, id string) (string, error) {
	if _, _, e := encodeIdentity(app, user); e != nil {
		return "", e
	}
	if _, e := encodeSegment(id); e != nil {
		return "", e
	}
	return s.paths.sessionLog(app, user, id)
}

type appScan struct {
	Logs     map[string]*sessionLog
	Records  []stateRecord
	MaxOrder uint64
}
type sharedProjection struct {
	App  stateCheckpoint
	User stateCheckpoint
}

func (s *Store) scanAppLocked(app string) (appScan, error) {
	encoded, e := encodeSegment(app)
	if e != nil {
		return appScan{}, e
	}
	base := filepath.Join("apps", encoded, "users")
	users, e := readDir(s.paths.root, base)
	if errors.Is(e, os.ErrNotExist) {
		return appScan{Logs: map[string]*sessionLog{}}, nil
	}
	if e != nil {
		return appScan{}, e
	}
	out := appScan{Logs: map[string]*sessionLog{}}
	sort.Slice(users, func(i, j int) bool { return users[i].Name() < users[j].Name() })
	for _, u := range users {
		if !u.IsDir() {
			continue
		}
		uid, e := decodeSegment(u.Name())
		if e != nil {
			continue
		}
		dir := filepath.Join(base, u.Name(), "sessions")
		entries, e := readDir(s.paths.root, dir)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return appScan{}, e
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			sid, e := decodeSegment(entry.Name()[:len(entry.Name())-6])
			if e != nil {
				continue
			}
			name := filepath.Join(dir, entry.Name())
			log, e := loadSessionLog(s.paths, name, app, uid, sid, s)
			if e != nil {
				return appScan{}, e
			}
			out.Logs[name] = log
			out.Records = append(out.Records, log.stateRecords...)
			for _, r := range log.stateRecords {
				if r.Order > out.MaxOrder {
					out.MaxOrder = r.Order
				}
			}
		}
	}
	sort.Slice(out.Records, func(i, j int) bool {
		a, b := out.Records[i], out.Records[j]
		if a.Order == 0 && b.Order > 0 {
			return true
		}
		if b.Order == 0 && a.Order > 0 {
			return false
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	for i := 1; i < len(out.Records); i++ {
		if out.Records[i].Order > 0 && out.Records[i].Order == out.Records[i-1].Order {
			s.warn(warning.WarnSessionOrderDuplicate, out.Records[i].Path, out.Records[i].Line, "duplicate positive record order")
			return appScan{}, fmt.Errorf("%w: duplicate order %d", ErrCorruptLog, out.Records[i].Order)
		}
	}
	return out, nil
}
func (s *Store) reserveOrderLocked(app string, max uint64) (uint64, error) {
	name, e := s.paths.appSequence(app)
	if e != nil {
		return 0, e
	}
	if e = s.paths.ensureParent(name); e != nil {
		return 0, e
	}
	old, _, e := readUint64File(s.paths.root, name)
	if e != nil {
		return 0, e
	}
	if old < max {
		old = max
	}
	if old == math.MaxUint64 {
		return 0, fmt.Errorf("session order exhausted")
	}
	next := old + 1
	if e = writeUint64File(s.paths.root, name, next, s.fsync); e != nil {
		return 0, e
	}
	return next, nil
}
func (s *Store) projectShared(app, user string, scan appScan) sharedProjection {
	a := stateCheckpoint{State: map[string]any{}}
	u := stateCheckpoint{State: map[string]any{}}
	for _, r := range scan.Records {
		maps.Copy(a.State, r.AppDelta)
		if r.UserID == user {
			maps.Copy(u.State, r.UserDelta)
		}
	}
	return sharedProjection{a, u}
}
func (s *Store) rebuildSharedLocked(app, user string, scan appScan, required bool) (sharedProjection, error) {
	an, e := s.paths.appState(app)
	if e != nil {
		return sharedProjection{}, e
	}
	ao, e := s.paths.appStateOrder(app)
	if e != nil {
		return sharedProjection{}, e
	}
	un, e := s.paths.userState(app, user)
	if e != nil {
		return sharedProjection{}, e
	}
	uo, e := s.paths.userStateOrder(app, user)
	if e != nil {
		return sharedProjection{}, e
	}
	a, e := readStateCheckpoint(s.paths.root, an, ao)
	if e != nil {
		return sharedProjection{}, e
	}
	u, e := readStateCheckpoint(s.paths.root, un, uo)
	if e != nil {
		return sharedProjection{}, e
	}
	for _, r := range scan.Records {
		if r.Order == 0 {
			if !a.Exists {
				maps.Copy(a.State, r.AppDelta)
			}
			if r.UserID == user && !u.Exists {
				maps.Copy(u.State, r.UserDelta)
			}
			continue
		}
		if r.Order > a.Through {
			maps.Copy(a.State, r.AppDelta)
		}
		if r.UserID == user && r.Order > u.Through {
			maps.Copy(u.State, r.UserDelta)
		}
	}
	if !a.Exists || !u.Exists {
		for _, r := range scan.Records {
			if r.Order == 0 {
				s.warn(warning.WarnSessionLegacyStateLoss, r.Path, r.Line, "legacy records cannot recover missing Create shared state")
			}
		}
	}
	if scan.MaxOrder > a.Through {
		a.Through = scan.MaxOrder
	}
	if scan.MaxOrder > u.Through {
		u.Through = scan.MaxOrder
	}
	if e = s.paths.ensureParent(an); e != nil {
		return sharedProjection{}, e
	}
	if e = s.paths.ensureParent(un); e != nil {
		return sharedProjection{}, e
	}
	if e = writeStateCheckpoint(s.paths.root, an, ao, a, s.fsync); e != nil {
		return sharedProjection{}, e
	}
	if e = writeStateCheckpoint(s.paths.root, un, uo, u, s.fsync); e != nil {
		return sharedProjection{}, e
	}
	return sharedProjection{a, u}, nil
}
func (s *Store) reference(log *sessionLog, app, user string, a, u map[string]any) loop.SessionRef {
	local, _, _ := splitState(log.header.State)
	for _, event := range log.events {
		x, _, _ := splitState(event.StateDelta)
		maps.Copy(local, x)
	}
	return loop.SessionRef{ID: log.header.ID, AppName: app, UserID: user, State: mergeState(local, a, u), LastUpdate: log.updated}
}
func cloneHeader(v header) header { v.State = cloneMap(v.State); return v }
func cloneMap(v map[string]any) map[string]any {
	if v == nil {
		return nil
	}
	data, e := json.Marshal(v)
	if e != nil {
		return maps.Clone(v)
	}
	var c map[string]any
	if json.Unmarshal(data, &c) != nil {
		return maps.Clone(v)
	}
	return c
}
func cloneEvent(v loop.Event) loop.Event {
	v.StateDelta = cloneMap(v.StateDelta)
	v.Raw = append(v.Raw[:0:0], v.Raw...)
	if v.Tool != nil {
		x := *v.Tool
		x.Args = cloneMap(x.Args)
		x.Content = cloneMap(x.Content)
		v.Tool = &x
	}
	if v.Usage != nil {
		x := *v.Usage
		v.Usage = &x
	}
	if v.Warning != nil {
		x := *v.Warning
		v.Warning = &x
	}
	return v
}
func readDir(root *os.Root, name string) ([]os.DirEntry, error) {
	d, e := root.Open(name)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	return d.ReadDir(-1)
}
func newUUIDv4() string {
	var v [16]byte
	if _, e := rand.Read(v[:]); e != nil {
		return ""
	}
	v[6] = v[6]&0x0f | 0x40
	v[8] = v[8]&0x3f | 0x80
	x := hex.EncodeToString(v[:])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}
func isCanonicalUUIDv4(v string) bool {
	if len(v) != 36 || v[8] != '-' || v[13] != '-' || v[18] != '-' || v[23] != '-' || v[14] != '4' || v[19] != '8' && v[19] != '9' && v[19] != 'a' && v[19] != 'b' {
		return false
	}
	for i := range v {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((v[i] >= '0' && v[i] <= '9') || (v[i] >= 'a' && v[i] <= 'f')) {
			return false
		}
	}
	return true
}
