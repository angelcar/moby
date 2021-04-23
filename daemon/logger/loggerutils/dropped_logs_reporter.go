package loggerutils

import (
	"sync/atomic"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/sirupsen/logrus"
)

var _ logger.Logger = &droppedLogsReporter{}
var _ logger.LogReader = &droppedLogsReporterWithReader{}

// WithDroppedLogsReporter wraps the passed in logger with a logger that counts, and periodically reports errors
// returned by the source logger.
// The goal of this wrapper is to avoid potential saturation of the daemon logs when the source logger repeatedly
// errors out (e.g. max-buffer-size reached, failure to write to disk or network, etc.).
func WithDroppedLogsReporter(src logger.Logger, info logger.Info) logger.Logger {
	r := &droppedLogsReporter{
		src:  src,
		info: info,
		done: make(chan struct{}),
	}
	go r.startReporter()
	if _, ok := src.(logger.LogReader); ok {
		return &droppedLogsReporterWithReader{r}
	}
	return r
}

// droppedLogsReporter periodically reports container logs dropped by the source logger (i.e. errors returned by
// src.Log()).
type droppedLogsReporter struct {
	src           logger.Logger
	info          logger.Info
	loggingErrors int32
	done          chan struct{}
}

func (r *droppedLogsReporter) startReporter() {
	ticker := newTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.reportLoggingErrors()
		case <-r.done:
			r.reportLoggingErrors()
			return
		}
	}
}

func (r *droppedLogsReporter) reportLoggingErrors() {
	if le := atomic.SwapInt32(&r.loggingErrors, 0); le > 0 {
		doReportLoggingErrorsFunc(r.src.Name(), r.info.ContainerID, le)
	}
}

// BufSize returns the buffer size of the underlying logger.
// Returns -1 if the logger doesn't match SizedLogger interface.
func (r *droppedLogsReporter) BufSize() int {
	if sl, ok := r.src.(logger.SizedLogger); ok {
		return sl.BufSize()
	}
	return -1
}

// Log invokes the Log of the underlying Logger. Any errors returned by the underlying logger are counted, and
// eventually reported in the daemon logs, but this method always returns nil.
func (r *droppedLogsReporter) Log(msg *logger.Message) error {
	if err := r.src.Log(msg); err != nil {
		atomic.AddInt32(&r.loggingErrors, 1)
	}
	return nil
}

// Name returns the nameFunc of the underlying logger.
func (r *droppedLogsReporter) Name() string {
	return r.src.Name()
}

// Close closes the logger.
func (r *droppedLogsReporter) Close() error {
	if r.isClosed() {
		return nil
	}
	err := r.src.Close()
	// Close the done channel after closing the underlying logger to account for possible logging errors during
	// the underlying logger's draining process
	close(r.done)
	return err
}

func (r *droppedLogsReporter) isClosed() bool {
	select {
	case _, ok := <-r.done:
		if !ok { // this means the done chan is already closed
			return true
		}
	default:
	}
	return false
}

var doReportLoggingErrorsFunc = func(driverName, container string, loggingErrors int32) {
	logrus.WithFields(logrus.Fields{
		"driver":    driverName,
		"container": container,
		"dropped":   loggingErrors,
	}).Warn("Container logs were dropped")
}

// newTicker is a wrapper for time.NewTicker. It is a variable so that the implementation can be swapped out for unit
// tests.
var newTicker = func(freq time.Duration) *time.Ticker {
	return time.NewTicker(freq)
}

type droppedLogsReporterWithReader struct {
	*droppedLogsReporter
}

func (r *droppedLogsReporterWithReader) ReadLogs(cfg logger.ReadConfig) *logger.LogWatcher {
	reader, ok := r.src.(logger.LogReader)
	if !ok {
		// something is wrong if we get here
		panic("expected logFunc reader")
	}
	return reader.ReadLogs(cfg)
}
