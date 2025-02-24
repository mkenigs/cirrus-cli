package worker

import (
	"fmt"
	"github.com/cirruslabs/cirrus-cli/internal/executor/endpoint"
	"github.com/cirruslabs/cirrus-cli/internal/worker"
	"github.com/dustin/go-humanize"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/natefinch/lumberjack.v2"
	"io"
	"os"
	"strconv"
	"strings"
)

var (
	configPath string
	name       string
	token      string
	labels     map[string]string

	// RPC-related variables.
	rpcEndpointAddress string

	// Logging-related variables.
	logLevel        string
	logFile         string
	logRotateSize   string
	logMaxRotations uint

	// Resources-related variables.
	resources map[string]string
)

func loggingLevelsExplainer() string {
	var levels []string

	for _, level := range logrus.AllLevels {
		levels = append(levels, `"`+level.String()+`"`)
	}

	if len(levels) == 0 {
		panic("logging library reports no logging levels to use, this shouldn't normally happen; " +
			"please submit an issue to https://github.com/cirruslabs/cirrus-cli/issues/new")
	}

	if len(levels) == 1 {
		return fmt.Sprintf("for example %s", levels[0])
	}

	return fmt.Sprintf("either %s or %s", strings.Join(levels[:len(levels)-1], ", "), levels[len(levels)-1])
}

func attacheFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVarP(&configPath, "file", "f", "", "configuration file path (e.g. /etc/cirrus/worker.yml)")

	// Default worker name to host name
	defaultName, _ := os.Hostname()
	// Make the default name pretty by removing common suffixes
	defaultName = strings.TrimSuffix(defaultName, ".lan")
	defaultName = strings.TrimSuffix(defaultName, ".local")
	cmd.PersistentFlags().StringVar(&name, "name", defaultName, "worker name to use when registering in the pool")
	_ = viper.BindPFlag("name", cmd.PersistentFlags().Lookup("name"))

	cmd.PersistentFlags().StringVar(&token, "token", "", "pool registration token")
	_ = viper.BindPFlag("token", cmd.PersistentFlags().Lookup("token"))

	cmd.PersistentFlags().StringToStringVar(&labels, "labels", map[string]string{},
		"additional labels to use (e.g. --labels distro=debian)")
	_ = viper.BindPFlag("labels", cmd.PersistentFlags().Lookup("labels"))

	// RPC-related variables
	cmd.PersistentFlags().StringVar(&rpcEndpointAddress, "rpc-endpoint", worker.DefaultRPCEndpoint, "RPC endpoint address")
	_ = viper.BindPFlag("rpc.endpoint", cmd.PersistentFlags().Lookup("rpc-endpoint"))
	_ = cmd.PersistentFlags().MarkHidden("rpc-endpoint")

	// Logging-related variables
	cmd.PersistentFlags().StringVar(&logLevel, "log-level", logrus.InfoLevel.String(),
		fmt.Sprintf("logging level to use, %s", loggingLevelsExplainer()))
	_ = viper.BindPFlag("log.level", cmd.PersistentFlags().Lookup("log-level"))
	_ = cmd.PersistentFlags().MarkHidden("log-level")

	cmd.PersistentFlags().StringVar(&logFile, "log-file", "", "log to the specified file instead of terminal")
	_ = viper.BindPFlag("log.file", cmd.PersistentFlags().Lookup("log-file"))
	_ = cmd.PersistentFlags().MarkHidden("log-file")

	cmd.PersistentFlags().StringVar(&logRotateSize, "log-rotate-size", "",
		"rotate the log file if it reaches the specified size, e.g. \"640 KB\" or \"100 MiB\"")
	_ = viper.BindPFlag("log.rotate-size", cmd.PersistentFlags().Lookup("log-rotate-size"))
	_ = cmd.PersistentFlags().MarkHidden("log-rotate-size")

	cmd.PersistentFlags().UintVar(&logMaxRotations, "log-max-rotations", 0,
		"how many already rotated log files to keep")
	_ = viper.BindPFlag("log.max-rotations", cmd.PersistentFlags().Lookup("log-max-rotations"))
	_ = cmd.PersistentFlags().MarkHidden("log-max-rotations")

	// Resources-related variables
	cmd.PersistentFlags().StringToStringVar(&resources, "resources", map[string]string{},
		"user-defined resources available on this Persistent Worker")
	_ = viper.BindPFlag("resources", cmd.PersistentFlags().Lookup("resources"))
	_ = cmd.PersistentFlags().MarkHidden("resources")
}

func buildWorker(cmd *cobra.Command) (*worker.Worker, error) {
	if configPath != "" {
		viper.SetConfigType("yaml")
		viper.SetConfigFile(configPath)
		if err := viper.ReadInConfig(); err != nil {
			return nil, err
		}
	}

	// Convert resources from map[string]string to map[string]float64
	resources := map[string]float64{}

	for key, stringValue := range viper.GetStringMapString("resources") {
		floatValue, err := strconv.ParseFloat(stringValue, 64)
		if err != nil {
			return nil, err
		}

		resources[key] = floatValue
	}

	opts := []worker.Option{
		worker.WithName(viper.GetString("name")),
		worker.WithRegistrationToken(viper.GetString("token")),
		worker.WithLabels(viper.GetStringMapString("labels")),
		worker.WithResources(resources),
	}

	// Configure RPC server (used for testing)
	if rpcEndpointAddress != "" {
		opts = append(opts, worker.WithRPCEndpoint(rpcEndpointAddress))
		opts = append(opts, worker.WithAgentEndpoint(endpoint.NewRemote(rpcEndpointAddress)))
	}

	// Configure logging
	logger := logrus.New()

	level, err := logrus.ParseLevel(viper.GetString("log.level"))
	if err != nil {
		return nil, err
	}
	logger.SetLevel(level)

	var output io.Writer
	if viper.IsSet("log.file") {
		logRotateSizeBytes := uint64(0)
		if viper.IsSet("log.rotate-size") {
			logRotateSizeBytes, err = humanize.ParseBytes(viper.GetString("log.rotate-size"))
			if err != nil {
				return nil, fmt.Errorf("failed to parse log size for rotation: %w", err)
			}
		}

		output = &lumberjack.Logger{
			Filename:   viper.GetString("log.file"),
			MaxSize:    int(logRotateSizeBytes / humanize.MByte),
			MaxBackups: int(viper.GetUint("log.max-rotations")),
		}
	} else {
		output = cmd.ErrOrStderr()
	}
	logger.SetOutput(output)

	opts = append(opts, worker.WithLogger(logger))

	// Instantiate worker
	return worker.New(opts...)
}
