# plan_no72: ロード完了シグナルのバージョン耐性化（lineID追跡 + 完了多重ソース）

ステータス: **DRAFT（ユーザー承認待ち）**
対象: ncap/cap_device.go, ncap/portmap.go(参照), mumu/mumu.go
関連不変条件: §4-1.3/4-1.4/4-1.6, §4-2.6/4-2.10/4-2.11/4-2.12
関連過去事例: No.23, No.31, No.39/40, No.44, No.56

---

## 0. ゴール

**どのパケットバージョンでも、各 ch ロードに対して完了シグナルが必ず・一度だけ発火する。**
バージョンアップでフィールドが消えても／戻っても巡回が破綻しない。

「どのケースでも問題が起きない」を、ncap 層を直接叩く決定論ユニットテストの**ケース行列**で機械的に担保する（§6 参照）。

---

## 1. 根本原因（確定済み・ログ実証）

連鎖:
1. 現バージョンの 0x15 は **SceneData を含まない**（`[0x15/16] lineID=` ログが全ログでゼロ。`sd==nil` 早期return [cap_device.go:1553]）。→ lineID は **PortMap 投票のみ**が情報源
2. PortMap 投票は **`sess.lineID == 0` のセッションだけ**が行う（[cap_device.go:1076,1125]）。クォーラム=2台
3. `resetTCPState()` は streams を消すだけで **lineID を保持**（[cap_device.go:152-154]）。マージも `newSess.lineID != 0` のときしか更新しない
4. 結果: 最初の ch で lineID が確定した台は、以降の ch 切替で `lineID != 0` のまま → **投票しない** → クォーラム不成立 → PortMap 未更新 → lineID が旧値で固着
5. lineID 固着 → 0x2E PostLoadReady の発火条件 `postLoadFiredForLineID != sess.lineID`（[cap_device.go:1714-1716], No.44）が **false** → **完了シグナル抑制**
6. No.39 で完了シグナルを lineID-change から 0x2E 一本に変更したため、5 で全滅。No.40 で MoveTimeout 30s→180s のため体感「固まる」

**実測（logs/log.txt）**: PostLoadReady 発火 = Ch3:7台 / Ch5:1台 / Ch6:0台。Ch5 で port=20085 への投票は Instance-16 の **1/2票のみ**でクォーラム不成立。

---

## 2. 設計（3レイヤ + 検出ログ）

### L1: lineID 追跡のバージョン耐性化（ncap）

**精密化した真の欠落（コード確認済み）**:
投票3箇所（[385] 監視ループ / [1082] C→S既存 / [1131] C→S新規）は**すべて C→S 方向**。
`handleServerToClientFast`（S→C fast-path）の serverIP 変更点 [1231-1244] は `tryPortMapLineID`（portMap を**読む**）を呼ぶが **`submitPortVote`（portMap を作る票）が無い**。
巡回中の ch 切替再接続は IDENT-P3 より先にサーバーが送る = **fast-path 経由**になりやすく（実ログ Ch5: 全台 "fast-path: サーバー変更"）、新セッションは lineID=0 だが**投票経路に到達しない** → 1台しか投票せず quorum(2) 不成立 → portMap 未更新 → 全台 lineID 固着。
No.23 で fast-path に `tryPortMapLineID` は足したが `submitPortVote` を足し忘れた**票供給の欠落**が真因。

**L1a-core. fast-path に投票追加（極小・本命）**:
- 対象: `handleServerToClientFast` Branch 1 [1231-1244]、`tryPortMapLineID(sess)` 呼出直後
- 追加: C→S 新規側 [1124-1133] と**同一パターン**の投票ブロック
  ```go
  if sess.lineID == 0 {
      cd.currentChannelMu.RLock(); patrolCh := cd.currentChannel; cd.currentChannelMu.RUnlock()
      if patrolCh > 0 { go cd.submitPortVote(patrolCh, newServerAddr, sess.label, now) }
  }
  ```
- 効果: fast-path 再接続した複数台が同一新ポートへ投票 → quorum 成立 → `portMap.Update` → 以降 `tryPortMapLineID` で lineID が新 ch に更新 → 既存 0x2E dedup（postLoadFiredForLineID != lineID）が**そのまま正しく発火**
- **No.44/56/39 の dedup・完了機構は一切触らない**（回帰面が投票/lineID 周辺に限定）

