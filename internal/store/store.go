// Package store reads session data from ~/.copilot/session-state/ JSONL files.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Session struct {
	ID        string
	CWD       string
	Branch    string
	Summary   string
	CreatedAt string
	UpdatedAt string
}

type Turn struct {
	Index             int
	UserMessage       string
	AssistantResponse string
	Timestamp         string
}

type Checkpoint struct {
	Number           int
	Title            string
	Overview         string
	History          string
	WorkDone         string
	TechnicalDetails string
	ImportantFiles   string
	NextSteps        string
	CreatedAt        string
}

type FileRef struct {
	Path     string
	ToolName string
	Turn     int
}

// Store reads session data from JSONL files in the session-state directory.
type Store struct {
	dir string
}

// Open opens the session-state directory for reading.
func Open(dir string) (*Store, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("open session state dir: %w", err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", dir)
	}

	return &Store{dir: dir}, nil
}

func (s *Store) Close() error { return nil }

func readWorkspace(sessionDir string) (*Session, error) {
	data, err := os.ReadFile(filepath.Join(sessionDir, "workspace.yaml"))
	if err != nil {
		return nil, err
	}

	m := parseSimpleYAML(string(data))

	return &Session{
		ID:        m["id"],
		CWD:       m["cwd"],
		Branch:    m["branch"],
		Summary:   m["summary"],
		CreatedAt: m["created_at"],
		UpdatedAt: m["updated_at"],
	}, nil
}

// parseSimpleYAML parses a flat "key: value" YAML file into a map.
func parseSimpleYAML(s string) map[string]string {
	m := make(map[string]string)

	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}

		m[key] = val
	}

	return m
}

func (s *Store) GetSession(id string) (*Session, error) {
	sess, err := readWorkspace(filepath.Join(s.dir, id))
	if err != nil {
		return nil, fmt.Errorf("session %s not found: %w", id, err)
	}

	return sess, nil
}

// LatestSessionIDByCWD scans session directories to find the most recently
// updated session matching the given working directory.
func (s *Store) LatestSessionIDByCWD(cwd string) (string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", fmt.Errorf("read session state dir: %w", err)
	}

	var bestID string
	var bestTime time.Time

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		sess, err := readWorkspace(filepath.Join(s.dir, e.Name()))
		if err != nil || sess.CWD != cwd {
			continue
		}

		t, err := time.Parse(time.RFC3339Nano, sess.UpdatedAt)
		if err != nil {
			continue
		}

		if bestID == "" || t.After(bestTime) {
			bestID = sess.ID
			bestTime = t
		}
	}

	if bestID == "" {
		return "", fmt.Errorf("no session found for cwd %s", cwd)
	}

	return bestID, nil
}

// event is a single line from events.jsonl.
type event struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// readEvents parses events.jsonl using ReadBytes to handle large lines.
// Tolerates a malformed trailing line (hook-time partial write).
func readEvents(sessionDir string) ([]event, error) {
	f, err := os.Open(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []event
	reader := bufio.NewReaderSize(f, 256*1024)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, " \r\n")
			if len(line) > 0 {
				var ev event
				if json.Unmarshal(line, &ev) == nil {
					events = append(events, ev)
				} else if err != nil {
					break // malformed trailing line
				}
			}
		}

		if err != nil {
			break
		}
	}

	return events, nil
}

func (s *Store) Turns(sessionID string) ([]Turn, error) {
	events, err := readEvents(filepath.Join(s.dir, sessionID))
	if err != nil {
		return nil, err
	}

	var turns []Turn
	var currentUser string
	var currentTS string
	var assistBuf strings.Builder
	var idx int
	inTurn := false

	flush := func() {
		if !inTurn {
			return
		}

		turns = append(turns, Turn{
			Index:             idx,
			UserMessage:       currentUser,
			AssistantResponse: strings.TrimSpace(assistBuf.String()),
			Timestamp:         currentTS,
		})
		idx++
		inTurn = false
		currentUser = ""
		currentTS = ""
		assistBuf.Reset()
	}

	for _, ev := range events {
		switch ev.Type {
		case "user.message":
			flush()

			var d struct {
				Content string `json:"content"`
			}

			_ = json.Unmarshal(ev.Data, &d)
			currentUser = d.Content
			currentTS = ev.Timestamp
			inTurn = true

		case "assistant.message":
			if !inTurn {
				continue
			}

			var d struct {
				Content string `json:"content"`
			}

			_ = json.Unmarshal(ev.Data, &d)
			if content := strings.TrimSpace(d.Content); content != "" {
				if assistBuf.Len() > 0 {
					assistBuf.WriteString("\n\n")
				}

				assistBuf.WriteString(content)
			}
		}
	}

	flush()

	return turns, nil
}

