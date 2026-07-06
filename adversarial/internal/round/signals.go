package round

import (
	"context"
	"os"
	"os/signal"
)

// InstallHandler returns a context that cancels on the named signals,
// plus a finalize function the caller defers. A second signal during
// finalize triggers os.Exit(130).
func InstallHandler(ctx context.Context, sigs ...os.Signal) (context.Context, func()) {
	if len(sigs) == 0 {
		sigs = []os.Signal{os.Interrupt}
	}
	derived, cancel := context.WithCancel(ctx)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, sigs...)
	doubleCh := make(chan struct{})
	go func() {
		<-ch
		cancel()
		select {
		case <-ch:
			os.Exit(130)
		case <-doubleCh:
		}
	}()
	return derived, func() {
		signal.Stop(ch)
		close(doubleCh)
		cancel()
	}
}
