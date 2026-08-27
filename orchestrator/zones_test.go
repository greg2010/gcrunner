package function

import (
	"testing"
	"time"
)

func TestZoneCache_TTL(t *testing.T) {
	now := time.Now()
	cache := &ZoneCache{
		zones:   make(map[string]zoneCacheEntry),
		ttl:     1 * time.Hour,
		nowFunc: func() time.Time { return now },
	}

	cache.zones["us-central1"] = zoneCacheEntry{
		zones:     []string{"us-central1-a", "us-central1-b", "us-central1-c"},
		fetchedAt: now,
	}

	cache.mu.RLock()
	entry, ok := cache.zones["us-central1"]
	cache.mu.RUnlock()
	if !ok {
		t.Fatal("expected cache entry")
	}
	if now.Sub(entry.fetchedAt) >= cache.ttl {
		t.Error("cache entry should be valid")
	}

	cache.nowFunc = func() time.Time { return now.Add(2 * time.Hour) }
	staleNow := cache.nowFunc()
	if staleNow.Sub(entry.fetchedAt) < cache.ttl {
		t.Error("cache entry should be stale after TTL")
	}
}

func TestZoneCache_DifferentRegions(t *testing.T) {
	now := time.Now()
	cache := &ZoneCache{
		zones:   make(map[string]zoneCacheEntry),
		ttl:     1 * time.Hour,
		nowFunc: func() time.Time { return now },
	}

	cache.zones["us-central1"] = zoneCacheEntry{
		zones:     []string{"us-central1-a", "us-central1-b"},
		fetchedAt: now,
	}
	cache.zones["europe-west1"] = zoneCacheEntry{
		zones:     []string{"europe-west1-b", "europe-west1-c", "europe-west1-d"},
		fetchedAt: now,
	}

	if len(cache.zones["us-central1"].zones) != 2 {
		t.Errorf("us-central1 zones = %d, want 2", len(cache.zones["us-central1"].zones))
	}
	if len(cache.zones["europe-west1"].zones) != 3 {
		t.Errorf("europe-west1 zones = %d, want 3", len(cache.zones["europe-west1"].zones))
	}
}
