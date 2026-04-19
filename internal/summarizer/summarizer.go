// Package summarizer generates session summaries via the Copilot SDK.
package summarizer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/otakakot/copilot-session-report/internal/store"
)

// denyAll is a permission handler that denies every tool-use request.
// The summarizer session only needs to generate text; it must not read
// files, run commands, or perform any other action on the local machine.
func denyAll(_ copilot.PermissionRequest, _ copilot.PermissionInvocation) (copilot.PermissionRequestResult, error) {
	return copilot.PermissionRequestResult{
		Kind: copilot.PermissionRequestResultKindDeniedByRules,
	}, nil
}

// minimalEnv builds a small environment for the child Copilot CLI process.
// Only variables required for the process to function are forwarded;
// secrets and unrelated state are excluded.
func minimalEnv() []string {
	allow := []string{
		"HOME", "USER", "LOGNAME", "SHELL",
		"PATH", "LANG", "LC_ALL", "LC_CTYPE",
		"TERM", "TMPDIR",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
		"USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
		"SystemRoot", "COMSPEC",
		"NO_COLOR",
	}

	env := make([]string, 0, len(allow)+1)
	for _, key := range allow {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}

	env = append(env, RecursionGuardEnv+"=1")

	return env
}

// RecursionGuardEnv is set on the child Copilot session to prevent
// the sessionEnd hook from re-invoking this tool recursively.
// Kept as a belt-and-suspenders safety net alongside disableAllHooks.
const RecursionGuardEnv = "COPILOT_REPORT_RECURSION_GUARD"

type Config struct {
	Model   string
	CLIPath string
}

type Input struct {
	Session     *store.Session
	Turns       []store.Turn
	Checkpoints []store.Checkpoint
	Files       []store.FileRef
	EndReason   string
}

// Result is the output of Summarize: a short one-line title plus the
// Markdown body (which includes section headings).
type Result struct {
	Title string
	Body  string
}

