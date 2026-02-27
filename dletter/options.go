package dletter

import "gopkg.in/natefinch/lumberjack.v2"

// Option defines a functional configuration option for the lumberjack logger.
type Option func(*lumberjack.Logger)

// WithMaxSize sets the maximum size in megabytes of the log file before it gets rotated.
func WithMaxSize(megabytes int) Option {
	return func(l *lumberjack.Logger) {
		l.MaxSize = megabytes
	}
}

// WithMaxBackups sets the maximum number of old log files to retain.
func WithMaxBackups(count int) Option {
	return func(l *lumberjack.Logger) {
		l.MaxBackups = count
	}
}

// WithMaxAge sets the maximum number of days to retain old log files based on the timestamp encoded in their filename.
func WithMaxAge(days int) Option {
	return func(l *lumberjack.Logger) {
		l.MaxAge = days
	}
}

// WithCompress determines if the rotated log files should be compressed using gzip.
func WithCompress(compress bool) Option {
	return func(l *lumberjack.Logger) {
		l.Compress = compress
	}
}
