package entrypoint

import (
	"log"
	"os"
	"time"
)

type Waiter interface {
	Wait(file string)
}

type RealWaiter struct{ waitFile string }

var _ Waiter = (*RealWaiter)(nil)

func (*RealWaiter) Wait(file string) {
	if file == "" {
		return
	}
	for ; ; time.Sleep(time.Second) {
		if _, err := os.Stat(file); err == nil {
			return
		} else if !os.IsNotExist(err) {
			log.Fatalf("Waiting for %q: %v", file, err)
		}
	}
}
