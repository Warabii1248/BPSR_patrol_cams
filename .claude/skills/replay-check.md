---
name: replay-check
description: pcap-replay で .pcap を実パーサに流しパース層の生存確認と golden 回帰比較を行う。引数は pcap パス（省略時は logs\ の最新 capture）と任意の golden パス。
allowed-tools: PowerShell, Read, Grep, Glob
---

# /replay-check — pcap replay によるパース層検証

## 目的

CLAUDE.md §10 の Level 2 検証。録画済み .pcap を `ncap` の実パーサ（handlePacket）に流し、
①パース層の生存確認（No.23/34 型破壊の検出）、②golden との回帰比較を行う。
ncap/ 変更後・ゲームアップデート後のチェックで使う。

## 引数の扱い

- `/replay-check <pcap> [golden]` — 指定 pcap を再生。golden 指定時は比較モード
- **引数なし**: `logs\*.pcap` の最新ファイルを Glob で探して使う。pcap が1つも無ければ
  「先に `.\release\pcap-record.exe` で録画が必要（本体と並行起動可）」と案内して終了

## 実行手順

1. `release\pcap-replay.exe` が無ければビルド: `go build -o release\pcap-replay.exe .\cmd\pcap-replay`
2. golden の有無で分岐:

   ```powershell
   # 生存確認のみ（golden なし）
   .\release\pcap-replay.exe -pcap <pcap>

   # golden 比較
   .\release\pcap-replay.exe -pcap <pcap> -golden <golden>
   ```

3. 出力を判定:
   - **「lineid 0件」「UID確定 0件」警告（exit 1）** = パース層破壊の兆候（No.23/34 型）。
     ゲームバージョンアップ直後なら「プロトコル変化」、コード変更直後なら「リグレッション」を第一仮説に
   - golden 比較の差分 = イベント種別ごとの件数差・内容差を報告（順序非依存マルチセット比較なので、純粋な順序差は出ない）
4. golden が `testdata\` に1つも無く、生存確認が通った場合は golden 作成を提案:

   ```powershell
   .\release\pcap-replay.exe -pcap <pcap> -record-golden testdata\golden_<版>.jsonl
   ```

## 報告フォーマット

- 結果1行目: 生存 OK / 破壊疑い / golden 一致 / golden 差分あり
- イベント件数サマリ（lineid / UID確定 / chat / detection）
- 差分があれば種別ごとの増減と代表例
- 次のアクション（golden 更新 or コード調査）を1行で

## 注意

- replay は Npcap 不要（OpenOffline）だが、ビルドには CGO 環境が要る
- golden はゲームバージョンに紐づく。**アップデート後は差分が出るのが正常** — その場合は新録画から golden を更新する（§10 の手順）
- chat イベントは sender/msg をハッシュ化して記録している（生ログは含まれない）
