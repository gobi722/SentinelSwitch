// Package logging builds this service's zap logger so that every log line
// always lands in an hourly-rotated file tree (baseDir/YYYY/MM/DD/HH.log),
// while the terminal only shows output during startup — once the service
// finishes booting, the console stops receiving new lines and everything
// after that point is file-only.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New builds a logger that always writes to the rotating file tree rooted at
// baseDir, and additionally echoes to stdout until the returned stopConsole
// func is called. Call stopConsole once all startup-phase logging is done —
// right before the service enters its blocking serve/run loop — so
// steady-state logs only land in the file.
func New(format, level, baseDir string) (logger *zap.Logger, stopConsole func(), err error) {
	rotator, err := newRotatingWriter(baseDir)
	if err != nil {
		return nil, nil, err
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
	fileCore := zapcore.NewCore(fileEncoder, zapcore.AddSync(rotator), zLevel)

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

// rotatingWriter writes to baseDir/YYYY/MM/DD/HH.log, transparently opening
// a new file (creating year/month/day directories as needed) whenever the
// wall-clock hour changes. Safe for concurrent use — zap's interceptors and
// pipeline workers all write through the same logger from multiple
// goroutines.
type rotatingWriter struct {
	mu      sync.Mutex
	baseDir string
	bucket  string // "YYYY/MM/DD/HH" of the currently open file
	file    *os.File
}

func newRotatingWriter(baseDir string) (*rotatingWriter, error) {
	w := &rotatingWriter{baseDir: baseDir}
	if err := w.rotateLocked(time.Now()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if hourBucket(now) != w.bucket {
		if err := w.rotateLocked(now); err != nil {
			return 0, err
		}
	}
	return w.file.Write(p)
}

// rotateLocked opens the file for now's hour bucket, creating
// baseDir/YYYY/MM/DD/ if needed, and closes whatever was open before.
// Caller must hold w.mu.
func (w *rotatingWriter) rotateLocked(now time.Time) error {
	dir := filepath.Join(w.baseDir, now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("logging: create log dir: %w", err)
	}
	path := filepath.Join(dir, now.Format("15")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logging: open log file: %w", err)
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = f
	w.bucket = hourBucket(now)
	return nil
}

func hourBucket(t time.Time) string {
	return t.Format("2006/01/02/15")
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
