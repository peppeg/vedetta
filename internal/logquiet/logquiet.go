// Package logquiet is the "Registro con meno dettagli" switch (map R7): the log
// keeps what Vedetta did (steps, counts, times, errors) and drops what was searched
// and read: every quoted text (searches, names, the words of an error) and every
// web address.
package logquiet

import (
	"io"
	"log"
	"os"
	"regexp"
	"sync/atomic"
)

var on atomic.Bool

// Set turns the quiet log on or off, at once.
func Set(v bool) { on.Store(v) }

// On reports whether the log is quiet.
func On() bool { return on.Load() }

var (
	address  = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	urlField = regexp.MustCompile(`\burl=\S+`)
	quoted   = regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"`)
	// the name of a site in an error of the network: "lookup x: no such host", "host x resolves…"
	hostName = regexp.MustCompile(`\b(lookup|host|resolve host) ((?:[A-Za-z0-9_-]+\.)+[A-Za-z]{2,})`)
)

// Redact takes the addresses and the quoted texts out of a log line.
func Redact(s string) string {
	s = address.ReplaceAllString(s, "[address]")
	s = urlField.ReplaceAllString(s, "url=[address]")
	s = hostName.ReplaceAllString(s, "$1 [site]")
	return quoted.ReplaceAllString(s, `"…"`)
}

type writer struct{ w io.Writer }

func (w writer) Write(p []byte) (int, error) {
	if !on.Load() {
		return w.w.Write(p)
	}
	if _, err := w.w.Write([]byte(Redact(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Install puts the filter in front of the standard log (stderr).
func Install() { log.SetOutput(writer{os.Stderr}) }
