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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Psiphon-Inc/conduit/cli/internal/config"
)

const (
	// ClientsPerInstance is the target number of clients per instance
	ClientsPerInstance = 100
)

// InstanceStats tracks stats for a single instance
type InstanceStats struct {
	ID         string
	IsLive     bool
	Connecting int
	Connected  int
	BytesUp    int64
	BytesDown  int64
}

// MultiService manages multiple conduit subprocess instances
type MultiService struct {
	config        *config.Config
	numInstances  int
	processes     []*exec.Cmd
	instanceStats []*InstanceStats
	cancel        context.CancelFunc
	mu            sync.Mutex
	startTime     time.Time
	statsDone     chan struct{}
}

// AggregateStatsJSON represents the JSON structure for multi-instance stats
type AggregateStatsJSON struct {
	LiveInstances     int            `json:"liveInstances"`
	TotalInstances    int            `json:"totalInstances"`
	ConnectingClients int            `json:"connectingClients"`
	ConnectedClients  int            `json:"connectedClients"`
	TotalBytesUp      int64          `json:"totalBytesUp"`
	TotalBytesDown    int64          `json:"totalBytesDown"`
	UptimeSeconds     int64          `json:"uptimeSeconds"`
	Timestamp         string         `json:"timestamp"`
	Instances         []InstanceJSON `json:"instances,omitempty"`
}

// InstanceJSON represents per-instance stats in JSON
type InstanceJSON struct {
	ID         string `json:"id"`
	IsLive     bool   `json:"isLive"`
	Connecting int    `json:"connecting"`
	Connected  int    `json:"connected"`
	BytesUp    int64  `json:"bytesUp"`
	BytesDown  int64  `json:"bytesDown"`
}

// NewMultiService creates a multi-instance service that spawns subprocesses
func NewMultiService(cfg *config.Config, numInstances int) (*MultiService, error) {
	instanceStats := make([]*InstanceStats, numInstances)
	for i := 0; i < numInstances; i++ {
		instanceStats[i] = &InstanceStats{
			ID: fmt.Sprintf("instance-%d", i),
		}
	}

	return &MultiService{
		config:        cfg,
		numInstances:  numInstances,
		processes:     make([]*exec.Cmd, numInstances),
		instanceStats: instanceStats,
		startTime:     time.Now(),
		statsDone:     make(chan struct{}),
	}, nil
}

// Run starts all subprocess instances and monitors them
func (m *MultiService) Run(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)

	clientsPerInstance := m.config.MaxClients / m.numInstances
	if clientsPerInstance < 1 {
		clientsPerInstance = 1
	}

	var bandwidthPerInstance float64
	if m.config.BandwidthBytesPerSecond > 0 {
		bandwidthPerInstance = float64(m.config.BandwidthBytesPerSecond) / float64(m.numInstances)
		bandwidthPerInstance = bandwidthPerInstance / 125000 // Convert to Mbps
	} else {
		bandwidthPerInstance = -1
	}

	bandwidthStr := "unlimited"
	if bandwidthPerInstance > 0 {
		bandwidthStr = fmt.Sprintf("%.0f Mbps/instance", bandwidthPerInstance)
	}
	fmt.Printf("Starting %d Psiphon Conduit instances (Max Clients/instance: %d, Bandwidth: %s)\n",
		m.numInstances, clientsPerInstance, bandwidthStr)

	var wg sync.WaitGroup
	errChan := make(chan error, m.numInstances)

	for i := 0; i < m.numInstances; i++ {
		instanceDataDir := filepath.Join(m.config.DataDir, fmt.Sprintf("instance-%d", i))

		if err := os.MkdirAll(instanceDataDir, 0700); err != nil {
			return fmt.Errorf("failed to create instance directory: %w", err)
		}

		wg.Add(1)
		go func(idx int, dataDir string) {
			defer wg.Done()
			if err := m.runInstance(ctx, idx, dataDir, clientsPerInstance, bandwidthPerInstance); err != nil {
				if ctx.Err() == nil {
					errChan <- fmt.Errorf("instance-%d: %w", idx, err)
				}
			}
		}(i, instanceDataDir)

		fmt.Printf("[instance-%d] Starting with data dir: %s\n", i, instanceDataDir)
	}

	go m.aggregateAndPrintStats(ctx)

	wg.Wait()

	// Cancel context to trigger final stats write
	m.cancel()

	// Wait for stats goroutine to complete its final write
	<-m.statsDone

	fmt.Println("All instances stopped.")

	select {
	case err := <-errChan:
		return err
	default:
		return nil
	}
}

// runInstance spawns and monitors a single conduit subprocess
func (m *MultiService) runInstance(ctx context.Context, idx int, dataDir string, maxClients int, bandwidthMbps float64) error {
	args := []string{"start",
		"--data-dir", dataDir,
		"-m", strconv.Itoa(maxClients),
	}

	if bandwidthMbps > 0 {
		args = append(args, "-b", fmt.Sprintf("%.2f", bandwidthMbps))
	} else {
		args = append(args, "-b", "-1")
	}

	// Pass through psiphon config path if set (for non-embedded config builds)
	if m.config.PsiphonConfigPath != "" {
		args = append(args, "-c", m.config.PsiphonConfigPath)
	}

	// Pass through verbosity (children always need at least -v to output STATS)
	if m.config.Verbosity >= 2 {
		args = append(args, "-vv")
	} else {
		args = append(args, "-v")
	}

	// Don't pass --stats-file to children; parent aggregates and writes combined file

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start instance: %w", err)
	}

	m.mu.Lock()
	m.processes[idx] = cmd
	m.mu.Unlock()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Printf("[instance-%d] %s\n", idx, scanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		m.parseInstanceOutput(idx, line)
	}

	return cmd.Wait()
}

