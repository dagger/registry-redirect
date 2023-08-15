package syslogger

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyslogWriter(t *testing.T) {
	// udp is ok in this test case:
	// we just want to test if it works
	sw := NewSyslogWriter("debug", "udp", "localhost:514", "test")
	err := sw.Connect()
	require.NoError(t, err, "Unexpected error in NewSyslogWriter")

	data := []byte("hello, world!")
	n, err := sw.Write(data)
	require.NoError(t, err, "Unexpected error in Write")
	require.Equal(t, len(data), n, "Write returned incorrect length")

	err = sw.Close()
	require.NoError(t, err, "Unexpected error in Close")

	_, err = sw.Write(data)
	require.Error(t, err, "Expected error in Write after Close, but got nil")
}

// Test concurrency with `go test -race`
func TestSyslogWriterConcurrency(t *testing.T) {
	// udp is ok in this test case:
	// we just want to test if concurrency races
	sw := NewSyslogWriter("debug", "udp", "localhost:514", "test")
	err := sw.Connect()
	require.NoError(t, err, "Unexpected error in NewSyslogWriter")

	data := []byte("hello, world!")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := sw.Write(data)
			require.NoError(t, err, "Unexpected error in Write")
			require.Equal(t, len(data), n, "Write returned incorrect length")
		}()
	}

	wg.Wait()

	err = sw.Close()
	require.NoError(t, err, "Unexpected error in Close")

	_, err = sw.Write(data)
	require.Error(t, err, "Expected error in Write after Close, but got nil")
}
