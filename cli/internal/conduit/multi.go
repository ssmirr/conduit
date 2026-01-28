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
	"io"
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
	ClientsPerInstance = 50
	// MaxInstances is the maximum number of instances allowed
	MaxInstances = 32
	// BytesPerSecondToMbps converts bytes per second to megabits per second
	BytesPerSecondToMbps = 1000 * 1000 / 8
	// MaxRestarts is the maximum number of times an instance can restart
	MaxRestarts = 5
	// RestartBackoff is the delay between restart attempts
	RestartBackoff = 5 * time.Second
	// IdleTimeout is how long an instance can be idle before automatic restart
	IdleTimeout = 1 * time.Hour
	// ShutdownTimeout is the grace period before force-killing child processes
	ShutdownTimeout = 2 * time.Second
)

// Compile regexes once at package initialization for performance
var (
	connectingRe = regexp.MustCompile(`Connecting:\s*(\d+)`)
	connectedRe  = regexp.MustCompile(`Connected:\s*(\d+)`)
	upRe         = regexp.MustCompile(`Up:\s*([\d.]+)\s*([KMGTPE]?B)`)
	downRe       = regexp.MustCompile(`Down:\s*([\d.]+)\s*([KMGTPE]?B)`)
)

// Byte unit multipliers for parsing human-readable byte values
var byteMultipliers = map[string]float64{
	"B":  1,
	"KB": 1024,
	"MB": 1024 * 1024,
	"GB": 1024 * 1024 * 1024,
	"TB": 1024 * 1024 * 1024 * 1024,
	"PB": 1024 * 1024 * 1024 * 1024 * 1024,
	"EB": 1024 * 1024 * 1024 * 1024 * 1024 * 1024,
}

// InstanceStats tracks stats for a single instance
type InstanceStats struct {
	ID           string
	IsLive       bool
	Connecting   int
	Connected    int
	BytesUp      int64
	BytesDown    int64
	RestartCount int       // Number of times this instance has been restarted
	LastZeroTime time.Time // Last time Connected was 0 (for idle timeout detection)
}

// MultiService manages multiple conduit subprocess instances
type MultiService struct {
	config        *config.Config
	numInstances  int
	processes     []*exec.Cmd
	instanceStats []*InstanceStats
	cancel        context.CancelFunc
	mu            sync.Mutex
	wg            sync.WaitGroup // Tracks all goroutines (instance restarts + I/O readers)
	startTime     time.Time
	statsDone     chan struct{}
	statsChanged  chan struct{} // Signals when stats have changed
}

// AggregateStatsJSON represents the JSON structure for multi-instance stats
type AggregateStatsJSON struct {
	LiveInstances     int            `json:"liveInstances"`
	TotalInstances    int            `json:"totalInstances"`
	ConnectingClients int            `json:"connectingClients"`
	ConnectedClients  int            `json:"connectedClients"`
	TotalBytesUp      int64          `json:"totalBytesUp"`
	TotalBytesDown    int64          `json:"totalBytesDown"`
	TotalRestarts     int            `json:"totalRestarts"`
	UptimeSeconds     int64          `json:"uptimeSeconds"`
	Timestamp         string         `json:"timestamp"`
	Instances         []InstanceJSON `json:"instances,omitempty"`
}

// InstanceJSON represents per-instance stats in JSON
type InstanceJSON struct {
	ID           string `json:"id"`
	IsLive       bool   `json:"isLive"`
	Connecting   int    `json:"connecting"`
	Connected    int    `json:"connected"`
	BytesUp      int64  `json:"bytesUp"`
	BytesDown    int64  `json:"bytesDown"`
	RestartCount int    `json:"restartCount"`
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
		statsChanged:  make(chan struct{}, 100), // Buffered to avoid blocking
	}, nil
}

