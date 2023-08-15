package logger

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chainguard-dev/registry-redirect/pkg/syslogger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"knative.dev/pkg/logging"
)

type Config struct {
	Level     string
	Component string
	Protocol  string
	Address   string
}

// Convert custom logging config to Knative logging config
// only Level is processed
func (c *Config) customConfigToKnativeConfig() (*logging.Config, error) {
	var knativeCfg logging.Config
	bytes, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %v", err)
	}
	err = json.Unmarshal(bytes, &knativeCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal into knative config: %v", err)
	}
	return &knativeCfg, nil
}

// NewLogger creates a new logger with the given configuration.
func NewLogger(ctx context.Context, cfg *Config) (context.Context, *syslogger.SyslogWriter, error) {
	syslogWriter := syslogger.NewSyslogWriter(cfg.Level, cfg.Protocol, cfg.Address, cfg.Component)

	err := syslogWriter.Connect() // connect to syslog server
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to syslog server: %v", err)
	}

	// Convert custom logging config to Knative logging config
	knativeCfg, err := cfg.customConfigToKnativeConfig()
	if err != nil {
		return nil, nil, err
	}

	// Create a new Zap logger and atomic level from Knative logging config
	zapLogger, atomicLevel := logging.NewLoggerFromConfig(knativeCfg, cfg.Component)

	// Convert zapLogger from sugared logger to base logger, which is faster and not prone to some of the common mistakes that can be made with sugared logger.
	baseLogger := zapLogger.Desugar()

	// Wrap the Core of baseLogger with a Core that writes to both the original Core and a new Core.
	baseLogger = baseLogger.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		// Create a new Core that writes to our SyslogWriter, uses JSON encoding, and has the same level as our original logger.
		syslogCore := zapcore.NewCore(
			zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),

			zapcore.AddSync(syslogWriter), // write to SyslogWriter
			atomicLevel,                   // same level as the original logger
		)

		// Combine the original Core and our new Core. Logs written to the resulting Core will be written to both Cores.
		combinedCore := zapcore.NewTee(core, syslogCore)

		return combinedCore
	}))

	// Update context with our combined logger. The logger is "sugared" again because it's typically more convenient to use.
	ctx = logging.WithLogger(ctx, baseLogger.Sugar())

	return ctx, syslogWriter, nil
}
