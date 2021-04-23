package logger

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

type mockLogDriver struct {
	logFunc     func(msg *Message) error
	nameFunc    func() string
	closeFunc   func() error
	bufSizeFunc func() int
}

func (l *mockLogDriver) Log(msg *Message) error {
	if l.logFunc != nil {
		return l.logFunc(msg)
	}
	return nil
}

func (l *mockLogDriver) Name() string {
	if l.nameFunc != nil {
		return l.nameFunc()
	}
	return "mock"
}

func (l *mockLogDriver) Close() error {
	if l.closeFunc != nil {
		return l.closeFunc()
	}
	return nil
}

func (l *mockLogDriver) BufSize() int {
	if l.bufSizeFunc != nil {
		return l.bufSizeFunc()
	}
	return 0
}

type mockLogDriverReader struct {
	*mockLogDriver
	readLogsFunc func(cfg ReadConfig) *LogWatcher
}

func (r *mockLogDriverReader) ReadLogs(cfg ReadConfig) *LogWatcher {
	if r.readLogsFunc != nil {
		return r.readLogsFunc(cfg)
	}
	return nil
}

func TestWithLogErrorReporter(t *testing.T) {
	l := &mockLogDriver{}
	r := WithLogErrorReporter(l, Info{})
	dlr := r.(*logErrorReporter)
	assert.Equal(t, l, dlr.src)
	if dlr.done == nil {
		t.Fatal("Expected non-nil done channel")
	}
}

func TestWithLogErrorReporterWithReader(t *testing.T) {
	l := &mockLogDriverReader{mockLogDriver: &mockLogDriver{}}
	r := WithLogErrorReporter(l, Info{})
	dlr := r.(*logErrorReporterWithReader)
	assert.Equal(t, l, dlr.src)
	if dlr.done == nil {
		t.Fatal("Expected non-nil done channel")
	}
	_, ok := r.(LogReader)
	assert.Equal(t, true, ok)
}

func TestLogErrorReporter_InvokesUnderlyingLogger(t *testing.T) {
	var logInvoked, nameInvoked, closeInvoked, bufSizeInvoked, readLogsInvoked bool
	mockErr := errors.New("mock error")
	mockName := "mockDriver"
	mockLogMsg := &Message{}
	mockReadConfig := ReadConfig{}
	mockLogWatcher := &LogWatcher{}
	mockBufSize := 1
	l := &mockLogDriverReader{
		mockLogDriver: &mockLogDriver{
			logFunc: func(msg *Message) error {
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
		readLogsFunc: func(cfg ReadConfig) *LogWatcher {
			assert.Equal(t, mockReadConfig, cfg)
			readLogsInvoked = true
			return mockLogWatcher
		},
	}

	r := &logErrorReporterWithReader{
		&logErrorReporter{
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

func TestLogErrorReporter_ReportDroppedLogs(t *testing.T) {
	l := &mockLogDriver{}
	info := Info{
		ContainerID: "c",
	}
	dlr := &logErrorReporter{
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
	err := dlr.Log(&Message{})
	assert.NilError(t, err)

	// simulate underlying logger returns error
	l.logFunc = func(msg *Message) error {
		return errors.New("mock logFunc error")
	}
	dlr.Log(&Message{}) // This one should be counted as dropped
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
