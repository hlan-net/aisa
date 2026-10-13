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
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			ReleaseOnCancel: true,
			LeaseDuration:   30 * time.Second,
			RenewDeadline:   20 * time.Second,
			RetryPeriod:     5 * time.Second,
			Name:            opts.Lease,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(lctx context.Context) {
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
	}
}

// Loop makes the passes at once, then every resync and on every trigger, until ctx is done.
func Loop(ctx context.Context, log *slog.Logger, resync time.Duration, trigger <-chan struct{}, passes ...Pass) {
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	for {
		for _, p := range passes {
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
