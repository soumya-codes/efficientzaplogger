package main

import (
	"bufio"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// LoggerConfig holds the configuration for the logger
type LoggerConfig struct {
	OutputType       string        // "console" or "file"
	FilePath         string        // Path to the log file (for file output)
	MaxSize          int           // Maximum size in megabytes before log rotation
	MaxBackups       int           // Maximum number of old log files to keep
	MaxAge           int           // Maximum number of days to retain old log files
	Compress         bool          // Whether to compress/zip old log files
	LogLevel         string        // Logging level as a string
	BufferSize       int           // Size of the buffer in bytes
	FlushInterval    time.Duration // Interval for periodic flushing
	EnableCaller     bool          // Enable caller information in logs
	EnableStacktrace bool          // Enable stack trace for Error and above levels
}

// DefaultConfig returns a LoggerConfig with default values
func DefaultConfig() LoggerConfig {
	return LoggerConfig{
		OutputType:       "console",
		FilePath:         "app.log",
		MaxSize:          10,
		MaxBackups:       7,
		MaxAge:           28,
		Compress:         false,
		LogLevel:         "info",
		BufferSize:       5 * 1024 * 1024,
		FlushInterval:    5 * time.Second,
		EnableCaller:     true,
		EnableStacktrace: true,
	}
}

// doubleSyncerWithBuffer implements zapcore.WriteSyncer with double buffering
type doubleSyncerWithBuffer struct {
	bufWriters    [2]*bufio.Writer
	currentBuffer atomic.Pointer[bufio.Writer]
	syncer        zapcore.WriteSyncer
	mu            sync.Mutex
}

func newDoubleSyncerWithBuffer(ws zapcore.WriteSyncer, size int) *doubleSyncerWithBuffer {
	s := &doubleSyncerWithBuffer{
		bufWriters: [2]*bufio.Writer{
			bufio.NewWriterSize(ws, size),
			bufio.NewWriterSize(ws, size),
		},
		syncer: ws,
	}
	s.currentBuffer.Store(s.bufWriters[0])
	return s
}

func (s *doubleSyncerWithBuffer) Write(p []byte) (int, error) {
	current := s.currentBuffer.Load()

	// Check if writing p would cause the buffer to exceed capacity
	if current.Buffered()+len(p) > int(float64(current.Size())*0.9) {
		// Buffer would exceed 90% capacity, so sync first
		if err := s.Sync(); err != nil {
			return 0, fmt.Errorf("sync before write failed: %w", err)
		}
		// After sync, get the current buffer again as it might have changed
		current = s.currentBuffer.Load()
	}

	n, err := current.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffer write error: %w", err)
	}
	return n, nil
}

func (s *doubleSyncerWithBuffer) Sync() error {
	var bufferToFlush *bufio.Writer

	s.mu.Lock()
	current := s.currentBuffer.Load()
	var next *bufio.Writer
	if current == s.bufWriters[0] {
		next = s.bufWriters[1]
	} else {
		next = s.bufWriters[0]
	}
	s.currentBuffer.Store(next)
	bufferToFlush = current
	defer s.mu.Unlock()

	if err := bufferToFlush.Flush(); err != nil {
		return fmt.Errorf("failed to flush buffer: %w", err)
	}
	return s.syncer.Sync()
}

// Logger wraps zap.Logger with additional functionality
type Logger struct {
	*zap.Logger
	config LoggerConfig
	syncer zapcore.WriteSyncer
	stop   chan struct{}
	wg     sync.WaitGroup
}

// NewLogger creates a new Logger instance
func NewLogger(cfg LoggerConfig) (*Logger, error) {
	logLevel, err := zapcore.ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("invalid log level: %w", err)
	}

	var writeSyncer zapcore.WriteSyncer
	switch cfg.OutputType {
	case "console":
		writeSyncer = zapcore.AddSync(os.Stdout)
	case "file":
		if cfg.FilePath == "" {
			return nil, fmt.Errorf("file path is required for file output")
		}
		lumberjackLogger := &lumberjack.Logger{
			Filename:   cfg.FilePath,
			MaxSize:    cfg.MaxSize,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAge,
			Compress:   cfg.Compress,
		}
		writeSyncer = zapcore.AddSync(lumberjackLogger)
	default:
		return nil, fmt.Errorf("invalid output type: %s", cfg.OutputType)
	}

	if cfg.BufferSize > 0 {
		writeSyncer = newDoubleSyncerWithBuffer(writeSyncer, cfg.BufferSize)
	}

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		writeSyncer,
		logLevel,
	)

	logger := &Logger{
		Logger: zap.New(core),
		config: cfg,
		syncer: writeSyncer,
		stop:   make(chan struct{}),
	}

	if cfg.EnableCaller {
		logger.Logger = logger.Logger.WithOptions(zap.AddCaller())
	}

	if cfg.EnableStacktrace {
		logger.Logger = logger.Logger.WithOptions(zap.AddStacktrace(zapcore.ErrorLevel))
	}

	logger.startPeriodicFlusher()

	return logger, nil
}

func (l *Logger) startPeriodicFlusher() {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		ticker := time.NewTicker(l.config.FlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := l.syncer.Sync(); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to flush logger: %v\n", err)
				}
			case <-l.stop:
				return
			}
		}
	}()
}

// Sync flushes any buffered log entries
func (l *Logger) Sync() error {
	return l.syncer.Sync()
}

// Close stops the periodic flusher and syncs the logger
func (l *Logger) Close() error {
	//close(l.stop)
	//l.wg.Wait()
	return l.Sync()
}

func main() {
	// Create a file logger
	cfg := DefaultConfig()
	cfg.OutputType = "file"
	cfg.FilePath = "app.log"
	cfg.FlushInterval = 5 * time.Second
	fileLogger, _ := NewLogger(cfg)
	defer fileLogger.Close()

	// Create a console logger
	/*	consoleLogger, _ := NewLogger(LoggerConfig{
			OutputType:    "console",
			LogLevel:      "debug",
			FlushInterval: 10 * time.Second,
		})
		defer consoleLogger.Close()
	*/

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				fileLogger.Info("File log from goroutine", zap.Int("goroutine", id), zap.Int("count", j))
				//consoleLogger.Debug("Console log from goroutine", zap.Int("goroutine", id), zap.Int("count", j))
				time.Sleep(10 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()
}