// Run starts all subprocess instances and monitors them
func (m *MultiService) Run(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)

	clientsPerInstance := max(m.config.MaxClients/m.numInstances, 1)

	var bandwidthPerInstance float64
	if m.config.BandwidthBytesPerSecond > 0 {
		bandwidthPerInstance = float64(m.config.BandwidthBytesPerSecond) / float64(m.numInstances)
		bandwidthPerInstance = bandwidthPerInstance / BytesPerSecondToMbps // Convert to Mbps
	} else {
		bandwidthPerInstance = -1
	}

	bandwidthStr := "unlimited"
	if bandwidthPerInstance > 0 {
		bandwidthStr = fmt.Sprintf("%.0f Mbps/instance", bandwidthPerInstance)
	}
	fmt.Printf("Starting %d Psiphon Conduit instances (Max Clients/instance: %d, Bandwidth: %s)\n",
		m.numInstances, clientsPerInstance, bandwidthStr)

	errChan := make(chan error, m.numInstances)

	for i := 0; i < m.numInstances; i++ {
		instanceDataDir := filepath.Join(m.config.DataDir, fmt.Sprintf("%d", i))

		if err := os.MkdirAll(instanceDataDir, 0700); err != nil {
			return fmt.Errorf("failed to create instance directory: %w", err)
		}

		m.wg.Add(1)
		go func(idx int, dataDir string) {
			defer m.wg.Done()
			restartCount := 0

			for {
				err := m.runInstance(ctx, idx, dataDir, clientsPerInstance, bandwidthPerInstance)

				// Check if this was a clean shutdown (context cancelled)
				if ctx.Err() != nil {
					return
				}

				// Instance crashed unexpectedly
				restartCount++

				// Update restart count in stats
				m.mu.Lock()
				m.instanceStats[idx].RestartCount = restartCount
				m.instanceStats[idx].IsLive = false
				m.mu.Unlock()

				if restartCount >= MaxRestarts {
					fmt.Printf("[instance-%d] Reached max restarts (%d), giving up\n", idx, MaxRestarts)
					if err != nil {
						errChan <- fmt.Errorf("instance-%d exceeded max restarts: %w", idx, err)
					}
					return
				}

				fmt.Printf("[instance-%d] Crashed (restart %d/%d), restarting in %v...\n",
					idx, restartCount, MaxRestarts, RestartBackoff)

				time.Sleep(RestartBackoff)
			}
		}(i, instanceDataDir)

		fmt.Printf("[instance-%d] Starting with data dir: %s\n", i, instanceDataDir)
	}

	go m.aggregateAndPrintStats(ctx)

	m.wg.Wait()

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

	// Pass through verbosity from parent to children
	for i := 0; i < m.config.Verbosity; i++ {
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

	// Monitor context cancellation for graceful shutdown with timeout
	// CommandContext will signal the child when ctx is cancelled, but we also
	// force-kill after ShutdownTimeout if it hasn't exited yet
	go func() {
		<-ctx.Done()
		// Give process time to exit gracefully after receiving signal
		time.Sleep(ShutdownTimeout)
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			// Force kill if still running after grace period
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	}()

	// Stream stderr with prefix in background
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		scanner := newLargeBufferScanner(stderr)
		for scanner.Scan() {
			fmt.Fprintf(os.Stderr, "[instance-%d] %s\n", idx, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "[instance-%d] %v\n", idx, err)
		}
	}()

	// Stream stdout and parse for stats
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		scanner := newLargeBufferScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			m.parseInstanceOutput(idx, line)
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "[instance-%d] %v\n", idx, err)
		}
	}()

	// Wait for process to exit
	return cmd.Wait()
}

// parseInstanceOutput processes output from a subprocess instance
func (m *MultiService) parseInstanceOutput(idx int, line string) {
	var changed bool

	m.mu.Lock()
	stats := m.instanceStats[idx]

	// Always show "Connected to Psiphon network" events (important milestone)
	if strings.Contains(line, "[OK] Connected to Psiphon network") {
		stats.IsLive = true
		fmt.Printf("[instance-%d] Connected to Psiphon network\n", idx)
		m.mu.Unlock()
		return
	}

	// Parse stats lines for aggregation, but only print per-instance stats in verbose mode
	if strings.Contains(line, "[STATS]") {
		changed = m.parseStatsLine(stats, line)
		// Only show individual instance stats if verbose
		if m.config.Verbosity >= 1 {
			fmt.Printf("[instance-%d] %s\n", idx, line)
		}

		m.mu.Unlock() // unlock before sending the signal to statsChanged

		if changed {
			select {
			case m.statsChanged <- struct{}{}:
			default:
			}
		}
	} else {
		// All other output only shown in verbose mode
		if m.config.Verbosity >= 1 {
			fmt.Printf("[instance-%d] %s\n", idx, line)
		}

		m.mu.Unlock()
	}
}

func (m *MultiService) parseStatsLine(stats *InstanceStats, line string) bool {
	changed := false

	if match := connectingRe.FindStringSubmatch(line); len(match) > 1 {
		if v, err := strconv.Atoi(match[1]); err == nil {
			if stats.Connecting != v {
				stats.Connecting = v
				changed = true
			}
		}
	}
	if match := connectedRe.FindStringSubmatch(line); len(match) > 1 {
		if v, err := strconv.Atoi(match[1]); err == nil {
			if stats.Connected != v {
				stats.Connected = v
				changed = true
			}
		}
	}
	if match := upRe.FindStringSubmatch(line); len(match) > 2 {
		newVal := parseByteValue(match[1], match[2])
		if stats.BytesUp != newVal {
			stats.BytesUp = newVal
			changed = true
		}
	}
	if match := downRe.FindStringSubmatch(line); len(match) > 2 {
		newVal := parseByteValue(match[1], match[2])
		if stats.BytesDown != newVal {
			stats.BytesDown = newVal
			changed = true
		}
	}

	return changed
}

