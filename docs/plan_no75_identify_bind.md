# plan_no75: Identify バインドを SceneData 無し下で成立させる（ground-truth lineID 解決）

ステータス: **承認済み（ユーザー GO）** / 実装待ち
対象: ncap/cap_device.go, mumu/mumu.go, main.go
関連: No.23(sd==nil), No.33/34(誤バインド/早期return), No.35/56(probe登録), No.72(fast-path投票), No.74(off-by-one)
レビュー: packet-analyst + patrol-flow-reviewer 両方必須

---

## 0. 背景（実機 4ch 巡回ログ 2026/06/18 で確定）

- 現ゲーム版は **0x15 に SceneData 無し**（`SceneData=なし` ログ多数）。lineID(ch) は portMap のみが情報源
- No.72 で巡回中の portMap 投票は機能（実機: 投票27/quorum12）。No.74 で off-by-one も修正済
- **だが binding（serial↔uid）が 0/8 で全滅**。原因:
  - Identify(runStaggerProbe) は各台を**別ch**へ振り（5555→Ch1, 5557→Ch13...）、`probe.targetCh == 観測lineID` の一意照合で bind する
  - probe 先ch のポートは portMap に無い → lineID 解決せず → `onLineIDObserved` 発火せず → `matched 0 pending probes` → 0/8
  - Identify は currentChannel を立てず（runStaggerProbe は onChannelSwitch を呼ばない）、かつ 1ch=1台で quorum(2) も組めない → portMap を自己構築できない
- **binding に必要なのは uid のみ**（0x15 charId から取得でき、SceneData 無しでも観測できている）。欠けているのは lineID だけで、これは「どの台か」を一意特定する照合キーに使われている

## 1. 方針（B-β: ground-truth で lineID を解決）

Identify は **ground truth**（「serial S を ch C へ送った」）を持つ。1台ずつ既知chへ送るので、再接続した台の lineID は probeCh だと確定できる。これを使って lineID を直接解決し、既存の一意 probe 照合 binding（無変更）を成立させる。

- **binding 本体（MatchLineChange の probe.targetCh 照合）は一切変更しない** → inter-probe 取り違え耐性を温存
- native は **excludeUIDs で従来どおり弾く**（登録済み確認済・MatchLineChange 冒頭でチェック）
- probe モードは **Identify 中のみ**有効 → 通常巡回の挙動（quorum=2 投票）は無変更

## 2. 変更内容（diff レベル）

### 2-1. ncap/cap_device.go
- `CapDevice` に `probeMode bool` + 保護用ロック（`currentChannelMu` 流用 or 新規 atomic）を追加
- `SetProbeMode(on bool)` setter を追加（mumu から呼ぶ）
- **`maybeSubmitPortVote`** を拡張（呼び出し3箇所: C→S既存/C→S新規/fast-path はそのまま）:
  ```go
  func (cd *CapDevice) maybeSubmitPortVote(sess *session, serverAddr string, now time.Time) {
      if sess.lineID != 0 { return }
      cd.currentChannelMu.RLock()
      patrolCh := cd.currentChannel
      probe := cd.probeMode
      cd.currentChannelMu.RUnlock()
      if patrolCh == 0 { return }
      if probe {
          // ground truth: 1台ずつ既知chへ送る Identify 中なので、再接続セッションの
          // lineID を currentChannel(=probeCh) に直接確定する（quorum 不要）。
          sess.lineID = patrolCh   // 呼び出し元で sess.mu 保持済み
          return
      }
      label := sess.label
      go cd.submitPortVote(patrolCh, serverAddr, label, now)
  }
  ```
  - 注意: `maybeSubmitPortVote` は3箇所すべて sess.mu 保持下で呼ばれる（No.72 packet-analyst 確認済）。直接代入可
  - **§4-1.3/4-1.7 厳守**: onLineIDObserved の発火条件・sd==nil 早期return は変更しない。lineID を先に確定しておくことで、後続の 0x15 char9d(sd==nil パス)が `uidNewlySet && lineID!=0` で onLineIDObserved を発火する（実ログで fast-path → 0x15 の順序を確認済）

