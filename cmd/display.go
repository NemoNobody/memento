package cmd

import (
	"errors"
	"io"
	"os"

	"github.com/NemoNobody/memento/internal/client"
)

// writeLine writes a single line with a trailing newline to w.
// Display paths build strings with strings.Builder + strconv + io.WriteString:
// fmt's formatting uses reflection and allocates far more than it needs to.
func writeLine(w io.Writer, line string) error {
	_, err := io.WriteString(w, line+"\n")
	return err
}

// fatalErr reports a fatal command error to stderr and exits (1).
func fatalErr(err error) {
	_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
	os.Exit(1)
}

// fatalNotFound distinguishes "the key you named does not exist" (exit 3)
// from every other failure (exit 1): scripts can tell a missing fact from
// an unreachable store without parsing stderr.
func fatalNotFound(err error) {
	_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
	os.Exit(3)
}

// fatalIfNotFound routes a store error by kind: a missing key is exit 3,
// anything else stays the generic exit 1.
func fatalIfNotFound(err error) {
	if errors.Is(err, client.ErrNotFound) {
		fatalNotFound(err)
	}
	fatalErr(err)
}