func (s *Store) Checkpoints(sessionID string) ([]Checkpoint, error) {
	cpDir := filepath.Join(s.dir, sessionID, "checkpoints")

	indexData, err := os.ReadFile(filepath.Join(cpDir, "index.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	type cpEntry struct {
		number int
		title  string
		file   string
	}

	var entries []cpEntry

	for _, line := range strings.Split(string(indexData), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "| #") || strings.HasPrefix(line, "|--") {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}

		num, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}

		entries = append(entries, cpEntry{
			number: num,
			title:  strings.TrimSpace(parts[2]),
			file:   strings.TrimSpace(parts[3]),
		})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].number < entries[j].number })

	var out []Checkpoint

	for _, e := range entries {
		cpPath := filepath.Join(cpDir, e.file)

		data, err := os.ReadFile(cpPath)
		if err != nil {
			continue
		}

		body := string(data)
		createdAt := ""
		if info, err := os.Stat(cpPath); err == nil {
			createdAt = info.ModTime().UTC().Format(time.RFC3339)
		}

		out = append(out, Checkpoint{
			Number:           e.number,
			Title:            e.title,
			Overview:         extractTag(body, "overview"),
			History:          extractTag(body, "history"),
			WorkDone:         extractTag(body, "work_done"),
			TechnicalDetails: extractTag(body, "technical_details"),
			ImportantFiles:   extractTag(body, "important_files"),
			NextSteps:        extractTag(body, "next_steps"),
			CreatedAt:        createdAt,
		})
	}

	return out, nil
}

var tagPatterns = func() map[string]*regexp.Regexp {
	tags := []string{"overview", "history", "work_done", "technical_details", "important_files", "next_steps"}
	m := make(map[string]*regexp.Regexp, len(tags))

	for _, tag := range tags {
		m[tag] = regexp.MustCompile(`(?s)<` + regexp.QuoteMeta(tag) + `>\s*(.*?)\s*</` + regexp.QuoteMeta(tag) + `>`)
	}

	return m
}()

func extractTag(body, tag string) string {
	re := tagPatterns[tag]
	if re == nil {
		return ""
	}

	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}

	return strings.TrimSpace(m[1])
}

func (s *Store) Files(sessionID string) ([]FileRef, error) {
	events, err := readEvents(filepath.Join(s.dir, sessionID))
	if err != nil {
		return nil, err
	}

	turnIdx := 0
	seen := make(map[string]bool)
	var out []FileRef

	for _, ev := range events {
		switch ev.Type {
		case "user.message":
			turnIdx++

		case "tool.execution_start":
			var d struct {
				ToolName  string          `json:"toolName"`
				Arguments json.RawMessage `json:"arguments"`
			}

			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}

			if d.ToolName != "edit" && d.ToolName != "create" {
				continue
			}

			var args struct {
				Path string `json:"path"`
			}

			// Arguments may be a JSON string or an object.
			if len(d.Arguments) > 0 && d.Arguments[0] == '"' {
				var raw string
				if json.Unmarshal(d.Arguments, &raw) == nil {
					_ = json.Unmarshal([]byte(raw), &args)
				}
			} else {
				_ = json.Unmarshal(d.Arguments, &args)
			}

			if args.Path == "" || seen[args.Path] {
				continue
			}

			seen[args.Path] = true
			out = append(out, FileRef{
				Path:     args.Path,
				ToolName: d.ToolName,
				Turn:     turnIdx,
			})
		}
	}

	return out, nil
}
