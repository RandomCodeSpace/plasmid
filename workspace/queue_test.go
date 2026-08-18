package workspace

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMutationQueueSerializesConcurrentMutations(t *testing.T) {
	queue := NewMutationQueue()
	const mutations = 64
	var active, maximum, completed int32
	var group sync.WaitGroup
	for range mutations {
		group.Add(1)
		go func() {
			defer group.Done()
			err := queue.Do(context.Background(), func() error {
				current := atomic.AddInt32(&active, 1)
				for {
					previous := atomic.LoadInt32(&maximum)
					if current <= previous || atomic.CompareAndSwapInt32(&maximum, previous, current) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&completed, 1)
				atomic.AddInt32(&active, -1)
				return nil
			})
			if err != nil {
				t.Errorf("Do error = %v", err)
			}
		}()
	}
	group.Wait()
	if maximum != 1 || completed != mutations {
		t.Fatalf("maximum=%d completed=%d", maximum, completed)
	}
}

func TestMutationQueueCanceledWaitersDoNotRun(t *testing.T) {
	queue := NewMutationQueue()
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = queue.Do(context.Background(), func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	tests := []struct {
		name    string
		context func() context.Context
		want    error
	}{
		{name: "canceled", context: canceledContext, want: context.Canceled},
		{name: "expired", context: expiredContext, want: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			err := queue.Do(test.context(), func() error { called = true; return nil })
			if !errors.Is(err, test.want) || called {
				t.Fatalf("Do = %v, called=%v", err, called)
			}
		})
	}
	close(release)
}

func TestMutationQueueCancellationRacingSlotReleaseDoesNotRun(t *testing.T) {
	for range 20 {
		queue := NewMutationQueue()
		release := make(chan struct{})
		started := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- queue.Do(context.Background(), func() error {
				close(started)
				<-release
				return nil
			})
		}()
		<-started

		ctx, cancel := context.WithCancel(context.Background())
		waiterDone := make(chan error, 1)
		called := make(chan struct{}, 1)
		go func() {
			waiterDone <- queue.Do(ctx, func() error {
				called <- struct{}{}
				return nil
			})
		}()
		cancel()
		close(release)
		if err := <-waiterDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("Do error = %v, want cancellation", err)
		}
		if err := <-holderDone; err != nil {
			t.Fatalf("holder Do error = %v", err)
		}
		select {
		case <-called:
			t.Fatal("canceled waiter ran")
		default:
		}
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func expiredContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	return ctx
}
