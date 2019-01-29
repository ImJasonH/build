package entrypoint

type fakeWaiter struct{ waited bool }

func (f *fakeWaiter) Wait(string) { f.waited = true }

type fakeRunner struct {
	args []string
}

func (f *fakeRunner) Run(args ...string) {
	f.args = args
}

type fakePostWriter struct{ wrote bool }

func (f *fakePostWriter) Write(string) { f.wrote = true }
