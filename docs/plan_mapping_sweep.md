# 全chマッピング（diff 仕様書・option A 逐次・ペア＋巡回ループ再利用版）

## 背景 / 原因（実機ログ 2026/06/20 で確定）

「全chマッピング」ボタンの自動フローが意図通り動かなかった。根本原因:

1. **stale assignment で巡回**: 自動フローの `compute-assignments` が `Status().Serials`（巡回開始まで空）→
   `ConnectSerials`（空）で `ok:false` を返し、JS がそれを無視 → 起動時ロードの**古い 2グループ assignment**で巡回。8台中2台しか動かなかった。
2. **巡回ループが直列 + assignment フィルタ**: 1tick=1ch でグループを並列に進められず「1ペアしか動かない」。
3. **park は全ch対象では無効**（退避先ch無し）。

実機での追検証（独自スイープ第1版）で判明した追加の不具合:

4. **全台が移動してしまう**（ペアのみ動かすべき）。
5. **ロード未完了のまま次 ch へ移動**: 独自スイープの完了判定 `CurrentCh==ch`（portMap 解決≈10s）は
   ロード完了（screen≈40s）より早く、ロード中に次 ch 切替を発行して失敗していた。

## 設計判断（ユーザー確定）

- **スイープ完了後 = 停止**（マッピングは一回限りのセットアップ。巡回は手動開始）
- **2台1ペアだけ動かす**（並列不可のため残りデバイスはアイドル）
- **ロード完了を待ってから次 ch へ**
- → 独自の逐次スイープ（`CurrentCh==ch` 判定）はやめ、**実証済みの巡回ループ（Start・一巡モード）を再利用**し、
  その完了待ち（0x2E/screen）でロード完了を保証する
- 並列化（全台を活かした高速化）は ncap 改修が前提のため将来タスク（C6/B）

## 並列が不可能な理由（重要・ncap 制約）

portMap が ch を**永続的に書く**経路は quorum のみ（`submitPortVote`→`portMap.Update`・cap_device.go:568/580）。
probeMode ground-truth は `sess.lineID` を設定するだけで portMap を書かない（cap_device.go:484）。
そして quorum/ground-truth の投票 ch は**単一グローバル `cd.currentChannel`**（cap_device.go:185, 474）。
巡回ループが 1tick=1ch なのはこの制約のため。全ペアが別chへ同時移動すると `currentChannel` が1値しか持てず
portMap が誤chで汚染される。→ **逐次（1ch ずつ確定）が下限**。台数では短縮不可。

## option A が成立する根拠（巡回ループ再利用）

- 巡回ループ（Start）は各 ch の**完了待ち（0x2E/screen）= ロード完了**まで次 ch へ進まない →
  ロード未完了のまま移動する問題が起きない。
- `patrolActive=true` 中は quorum 成立で `portMap.Update` + `applyPortMapToSessions` を**自動実行**（No.79）。
  ペア2台が同一chへ入ることで quorum(2) が成立し未登録chを学習する。
- `LoopMode=false`（一巡モード）で全 ch を1周したら自動停止する。
- `onChannelSwitch(ch)` で切替前に投票chを正す（No.74）— 巡回ループが既に実施。

## 変更内容

### A. mumu/mumu.go — 新メソッド `MapSweep`（実証済み巡回ループの薄いラッパ）

```
func (p *Patroller) MapSweep(serials []string, channels []uint32)
```

並列不可（単一 currentChannel 制約）のため **2台1ペアだけ**を使い、巡回ループを一巡モードで起動する:

1. serials/channels 空チェック → ログして return
2. `pair := serials[:2]`（quorum=2 を満たす最小構成。残りデバイスはアイドル）。2台未満なら警告ログ
3. `p.SetDeviceAssignments(nil)`（分担フィルタ無効化＝ペアのみ動かす・stale assignment 影響も排除）
4. `p.Start(pair, channels, "", PatrolOptions{LoopMode: false})`
   - 各 ch の完了待ち（0x2E/screen）がロード完了を保証
   - `patrolActive=true`（Start が設定）→ quorum 自動確定で portMap が埋まる（No.79）
   - 一巡で自動停止

