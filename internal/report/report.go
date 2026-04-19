// Package report assembles a Markdown report for a finished Copilot CLI session.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/otakakot/copilot-session-report/internal/store"
)

type Input struct {
	Session     *store.Session
	Turns       []store.Turn
	Checkpoints []store.Checkpoint
	Files       []store.FileRef
	StartedAt   time.Time
	EndedAt     time.Time
	EndReason   string
	Title       string
	Summary     string
}

func (in *Input) Build() string {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "Copilot CLI Session Report"
	}

	var b strings.Builder
	b.WriteString("---\n")
	writeYAMLString(&b, "title", title)
	writeYAMLString(&b, "session_id", in.Session.ID)
	writeYAMLString(&b, "branch", in.Session.Branch)
	writeYAMLString(&b, "cwd", in.Session.CWD)
	writeYAMLString(&b, "started_at", in.StartedAt.Format(time.RFC3339))
	writeYAMLString(&b, "ended_at", in.EndedAt.Format(time.RFC3339))
	writeYAMLString(&b, "duration", in.EndedAt.Sub(in.StartedAt).Round(time.Second).String())
	writeYAMLString(&b, "end_reason", in.EndReason)
	fmt.Fprintf(&b, "turn_count: %d\n", len(in.Turns))
	fmt.Fprintf(&b, "touched_file_count: %d\n", len(in.Files))
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "# %s\n\n", title)
	b.WriteString("## Summary\n")

	if strings.TrimSpace(in.Summary) == "" {
		b.WriteString("_(AI 要約は無効化されているか、生成に失敗しました)_\n")
	} else {
		b.WriteString(strings.TrimSpace(in.Summary))
		b.WriteString("\n")
	}

	if len(in.Files) > 0 {
		b.WriteString("\n## Touched files\n")

		const max = 30
		for i, f := range in.Files {
			if i >= max {
				fmt.Fprintf(&b, "- ...and %d more\n", len(in.Files)-max)
				break
			}

			tool := f.ToolName
			if tool == "" {
				tool = "?"
			}

			fmt.Fprintf(&b, "- `%s` (%s)\n", f.Path, tool)
		}
	}

	return b.String()
}

// writeYAMLString writes a `key: "value"` line, JSON-style escaping the value.
// YAML 1.2 accepts JSON-compatible double-quoted scalars, so json.Marshal of a
// string produces a valid YAML double-quoted scalar.
func writeYAMLString(b *strings.Builder, key, value string) {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte(`""`)
	}

	fmt.Fprintf(b, "%s: %s\n", key, encoded)
}

// Path computes <root>/YYYY/MM/DD/HH-MM-SS.md.
// If a file at the primary path already exists, "_<suffix>" is appended.
func Path(root string, end time.Time, suffix string) string {
	year := end.Format("2006")
	month := end.Format("01")
	day := end.Format("02")
	base := end.Format("15-04-05")
	dir := filepath.Join(root, year, month, day)

	primary := filepath.Join(dir, base+".md")
	if _, err := os.Stat(primary); os.IsNotExist(err) {
		return primary
	}

	if suffix == "" {
		suffix = end.Format("000")
	}

	return filepath.Join(dir, base+"_"+suffix+".md")
}

func Write(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return os.WriteFile(path, []byte(body), 0o600)
}
