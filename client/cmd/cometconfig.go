package cmd

import (
	"context"
	"path/filepath"
	"time"

	cfg "github.com/cometbft/cometbft/config"
	"github.com/spf13/viper"

	"github.com/piplabs/story/lib/errors"
	"github.com/piplabs/story/lib/log"
)

//nolint:gochecknoglobals // Overrides cometbft default moniker for testing.
var testMoniker string

// DefaultCometConfig returns the default cometBFT config.
func DefaultCometConfig(homeDir string) cfg.Config {
	conf := cfg.DefaultConfig()

	if testMoniker != "" {
		conf.Moniker = testMoniker
	}

	conf.RootDir = homeDir
	conf.SetRoot(conf.RootDir)
	conf.LogLevel = "error"                                // Decrease default comet log level, it is super noisy.
	conf.TxIndex = &cfg.TxIndexConfig{Indexer: "null"}     // Disable tx indexing.
	conf.StateSync.DiscoveryTime = time.Second * 10        // Increase discovery time
	conf.StateSync.ChunkRequestTimeout = time.Minute       // Increase timeout
	conf.Mempool.Type = cfg.MempoolTypeNop                 // Disable cometBFT mempool
	conf.Mempool.Broadcast = false                         // Disable mempool broadcast
	conf.Consensus.TimeoutCommit = 1500 * time.Millisecond // Increase block time

	return *conf
}

// parseCometConfig parses the cometBFT config from disk and verifies it.
func parseCometConfig(ctx context.Context, homeDir string) (cfg.Config, error) {
	const (
		file = "config" // CometBFT config files are named config.toml
		dir  = "config" // CometBFT config files are stored in the config directory
	)

	v := viper.New()
	v.SetConfigName(file)
	v.AddConfigPath(filepath.Join(homeDir, dir))

	// Attempt to read the cometBFT config file, gracefully ignoring errors
	// caused by a config file not being found. Return an error
	// if we cannot parse the config file.
	if err := v.ReadInConfig(); err != nil {
		// It's okay if there isn't a config file
		var cfgError viper.ConfigFileNotFoundError
		if ok := errors.As(err, &cfgError); !ok {
			return cfg.Config{}, errors.Wrap(err, "read comet config")
		}

		log.Warn(ctx, "No comet config.toml file found, using default config", nil)
	}

	conf := DefaultCometConfig(homeDir)

	if err := v.Unmarshal(&conf); err != nil {
		return cfg.Config{}, errors.Wrap(err, "unmarshal comet config")
	}

	if err := conf.ValidateBasic(); err != nil {
		return cfg.Config{}, errors.Wrap(err, "validate comet config")
	}

	if warnings := conf.CheckDeprecated(); len(warnings) > 0 {
		for _, warning := range warnings {
			log.Info(ctx, "Deprecated CometBFT config", "usage", warning)
		}
	}

	return conf, nil
}