// parseByteValue converts a human-readable byte string to int64
func parseByteValue(numStr, unit string) int64 {
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}

	if mult, ok := byteMultipliers[unit]; ok {
		return int64(val * mult)
	}
	return int64(val)
}

// aggregateAndPrintStats prints combined stats when changes occur
func (m *MultiService) aggregateAndPrintStats(ctx context.Context) {
	defer close(m.statsDone)

	for {
		select {
		case <-ctx.Done():
			// Final stats write on shutdown
			m.printAndWriteStats()
			return
		case <-m.statsChanged:
			m.printAndWriteStats()
		}
	}
}

// printAndWriteStats aggregates, prints, and optionally writes stats to file
func (m *MultiService) printAndWriteStats() {
	// Copy data under lock, then release before I/O
	m.mu.Lock()

	var liveCount, totalConnecting, totalConnected, totalRestarts int
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
		totalRestarts += stats.RestartCount

		instances[i] = InstanceJSON{
			ID:           stats.ID,
			IsLive:       stats.IsLive,
			Connecting:   stats.Connecting,
			Connected:    stats.Connected,
			BytesUp:      stats.BytesUp,
			BytesDown:    stats.BytesDown,
			RestartCount: stats.RestartCount,
		}

		// Check for idle timeout: if instance has been at 0 connections for > 1 hour, restart it
		if stats.IsLive && stats.Connected == 0 {
			if stats.LastZeroTime.IsZero() {
				stats.LastZeroTime = time.Now()
			} else if time.Since(stats.LastZeroTime) > IdleTimeout {
				fmt.Printf("[instance-%d] Idle for %v with no connections, restarting...\n",
					i, time.Since(stats.LastZeroTime).Truncate(time.Second))
				if m.processes[i] != nil {
					m.processes[i].Process.Kill()
				}
				stats.LastZeroTime = time.Time{} // Reset timer
			}
		} else if stats.Connected > 0 {
			stats.LastZeroTime = time.Time{}
		}
	}

	uptime := time.Since(m.startTime).Truncate(time.Second)
	statsFile := m.config.StatsFile
	verbosity := m.config.Verbosity

	m.mu.Unlock()
	// Lock released - safe to do I/O operations now

	// Print aggregate stats to console
	restartInfo := ""
	if totalRestarts > 0 {
		restartInfo = fmt.Sprintf(" | Restarts: %d", totalRestarts)
	}
	fmt.Printf("[AGGREGATE] %s Live: %d/%d | Connecting: %d | Connected: %d | Up: %s | Down: %s | Uptime: %s%s\n",
		time.Now().Format("2006-01-02 15:04:05"),
		liveCount,
		m.numInstances,
		totalConnecting,
		totalConnected,
		formatBytes(totalUp),
		formatBytes(totalDown),
		formatDuration(uptime),
		restartInfo,
	)

	// Write stats to file if configured
	if statsFile != "" {
		statsJSON := AggregateStatsJSON{
			LiveInstances:     liveCount,
			TotalInstances:    m.numInstances,
			ConnectingClients: totalConnecting,
			ConnectedClients:  totalConnected,
			TotalBytesUp:      totalUp,
			TotalBytesDown:    totalDown,
			TotalRestarts:     totalRestarts,
			UptimeSeconds:     int64(uptime.Seconds()),
			Timestamp:         time.Now().Format(time.RFC3339),
			Instances:         instances,
		}

		data, err := json.MarshalIndent(statsJSON, "", "  ")
		if err != nil {
			fmt.Printf("[ERROR] Failed to marshal stats: %v\n", err)
			return
		}

		if err := os.WriteFile(statsFile, data, 0644); err != nil {
			fmt.Printf("[ERROR] Failed to write stats file %s: %v\n", statsFile, err)
		} else if verbosity >= 2 {
			fmt.Printf("[DEBUG] Wrote stats to %s\n", statsFile)
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

// newLargeBufferScanner creates a scanner with increased buffer size to handle long lines
func newLargeBufferScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	// Increase buffer size to handle long lines (up to 1MB)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	return scanner
}

// CalculateInstances determines how many instances to run based on max clients
func CalculateInstances(maxClients int) int {
	instances := maxClients / ClientsPerInstance
	if instances < 1 {
		instances = 1
	}
	if instances > MaxInstances {
		instances = MaxInstances
	}
	return instances
}
