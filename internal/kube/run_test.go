package kube

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestLoopRunsAtOnceAndOnATrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trigger := make(chan struct{}, 1)
	var runs atomic.Int32
	results := make(chan error, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Loop(ctx, slog.New(slog.DiscardHandler), time.Hour, trigger, Pass{
			Name: "test",
			Run: func(context.Context) error {
				if runs.Add(1) == 1 {
					return errors.New("first pass fails")
				}
				return nil
			},
			Done: func(err error) { results <- err },
		})
	}()

	if err := <-results; err == nil {
		t.Fatal("the first pass's error was not passed to Done")
	}
	trigger <- struct{}{}
	if err := <-results; err != nil {
		t.Fatalf("second pass: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return when its context was cancelled")
	}
}

func TestRunElectsThisReplicaAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	k8s := fake.NewClientset()
	var mu sync.Mutex
	var leader []bool
	passed := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, k8s, slog.New(slog.DiscardHandler), RunOptions{
			Namespace: "aisa", Identity: "aisa-0", Lease: "aisa-leader", Resync: time.Hour,
			OnLeader: func(on bool) { mu.Lock(); leader = append(leader, on); mu.Unlock() },
		}, Pass{Name: "test", Run: func(context.Context) error {
			select {
			case passed <- struct{}{}:
			default:
			}
			return nil
		}})
	}()
	select {
	case <-passed:
	case <-time.After(10 * time.Second):
		t.Fatal("no pass: this replica was not elected")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leader) != 2 || !leader[0] || leader[1] {
		t.Errorf("OnLeader calls = %v, want true then false", leader)
	}
}
