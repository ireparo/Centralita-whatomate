package crm

import (
	"testing"
	"time"
)

func TestLookupCache_PutGetFound(t *testing.T) {
	cache := NewLookupCache(5*time.Minute, 30*time.Second)
	resp := &LookupResponse{
		Found:           true,
		NormalizedPhone: "34637000111",
		Customer:        &Customer{ID: 42, Name: "Juan Pérez"},
	}
	cache.Put("34637000111", resp)

	got, ok := cache.Get("34637000111")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Customer == nil || got.Customer.ID != 42 {
		t.Errorf("got %+v, want customer 42", got)
	}
}

func TestLookupCache_NotFoundShorterTTL(t *testing.T) {
	cache := NewLookupCache(time.Hour, 50*time.Millisecond)
	resp := &LookupResponse{Found: false, NormalizedPhone: "34000000000"}
	cache.Put("34000000000", resp)

	if _, ok := cache.Get("34000000000"); !ok {
		t.Fatal("expected immediate hit")
	}

	time.Sleep(100 * time.Millisecond)

	if _, ok := cache.Get("34000000000"); ok {
		t.Error("expected expiration after negative TTL")
	}
}

func TestLookupCache_Invalidate(t *testing.T) {
	cache := NewLookupCache(time.Hour, time.Hour)
	cache.Put("34637000111", &LookupResponse{Found: true, NormalizedPhone: "34637000111"})

	if _, ok := cache.Get("34637000111"); !ok {
		t.Fatal("expected hit before invalidate")
	}

	cache.Invalidate("34637000111")

	if _, ok := cache.Get("34637000111"); ok {
		t.Error("expected miss after invalidate")
	}
}

func TestLookupCache_NilSafe(t *testing.T) {
	var cache *LookupCache
	cache.Put("x", &LookupResponse{Found: true})
	if _, ok := cache.Get("x"); ok {
		t.Error("nil cache Get should return false")
	}
	if cache.Size() != 0 {
		t.Error("nil cache Size should return 0")
	}
	cache.Invalidate("x") // should not panic
}
