package interruptcontext

import (
	"context"
	"errors"
	"os"
	"os/signal"
)

var (
	Interrupted = errors.New("interrupted")
)

func WithGrafulInterrupt(ctx context.Context) (context.Context, <-chan struct{}) {
	// Setup graceful interrupt channel on os.Interrupt.
	graceCh := make(chan struct{})
	interruptCh := make(chan os.Signal, 1)
	signal.Notify(interruptCh, os.Interrupt)
	go func() {
		<-interruptCh
		close(graceCh)
	}()

	// TODO: maybe cancel context on a second interrupt instead of a kill?

	// Setup context cancelation on os.Kill.
	newCtx, cancel := context.WithCancel(ctx)
	killSig := make(chan os.Signal, 1)
	signal.Notify(killSig, os.Kill)
	go func() {
		<-killSig
		cancel()
	}()

	return newCtx, graceCh
}

func WithInterrupt(ctx context.Context) context.Context {
	// Setup context cancelation on os.Interrupt.
	newCtx, cancel := context.WithCancel(ctx)
	interruptCh := make(chan os.Signal, 1)
	signal.Notify(interruptCh, os.Interrupt)
	go func() {
		<-interruptCh
		cancel()
	}()
	return newCtx
}

func DoGraceful(ctx context.Context, f func()) {
	select {
	case <-ctx.Done():
	default:
		f()
	}
}
