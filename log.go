package main

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// newLogger builds the process-wide zerolog.Logger using ConsoleWriter — the
// same colored, human-readable format reaparr and diun use in this stack
// ("TIME | LEVEL | message key=value ..."). Colors are deliberately left on
// even though stdout is normally a pipe in a container: `docker logs` and the
// log viewers used against this stack render ANSI codes fine.
//
// Log call sites build a complete, readable sentence via fmt.Sprintf and pass
// it as the message; any attached fields are trailing context, not the
// primary way information is conveyed.
func newLogger(levelStr string) zerolog.Logger {
	level, err := zerolog.ParseLevel(strings.ToLower(levelStr))
	if err != nil || levelStr == "" {
		level = zerolog.InfoLevel
	}

	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
	}

	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}
