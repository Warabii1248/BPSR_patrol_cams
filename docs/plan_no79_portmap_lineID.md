# No.79 実装仕様書: ch切替後 PostLoadReady が発火しない問題

## 問題

巡回で portMap に未登録のチャンネルへ切替えると、全デバイスがタイムアウトして次のチャンネルへスキップする。

実機ログ（2026/06/19）:
- ch=7: SUCCESS（portMap に既登録 → 0x2E PostLoadReady 4台確認）
- ch=9/14/21/30: TIMEOUT（55秒後スキップ）

---

## 根本原因（ログで確認済み）

### フロー（ch=9 の例）

```
18:22:11  SetCurrentChannel(9)  ← SwitchGroup 前に呼ぶ（No.74）
18:22:15  Instance-10 新セッション作成 (clientEndpoint=<local-ip>:port)
          fast-path: serverIP = "...:20102", lineID = 0
          [0x15] charId=41372054
          → Instance-9 にセッションマージ
```

### mergeSessionIfDuplicate の現状（cap_device.go L793-810）

```go
if newSess.lineID != 0 {            // newSess.lineID==0 なのでスキップ → 旧 lineID=7 が残る
    existing.lineID = newSess.lineID
    ...
}
if newSess.serverIP != "" {
    existing.serverIP = newSess.serverIP   // ch=9 サーバーに更新される
    existing.serverIPSetAt = newSess.serverIPSetAt
}
// postLoadFiredForLineID は newSess==0 → 既存値(7)が残る
```

### マージ後の Instance-9 の状態

| フィールド | 値 | 備考 |
|---|---|---|
| serverIP | "...:20102" | ch=9 サーバー（更新される）|
| lineID | 7 | ch=7 の値が**残る** |
| postLoadFiredForLineID | 7 | ch=7 で PostLoadReady 発火済み |

### 0x2E PostLoadReady 条件（processSyncToMeDeltaInfo L1788 付近）

```go
if uid != 0 && sess.lineID != 0 &&
   cd.onPostLoadReady != nil &&
   sess.postLoadFiredForLineID != sess.lineID {  // 7 == 7 → false → 発火しない
```

ch=9 サーバーの 0x2E が届いても「ch=7 でもう発火済み」と判定 → moveSignal 送られず → タイムアウト。

### tryPortMapLineID が効かない理由

```go
func (cd *CapDevice) tryPortMapLineID(sess *session) {
    if sess.lineID != 0 { return }  // lineID=7 → 即リターン
```

portMap に ch=9 が確定しても lineID!=0 の既存セッションには反映されない。

---

## 並行性の前提（重要・設計根拠）

- **packet 処理は単一ゴルーチン**（cap_device.go L652-660 の単一コンシューマ）。
  `handlePacket` 系内の lineID 書き込みは sess.mu なしでも互いに安全。sess.mu は GUI 読み取り対策。
- **submitPortVote は別ゴルーチン**（L450 `go cd.submitPortVote(...)`）。sess.mu を一切保持していない。
- **mergeSessionIfDuplicate** は `newSess.mu`（呼び出し元保持）→ `sessionsMu.Lock(write)` の順でロックする。
- 既存のロック順序の輪を作らないため、新ヘルパ `applyPortMapToSessions` は
  **sessionsMu.RLock 下でセッションポインタをスナップショット → RUnlock → 各 sess.mu を個別取得**
  とする（sessionsMu と sess.mu を同時保持しない）。これで mergeSessionIfDuplicate とのデッドロックを回避する。

---

## 修正仕様

### Fix-A: mergeSessionIfDuplicate — serverIP 変化時に lineID リセット

**ファイル**: `ncap/cap_device.go` / `mergeSessionIfDuplicate`（L799 の serverIP 更新ブロック）

**変更前**:
```go
if newSess.serverIP != "" {
    existing.serverIP = newSess.serverIP
    existing.serverIPSetAt = newSess.serverIPSetAt
}
```

