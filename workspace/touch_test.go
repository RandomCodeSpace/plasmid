package workspace

import (
	"context"
	"sync"
	"testing"
)

type touchObserverFunc func(context.Context, Touch)

func (f touchObserverFunc) ObserveTouch(ctx context.Context, touch Touch) { f(ctx, touch) }

func TestTouchBusBehavior(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name     string
		ctx      context.Context
		touch    Touch
		exercise func(*TouchBus, context.Context, Touch, *[]int, *context.Context)
		want     []int
		wantCtx  context.Context
	}{
		{
			name:  "synchronous registration order",
			ctx:   context.Background(),
			touch: Touch{Kind: TouchRead},
			exercise: func(bus *TouchBus, ctx context.Context, touch Touch, order *[]int, _ *context.Context) {
				bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { *order = append(*order, 1) }))
				bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { *order = append(*order, 2) }))
				bus.Publish(ctx, touch)
			},
			want: []int{1, 2},
		},
		{
			name:  "idempotent unsubscribe",
			ctx:   context.Background(),
			touch: Touch{Kind: TouchDelete},
			exercise: func(bus *TouchBus, ctx context.Context, touch Touch, order *[]int, _ *context.Context) {
				bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { *order = append(*order, 1) }))
				unsubscribe := bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { *order = append(*order, 2) }))
				bus.Publish(context.Background(), Touch{Kind: TouchRead})
				unsubscribe()
				unsubscribe()
				bus.Publish(ctx, touch)
			},
			want: []int{1, 2, 1},
		},
		{
			name:  "context forwarding and panic isolation",
			ctx:   canceled,
			touch: Touch{Kind: TouchEdit},
			exercise: func(bus *TouchBus, ctx context.Context, touch Touch, order *[]int, received *context.Context) {
				bus.Subscribe(touchObserverFunc(func(context.Context, Touch) { panic("observer failure") }))
				bus.Subscribe(touchObserverFunc(func(ctx context.Context, touch Touch) {
					if touch.Kind != TouchEdit {
						t.Errorf("kind = %v", touch.Kind)
					}
					*order = append(*order, 1)
					*received = ctx
				}))
				bus.Publish(ctx, touch)
			},
			want:    []int{1},
			wantCtx: canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bus := NewTouchBus()
			var order []int
			var received context.Context
			test.exercise(bus, test.ctx, test.touch, &order, &received)
			if len(order) != len(test.want) {
				t.Fatalf("delivery order = %v, want %v", order, test.want)
			}
			for index := range test.want {
				if order[index] != test.want[index] {
					t.Fatalf("delivery order = %v, want %v", order, test.want)
				}
			}
			if test.wantCtx != nil && (received != test.wantCtx || received.Err() != context.Canceled) {
				t.Fatalf("context was not forwarded intact")
			}
		})
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
