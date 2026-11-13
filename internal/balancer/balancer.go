// Package balancer selects service instances for client requests.
package balancer

import (
	"errors"
	"sync"

	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
)

var (
	// ErrNoInstances indicates an empty instance list.
	ErrNoInstances = errors.New("balancer: no instances")
	// ErrNoWeightedInstances indicates that every instance has a non-positive
	// weight.
	ErrNoWeightedInstances = errors.New("balancer: no instances with positive weight")
	// ErrInvalidInstances indicates duplicate IDs in one selection input.
	ErrInvalidInstances = errors.New("balancer: invalid instances")
)

// Balancer selects one instance from an immutable snapshot.
type Balancer interface {
	Pick(instances []registry.Instance) (registry.Instance, error)
}

// RoundRobin rotates through instance order without global modification.
type RoundRobin struct {
	mu   sync.Mutex
	next uint64
}

// NewRoundRobin creates a round-robin balancer.
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

// Pick returns the next instance in stable input order.
func (balancer *RoundRobin) Pick(instances []registry.Instance) (registry.Instance, error) {
	if len(instances) == 0 {
		return registry.Instance{}, ErrNoInstances
	}
	if err := validateInstances(instances); err != nil {
		return registry.Instance{}, err
	}
	balancer.mu.Lock()
	index := balancer.next % uint64(len(instances))
	balancer.next++
	balancer.mu.Unlock()
	return instances[index], nil
}

// WeightedRoundRobin implements smooth weighted round robin.
//
// The state key is the stable instance ID, so adding or removing another
// instance does not reset the fairness state of surviving instances.
type WeightedRoundRobin struct {
	mu      sync.Mutex
	current map[string]int64
}

// NewWeightedRoundRobin creates a weighted round-robin balancer.
func NewWeightedRoundRobin() *WeightedRoundRobin {
	return &WeightedRoundRobin{current: make(map[string]int64)}
}

// Pick selects the instance with the largest accumulated weight and then
// subtracts the total positive weight.
func (balancer *WeightedRoundRobin) Pick(instances []registry.Instance) (registry.Instance, error) {
	if len(instances) == 0 {
		return registry.Instance{}, ErrNoInstances
	}
	if err := validateInstances(instances); err != nil {
		return registry.Instance{}, err
	}

	balancer.mu.Lock()
	defer balancer.mu.Unlock()

	var selected registry.Instance
	selectedIndex := -1
	var selectedCurrent int64
	var totalWeight int64
	active := make(map[string]struct{}, len(instances))
	for index, instance := range instances {
		if instance.Weight <= 0 {
			continue
		}
		active[instance.ID] = struct{}{}
		current := balancer.current[instance.ID] + int64(instance.Weight)
		balancer.current[instance.ID] = current
		totalWeight += int64(instance.Weight)
		if selectedIndex == -1 || current > selectedCurrent {
			selected = instance
			selectedIndex = index
			selectedCurrent = current
		}
	}
	if selectedIndex == -1 {
		for id := range balancer.current {
			delete(balancer.current, id)
		}
		return registry.Instance{}, ErrNoWeightedInstances
	}
	for id := range balancer.current {
		if _, ok := active[id]; !ok {
			delete(balancer.current, id)
		}
	}
	balancer.current[selected.ID] -= totalWeight
	return selected, nil
}

func validateInstances(instances []registry.Instance) error {
	seen := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		if instance.ID == "" || instance.Address == "" {
			return ErrInvalidInstances
		}
		if _, ok := seen[instance.ID]; ok {
			return ErrInvalidInstances
		}
		seen[instance.ID] = struct{}{}
	}
	return nil
}
