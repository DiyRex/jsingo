package main

import (
	"fmt"
	"os"
)

// logger writes human-readable progress to stderr.
//
// stdout is left clean so a command's actual output can be piped. Colour is
// used only when stderr is a terminal.
type logger struct{ colour bool }

func newLogger() *logger {
	fi, err := os.Stderr.Stat()
	return &logger{colour: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

func (l *logger) step(format string, args ...any) {
	l.emit("36", "==>", format, args...)
}

func (l *logger) ok(format string, args ...any) {
	l.emit("32", "  ok", format, args...)
}

func (l *logger) warn(format string, args ...any) {
	l.emit("33", "warn", format, args...)
}

func (l *logger) error(format string, args ...any) {
	l.emit("31", " err", format, args...)
}

func (l *logger) info(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "      %s\n", fmt.Sprintf(format, args...))
}

func (l *logger) emit(colour, prefix, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if l.colour {
		fmt.Fprintf(os.Stderr, "\033[%sm%s\033[0m %s\n", colour, prefix, msg)
		return
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", prefix, msg)
}
