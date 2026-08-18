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
		name string
		ctx  context.Context
		want error
	}{
		{name: "canceled", ctx: canceledContext(), want: context.Canceled},
		{name: "expired", ctx: expiredContext(), want: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			err := queue.Do(test.ctx, func() error { called = true; return nil })
			if !errors.Is(err, test.want) || called {
				t.Fatalf("Do = %v, called=%v", err, called)
			}
		})
	}
	close(release)
}

func TestMutationQueueCancellationRacingSlotReleaseDoesNotRun(t *testing.T) {
	queue := NewMutationQueue()
	queue.sem <- struct{}{}
	waiting := make(chan struct{})
	acquired := make(chan struct{})
	continueAfterCancel := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	called := false
	go func() {
		done <- queue.do(
			ctx,
			func() error {
				called = true
				return nil
			},
			func() error {
				close(waiting)
				return nil
			},
			func() error {
				close(acquired)
				<-continueAfterCancel
				return nil
			},
		)
	}()
	<-waiting
	<-queue.sem
	<-acquired
	cancel()
	close(continueAfterCancel)
	err := <-done
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("Do = %v, called=%v", err, called)
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
