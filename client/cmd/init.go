package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	cmtconfig "github.com/cometbft/cometbft/config"
	k1 "github.com/cometbft/cometbft/crypto/secp256k1"
	cmtos "github.com/cometbft/cometbft/libs/os"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/privval"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/spf13/cobra"

	iliadcfg "github.com/piplabs/story/client/config"
	libcmd "github.com/piplabs/story/lib/cmd"
	"github.com/piplabs/story/lib/errors"
	"github.com/piplabs/story/lib/log"
	"github.com/piplabs/story/lib/netconf"
)

// InitConfig is the config for the init command.
type InitConfig struct {
	HomeDir       string
	Network       netconf.ID
	TrustedSync   bool
	Force         bool
	Clean         bool
	Cosmos        bool
	ExecutionHash common.Hash
}

// newInitCmd returns a new cobra command that initializes the files and folders required by iliad.
func newInitCmd() *cobra.Command {
	// Default config flags
	cfg := InitConfig{
		HomeDir: iliadcfg.DefaultHomeDir(),
		Force:   false,
	}

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initializes required iliad files and directories",
		Long: `Initializes required iliad files and directories.

Ensures all the following files and directories exist:
  <home>/                            # Iliad home directory
  ├── config                         # Config directory
  │   ├── config.toml                # CometBFT configuration
  │   ├── genesis.json               # Iliad chain genesis file
  │   ├── iliad.toml                  # Iliad configuration
  │   ├── node_key.json              # Node P2P identity key
  │   └── priv_validator_key.json    # CometBFT private validator key (back this up and keep it safe)
  ├── data                           # Data directory
  │   ├── snapshots                  # Snapshot directory
  │   ├── priv_validator_state.json  # CometBFT private validator state (slashing protection)

Existing files are not overwritten, unless --clean is specified.
The home directory should only contain subdirectories, no files, use --force to ignore this check.
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := libcmd.LogFlags(ctx, cmd.Flags()); err != nil {
				return err
			}

			return InitFiles(cmd.Context(), cfg)
		},
	}

	bindInitFlags(cmd.Flags(), &cfg)

	return cmd
}

// InitFiles initializes the files and folders required by iliad.
// It ensures a network and genesis file is generated/downloaded for the provided network.
//
//nolint:gocognit,nestif // This is just many sequential steps.
func InitFiles(ctx context.Context, initCfg InitConfig) error {
	if initCfg.Network == "" {
		return errors.New("required flag --network empty")
	}

	log.Info(ctx, "Initializing iliad files and directories")
	homeDir := initCfg.HomeDir
	network := initCfg.Network

	if err := prepareHomeDirectory(ctx, initCfg, homeDir); err != nil {
		return err
	}

	// Initialize default configs.
	comet := DefaultCometConfig(homeDir)

	var cfg iliadcfg.Config

	switch {
	case network == netconf.Iliad:
		cfg = iliadcfg.IliadConfig
	case network == netconf.Local:
		cfg = iliadcfg.LocalConfig
	default:
		cfg = iliadcfg.DefaultConfig()
		cfg.HomeDir = homeDir
		cfg.Network = network
	}

	// Folders
	folders := []struct {
		Name string
		Path string
	}{
		{"home", homeDir},
		{"data", filepath.Join(homeDir, cmtconfig.DefaultDataDir)},
		{"config", filepath.Join(homeDir, cmtconfig.DefaultConfigDir)},
		{"comet db", comet.DBDir()},
		{"snapshot", cfg.SnapshotDir()},
		{"app db", cfg.AppStateDir()},
	}
	for _, folder := range folders {
		if cmtos.FileExists(folder.Path) {
			// Dir exists, just skip
			continue
		} else if err := cmtos.EnsureDir(folder.Path, 0o755); err != nil {
			return errors.Wrap(err, "create folder")
		}
		log.Info(ctx, "Generated folder", "reason", folder.Name, "path", folder.Path)
	}

	// Add P2P seeds to comet config
	if seeds := network.Static().ConsensusSeeds(); len(seeds) > 0 {
		comet.P2P.Seeds = strings.Join(seeds, ",")
	}

	// Setup comet config
	cmtConfigFile := filepath.Join(homeDir, cmtconfig.DefaultConfigDir, cmtconfig.DefaultConfigFileName)
	if cmtos.FileExists(cmtConfigFile) {
		log.Info(ctx, "Found comet config file", "path", cmtConfigFile)
	} else {
		cmtconfig.WriteConfigFile(cmtConfigFile, &comet) // This panics on any error :(
		log.Info(ctx, "Generated default comet config file", "path", cmtConfigFile)
	}

	// Setup iliad config
	iliadConfigFile := cfg.ConfigFile()
	if cmtos.FileExists(iliadConfigFile) {
		log.Info(ctx, "Found iliad config file", "path", iliadConfigFile)
	} else if err := iliadcfg.WriteConfigTOML(cfg, log.DefaultConfig()); err != nil {
		return err
	} else {
		log.Info(ctx, "Generated default iliad config file", "path", iliadConfigFile)
	}

	// Setup comet private validator
	var pv *privval.FilePV
	privValKeyFile := comet.PrivValidatorKeyFile()
	privValStateFile := comet.PrivValidatorStateFile()
	if cmtos.FileExists(privValKeyFile) {
		pv = privval.LoadFilePV(privValKeyFile, privValStateFile) // This hard exits on any error.
		log.Info(ctx, "Found cometBFT private validator",
			"key_file", privValKeyFile,
			"state_file", privValStateFile,
		)
	} else {
		pv = privval.NewFilePV(k1.GenPrivKey(), privValKeyFile, privValStateFile)
		pv.Save()
		log.Info(ctx, "Generated private validator",
			"key_file", privValKeyFile,
			"state_file", privValStateFile)
	}

	// Setup node key
	nodeKeyFile := comet.NodeKeyFile()
	if cmtos.FileExists(nodeKeyFile) {
		log.Info(ctx, "Found node key", "path", nodeKeyFile)
	} else if _, err := p2p.LoadOrGenNodeKey(nodeKeyFile); err != nil {
		return errors.Wrap(err, "load or generate node key")
	} else {
		log.Info(ctx, "Generated node key", "path", nodeKeyFile)
	}

	// Setup genesis file
	genFile := comet.GenesisFile()
	if cmtos.FileExists(genFile) {
		log.Info(ctx, "Found genesis file", "path", genFile)
	} else if len(network.Static().ConsensusGenesisJSON) > 0 {
		if err := os.WriteFile(genFile, network.Static().ConsensusGenesisJSON, 0o644); err != nil {
			return errors.Wrap(err, "failed to write genesis file")
		}
		pubKey, err := pv.GetPubKey()
		if err != nil {
			return errors.Wrap(err, "failed to get public key")
		}

		// Derive the various addresses from the public key
		accAddr := sdk.AccAddress(pubKey.Address().Bytes()).String()
		valAddr := sdk.ValAddress(pubKey.Address().Bytes()).String()
		pubKeyBase64 := base64.StdEncoding.EncodeToString(pubKey.Bytes())
		fmt.Println("Base64 Encoded Public Key:", pubKeyBase64)

		genesisJSON := string(network.Static().ConsensusGenesisJSON)
		genesisJSON = strings.ReplaceAll(genesisJSON, "{{LOCAL_ACCOUNT_ADDRESS}}", accAddr)
		genesisJSON = strings.ReplaceAll(genesisJSON, "{{LOCAL_VALIDATOR_ADDRESS}}", valAddr)
		genesisJSON = strings.ReplaceAll(genesisJSON, "{{LOCAL_VALIDATOR_KEY}}", pubKeyBase64)

		err = os.WriteFile(genFile, []byte(genesisJSON), 0o644)

		if err != nil {
			return errors.Wrap(err, "save genesis file")
		}
		log.Info(ctx, "Generated well-known network genesis file", "path", genFile)
	} else {
		return errors.New("network genesis file not supported yet", "network", network)
	}

	return nil
}

func checkHomeDir(homeDir string) error {
	files, _ := os.ReadDir(homeDir) // Ignore error, we'll just assume it's empty.
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		return errors.New("home directory contains unexpected file(s), use --force to initialize anyway",
			"home", homeDir, "example_file", file.Name())
	}

	return nil
}

func prepareHomeDirectory(ctx context.Context, initCfg InitConfig, homeDir string) error {
	if !initCfg.Force {
		log.Info(ctx, "Ensuring provided home folder does not contain files, since --force=true")
		if err := checkHomeDir(homeDir); err != nil {
			return err
		}
	}

	if initCfg.Clean {
		log.Info(ctx, "Deleting home directory, since --clean=true")
		if err := os.RemoveAll(homeDir); err != nil {
			return errors.Wrap(err, "remove home dir")
		}
	}

	return nil
}
