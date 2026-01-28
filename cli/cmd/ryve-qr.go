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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"
)

// Structs for the Ryve payload
type ryvePayloadData struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}
type ryvePayload struct {
	Version int             `json:"version"`
	Data    ryvePayloadData `json:"data"`
}

const keyFileName = "conduit_key.json"

var nodeNameFlag string

var ryveQRCmd = &cobra.Command{
	Use:   "ryve-qr",
	Short: "Generate a QR code to link the node with the Ryve app",
	Long:  `Reads the node identity key, formats it for Ryve, and displays a QR code for linking with the Ryve mobile app.`,
	RunE:  runRyveQR,
}

func init() {
	rootCmd.AddCommand(ryveQRCmd)
	ryveQRCmd.Flags().StringVarP(&nodeNameFlag, "name", "n", "", "custom name for the node")
}

func runRyveQR(cmd *cobra.Command, args []string) error {
	keyPath := filepath.Join(GetDataDir(), keyFileName)

	keyJSON, err := os.ReadFile(keyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s not found in data directory: %s\nRun 'conduit start' once to generate a key.", keyFileName, GetDataDir())
		}
		return fmt.Errorf("failed to read key file: %w", err)
	}

	var pk map[string]string
	if err := json.Unmarshal(keyJSON, &pk); err != nil {
		return fmt.Errorf("failed to parse key file JSON: %w", err)
	}

	privateKey, ok := pk["privateKeyBase64"]
	if !ok || privateKey == "" {
		return fmt.Errorf("privateKeyBase64 not found in key file: %s", keyPath)
	}

	nodeName := nodeNameFlag
	if nodeName == "" {
		var err error
		nodeName, err = os.Hostname()
		if err != nil {
			nodeName = "MyConduitNode" // Fallback name
		}
	}

	payload := ryvePayload{
		Version: 1,
		Data: ryvePayloadData{
			Key:  privateKey,
			Name: nodeName,
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to create Ryve JSON payload: %w", err)
	}

	b64Claim := base64.StdEncoding.EncodeToString(payloadJSON)
	finalURL := fmt.Sprintf("network.ryve.app://(app)/conduits?claim=%s", b64Claim)

	fmt.Println("\nScan the QR code with the Ryve app to link your Conduit node:")

	config := qrterminal.Config{
		Level:     qrterminal.L,
		Writer:    os.Stdout,
		HalfBlocks: true,
		QuietZone: 2, 
	}
	
	qrterminal.GenerateWithConfig(finalURL, config)
	
	fmt.Println("") 
	return nil
}