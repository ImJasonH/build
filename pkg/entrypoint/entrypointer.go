package entrypoint

type Entrypointer struct {
	Entrypoint, WaitFile, PostFile string
	Args                           []string

	Runner     Runner
	Waiter     Waiter
	PostWriter PostWriter
}

func (e Entrypointer) Go() {
	if e.WaitFile != "" {
		e.Waiter.Wait(e.WaitFile)
	}

	if e.Entrypoint != "" {
		e.Args = append([]string{e.Entrypoint}, e.Args...)
	}
	e.Runner.Run(e.Args...)

	if e.PostFile != "" {
		e.PostWriter.Write(e.PostFile)
	}
}
