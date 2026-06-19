# No.80 実装仕様書: screen モードを 0x2E 非依存にする（ch切替起点の画面判定）

## ユーザー指示
「screen 判定の時は 0x2E に関係なく認識するようにして」

---

## 現状の問題（実機ログ 2026/06/19 20:00〜 で確認）

### completion の実態（2経路とも 0x2E 起点）

1. **正規経路**: capDevice が 0x2E 受信 → `onPostLoadReady` → `guiServer.NotifyPostLoadReady` →
   `patroller.NotifyPostLoadReady` → mode 別 strategy（screen/time/either）→ `notifyMoveSignal`
2. **fallback 経路**: guiWriter が `[0x2E] UUID=` ログ文字列を検出 → `NotifyChMovePacket` →
   `notifyMoveSignal`（**即時・strategy を経由しない**・lineID=0）

### ログから判明した事実

- `[DBG][0x2E]` ログ（NotifyPostLoadReady 冒頭の Vlogf）が **0 件** = 正規経路は一度も発火していない
- `[Instance-N][0x2E] UUID=あり` は **Ch14 のみ 8 台**、Ch21/33/37/45… では **0 件**
- Ch14 の完了は fallback 経路（NotifyChMovePacket = ログ検出）で got した（画面判定を経由していない）
- 画面判定の verbose ログ（`[Screen]` / `黒消失` / `画面判定タイムアウト`）が **皆無**（verbose=true なのに）

### 結論

screen モード（config.local.json: `load_detect_mode=screen`）でも、completion は 0x2E の到来が起点。
0x2E が来ない ch（Ch21 以降）では画面判定が一切起動せず「移動失敗（完了シグナルなし）」でスキップ。
**= ユーザーの言う「画面認識判定が働いていない」の直接原因。**

---

## 新設計（screen モード時のみ・time/either は不変）

screen モードでは completion を **ch切替（SwitchGroup 完了）起点の画面判定**に切り替え、0x2E に依存しない。

### 変更点（すべて mumu/mumu.go）

#### (1) 巡回ループ: SwitchGroup 完了後に画面ポーリングを起動

`Start()` 内の巡回ループ、L2358（`switchDoneAt := time.Now()`）の直後・完了待ちループ（L2381〜）の前に挿入:

```go
// screen モード: ch切替起点で各台の画面判定を起動（0x2E 非依存・No.80）。
// time/either は従来どおり 0x2E（NotifyPostLoadReady / NotifyChMovePacket）起点。
var screenCancel context.CancelFunc
if strings.ToLower(currentCfg.LoadDetectMode) == "screen" {
    var screenCtx context.Context
    screenCtx, screenCancel = context.WithCancel(ctx)
    for _, ser := range switchTargets {
        ser := ser
        go p.runScreenStrategyAtSwitch(screenCtx, ser, ch, switchDoneAt, currentCfg)
    }
}
```

完了待ちループ（フェーズ1/2）を抜けた後、次 ch に進む前に `screenCancel()` を呼び、
当該 ch のポーリング goroutine を確実に停止する（次 ch へ残骸が漏れない）。

#### (2) runScreenStrategyAtSwitch（新規・screen.go）

ch切替直後はまだ通常画面（非黒）の可能性があるため、現状の `waitForBlackGone`（初回非黒で即完了）を
そのまま使うと誤完了する。**2段階**にする:

```go
// runScreenStrategyAtSwitch は ch切替起点の画面判定（0x2E 非依存）。
//  段階1: ロード画面（黒）に入るのを待つ（grace。来なければ段階2へ直行）
//  段階2: 黒画面が消える（ロード完了）のを待つ → notifyMoveSignal(serial, ch)
// いずれもタイムアウト時は強制進行（永久待ち防止）。
func (p *Patroller) runScreenStrategyAtSwitch(ctx context.Context, serial string, lineID uint32, startedAt time.Time, cfg Config) {
    // 段階1: 黒入り待ち（grace = ScreenPollInterval×N、上限あり）
    p.waitForBlackEnter(ctx, serial, cfg)   // 黒検知 or grace 超過で抜ける
    if ctx.Err() != nil { return }
    // 段階2: 黒消失待ち（既存 waitForBlackGone を流用）
    ok := p.waitForBlackGone(ctx, serial, cfg)
    if ctx.Err() != nil { return }
    if !ok {
        log.Printf("[MuMu] 画面判定タイムアウト serial=%s → 強制進行", serial)
    } else {
        debuglog.Vlogf("0x2E", "[Screen] serial=%s ロード完了検知 経過%.1fs", serial, time.Since(startedAt).Seconds())
    }
    p.notifyMoveSignal(serial, lineID, time.Now())
}
```

`waitForBlackEnter`（新規）: 黒画面を検知するまで or grace 上限までポーリング。
grace は「黒入りラグ」吸収用の短い上限（例: `max(ScreenPollInterval×6, 3s)`）。
grace 超過（黒を一度も観測しない）でも段階2へ進む（黒なしでロード完了済みのケースを救済）。

