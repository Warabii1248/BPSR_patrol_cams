## 概要

<!-- 何を変更したか1-2行 -->

## 関連 changelog

No.__ （changelog.txt にエントリ追加済み）

## チェックリスト（CLAUDE.md §6）

- [ ] `go build ./...` エラーなし
- [ ] `go vet ./...` 警告なし
- [ ] `go test ./...` 通る
- [ ] `go test -race ./...` 通る（並行バグ検知）
- [ ] 変更ハンドラの全 caller を grep で確認した
- [ ] CLAUDE.md §4 不変条件 を破っていない
- [ ] CLAUDE.md §5 過去罠リスト で類似ケース確認
- [ ] 既存 config.json が無変更で動作する（後方互換性）
- [ ] subagent レビュー実施（ncap/mumu/appconfig 変更時）
  - [ ] packet-analyst（ncap/ pb/ 変更時）
  - [ ] patrol-flow-reviewer（mumu/ 変更時）
  - [ ] config-compat-checker（appconfig/ config/ 変更時）

## 影響範囲

<!-- 変更ファイル列挙・後方互換性メモ -->

## 動作確認

<!-- どう確認したか・実機テスト有無 -->
