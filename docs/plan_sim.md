# patrol-sim: 実機レス検証シミュレータ 実装仕様書

目的: 実機（MuMu 8台 + ゲーム + Npcap）なしで巡回ロジックの検証サイクルを回し、実機検証は最終確認のみにする。

## 結論（フィージビリティ調査結果）

**mumu/ ncap/ gui/ を一切変更せず（新規ファイル追加のみ）構築可能。**

根拠:
- ADB 呼び出しは全て `cfg.ADBPath` 経由（mumu.go newCmd / runAdbBinary）→ 偽 adb.exe を指すだけで差し替え完了
- ncap→Patroller のシグナルは exported メソッド経由
  （main.go:315 `SetLineIDObserver`→`NotifyLineIDChange`、main.go:320 `SetPostLoadReadyCallback`→`NotifyPostLoadReady`）
  → ncap を起動せず、ハーネスが同じメソッドを直接呼べば「疑似パケットシグナル」になる
- 状態検証も exported（`PatrolStatus.Phase`、`DeviceStatus.{ExpectedCh,ActualCh,Mismatch}`、`GetSerialMaps` 等）

## アーキテクチャ

```
┌── patrol-sim.exe（ハーネス・1プロセス）──────────────────────┐
│                                                              │
│  実 mumu.Patroller（本番コードそのまま・無変更）               │
│     cfg.ADBPath = fake-adb.exe ──exec──┐                     │
│                                        ▼                     │
│  simサーバー（TCP 127.0.0.1:ephemeral）◄── fake-adb.exe       │
│     - 全 adb コマンドを受信記録（KEYCODE_ENTER = switch完了）  │
│     - screencap 問い合わせ → 黒/非黒を応答                    │
│                                                              │
│  ゲームサーバーシミュレータ（goroutine）                       │
│     - ENTER 検知 → シナリオ遅延後に                           │
│       NotifyLineIDChange / NotifyPostLoadReady を注入         │
│     - 外乱イベント注入（ネイティブch移動・無応答・0x2E連射）    │
│                                                              │
│  アサーションエンジン                                          │
│     - ログ捕捉（log.SetOutput tee）+ PatrolStatus ポーリング   │
│     - シナリオ毎に PASS / FAIL 出力                           │
└──────────────────────────────────────────────────────────────┘
```

## 検証できる範囲 / できない範囲

| 対象 | sim | 備考 |
|---|---|---|
| wait loop 4箇所の lineID ガード（No.54） | ✅ | stale lineID 注入で直接テスト |
| dwell クランプ（No.54） | ✅ | Phase 遷移時刻で計測 |
| ネイティブ干渉シナリオ（No.55 の本丸） | ✅ | 別 UID シグナル注入 |
| donor lineID / merge 引き継ぎ（No.55/56 の ncap 側） | ❌ | ncap 不起動のため。Level 2（後述） |
| RecordPatrolMove タイミング（No.56） | ✅ | probe 登録と ENTER 受信の順序検証 |
| probe/match・誤バインド・excludeUIDs | ✅ | UID 自由に注入可能 |
| screen / either / time 戦略 | ✅ | fake screencap で黒→非黒遷移 |
| guiWriter log-grep 経路（No.55 の gui 側） | △ | Phase C でログ行注入により対応 |
| ncap バイナリパース・session 管理 | ❌ | pcap replay（Level 2、ncap 変更要） |
| 実機 ADB 挙動（offline・MuMu 再起動） | ❌ | 実機最終確認に残す |
| 実ゲームのタイミング分布 | △ | 初期値は仮定。実機 verbose ログ 1 回で較正 |

## コンポーネント仕様

### 1. cmd/fake-adb/main.go（新規・~150行）

- 起動毎に環境変数 `PATROL_SIM_ADDR`（ハーネスが os.Setenv → exec 継承）へ TCP 接続
- 送信: `{"args":["−s","127.0.0.1:16384","shell","input","keyevent","KEYCODE_ENTER"],"ts":...}` JSON 1行
- 応答待ち: simサーバーが `{"exit":0,"stdout":"...","delay_ms":300}` を返す → delay_ms 待って stdout 出力し exit
  - これで ADB コマンド遅延の再現とコマンド毎の応答制御を simサーバー側に集約
- `exec-out screencap -p` の場合: stdout はサーバー応答 `"screen":"black"|"normal"` に応じて埋め込み PNG（黒 16x16 / 白 16x16 を go:embed）を出力
- `devices` の場合: サーバー応答のシリアル一覧を `List of devices attached\n<serial>\tdevice` 形式で出力
- `PATROL_SIM_ADDR` 未設定なら即 exit 1（誤って本物環境で使われた事故防止）

### 2. cmd/patrol-sim/main.go + sim パッケージ（新規・~600行）

- `patrol-sim.exe -scenario scenarios/native_move.json [-v]`
- 起動シーケンス:
  1. TCP listen（ephemeral port）→ `PATROL_SIM_ADDR` 設定
  2. `mumu.NewPatroller(cfg)` — cfg はシナリオから合成（ADBPath=fake-adb.exe、本番 config.json は読まない）
  3. `LoadSerialUIDMap` / `LoadSerialLabelMap` / `SetExcludeUIDs` をシナリオ値で投入
  4. `log.SetOutput(io.MultiWriter(os.Stderr, capture))` でログ捕捉
  5. `p.Start(serials, channels, "", opts)` — channelsFile は一時ファイル
