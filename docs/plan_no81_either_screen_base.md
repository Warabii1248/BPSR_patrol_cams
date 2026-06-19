# No.81 実装仕様書: either を screen ベース + 0x2E 保険にする

## ユーザー指示
「either モードは screen 判定が不具合が起きた時のための保険として 0x2E も使う。
順番としては screen モード起動 → 0x2E 待ち → 基本的に先に 0x2E が来る → screen 判定 or 時間経過で判定。
screen が無反応でも進めるようにする」
「逆に 0x2E が来なくても screen が判定できれば進むように。」

→ 要するに **screen 判定と 0x2E のどちらか一方でも成立すれば進む**（OR 条件・先勝ち）。
screen が無反応でも 0x2E（と時間経過）で進み、0x2E が来なくても screen 判定で進む。

---

## 現状の問題

either は `runEitherStrategy`（**0x2E 起点**）のまま。No.80 で screen は ch切替起点にしたが either は未対応。
0x2E が来ない/遅い ch では either の screen 側も時間側も起動せず詰まる（実機で確認）。

---

## 新設計（either モード時。screen / time は不変）

either の completion を「**screen 判定（ch切替起点・メイン）＋ 0x2E（保険）**」の先勝ちにする。
3 経路が独立に `notifyMoveSignal` を出し、巡回完了待ちループの `respondedSet`（serial キー）が
先に来たものだけをカウントする（重複は `!respondedSet[label]` で自動排除）。

| 経路 | 起点 | 完了条件 | 役割 |
|---|---|---|---|
| screen | ch切替（巡回ループ）| 黒入り→黒消失 検知、または ScreenDetectTimeout で強制進行 | **メイン**（+ 時間経過の保険） |
| 0x2E（正規）| 0x2E 受信（NotifyPostLoadReady）| 0x2E + 安定化遅延 | 保険（基本これが先に来る）|
| 0x2E（fallback）| 0x2E ログ検出（NotifyChMovePacket）| 即時 | 保険 |

「screen 無反応でも進む」= `runScreenStrategyAtSwitch` 内の ScreenDetectTimeout 強制進行（時間経過の保険）＋ 0x2E。

---

## 変更内容（mumu/mumu.go のみ）

### (1) 巡回ループ: either も ch切替起点で画面判定を起動

`Start()` 内の巡回ループ、`if currentCfg.MoveTimeout > 0 {` ブロック内の現状（No.80）:
```go
				var screenCancel context.CancelFunc
				if strings.ToLower(currentCfg.LoadDetectMode) == "screen" {
					screenCtx, cancel := context.WithCancel(ctx)
					screenCancel = cancel
					for _, ser := range switchTargets {
						ser := ser
						go p.runScreenStrategyAtSwitch(screenCtx, ser, ch, switchDoneAt, currentCfg)
					}
				}
```
を以下に変更（screen に加え either でも起動）:
```go
				// No.80/81: screen と either は ch切替起点で画面判定を起動する（0x2E 非依存）。
				// either は加えて 0x2E（NotifyPostLoadReady / NotifyChMovePacket）も保険として
				// completion に使う（respondedSet が先勝ちで重複排除）。time は従来どおり 0x2E 起点のみ。
				var screenCancel context.CancelFunc
				ldMode := strings.ToLower(currentCfg.LoadDetectMode)
				if ldMode == "screen" || ldMode == "either" {
					screenCtx, cancel := context.WithCancel(ctx)
					screenCancel = cancel
					for _, ser := range switchTargets {
						ser := ser
						go p.runScreenStrategyAtSwitch(screenCtx, ser, ch, switchDoneAt, currentCfg)
					}
				}
```

### (2) NotifyPostLoadReady: case "either" を runTimeStrategy に変更

`NotifyPostLoadReady` 内の `switch mode {` を変更。現状（No.80 適用後）:
```go
		case "screen":
			// No.80: screen は ch切替起点（runScreenStrategyAtSwitch）で起動するため
			// 0x2E 起点では何もしない（二重 moveSignal 防止）。
			return
		case "either":
			p.runEitherStrategy(ctx, serial, lineID, fireAt, cfg)
		default: // "time"
			p.runTimeStrategy(ctx, serial, lineID, fireAt)
```
を以下に変更（either の screen 側は巡回ループが担当。0x2E 側は安定化遅延つき completion = 保険）:
```go
		case "screen":
			// No.80: screen は ch切替起点（runScreenStrategyAtSwitch）で起動するため
			// 0x2E 起点では何もしない（二重 moveSignal 防止）。
			return
		case "either":
			// No.81: either の screen 判定は ch切替起点（巡回ループ）が担当。
			// 0x2E はその保険として 0x2E + 安定化遅延で completion を出す（先に来た方が先勝ち）。
			p.runTimeStrategy(ctx, serial, lineID, fireAt)
		default: // "time"
			p.runTimeStrategy(ctx, serial, lineID, fireAt)
```

### (3) NotifyChMovePacket: either は通す（変更なし）

現状（No.80）は screen モードのみ return。either はガードに掛からず `notifyMoveSignal` を出す
（= 0x2E ログ fallback も either の保険になる）。**変更不要**。確認のみ。

---

## 不変条件チェック（§4-2）

| 条件 | 影響 | 判定 |
|---|---|---|
| §4-2.5 moveSignal 時刻フィルタ switchStartAt 基準 | screen/0x2E/fallback いずれも notifyMoveSignal の t=now or 0x2E時刻（>switchStartAt）| OK |
| §4-2.6 完了シグナル経路 | either のみ変更（screen=ch切替起点、0x2E=保険）。screen/time は不変 | 仕様変更（意図的）|
| §4-2.11 moveSignalMsg は lineID 携帯 | runScreenStrategyAtSwitch は lineID=ch、runTimeStrategy は lineID 引数を携帯 | OK |
| §4-2.12 NotifyPostLoadReady 同 lineID 1回 | either で runTimeStrategy 使用。ncap 側 postLoadFiredForLineID dedup は不変 | OK |
| §4-4.2 NotifyChMovePacket 呼び出しガード | gui/ 無変更。either は内部ガードに掛からない | OK |
| screenCancel スコープ | (1) で either も同ブロックで cancel される | OK |

### 二重カウント検証
either では同一 serial に対し screen / 0x2E正規 / 0x2E fallback が最大3回 notifyMoveSignal を出しうるが、
完了待ちループ4箇所すべてで `!respondedSet[msg.label]` ガードがあるため 1 回のみ got++（§4-2.11 の既存ガード）。

---

## 検証（§10）

- `go build ./...` / `go vet ./...`
- **新規 scenarios/either_mode.json**（load_detect_mode=either）を追加し run_all で PASS:
  - sim は onEnter で SetScreenState(black) + NotifyPostLoadReady を出す → screen と 0x2E の両経路が走る。
    先勝ちで完了し、二重カウントしないこと（移動失敗 forbid / all_devices_actual_ch_follows）。
- baseline（time）/ screen_mode（screen）が退行しないこと。
- 実機: `go run . -l`（either）で 0x2E が来れば早く、来なくても screen/タイムアウトで巡回が進むこと。

---

## 変更ファイル

| ファイル | 変更 |
|---|---|
| `mumu/mumu.go` | 巡回ループの画面判定起動条件に either 追加 / NotifyPostLoadReady case "either" を runTimeStrategy に |
| `scenarios/either_mode.json` | 新規（either の sim シナリオ） |

注: `runEitherStrategy`（旧・0x2E起点の time+screen 並走）は未使用になるが、削除はせず残置（影響最小化）。
