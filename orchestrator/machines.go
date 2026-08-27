package function

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
)

// MachineTypeInfo records the resource values Google Compute Engine reports for a machine type.
type MachineTypeInfo struct {
	Name     string
	Family   string
	Category string
	VCPUs    int32
	MemoryMB int32
}

// MachineTypeCache is safe for concurrent use.
type MachineTypeCache struct {
	mu      sync.RWMutex
	types   map[string]machineTypeCacheEntry
	ttl     time.Duration
	nowFunc func() time.Time
}

type machineTypeCacheEntry struct {
	types     []*MachineTypeInfo
	fetchedAt time.Time
}

var machineTypeCache = &MachineTypeCache{
	types:   make(map[string]machineTypeCacheEntry),
	ttl:     1 * time.Hour,
	nowFunc: time.Now,
}

// ListMachineTypes returns cached machine types for a zone or queries Compute Engine.
func ListMachineTypes(ctx context.Context, project, zone string) ([]*MachineTypeInfo, error) {
	return machineTypeCache.list(ctx, project, zone)
}

func (c *MachineTypeCache) list(ctx context.Context, project, zone string) ([]*MachineTypeInfo, error) {
	now := c.nowFunc()

	c.mu.RLock()
	entry, ok := c.types[zone]
	c.mu.RUnlock()

	if ok && now.Sub(entry.fetchedAt) < c.ttl {
		return entry.types, nil
	}

	types, err := fetchMachineTypes(ctx, project, zone)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.types[zone] = machineTypeCacheEntry{types: types, fetchedAt: now}
	c.mu.Unlock()

	return types, nil
}

func fetchMachineTypes(ctx context.Context, project, zone string) ([]*MachineTypeInfo, error) {
	client, err := compute.NewMachineTypesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create machine types client: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("WARN machine_types_client_close_failed error=%v", err)
		}
	}()

	it := client.List(ctx, &computepb.ListMachineTypesRequest{
		Project: project,
		Zone:    zone,
	})

	var types []*MachineTypeInfo
	for {
		mt, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list machine types: %w", err)
		}
		family, category := parseMachineFamily(mt.GetName())
		types = append(types, &MachineTypeInfo{
			Name:     mt.GetName(),
			Family:   family,
			Category: category,
			VCPUs:    mt.GetGuestCpus(),
			MemoryMB: mt.GetMemoryMb(),
		})
	}

	return types, nil
}

// ResolveMachineType selects the smallest supported type that satisfies labels.
// It returns a fatal creation error when labels are invalid or no type matches.
func ResolveMachineType(ctx context.Context, project, zone string, labels *RunnerLabels) (string, error) {
	if labels.MachineMode == "exact" {
		return labels.Machine, nil
	}

	types, err := ListMachineTypes(ctx, project, zone)
	if err != nil {
		return "", fmt.Errorf("list machine types for %s: %w", zone, err)
	}

	family := labels.Family
	if labels.MachineMode == "auto" {
		family = "n2d"
	}
	families := strings.Split(family, "+")

	minCPU, maxCPU, err := parseRange(labels.CPU)
	if err != nil {
		return "", &vmCreationError{kind: insertErrorFatal, err: fmt.Errorf("parse cpu label: %w", err)}
	}
	minRAM, maxRAM, err := parseRange(labels.RAM)
	if err != nil {
		return "", &vmCreationError{kind: insertErrorFatal, err: fmt.Errorf("parse ram label: %w", err)}
	}

	var best *MachineTypeInfo
	for _, mt := range types {
		if !matchesFamily(mt, families) {
			continue
		}
		if labels.KVM && kvmUnsupportedFamilies[mt.Family] {
			continue
		}
		if isSharedCoreCategory(mt.Category) {
			continue
		}
		if isGPUCategory(mt.Category) {
			continue
		}
		if minCPU > 0 && int(mt.VCPUs) < minCPU {
			continue
		}
		if maxCPU > 0 && int(mt.VCPUs) > maxCPU {
			continue
		}
		ramGB := int(mt.MemoryMB) / 1024
		if minRAM > 0 && ramGB < minRAM {
			continue
		}
		if maxRAM > 0 && ramGB > maxRAM {
			continue
		}
		if best == nil || mt.VCPUs < best.VCPUs || (mt.VCPUs == best.VCPUs && mt.MemoryMB < best.MemoryMB) {
			best = mt
		}
	}

	if best == nil {
		return "", &vmCreationError{
			kind: insertErrorNoMachineType,
			err: fmt.Errorf("no machine type matching constraints (families=%v, cpu=%s, ram=%s) in zone %s",
				families, labels.CPU, labels.RAM, zone),
		}
	}

	return best.Name, nil
}

func parseMachineFamily(name string) (family, category string) {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return name, ""
	}

	if parts[0] == "custom" {
		return "n1", "custom"
	}

	family = parts[0]
	category = parts[1]
	return
}

func matchesFamily(mt *MachineTypeInfo, families []string) bool {
	for _, f := range families {
		if mt.Family == f {
			return true
		}
	}
	return false
}

func isSharedCoreCategory(category string) bool {
	switch category {
	case "micro", "small", "medium":
		return true
	}
	return false
}

func isGPUCategory(category string) bool {
	switch category {
	case "highgpu", "megagpu", "ultragpu", "edgegpu", "maxgpu":
		return true
	}
	return false
}

func parseRange(s string) (min, max int, err error) {
	if s == "" {
		return 0, 0, nil
	}

	parts := strings.Split(s, "+")
	if len(parts) > 2 {
		return 0, 0, fmt.Errorf("invalid range %q", s)
	}
	min, err = strconv.Atoi(parts[0])
	if err != nil || min <= 0 {
		return 0, 0, fmt.Errorf("invalid range minimum %q", parts[0])
	}
	max = min
	if len(parts) == 2 {
		max, err = strconv.Atoi(parts[1])
		if err != nil || max <= 0 {
			return 0, 0, fmt.Errorf("invalid range maximum %q", parts[1])
		}
	}
	if min > max {
		return 0, 0, fmt.Errorf("range minimum %d exceeds maximum %d", min, max)
	}
	return min, max, nil
}
