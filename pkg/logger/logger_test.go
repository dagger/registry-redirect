package logger

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/fortytw2/leaktest"
	"github.com/stretchr/testify/require"
	"knative.dev/pkg/logging"
)

// Utility functions copied from main.go
// These is the graceful shutdown logic we want to test
type CustomHandler struct {
	wg      *sync.WaitGroup
	handler http.Handler
}

func NewCustomHandler(wg *sync.WaitGroup, handler http.Handler) *CustomHandler {
	return &CustomHandler{wg: wg, handler: handler}
}

func (h *CustomHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.wg.Add(1)
	defer h.wg.Done()
	h.handler.ServeHTTP(w, r)
}

// simple handler that logs the request
// and timeouts to make sure the server is not blocked
// We do that to simulate a long running request
func makeLoggingHandler(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger := logging.FromContext(ctx)
		requestID := r.URL.Query().Get("id")

		timeout := time.After(1 * time.Second)
		select {
		case <-ctx.Done():
		case <-timeout:
		}

		logger.Infof("Received request:%s %s, with id: %s", r.Method, r.URL, requestID) // use our logger to test concurrency
	}
}

func syslogServer(port string, messages chan<- string, ready chan<- struct{}, shutdownSyslog <-chan struct{}, waitForMessages chan<- struct{}) {
	// start listening on the port
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("Error starting the server: %v", err)
	}

	// Notify that server is ready
	ready <- struct{}{}

	var wg sync.WaitGroup
	connections := &sync.Map{}

	// Shutdown logic
	go func() {
		<-shutdownSyslog
		ln.Close()

		// closing connections to force the scanner to stop
		connections.Range(func(k, v interface{}) bool {
			if conn, ok := k.(net.Conn); ok {
				conn.Close()
			}
			return true
		})
	}()

	// start accepting connections
	for {
		conn, err := ln.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Op == "accept" {
				// Listener closed, stopping accepting connections
				break
			}
			log.Printf("Error accepting connection: %v", err)
			continue
		}

		// new connection, add it to the map
		connections.Store(conn, true)

		// read from the connection
		// and send the messages to the message channel
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()
			defer conn.Close()
			defer connections.Delete(conn) // delete the connection from map when done

			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				text := scanner.Text()
				messages <- text
			}
		}(conn)
	}

	// no more incoming connections, wait for all connections to be closed
	wg.Wait()
	close(messages)
	waitForMessages <- struct{}{}
}

func startHTTPServer(
	ctx context.Context,
	port string,
	handler http.Handler,
	shutdownSyslog chan<- struct{},
) (<-chan struct{}, chan<- struct{}, error) {
	ready := make(chan struct{})
	shutdownHTTP := make(chan struct{})

	var wg sync.WaitGroup
	// customHandler := NewCustomHandler(&wg, handler, count)
	customHandler := NewCustomHandler(&wg, handler)
	server := &http.Server{
		Addr:    ":" + port,
		Handler: customHandler,
	}

	// Start the server
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error starting the server: %v", err)
		}
	}()

	// Listen for shutdown signal
	go func(ctx context.Context, wg *sync.WaitGroup) {
		<-shutdownHTTP

		// Start shutdown process of HTTP server
		if err := server.Shutdown(ctx); err != nil {
			log.Fatalf("Server Shutdown: %v", err)
		}

		// Wait for all HTTP requests to be processed
		// Feature that we test on the prod server
		wg.Wait()

		// Notify that the HTTP server is shutdown
		close(shutdownSyslog)
	}(ctx, &wg)

	go func() {
		for {
			conn, err := net.Dial("tcp", ":"+port)
			if err == nil {
				_ = conn.Close()
				close(ready)
				return
			}
			time.Sleep(100 * time.Millisecond) // Small delay to retry
		}
	}()

	return ready, shutdownHTTP, nil
}

func TestLoggerGraceFulShutDown(t *testing.T) {
	defer leaktest.Check(t)() // Ensure all goroutines are shutdown

	amountOfMessages := 10                          // amount of messages to send to the logger
	messages := make(chan string, amountOfMessages) // channel to receive syslog messages
	waitForMessages := make(chan struct{})          // channel to notify when all messages are received

	syslogReady := make(chan struct{})    // channel to notify when the syslog server is ready
	shutdownSyslog := make(chan struct{}) // channel to notify when to shutdown the syslog server

	go syslogServer("16901", messages, syslogReady, shutdownSyslog, waitForMessages)

	<-syslogReady // Wait until the syslog server is ready

	// start the logger and http server
	logCfg := Config{
		Level:     "info",
		Component: "dagger-registry",
		Protocol:  "tcp",
		Address:   "127.0.0.1:16901",
	}

	ctx, err := NewLogger(context.Background(), &logCfg)
	if err != nil {
		panic(err)
	}

	// init the server mux
	cancelContext, cancel := context.WithCancel(ctx)

	mux := http.NewServeMux()
	handler := makeLoggingHandler(cancelContext)
	mux.HandleFunc("/", handler)

	serverReady, shutdownHTTP, err := startHTTPServer(ctx, "8080", mux, shutdownSyslog)
	if err != nil {
		panic(err)
	}

	<-serverReady // Wait until the http server is ready

	// Send requests to the http server
	// almost at the same time
	var wg sync.WaitGroup
	for i := 0; i < amountOfMessages; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := http.Get(fmt.Sprintf("http://localhost:8080/?id=%d", i))
			if err != nil {
				panic(err)
			}
		}(i)
	}

	cancel()  // Cancel the context to stop the http server
	wg.Wait() // Wait for all requests to be sent

	// Simulate shutdown signal while requests are still being processed
	// they will be processed at the same time as the shutdown signal
	// This is the feature that we test on the prod server: do we lose messages?
	close(shutdownHTTP)

	<-waitForMessages // wait for all messages to be received and processed in message channel

	// check that we received the expected amount of messages
	require.EqualValues(t, amountOfMessages, len(messages),
		fmt.Sprintf("Expected %d messages, got %d", amountOfMessages, len(messages)),
	)
}
