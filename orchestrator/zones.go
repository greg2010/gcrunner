package function

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// ZoneCache is safe for concurrent use.
type ZoneCache struct {
	mu      sync.RWMutex
	zones   map[string]zoneCacheEntry
	ttl     time.Duration
	nowFunc func() time.Time
}

type zoneCacheEntry struct {
	zones     []string
	fetchedAt time.Time
}

var zoneCache = &ZoneCache{
	zones:   make(map[string]zoneCacheEntry),
	ttl:     1 * time.Hour,
	nowFunc: time.Now,
}

// ListZones returns cached UP zones or queries Compute Engine.
func ListZones(ctx context.Context, project, region string) ([]string, error) {
	return zoneCache.list(ctx, project, region)
}

func (c *ZoneCache) list(ctx context.Context, project, region string) ([]string, error) {
	now := c.nowFunc()

	c.mu.RLock()
	entry, ok := c.zones[region]
	c.mu.RUnlock()

	if ok && now.Sub(entry.fetchedAt) < c.ttl {
		return entry.zones, nil
	}

	zones, err := fetchZones(ctx, project, region)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.zones[region] = zoneCacheEntry{zones: zones, fetchedAt: now}
	c.mu.Unlock()

	return zones, nil
}

func fetchZones(ctx context.Context, project, region string) ([]string, error) {
	client, err := compute.NewZonesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create zones client: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("WARN zones_client_close_failed error=%v", err)
		}
	}()

	filter := fmt.Sprintf(`status = "UP" AND name : "%s-*"`, region)
	it := client.List(ctx, &computepb.ListZonesRequest{
		Project: project,
		Filter:  proto.String(filter),
	})

	var zones []string
	for {
		zone, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list zones: %w", err)
		}
		zones = append(zones, zone.GetName())
	}

	if len(zones) == 0 {
		return nil, fmt.Errorf("no available zones found for region %s", region)
	}

	return zones, nil
}
