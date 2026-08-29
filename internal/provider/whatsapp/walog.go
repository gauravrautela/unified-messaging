package whatsapp

import (
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// slogAdapter forwards whatsmeow's own logger into slog. It is only wired in
// when the service logger has debug enabled: whatsmeow's lines can carry JIDs
// and stanza detail that the service's own redaction rules never emit, so
// this is a diagnostic aid for an operator who asked for it, not a default.
type slogAdapter struct {
	log *slog.Logger
}

func waLogger(log *slog.Logger) waLog.Logger {
	if log == nil || !log.Enabled(nil, slog.LevelDebug) {
		return waLog.Noop
	}
	return &slogAdapter{log: log.With("component", "whatsmeow")}
}

func (l *slogAdapter) Warnf(msg string, args ...any)  { l.log.Warn(fmt.Sprintf(msg, args...)) }
func (l *slogAdapter) Errorf(msg string, args ...any) { l.log.Error(fmt.Sprintf(msg, args...)) }
func (l *slogAdapter) Infof(msg string, args ...any)  { l.log.Info(fmt.Sprintf(msg, args...)) }
func (l *slogAdapter) Debugf(msg string, args ...any) { l.log.Debug(fmt.Sprintf(msg, args...)) }
func (l *slogAdapter) Sub(module string) waLog.Logger {
	return &slogAdapter{log: l.log.With("module", module)}
}
