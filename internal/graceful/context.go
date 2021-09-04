package graceful

import (
	"context"
	"errors"
	"os"
	"os/signal"
)

var (
	Interrupted = errors.New("interrupted")
)

func WithInterrupt(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go func() {
		<-interrupt
		cancel()
	}()

	return ctx, cancel
}

func WithKill(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	kill := make(chan os.Signal, 1)
	signal.Notify(kill, os.Kill)
	go func() {
		<-kill
		cancel()
	}()

	return ctx, cancel
}

func Do(ctx context.Context, f func()) {
	select {
	case <-ctx.Done():
	default:
		f()
	}
}
