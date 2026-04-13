/*
 * stopper — a simple terminal stopwatch with time tracking
 *
 * Keys:
 *   space   record lap
 *   i / I   record lap + open note prompt (appended to log file)
 *   q / Q   quit
 *   Ctrl+C  quit
 *
 * Flags:
 *   -o <file>   log file (default: stopper.log)
 *   -u <user>   username (default: $USER or "unknown")
 *   -v          print version and exit
 *
 * Log format (tab-separated):
 *   # session 2026-04-10T14:32:07+03:00  user toomas  go1.22.1 commit:a3f9c12
 *   2026-04-10T14:32:07+03:00\tlap\t0h14m32.0s
 *   2026-04-10T14:45:11+03:00\tnote\t1h45m11.0s\tstarting deep work block
 *   # end 2026-04-10T16:02:44+03:00  total 1h30m37.0s
 *
 * Works on Linux and macOS. Requires golang.org/x/term.
 */

/*
MIT License
Copyright (c) 2018 - 2026 eix-1eq0
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:
The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.
THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// ----------------------------------------------------------------------------
// Version
// ----------------------------------------------------------------------------

func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "stopper version (unknown build info)"
	}

	goVersion := info.GoVersion
	commit := "(devel)"
	buildTime := "(devel)"

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 7 {
				commit = s.Value[:7]
			} else {
				commit = s.Value
			}
		case "vcs.time":
			buildTime = s.Value
		}
	}

	return fmt.Sprintf("stopper\n  go:      %s\n  commit:  %s\n  built:   %s",
		goVersion, commit, buildTime)
}

// ----------------------------------------------------------------------------
// Time formatting
// ----------------------------------------------------------------------------

// formattedTime formats a duration given in deciseconds into a human-readable
// string. Omits leading zero components, e.g. "3m7.2s" instead of "0h3m7.2s".
func formattedTime(deciseconds int64) string {
	d := deciseconds / 864000
	h := (deciseconds / 36000) % 24
	m := (deciseconds / 600) % 60
	s := (deciseconds / 10) % 60
	ds := deciseconds % 10

	switch {
	case d > 0:
		return fmt.Sprintf("%dd%dh%dm%d.%ds", d, h, m, s, ds)
	case h > 0:
		return fmt.Sprintf("%dh%dm%d.%ds", h, m, s, ds)
	case m > 0:
		return fmt.Sprintf("%dm%d.%ds", m, s, ds)
	default:
		return fmt.Sprintf("%d.%ds", s, ds)
	}
}

// ----------------------------------------------------------------------------
// Terminal helpers
// ----------------------------------------------------------------------------

func stty(args ...string) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	cmd.Run()
}

func setRaw() {
	stty("cbreak", "min", "1")
	stty("-echo")
}

func setNormal() {
	stty("sane")
}

// ----------------------------------------------------------------------------
// Log file
// ----------------------------------------------------------------------------

type logger struct {
	mu   sync.Mutex
	file *os.File
	user string
}

func openLogger(path, user string, startTime time.Time, buildInfo string) (*logger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	l := &logger{file: f, user: user}
	l.writeRaw(fmt.Sprintf("# session %s  user %s  %s\n",
		startTime.Format(time.RFC3339), user, buildInfo))
	return l, nil
}

func (l *logger) writeRaw(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.file.WriteString(line)
}

func (l *logger) writeLap(ts time.Time, elapsed int64) {
	l.writeRaw(fmt.Sprintf("%s\tlap\t%s\n",
		ts.Format(time.RFC3339), formattedTime(elapsed)))
}

func (l *logger) writeNote(ts time.Time, elapsed int64, note string) {
	l.writeRaw(fmt.Sprintf("%s\tnote\t%s\t%s\n",
		ts.Format(time.RFC3339), formattedTime(elapsed), note))
}

func (l *logger) writeEnd(ts time.Time, total int64) {
	l.writeRaw(fmt.Sprintf("# end %s  total %s\n",
		ts.Format(time.RFC3339), formattedTime(total)))
}

func (l *logger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.file.Close()
}

// ----------------------------------------------------------------------------
// Note input using golang.org/x/term
// ----------------------------------------------------------------------------

// readNote suspends raw mode, prompts for a note, and returns the text.
// Returns ("", false) if the user cancelled with ESC or empty input.
func readNote(prompt string) (string, bool) {
	// Restore normal terminal for line editing
	setNormal()
	defer setRaw()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		// Fall back to simple cooked-mode read if term.MakeRaw fails
		fmt.Print(prompt)
		var line string
		fmt.Scanln(&line)
		line = strings.TrimSpace(line)
		if line == "" {
			return "", false
		}
		return line, true
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	t := term.NewTerminal(os.Stdin, prompt)
	line, err := t.ReadLine()
	if err != nil {
		// EOF or ESC-like termination
		return "", false
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	return line, true
}

// ----------------------------------------------------------------------------
// Main
// ----------------------------------------------------------------------------

func main() {
	// --- Flags ---
	outFile := flag.String("o", "stopper.log", "log file path")
	userName := flag.String("u", "", "username (default: $USER)")
	showVer := flag.Bool("v", false, "print version and exit")
	flag.Parse()

	if *showVer {
		fmt.Println(versionString())
		os.Exit(0)
	}

	user := *userName
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		user = "unknown"
	}

	// Build a short build-info string for the session line
	buildInfo := "devel"
	if info, ok := debug.ReadBuildInfo(); ok {
		buildInfo = info.GoVersion
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				buildInfo += " commit:" + s.Value[:7]
			}
		}
	}

	// --- Open log file ---
	startTime := time.Now()
	log, err := openLogger(*outFile, user, startTime, buildInfo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stopper: cannot open log file %q: %v\n", *outFile, err)
		os.Exit(1)
	}
	defer log.close()

	// --- Terminal setup ---
	setRaw()

	// paused gates ticker output during note entry
	var pauseMu sync.Mutex
	paused := false

	setPaused := func(v bool) {
		pauseMu.Lock()
		paused = v
		pauseMu.Unlock()
	}
	isPaused := func() bool {
		pauseMu.Lock()
		defer pauseMu.Unlock()
		return paused
	}

	// cleanup runs on any exit path
	total := func() int64 {
		//return time.Since(startTime).Milliseconds() / 100
		return time.Now().Round(0).Sub(startTime.Round(0)).Milliseconds() / 100
	}
	cleanup := func() {
		log.writeEnd(time.Now(), total())
		setNormal()
	}

	// --- Signal handler (Ctrl+C, SIGTERM) ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		fmt.Println()
		os.Exit(0)
	}()

	// --- Shared lap state ---
	var lapMu sync.Mutex
	var last int64 // deciseconds at last lap

	getLast := func() int64 {
		lapMu.Lock()
		defer lapMu.Unlock()
		return last
	}
	setLast := func(v int64) {
		lapMu.Lock()
		last = v
		lapMu.Unlock()
	}

	// lapCount is only touched from the input goroutine, no mutex needed
	lapCount := 0

	// recordLap prints and logs a lap; called from input goroutine only.
	recordLap := func(ts time.Time, now int64) {
		lapCount++
		lap := now - getLast()
		fmt.Printf("%3d  Total: %-12s  Lap: %-12s  %s\n",
			lapCount,
			formattedTime(now),
			formattedTime(lap),
			ts.Format("2006-01-02 15:04:05"),
		)
		log.writeLap(ts, now)
		setLast(now)
	}

	// --- Input goroutine ---
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				continue
			}
			switch buf[0] {
			case ' ':
				setPaused(true)
				fmt.Print("\r\033[K") //Clear the current runtime line first
				ts := time.Now()
				now := total()
				recordLap(ts, now)
				setPaused(false)

			case 'i', 'I':
				// Pause the ticker display while editing
				setPaused(true)
				fmt.Print("\r\033[K") //Clear the current runtime line first
				ts := time.Now()
				now := total()
				recordLap(ts, now)
				note, ok := readNote("Note: ")
				if ok {
					log.writeNote(ts, now, note)
					fmt.Printf("  ✓ noted\n")
				} else {
					fmt.Printf("  (note discarded)\n")
				}
				setPaused(false)

			case 'q', 'Q':
				cleanup()
				fmt.Println()
				os.Exit(0)
			}
		}
	}()

	// --- Ticker: live display ---
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	const clearLine = "\r\033[K"
	for range ticker.C {
		if isPaused() {
			continue
		}
		now := total()
		fmt.Printf("%sRuntime: %-12s  Lap: %s",
			clearLine,
			formattedTime(now),
			formattedTime(now-getLast()),
		)
	}
}
