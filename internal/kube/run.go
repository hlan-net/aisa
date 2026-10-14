package kube

import (
	"context"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Pass is one kind of object that the loop keeps: Run makes a pass, Done gets its result.
type Pass struct {
	Name string
	Run  func(ctx context.Context) error
	Done func(err error)
}

// RunOptions configure Run.
type RunOptions struct {
	// Namespace holds the Lease, and Identity names this replica in it (the pod's name).
	Namespace, Identity, Lease string
	// Resync is the interval of the passes.
	Resync time.Duration
	// Trigger starts a pass at once, such as after a change of the consumers' declarations.
	Trigger <-chan struct{}
	// OnLeader is called with true when this replica starts to write, and with false when it
	// has stopped: after its last pass has returned, so that nothing the passes report comes
	// after it.
	OnLeader func(bool)
}

// Run takes part in the election of the replica that writes the objects, and makes the passes
// while it is the one, until ctx is done. Only one replica writes: two would issue a key each.
//
// client-go starts the work of a leader in a goroutine of its own and does not wait for it when
// it stops leading, so Run does not let it release the Lease: it waits for the passes to return,
// and only then releases the Lease itself. Until then another replica cannot take the Lease
// before it expires, and the passes, whose context is cancelled, end within moments.
func Run(ctx context.Context, k8s kubernetes.Interface, log *slog.Logger, opts RunOptions, passes ...Pass) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: opts.Lease, Namespace: opts.Namespace},
		Client:     k8s.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: opts.Identity},
	}
	leader := func(on bool) {
		if opts.OnLeader != nil {
			opts.OnLeader(on)
		}
	}
	for ctx.Err() == nil {
		begun, done := make(chan struct{}), make(chan struct{})
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:          lock,
			LeaseDuration: 30 * time.Second,
			RenewDeadline: 20 * time.Second,
			RetryPeriod:   5 * time.Second,
			Name:          opts.Lease,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(lctx context.Context) {
					close(begun)
					defer close(done)
					log.Info("this replica writes the Kubernetes objects", "lease", opts.Lease)
					leader(true)
					Loop(lctx, log, opts.Resync, opts.Trigger, passes...)
					leader(false)
				},
				OnStoppedLeading: func() {
					log.Info("this replica no longer writes the Kubernetes objects", "lease", opts.Lease)
				},
			},
		})
		if err != nil {
			// A configuration error: nothing to retry.
			log.Error("leader election cannot start; the Kubernetes objects are not written", "error", err)
			return
		}
		elector.Run(ctx)
		select {
		case <-begun:
			<-done
			if ctx.Err() != nil {
				release(lock, opts.Identity, log)
			}
		default:
			// Not elected; or elected and stopped before the work began, which then returns at
			// once, its context being cancelled.
		}
	}
}

// release gives up the Lease at shutdown, once this replica's passes have returned, so that
// another replica takes over without waiting for it to expire. It does what client-go's
// ReleaseOnCancel does, at the time that is safe.
func release(lock resourcelock.Interface, identity string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, _, err := lock.Get(ctx)
	if err != nil || rec.HolderIdentity != identity {
		return
	}
	now := metav1.NewTime(time.Now())
	if err := lock.Update(ctx, resourcelock.LeaderElectionRecord{
		LeaderTransitions:    rec.LeaderTransitions,
		LeaseDurationSeconds: 1,
		RenewTime:            now,
		AcquireTime:          now,
	}); err != nil {
		log.Warn("releasing the Lease failed; another replica takes over when it expires", "error", err)
	}
}

// Loop makes the passes at once, then every resync and on every trigger, until ctx is done.
func Loop(ctx context.Context, log *slog.Logger, resync time.Duration, trigger <-chan struct{}, passes ...Pass) {
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	for {
		for _, p := range passes {
			// Checked before each pass: a pass never starts once this replica stops leading.
			if ctx.Err() != nil {
				return
			}
			err := p.Run(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Error("a pass over the Kubernetes objects failed; the next one tries again", "objects", p.Name, "error", err)
			}
			if p.Done != nil {
				p.Done(err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-trigger:
		}
	}
}
