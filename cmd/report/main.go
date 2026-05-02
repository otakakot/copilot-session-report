// Command report writes a Markdown summary of a finished
// Copilot CLI session. Designed to be invoked from the Copilot CLI sessionEnd hook.
//
// Usage:
//
// report             # consume sessionEnd JSON from stdin (hook mode)
// report --install   # register self as sessionEnd hook in ~/.copilot/config.json
// report --help
// report --version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/otakakot/copilot-session-report/internal/hookinstall"
	"github.com/otakakot/copilot-session-report/internal/report"
	"github.com/otakakot/copilot-session-report/internal/store"
	"github.com/otakakot/copilot-session-report/internal/summarizer"
)

var version = "dev"

func main() {
	// Pre-scan for --debug before flag parsing so the recursion guard can
	// decide whether to log. This avoids moving the guard after Parse().
	debugMode := hasFlag(os.Args[1:], "debug")

	// Recursion guard: if this process is invoked from a Copilot session that
	// we ourselves spawned (for summarization), bail out immediately so we
	// don't loop forever.
	if os.Getenv(summarizer.RecursionGuardEnv) == "1" {
		if debugMode {
			logger, closeLog := openSpecialLogger("_recursion-skipped")
			defer closeLog()
			logger.Info("skip: recursion guard active", "env", summarizer.RecursionGuardEnv)
		}

		os.Exit(0)
	}

	flagSet := flag.NewFlagSet("report", flag.ContinueOnError)
	install := flagSet.Bool("install", false, "register this binary as a sessionEnd hook in ~/.copilot/config.json")
	showVersion := flagSet.Bool("version", false, "print version and exit")
	debug := flagSet.Bool("debug", false, "enable debug logging to ~/.copilot-session-report-logs/")

	flagSet.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
	}
	if err := flagSet.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(0)
		}

		os.Exit(2)
	}

	if flagSet.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected positional arguments: %v\n", flagSet.Args())
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch {
	case *showVersion:
		fmt.Println(version)
		return
	case *install:
		var logger *slog.Logger
		var closeLog func()
		if *debug {
			logger, closeLog = openSpecialLogger("_install")
		} else {
			logger = slog.New(slog.DiscardHandler)
			closeLog = func() {}
		}
		defer closeLog()

		logger.Info("install start", "version", version)
		if err := runInstall(*debug); err != nil {
			logger.Error("install failed", "err", err)
			fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
			os.Exit(1)
		}

		logger.Info("install ok")
		return
	default:
		// Hook mode: read sessionEnd JSON from stdin.
		// Always exit 0 to avoid breaking the Copilot CLI hook chain;
		// errors are written to stderr only.
		startedAt := time.Now()
		ll := newLazyLogger(*debug)
		defer ll.Close()

		err := runHook(ll)
		// Make sure we have an active sink even if runHook returned before
		// resolving the session_id (so queued events still land on disk).
		ll.ActivateFallback()
		if err != nil {
			ll.Error("hook failed", "elapsed", time.Since(startedAt).Round(time.Millisecond), "err", err)
			fmt.Fprintf(os.Stderr, "report: %v\n", err)
		} else {
			ll.Info("hook done", "elapsed", time.Since(startedAt).Round(time.Millisecond))
		}
	}
}

// hasFlag checks whether the given flag name appears in args.
// Used for lightweight pre-scanning before full flag parsing.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name || a == "-"+name {
			return true
		}
	}

	return false
}

const usage = `report — write a Markdown report when a Copilot CLI session ends.

Usage:
  report                    read sessionEnd JSON from stdin (hook mode, default)
  report --install          register this binary into ~/.copilot/config.json
  report --install --debug  register with debug logging enabled
  report --debug            enable debug logging for this invocation
  report --help             show this help
  report --version          print version

Environment variables (hook mode):
  COPILOT_REPORT_DIR             output root (default ~/.copilot-session-report)
  COPILOT_HOME                   Copilot home dir (default ~/.copilot)
  COPILOT_SESSION_STATE_DIR      path to session-state dir (default ~/.copilot/session-state)
  COPILOT_REPORT_MODEL           Copilot model for summary (default gpt-5-mini)
  COPILOT_REPORT_NO_SUMMARY      "1" to skip AI summary
  COPILOT_CLI_PATH               override Copilot CLI binary used by the SDK
  COPILOT_SESSION_ID             explicit session id (otherwise resolved by cwd)
  COPILOT_REPORT_LOG_DIR         hook log directory (default ~/.copilot-session-report-logs)
  COPILOT_REPORT_LOG_DISABLE     "1" to force-disable logging even with --debug
`

type sessionEndPayload struct {
	Timestamp int64  `json:"timestamp"` // ms since epoch
	CWD       string `json:"cwd"`
	Reason    string `json:"reason"`
}