**L1a-robust（任意・stale portMap 対策）**:
- 投票3+1箇所の条件を `sess.lineID == 0 || portMap.LookupByPort(addr) != patrolCh` に拡張し、既バインド台も新ポートで再投票可能にする。前回起動の古い portMap エントリ対策。Phase 1 では core を優先し、robust は core 検証後に要否判断

### L2: 完了シグナルを lineID 番号から切り離す（ncap）

**L2a. 再接続で dedup をリセット** — 「同 lineID 番号 = 発火済み」の混同を解消。

- 対象: serverIP 変更時（再接続検出）＋ idle reset の UID保持パス
- 変更: serverIP が変化したら `sess.postLoadFiredForLineID = 0` にリセット（= 次の 0x2E は lineID 番号に関わらず発火可能にする）
- マージ（No.56 [745-748]）の `postLoadFiredForLineID` 引き継ぎは **server未変更時のみ意味を持つ**よう、再接続側で 0 リセットされた値が優先されるロジックに整える（詳細は §3）
- 安全性（No.44 retention）: 0x2E バーストのスパム抑制は「同一ロード内」。serverIP 変更（再接続）でのみリセットするので、戦闘中バースト（serverIP 不変）では従来どおり1回に抑制される

**L2b. lineID 不明でも発火（lineID=0 で通知）** — portMap も SceneData も無い最悪ケースの保険。

- 対象: 0x2E PostLoadReady 発火条件 [1714-1718]
- 現状: `uid!=0 && sess.lineID!=0 && postLoadFiredForLineID != sess.lineID`
- 変更: `sess.lineID!=0` 必須を外し、`uid!=0 && !firedThisLoad` で発火。`onPostLoadReady(uid, sess.lineID, now)` の lineID は 0 でも可（mumu wait-loop は `msg.lineID==0` を受理 [2360]）。`firedThisLoad` は L2a のリセットと連動するフラグ（lineID 番号非依存）
- 安全性: stale 0x2E（旧ch・切替前）の排除は mumu 側の時刻フィルタ `msg.t.After(switchStartAt)` [2359] と L2a の再接続リセットで担保

### L3: 構造的フォールバック完了シグナル（mumu）

**0x2E 自体が将来消えても**完了が出るようにする保険。

- 対象: `NotifyLineIDChange`/`MatchLineChange`（再接続=uidNewlySet で常時呼ばれる [mumu.go:1430,1510]）
- 変更: 巡回中・既バインド serial について、`MatchLineChange` 受信時に「**まだ当該切替サイクルで完了シグナルが出ていなければ**、loadStabilizationDelay 後に暫定完了を発火」する遅延タスクを登録。`NotifyPostLoadReady`（0x2E 経路）が先に発火したら遅延タスクをキャンセル
- 優先順位: **0x2E（正確）優先、来なければ再接続+遅延（構造的）でフォールバック**
- 安全性（No.39 の早すぎる発火回避）: フォールバックは 0x2E が来ない場合のみ・遅延付き・1サイクル1回。screen モードでは screen strategy が別途完了を律速するため二重発火しないよう serial+cycle で dedup

### D: バージョン検出ログ（ncap）

- 0x15 受信時に **SceneData 有無**を、0x2E 受信時に **UUID 有無**を、セッション単位で初回／変化時にログ（沈黙劣化を可視化）
- 例: `[%s][0x15] SceneData=なし (portMap依存モード)` / `[%s][0x2E] UUID=あり`
- 将来「無くなったものが戻った」ら `SceneData=あり` ログで即座に気付ける

---

## 3. 変更ファイルと差分概要（実装は subagent）

### Phase 1（ncap）: L1a + L2a + L2b + D
- `ncap/cap_device.go`
  - `handleClientToServer` 投票2箇所: 投票条件を L1a に拡張（portMap 参照ヘルパー追加可）
  - serverIP 変更検出箇所: `postLoadFiredForLineID=0` リセット（L2a）
  - idle reset UID保持パス [649-665]: 同上リセット（L2a）
  - 0x2E 発火条件 [1714-1718]: `sess.lineID!=0` 撤廃・firedThisLoad ベースへ（L2b）
  - `mergeSessionIfDuplicate` [745-748]: postLoadFiredForLineID 引き継ぎを L2a と整合（再接続リセット優先）
  - 0x15/0x2E に検出ログ追加（D）
- `session` 構造体: 必要なら `firedThisLoad bool`（または既存 postLoadFiredForLineID の意味を「ロードサイクルID」へ再定義）。**フィールド意味変更は §4-1 に追記**