### 2-2. mumu/mumu.go
- `Patroller` に `onProbeMode func(bool)` + `SetOnProbeMode` setter を追加
- **`runStaggerProbe`**:
  - 冒頭で `if p.onProbeMode != nil { p.onProbeMode(true) }`、`defer` で `(false)`
  - 各台ループ内、`RecordPatrolMove` の直後・`SwitchGroup` の前に
    `if p.onChannelSwitch != nil { p.onChannelSwitch(probeCh) }` を呼び currentChannel=probeCh を立てる
    （No.74 と同様、再接続より前に正しいchを立てる）

### 2-3. main.go
- `guiServer.SetOnProbeMode(capDevice.SetProbeMode)` 相当の配線を追加（onChannelSwitch と同経路）
  - gui.Server 側にも `SetOnProbeMode` 委譲 + Patroller への転送が要る場合は追加

## 3. 状態遷移（Identify 1台分）

1. runStaggerProbe: probeMode=ON, currentChannel=probeCh, RecordPatrolMove(S, probeCh)
2. ADB switch_channel S → probeCh
3. S の game client 再接続 → handleServerToClientFast (fast-path) → `maybeSubmitPortVote` → **probeMode なので sess.lineID=probeCh 直接確定**
4. S の 0x15 charId 到着 → uidNewlySet=true, sess.lineID=probeCh → **onLineIDObserved(uid, probeCh) 発火**
5. mumu MatchLineChange: uid の lineID=probeCh が S の一意 probe(targetCh=probeCh) にマッチ → **bind 成立**
6. native が再接続しても excludeUIDs で弾かれ bind されない（lineID は誤設定されうるが、native は bind 対象外で影響なし）

## 4. 不変条件チェック（§4）

- §4-1.3: onLineIDObserved 発火条件 無変更 ✓
- §4-1.7: sd==nil 早期return 追加しない ✓（lineID を先に立てるだけ）
- §4-2.1: RecordPatrolMove は SwitchGroup 前 維持。onChannelSwitch も前に追加（No.74 と整合）✓
- §4-2.3: MatchLineChange の probe 時刻フィルタ 無変更 ✓
- §4-2.9: excludeUIDs で native 除外 維持 ✓
- probeMode は Identify 中のみ ON → 通常巡回の quorum 投票は無変更（patrol の native 安全性を温存）✓

## 5. 検証

- ncap ユニットテスト追加: probeMode ON 時に reconnect(lineID==0)→lineID=currentChannel 直接確定、OFF 時は従来どおり投票のみ
- `go build ./...` / `go vet ./...` / `go test ./...`
- `run_all.ps1` 全シナリオ PASS（probeMode は Identify 専用なので既存巡回 sim に影響しないこと）
- **実機 go run .**: Identify 実行 → `識別フェーズ完了: N/N 台バインド成立` を確認（現状 0/8 → 改善）
- packet-analyst（ncap）+ patrol-flow-reviewer（mumu）レビュー BLOCKING ゼロ

## 6. リスク

- ncap の lineID 設定経路に触れる（No.33/34 領域）→ probeMode のスコープを Identify 中に厳密限定。OFF 時は 1 行も挙動が変わらないことを diff で確認
- probeMode 中の native 干渉: lineID 誤設定されうるが bind は excludeUIDs で防ぐ。portMap には書かない（direct-set は sess.lineID のみ）ので汚染なし
- 取りこぼし: probeCh への再接続が遅れ、次の台の currentChannel に上書きされる前に bind できない場合 → runStaggerProbe の stagger 間隔（~7s）で吸収。必要なら「その台が bind するまで待ってから次へ」を追加（Phase 2 オプション）

## 7. コミット

No.75 単独。subagent 実装 → 主セッション diff 照合 → packet-analyst + patrol-flow-reviewer → build/vet/test + run_all → 実機 go run . → changelog → commit。
