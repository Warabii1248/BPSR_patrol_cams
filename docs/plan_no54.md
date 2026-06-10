# 実装プラン No.54〜57: ネイティブ干渉シグナル消失・dwell下限・wait loop ガード修正

実装者向け仕様書（diff レベル）。背景調査は 2026/06/10 のレビューで完了済み。
**このファイルの仕様どおりに実装すること。解釈の余地がある場合は実装前にユーザーへ質問する。**

---

## 共通ルール（全 Phase 必須）

1. **1 Phase = 1 commit = changelog 1 エントリ**（No.54 から連番）
2. 各 Phase 完了時に `go build ./...` と `go vet ./...` をエラーゼロで通す
3. mumu/ 変更後は **patrol-flow-reviewer** subagent、ncap/ 変更後は **packet-analyst** subagent でレビューしてからコミット
4. **禁止事項**:
   - `pb/bp.pb.go` の編集
   - 既存の wait loop 3箇所（buffered drain / Phase1 / Phase2）、`MatchLineChange`、`processSyncContainerData` のロジック変更（本プランのスコープ外）
   - 早期 return の追加（CLAUDE.md §5: No.33→34 リグレッションの起源）
   - 周辺コードのリファクタ・整形・コメント追加（指定された変更のみ行う）
5. 本仕様のコードはレビュー時点の行番号基準。実装時は行番号でなく**コード内容で位置を特定**すること

---

## Phase 1（No.54）: dwell 下限修正 + extraWait lineID ガード + ドキュメント同期

### 1-1. dwell 下限クランプ修正（mumu/mumu.go・2箇所）

**問題**: `patrol_dwell_secs: 2` が 5s に切り上げられる（コメントは「未設定なら最低5秒」だが実装は全値クランプ）。

**箇所A** — `Start()` 内の初期化（現 2005-2009 行付近）:

```go
// 変更前
// dwell: cfg.DwellDuration が未設定なら最低5秒
dwell := cfg.DwellDuration
if dwell < 5*time.Second {
	dwell = 5 * time.Second
}

// 変更後
// dwell: 未設定(<=0)なら5秒、設定済みなら最低1秒（ADB連打防止の安全下限）
dwell := cfg.DwellDuration
if dwell <= 0 {
	dwell = 5 * time.Second
} else if dwell < time.Second {
	dwell = time.Second
}
```

**箇所B** — 巡回ループ内の毎サイクル再取得（現 2122-2127 行付近、`dwell = currentCfg.DwellDuration` の直後）。箇所Aと同一ロジックに変更（変数は `currentCfg.DwellDuration`）。

### 1-2. extraWait（発行前再確認）ループに lineID ガード追加（mumu/mumu.go）

**問題**: sig を消費するループは4箇所あるが、No.44 の stale lineID ガードが「発行前追加待ち」ループ（`extraWait:` ラベル、現 2531-2547 行付近）にだけ無い。前サイクルの遅延発火シグナルが got にカウントされる。

```go
// 変更前
case msg := <-sig:
	if msg.t.After(switchStartAt) && !respondedSet[msg.label] {
		got++
		respondedSet[msg.label] = true
		log.Printf("[MuMu] 巡回: Ch%d [lineID+delay] %s (%d/%d台) ← 発行前", ch, msg.label, got, need)
	}

// 変更後（他3ループと同一パターン）
case msg := <-sig:
	if msg.t.After(switchStartAt) &&
		(msg.lineID == 0 || msg.lineID == ch) &&
		!respondedSet[msg.label] {
		got++
		respondedSet[msg.label] = true
		log.Printf("[MuMu] 巡回: Ch%d [lineID+delay] %s (%d/%d台) ← 発行前", ch, msg.label, got, need)
	} else if msg.t.After(switchStartAt) && msg.lineID != 0 && msg.lineID != ch {
		debuglog.Vlogf("巡回", "  → stale lineID=%d skipped (target=%d, serial=%s)", msg.lineID, ch, msg.label)
	}
```

### 1-3. CLAUDE.md 同期

- §4-2.11: 「wait loop（3箇所）」→「wait loop（4箇所: buffered drain / フェーズ1 / フェーズ2 / 発行前追加待ち）」
- §4-4.4: `btn-ch-save` / `_setChSaveBtnDisabled` の項目を削除（No.46 で該当コードは削除済み）

---

## Phase 2（No.55）: ネイティブクライアント干渉対策

### 2-1. UUIDログ fallback 経路に UID 検証を追加（gui/gui.go + mumu/mumu.go）