### Phase 2（mumu）: L3
- `mumu/mumu.go`
  - `MatchLineChange` 既バインドパス [1450-1454]: 巡回中なら暫定完了の遅延タスク登録
  - `NotifyPostLoadReady` [1080]: 発火時に同 serial の暫定完了タスクをキャンセル（0x2E 優先）
  - 暫定完了タスク管理マップ（serial→cancel）追加。Stop 時に全キャンセル（No.46 と同様）

> Phase 1 だけで観測バグ（lineID固着→0x2E抑制）は解消する。Phase 2 は 0x2E 消失への保険。**1 Phase = 1 commit = changelog 1 エントリ**。

---

## 4. 状態遷移表（全パス・§5 罠回避のため明示）

ch 切替後の1台の完了判定:

| # | SceneData | portMap(new port) | 0x2E UUID | 再接続(server変更) | 期待: lineID | 期待: 完了シグナル |
|---|---|---|---|---|---|---|
| P1 | あり | — | あり | あり | 0x15 で新ch | 0x2E で発火 |
| P2 | なし | quorum成立 | あり | あり | 投票→新ch | 0x2E で発火 |
| P3 | なし | quorum不成立→**再投票で成立**(L1a) | あり | あり | 投票→新ch | 0x2E で発火（**現バグの修正点**） |
| P4 | なし | 不成立のまま | あり | あり | 0 のまま | **lineID=0 で発火**(L2b)・dedupは再接続リセット(L2a) |
| P5 | なし | — | **なし**(将来) | あり | 0/旧 | **L3 構造フォールバックで発火** |
| P6 | — | — | stale(旧ch・切替前) | なし | — | **発火しない**（時刻[2359]+再接続未発生で除外） |
| P7 | — | — | バースト(戦闘・同一ロード) | なし | 不変 | **1回のみ**（No.44 維持: serverIP不変でリセットされない） |
| P8 | あり(将来復活) | — | あり | あり | 0x15 で新ch | 0x2E で発火（L1a投票は休眠で衝突なし） |

---

## 5. 不変条件への影響（§4 更新要否）

- §4-1.3（onLineIDObserved 発火条件）: 不変。L3 は mumu 側で MatchLineChange を**消費**するだけで発火条件は変えない
- §4-1.4（SceneData なしパスで uidNewlySet 発火）: 不変（維持）
- **§4-1（新規）**: 「`postLoadFiredForLineID` は serverIP 変更で 0 リセットする（再接続=新ロードのため。No.44 のスパム抑制は serverIP 不変時のみ有効）」を追記
- §4-2.6（完了=0x2E UUID+遅延）: **改定**。「0x2E 優先・不在時は再接続+遅延の構造フォールバック（L3）」を追記
- §4-2.10（MatchLineChange は notifyMoveSignal を発火しない）: **改定**。「ただし 0x2E 不在時のフォールバックとして暫定完了を遅延発火しうる（0x2E 到着で取消）」を追記
- §4-2.12（NotifyPostLoadReady は同 lineID 内1回）: **改定**。「同一ロードサイクル内1回（serverIP 不変の間）」へ言い換え
- 後方互換: config スキーマ変更なし。port_ch_map.json フォーマット不変。既存 config.json 無変更で動作

---

## 6. 検証（「どのケースでも」の機械的担保）

### 6-1. ncap 決定論ユニットテスト（新設・Phase 1 の主担保）
`ncap/cap_device_test.go` に追加。`newCapDeviceForTest()` を拡張し、`onLineIDObserved`/`onPostLoadReady` のコールバックを記録するスパイを差す。`pb.SyncContainerData`/`pb.SyncToMeDeltaInfo` を `proto.Marshal` して合成ペイロードを作り、実ハンドラ（`processSyncContainerData`/`processSyncToMeDeltaInfo`/C→S 投票経路）に通す。

§4 の **P1〜P8 を1ケース=1テスト**として実装し、全行が:
- lineID が期待値に更新される
- onPostLoadReady が**期待回数だけ**呼ばれる（P6=0回, P7=1回 を含む）

を assert。これが「全ケース」のレグレッションガード。

加えて **multi-channel 連続切替**（ch3→ch5→ch6, SceneData なし）で各 ch ごとに完了が1回ずつ出ることを通し検証（実バグの再現→修正確認）。

