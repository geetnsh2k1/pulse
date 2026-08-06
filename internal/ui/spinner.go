package ui

import (
	"fmt"
	"time"
)

// Wave is pulse's wordmark: a heartbeat line, in amber.
func Wave() []string {
	return []string{
		Accent(" ─╮ ╭─╮ ╭──"),
		Accent("  ╰─╯ ╰─╯"),
	}
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner animates a slow step on a TTY (frame + label + elapsed seconds);
// off-TTY it degrades to the classic "label… done" two-piece print.
type Spinner struct {
	label   string
	started time.Time
	stop    chan struct{}
	done    chan struct{}
}

// StartSpinner begins animating. Nothing else may print until Success/Fail.
func StartSpinner(label string) *Spinner {
	s := &Spinner{label: label, started: time.Now()}
	if !Enabled() {
		fmt.Printf("  %s… ", label)
		return s
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-s.stop:
				fmt.Print("\r\x1b[2K") // clear the spinner line
				return
			case <-tick.C:
				fmt.Printf("\r  %s %s %s", Accent(spinnerFrames[i%len(spinnerFrames)]),
					Dim(s.label+"…"), Dim(fmt.Sprintf("%ds", int(time.Since(s.started).Seconds()))))
			}
		}
	}()
	return s
}

func (s *Spinner) finish(glyph, note string) {
	elapsed := time.Since(s.started).Round(100 * time.Millisecond)
	if s.stop == nil { // plain mode: complete the "label… " line
		fmt.Printf("%s\n", note)
		return
	}
	close(s.stop)
	<-s.done
	fmt.Printf("  %s %s %s\n", glyph, Dim(s.label), Dim(fmt.Sprintf("— %s (%s)", note, elapsed)))
}

// Success ends the spinner with a green check.
func (s *Spinner) Success() { s.finish(OK("✓"), "done") }

// Fail ends the spinner with a note; the caller prints the remedy.
func (s *Spinner) Fail(note string) { s.finish(Err("✗"), note) }
