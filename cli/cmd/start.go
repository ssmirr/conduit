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

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Psiphon-Inc/conduit/cli/internal/conduit"
	"github.com/Psiphon-Inc/conduit/cli/internal/config"
	"github.com/spf13/cobra"
)

var (
	maxClients        int
	bandwidthMbps     float64
	psiphonConfigPath string
	statsFilePath     string
	multiInstance     bool
)

const clientsPerInstance = 100

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Conduit inproxy service",
	Long:  getStartLongHelp(),
	RunE:  runStart,
}

func getStartLongHelp() string {
	if config.HasEmbeddedConfig() {
		return `Start the Conduit inproxy service to relay traffic for users in censored regions.

Use --multi-instance to automatically run multiple instances based on max-clients
(1 instance per 100 clients). Each instance gets its own key and reputation.`
	}
	return `Start the Conduit inproxy service to relay traffic for users in censored regions.

Requires a Psiphon network configuration file (JSON) containing the
PropagationChannelId, SponsorId, and broker specifications.

Use --multi-instance to automatically run multiple instances based on max-clients
(1 instance per 100 clients). Each instance gets its own key and reputation.`
}

func init() {
	rootCmd.AddCommand(startCmd)

	startCmd.Flags().IntVarP(&maxClients, "max-clients", "m", config.DefaultMaxClients, "maximum number of proxy clients (1-1000)")
	startCmd.Flags().Float64VarP(&bandwidthMbps, "bandwidth", "b", config.DefaultBandwidthMbps, "total bandwidth limit in Mbps (-1 for unlimited)")
	startCmd.Flags().StringVarP(&statsFilePath, "stats-file", "s", "", "persist stats to JSON file (default: stats.json in data dir if flag used without value)")
	startCmd.Flags().Lookup("stats-file").NoOptDefVal = "stats.json"
	startCmd.Flags().BoolVar(&multiInstance, "multi-instance", false, "run multiple instances (1 per 100 max-clients)")

	// Only show --psiphon-config flag if no config is embedded
	if !config.HasEmbeddedConfig() {
		startCmd.Flags().StringVarP(&psiphonConfigPath, "psiphon-config", "c", "", "path to Psiphon network config file (JSON)")
	}
}

func runStart(cmd *cobra.Command, args []string) error {
	// Determine psiphon config source: flag > embedded > error
	effectiveConfigPath := psiphonConfigPath
	useEmbedded := false

	if psiphonConfigPath != "" {
		// User provided a config path - validate it exists
		if _, err := os.Stat(psiphonConfigPath); os.IsNotExist(err) {
			return fmt.Errorf("psiphon config file not found: %s", psiphonConfigPath)
		}
	} else if config.HasEmbeddedConfig() {
		// No flag provided, but we have embedded config
		useEmbedded = true
	} else {
		// No flag and no embedded config
		return fmt.Errorf("psiphon config required: use --psiphon-config flag or build with embedded config")
	}

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		cancel()
	}()

	// Run in multi-instance or single-instance mode
	if multiInstance {
		return runMultiInstance(ctx, effectiveConfigPath, useEmbedded)
	}
	return runSingleInstance(ctx, effectiveConfigPath, useEmbedded)
}

// runSingleInstance runs the original single-instance mode
func runSingleInstance(ctx context.Context, configPath string, useEmbedded bool) error {
	// Resolve stats file path - if relative, place in data dir
	resolvedStatsFile := statsFilePath
	if resolvedStatsFile != "" && !filepath.IsAbs(resolvedStatsFile) {
		resolvedStatsFile = filepath.Join(GetDataDir(), resolvedStatsFile)
	}

	// Load or create configuration (auto-generates keys on first run)
	cfg, err := config.LoadOrCreate(config.Options{
		DataDir:           GetDataDir(),
		PsiphonConfigPath: configPath,
		UseEmbeddedConfig: useEmbedded,
		MaxClients:        maxClients,
		BandwidthMbps:     bandwidthMbps,
		Verbosity:         Verbosity(),
		StatsFile:         resolvedStatsFile,
	})
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Create conduit service
	service, err := conduit.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to create conduit service: %w", err)
	}

	// Print startup message
	bandwidthStr := "unlimited"
	if bandwidthMbps != config.UnlimitedBandwidth {
		bandwidthStr = fmt.Sprintf("%.0f Mbps", bandwidthMbps)
	}
	fmt.Printf("Starting Psiphon Conduit (Max Clients: %d, Bandwidth: %s)\n", cfg.MaxClients, bandwidthStr)

	// Run the service
	if err := service.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("conduit service error: %w", err)
	}

	fmt.Println("Stopped.")
	return nil
}

// runMultiInstance runs multiple instances based on max-clients (1 per 100)
func runMultiInstance(ctx context.Context, configPath string, useEmbedded bool) error {
	// Calculate number of instances: ceil(maxClients / 100)
	instanceCount := (maxClients + clientsPerInstance - 1) / clientsPerInstance
	if instanceCount < 1 {
		instanceCount = 1
	}
	if instanceCount > 32 {
		instanceCount = 32
	}

	// Calculate clients per instance
	clientsPerInst := maxClients / instanceCount
	if clientsPerInst < 1 {
		clientsPerInst = 1
	}

	baseDataDir := GetDataDir()

	// Create instance configurations
	var instanceConfigs []*config.Config
	for i := 0; i < instanceCount; i++ {
		// Create config first to get the key, then use key hash for directory name
		tempDataDir := filepath.Join(baseDataDir, fmt.Sprintf("instance-%d", i))

		// Resolve stats file path for this instance
		var statsFile string
		if statsFilePath != "" {
			ext := filepath.Ext(statsFilePath)
			base := statsFilePath[:len(statsFilePath)-len(ext)]
			statsFile = filepath.Join(baseDataDir, fmt.Sprintf("%s-instance-%d%s", base, i, ext))
		}

		cfg, err := config.LoadOrCreate(config.Options{
			DataDir:           tempDataDir,
			PsiphonConfigPath: configPath,
			UseEmbeddedConfig: useEmbedded,
			MaxClients:        clientsPerInst,
			BandwidthMbps:     bandwidthMbps,
			Verbosity:         Verbosity(),
			StatsFile:         statsFile,
		})
		if err != nil {
			return fmt.Errorf("failed to create config for instance %d: %w", i, err)
		}

		// Rename directory to use key short hash
		keyHash := cfg.GetKeyShortHash()
		if keyHash != "" {
			newDataDir := filepath.Join(baseDataDir, keyHash)
			if tempDataDir != newDataDir {
				// Move if different and new doesn't exist
				if _, err := os.Stat(newDataDir); os.IsNotExist(err) {
					os.Rename(tempDataDir, newDataDir)
					cfg.DataDir = newDataDir
				}
			}
		}

		instanceConfigs = append(instanceConfigs, cfg)
	}

	// Create multi-instance service
	multiService, err := conduit.NewMultiService(instanceConfigs)
	if err != nil {
		return fmt.Errorf("failed to create multi-instance service: %w", err)
	}

	// Print startup message
	bandwidthStr := "unlimited"
	if bandwidthMbps != config.UnlimitedBandwidth {
		bandwidthStr = fmt.Sprintf("%.0f Mbps", bandwidthMbps)
	}
	fmt.Printf("Starting %d Psiphon Conduit instances (Max Clients/instance: %d, Bandwidth: %s)\n",
		instanceCount, clientsPerInst, bandwidthStr)

	// Run the multi-instance service
	if err := multiService.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("multi-instance service error: %w", err)
	}

	fmt.Println("All instances stopped.")
	return nil
}