**変更後**:
```go
if newSess.serverIP != "" {
    if existing.serverIP != newSess.serverIP {
        // サーバー変更（ch切替）検出 → 旧 lineID を破棄して再決定
        log.Printf("[%s] マージ: serverIP変化 [%s]→[%s], lineID=%d リセット",
            existing.label, existing.serverIP, newSess.serverIP, existing.lineID)
        existing.lineID = 0
        existing.postLoadFiredForLineID = 0
    }
    existing.serverIP = newSess.serverIP
    existing.serverIPSetAt = newSess.serverIPSetAt
    cd.tryPortMapLineID(existing) // portMap に既登録なら即 lineID 補完
}
```

注意:
- このブロックは既存 L805 の `postLoadFiredForLineID` 引き継ぎより**前**にある。newSess は新規 fast-path なので newSess.postLoadFiredForLineID==0 → L805 は existing を上書きしない（Fix-A のリセットが保たれる）。
- packet ゴルーチン内のため sess.mu なしで existing を書く（既存 L794 と同じ前提）。

### Fix-B/C 共通: applyPortMapToSessions ヘルパ追加（デッドロック安全）

**ファイル**: `ncap/cap_device.go`（ApplyPortMapUpdate の近く）

```go
// applyPortMapToSessions は serverIP 一致かつ lineID 未確定のセッションへ ch を反映する。
// sessionsMu と sess.mu を同時保持しないようスナップショット方式で実装する
// （mergeSessionIfDuplicate の newSess.mu→sessionsMu 順とのデッドロック回避）。
func (cd *CapDevice) applyPortMapToSessions(ch uint32, serverIP string) {
    cd.sessionsMu.RLock()
    seen := make(map[*session]bool, len(cd.sessions))
    snapshot := make([]*session, 0, len(cd.sessions))
    for _, sess := range cd.sessions {
        if !seen[sess] {
            seen[sess] = true
            snapshot = append(snapshot, sess)
        }
    }
    cd.sessionsMu.RUnlock()

    for _, sess := range snapshot {
        sess.mu.Lock()
        if sess.serverIP == serverIP && sess.lineID == 0 {
            sess.lineID = ch
            log.Printf("[%s] PortMap確定: lineID=Ch%d (serverIP=%s)", sess.label, ch, serverIP)
        }
        sess.mu.Unlock()
    }
}
```

### Fix-B: ApplyPortMapUpdate — 既存セッションへ反映（GUI 手動確認パス）

**変更前**:
```go
func (cd *CapDevice) ApplyPortMapUpdate(ch uint32, serverIP string) {
    if cd.portMap != nil {
        cd.portMap.Update(ch, serverIP)
    }
}
```

**変更後**:
```go
func (cd *CapDevice) ApplyPortMapUpdate(ch uint32, serverIP string) {
    if cd.portMap != nil {
        cd.portMap.Update(ch, serverIP)
    }
    cd.applyPortMapToSessions(ch, serverIP)
}
```

### Fix-C: 巡回中は portMap クォーラム成立で自動確定

**目的**: portMap 未登録 ch でもクォーラム成立（≈5秒）で即 lineID を確定し、55秒タイムアウト前に PostLoadReady を発火させる。

**(1) CapDevice struct にフィールド追加**（currentChannelMu で保護・atomic 不使用で新規 import を避ける）:

L191 `probeMode bool` の直後に追加:
```go
// patrolActive は巡回中のみ true。true の間は submitPortVote が quorum 成立時に
// portMap を GUI 確認なしで自動確定する。currentChannelMu で保護する。
patrolActive bool
```

**(2) SetPatrolActive メソッド追加**（SetProbeMode 付近）:
```go
// SetPatrolActive は巡回の開始/停止を通知する。
// true の間、portMap クォーラム成立は GUI 確認を待たず自動確定される。
func (cd *CapDevice) SetPatrolActive(active bool) {
    cd.currentChannelMu.Lock()
    cd.patrolActive = active
    cd.currentChannelMu.Unlock()
}
```