// Summarize asks Copilot (via the SDK) for a Markdown summary of the session.
// It uses the user's logged-in Copilot account.
// Returns (Result{}, err) if the SDK fails — caller should fall back to no-summary.
func Summarize(ctx context.Context, cfg Config, in Input) (Result, error) {
	if cfg.Model == "" {
		cfg.Model = "gpt-5-mini"
	}

	// Create a temporary config directory with hooks disabled.
	// This prevents the child Copilot CLI from firing sessionEnd hooks,
	// avoiding infinite recursion when the summarization session ends.
	tmpDir, err := os.MkdirTemp("", "copilot-report-*")
	if err != nil {
		return Result{}, fmt.Errorf("create temp config dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"),
		[]byte(`{"disableAllHooks":true}`), 0o600); err != nil {
		return Result{}, fmt.Errorf("write temp config: %w", err)
	}

	client := copilot.NewClient(&copilot.ClientOptions{
		LogLevel: "error",
		CLIPath:  cfg.CLIPath,
		Env:      minimalEnv(),
	})

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		return Result{}, fmt.Errorf("start copilot client: %w", err)
	}
	defer func() { _ = client.Stop() }()

	session, err := client.CreateSession(ctx, &copilot.SessionConfig{
		Model:               cfg.Model,
		ClientName:          "copilot-session-report",
		ConfigDir:           tmpDir,
		AvailableTools:      []string{},
		OnPermissionRequest: denyAll,
		SystemMessage: &copilot.SystemMessageConfig{
			Mode:    "replace",
			Content: systemPrompt,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("create session: %w", err)
	}
	defer func() { _ = session.Disconnect() }()

	response, err := session.SendAndWait(ctx, copilot.MessageOptions{
		Prompt: buildPrompt(in),
	})
	if err != nil {
		return Result{}, fmt.Errorf("send prompt: %w", err)
	}

	if response == nil {
		return Result{}, errors.New("no response from summarizer")
	}

	d, ok := response.Data.(*copilot.AssistantMessageData)
	if !ok || strings.TrimSpace(d.Content) == "" {
		return Result{}, errors.New("empty summary")
	}

	return parseResult(strings.TrimSpace(d.Content)), nil
}

// parseResult splits the LLM output into a one-line title and a Markdown body.
// The model is instructed to start with `TITLE: <text>` on the first line.
// If that prefix is missing, the entire output is treated as the body and
// Title stays empty (caller falls back to a generic title).
func parseResult(raw string) Result {
	raw = strings.TrimSpace(raw)
	first, rest, hasNL := strings.Cut(raw, "\n")
	first = strings.TrimSpace(first)
	first = strings.TrimPrefix(first, "#")
	first = strings.TrimSpace(first)

	const prefix = "TITLE:"
	if strings.HasPrefix(strings.ToUpper(first), prefix) {
		title := strings.TrimSpace(first[len(prefix):])
		title = strings.Trim(title, "\"'`")
		body := ""
		if hasNL {
			body = strings.TrimSpace(rest)
		}

		return Result{Title: title, Body: body}
	}

	return Result{Body: raw}
}

const systemPrompt = `You are an assistant that summarizes a finished GitHub Copilot CLI session.
Write the summary in Japanese Markdown. Be concise and factual.

Output format (strictly follow):
1. The first line MUST be exactly: TITLE: <短いタイトル>
   - 30 文字以内、句点なし、装飾記号なし。
   - セッションの内容（何をしたか）を端的に表すこと。例: "認証フローのリファクタとテスト追加"。
2. 2 行目以降は Markdown 本文。トップレベル見出し (#) は出力しない。caller が独自のタイトル見出しを付ける。
3. 本文は次のセクション (### 見出し) を順番に含める。該当する事柄が無いセクションは省略可:
   - ### 概要 (1〜3 文)
   - ### 主な変更点 (実際に編集・追加・削除したコードや設定の要点を箇条書き)
   - ### 知見・学び
   - ### 次にやるべきこと (箇条書き)

「### 知見・学び」セクションの書き方 (重要):
- セッション中に **新たに分かったこと・学んだこと・再利用したいテクニック** だけを書く。
- 具体的には次のような情報を優先的に拾うこと:
  - 調査の過程で参照した URL (公式ドキュメント、Issue、ブログ等) と、そこから得た要点
  - ライブラリ・API・コマンドの仕様で「ハマったポイント」「直感に反する挙動」
  - エラーメッセージとその原因・対処法
  - 設計上のトレードオフや、採用しなかった代替案とその理由
  - 一般化できる知識 (今後別プロジェクトでも役立ちそうな Tips)
- 箇条書き 1 件は 1〜3 文程度。URL がある場合は \"- [...] : 要点\" の形で URL も記載する。
- セッションの **メタ情報** (session_id, cwd, branch, end_reason, ファイルパスの単純列挙など) を書いてはいけない。それらは別欄で表示済み。
- 知見が本当に何も無いなら「- 特筆すべき知見なし」とだけ書く。無理に埋めない。

余計な前置き、謝罪、メタコメントは書かない。`

const (
	maxTurns       = 40
	maxTurnContent = 4000
	maxFiles       = 50
	maxCheckpoints = 3
)

func buildPrompt(in Input) string {
	var b strings.Builder
	b.WriteString("以下は GitHub Copilot CLI セッションの記録です。これを要約してください。\n\n")
	b.WriteString("## メタ情報\n")

	if in.Session != nil {
		fmt.Fprintf(&b, "- session_id: %s\n", in.Session.ID)
		fmt.Fprintf(&b, "- cwd: %s\n", in.Session.CWD)
		fmt.Fprintf(&b, "- branch: %s\n", in.Session.Branch)
	}

	fmt.Fprintf(&b, "- end_reason: %s\n\n", in.EndReason)

	if n := len(in.Checkpoints); n > 0 {
		b.WriteString("## 直近のチェックポイント\n")

		start := 0
		if n > maxCheckpoints {
			start = n - maxCheckpoints
		}

		for _, c := range in.Checkpoints[start:] {
			fmt.Fprintf(&b, "### Checkpoint #%d %s\n", c.Number, c.Title)
			writeSection(&b, "overview", c.Overview)
			writeSection(&b, "work_done", c.WorkDone)
			writeSection(&b, "next_steps", c.NextSteps)
			writeSection(&b, "important_files", c.ImportantFiles)
		}

		b.WriteString("\n")
	}

	if n := len(in.Files); n > 0 {
		b.WriteString("## 編集されたファイル\n")

		limit := n
		if limit > maxFiles {
			limit = maxFiles
		}

		for _, f := range in.Files[:limit] {
			fmt.Fprintf(&b, "- %s (%s)\n", f.Path, f.ToolName)
		}

		if n > limit {
			fmt.Fprintf(&b, "- ...他 %d 件\n", n-limit)
		}

		b.WriteString("\n")
	}

	if n := len(in.Turns); n > 0 {
		b.WriteString("## 会話 (古い順、トリミング済)\n")

		start := 0
		if n > maxTurns {
			start = n - maxTurns
		}

		for _, t := range in.Turns[start:] {
			fmt.Fprintf(&b, "### Turn %d\n", t.Index)
			writeSection(&b, "user", t.UserMessage)
			writeSection(&b, "assistant", t.AssistantResponse)
		}
	}

	return b.String()
}

func writeSection(b *strings.Builder, label, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	if len(content) > maxTurnContent {
		content = content[:maxTurnContent] + "...(truncated)"
	}

	fmt.Fprintf(b, "**%s**:\n%s\n\n", label, content)
}
