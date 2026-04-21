// BPSR patrol cams GAS Sync - content.js v2.0

(function () {
    if (window.__gasSync) return;
    window.__gasSync = true;

    // ===== 設定 =====
    const GO_SERVER_URL = 'http://localhost:8080/api/patrol/channels/gas';
    const FETCH_INTERVAL_MS = 10 * 60 * 1000; // 10分
    const SPAWN_THRESHOLD_HOURS = 20.0;
    // ================

    function parseElapsedSeconds(timerStr) {
        if (!timerStr) return null;
        const str = timerStr.trim();
        const exceeded = str.startsWith('+');
        const s = exceeded ? str.slice(1) : str;
        const parts = s.split(':').map(Number);
        if (parts.some(isNaN)) return null;
        let displaySec = 0;
        if (parts.length === 3)      displaySec = parts[0]*3600 + parts[1]*60 + parts[2];
        else if (parts.length === 2) displaySec = parts[0]*60 + parts[1];
        else return null;
        return exceeded ? 24*3600 + displaySec : 24*3600 - displaySec;
    }

    function collectEntries() {
        const btns = document.querySelectorAll('.channel-btn');
        if (btns.length === 0) return null;
        const thresholdSec = SPAWN_THRESHOLD_HOURS * 3600;
        const entries = [];
        const seen = new Set();

        btns.forEach(btn => {
            const numEl   = btn.querySelector('.channel-number');
            const timerEl = btn.querySelector('.channel-timer');
            if (!numEl) return;
            const ch = parseInt(numEl.textContent.trim(), 10);
            if (isNaN(ch)) return;
            if (!timerEl || !timerEl.textContent.trim()) return;

            const elapsedSec = parseElapsedSeconds(timerEl.textContent.trim());
            if (elapsedSec === null || elapsedSec < thresholdSec) return;

            const card = btn.closest('.enemy-card');
            let enemy = '';
            if (card) {
                const nameEl = card.querySelector('.enemy-name');
                enemy = nameEl ? nameEl.textContent.trim() : (card.dataset.name || '');
            }

            const key = `${ch}:${enemy}`;
            if (!seen.has(key)) {
                seen.add(key);
                entries.push({ channel: ch, enemy });
            }
        });

        return entries;
    }

    function sendToGoServer(entries) {
        if (entries.length === 0) {
            console.log('[GAS Sync] 閾値以上のchなし（送信スキップ）');
            showBadge('0ch（送信なし）', '#888');
            return;
        }
        const counts = {};
        entries.forEach(e => { counts[e.enemy] = (counts[e.enemy] || 0) + 1; });
        const summary = Object.entries(counts).map(([k, v]) => `${k}:${v}`).join(' / ');

        fetch(GO_SERVER_URL, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ entries })
        })
        .then(res => {
            if (res.ok) {
                console.log(`[GAS Sync] 送信成功: ${entries.length}件 (${summary})`);
                showBadge(`✓ ${entries.length}件 送信済 (${summary})`, '#00ff9d');
            } else {
                console.warn('[GAS Sync] 送信失敗:', res.status);
                showBadge('✗ 送信失敗', '#ff4466');
            }
        })
        .catch(err => {
            console.error('[GAS Sync] 接続エラー:', err.message);
            showBadge('✗ 接続失敗（Go起動確認）', '#ff4466');
        });
    }

    function showBadge(msg, color) {
        let badge = document.getElementById('_gas_sync_badge');
        if (!badge) {
            badge = document.createElement('div');
            badge.id = '_gas_sync_badge';
            badge.style.cssText = [
                'position:fixed','bottom:16px','right:16px','z-index:99999',
                'font-weight:bold','padding:8px 16px','border-radius:8px',
                'font-size:13px','box-shadow:0 2px 8px rgba(0,0,0,0.5)',
                'pointer-events:none','transition:opacity 0.5s','font-family:monospace'
            ].join(';');
            document.body.appendChild(badge);
        }
        badge.style.background = color || '#00ff9d';
        badge.style.color = (color === '#888' || color === '#ff4466') ? '#fff' : '#000';
        badge.textContent = msg;
        badge.style.opacity = '1';
        clearTimeout(badge._t);
        badge._t = setTimeout(() => { badge.style.opacity = '0'; }, 5000);
    }

    function run() {
        const entries = collectEntries();
        if (entries === null) {
            console.log('[GAS Sync] .channel-btn がまだない');
            return;
        }
        const counts = {};
        entries.forEach(e => { counts[e.enemy] = (counts[e.enemy] || 0) + 1; });
        const summary = Object.entries(counts).map(([k, v]) => `${k}:${v}`).join(' / ') || 'なし';
        console.log(`[GAS Sync] 取得: ${entries.length}件が閾値${SPAWN_THRESHOLD_HOURS}h以上 (${summary})`);
        sendToGoServer(entries);
    }

    let started = false;
    const check = setInterval(() => {
        if (document.querySelectorAll('.channel-btn').length > 0) {
            if (!started) {
                started = true;
                clearInterval(check);
                console.log('[GAS Sync] チャンネルボタン検出 → 実行開始');
                setTimeout(() => {
                    run();
                    setInterval(run, FETCH_INTERVAL_MS);
                }, 1000);
            }
        }
    }, 500);

    setTimeout(() => {
        if (!started) {
            clearInterval(check);
            console.log('[GAS Sync] このフレームにはチャンネルなし（スキップ）');
        }
    }, 60000);

    console.log(`[GAS Sync] v2.0 起動 (間隔=${FETCH_INTERVAL_MS/60000}分, 閾値=${SPAWN_THRESHOLD_HOURS}h)`);
})();