**(3) submitPortVote のクォーラム成立ブロック分岐**（L516-530）:

**変更前**:
```go
if len(existing) >= portVoteQuorum {
    voteCount := len(existing)
    winServerIP := existing[len(existing)-1].serverIP
    delete(cd.portVotes, port)

    fn := cd.portMapPendingFn
    if fn != nil {
        log.Printf("[PortMap] クォーラム成立: ... → 確認待ち", ...)
        go fn(ch, winServerIP, oldIP, voteCount)
    } else {
        log.Printf("[PortMap] クォーラム成立: ... → 更新", ...)
        cd.portMap.Update(ch, winServerIP)
    }
}
```

**変更後**:
```go
if len(existing) >= portVoteQuorum {
    voteCount := len(existing)
    winServerIP := existing[len(existing)-1].serverIP
    delete(cd.portVotes, port)

    cd.currentChannelMu.RLock()
    patrolling := cd.patrolActive
    cd.currentChannelMu.RUnlock()

    fn := cd.portMapPendingFn
    if patrolling {
        // 巡回中は GUI 確認を待たず自動確定（55秒タイムアウト対策）
        log.Printf("[PortMap] クォーラム成立: ch=%d port=%s serverIP=%s (%d台) → 巡回中自動確定",
            ch, port, winServerIP, voteCount)
        if cd.portMap != nil {
            cd.portMap.Update(ch, winServerIP)
        }
        cd.applyPortMapToSessions(ch, winServerIP)
        if fn != nil {
            go fn(ch, winServerIP, oldIP, voteCount) // GUI 表示は継続（確認は不要）
        }
    } else if fn != nil {
        log.Printf("[PortMap] クォーラム成立: ch=%d port=%s serverIP=%s (%d台確認) → 確認待ち",
            ch, port, winServerIP, voteCount)
        go fn(ch, winServerIP, oldIP, voteCount)
    } else {
        log.Printf("[PortMap] クォーラム成立: ch=%d port=%s serverIP=%s (%d台確認) → 更新",
            ch, port, winServerIP, voteCount)
        if cd.portMap != nil {
            cd.portMap.Update(ch, winServerIP)
        }
    }
}
```

注: applyPortMapToSessions は portVotesMu 保持下で呼ぶが、内部で portVotesMu を再取得しないためデッドロックなし。

### Fix-C 配線（既存 SetOnProbeMode パターンに合わせる）

`SetPatrolStartFn` 等は存在しない。`onProbeMode` と同じ三段配線で実装する。

**(4) mumu/mumu.go**:
- struct（L903 `onProbeMode func(bool)` 付近）に追加:
  ```go
  onPatrolActive func(bool) // 巡回 Start/Stop 時コールバック
  ```
- Setter 追加（SetOnProbeMode 付近）:
  ```go
  func (p *Patroller) SetOnPatrolActive(fn func(bool)) {
      p.mu.Lock()
      p.onPatrolActive = fn
      p.mu.Unlock()
  }
  ```
- `Start()`（L2016）: チャンネル空チェックを通過し巡回確定後（`p.Stop()` より後・goroutine 起動前の適切な箇所）に `true` で発火。コールバックは p.mu を取らずに呼ぶ（デッドロック回避のためローカル変数経由）:
  ```go
  // Start() 内、巡回状態確定後
  p.mu.RLock(); fn := p.onPatrolActive; p.mu.RUnlock()
  if fn != nil { fn(true) }
  ```
- `Stop()`（L1899）: `p.status.Running = false` 確定後（mu Unlock 後）に `false` で発火:
  ```go
  p.mu.RLock(); fn := p.onPatrolActive; p.mu.RUnlock()
  if fn != nil { fn(false) }
  ```
  （Stop は Start から `p.Stop()` として呼ばれるため、false→直後に true となるが順序は保たれる。冪等。）

