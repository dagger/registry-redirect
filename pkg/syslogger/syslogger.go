package syslogger

import (
	"fmt"
	"log/syslog"
	"sync"
)

type SyslogWriter struct {
	writer *syslog.Writer
	mutex  sync.RWMutex
}

func NewSyslogWriter(level, protocol, address, tag string) (*SyslogWriter, error) {
	logLevel := syslogLevel(level) | syslog.LOG_LOCAL0

	w, err := syslog.Dial(protocol, address, logLevel, tag)
	if err != nil {
		return nil, err
	}
	return &SyslogWriter{writer: w}, nil
}

// Send p to syslog and return the number of bytes written and any error encountered
func (sw *SyslogWriter) Write(p []byte) (n int, err error) {
	// Read lock: we accept concurrent writes to the syslog writer
	sw.mutex.RLock()
	defer sw.mutex.RUnlock()

	if sw.writer == nil {
		return 0, fmt.Errorf("syslog writer is closed")
	}

	len, err := sw.writer.Write(p)
	if err != nil {
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

	if sw.writer != nil {
		err := sw.writer.Close()
		sw.writer = nil
		return err
	}
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