**問題**: ネイティブクライアントの ch 移動で `[Instance-K][0x2E] UUID=` ログが出ると、
`guiWriter.Write` → `NotifyChMovePacket` が excludeUIDs 未適用・lineID=0 で moveSignal に
phantom シグナルを注入する。

**重要**: `!HasBinding()` のような経路全体ガードは禁止（No.12 リグレッション）。
per-uid 解決方式で実装する。**uid が取れない行は従来動作を完全維持すること。**

**(a) gui/gui.go — UID 抽出ヘルパー追加**（`extractInstanceLabel` の直後に配置）:

```go
// extractUIDFromLine は "[0x2E] UUID=... (UID=12345)" 形式のログ行から UID を抽出する。
// 見つからない・解析失敗時は 0 を返す（呼び出し側で従来動作にフォールバック）。
func extractUIDFromLine(line string) uint64 {
	idx := strings.Index(line, "(UID=")
	if idx < 0 {
		return 0
	}
	rest := line[idx+5:]
	end := strings.IndexByte(rest, ')')
	if end <= 0 {
		return 0
	}
	v, err := strconv.ParseUint(rest[:end], 10, 64)
	if err != nil {
		return 0
	}
	return v
}
```

`strconv` の import 追加を忘れない。

**(b) gui/gui.go — guiWriter.Write の呼び出し変更**:

```go
// 変更前
w.srv.patroller.NotifyChMovePacket(extractInstanceLabel(line))
// 変更後
w.srv.patroller.NotifyChMovePacket(extractInstanceLabel(line), extractUIDFromLine(line))
```

**(c) gui/gui.go — `Server.NotifyChMovePacket` ラッパー（現 376-381 行付近）**:
シグネチャを `(label string, uid uint64)` に変更し透過転送。
変更前に `grep -rn "NotifyChMovePacket"` で**全呼び出し元**を確認し、すべて更新すること
（main.go / cmd/chat-reporter にも呼び出しがあれば同様に更新）。

**(d) mumu/mumu.go — `Patroller.NotifyChMovePacket` 本体**:

```go
// NotifyChMovePacket は guiWriter が "[0x2E] UUID=" ログ行を検出したとき呼ばれる fallback 経路。
// uid が判明している場合は除外UID・バインドを検証し、phantom シグナル（同一PC上の
// ネイティブクライアント等）を弾く。uid==0（解析不能な行）は従来動作を維持する。
func (p *Patroller) NotifyChMovePacket(instanceLabel string, uid uint64) {
	if uid != 0 {
		// 除外 UID → 破棄
		p.excludeUIDsMu.RLock()
		excluded := p.excludeUIDs[uid]
		p.excludeUIDsMu.RUnlock()
		if excluded {
			return
		}
		// バインド済み uid → 確実な serial で送信
		p.serialUIDMu.RLock()
		serial := p.uidToSerial[uid]
		p.serialUIDMu.RUnlock()
		if serial != "" {
			p.notifyMoveSignal(serial, 0, time.Now())
			return
		}
		// uid 判明かつ未バインド → 巡回デバイスに紐付けられないため破棄（phantom 防止）
		return
	}
	// uid 不明: 従来どおり label → serial 解決で送信（既存コードをそのまま残す）
	if instanceLabel == "" {
		return
	}
	serial := instanceLabel
	p.serialLabelMu.RLock()
	if p.labelToSerial != nil {
		if s := p.labelToSerial[instanceLabel]; s != "" {
			serial = s
		}
	}
	p.serialLabelMu.RUnlock()
	p.notifyMoveSignal(serial, 0, time.Now())
}
```

**挙動変更点（ユーザー承認済み）**: uid が判明していて未バインドのシグナルは破棄される。
識別（Identify）前のデバイスはこの fallback 経路ではカウントされなくなるが、
正規経路（NotifyPostLoadReady）も元々バインド前提のため運用上の影響なし。

### 2-2. 0x2E donor lineID 補完を同一 userUID 限定に（ncap/cap_device.go）

**問題**: `processSyncToMeDeltaInfo` 末尾の lineID 補完（現 1734-1756 行付近）の donor は
「同 clientIP で confirmedAt 最新」= MuMu NAT では直前に ch 移動したネイティブの
セッションが選ばれ、別インスタンスの ch を借用してしまう。

**変更**: 外側条件に `sess.userUID != 0` を追加し、donor の userUID 一致を必須にする。

