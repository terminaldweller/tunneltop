package shellwords

import (
	"fmt"
	"strings"
	"unicode"
)

// Split parses a small POSIX-shell-like command line into argv.
// It supports whitespace splitting, single quotes, double quotes, and backslash
// escaping. It intentionally does not expand variables, globs, command
// substitutions, or any other shell feature.
func Split(s string) ([]string, error) {
	var args []string
	var b strings.Builder

	const (
		plain = iota
		single
		double
	)

	state := plain
	inArg := false
	escaped := false

	flush := func() {
		args = append(args, b.String())
		b.Reset()
		inArg = false
	}

	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			inArg = true
			escaped = false
			continue
		}

		switch state {
		case plain:
			switch {
			case r == '\\':
				escaped = true
				inArg = true
			case r == '\'':
				state = single
				inArg = true
			case r == '"':
				state = double
				inArg = true
			case unicode.IsSpace(r):
				if inArg {
					flush()
				}
			default:
				b.WriteRune(r)
				inArg = true
			}

		case single:
			if r == '\'' {
				state = plain
			} else {
				b.WriteRune(r)
			}

		case double:
			switch r {
			case '\\':
				escaped = true
			case '"':
				state = plain
			default:
				b.WriteRune(r)
			}
		}
	}

	if escaped {
		return nil, fmt.Errorf("trailing backslash")
	}
	if state == single {
		return nil, fmt.Errorf("unterminated single quote")
	}
	if state == double {
		return nil, fmt.Errorf("unterminated double quote")
	}
	if inArg {
		flush()
	}

	return args, nil
}
