package loggerutils

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/daemon/logger"
	"gotest.tools/v3/assert"
)

type mockLogger struct {
	logFunc     func(msg *logger.Message) error
	nameFunc    func() string
	closeFunc   func() error
	bufSizeFunc func() int
}

func (l *mockLogger) Log(msg *logger.Message) error {
	if l.logFunc != nil {
		return l.logFunc(msg)
	}
	return nil
}

func (l *mockLogger) Name() string {
	if l.nameFunc != nil {
		return l.nameFunc()
	}
	return "mock"
}

func (l *mockLogger) Close() error {
	if l.closeFunc != nil {
		return l.closeFunc()
	}
	return nil
}

func (l *mockLogger) BufSize() int {
	if l.bufSizeFunc != nil {
		return l.bufSizeFunc()
	}
	return 0
}

type mockLoggerReader struct {
	*mockLogger
	readLogsFunc func(cfg logger.ReadConfig) *logger.LogWatcher
}

func (r *mockLoggerReader) ReadLogs(cfg logger.ReadConfig) *logger.LogWatcher {
	if r.readLogsFunc != nil {
		return r.readLogsFunc(cfg)
	}
	return nil
}

func TestWithDroppedLogsReporter(t *testing.T) {
	l := &mockLogger{}
	r := WithDroppedLogsReporter(l, logger.Info{})
	dlr := r.(*droppedLogsReporter)
	assert.Equal(t, l, dlr.src)
	if dlr.done == nil {
		t.Fatal("Expected non-nil done channel")
	}
}

func TestWithDroppedLogsReporterWithReader(t *testing.T) {
	l := &mockLoggerReader{mockLogger: &mockLogger{}}
	r := WithDroppedLogsReporter(l, logger.Info{})
	dlr := r.(*droppedLogsReporterWithReader)
	assert.Equal(t, l, dlr.src)
	if dlr.done == nil {
		t.Fatal("Expected non-nil done channel")
	}
	_, ok := r.(logger.LogReader)
	assert.Equal(t, true, ok)
}

func TestDroppedLogsReporter_InvokesUnderlyingLogger(t *testing.T) {
	var logInvoked, nameInvoked, closeInvoked, bufSizeInvoked, readLogsInvoked bool
	mockErr := errors.New("mock error")
	mockName := "mockDriver"
	mockLogMsg := &logger.Message{}
	mockReadConfig := logger.ReadConfig{}
	mockLogWatcher := &logger.LogWatcher{}
	mockBufSize := 1
	l := &mockLoggerReader{
		mockLogger: &mockLogger{
			logFunc: func(msg *logger.Message) error {
				assert.Equal(t, mockLogMsg, msg)
				logInvoked = true
				return mockErr
			},
			nameFunc: func() string {
				nameInvoked = true
				return mockName
			},
			closeFunc: func() error {
				closeInvoked = true
				return mockErr
			},
			bufSizeFunc: func() int {
				bufSizeInvoked = true
				return mockBufSize
			},
		},
		readLogsFunc: func(cfg logger.ReadConfig) *logger.LogWatcher {
			assert.Equal(t, mockReadConfig, cfg)
			readLogsInvoked = true
			return mockLogWatcher
		},
	}

	r := &droppedLogsReporterWithReader{
		&droppedLogsReporter{
			src:  l,
			done: make(chan struct{}),
		},
	}

	logErr := r.Log(mockLogMsg)
	assert.Equal(t, true, logInvoked)
	assert.NilError(t, logErr)

	name := r.Name()
	assert.Equal(t, true, nameInvoked)
	assert.Equal(t, mockName, name)

	closeErr := r.Close()
	assert.Equal(t, true, closeInvoked)
	assert.Equal(t, mockErr, closeErr)
	assert.Equal(t, true, r.isClosed())

	// Check calling Close is idempotent
	closeInvoked = false
	closeErr = r.Close()
	assert.Equal(t, false, closeInvoked)
	assert.NilError(t, closeErr)
	assert.Equal(t, true, r.isClosed())

	bz := r.BufSize()
	assert.Equal(t, true, bufSizeInvoked)
	assert.Equal(t, mockBufSize, bz)

	w := l.ReadLogs(mockReadConfig)
	assert.Equal(t, true, readLogsInvoked)
	assert.Equal(t, mockLogWatcher, w)
}

func TestDroppedLogsReporter_ReportDroppedLogs(t *testing.T) {
	l := &mockLogger{}
	info := logger.Info{
		ContainerID: "c",
	}
	dlr := &droppedLogsReporter{
		src:  l,
		info: info,
		done: make(chan struct{}),
	}

	reportDroppedLogsFuncBkp := doReportLoggingErrorsFunc
	defer func() {
		doReportLoggingErrorsFunc = reportDroppedLogsFuncBkp
	}()
	reportDroppedLogsFuncInvoked := false
	doReportLoggingErrorsFunc = func(driverName, containerId string, loggingErrors int32) {
		reportDroppedLogsFuncInvoked = true
		assert.Equal(t, driverName, l.Name(), "Wrong driverName was received")
		assert.Equal(t, containerId, info.ContainerID, "Wrong container id was received")
		assert.Equal(t, int32(1), loggingErrors, "Expected one logging error")
	}

	l.logFunc = nil
	err := dlr.Log(&logger.Message{})
	assert.NilError(t, err)

	// simulate underlying logger returns error
	l.logFunc = func(msg *logger.Message) error {
		return errors.New("mock logFunc error")
	}
	dlr.Log(&logger.Message{}) // This one should be counted as dropped
	assert.NilError(t, err)

	ticks := make(chan time.Time)
	newTickerBkp := newTicker
	defer func() {
		newTicker = newTickerBkp
	}()
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(started)
		dlr.startReporter()
		close(finished)
	}()
	<-started

	assert.Equal(t, int32(1), atomic.LoadInt32(&dlr.loggingErrors), "Expected one logging error")

	select {
	case ticks <- time.Time{}:
		dlr.Close()
	case <-time.After(time.Second * 5):
		t.Fatal("startReporter didn't process the tick")
	}

	select {
	case <-finished:
	case <-time.After(time.Second * 5):
		t.Fatal("startReporter didn't exit after closing the logger")
	}

	assert.Equal(t, true, reportDroppedLogsFuncInvoked, "Expected doReportLoggingErrorsFunc to be invoked")
	// Check the counter is reset after the tick
	assert.Equal(t, int32(0), atomic.LoadInt32(&dlr.loggingErrors), "Expected zero logging errors after the tick")
}
