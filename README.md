# copilot-session-report

GitHub Copilot CLI のセッションが終了したタイミングで、そのセッションの内容を Markdown レポートとして書き出す Go 製 CLI ツールです。

Copilot CLI の `sessionEnd` フックから呼び出して使います。要約は [github/copilot-sdk](https://github.com/github/copilot-sdk) (Go) を経由してログイン中の Copilot アカウント (= 無料枠) で生成します。外部 API キーは不要です。

## 特長

- **無依存運用**: ログイン済みの Copilot CLI さえ動いていれば、追加の API キーは不要。
- **シンプルな CLI**: フラグは `--install` / `--debug` / `--help` / `--version` の 4 つだけ。フラグなし起動が「フックモード」（`sessionEnd` JSON を stdin から読む）。
- **無限ループ対策**: 当ツールが要約のために子 Copilot セッションを起こしても、ガード環境変数で `sessionEnd` 連鎖発火を確実に断ち切ります。
- **きれいなディレクトリ構成**: 出力は `<root>/YYYY/MM/DD/HH-MM-SS.md`。

## インストール

### 必要環境

- Go 1.26 以降
- ログイン済みの GitHub Copilot CLI (`copilot` コマンドがパスにあること)
- macOS / Linux / Windows いずれも動作 (主要動作確認は macOS arm64)

### ビルドとインストール

このリポジトリをクローンし、リポジトリルート（`go.mod` のある場所）で次を実行します:

```sh
git clone https://github.com/otakakot/copilot-session-report.git
cd copilot-session-report
go install ./cmd/report
```

ビルド済みバイナリ `report` が `$(go env GOBIN)`（未設定なら `$(go env GOPATH)/bin`、通常は `~/go/bin`）に配置されます。

`PATH` を通します:

```sh
# bash / zsh の場合 (~/.bashrc や ~/.zshrc に追記)
export PATH="$(go env GOPATH)/bin:$PATH"
```

### インストールの確認

```sh
report --version
report --help
```

### Copilot CLI のフックに登録

インストールしただけではまだ何も起きません。最後に必ず以下を実行して、`sessionEnd` フックを登録してください:

```sh
report --install
```

フック動作時のデバッグログも出力したい場合は `--debug` を併用します:

```sh
report --install --debug
```

これで `~/.copilot/config.json` の `hooks` フィールドに当ツールが書き込まれ、次回以降の Copilot CLI セッション終了時に自動でレポートが生成されます。

> **NOTE**: Copilot CLI のユーザーレベルフックは `~/.copilot/config.json` の `hooks` フィールド (インライン) で定義する仕様です (`copilot help config` の `hooks` 項参照)。`~/.copilot/hooks.json` という独立したファイルは Copilot CLI からは読み込まれません。

### アップデート

リポジトリを最新化してから再度 `go install` するだけです。バイナリのパスは変わらないため、フックの再登録は不要です:

```sh
git pull
go install ./cmd/report
```

### アンインストール

1. `~/.copilot/config.json` を開き、`hooks.sessionEnd` 配列から `installedBy: "copilot-session-report"` のエントリを削除する (配列が空になれば `hooks` フィールドごと消して構いません)
2. バイナリを削除する: `rm "$(command -v report)"`

## config.json への書き込み内容

`report --install` を実行すると `~/.copilot/config.json` に次のような `hooks` フィールドが追加 / 更新されます。`installedBy` マーカーで自分のエントリだけを置換するので、既存の他フックや他のフィールドは保持されます。書き込み前に `config.json.bak` を作成し、書き込み後の内容に差分がなければ自動で削除します。

```json
{
  "hooks": {
    "sessionEnd": [
      {
        "type": "command",
        "bash": "'/Users/you/go/bin/report'",
        "powershell": "& '/Users/you/go/bin/report'",
        "timeoutSec": 300,
        "installedBy": "copilot-session-report"
      }
    ]
  }
}
```

`report --install --debug` で登録した場合は `--debug` フラグが付加されます:

```json
{
  "hooks": {
    "sessionEnd": [
      {
        "type": "command",
        "bash": "'/Users/you/go/bin/report' --debug",
        "powershell": "& '/Users/you/go/bin/report' --debug",
        "timeoutSec": 300,
        "installedBy": "copilot-session-report"
      }
    ]
  }
}
```

> **NOTE**: 再帰ガード (`COPILOT_REPORT_RECURSION_GUARD=1`) は hooks のコマンド文字列には埋め込みません。当ツールが要約のために起こす子 Copilot SDK セッションの環境変数として注入し、子セッションの `sessionEnd` 発火時に即 exit させます。

以後、Copilot CLI のセッションが終了するたびに自動でレポートが生成されます。

## 使い方

通常はフックから自動起動されますが、手動でも実行できます。Copilot CLI のフック仕様 (`sessionEnd`) と同じ JSON を stdin に流してください。

```sh
printf '{"timestamp":%d,"cwd":"%s","reason":"complete"}' \
  "$(($(date +%s) * 1000))" "$PWD" \
  | report
```

レポートは既定で `~/.copilot-session-report/<YYYY>/<MM>/<DD>/<HH-MM-SS>.md` に書き出されます。

## 出力ファイル例

```
~/.copilot-session-report/
└── 2026/
    └── 04/
        └── 19/
            └── 00-07-01.md
```

レポート本文の例:

```markdown
---
title: "認証フローのリファクタとテスト追加"
session_id: "8cc880ad-9040-4e91-843e-1723b1652e69"
branch: "main"
cwd: "/Users/otakakot/Repository/pogo"
started_at: "2026-04-19T00:05:06+09:00"
ended_at: "2026-04-19T00:07:01+09:00"
duration: "1m55s"
end_reason: "complete"
turn_count: 12
touched_file_count: 4
---

# 認証フローのリファクタとテスト追加

## Summary
### 概要
...

### 主な変更点
- ...

### 知見・学び
- ...

### 次にやるべきこと
- ...

## Touched files
- `cmd/main.go` (edit)
```

タイトルはセッション内容から AI が抽出した短い見出しが入り、YAML front matter (`title:`) と Markdown の `# 見出し` の両方で使われます。Obsidian や静的サイトジェネレータのタイトルとしてそのまま使えます。

## フラグ

| フラグ | 説明 |
| --- | --- |
| (なし) | stdin から `sessionEnd` JSON を読み、レポートを生成して書き出す。フックからの起動はこれを使う。 |
| `--install` | `~/.copilot/config.json` の `hooks.sessionEnd` に当 CLI を登録する。 |
| `--debug` | デバッグログを `~/.copilot-session-report-logs/` に出力する。`--install` と併用すると、フック経由の起動時にも自動でログが出力される。 |
| `--help` | ヘルプを表示する。 |
| `--version` | バージョンを表示する。 |

## 環境変数

ユーザーが触れるフラグは最小限に抑えています。挙動の調整は環境変数で行ってください。

| 環境変数 | デフォルト | 説明 |
| --- | --- | --- |
| `COPILOT_REPORT_DIR` | `~/.copilot-session-report` | レポート出力ルートディレクトリ。 |
| `COPILOT_HOME` | `~/.copilot` | Copilot ホームディレクトリ。 |
| `COPILOT_SESSION_STATE_DIR` | `~/.copilot/session-state` | セッションステートディレクトリ (JSONL ファイルの読み取り元)。 |
| `COPILOT_REPORT_MODEL` | `gpt-5-mini` | 要約に使う Copilot モデル名。 |
| `COPILOT_REPORT_NO_SUMMARY` | `0` | `1` を設定すると AI 要約をスキップしてメタ情報だけのレポートを書きます。 |
| `COPILOT_CLI_PATH` | (空) | Copilot SDK が呼び出す `copilot` バイナリのパスを上書きしたい場合に指定します。 |
| `COPILOT_SESSION_ID` | (空) | 明示的に対象セッション ID を指定したい場合に設定します。未指定なら `cwd` で最新のセッションを採用します。 |
| `COPILOT_REPORT_LOG_DIR` | `~/.copilot-session-report-logs` | フック動作ログの出力ディレクトリ。1 セッションにつき `<session_id>.log` を 1 ファイル作成します。 |
| `COPILOT_REPORT_LOG_DISABLE` | (空) | `1` を設定すると `--debug` 指定時でもログ書き出しを強制的に無効化します。 |
| `COPILOT_REPORT_RECURSION_GUARD` | (空) | **内部用**。`1` を設定すると当ツールは即時終了します。SDK 経由の子セッションの環境変数として自動的に設定されます。手動で触る必要はありません。 |

## 無限ループの防止について

当ツールは `sessionEnd` フックで起動され、内部で **Copilot SDK 経由の Copilot セッション** をもう一つ立ち上げて要約を作ります。この子セッションが終了するときも当然 `sessionEnd` フックが発火するので、対策がないと無限ループします。

そこで次の二重のガードを入れています:

1. **子セッションの hooks を無効化**: SDK の `ConfigDir` オプションで `disableAllHooks: true` を設定した一時ディレクトリを指定し、子 Copilot CLI のフック発火自体を抑制。
2. **環境変数ガード (バックアップ)**: SDK 経由で立ち上げる Copilot サブセッションには `Env` オプション経由で `COPILOT_REPORT_RECURSION_GUARD=1` を伝播。万が一 hooks 無効化が効かなかった場合でも、当ツールは起動直後にこの環境変数を読み `1` のときは即 exit。

これにより子セッション終了時のフック発火自体が起きず、仮に発火しても即 exit となるため、確実にループしません。

## 失敗時の動作

レポート生成中にエラーが起きてもフックチェーン自体は壊さないよう、当ツールは **常に exit 0** で終了します。エラーは標準エラー出力にメッセージとして残します。

要約 (Copilot SDK 呼び出し) が失敗した場合は、要約欄を「無効化されたか生成に失敗」した旨のプレースホルダで埋め、メタ情報・編集ファイル一覧のみのレポートを保存します。

## ログ (フック動作の確認)

デフォルトではログファイルは作成されません。フックの動作を確認したいときは `--debug` フラグを付けてインストールしてください:

```sh
report --install --debug
```

これにより、フック起動時に `~/.copilot-session-report-logs/` 配下に **1 セッション 1 ファイル** で `slog` のテキスト形式ログが追記されます。

```
~/.copilot-session-report-logs/
├── 8cc880ad-9040-4e91-843e-1723b1652e69.log  # 通常のセッション (UUID は session_id)
├── _recursion-skipped.log                    # 再帰ガードで即終了したケースをすべて集約
├── _unknown-session.log                      # session_id を特定できなかった呼び出し
└── _install.log                              # `report --install` の実行履歴
```

書き込まれる主な内容:

- `start` — フック起動を検知 (argv, version)
- `payload` — sessionEnd JSON の中身 (cwd, reason, timestamp)
- `resolved session` — 対象 session_id の特定
- `summary ok` / `summary failed` — Copilot SDK 経由の要約結果と所要時間
- `wrote report` — レポート書き出しの絶対パスと統計 (turns/files)
- `hook done` / `hook failed` — フック全体の所要時間
- `skip: recursion guard active` — 再帰ガードで即終了した呼び出し

「フックが本当に動いているのか?」を確認したい時は対象セッションの log を `tail -f` してください。出力先と無効化は `COPILOT_REPORT_LOG_DIR` / `COPILOT_REPORT_LOG_DISABLE` で制御できます。

実装には標準ライブラリの `log/slog` (`slog.NewTextHandler`) を使っており、追加依存はありません。session_id 確定前に発生したイベント (`start`, `payload`) は内部キューにバッファされ、`<session_id>.log` を開いた瞬間にまとめて flush されるため、ファイルには時系列順の完全な記録が残ります。

## ライセンス

未指定（個人プロジェクト）。