### 6-2. Level 1 patrol-sim / 1.5 gui-sim（Phase 2 + 非回帰）
- L3 の暫定完了は mumu 層。既存 `run_all.ps1` 全7シナリオ PASS を維持
- `sim/gameserver.go` を拡張: 「0x2E を注入しない（NotifyPostLoadReady を呼ばない）」シナリオフラグを追加し、L3 フォールバックで巡回が進むことを検証する新シナリオ `no_0x2e_fallback` を追加（patrol-sim 層で完了する）
- 注意: sim は ncap をバイパスするため、L1a/L2 の検証は **6-1 が主**。sim は L3 と mumu 非回帰の担保

### 6-3. Level 2 pcap-replay（任意・あれば）
- 実トラフィック golden があれば `pcap-replay -golden` で lineid/UID 件数の非回帰確認。ゲームアップデート後は §10 手順で golden 更新

### 6-4. 静的
- `go build ./...` / `go vet ./...` / `go test ./ncap/ ./mumu/ ./appconfig/`
- packet-analyst（Phase1）/ patrol-flow-reviewer（Phase2）レビューで BLOCKING ゼロ

---

## 7. リスクと留意

- **§5 連鎖罠**: No.30→35 の「修正が次のバグを生む」連鎖の領域。状態遷移は §4 表で全パス固定し、6-1 で機械検証してから commit
- L2b（lineID=0 発火）は stale 保護を弱める方向 → L2a の再接続リセットと mumu 時刻フィルタの両方が効いていることを P6 テストで担保
- L3 は No.39 が外した経路の限定復活 → 「0x2E 到着で取消」「1サイクル1回」「遅延付き」で早すぎる発火を回避。P5/二重発火なしをテスト
- フィールド意味変更（postLoadFiredForLineID → ロードサイクル）は packet-analyst の重点確認事項

---

## 8. フェーズ計画（コミット単位）— リスク最小の増分分割（改訂）

**重要**: 観測バグ（lineID固着→0x2E抑制）は **L1a 単独で解消**する。lineID が再投票で新ch化すれば
既存 No.44 dedup（`postLoadFiredForLineID != lineID`）はそのまま正しく発火する。
よって No.44/56 の dedup には**触れず**、リスクを最小化できる。L2/L3 は別フェーズの保険。

1. **Phase 1（ncap, No.72）= L1a + D**:
   投票ゲート緩和 + 版検出ログのみ。**No.44/56 の 0x2E dedup・完了機構は無変更**。
   → 観測バグ解消。回帰面は No.09/23/33/34/35（投票・lineID・bind 周辺）に限定
   - テスト: P1/P2/P3 + T-No09/T-No23/T-No33/T-No34 + multi-channel 連続切替の実バグ再現→修正確認
2. **Phase 2（ncap, No.73）= L2a + L2b**（保険・portMapもSceneDataも無い最悪ケース）:
   dedup を lineID番号から `loadCycle` ベースへ。**No.44/56 の状態機械を直接触る高リスク** → 単独フェーズで重点レビュー
   - テスト: P4 + T-No44 + T-No56（戦闘連射・merge二重発火の歴史的シナリオを必須）
3. **Phase 3（mumu, No.74）= L3**（0x2E 自体の将来消失への保険）:
   構造的フォールバック完了。No.37/39 の早すぎ発火回避を遅延・取消で担保
   - テスト: P5 + T-No36/T-No37/T-No39 + patrol-sim `no_0x2e_fallback`

各フェーズ: subagent 実装 → 主セッション diff 照合 → レビュー agent → 動的検証 → changelog → commit。
**Phase 1 完了・検証後にユーザーへ報告し、Phase 2/3 着手可否を再確認する**（保険フェーズは高リスクのため）。

---

## 9. 過去の巡回シーケンス不具合 網羅マトリクス（着手条件）

本変更が「これまでの巡回不具合」を再発させないこと＝下表の全行をテスト/不変条件で固定する。
★ = 本変更が直接触る高リスク。各行に対応する 6-1 ユニットテストケース（T番号）を新設する。

