// Package logging builds this service's zap logger so that every log line
// always lands in its own log file, while the terminal only shows output
// during startup — once the service finishes booting, the console stops
// receiving new lines and everything after that point is file-only.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New builds a logger that always writes to file, and additionally echoes to
// stdout until the returned stopConsole func is called. Call stopConsole once
// all startup-phase logging is done — right before the service enters its
// blocking serve/run loop — so steady-state logs only land in the file.
func New(format, level, file string) (logger *zap.Logger, stopConsole func(), err error) {
	if dir := filepath.Dir(file); dir != "." {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, nil, fmt.Errorf("logging: create log dir: %w", mkErr)
		}
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("logging: open log file: %w", err)
	}

	var zLevel zapcore.Level
	if unErr := zLevel.UnmarshalText([]byte(level)); unErr != nil {
		zLevel = zapcore.InfoLevel
	}

	var fileEncoder zapcore.Encoder
	if format == "json" {
		fileEncoder = zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	} else {
		fileEncoder = zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	}
	fileCore := zapcore.NewCore(fileEncoder, zapcore.AddSync(f), zLevel)

	var consoleOn atomic.Bool
	consoleOn.Store(true)
	consoleCore := &toggleCore{
		Core:    zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()), zapcore.AddSync(os.Stdout), zLevel),
		enabled: &consoleOn,
	}

	core := zapcore.NewTee(fileCore, consoleCore)
	logger = zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))

	return logger, func() { consoleOn.Store(false) }, nil
}

// toggleCore wraps a Core so it can be switched off at runtime without
// tearing down the logger — used to stop echoing to the terminal once
// startup is complete while the wrapped file core keeps logging everything.
type toggleCore struct {
	zapcore.Core
	enabled *atomic.Bool
}

func (c *toggleCore) Enabled(lvl zapcore.Level) bool {
	return c.enabled.Load() && c.Core.Enabled(lvl)
}

func (c *toggleCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if !c.enabled.Load() {
		return ce
	}
	return c.Core.Check(e, ce)
}

func (c *toggleCore) With(fields []zapcore.Field) zapcore.Core {
	return &toggleCore{Core: c.Core.With(fields), enabled: c.enabled}
}