// parseInstanceOutput processes output from a subprocess instance
func (m *MultiService) parseInstanceOutput(idx int, line string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := m.instanceStats[idx]

	if strings.Contains(line, "[OK] Connected to Psiphon network") {
		stats.IsLive = true
		fmt.Printf("[instance-%d] Connected to Psiphon network\n", idx)
		return
	}

	if strings.Contains(line, "[STATS]") {
		m.parseStatsLine(stats, line)
		return
	}

	if m.config.Verbosity >= 1 {
		fmt.Printf("[instance-%d] %s\n", idx, line)
	}
}

func (m *MultiService) parseStatsLine(stats *InstanceStats, line string) {
	connectingRe := regexp.MustCompile(`Connecting:\s*(\d+)`)
	connectedRe := regexp.MustCompile(`Connected:\s*(\d+)`)
	upRe := regexp.MustCompile(`Up:\s*([\d.]+)\s*([KMGTPE]?B)`)
	downRe := regexp.MustCompile(`Down:\s*([\d.]+)\s*([KMGTPE]?B)`)

	if match := connectingRe.FindStringSubmatch(line); len(match) > 1 {
		if v, err := strconv.Atoi(match[1]); err == nil {
			stats.Connecting = v
		}
	}
	if match := connectedRe.FindStringSubmatch(line); len(match) > 1 {
		if v, err := strconv.Atoi(match[1]); err == nil {
			stats.Connected = v
		}
	}
	if match := upRe.FindStringSubmatch(line); len(match) > 2 {
		stats.BytesUp = parseByteValue(match[1], match[2])
	}
	if match := downRe.FindStringSubmatch(line); len(match) > 2 {
		stats.BytesDown = parseByteValue(match[1], match[2])
	}
}

// parseByteValue converts a human-readable byte string to int64
func parseByteValue(numStr, unit string) int64 {
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}

	multipliers := map[string]float64{
		"B":  1,
		"KB": 1024,
		"MB": 1024 * 1024,
		"GB": 1024 * 1024 * 1024,
		"TB": 1024 * 1024 * 1024 * 1024,
		"PB": 1024 * 1024 * 1024 * 1024 * 1024,
		"EB": 1024 * 1024 * 1024 * 1024 * 1024 * 1024,
	}

	if mult, ok := multipliers[unit]; ok {
		return int64(val * mult)
	}
	return int64(val)
}

// aggregateAndPrintStats periodically prints combined stats from all instances
func (m *MultiService) aggregateAndPrintStats(ctx context.Context) {
	defer close(m.statsDone)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final stats write on shutdown
			m.printAndWriteStats()
			return
		case <-ticker.C:
			m.printAndWriteStats()
		}
	}
}

// printAndWriteStats aggregates, prints, and optionally writes stats to file
func (m *MultiService) printAndWriteStats() {
	m.mu.Lock()
	defer m.mu.Unlock()

	var liveCount, totalConnecting, totalConnected int
	var totalUp, totalDown int64

	instances := make([]InstanceJSON, m.numInstances)
	for i, stats := range m.instanceStats {
		if stats.IsLive {
			liveCount++
		}
		totalConnecting += stats.Connecting
		totalConnected += stats.Connected
		totalUp += stats.BytesUp
		totalDown += stats.BytesDown

		instances[i] = InstanceJSON{
			ID:         stats.ID,
			IsLive:     stats.IsLive,
			Connecting: stats.Connecting,
			Connected:  stats.Connected,
			BytesUp:    stats.BytesUp,
			BytesDown:  stats.BytesDown,
		}
	}

	uptime := time.Since(m.startTime).Truncate(time.Second)

	fmt.Printf("%s [AGGREGATE] Live: %d/%d | Connecting: %d | Connected: %d | Up: %s | Down: %s | Uptime: %s\n",
		time.Now().Format("2006-01-02 15:04:05"),
		liveCount,
		m.numInstances,
		totalConnecting,
		totalConnected,
		formatBytes(totalUp),
		formatBytes(totalDown),
		formatDuration(uptime),
	)

	if m.config.StatsFile != "" {
		statsJSON := AggregateStatsJSON{
			LiveInstances:     liveCount,
			TotalInstances:    m.numInstances,
			ConnectingClients: totalConnecting,
			ConnectedClients:  totalConnected,
			TotalBytesUp:      totalUp,
			TotalBytesDown:    totalDown,
			UptimeSeconds:     int64(uptime.Seconds()),
			Timestamp:         time.Now().Format(time.RFC3339),
			Instances:         instances,
		}

		data, err := json.MarshalIndent(statsJSON, "", "  ")
		if err != nil {
			fmt.Printf("[ERROR] Failed to marshal stats: %v\n", err)
			return
		}

		if err := os.WriteFile(m.config.StatsFile, data, 0644); err != nil {
			fmt.Printf("[ERROR] Failed to write stats file %s: %v\n", m.config.StatsFile, err)
		} else if m.config.Verbosity >= 2 {
			fmt.Printf("[DEBUG] Wrote stats to %s\n", m.config.StatsFile)
		}
	}
}

// Stop gracefully shuts down all instances
func (m *MultiService) Stop() {
	if m.cancel != nil {
		m.cancel()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for i, cmd := range m.processes {
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Signal(os.Interrupt)
			m.processes[i] = nil
		}
	}
}

// CalculateInstances determines how many instances to run based on max clients
func CalculateInstances(maxClients int) int {
	instances := maxClients / ClientsPerInstance
	if instances < 1 {
		instances = 1
	}
	// Cap at reasonable maximum
	if instances > 10 {
		instances = 10
	}
	return instances
}
