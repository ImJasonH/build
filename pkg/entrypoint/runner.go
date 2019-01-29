package entrypoint

import (
	"log"
	"os"
	"os/exec"
	"syscall"
)

type Runner interface {
	Run(args ...string)
}

type RealRunner struct{}

var _ Runner = (*RealRunner)(nil)

func (*RealRunner) Run(args ...string) {
	if len(args) == 0 {
		return
	}
	name, args := args[0], args[1:]

	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			// Copied from https://stackoverflow.com/questions/10385551/get-exit-code-go
			// This works on both Unix and Windows. Although
			// package syscall is generally platform dependent,
			// WaitStatus is defined for both Unix and Windows and
			// in both cases has an ExitStatus() method with the
			// same signature.
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				os.Exit(status.ExitStatus())
			}
			log.Fatalf("Error executing command (ExitError): %v", err)
		}
		log.Fatalf("Error executing command: %v", err)
	}
}