定数: `const mapSweepQuorum = 2`（= ncap `portVoteQuorum`。未登録ch学習に必要な最小投票数）。

**付随修正**（Start 共通）: 巡回 goroutine の defer に `onPatrolActive(false)` を追加。
一巡モード(LoopMode=false)の自動停止は `Stop()` を経由しないため、従来は巡回後も `patrolActive=true` が
残り ncap が quorum 自動確定を続けていた（既存の軽微な漏れ）。マッピング含む全 LoopMode=false 巡回で解消。
`Stop()` 経由でも冪等（二重 false は ncap 側で無害）。

### B. gui/handlers.go — 新エンドポイント `handlePortMapSweep`

```
POST /api/portmap/sweep   body: {serials:[], channels:[]}
```

- `handlePatrolStart` と同型ガード（`patrolEnabled` / POST / `Status().Running` なら 409）
- channels 空なら `s.patrolChannels` フォールバック → それも空なら `{ok:false,error:"チャンネルリストが空です"}`
- `go s.patroller.MapSweep(req.Serials, channels)`（即 200 返却・バックグラウンド）

### C. gui/gui.go — ルート登録

- `mux.HandleFunc("/api/portmap/sweep", s.handlePortMapSweep)`

### D. gui/assets/app.js — 自動フローを sweep に切替

- `finishAutoPatrolFlow`:
  - `/api/devices/assignments/compute` 呼び出しを**削除**（MapSweep は assignment 非依存・① stale 経路も排除）
  - `patrolStart(...)` → `fetch('/api/portmap/sweep', {serials:_autoFlowSerials, channels:_autoFlowChs||patrolChannels})`
  - 起動後 `/api/patrol/status` を `running` でポーリング（マッピングは一巡パトロールとして走るため phase 非依存・
    一度 `running=true` を観測してから完了判定）。「マッピング中 (current_index/total_channels)」表示 →
    `running=false` で「✓ 全chマッピング完了。巡回は手動で開始してください」

### E. 既存うけ作業ツリーの整理（reconcile・ユーザー承認済）

- **KEEP**: `ComputeDeviceAssignments(deviceCount, channels []uint32)` 新シグネチャ + mumu_test.go
  （monster 巡回の分担用に温存。MapSweep は使わないが回帰なし）
- **REVERT 済**: park 一式（`ParkBeforePatrol`/`pickParkChannel`/Start内park/`park_before`/`plan_park_assignments.md`）
- `handleComputeAssignments`: 現状維持（MapSweep フローは compute 呼び出しを削除するため stale バグは無害化）

## 不変条件への影響（§4 照合）

- §4-1（ncap）: **無変更**。`SetCurrentChannel`(No.74)/quorum自動確定(No.79) の既存挙動をそのまま利用
- §4-2 全項（巡回状態機械）: MapSweep は巡回ループ（Start）を**そのまま再利用**するため状態機械は不変。
  唯一の Start 改変は defer の `onPatrolActive(false)` 追加（一巡停止時の patrolActive 漏れ修正・冪等）。
- マッピングは「ペア2台・LoopMode=false の通常巡回」として実行 → 既存の完了待ち・quorum・自動停止を流用
- 後方互換: 新 API・新メソッドの追加 + 一巡停止時の patrolActive 修正。config スキーマ変更なし

## 検証

- `go build ./...` / `go vet ./...` / `go test ./mumu/`
- patrol-flow-reviewer レビュー（§9）
- `run_all.ps1`（既存 mumu 経路に回帰がないこと）
- 実機: 全chマッピング → ペア2台が逐次でロード完了を待ちつつ portMap を埋める → 一巡で停止、を確認
