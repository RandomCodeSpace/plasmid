package workspace

import (
	"context"
	"sync"
	"testing"
)

type touchObserverFunc func(context.Context, Touch)

func (f touchObserverFunc) ObserveTouch(ctx context.Context, touch Touch) { f(ctx, touch) }

func TestTouchBusDeliversInRegistrationOrder(t *testing.T) {
	bus := NewTouchBus()
	var order []int
	bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { order = append(order, 1) }))
	bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { order = append(order, 2) }))
	bus.Publish(context.Background(), Touch{Kind: TouchRead})
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("delivery order = %v, want [1 2]", order)
	}
}

func TestTouchBusUnsubscribeIsIdempotent(t *testing.T) {
	bus := NewTouchBus()
	var order []int
	bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { order = append(order, 1) }))
	unsubscribe := bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { order = append(order, 2) }))
	ctx := context.Background()
	bus.Publish(ctx, Touch{Kind: TouchRead})
	unsubscribe()
	unsubscribe()
	bus.Publish(ctx, Touch{Kind: TouchDelete})
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("delivery order = %v, want [1 2 1]", order)
	}
}

func TestTouchBusForwardsContextAndRecoversObserverPanics(t *testing.T) {
	bus := NewTouchBus()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { panic("observer failure") }))
	var received context.Context
	bus.Subscribe(touchObserverFunc(func(observerContext context.Context, touch Touch) {
		if touch.Kind != TouchEdit {
			t.Errorf("kind = %v, want %v", touch.Kind, TouchEdit)
		}
		received = observerContext
	}))
	bus.Publish(ctx, Touch{Kind: TouchEdit})
	if received != ctx || received.Err() != context.Canceled {
		t.Fatal("context was not forwarded intact")
	}
}

func TestTouchBusConcurrentSafety(t *testing.T) {
	bus := NewTouchBus()
	const workers = 64
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			unsubscribe := bus.Subscribe(touchObserverFunc(func(context.Context, Touch) {}))
			bus.Publish(context.Background(), Touch{Kind: TouchSearch})
			unsubscribe()
		}()
	}
	group.Wait()
}

func TestNormalizeTouchPath(t *testing.T) {
	tests := map[string]string{
		"./src/main.go": "src/main.go",
		`src\main.go`:   "src/main.go",
		"src/nested.go": "src/nested.go",
	}
	for input, want := range tests {
		if got := NormalizeTouchPath(input); got != want {
			t.Fatalf("NormalizeTouchPath(%q) = %q, want %q", input, got, want)
		}
	}
}