| No | 過去不具合 | 確立した不変条件 | 本変更の干渉 | 担保（テスト/ガード） |
|---|---|---|---|---|
| **No.44**★ | 戦闘中デバイスの stale 0x2E 連射で別ch完了を汚染 | §4-2.11/12・postLoadFiredForLineID dedup | L2a/L2b が dedup を改変 | **T-No44**: 2台、片方が旧ch(serverIP不変)で0x2E連射しつつ他台をch切替 → 連射は新chに**カウントされず**・連射台は**1回のみ**発火 |
| **No.36**★ | バインド済み台のch移動(merge,userUID不変)で完了欠落 | MatchLineChange→完了（→No.39で0x2E化） | L1a+L2a で 0x2E 経路復活／L3 で構造復活 | **T-No36**: バインド済み・merge・userUID不変・sd==nil → 完了が1回発火 |
| **No.37**★ | lineID-change(=ロード0%)を完了にすると早すぎ取りこぼし | §4-2.6 遅延必須 | L3 が lineID-change相当のタイミング | **T-No37**: L3 フォールバックは再接続**即時でなく遅延後**に発火（0x2E到着なら取消） |
| **No.39**★ | 完了を 0x2E UUID に一本化 | §4-2.10 MatchLineChange は完了発火しない | L3 が部分巻き戻し | **T-No39**: 0x2E 到着時は 0x2E 経路優先・L3 は**二重発火しない** |
| **No.34**★ | sd==nil パスへの早期return追加で onLineIDObserved 全停止 | §4-1.7 sd==nil に早期return禁止 | L1a/L2 が同パス近傍を編集 | **T-No34**: sd==nil かつ uidNewlySet かつ lineID既知 → onLineIDObserved 発火（早期returnで潰さない） |
| **No.33**★ | onLineIDObserved 過剰発火で本物クライアント誤バインド | §4-1.3 lineID変化/uidNewlySet時のみ発火 | L3 が MatchLineChange を消費 | **T-No33**: 本物同ch静止(oldCh==lineID,uidNewlySet=false) → 発火せず／L3 は excludeUIDs と未バインド uid を弾く |
| **No.23**★ | sd==nil で onLineIDObserved 未発火＋fast-path で lineID=0 | sd==nil+portMap既知で発火・fast-path に tryPortMapLineID(§4-1.6) | L1a が投票/lineID更新を改変 | **T-No23**: sd==nil+portMap解決済み → 発火／S→C fast-path 新規セッションも lineID 取得 |
| **No.56**★ | merge で dedup未引継ぎ→同lineID二重発火 | merge は postLoadFiredForLineID 引継ぎ | L2a(再接続リセット)が引継ぎと競合 | **T-No56**: 再接続→merge で dedup が「二重発火せず」かつ「新ロードを抑制しない」両立 |
| **No.55** | NotifyChMovePacket phantom・donor別uid借用 | uid検証・donor同userUID限定 | 不干渉（donor/NotifyChMovePacket は変更せず） | 既存維持（変更しないことを diff 照合で確認） |
| **No.35** | RecordPatrolMove が SwitchGroup後で probe空 | §4-2.1 SwitchGroup前 | 不干渉 | 既存維持・run_all.ps1 |
| **No.26** | Probe バインド二重確立 | §4-2.4 ロック後再チェック | 不干渉 | 既存維持 |
| **No.29** | 既到達chスキップ・need計算 | §4-2.7 | 不干渉 | 既存維持・run_all.ps1 |
| **No.27** | sess.userUID=rawUUID>>16 | §4-1.2 | 不干渉 | 既存維持 |
| **No.12** | ch移動完了欠落(!HasBinding ガード) | §4-4.2 ガード再導入禁止 | 不干渉（guiWriter 非変更） | 既存維持 |
| **No.09** | NAT 全台同clientIP | §4-1.1 識別キーに clientIP不使用 | L1a 投票は serverIP+label キー | **T-No09**: 同clientIP複数台でも serial 別に正しく投票/発火 |

> 結論: 観測バグ（lineID固着→0x2E抑制）の修正に必要な L1a/L2/L3 は、過去の No.23/33/34/36/37/39/44/55/56 と**同一の状態機械を再び触る**。よって 6-1 のケース行列を上表 T-No** で**歴史的シナリオまで拡張**し、Phase 1 commit 前に全 PASS を必須条件とする。これが「網羅」の機械的定義。

### 6-1 拡張テスト一覧（再掲・統合）
P1〜P8（§4 基本パス）＋ T-No09/23/33/34/36/37/39/44/56（歴史的回帰ガード）。
Phase 1 では ncap 内で表現可能な T-No09/23/34/56 と P1〜P7 を必須化。
L3 依存の T-No36/37/39/44（mumu 層の完了消費を含むもの）は Phase 2 の patrol-sim/gui-sim ＋ mumu ユニットで担保。
