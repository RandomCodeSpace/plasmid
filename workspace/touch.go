package workspace

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// NormalizeTouchPath returns the slash-separated workspace-relative form used
// by every lazy activation observer.
func NormalizeTouchPath(path string) string {
	return strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./")
}

// TouchKind identifies the workspace operation that touched a path.
type TouchKind int

const (
	TouchRead TouchKind = iota
	TouchWrite
	TouchEdit
	TouchList
	TouchSearch
	TouchDelete
)

// Touch describes a workspace operation. Content is non-nil only for writes
// and edits. InvocationID is empty outside a native tool invocation.
type Touch struct {
	SessionID    string
	InvocationID string
	Path         string
	Kind         TouchKind
	Content      []byte
	Version      int64
	At           time.Time
}

// TouchObserver receives synchronous workspace touch events.
type TouchObserver interface {
	ObserveTouch(context.Context, Touch)
}

type touchSubscription struct {
	id       uint64
	observer TouchObserver
}

// TouchBus fans out touch events in registration order.
type TouchBus struct {
	mu             sync.RWMutex
	subscribers    []touchSubscription
	nextSubscriber uint64
}

// NewTouchBus creates an empty touch bus.
func NewTouchBus() *TouchBus { return &TouchBus{} }

// Subscribe adds an observer and returns an idempotent unsubscribe function.
func (b *TouchBus) Subscribe(observer TouchObserver) func() {
	b.mu.Lock()
	b.nextSubscriber++
	id := b.nextSubscriber
	b.subscribers = append(b.subscribers, touchSubscription{id: id, observer: observer})
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			for index, subscriber := range b.subscribers {
				if subscriber.id == id {
					b.subscribers = append(b.subscribers[:index], b.subscribers[index+1:]...)
					return
				}
			}
		})
	}
}

// Publish synchronously notifies a snapshot of subscribers in registration order.
func (b *TouchBus) Publish(ctx context.Context, touch Touch) {
	b.mu.RLock()
	subscribers := append([]touchSubscription(nil), b.subscribers...)
	b.mu.RUnlock()
	for _, subscriber := range subscribers {
		func(observer TouchObserver) {
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.Default().Error("workspace touch observer panicked", "panic", recovered)
				}
			}()
			observer.ObserveTouch(ctx, touch)
		}(subscriber.observer)
	}
}