> ⚠ 実機調整ポイント: 段階1の grace 値と、`waitForBlackGone` の初回非黒即完了の扱いは
> 実機のロード画面遷移（ENTER→黒 のラグ）に依存する。初回実走行後にログを見て調整する。

#### (3) NotifyPostLoadReady: screen モードでは strategy を起動しない

L1136 の switch を変更。screen モードは巡回ループ側で起動するため、0x2E 起点では起動しない:

```go
switch mode {
case "screen":
    // No.80: screen は ch切替起点（runScreenStrategyAtSwitch）で起動する。
    // 0x2E 起点では何もしない（二重 moveSignal 防止）。
    return
case "either":
    p.runEitherStrategy(ctx, serial, lineID, fireAt, cfg)
default: // "time"
    p.runTimeStrategy(ctx, serial, lineID, fireAt)
}
```
（cancel/postLoadCancels 登録より前に return するか、登録後 return かは実装で整合させる）

#### (4) NotifyChMovePacket: screen モードでは moveSignal を抑制

`NotifyChMovePacket`（L957）冒頭に追加:

```go
// No.80: screen モードでは 0x2E ログ起点の即時 moveSignal を無効化する
// （ch切替起点の画面判定のみで完了を判定するため）。
p.mu.RLock()
mode := strings.ToLower(p.cfg.LoadDetectMode)
p.mu.RUnlock()
if mode == "screen" {
    return
}
```

> §4-4.2 は「guiWriter.Write 側の NotifyChMovePacket **呼び出し**にガードを付けるな」という制約。
> 本変更は NotifyChMovePacket **内部**で screen モード時に抑制するもので、gui/ のコードは触らない → §4-4.2 抵触なし。

---

## 不変条件チェック（§4-2）

| 条件 | 影響 | 判定 |
|---|---|---|
| §4-2.1 RecordPatrolMove は SwitchGroup 前 | 不変（既存位置のまま） | OK |
| §4-2.5 moveSignal 時刻フィルタは switchStartAt 基準 | 画面判定は switchDoneAt 後に起動、notifyMoveSignal の t は now（>switchStartAt）| OK |
| §4-2.6 完了シグナルは NotifyPostLoadReady 経路 | **screen モードのみ** ch切替起点に変更。time/either は不変 | 仕様変更（意図的） |
| §4-2.10 MatchLineChange は notifyMoveSignal しない | 不変 | OK |
| §4-2.11 moveSignalMsg は lineID 携帯 | runScreenStrategyAtSwitch は lineID=ch を渡す | OK |
| §4-2.12 NotifyPostLoadReady 同 lineID 1回 | screen モードでは NotifyPostLoadReady 無効化のため無関係。time/either 不変 | OK |
| §4-4.2 guiWriter の NotifyChMovePacket 呼び出しガード禁止 | gui/ は無変更。抑制は NotifyChMovePacket 内部 | OK |

---

## 検証（§10）

- `go build ./...` / `go vet ./...`
- `.\run_all.ps1` の **screen_mode シナリオ**（load_detect_mode=screen）が PASS すること。
  - sim は `onEnter` で `SetScreenState(black)` → post_load_delay 後 `normal`。
    新設計は段階1で黒検知 → 段階2で normal 検知 → 完了。NotifyPostLoadReady 注入は screen モードで無視される。
  - 既存 screen_mode の assert（`移動失敗` forbid / `all_devices_actual_ch_follows`）が PASS するか。
- 他シナリオ（time モード）が退行しないこと（NotifyPostLoadReady / NotifyChMovePacket の time 経路不変）。
- 実機: `go run . -l`（screen モード）で Ch21 以降も画面判定で完了し巡回が進むこと + ログに `[Screen] ロード完了検知` が出ること。

---

## 確認したい点（実装前にユーザー判断が必要なら）

1. **段階1（黒入り待ち）の要否**: 実機で ch切替後すぐ画面ポーリングして、ロード画面（黒）に入る前の
   通常画面を「即ロード完了」と誤検知する懸念がある。本仕様は2段階（黒入り→黒消失）で防ぐ案。
   実機のロード遷移が「ENTER 後ほぼ即黒」なら段階1は短い grace で十分。
2. **screen モードで 0x2E を完全無視してよいか**: 本仕様は NotifyPostLoadReady / NotifyChMovePacket の
   moveSignal を screen モードで抑制する（指示「0x2E に関係なく」に従う）。time/either は不変。

---

## 変更ファイル

| ファイル | 変更 |
|---|---|
| `mumu/mumu.go` | 巡回ループに screen ポーリング起動 + screenCancel / NotifyPostLoadReady screen ガード / NotifyChMovePacket screen ガード |
| `mumu/screen.go` | runScreenStrategyAtSwitch + waitForBlackEnter 新規 |
