package redirect_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func serveRequest(t *testing.T, handler http.Handler, req *http.Request, want int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s: status = %d, want %d; body = %s", req.Method, req.URL, rec.Code, want, rec.Body.String())
	}
	return rec
}

// Each call gets its own request; errors are reported from the test goroutine.
func burstRequests(t *testing.T, handler http.Handler, n int, request func(int) *http.Request) map[int]int {
	t.Helper()
	results := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, request(i))
			results <- rec.Code
		}()
	}
	counts := make(map[int]int)
	for i := 0; i < n; i++ {
		select {
		case status := <-results:
			counts[status]++
		case <-time.After(5 * time.Second):
			t.Fatal("request burst did not finish")
		}
	}
	return counts
}
