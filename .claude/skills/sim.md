---
name: sim
description: Level 1 sim 検証を実行して結果を要約する。引数なしで run_all.ps1（全7シナリオ・約7分）、引数にシナリオ名で個別実行。FAIL 時は flake 切り分けの自動再実行付き。
allowed-tools: PowerShell, Read, Grep, Glob
---

# /sim — patrol-sim 検証の実行と要約

## 目的

CLAUDE.md §10 の Level 1 検証（実機レス・本番 Patroller + fake-adb）を実行し、PASS/FAIL を要約する。
mumu/ 変更後のコミット前チェック（§6）で必須。

## 引数の扱い

- **引数なし**: `.\run_all.ps1` で全7シナリオ（baseline / native_move / native_move_same_ch / burst_0x2e / silent_one / slow_signal / screen_mode）
- **引数あり**（例: `/sim baseline`）: 該当シナリオのみ個別実行

## 実行手順

### 全シナリオ（引数なし）

1. PowerShell で実行（ビルド込み・約7分。timeout は 600000ms を指定）:

   ```powershell
   .\run_all.ps1
   ```

2. exit code 0 = 全 PASS。非 0 = いずれか FAIL。
3. **FAIL があった場合は同じコマンドをもう1回だけ再実行**（ビルド直後初回の flake 既知・§10）。
   - 再実行で PASS → flake と判定し、その旨を添えて PASS 報告
   - 再実行でも FAIL → 本物。`logs\sim\<name>.log` を Read し、FAIL 行（assert 違反・forbid ヒット・dwell 範囲外）を特定

### 個別シナリオ（引数あり）

1. `scenarios\<引数>.json` の存在を Glob で確認。無ければ scenarios/ の一覧を出して終了
2. `release\patrol-sim.exe` と `release\fake-adb.exe` が無ければ先に `.\run_all.ps1` でビルド（または `go build -o release\patrol-sim.exe .\cmd\patrol-sim` 等）
3. 実行:

   ```powershell
   .\release\patrol-sim.exe -scenario scenarios\<name>.json
   ```

4. FAIL 時の flake 再実行ルールは全シナリオ実行と同じ

## 報告フォーマット

- 結果テーブル: シナリオ名 / PASS・FAIL / 所要時間（ログから取れる場合）
- FAIL があれば: 違反した assert（forbid ヒット行 or dwell 実測値）と該当ログ抜粋、`logs\sim\<name>.log` へのパス
- flake 再実行をした場合はその旨を明記（1回目 FAIL → 2回目 PASS 等）
- dwell 実測がログにあれば代表値を1行で添える

## 注意

- dwell 実測には extraWait（発行前追加待ち）が合算される計測特性がある（§10）。dwell 超過 FAIL を即バグと断定せず、forbid「発行前追加待ちタイムアウト」のヒット有無を併せて確認
- sim は本番 mumu.Patroller をそのまま動かす。FAIL の原因切り分けは「シナリオ/sim 側」と「mumu 本体」の両方を疑う
- 再実行は **1回まで**。2連続 FAIL は本物として報告する