- ゲームサーバーシミュレータ:
  - serial 毎のステート: 現在ch・ロード中フラグ
  - ENTER 受信 → screencap 状態を黒に → `line_id_change_delay` 後 `NotifyLineIDChange(uid, ch, now)` → `post_load_delay` 後 `NotifyPostLoadReady(uid, ch, now)` + screencap 非黒に
  - 遅延は `{"min_ms":N,"max_ms":M}` の一様乱数（シード固定オプション `seed` で再現可能に）
- イベント注入: `PatrolStatus().Phase` を 100ms ポーリングし `at_phase` 一致でトリガー

### 3. シナリオ JSON（scenarios/*.json）

```json
{
  "name": "native_ch_move_during_wait",
  "seed": 1,
  "devices": [
    {"serial": "emu-1", "uid": 41000001, "label": "Instance-1", "initial_ch": 3}
  ],
  "channels": [1, 2, 3],
  "patrol": {"dwell_secs": 2, "move_timeout_secs": 60, "load_detect_mode": "packet"},
  "server": {
    "line_id_change_delay": {"min_ms": 1500, "max_ms": 3000},
    "post_load_delay": {"min_ms": 3000, "max_ms": 6000},
    "adb_cmd_delay_ms": 300
  },
  "events": [
    {"at_phase": "wait_0x2e", "type": "native_move", "uid": 430314, "to_ch": 7}
  ],
  "assert": {
    "max_cycles": 3,
    "forbid_log_patterns": ["移動失敗", "未応答", "Mismatch"],
    "require_log_patterns": ["stale lineID=7 skipped"],
    "dwell_phase_secs": {"min": 1.8, "max": 2.5},
    "all_devices_actual_ch_follows": true
  }
}
```

イベント type:
- `native_move`: 指定 uid（デバイス外）で `NotifyLineIDChange` + `NotifyPostLoadReady` を注入（実機でネイティブクライアントが ch 移動した時に ncap が発火するのと同一経路）
- `silent_device`: 指定 serial のサーバー応答を1サイクル分抑止（無応答・タイムアウト系）
- `burst_0x2e`: 指定 uid で `NotifyPostLoadReady` を同 lineID で N 連射（戦闘中連射の再現・dedup 検証）
- `delayed_signal`: 特定 serial の post_load_delay を一時的に大きく（probeWindow 際どい系）

### 4. アサーション

- ログ捕捉バッファに対する forbid/require パターン照合
- `PatrolStatus` 遷移記録から dwell フェーズ実測秒を算出
- `GetDeviceStatuses()` で全 serial の `ActualCh == ExpectedCh`（Mismatch なし）確認
- 終了: `max_cycles` 巡回後 `p.Stop()` → 結果集計 → exit 0/1（CI 利用可能）

### 5. 標準シナリオセット（No.54〜56 検証の置き換え）

| シナリオ | 検証対象 |
|---|---|
| `baseline.json` | 正常系 3台×3ch、dwell=2s 実測 |
| `native_move.json` | wait_0x2e 中のネイティブ移動 → stale skip・カウント正常 |
| `native_move_same_ch.json` | ネイティブが偶然同 ch へ移動（lineID==target の偽陽性ケース確認） |
| `burst_0x2e.json` | PostLoadReady dedup（1 serial 1 lineID 1回） |
| `silent_one.json` | 1台無応答 → タイムアウト → 移動失敗記録が正しく出る（出ることが正） |
| `slow_signal.json` | 遅延ギリギリ（probeWindow 60s 内） |
| `screen_mode.json` | load_detect_mode=screen で黒→非黒遷移検知 |

## Phase 分割（実装順）

- **Phase A**: fake-adb + ハーネス基盤 + baseline.json が PASS（正常系成立）
- **Phase B**: イベント注入 4種 + native_move / burst_0x2e / silent_one シナリオ
- **Phase C**: 残シナリオ + アサーション拡充 + `run_all.ps1`（全シナリオ一括実行）+ README

各 Phase: sonnet subagent 実装 → Fable diff 照合 → `go build ./...` / `go vet ./...` → 1 commit。
既存コード無変更のため patrol-flow-reviewer レビューは Phase A のみ（Patroller API の使い方が本番 main.go と同等か確認）で十分。

## 制約・禁止事項（実装 subagent 向け）

- **mumu/ ncap/ gui/ appconfig/ の既存ファイルは 1 行も変更しない**（必要が生じたら実装中断して報告）
- 本番 `config/config.json` を読まない・書かない
- fake-adb は `PATROL_SIM_ADDR` 未設定時に即終了（本物 ADB と取り違え事故防止）
- 時計の仮想化（mock clock）はしない — 実時間で動かす（1シナリオ 30秒〜2分想定）

## Level 2（今回スコープ外・要望あれば別途）

ncap 層（0x15/0x2E パース・session merge・donor 補完）の sim 検証には pcap replay
（gopacket OpenOffline でキャプチャ済み .pcap を流す）が必要。
ncap/cap_device.go にオフラインハンドル注入の変更が要る = **要ユーザー事前確認**。
実機で一度 .pcap を録ればパース層の回帰テストが恒久資産になるメリットあり。

## 較正手順（初回実機検証時に1回）

1. 実機巡回を debug_verbose ON で 1〜2 巡回
2. ログから `switch_channel done` → `[0x2E] UUID=` の遅延分布を抽出
3. scenarios/*.json の `server.*_delay` を実測値レンジに更新
