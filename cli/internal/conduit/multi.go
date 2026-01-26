/*
 * Copyright (c) 2026, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package conduit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Psiphon-Inc/conduit/cli/internal/config"
)

// MultiService manages multiple Conduit inproxy instances in parallel
type MultiService struct {
	instances []*Instance
	mu        sync.RWMutex
}

// Instance represents a single running inproxy instance
type Instance struct {
	ID      int
	KeyHash string // Short hash of public key for identification
	Config  *config.Config
	Service *Service
	Stats   Stats
}

// NewMultiService creates a new multi-instance service
func NewMultiService(configs []*config.Config) (*MultiService, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("at least one instance configuration is required")
	}

	instances := make([]*Instance, len(configs))
	for i, cfg := range configs {
		service, err := New(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create service for instance %d: %w", i, err)
		}

		// Get key hash for this instance
		keyHash := cfg.GetKeyShortHash()
		if keyHash == "" {
			keyHash = fmt.Sprintf("%d", i)
		}

		instances[i] = &Instance{
			ID:      i,
			KeyHash: keyHash,
			Config:  cfg,
			Service: service,
		}
	}

	return &MultiService{
		instances: instances,
	}, nil
}

// Run starts all instances and blocks until context is cancelled
func (m *MultiService) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(m.instances))

	// Start all instances
	for _, inst := range m.instances {
		wg.Add(1)
		go func(instance *Instance) {
			defer wg.Done()
			if err := m.runInstance(ctx, instance); err != nil {
				errChan <- fmt.Errorf("[%s] error: %w", instance.KeyHash, err)
			}
		}(inst)
	}

	// Start stats aggregation goroutine
	statsDone := make(chan struct{})
	go func() {
		m.runStatsAggregator(ctx)
		close(statsDone)
	}()

	// Wait for all instances to complete
	wg.Wait()
	<-statsDone

	// Check for errors
	close(errChan)
	var firstErr error
	for err := range errChan {
		if firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// runInstance runs a single instance with key-hash prefixed logging
func (m *MultiService) runInstance(ctx context.Context, instance *Instance) error {
	// Log instance startup with key hash prefix
	fmt.Printf("[%s] Starting with data dir: %s\n", instance.KeyHash, instance.Config.DataDir)

	// Create instance-specific context
	instanceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Run the service
	err := instance.Service.Run(instanceCtx)

	// Update stats one final time
	m.mu.Lock()
	instance.Stats = instance.Service.GetStats()
	m.mu.Unlock()

	fmt.Printf("[%s] Stopped\n", instance.KeyHash)
	return err
}

// runStatsAggregator periodically logs aggregated stats from all instances
func (m *MultiService) runStatsAggregator(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.logAggregatedStats()
		}
	}
}

// logAggregatedStats logs combined stats from all instances
func (m *MultiService) logAggregatedStats() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var totalConnecting, totalConnected int
	var totalBytesUp, totalBytesDown int64
	var liveCount int

	for _, inst := range m.instances {
		stats := inst.Service.GetStats()
		totalConnecting += stats.ConnectingClients
		totalConnected += stats.ConnectedClients
		totalBytesUp += stats.TotalBytesUp
		totalBytesDown += stats.TotalBytesDown
		if stats.IsLive {
			liveCount++
		}
	}

	fmt.Printf("%s [AGGREGATE] Live: %d/%d | Connecting: %d | Connected: %d | Up: %s | Down: %s\n",
		time.Now().Format("2006-01-02 15:04:05"),
		liveCount, len(m.instances),
		totalConnecting,
		totalConnected,
		formatBytes(totalBytesUp),
		formatBytes(totalBytesDown),
	)
}

// GetAggregatedStats returns combined stats from all instances
func (m *MultiService) GetAggregatedStats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var aggregated Stats
	var earliestStart time.Time

	for _, inst := range m.instances {
		stats := inst.Service.GetStats()
		aggregated.ConnectingClients += stats.ConnectingClients
		aggregated.ConnectedClients += stats.ConnectedClients
		aggregated.TotalBytesUp += stats.TotalBytesUp
		aggregated.TotalBytesDown += stats.TotalBytesDown
		if stats.IsLive {
			aggregated.IsLive = true
		}
		if earliestStart.IsZero() || stats.StartTime.Before(earliestStart) {
			earliestStart = stats.StartTime
		}
	}
	aggregated.StartTime = earliestStart

	return aggregated
}

// GetInstanceCount returns the number of instances
func (m *MultiService) GetInstanceCount() int {
	return len(m.instances)
}
