package main

import (
	"fmt"

	"github.com/mike76-dev/sia-satellite/node/api"
	"github.com/spf13/cobra"

	"gitlab.com/NebulousLabs/errors"
)

var (
	consensusCmd = &cobra.Command{
		Use:   "consensus",
		Short: "Print the current state of consensus",
		Long:  "Print the current state of consensus such as current block, block height, and target.",
		Run:   wrap(consensuscmd),
	}
)

// consensuscmd is the handler for the command `satc consensus`.
// Prints the current state of consensus.
func consensuscmd() {
	cg, err := httpClient.ConsensusGet()
	if errors.Contains(err, api.ErrAPICallNotRecognized) {
		// Assume module is not loaded if status command is not recognized.
		fmt.Printf("Consensus:\n  Status: %s\n\n", moduleNotReadyStatus)
		return
	} else if err != nil {
		die("Could not get current consensus state:", err)
	}

	if cg.Synced {
		fmt.Printf(`Synced: %v
Block:      %v
Height:     %v
Target:     %v
Difficulty: %v
`, yesNo(cg.Synced), cg.CurrentBlock, cg.Height, cg.Target, cg.Difficulty)
	} else {
		fmt.Printf(`Synced: %v
Height: %v
`, yesNo(cg.Synced), cg.Height)
	}
}
