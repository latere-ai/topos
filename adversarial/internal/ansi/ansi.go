// Package ansi holds the minimal set of ANSI escape codes shared by the
// terminal-output packages (round progress lines and the rendered
// summary). It is a leaf package with no internal imports so any
// presentation package can depend on it without risking an import cycle.
package ansi

// SGR escape codes. Kept deliberately small: only the colors and
// attributes the output packages actually use.
const (
	Reset   = "\x1b[0m"
	Bold    = "\x1b[1m"
	Dim     = "\x1b[2m"
	Cyan    = "\x1b[36m"
	Magenta = "\x1b[35m"
	Green   = "\x1b[32m"
	Red     = "\x1b[31m"
	Yellow  = "\x1b[33m"
)
