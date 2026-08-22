package main

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// newLogger builds the process-wide zerolog.Logger using ConsoleWriter — the
// same colored, human-readable format used elsewhere in this fleet (reaparr,
// diun): "TIME | LEVEL | message key=value ...". Level is configurable via
// LOG_LEVEL.
//
// Log call sites are expected to build a complete, readable sentence and
// pass it as the message with no additional fields; ConsoleWriter appends
// any attached fields as trailing key=value pairs automatically.
func newLogger(levelStr string) zerolog.Logger {
	level, err := zerolog.ParseLevel(strings.ToLower(levelStr))
	if err != nil {
		level = zerolog.InfoLevel
	}

	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
		// NoColor deliberately left false (colors forced on) even though
		// stdout is normally a pipe, not a real TTY, in a container —
		// `docker logs` and the log viewers actually used against this
		// stack (Dozzle, Portainer, etc.) render ANSI color codes fine.
	}

	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}