```go
// 変更前
if sess.lineID == 0 {
	if donor := cd.findConfirmedSessionByClientIP(sess.clientIP); donor != nil && donor != sess {
		donor.mu.Lock()
		donorLine := donor.lineID
		donorMap := donor.mapID
		donor.mu.Unlock()
		if donorLine != 0 {

// 変更後
if sess.lineID == 0 && sess.userUID != 0 {
	if donor := cd.findConfirmedSessionByClientIP(sess.clientIP); donor != nil && donor != sess {
		donor.mu.Lock()
		donorLine := donor.lineID
		donorMap := donor.mapID
		donorUID := donor.userUID
		donor.mu.Unlock()
		if donorLine != 0 && donorUID == sess.userUID {
```

ブロック内の残り（lineID/mapID コピー、ログ、onLineIDObserved 発火）は**一切変更しない**。
本来の用途（再起動後の同一アカウント補完）は uid 一致で引き続き機能する。

### 2-3. exclude_uids の検証（ユーザー作業・コード変更なし）

ネイティブクライアント起動中のログ `[0x2E] UUID=... (UID=N)` で実 UID を確認し、
config.json の `exclude_uids`（現在 `[430314]`）と一致するか検証。
ズレていれば GUI 設定画面の「除外 UID」から修正する。

---

## Phase 3（No.56）: 体質改善

### 3-1. 巡回本ループの RecordPatrolMove を SwitchGroup 前へ（mumu/mumu.go）

**問題**: CLAUDE.md §4-2.1 は「RecordPatrolMove は SwitchGroup の前」だが、No.35 で
修正されたのは runStaggerProbe のみ。巡回本ループは SwitchGroup 全完了後
（`switchDoneAt` 時刻）に登録しており、(1) 巡回中の再バインド不成立、
(2) ExpectedCh 更新遅れによる一時的 Mismatch ログを引き起こす。

**変更**: `switchStartTimes` 更新ブロック（`p.switchStartTimesMu.Unlock()`）の直後・
SwitchGroup の for ループの**前**に以下を移動:

```go
// per-device CH バインド用: 各 serial の切替予約を ADB 発行前に登録する（§4-2.1, No.35 と同根）。
// 既到達スキップ分（switchTargets 外）も ExpectedCh 更新のため targets 全件で呼ぶ（No.29）。
for _, ser := range targets {
	p.RecordPatrolMove(ser, ch, switchStartAt)
}
```

注意点:
- 対象は **`targets` 全件**（`switchTargets` ではない）— No.29 不変条件
- 時刻は **`switchStartAt`**（旧コードの `switchDoneAt` から変更）
- `switchDoneAt := time.Now()` 直後にある**旧 RecordPatrolMove ループを削除**する
- `runStaggerProbe` 内の RecordPatrolMove は**触らない**（No.35 で修正済み）

### 3-2. mergeSessionIfDuplicate にロード判定フィールド引き継ぎ（ncap/cap_device.go）

**問題**: マージ時に `postLoadFiredForLineID` / `lineIDChangedAt` が existing にコピー
されず、0x2E 先着パターンで PostLoadReady が同一 lineID で二重発火し得る。

**変更**: `mergeSessionIfDuplicate` 内の `existing.userUID = newSess.userUID` の直後に追加:

```go
// ロード完了シグナルの dedup 状態を引き継ぐ（同一 lineID 二重発火防止・No.44 関連）
if newSess.postLoadFiredForLineID != 0 {
	existing.postLoadFiredForLineID = newSess.postLoadFiredForLineID
}
if existing.lineIDChangedAt.Before(newSess.lineIDChangedAt) {
	existing.lineIDChangedAt = newSess.lineIDChangedAt
}
```

---

## 動作検証（各 Phase 後）

| Phase | 確認方法 |
|---|---|
| 1-1 | `patrol_dwell_secs: 2` で巡回し、dwell_wait フェーズが約2sになること（サイクル全体は ADB 処理分上乗せされる点は正常） |
| 1-2 | 通常巡回が従来どおり完走すること。debug_verbose で stale skip ログが extraWait 中にも出得ること |
| 2-1 | 巡回 wait_0x2e 中にネイティブクライアントで ch 移動 → 全台分のシグナルが正しくカウントされ「移動失敗」「未応答」が出ないこと |
| 2-2 | 巡回が従来どおり完走 + ネイティブ ch 移動時に `[0x2E] lineID補完` で別インスタンスの ch が借用されないこと |
| 3-1 | 巡回サイクル毎の `[Patroller][Mismatch]` 一時ログが消えること。識別済み環境で巡回が完走すること |
| 3-2 | `[PostLoadReady]` ログ（debug_verbose）が 1 serial / 1 lineID につき 1 回であること |

## changelog 採番

- Phase 1 → No.54、Phase 2 → No.55、Phase 3 → No.56（テンプレは CLAUDE.md §7）
