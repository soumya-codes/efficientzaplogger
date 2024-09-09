# Efficient Zap Logger
An in memory highly efficient logger over zap for applications that need extremely high logging throughput.

## Features
- Allows users to choose between console and file as the logging target.
- Implements a highly efficient logger over zap which logs to memory and is flushed async to a file.
- Allows users to configure the log rotation and log retention.
- Uses the buffer switch mechanism and lock free constructs to ensure minimal contention.
- The logger is designed to be used in high throughput applications where logging to file directly is not an option.

## Caution
To avoid log loss which is possible today with extremely high throughput, the current logger implementation needs to be enhanced by add buffering in between buffer switches .