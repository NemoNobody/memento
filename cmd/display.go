package cmd

import (
	"io"
	"os"
)

// writeLine writes a single line with a trailing newline to w.
// Display paths build strings with strings.Builder + strconv + io.WriteString:
// fmt's formatting uses reflection and allocates far more than it needs to.
func writeLine(w io.Writer, line string) error {
	_, err := io.WriteString(w, line+"\n")
	return err
}

// fatalErr reports a fatal command error to stderr and exits.
func fatalErr(err error) {
	_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
	os.Exit(1)
}