**(5) gui/gui.go**:
- struct（L56 `probeModeNotifyFn func(bool)` 付近）に追加:
  ```go
  patrolActiveNotifyFn func(bool)
  ```
- Setter 追加（SetProbeModeNotifyFn 付近）:
  ```go
  func (s *Server) SetPatrolActiveNotifyFn(fn func(bool)) {
      s.patrolActiveNotifyFn = fn
  }
  ```
- Run() の relay 配線（L194 SetOnProbeMode の隣・`if s.patrolEnabled` ブロック内）:
  ```go
  s.patroller.SetOnPatrolActive(func(on bool) {
      s.mu.RLock()
      fn := s.patrolActiveNotifyFn
      s.mu.RUnlock()
      if fn != nil {
          fn(on)
      }
  })
  ```

**(6) main.go**（L349 SetProbeModeNotifyFn の隣）:
```go
guiServer.SetPatrolActiveNotifyFn(func(on bool) {
    capDevice.SetPatrolActive(on)
})
```

---

## 変更ファイル一覧

| ファイル | 変更内容 |
|---|---|
| `ncap/cap_device.go` | Fix-A mergeSessionIfDuplicate / Fix-B ApplyPortMapUpdate / Fix-B,C applyPortMapToSessions / Fix-C patrolActive・SetPatrolActive・submitPortVote 分岐 |
| `mumu/mumu.go` | onPatrolActive フィールド・SetOnPatrolActive・Start/Stop で発火 |
| `gui/gui.go` | patrolActiveNotifyFn フィールド・SetPatrolActiveNotifyFn・relay 配線 |
| `main.go` | SetPatrolActiveNotifyFn → capDevice.SetPatrolActive 配線 |

---

## 不変条件チェック（§4）

| 条件 | 影響 | 判定 |
|---|---|---|
| §4-1.3 onLineIDObserved 発火条件 | 本変更は onLineIDObserved を呼ばない（lineID 直接設定のみ） | OK |
| §4-1.4 SceneData なしパス | processSyncContainerData 非変更。mergeSessionIfDuplicate のみ変更 | OK |
| §4-1.6 fast-path 新規時 tryPortMapLineID | 既存呼出維持。Fix-A はマージ時の追加補完 | OK |
| §4-2.12 postLoadFiredForLineID 同 lineID 1回 | lineID リセット時に同時リセット → 新 ch で再発火可能（意図通り） | OK |
| §4-3 config 互換 | config スキーマ変更なし | OK |

---

## 検証手順（§10）

1. `go build ./...` エラーなし
2. `go vet ./...` 警告ゼロ
3. `.\run_all.ps1` 全7シナリオ PASS（mumu/ 変更あり → 必須）
4. （任意）golden があれば `pcap-replay -golden` で ncap 回帰比較
5. 実機: portMap 未登録 ch で巡回 → クォーラム成立後すぐ PostLoadReady 4台 → 次 ch へ進む

---

## リグレッションリスク評価

- portMap 登録済み ch: `tryPortMapLineID` が session 作成時に即 lineID 決定 → Fix-C 分岐に到達せず、従来挙動。
- 非巡回時（patrolActive=false）: submitPortVote は従来どおり確認待ち（portMapPendingFn 経由）。挙動変化なし。
- デッドロック: applyPortMapToSessions はスナップショット方式で sessionsMu と sess.mu を同時保持しない → mergeSessionIfDuplicate との輪なし。
- 残存レース: Fix-A（packet goroutine, mu なし lineID=0）と Fix-C（別 goroutine, mu 下 lineID=ch）の交錯は self-heal（lineID==0 ガードで次パケット/次投票が再解決）。既存コードの並行性ポリシーと同等。
