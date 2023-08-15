package syslogger

import (
	"fmt"
	"log/syslog"
	"sync"
)

type SyslogWriter struct {
	writer      *syslog.Writer
	mutex       sync.RWMutex
	protocol    string
	address     string
	tag         string
	level       string
	isConnected bool
	isClosed    bool
}

func NewSyslogWriter(level, protocol, address, tag string) *SyslogWriter {
	return &SyslogWriter{
		protocol:    protocol,
		address:     address,
		tag:         tag,
		level:       level,
		isConnected: false, // Initially set to false since we're not connecting right away
		isClosed:    false,
	}
}

func (sw *SyslogWriter) Connect() error {
	sw.mutex.Lock()
	defer sw.mutex.Unlock()

	if sw.isConnected && sw.writer != nil {
		return nil // Already connected
	}

	// If we're here, either the connection flag is false, or the writer is nil (or both).
	// It makes sense to try establishing a connection in such cases.
	return sw.connect()
}

func (sw *SyslogWriter) connect() error {
	logLevel := syslogLevel(sw.level) | syslog.LOG_LOCAL0
	w, err := syslog.Dial(sw.protocol, sw.address, logLevel, sw.tag)
	if err != nil {
		return err
	}
	sw.writer = w
	sw.isConnected = true
	return nil
}

func (sw *SyslogWriter) Write(p []byte) (n int, err error) {
	sw.mutex.Lock()
	defer sw.mutex.Unlock()

	// If we triggered the Close() method, don't try to reconnect
	if sw.isClosed {
		return 0, fmt.Errorf("syslog writer has been closed")
	}

	if !sw.isConnected || sw.writer == nil {
		if err := sw.connect(); err != nil {
			return 0, err
		}
	}

	len, err := sw.writer.Write(p)
	if err != nil {
		// Mark connection as lost
		sw.writer = nil
		sw.isConnected = false
		return 0, err
	}
	return len, nil
}

// Idempotent close: if the writer is already closed, this is a no-op
func (sw *SyslogWriter) Close() error {
	// Write lock: we want to make sure that all concurrent writes are finished before closing the writer
	// shall not happen, as we only close the writer when the application is shutting down, after sync.Waitgroup is done
	// but performance cost is minimal, and it is useful for testing
	sw.mutex.Lock()
	defer sw.mutex.Unlock()

	if sw.isClosed || sw.writer == nil {
		return nil // Already closed
	}
	err := sw.writer.Close()
	if err != nil {
		return err
	}
	sw.writer = nil
	sw.isConnected = false
	sw.isClosed = true

	return nil
}

// Extract syslog priority from log level
func syslogLevel(level string) syslog.Priority {
	switch level {
	case "debug":
		return syslog.LOG_DEBUG
	case "info":
		return syslog.LOG_INFO
	case "warn":
		return syslog.LOG_WARNING
	case "error":
		return syslog.LOG_ERR
	case "dpanic", "panic", "fatal":
		return syslog.LOG_CRIT
	default: // Default to info level if no level has been configured
		return syslog.LOG_INFO
	}
}