func runHook(logger *lazyLogger) error {
	logger.Info("start", "argv", os.Args, "version", version)

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20)) // 1 MiB cap
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	if len(raw) == 0 {
		return errors.New("no sessionEnd payload on stdin")
	}

	var payload sessionEndPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("parse sessionEnd json: %w", err)
	}

	logger.Info("payload", "cwd", payload.CWD, "reason", payload.Reason, "timestamp", payload.Timestamp)

	endedAt := time.UnixMilli(payload.Timestamp)
	if payload.Timestamp == 0 {
		endedAt = time.Now()
	}

	endedAt = endedAt.Local()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home dir: %w", err)
	}

	copilotHome := envOr("COPILOT_HOME", filepath.Join(home, ".copilot"))
	stateDir := envOr("COPILOT_SESSION_STATE_DIR", filepath.Join(copilotHome, "session-state"))
	outDir := reportDir()
	model := envOr("COPILOT_REPORT_MODEL", "gpt-5-mini")
	cliPath := os.Getenv("COPILOT_CLI_PATH")
	noSummary := os.Getenv("COPILOT_REPORT_NO_SUMMARY") == "1"

	st, err := store.Open(stateDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Kept slightly shorter than the hook timeoutSec (300s = 5min) so
	// we cancel and finish writing the report log before the hook host kills
	// the process.
	ctx, cancel := context.WithTimeout(context.Background(), 290*time.Second)
	defer cancel()

	sessionID := os.Getenv("COPILOT_SESSION_ID")
	if sessionID == "" {
		sessionID, err = st.LatestSessionIDByCWD(payload.CWD)
		if err != nil {
			return fmt.Errorf("resolve session id: %w", err)
		}
	}

	// Now that we know the session id, route logs to the per-session file
	// and flush any events queued during the bootstrap phase above.
	logger.Activate(sessionID)
	logger.Info("resolved session", "session_id", sessionID)

	sess, err := st.GetSession(sessionID)
	if err != nil {
		return err
	}

	turns, err := st.Turns(sessionID)
	if err != nil {
		return err
	}

	checkpoints, err := st.Checkpoints(sessionID)
	if err != nil {
		return err
	}

	files, err := st.Files(sessionID)
	if err != nil {
		return err
	}

	startedAt := parseStoreTime(sess.CreatedAt)
	if startedAt.IsZero() {
		startedAt = endedAt
	}

	var (
		summary string
		title   string
	)

	if !noSummary {
		summaryStart := time.Now()
		s, sumErr := summarizer.Summarize(ctx, summarizer.Config{
			Model:   model,
			CLIPath: cliPath,
		}, summarizer.Input{
			Session:     sess,
			Turns:       turns,
			Checkpoints: checkpoints,
			Files:       files,
			EndReason:   payload.Reason,
		})
		if sumErr != nil {
			logger.Error("summary failed", "elapsed", time.Since(summaryStart).Round(time.Millisecond), "err", sumErr)
			fmt.Fprintf(os.Stderr, "report: summary skipped: %v\n", sumErr)
		} else {
			logger.Info("summary ok",
				"elapsed", time.Since(summaryStart).Round(time.Millisecond),
				"title", s.Title,
				"body_bytes", len(s.Body),
			)
			summary = s.Body
			title = s.Title
		}
	} else {
		logger.Info("summary disabled by COPILOT_REPORT_NO_SUMMARY=1")
	}

	in := &report.Input{
		Session:     sess,
		Turns:       turns,
		Checkpoints: checkpoints,
		Files:       files,
		StartedAt:   startedAt,
		EndedAt:     endedAt,
		EndReason:   payload.Reason,
		Title:       title,
		Summary:     summary,
	}

	suffix := ""
	if len(sessionID) >= 8 {
		suffix = sessionID[:8]
	}

	outPath := report.Path(outDir, endedAt, suffix)
	if err := report.Write(outPath, in.Build()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	logger.Info("wrote report",
		"path", outPath,
		"turns", len(turns),
		"files", len(files),
	)
	fmt.Fprintf(os.Stderr, "report: wrote %s\n", outPath)

	return nil
}

// reportDir returns the directory under which report Markdown files are written.
// Default: ~/.copilot-session-report. Override with COPILOT_REPORT_DIR.
// Returns "" when the home directory cannot be determined.
func reportDir() string {
	if v := os.Getenv("COPILOT_REPORT_DIR"); v != "" {
		return v
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".copilot-session-report")
}

// logDir returns the directory under which per-session log files are written.
// Default: ~/.copilot-session-report-logs. Override with COPILOT_REPORT_LOG_DIR.
// Returns "" when the home directory cannot be determined.
func logDir() string {
	if v := os.Getenv("COPILOT_REPORT_LOG_DIR"); v != "" {
		return v
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".copilot-session-report-logs")
}

// openLogFile creates the log directory and opens <dir>/<id>.log for append.
// Returns (nil, nil) when logging is disabled via COPILOT_REPORT_LOG_DISABLE=1
// so callers can fall back to a discard handler.
func openLogFile(id string) (*os.File, error) {
	if os.Getenv("COPILOT_REPORT_LOG_DISABLE") == "1" {
		return nil, nil
	}

	dir := logDir()
	if dir == "" {
		return nil, errors.New("no home dir")
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	return os.OpenFile(filepath.Join(dir, sanitizeID(id)+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// openSpecialLogger opens an immediate file logger for non-hook modes
// (recursion-guard skip, --install). Falls back to a discard logger on error.
func openSpecialLogger(id string) (*slog.Logger, func()) {
	f, err := openLogFile(id)
	if err != nil || f == nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "report: log disabled: %v\n", err)
		}

		return slog.New(slog.DiscardHandler), func() {}
	}

	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("pid", os.Getpid())

	return logger, func() { _ = f.Close() }
}

// lazyLogger buffers log records emitted before the Copilot session id is
// known, then flushes them to ~/.copilot-session-report-logs/<session_id>.log once
// Activate is called. This preserves all hook-lifecycle events (start,
// payload, etc.) in the per-session log file even though we cannot determine
// the destination filename until after the session-store lookup.
type lazyLogger struct {
	mu      sync.Mutex
	pid     int
	debug   bool
	handler slog.Handler // nil until Activate
	queue   []slog.Record
	file    *os.File
}

func newLazyLogger(debug bool) *lazyLogger {
	return &lazyLogger{pid: os.Getpid(), debug: debug}
}

func (l *lazyLogger) log(level slog.Level, msg string, args ...any) {
	r := slog.NewRecord(time.Now(), level, msg, 0)
	r.AddAttrs(slog.Int("pid", l.pid))
	r.Add(args...)

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.handler != nil {
		_ = l.handler.Handle(context.Background(), r)
		return
	}

	l.queue = append(l.queue, r)
}

func (l *lazyLogger) Info(msg string, args ...any)  { l.log(slog.LevelInfo, msg, args...) }
func (l *lazyLogger) Error(msg string, args ...any) { l.log(slog.LevelError, msg, args...) }

// Activate opens the per-session log file and flushes all queued records.
// Subsequent Info/Error calls write directly to the file.
// Safe to call multiple times — only the first call has effect.
// If debug is false, uses a discard handler (no files are written).
func (l *lazyLogger) Activate(sessionID string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.handler != nil {
		return
	}

	if !l.debug {
		l.handler = slog.DiscardHandler
		l.queue = nil
		return
	}

	if sessionID == "" {
		sessionID = "_unknown-session"
	}

	f, err := openLogFile(sessionID)
	if err != nil || f == nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "report: log disabled: %v\n", err)
		}

		l.handler = slog.DiscardHandler
		l.queue = nil
		return
	}

	l.file = f
	l.handler = slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})

	for _, r := range l.queue {
		_ = l.handler.Handle(context.Background(), r)
	}

	l.queue = nil
}

