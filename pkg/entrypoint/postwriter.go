package entrypoint

import (
	"log"
	"os"
)

type PostWriter interface {
	Write(file string)
}

type RealPostWriter struct{}

var _ PostWriter = (*RealPostWriter)(nil)

func (*RealPostWriter) Write(file string) {
	if file == "" {
		return
	}
	if _, err := os.Create(file); err != nil {
		log.Fatalf("Creating %q: %v", file, err)
	}
}
