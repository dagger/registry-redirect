package redirect

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestResponseCacheExpiresAtFixedTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newResponseCache(45*time.Second, 10)
		answer := newCachedResponse(http.StatusOK, nil, []byte(`{"token":"test"}`))
		c.set("scope", answer)

		time.Sleep(44 * time.Second)
		if got, ok := c.get("scope"); !ok || got != answer {
			t.Fatal("cache miss before expiry")
		}
		// Reading the entry must not extend its lifetime.
		time.Sleep(time.Second)
		if _, ok := c.get("scope"); ok {
			t.Fatal("cache hit at expiry")
		}
		if len(c.entries) != 0 {
			t.Fatal("expired entry was not removed")
		}
	})
}

func TestResponseCacheCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newResponseCache(time.Minute, 2)
		answer := newCachedResponse(http.StatusOK, nil, nil)
		c.set("a", answer)
		c.set("b", answer)
		if c.set("c", answer) {
			t.Fatal("stored a third entry in a cache capped at two")
		}
		if _, ok := c.get("c"); ok {
			t.Fatal("refused entry is readable")
		}
		replacement := newCachedResponse(http.StatusUnauthorized, nil, nil)
		if !c.set("a", replacement) {
			t.Fatal("overwrite of an existing key was refused")
		}
		if got, _ := c.get("a"); got != replacement {
			t.Fatal("overwrite did not replace the response")
		}

		time.Sleep(time.Minute)
		if !c.set("c", answer) || len(c.entries) != 1 {
			t.Fatal("expired entries did not make room for a new response")
		}
	})
}

func TestResponseCacheDisabled(t *testing.T) {
	var c *responseCache
	if _, ok := c.get("x"); ok {
		t.Fatal("disabled cache reported a hit")
	}
	if c.set("x", newCachedResponse(http.StatusOK, nil, nil)) {
		t.Fatal("disabled cache stored a response")
	}
}

func TestCachedResponseCopiesHeaderAndBody(t *testing.T) {
	header := http.Header{"Www-Authenticate": {"Bearer realm=x"}}
	body := []byte("original")
	entry := newCachedResponse(http.StatusUnauthorized, header, body)
	header.Set("Www-Authenticate", "tampered")
	copy(body, "TAMPERED")

	if got := entry.header.Get("Www-Authenticate"); got != "Bearer realm=x" {
		t.Fatalf("stored header = %q, changed through the caller's map", got)
	}
	if got := string(entry.body); got != "original" {
		t.Fatalf("stored body = %q, changed through the caller's slice", got)
	}
}