// ActivateFallback activates the logger using the unknown-session bucket if
// it has not been activated yet (e.g. on early failure paths).
func (l *lazyLogger) ActivateFallback() {
	l.mu.Lock()
	already := l.handler != nil
	l.mu.Unlock()

	if !already {
		l.Activate("")
	}
}

func (l *lazyLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}

	return nil
}

func runInstall(debug bool) error {
	exec, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}

	abs, err := filepath.Abs(exec)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	configPath := filepath.Join(home, ".copilot", "config.json")

	changed, err := hookinstall.Install(configPath, abs, debug)
	if err != nil {
		return err
	}

	if changed {
		fmt.Printf("installed sessionEnd hook in %s -> %s\n", configPath, abs)
	} else {
		fmt.Printf("sessionEnd hook already installed in %s\n", configPath)
	}

	dirs := []string{reportDir()}
	if debug {
		dirs = append(dirs, logDir())
	}

	for _, dir := range dirs {
		if dir == "" {
			continue
		}

		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}

		fmt.Printf("ensured directory: %s\n", dir)
	}

	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

// sanitizeID restricts an ID string to safe filename characters.
// Anything outside [A-Za-z0-9._-] is replaced with '_', and the result
// is truncated to 128 chars to prevent overly long filenames.
var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitizeID(id string) string {
	s := unsafeChars.ReplaceAllString(id, "_")
	if len(s) > 128 {
		s = s[:128]
	}

	if s == "" {
		return "_empty"
	}

	return s
}

func parseStoreTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}

	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Local()
	}

	return time.Time{}
}
