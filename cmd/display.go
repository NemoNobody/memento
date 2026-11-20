package cmd

import (
	"errors"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/NemoNobody/velruna/internal/client"
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

// displaySafe renders agent- or server-supplied text on a human terminal
// without letting invisible runes forge the output (the fetched-page path
// strips the same classes at extract time; stored fact text can carry
// them because `add` accepts arbitrary values). ESC/C0/C1 and zero-width
// or bidi runes are dropped; newlines and tabs survive when keepNewline,
// because a multi-line value is deliberate layout.
func displaySafe(s string, keepNewline bool) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			if keepNewline {
				return r
			}
			return -1
		case unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}
