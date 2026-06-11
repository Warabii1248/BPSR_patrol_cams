// ===== データ管理 / 折りたたみ =====
function toggleDMSection(name){
  const card=document.getElementById('dm-'+name+'-card');
  const btn=document.getElementById('dm-'+name+'-toggle-btn');
  if(!card)return;
  const minimized=card.classList.toggle('minimized');
  if(btn)btn.textContent=minimized?'＋ 展開':'－ 最小化';
  localStorage.setItem('dm-minimized-'+name,String(minimized));
}
function applyDMSectionState(name){
  const minimized=localStorage.getItem('dm-minimized-'+name)==='true';
  const card=document.getElementById('dm-'+name+'-card');
  const btn=document.getElementById('dm-'+name+'-toggle-btn');
  if(card)card.classList.toggle('minimized',minimized);
  if(btn)btn.textContent=minimized?'＋ 展開':'－ 最小化';
}
// ===== データ管理 / 記憶済みデバイス情報 =====
function loadDeviceMemory(){
  fetch('/api/devices/memory').then(r=>r.json()).then(entries=>{
    const list=document.getElementById('devmem-list');
    const count=document.getElementById('devmem-count');
    if(!list)return;
    if(!entries||entries.length===0){
      list.innerHTML='<div class="no-devices">記憶データなし</div>';
      if(count)count.textContent='';
      return;
    }
    entries.sort((a,b)=>(a.serial<b.serial?-1:1));
    list.innerHTML=entries.map(e=>{
      const uid=e.uid?'UID: '+e.uid:'UID: --';
      const label=e.label||'--';
      const ch=e.current_ch>0?'Ch'+e.current_ch:'--';
      const dot=e.confirmed?'<span style="color:var(--accent2)">●</span>':'<span style="color:var(--text3)">○</span>';
      const delBtn=e.uid?'<button class="btn" style="margin-left:8px;padding:1px 7px;font-size:.78em;color:#f87171;border-color:#f87171" onclick="deleteSerialUID(\''+escHtml(e.serial)+'\')">削除</button>':'';
      return '<div class="device-entry">'+
        dot+' <span class="serial">'+escHtml(e.serial)+'</span> '+
        '<span style="color:var(--accent);font-weight:600;margin-left:6px">'+escHtml(label)+'</span> '+
        '<span class="uid" style="margin-left:6px">'+uid+'</span> '+
        '<span style="margin-left:auto;color:var(--text2);font-size:.85em">'+ch+'</span>'+
        delBtn+
        '</div>';
    }).join('');
    if(count)count.textContent='合計 '+entries.length+' 件';
  }).catch(()=>{
    const list=document.getElementById('devmem-list');
    if(list)list.innerHTML='<div class="no-devices" style="color:#f87171">取得失敗</div>';
  });
}
function deleteSerialUID(serial){
  if(!confirm('serial='+serial+' の UID バインドを削除しますか？'))return;
  fetch('/api/serial_uid/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({serial:serial})})
    .then(r=>r.json()).then(d=>{
      if(d.ok)loadDeviceMemory();
      else toast('削除失敗: '+(d.error||''),'error');
    }).catch(e=>toast('削除エラー: '+e.message,'error'));
}
// ===== データ管理 / chマッピング =====
function loadPortMapEntries() {
  fetch('/api/portmap/entries').then(r=>r.json()).then(entries=>{
    const tbody = document.getElementById('portmap-tbody');
    const count = document.getElementById('portmap-count');
    if (!tbody) return;
    if (!entries || entries.length === 0) {
      tbody.innerHTML = '<tr><td colspan="3" style="padding:12px 10px;color:var(--text3);text-align:center">マッピングデータなし</td></tr>';
      if (count) count.textContent = '';
      return;
    }
    entries.sort((a,b) => a.ch - b.ch);
    tbody.innerHTML = entries.map(e => {
      const dt = e.updated_at ? new Date(e.updated_at).toLocaleString('ja-JP') : '--';
      return '<tr style="border-bottom:1px solid var(--border)">' +
        '<td style="padding:5px 10px;font-weight:600;color:var(--accent)">'+e.ch+'</td>' +
        '<td style="padding:5px 10px;font-family:monospace;font-size:var(--fs-sm)">'+e.server_ip+'</td>' +
        '<td style="padding:5px 10px;color:var(--text2);font-size:var(--fs-sm)">'+dt+'</td>' +
        '</tr>';
    }).join('');
    if (count) count.textContent = '合計 '+entries.length+' 件';
  }).catch(()=>{
    const tbody = document.getElementById('portmap-tbody');
    if (tbody) tbody.innerHTML = '<tr><td colspan="3" style="padding:12px 10px;color:#f87171;text-align:center">取得失敗</td></tr>';
  });
}
function portmapSetStatus(msg, isErr) {
  const el = document.getElementById('portmap-status');
  if (!el) return;
  el.textContent = msg;
  el.style.color = isErr ? '#f87171' : 'var(--text2)';
}
async function portmapManualUpdate() {
  const ch = parseInt((document.getElementById('manual-ch')||{}).value)||0;
  const ip = ((document.getElementById('manual-ip')||{}).value||'').trim();
  const port = parseInt((document.getElementById('manual-port')||{}).value)||0;
  if (!ch || ch <= 0) { portmapSetStatus('ch番号を入力してください', true); return; }
  if (!ip) { portmapSetStatus('IPアドレスを入力してください', true); return; }
  if (!port || port <= 0) { portmapSetStatus('ポート番号を入力してください', true); return; }
  portmapSetStatus('手動マッピング更新中...', false);
  try {
    const r = await fetch('/api/portmap/manual', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({ch, ip, port})});
    const d = await r.json();
    if (d.ok) {
      portmapSetStatus('ch'+ch+' → '+ip+':'+port+' に更新しました', false);
      loadPortMapEntries();
    } else {
      portmapSetStatus('エラー: '+(d.error||'不明'), true);
    }
  } catch(e) { portmapSetStatus('エラーが発生しました', true); }
}
function updateIdentifiedBadge() {
  fetch('/api/devices/identified').then(r=>r.json()).then(d=>{
    const el = document.getElementById('devices-id-badge');
    if (!el) return;
    if (d.total === 0) {
      el.textContent = '-- デバイス未設定';
      el.style.background = 'transparent';
      el.style.color = 'var(--text3)';
    } else if (d.identified) {
      el.textContent = '✅ 全デバイス追跡可能 ('+d.ready+'/'+d.total+'台)';
      el.style.background = 'rgba(34,197,94,0.12)';
      el.style.color = '#22c55e';
    } else {
      el.textContent = '⚠ 認識中... ('+d.ready+'/'+d.total+'台)';
      el.style.background = 'rgba(234,179,8,0.12)';
      el.style.color = '#ca8a04';
    }
  }).catch(()=>{});
}
let _identifyPollTimer = null;
function stopIdentifyPolling() {
  if (_identifyPollTimer) { clearInterval(_identifyPollTimer); _identifyPollTimer = null; }
}
function startIdentifyPolling() {
  if (_identifyPollTimer) return;
  _identifyPollTimer = setInterval(async () => {
    try {
      const d = await fetch('/api/devices/identified').then(r=>r.json());
      updateIdentifiedBadge();
      if (d.identified) {
        stopIdentifyPolling();
        const ok = confirm('✅ 全デバイス認識完了 ('+d.total+'台)\n1〜100ch を巡回リストに反映しますか？\n（デバイス分担設定も自動で計算されます）');
        if (!ok) return;
        const chs = Array.from({length:100},(_,i)=>i+1);
        const r = await fetch('/api/patrol/channels', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({channels:chs})});
        const rd = await r.json();
        if (!rd.ok) { portmapSetStatus('✗ 巡回リスト保存失敗', true); return; }
        const ad = await fetch('/api/devices/assignments/compute', {method:'POST'}).then(r=>r.json()).catch(()=>({ok:false}));
        if (ad.ok) {
          portmapSetStatus('✓ 1〜100ch 反映 + '+ad.device_count+'台を'+ad.groups+'グループに分担設定', false);
        } else {
          portmapSetStatus('✓ 1〜100ch を巡回リストに反映しました', false);
        }
        if (typeof loadPatrolChannels === 'function') loadPatrolChannels();
      }
    } catch(e) {}
  }, 5000);
}
async function portmapMapAll() {
  portmapSetStatus('全chマッピング実行中...', false);
  try {
    const d = await fetch('/api/portmap/map-all', {method:'POST'}).then(r=>r.json());
    portmapSetStatus('完了: '+d.mapped+' 件をマッピング → デバイス認識を確認中...', false);
    loadPortMapEntries();
    updateIdentifiedBadge();
    startIdentifyPolling();
  } catch(e) { portmapSetStatus('エラーが発生しました', true); }
}
function portmapMapCh() {
  const inp = document.getElementById('portmap-ch-input');
  const ch = inp ? parseInt(inp.value) : 0;
  if (!ch || ch <= 0) { portmapSetStatus('ch番号を入力してください', true); return; }
  portmapSetStatus('ch'+ch+' にマッピング中...', false);
  fetch('/api/portmap/map-ch', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({ch:ch})})
    .then(r=>r.json()).then(d=>{
      portmapSetStatus('ch'+ch+' にマッピングしました', false);
      if (inp) inp.value = '';
      loadPortMapEntries();
    }).catch(()=>portmapSetStatus('エラーが発生しました', true));
}
async function portmapSetAsPatrol() {
  portmapSetStatus('ポートマップからch一覧を取得中...', false);
  let entries;
  try { entries = await fetch('/api/portmap/entries').then(r=>r.json()); } catch(e) { portmapSetStatus('取得失敗', true); return; }
  if (!entries || entries.length === 0) { portmapSetStatus('マッピングデータがありません', true); return; }
  const chs = [...new Set(entries.map(e=>e.ch).filter(c=>c>0 && c<=100))].sort((a,b)=>a-b);
  if (chs.length === 0) { portmapSetStatus('有効なch（1〜100）がありません', true); return; }
  const preview = chs.slice(0,5).join(',') + (chs.length>5 ? '...' : '');
  const ok = confirm('ポートマップの '+chs.length+' ch ['+preview+'] を巡回リストに設定しますか？\n現在のリストは上書きされます。');
  if (!ok) return;
  try {
    const r = await fetch('/api/patrol/channels', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({channels:chs})});
    const d = await r.json();
    if (d.ok) {
      portmapSetStatus('✓ '+chs.length+' chを巡回リストに反映しました', false);
      if (typeof loadPatrolChannels === 'function') loadPatrolChannels();
    } else {
      portmapSetStatus('✗ 保存失敗', true);
    }
  } catch(e) { portmapSetStatus('✗ エラー: '+e.message, true); }
}
// 巡回チャンネルリストから開始Chプルダウン生成・同期
function updateStartChDropdown() {
  fetch('/api/patrol/channels').then(r=>r.json()).then(data=>{
    const sel = document.getElementById('dash-patrol-start-ch-select');
    if (!sel) return;
    sel.innerHTML = '<option value="0">(前回位置)</option>';
    (data.channels||[]).forEach(function(ch){
      sel.innerHTML += '<option value="'+ch+'">'+ch+'</option>';
    });
    // 巡回ビューの開始Ch入力値と同期（リスト外の値は手動optionとして注入）
    var inp = document.getElementById('patrol-start-ch');
    if(inp)_ensureStartChOption(sel, inp.value||'0');
  }).catch(()=>{});
}
document.addEventListener('DOMContentLoaded',function(){
  updateStartChDropdown();
  var sel = document.getElementById('dash-patrol-start-ch-select');
  if(sel){
    sel.addEventListener('change',function(){syncPatrolStartCh(sel.value);});
  }
  applyDMSectionState('portmap');
  applyDMSectionState('devmem');
});
// ...既存コード...
const EDITABLE_LAYOUT_VIEWS=['patrol','chat-log','dashboard'];
let currentViewId='dashboard';
function isLayoutEditableView(id){
	return EDITABLE_LAYOUT_VIEWS.includes(id);
}
function syncLayoutEditState(){
	const dashGrid=document.getElementById('dashboard-grid');
	const patrolRoot=document.getElementById('patrol-layout-root');
	if(dashGrid){
		const dashEdit=layoutEditMode&&currentViewId==='dashboard';
		dashGrid.classList.toggle('edit-mode',dashEdit);
		const swapH=document.getElementById('dash-swap-handle');
		if(swapH)swapH.style.display=dashEdit?'flex':'none';
	}
	if(patrolRoot)patrolRoot.classList.toggle('edit-mode',layoutEditMode&&currentViewId==='patrol');
	const chatView=document.getElementById('view-chat-log');
	if(chatView)chatView.classList.toggle('edit-mode',layoutEditMode&&currentViewId==='chat-log');
	const btn=document.getElementById('nav-layout-edit');
	if(btn){
		const editable=isLayoutEditableView(currentViewId);
		btn.classList.toggle('layout-edit-active',editable&&layoutEditMode);
		btn.classList.toggle('layout-edit-disabled',!editable);
		btn.setAttribute('aria-disabled',editable?'false':'true');
		btn.title=editable?'レイアウト編集':'このページではレイアウト編集できません';
	}
}
function switchView(id,navEl){
  document.querySelectorAll('.view').forEach(v=>v.classList.remove('active'));
  document.querySelectorAll('.nav-item').forEach(n=>n.classList.remove('active'));
  const v=document.getElementById('view-'+id);if(v)v.classList.add('active');
  if(navEl)navEl.classList.add('active');
	currentViewId=id;
	syncLayoutEditState();
	if(id==='detect-log'){const la=document.getElementById('log-area');if(la)la.scrollTop=la.scrollHeight;}
	else if(id==='chat-log'){const ca=document.getElementById('chat-area');if(ca)ca.scrollTop=ca.scrollHeight;}
}
function initPanelDragAndCollapse(){
  const cards=document.querySelectorAll('.card');
  let dragSrc=null;
	function placeDraggedCardInContainer(container, clientY){
		if(!dragSrc||!container)return;
		const siblings=[...container.querySelectorAll(':scope > .card')].filter(card=>card!==dragSrc);
		const target=siblings.find(card=>{
			const rect=card.getBoundingClientRect();
			return clientY < rect.top + rect.height/2;
		});
		if(target)container.insertBefore(dragSrc, target);
		else container.appendChild(dragSrc);
	}
  cards.forEach(card=>{
    card.draggable=true;
	// dashboard-grid系カードのドラッグは専用処理で個別管理
	if(card.closest('.dashboard-grid'))return;
    card.addEventListener('dragstart',e=>{
			const patrolRoot=card.closest('#patrol-layout-root');
			if(!patrolRoot||!patrolRoot.classList.contains('edit-mode')){e.preventDefault();return;}
      const titleEl=card.querySelector('.card-title');
      if(titleEl && !titleEl.contains(e.target)){e.preventDefault();return;}
      dragSrc=card;
      card.classList.add('dragging');
      e.dataTransfer.effectAllowed='move';
      e.dataTransfer.setData('text/plain','');
    });
    card.addEventListener('dragend',()=>{
      card.classList.remove('dragging');
      dragSrc=null;
      document.querySelectorAll('.card-placeholder').forEach(el=>el.classList.remove('card-placeholder'));
    });
    card.addEventListener('dragover',e=>{
      if(!dragSrc || dragSrc===card) return;
      e.preventDefault();
			placeDraggedCardInContainer(card.parentElement, e.clientY);
    });
    card.addEventListener('drop',e=>{
      if(!dragSrc||dragSrc===card) return;
      e.preventDefault();
			placeDraggedCardInContainer(card.parentElement, e.clientY);
			if(dragSrc.closest('#patrol-layout-root'))savePatrolLayout();
      dragSrc.classList.remove('dragging');
      dragSrc=null;
    });
  });
  document.querySelectorAll('.col, .panel-grid').forEach(container=>{
    container.addEventListener('dragover',e=>{
      if(!dragSrc) return;
      e.preventDefault();
			placeDraggedCardInContainer(container, e.clientY);
    });
    container.addEventListener('drop',e=>{
      if(!dragSrc) return;
			e.preventDefault();
			placeDraggedCardInContainer(container, e.clientY);
			if(dragSrc.closest('#patrol-layout-root'))savePatrolLayout();
			dragSrc.classList.remove('dragging');
			dragSrc=null;
    });
  });
}
// ── Grid drag & drop ──
function initGridDragDrop(){
  const grid=document.getElementById('dashboard-grid');if(!grid)return;
  let activeDrag=null;
  let placeholder=null;
  function getOrCreatePlaceholder(){
    if(!placeholder){
      placeholder=document.createElement('div');
      placeholder.className='card grid-drop-indicator';
      placeholder.dataset.placeholder='1';
    }
    return placeholder;
  }
  function syncPlaceholderSize(dragCard){
    const ph=getOrCreatePlaceholder();
    DASH_SIZE_CLASSES.forEach(c=>ph.classList.remove(c));
    const sc=DASH_SIZE_CLASSES.find(c=>dragCard.classList.contains(c))||'panel-size-1x1';
    ph.classList.add(sc);
    return ph;
  }
  function removePlaceholder(){
    if(placeholder&&placeholder.parentNode)placeholder.remove();
  }
  // 2D位置から挿入先兄弟カードを決定する
  function getInsertBefore(clientX,clientY){
    const gridRect=grid.getBoundingClientRect();
    const siblings=[...grid.querySelectorAll(':scope > .card')].filter(c=>c!==activeDrag&&!c.dataset.placeholder);
    for(const s of siblings){
      const r=s.getBoundingClientRect();
      const midY=r.top+r.height/2;
      const midX=r.left+r.width/2;
      // 全幅カード(2列span)はY中心のみで判定
      if(r.width>gridRect.width*0.8){
        if(clientY<midY)return s;
      } else {
        // 1列カード: Yで行を判定しつつXで左右を判定
        if(clientY<r.top)return s; // このカードの行より上
        if(clientY<midY&&clientX<midX)return s; // 同行左半
      }
    }
    return null;
  }
  function updatePlaceholder(clientX,clientY){
    const ph=syncPlaceholderSize(activeDrag);
    const before=getInsertBefore(clientX,clientY);
    if(before){if(ph.nextSibling!==before)grid.insertBefore(ph,before);}
    else{if(grid.lastChild!==ph)grid.appendChild(ph);}
  }
  function attachCard(card){
    card.draggable=true;
    card.addEventListener('dragstart',e=>{
      if(!grid.classList.contains('edit-mode')){e.preventDefault();return;}
			if(e.target&&e.target.closest('.panel-resize-handle-x, .panel-resize-handle-y')){e.preventDefault();return;}
      activeDrag=card;
      card.classList.add('dragging');
      e.dataTransfer.effectAllowed='move';
      e.dataTransfer.setData('text/plain','');
    });
    card.addEventListener('dragend',()=>{
      removePlaceholder();
      card.classList.remove('dragging');
      activeDrag=null;
    });
  }
  grid.querySelectorAll(':scope > .card').forEach(attachCard);
  grid.addEventListener('dragover',e=>{
    if(!activeDrag||!grid.classList.contains('edit-mode'))return;
    e.preventDefault();
    updatePlaceholder(e.clientX,e.clientY);
  });
  grid.addEventListener('drop',e=>{
    if(!activeDrag||!grid.classList.contains('edit-mode'))return;
    e.preventDefault();
    // プレースホルダーの描画カラム（左右）をドロップ前に読み取る
    let targetColumn = getPanelColumn(activeDrag); // フォールバック: 元のカラム
    if(placeholder && placeholder.parentNode === grid) {
      const gridRect = grid.getBoundingClientRect();
      const phRect = placeholder.getBoundingClientRect();
      targetColumn = (phRect.left + phRect.width/2 > gridRect.left + gridRect.width/2) ? 2 : 1;
    }
    if(placeholder&&placeholder.parentNode===grid){
      grid.insertBefore(activeDrag,placeholder);
    }
    removePlaceholder();
    activeDrag.classList.remove('dragging');
    // ドラッグしたカードのカラムクラスのみ更新（他のカードは変更しない）
    if(getPanelWidthUnits(activeDrag) === 1) {
      activeDrag.classList.remove('panel-col-1','panel-col-2');
      activeDrag.classList.add(targetColumn === 2 ? 'panel-col-2' : 'panel-col-1');
    }
    activeDrag=null;
		updateDashboardResizeHandlePositions();
    saveDashboardLayout();
  });
  grid.addEventListener('dragleave',e=>{
    if(!grid.contains(e.relatedTarget))removePlaceholder();
  });
}
function initPatrolGridDragDrop(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	let activeDrag=null;
	let placeholder=null;
	function getOrCreatePlaceholder(){
		if(!placeholder){
			placeholder=document.createElement('div');
			placeholder.className='card grid-drop-indicator';
			placeholder.dataset.placeholder='1';
		}
		return placeholder;
	}
	function syncPlaceholderSize(dragCard){
		const ph=getOrCreatePlaceholder();
		DASH_SIZE_CLASSES.forEach(c=>ph.classList.remove(c));
		const sc=DASH_SIZE_CLASSES.find(c=>dragCard.classList.contains(c))||'panel-size-1x1';
		ph.classList.add(sc);
		return ph;
	}
	function removePlaceholder(){
		if(placeholder&&placeholder.parentNode)placeholder.remove();
	}
	function getInsertBefore(clientX,clientY){
		const gridRect=grid.getBoundingClientRect();
		const siblings=[...grid.querySelectorAll(':scope > .card')].filter(c=>c!==activeDrag&&!c.dataset.placeholder);
		for(const s of siblings){
			const r=s.getBoundingClientRect();
			const midY=r.top+r.height/2;
			const midX=r.left+r.width/2;
			if(r.width>gridRect.width*0.8){
				if(clientY<midY)return s;
			}else{
				if(clientY<r.top)return s;
				if(clientY<midY&&clientX<midX)return s;
			}
		}
		return null;
	}
	function updatePlaceholder(clientX,clientY){
		const ph=syncPlaceholderSize(activeDrag);
		const before=getInsertBefore(clientX,clientY);
		if(before){if(ph.nextSibling!==before)grid.insertBefore(ph,before);}
		else{if(grid.lastChild!==ph)grid.appendChild(ph);}
	}
	function attachCard(card){
		card.draggable=true;
		card.addEventListener('dragstart',e=>{
			if(!grid.classList.contains('edit-mode')){e.preventDefault();return;}
			if(e.target&&e.target.closest('.panel-resize-handle-x, .panel-resize-handle-y')){e.preventDefault();return;}
			activeDrag=card;
			card.classList.add('dragging');
			e.dataTransfer.effectAllowed='move';
			e.dataTransfer.setData('text/plain','');
		});
		card.addEventListener('dragend',()=>{
			removePlaceholder();
			card.classList.remove('dragging');
			activeDrag=null;
		});
	}
	grid.querySelectorAll(':scope > .card').forEach(attachCard);
	grid.addEventListener('dragover',e=>{
		if(!activeDrag||!grid.classList.contains('edit-mode'))return;
		e.preventDefault();
		updatePlaceholder(e.clientX,e.clientY);
	});
	grid.addEventListener('drop',e=>{
		if(!activeDrag||!grid.classList.contains('edit-mode'))return;
		e.preventDefault();
		let targetColumn=getPanelColumn(activeDrag);
		if(placeholder&&placeholder.parentNode===grid){
			const gridRect=grid.getBoundingClientRect();
			const phRect=placeholder.getBoundingClientRect();
			targetColumn=(phRect.left + phRect.width/2 > gridRect.left + gridRect.width/2) ? 2 : 1;
		}
		if(placeholder&&placeholder.parentNode===grid)grid.insertBefore(activeDrag,placeholder);
		removePlaceholder();
		activeDrag.classList.remove('dragging');
		if(getPanelWidthUnits(activeDrag)===1){
			activeDrag.classList.remove('panel-col-1','panel-col-2');
			activeDrag.classList.add(targetColumn===2?'panel-col-2':'panel-col-1');
		}
		activeDrag=null;
		updatePatrolResizeHandlePositions();
		savePatrolLayout();
	});
	grid.addEventListener('dragleave',e=>{
		if(!grid.contains(e.relatedTarget))removePlaceholder();
	});
}
// ── Log filter chips ──
const LOG_CATS=[
  {id:'mumu',  label:'[MuMu]', test:l=>l.includes('[MuMu]')},
  {id:'det',   label:'検知',    test:l=>l.includes('[DETECTION]')||l.includes('[検知]')},
  {id:'chat',  label:'チャット',test:l=>l.includes('[チャット')||l.includes('[CHAT-')},
  {id:'pkt',   label:'パケット',test:l=>/\[0x[0-9a-fA-F]+\]/.test(l)||/\[Instance-/.test(l)},
  {id:'gas',   label:'GAS',    test:l=>l.includes('[GASFetch]')},
  {id:'gui',   label:'GUI',    test:l=>l.includes('[GUI]')},
  {id:'other', label:'その他', test:_=>true},
];
const logFilter={mumu:true,det:true,chat:true,pkt:false,gas:true,gui:true,other:true};
function getCat(line){for(const c of LOG_CATS){if(c.test(line))return c.id;}return 'other';}
function isVisible(line){return logFilter[getCat(line)]!==false;}
function buildFilterBar(){
  const bar=document.getElementById('log-filter-bar');if(!bar)return;
  bar.innerHTML=LOG_CATS.map(c=>'<div id="fc-'+c.id+'" class="chip'+(logFilter[c.id]?' active':'')+'" onclick="toggleCat(\''+c.id+'\')">'+c.label+'</div>').join('');
}
function toggleCat(id){
  logFilter[id]=!logFilter[id];
  const chip=document.getElementById('fc-'+id);if(chip)chip.className='chip'+(logFilter[id]?' active':'');
  document.querySelectorAll('#log-area .log-line').forEach(div=>{div.style.display=isVisible(div.textContent)?'':'none';});
}
// ── Escape ──
function escHtml(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
function escAttrJs(s){return JSON.stringify(String(s)).replace(/"/g,'&quot;');}
// ── Toast 通知（右下スタック・4秒自動消滅） ──
function toast(msg,type){
  let box=document.getElementById('toast-box');
  if(!box){box=document.createElement('div');box.id='toast-box';document.body.appendChild(box);}
  const t=document.createElement('div');
  t.className='toast'+(type==='error'?' error':'');
  t.textContent=msg;
  box.appendChild(t);
  setTimeout(()=>{t.classList.add('out');setTimeout(()=>t.remove(),300);},4000);
}
// ── Log / SSE ──
(function(){
  const la=document.getElementById('log-area');let userScrolling=false;
  if(la){la.addEventListener('scroll',()=>{userScrolling=(la.scrollHeight-la.scrollTop-la.clientHeight)>20;});}
  window._logUserScrolling=()=>userScrolling;
})();
function appendLog(line){
  const la=document.getElementById('log-area');if(!la)return;
  const div=document.createElement('div');
  const isDetect=line.includes('[DETECTION]')||/金ウリボ|金ナッポ|銀ナッポ|ウリボ・ゴールド/.test(line);
  div.className='log-line'+(isDetect?' detect':'');
  let tagCls='info',tagTxt='INFO';
  if(line.includes('完了')||line.includes('確立')||line.includes('起動完了')||line.includes(' ok')||line.includes('[OK]')){tagCls='ok';tagTxt='OK';}
  if(isDetect||line.toLowerCase().includes('warn')){tagCls='warn';tagTxt='WARN';}
  if(line.toLowerCase().includes('error')||line.includes('失敗')||line.toLowerCase().includes('fatal')){tagCls='err';tagTxt='ERR';}
  const tm=line.match(/\d{4}\/\d{2}\/\d{2} (\d{2}:\d{2}:\d{2})/);
  const timeStr=tm?tm[1]:'';const rest=tm?line.slice(tm[0].length).trim():line;
  div.innerHTML='<span class="log-time">'+escHtml(timeStr)+'</span>'
    +'<span class="log-tag '+tagCls+'">'+tagTxt+'</span>'
    +'<span class="log-msg">'+escHtml(rest)+'</span>';
  if(!isVisible(line))div.style.display='none';
  la.appendChild(div);
	if(la.children.length>1500)la.removeChild(la.firstChild);
	if(!window._logUserScrolling())la.scrollTop=la.scrollHeight;
}
async function testDetect(monster){
  const url = monster ? '/api/test-detect?monster=' + encodeURIComponent(monster) : '/api/test-detect';
  await fetch(url,{method:'POST'});
}
(function(){
  const src=new EventSource('/events');
  src.onmessage=e=>{
    appendLog(e.data);
    if(e.data.includes('[DETECTION]')){loadGoldHistory();loadPatrolChannels();if(!e.data.includes('[DETECTION:SILENT]'))playNotifyBeep();}
    else if(e.data.includes('[GUI] 金ウリボ')){loadGoldHistory();loadPatrolChannels();}
    if(e.data.includes('channels.txt')){loadPatrolChannels();}
    if(e.data.includes('[PORTMAP_PENDING]')){pmCheckPending();}
    if(e.data.includes('[NO_DEVICE]')){showNoDeviceDialog();}
  };
  fetch('/api/logs').then(r=>r.json()).then(d=>(Array.isArray(d)?d:(d&&d.logs)||[]).forEach(appendLog));
})();
// ── デバイス未検出ダイアログ ──
function showNoDeviceDialog(){
  const ov=document.getElementById('no-device-overlay');
  if(ov)ov.style.display='flex';
}
// ── PortMap 変更確認モーダル ──
let _pmChanges=[];
async function pmCheckPending(){
  const data=await fetch('/api/portmap/pending').then(r=>r.json()).catch(()=>[]);
  _pmChanges=data||[];
  const ov=document.getElementById('pm-overlay');
  if(!ov)return;
  if(_pmChanges.length>0){pmRender();ov.style.display='flex';}
  // 変更が0件なら既に処理済みなので閉じたまま
}
function pmRender(){
  const body=document.getElementById('pm-body');
  if(!body)return;
  body.innerHTML=_pmChanges.map(c=>{
    const oldPart=c.old_ip?('<span class="pm-ip">'+escHtml(c.old_ip)+'</span><span class="pm-arrow">→</span>'):'<span class="pm-arrow">新規</span>';
    return '<div class="pm-change-row">'
      +'<div><span class="pm-ch">Ch'+Number(c.ch)+'</span></div>'
      +'<div>'+oldPart+'<span class="pm-ip">'+escHtml(c.new_ip)+'</span></div>'
      +'<div class="pm-votes">'+c.vote_count+'台が同一ポートを検知</div>'
      +'</div>';
  }).join('');
}
async function pmApplyAll(){
  const ids=_pmChanges.map(c=>c.id);
  await fetch('/api/portmap/confirm',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ids,action:'apply'})});
  document.getElementById('pm-overlay').style.display='none';
  _pmChanges=[];
}
async function pmRejectAll(){
  const ids=_pmChanges.map(c=>c.id);
  await fetch('/api/portmap/confirm',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ids,action:'reject'})});
  document.getElementById('pm-overlay').style.display='none';
  _pmChanges=[];
}
// ── Gold History ──
function formatAgo(seconds){
  if(seconds < 60) return 'たった今';
  const mins = Math.floor(seconds / 60);
  if(mins < 60) return mins + '分前';
  const hrs = Math.floor(mins / 60);
  return hrs + '時間前';
}
async function removeGoldHistory(timestamp){
  try{
    const res = await fetch('/api/gold-history/delete', {
      method: 'DELETE',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({timestamp})
    });
    if(!res.ok) throw new Error('delete failed');
    await loadGoldHistory();
  }catch(_){ toast('履歴の削除に失敗しました','error'); }
}
async function clearAllGoldHistory(){
	if(!confirm('検知履歴を全件削除しますか？'))return;
	try{
		const res = await fetch('/api/gold-history/clear', {
			method: 'DELETE'
		});
		if(!res.ok) throw new Error('clear failed');
		await loadGoldHistory();
	}catch(_){ toast('履歴のクリアに失敗しました','error'); }
}
async function loadGoldHistory(){
  try{
    const h=await fetch('/api/gold-history').then(r=>r.json());
    const detEl=document.getElementById('dash-detect-count');
    const metaEl=document.getElementById('dash-detect-meta');
    const nowSec=Math.floor(Date.now()/1000);
    let perHour=0;
    let latestAgo='--';
    if(Array.isArray(h) && h.length > 0){
      h.forEach(e=>{
        if(e && typeof e.timestamp === 'number' && nowSec - e.timestamp <= 3600){
          perHour++;
        }
      });
      const latest = h[0];
      if(latest && typeof latest.timestamp === 'number'){
        latestAgo = formatAgo(Math.max(0, nowSec - latest.timestamp));
      }
    }
    if(detEl) detEl.textContent = Array.isArray(h) ? String(h.length) : '0';
    if(metaEl) metaEl.textContent = String(perHour) + '件/時 · ' + latestAgo;
    // v2: track detected channels for matrix overlay + update count badge (20h expiry)
    const detSet = new Set();
    const _detNowSec = Math.floor(Date.now()/1000);
    const _det20h = 20*3600;
    if(Array.isArray(h)) h.forEach(e=>{ if(e && Number.isFinite(e.channel) && (_detNowSec-(e.timestamp||0))<_det20h) detSet.add(Number(e.channel)); });
    _detectedChannels = detSet;
    const goldCnt = document.getElementById('dash-gold-count');
    if(goldCnt) goldCnt.textContent = Array.isArray(h)?String(h.length):'0';
    if(typeof renderChannelMatrix === 'function') renderChannelMatrix();
    const filtered = (!h || h.length===0) ? [] : (_goldFilter==='all' ? h : h.filter(e=>(e.monster_name||'金ウリボ')===_goldFilter));
    const tbl = (filtered.length === 0) ? '<div class="no-history">検知履歴なし</div>'
      : '<table class="gold-table"><colgroup><col class="col-time"><col class="col-name"><col class="col-ch"><col class="col-loc"><col class="col-action"></colgroup>'
      + '<thead><tr><th>時刻</th><th>名前</th><th>Ch</th><th>場所</th><th></th></tr></thead><tbody>'
      + filtered.map(e=>{const nm=e.monster_name||'金ウリボ';const cls=nm==='銀ナッポ'?'name-cell silver':'name-cell';return '<tr><td class="time-cell">'+escHtml(e.time||'')+'</td><td class="'+cls+'">'+escHtml(nm)+'</td><td class="ch-cell">Ch'+Number(e.channel)+'</td><td>'+escHtml(e.location||'')+'</td><td class="action-cell"><button onclick="removeGoldHistory('+Number(e.timestamp)+')">×</button></td></tr>'}).join('')
      + '</tbody></table>';
    const c1=document.getElementById('gold-history-container');if(c1)c1.innerHTML=tbl;
    const c2=document.getElementById('gold-history-container-patrol');if(c2)c2.innerHTML=tbl;
  }catch(_){}
}
// ── Gold history filter ──
let _goldFilter = localStorage.getItem('goldHistoryFilter')||'all';
function setGoldFilter(f,btn,scope){
  _goldFilter=f;
  localStorage.setItem('goldHistoryFilter',f);
  const barId = scope==='patrol'?'gold-filter-bar-patrol':'gold-filter-bar-dash';
  const bar=document.getElementById(barId);
  if(bar) bar.querySelectorAll('.btn-gf').forEach(b=>b.classList.toggle('active',b.dataset.filter===f));
  loadGoldHistory();
}
// ── Channel matrix (v2 dashboard) ──
let _lastPatrolStatus = null;
let _detectedChannels = new Set();
// 'patrol' = show only patrol channels; 'all' = show 1..100
let _matrixMode = localStorage.getItem('dashMatrixMode') || 'patrol';
function setMatrixMode(mode){
  _matrixMode = (mode === 'all') ? 'all' : 'patrol';
  localStorage.setItem('dashMatrixMode', _matrixMode);
  const bp = document.getElementById('btn-matrix-mode-patrol');
  const ba = document.getElementById('btn-matrix-mode-all');
  if(bp)bp.classList.toggle('active', _matrixMode === 'patrol');
  if(ba)ba.classList.toggle('active', _matrixMode === 'all');
  renderChannelMatrix();
}
function renderChannelMatrix(statusOpt){
  const grid = document.getElementById('dash-ch-matrix');
  if(!grid)return;
  if(statusOpt)_lastPatrolStatus = statusOpt;
  // sync toggle button active state on first render
  const bpBtn = document.getElementById('btn-matrix-mode-patrol');
  const baBtn = document.getElementById('btn-matrix-mode-all');
  if(bpBtn && baBtn && !bpBtn.classList.contains('active') && !baBtn.classList.contains('active')){
    if(_matrixMode === 'all') baBtn.classList.add('active');
    else bpBtn.classList.add('active');
  }
  const status = _lastPatrolStatus || {};
  const chs = Array.isArray(patrolChannels)?patrolChannels.slice():[];
  if(chs.length===0 && _matrixMode === 'patrol'){
    grid.innerHTML = '<div style="grid-column:1/-1;color:var(--text3);padding:14px;font-size:var(--fs-sm);text-align:center">巡回チャンネルが登録されていません — <a href="#" onclick="switchView(\'patrol\',document.getElementById(\'nav-patrol\'));return false" style="color:var(--accent)">編集</a></div>';
    const cntEl = document.getElementById('dash-matrix-count');
    if(cntEl)cntEl.textContent = '0 ch';
    return;
  }
  const inPatrol = new Set(chs);
  const moveFailedSet = new Set(Array.isArray(status.move_failed_channels)?status.move_failed_channels:[]);
  const currentCh = Number(status.current_channel)||0;
  const currentIdx = Number(status.current_index);
  const ordered = patrolReversed?chs.slice().sort((a,b)=>b-a):chs.slice().sort((a,b)=>a-b);
  const doneSet = new Set();
  if(status.running && Number.isFinite(currentIdx) && currentIdx >= 0){
    for(let i=0;i<Math.min(currentIdx, ordered.length);i++)doneSet.add(ordered[i]);
  }
  let cellsToShow = [];
  let cols = 10;
  if(_matrixMode === 'all'){
    cols = 20;
    for(let ch=1; ch<=100; ch++)cellsToShow.push(ch);
  }else{
    cellsToShow = chs.slice().sort((a,b)=>a-b);
    const n = cellsToShow.length;
    if(n <= 6) cols = Math.max(4, n);
    else if(n <= 16) cols = 8;
    else if(n <= 30) cols = 10;
    else if(n <= 60) cols = 12;
    else cols = 14;
  }
  grid.style.setProperty('--ch-cols', cols);
  let html = '';
  for(const ch of cellsToShow){
    const isInPatrol = inPatrol.has(ch);
    if(!isInPatrol){
      html += '<div class="ch-cell" title="ch.'+ch+' (巡回対象外)" style="opacity:.22">'+ch+'</div>';
      continue;
    }
    let cls = 'ch-cell queued';
    let title = 'ch.'+ch;
    if(ch === currentCh){cls = 'ch-cell current'; title = 'ch.'+ch+' (現在)';}
    else if(moveFailedSet.has(ch)){cls = 'ch-cell move-failed'; title = 'ch.'+ch+' (移動失敗)';}
    else if(doneSet.has(ch)){cls = 'ch-cell done'; title = 'ch.'+ch+' (完了)';}
    if(_detectedChannels.has(ch)){cls += ' detected'; title += ' \u2605検知済';}
    html += '<div class="'+cls+'" title="'+title+'">'+ch+'</div>';
  }
  grid.innerHTML = html;
  const cntEl = document.getElementById('dash-matrix-count');
  if(cntEl){
    const maxCh = chs.reduce((m,c)=>Math.max(m,c),0);
    cntEl.textContent = (_matrixMode==='all') ? (chs.length+' / 100 ch') : (chs.length+' ch');
  }
}
// ── Dashboard device summary ──
function renderDashDevices(devs,deviceMap){
  const el=document.getElementById('dash-device-list');if(!el)return;
  const cntEl=document.getElementById('dash-dev-count');
  const kpi=document.getElementById('dash-kpi-dev');
  const kpiSub=document.getElementById('dash-kpi-dev-sub');
  const kpiState=document.getElementById('dash-kpi-dev-state');
  if(!devs||devs.length===0){
    el.innerHTML='<div class="no-devices" style="padding:8px;color:var(--text3);font-size:var(--fs-sm)">デバイスが見つかりません</div>';
    if(cntEl)cntEl.textContent='0';
    if(kpi)kpi.textContent='0';
    if(kpiSub)kpiSub.textContent='';
    if(kpiState)kpiState.textContent='検出なし';
    return;
  }
  const connected=devs.length;
  el.innerHTML=devs.map(d=>{
    const info=deviceMap[d]||{};
    const uid=info.user_uid||'',ch=info.current_ch||info.line_id||'',confirmed=info.confirmed||false;
    const sub=uid?((confirmed?'\u{1F517} ':'')+'UID:'+uid+(ch?' Ch'+ch:'')):(d.split(':')[1]||d);
    const chDisplay=ch?String(ch):(confirmed?'<span style="color:var(--accent2);font-size:11px">ON</span>':'\u2014');
    return '<div class="devpill" data-serial="'+escHtml(d)+'">'
      +'<span class="dot2 '+(confirmed?'ok':'warn')+'"></span>'
      +'<div class="dp-body"><div class="dp-name">'+escHtml(d)+'</div><div class="dp-sub">'+escHtml(sub)+'</div></div>'
      +'<span class="dp-ch">'+chDisplay+'</span>'
      +'</div>';
  }).join('');
  if(cntEl)cntEl.textContent=connected+' / '+devs.length;
  if(kpi)kpi.textContent=String(connected);
  if(kpiSub)kpiSub.textContent='/'+devs.length;
  if(kpiState)kpiState.textContent='全台接続中';
}
// ── Devices ──
let selectedDevices=new Set();
let currentDevices=[];
function selectedSerials(){return[...selectedDevices];}
function selectAllDevices(){currentDevices.forEach(d=>selectedDevices.add(d));renderDeviceList();}
function deselectAllDevices(){selectedDevices.clear();renderDeviceList();}
function renderDeviceList(){
  const el=document.getElementById('device-list');if(!el)return;
  if(!currentDevices||currentDevices.length===0){el.innerHTML='<div class="no-devices">デバイスが見つかりません</div>';return;}
  el.innerHTML=currentDevices.map(d=>{
    const info=currentDeviceMap[d]||{};const uid=info.user_uid||'',ch=info.current_ch||info.line_id||'',confirmed=info.confirmed||false;
    const checked=selectedDevices.has(d)?'checked':'';
    const uidHtml=uid?('<span class="uid">'+(confirmed?'🔗':'')+' UID:'+uid+(ch?' Ch'+ch:'')+'</span>'):'';
    const eid='ch-'+encodeURIComponent(d);
    return '<div class="device-entry'+(confirmed?' matched':'')+'">'
      +'<label class="check-label"><input type="checkbox" '+checked+' onchange="toggleDevice('+escAttrJs(d)+',this.checked)"><span class="serial">'+escHtml(d)+'</span>'+uidHtml+'</label>'
      +'<div style="display:flex;gap:6px;margin-top:4px"><input type="number" id="'+escHtml(eid)+'" min="1" max="999" value="1" style="width:65px"><button style="padding:3px 8px;font-size:.8em" onclick="switchOne('+escAttrJs(d)+')">切替</button></div></div>';
  }).join('');
}
let currentDeviceMap={};
async function scanDevices(){
  const st=document.getElementById('adb-op-status');if(st)st.textContent='スキャン中...';
  const r=await fetch('/api/devices');const res=await r.json();
  const devs=Array.isArray(res)?res:(res.devices||[]);
  const mapRes=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
  const deviceMap={};if(mapRes.devices)mapRes.devices.forEach(e=>{if(e.serial)deviceMap[e.serial]=e;if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
  if(devs&&devs.length>0){chatKnownSerials=devs;refreshChatDeviceDropdown();}
  renderDashDevices(devs,deviceMap);
  currentDevices=devs||[];currentDeviceMap=deviceMap;
  renderDeviceList();
  if(st)st.textContent=devs.length>0?'✓ '+devs.length+'台検出':'デバイスが見つかりません';
  setTimeout(()=>{if(st)st.textContent='';},3000);
}
async function runIdentify(){
  if(window._patrolRunning){toast('巡回中は実行できません。巡回を停止してください','error');return;}
  const btn=document.getElementById('btn-identify');
  const st=document.getElementById('adb-op-status');
  if(btn){btn.disabled=true;btn.textContent='🔎 識別中...';}
  if(st)st.textContent='デバイス識別フェーズ実行中（完了まで最大30s）...';
  try{
    const res=await fetch('/api/patrol/identify',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'}).then(r=>r.json()).catch(()=>({ok:false}));
    if(st){st.textContent=res.ok?'✓ 識別開始（バックグラウンド実行中）':'✗ '+(res.error||'識別失敗');setTimeout(()=>st.textContent='',6000);}
  }finally{
    if(btn){btn.disabled=false;btn.textContent='🔎 デバイス認識';}
  }
}
async function restartADB(){
  const st=document.getElementById('adb-op-status');if(st)st.textContent='ADB再起動中...';
  await fetch('/api/adb/restart',{method:'POST'});
  await scanDevices();
}
async function killADB(){
  if(!confirm('ADBサーバーを停止しますか？'))return;
  const st=document.getElementById('adb-op-status');if(st)st.textContent='ADB停止中...';
  const res=await fetch('/api/adb/kill-server',{method:'POST'}).then(r=>r.json()).catch(()=>({ok:false}));
  if(st){st.textContent=res.ok?'✓ ADB停止完了':'✗ 停止失敗';setTimeout(()=>st.textContent='',3000);}
}
async function addADBDevice(){
  const inp=document.getElementById('adb-add-serial');
  const serial=(inp?inp.value:'').trim();
  const st=document.getElementById('adb-op-status');
  if(!serial){if(st){st.textContent='host:portを入力してください';setTimeout(()=>st.textContent='',3000);}return;}
  if(st)st.textContent='接続中...';
  const res=await fetch('/api/adb/connect',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({serial})}).then(r=>r.json()).catch(()=>({ok:false}));
  if(st){st.textContent=res.ok?'✓ '+(res.message||'接続完了'):'✗ '+(res.error||'接続失敗');setTimeout(()=>st.textContent='',4000);}
  if(inp&&res.ok)inp.value='';
  if(res.ok)await scanDevices();
}
function toggleDevice(s,c){c?selectedDevices.add(s):selectedDevices.delete(s);}
// ── Continuous Tap ──
// ── Chat Panel ──
let chatEvents=[],chatIPToSerial={},chatKnownSerials=[],chatKnownLabels=new Set();
let notifySoundEnabled=localStorage.getItem('notifySoundEnabled')!=='false';
let notifySoundVolume=parseFloat(localStorage.getItem('notifySoundVolume')||'0.5');
const DEFAULT_CHAT_LOCATION_RULES=[];
const CHAT_MONSTER_ALIASES=[];
const CHAT_REPORT_VERBS=['発見','出現','いた','居た','います','あり','あります','湧','わき','沸','出た','でた','見つけ','みつけ','確認'];
const CHAT_MONSTER_HINT_WORDS=['金','きん','gold','銀','ぎん','silver','うり','ウリ','豚','猪','boar','なぽ','なっぽ','ナッポ','nappo'];
function chatMessageLength(text){return Array.from(String(text||'')).length;}
function normalizeCsvList(items){return [...new Set((Array.isArray(items)?items:[]).map(v=>String(v||'').trim()).filter(Boolean))];}
function normalizeChatCandidateText(text){
	return String(text||'').toLowerCase().replace(/[\s\u3000・._\-\/]/g,'').replace(/[０-９]/g, ch=>String.fromCharCode(ch.charCodeAt(0)-0xFEE0));
}
function removeKnownChatTokens(baseText, tokenList){
	let rest=String(baseText||'');
	normalizeCsvList(tokenList).map(v=>normalizeChatCandidateText(v)).filter(Boolean).sort((a,b)=>b.length-a.length).forEach(tok=>{
		rest=rest.split(tok).join('');
	});
	return rest;
}
function estimateChatMessageNoiseCount(facts){
	let rest=String(facts&&facts.compactMessage||'');
	if(!rest)return 0;
	if(facts.channel>0){
		const ch=String(facts.channel);
		rest=rest.replace(new RegExp('(?:^|[^0-9])'+ch+'ch','g'),'');
		rest=rest.replace(new RegExp('ch'+ch,'g'),'');
		rest=rest.replace(new RegExp('^'+ch+'(?=[^0-9]|$)','g'),'');
	}
	rest=rest.replace(/[0-9]{2,4}[,、.．][0-9]{2,4}/g,'');
	const tokens=[];
	if(facts.locationRule&&Array.isArray(facts.locationRule.aliases))tokens.push(...facts.locationRule.aliases);
	if(facts.monster){
		const monsterRule=getChatMonsterAliases().find(rule=>rule.name===facts.monster);
		if(monsterRule&&Array.isArray(monsterRule.aliases))tokens.push(...monsterRule.aliases);
		tokens.push(facts.monster);
	}
	tokens.push(...CHAT_REPORT_VERBS);
	tokens.push(...CHAT_MONSTER_HINT_WORDS);
	rest=removeKnownChatTokens(rest,tokens);
	rest=rest.replace(/[%％!！?？,、。．.~-]/g,'');
	rest=rest.replace(/[0-9]/g,'');
	return Array.from(rest).length;
}
function cloneChatLocationRule(rule){
	return {name:String(rule.name||''),aliases:normalizeCsvList(rule.aliases||[]),monsters:normalizeCsvList(rule.monsters||[])};
}
function parseChatLocationRuleLine(line){
	const raw=String(line||'').trim();
	if(!raw)return null;
	const parts=raw.split('|').map(v=>v.trim());
	const name=parts[0]||'';
	if(!name)return null;
	const aliases=normalizeCsvList([name].concat((parts[1]||'').split(',').map(v=>v.trim()).filter(Boolean)));
	const monsters=normalizeCsvList((parts[2]||'').split(',').map(v=>v.trim()).filter(Boolean));
	return {name,aliases,monsters};
}
function parseChatMonsterAliasRuleLine(line){
	const raw=String(line||'').trim();
	if(!raw)return null;
	const parts=raw.split('|').map(v=>v.trim());
	const name=parts[0]||'';
	if(!name)return null;
	const aliases=normalizeCsvList((parts[1]||'').split(',').map(v=>v.trim()).filter(Boolean));
	return {name,aliases};
}
function getChatLocationRules(){
	const merged=new Map();
	DEFAULT_CHAT_LOCATION_RULES.forEach(rule=>{
		const copy=cloneChatLocationRule(rule);
		merged.set(copy.name,copy);
	});
	normalizeCsvList(cfgData.chat_report_location_rules).forEach(line=>{
		const parsed=parseChatLocationRuleLine(line);
		if(!parsed)return;
		const existing=merged.get(parsed.name);
		if(existing){
			existing.aliases=normalizeCsvList(existing.aliases.concat(parsed.aliases));
			existing.monsters=normalizeCsvList(existing.monsters.concat(parsed.monsters));
		}else{
			merged.set(parsed.name,parsed);
		}
	});
	return [...merged.values()];
}
function getChatMonsterAliases(){
	const merged=new Map();
	CHAT_MONSTER_ALIASES.forEach(rule=>{
		merged.set(rule.name,{name:rule.name,aliases:normalizeCsvList(rule.aliases||[])});
	});
	normalizeCsvList(cfgData.chat_report_monster_alias_rules).forEach(line=>{
		const parsed=parseChatMonsterAliasRuleLine(line);
		if(!parsed)return;
		const existing=merged.get(parsed.name);
		if(existing)existing.aliases=normalizeCsvList(existing.aliases.concat(parsed.aliases));
		else merged.set(parsed.name,{name:parsed.name,aliases:parsed.aliases});
	});
	return [...merged.values()];
}
function findAliasGroup(compactText, groups){
	for(const group of groups){
		if(group.aliases.some(alias=>compactText.includes(normalizeChatCandidateText(alias))))return group.name;
	}
	return '';
}
function findAliasRule(compactText, groups){
	for(const group of groups){
		if(group.aliases.some(alias=>compactText.includes(normalizeChatCandidateText(alias))))return group;
	}
	return null;
}
function extractChatCandidateFacts(ev){
	const rawMessage=String(ev&&ev.message||'');
	const rawSender=String(ev&&ev.sender||'');
	const compactMessage=normalizeChatCandidateText(rawMessage);
	const asciiMessage=compactMessage;
	let channel=0;
	let match=asciiMessage.match(/(?:^|[^0-9])([0-9]{1,3})ch/);
	if(!match)match=asciiMessage.match(/ch([0-9]{1,3})/);
	if(!match)match=asciiMessage.match(/^([0-9]{1,3})(?=[^0-9]|$)/);
	if(match)channel=parseInt(match[1],10)||0;
	const locationRule=findAliasRule(compactMessage, getChatLocationRules());
	const location=locationRule?locationRule.name:'';
	const explicitMonster=findAliasGroup(compactMessage, getChatMonsterAliases());
	const hasGoldWord=['金','きん','gold'].some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const hasSilverWord=['銀','ぎん','silver'].some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const hasBoarWord=['うり','ウリ','豚','猪','boar'].some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const hasNappoWord=['なぽ','なっぽ','ナッポ','ナッポ','nappo'].some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const spawnMonsters=locationRule&&Array.isArray(locationRule.monsters)?locationRule.monsters:[];
	let inferredMonster='';
	if(!explicitMonster){
		if(spawnMonsters.length===1){
			inferredMonster=spawnMonsters[0];
		}else if(spawnMonsters.includes('金ナッポ') && spawnMonsters.includes('銀ナッポ')){
			if(hasGoldWord && !hasBoarWord)inferredMonster='金ナッポ';
			else if(hasSilverWord && !hasBoarWord)inferredMonster='銀ナッポ';
			else if(hasNappoWord && hasGoldWord)inferredMonster='金ナッポ';
			else if(hasNappoWord && hasSilverWord)inferredMonster='銀ナッポ';
		}
		if(!inferredMonster){
			if(hasGoldWord && !hasBoarWord && (hasNappoWord || location || channel>0))inferredMonster='金ナッポ';
			else if(hasSilverWord && !hasBoarWord && (hasNappoWord || location || channel>0))inferredMonster='銀ナッポ';
			else if((channel>0 && location) || (channel>0 && hasBoarWord) || (location && hasBoarWord))inferredMonster='ウリボ・ゴールド';
		}
	}
	const monster=explicitMonster||inferredMonster;
	const hasCoords=/[0-9]{2,4}[,、.．][0-9]{2,4}/.test(asciiMessage);
	const hasReportVerb=CHAT_REPORT_VERBS.some(v=>compactMessage.includes(normalizeChatCandidateText(v)));
	const sender=rawSender.toLowerCase();
	return {rawMessage, compactMessage, sender, channel, location, locationRule, spawnMonsters, monster, explicitMonster, inferredMonster, hasGoldWord, hasSilverWord, hasBoarWord, hasNappoWord, hasCoords, hasReportVerb};
}
function getChatCandidateConfig(){
	return {
		senders: normalizeCsvList(cfgData.chat_report_senders),
		excludedSenders: normalizeCsvList(cfgData.chat_report_excluded_senders),
		minLength: parseInt(cfgData.chat_report_min_length)||4,
		maxLength: parseInt(cfgData.chat_report_max_length)||80,
	};
}
function getChatCandidateScore(ev,opts){
	opts=opts||{};
	const facts=extractChatCandidateFacts(ev);
	const message=facts.rawMessage.toLowerCase();
	const sender=facts.sender;
	const length=chatMessageLength(facts.rawMessage);
	const rules=getChatCandidateConfig();
	if(!message||length<rules.minLength||length>rules.maxLength)return 0;
	const excludeKeywords=['ありがとう','ありがと','よろしく','こん','こんばんは','おつ','了解','りょ','募集','売り','買い','null'];
	if(!opts.ignoreExcluded&&rules.excludedSenders.some(v=>sender.includes(v.toLowerCase())))return 0;
	if(excludeKeywords.some(v=>message.includes(v)))return 0;
	if(!facts.channel && !facts.location && !facts.monster && !facts.hasCoords && !facts.hasReportVerb)return 0;
	let score=0;
	if(length>=6&&length<=36)score+=1;
	if(facts.channel>0)score+=2;
	if(facts.location)score+=2;
	if(facts.explicitMonster)score+=3;
	else if(facts.inferredMonster)score+=2;
	if(facts.spawnMonsters.length===1 && facts.location)score+=2;
	if(facts.spawnMonsters.length>1 && facts.location && facts.inferredMonster)score+=1;
	if(facts.hasReportVerb)score+=1;
	if(facts.hasCoords)score+=1;
	if(facts.channel>0 && facts.location)score+=2;
	if(facts.monster && (facts.channel>0 || facts.location))score+=2;
	if(facts.inferredMonster==='ウリボ・ゴールド' && facts.channel>0 && facts.location)score+=2;
	if((facts.inferredMonster==='金ナッポ' || facts.inferredMonster==='銀ナッポ') && facts.channel>0 && facts.location)score+=2;
	if((facts.hasGoldWord || facts.hasSilverWord) && !facts.hasBoarWord)score+=1;
	if(rules.senders.length && (facts.channel>0 || facts.location || facts.monster))score+=1;
	const noiseCount=estimateChatMessageNoiseCount(facts);
	if(noiseCount>0){
		score-=Math.min(8, noiseCount*2);
		if(facts.channel>0 && !facts.location && !facts.monster && noiseCount>=2)score-=2;
	}
	return Math.max(0, score);
}
function isChatCandidate(ev){
	const facts=extractChatCandidateFacts(ev);
	if(!(facts.channel>0) || !facts.location)return false;
	const score=getChatCandidateScore(ev);
	return score>=getChatNotifyMinScore();
}
function isChatExcludedPatrolHit(ev){
	const rules=getChatCandidateConfig();
	const sender=(ev.sender||'').toLowerCase();
	if(!rules.excludedSenders.some(v=>v&&sender.includes(v.toLowerCase())))return false;
	const facts=extractChatCandidateFacts(ev);
	if(!(facts.channel>0))return false;
	return getChatCandidateScore(ev,{ignoreExcluded:true})>=getChatNotifyMinScore();
}
function dedupeChatEvents(source){
	const map=new Map();
	(source||[]).forEach(ev=>{const k=ev.channel+'|'+ev.sender+'|'+ev.message;const prev=map.get(k);if(prev){prev.recv_count=(prev.recv_count||1)+1;}else{map.set(k,Object.assign({},ev,{recv_count:1}));}});
	return Array.from(map.values());
}
function getPickedChatEvents(source){
	return dedupeChatEvents((source||[]).filter(isChatCandidate));
}
function chatCandidateMetaHtml(ev){
	const facts=extractChatCandidateFacts(ev);
	const parts=[];
	if(facts.channel>0)parts.push('Ch'+facts.channel);
	if(facts.location)parts.push(facts.location);
	if(facts.monster)parts.push(facts.monster);
	if(!parts.length)return '';
	const label=facts.explicitMonster?'判定':'推定';
	return '<div style="margin-top:4px;font-size:var(--fs-xs);color:var(--text3)">'+label+': '+escHtml(parts.join(' / '))+'</div>';
}
function chatMsgHtml(ev,opts){
	opts=opts||{};
  const serial=ev.label||chatIPToSerial[ev.client_ip]||ev.client_ip;
  const ch=ev.has_ch?'<span style="color:#4f8ef7;margin-right:3px;font-size:.88em">Ch'+ev.channel+'</span>':'';
	const scoreBadge=opts.report?'<span class="chat-report-score">score '+getChatCandidateScore(ev)+'</span>':'';
	const rowClass='chat-msg'+(opts.report?' report':'');
	const actionHtml=opts.withActions===false?'':'<span class="chat-msg-actions">'
		+'<button type="button" class="chat-action-btn exclude" data-action="sender-exclude" data-sender="'+escHtml(ev.sender)+'" onclick="applyChatFilterAction(this)">発言者-</button>'
		+'</span>';
	return '<div class="'+rowClass+'" onmouseover="this.style.background=\'var(--bg2)\'" onmouseout="this.style.background=\'\'">'
		+'<div class="chat-msg-main">'
		+'<div class="chat-msg-body">'
		+'<span style="color:var(--text3);margin-right:5px">'+escHtml(ev.time)+'</span>'
		+'<span style="color:var(--text2);font-size:.88em;margin-right:3px">['+escHtml(serial)+']</span>'
		+ch+'<span style="color:var(--warn);font-weight:600;margin-right:3px">'+escHtml(ev.sender)+'</span>'
		+'<span class="chat-msg-text">'+escHtml(ev.message)+'</span>'
		+(ev.recv_count>1?'<span class="chat-dup-count" style="color:var(--text3);font-size:.78em;margin-left:6px">('+ev.recv_count+'台受信)</span>':'')
		+(opts.report?chatCandidateMetaHtml(ev):'')
		+scoreBadge
		+'</div>'
		+actionHtml
		+'</div></div>';
}
function getChatRowSelection(row){
	const selection=window.getSelection?window.getSelection():null;
	if(!selection||selection.rangeCount===0)return '';
	const text=String(selection).trim();
	if(!text)return '';
	const range=selection.getRangeAt(0);
	const startNode=range.startContainer;
	const endNode=range.endContainer;
	if(row && row.contains(startNode) && row.contains(endNode))return text;
	return '';
}
function getChatActionValue(action,btn){
	if(action==='sender-include' || action==='sender-exclude')return String(btn.dataset.sender||'').trim();
	const row=btn.closest('.chat-msg');
	const selected=getChatRowSelection(row);
	if(selected)return selected;
	return String(btn.dataset.message||'').trim();
}
function updateChatFilterTextarea(key, values){
	const normalized=normalizeCsvList(values);
	cfgData[key]=normalized;
	const el=document.getElementById('cfg-'+key);
	if(el)el.value=normalized.join('\n');
}
async function appendChatFilterValue(key, value){
	if(!value)return;
	const current=Array.isArray(cfgData[key])?cfgData[key]:[];
	updateChatFilterTextarea(key, current.concat([value]));
	renderChatCandidatePanels();
	await saveConfig(true);
}
async function applyChatFilterAction(btn){
	const action=btn&&btn.dataset?btn.dataset.action:'';
	const value=getChatActionValue(action, btn);
	if(!action||!value)return;
	if(action==='sender-include')await appendChatFilterValue('chat_report_senders', value);
	else if(action==='sender-exclude')await appendChatFilterValue('chat_report_excluded_senders', value);
}
function refreshChatDeviceDropdown(){
  const sel=document.getElementById('chat-device-select');if(!sel)return;
  const current=sel.value;
  const serials=chatKnownSerials.length?[...chatKnownSerials]:[...new Set(Object.values(chatIPToSerial))].sort();
  const labels=[...chatKnownLabels].sort();
  const list=[...new Set([...labels,...serials])].sort();
  sel.innerHTML='<option value="">すべて</option>'+(list.length?list:serials).map(s=>'<option value="'+escHtml(s)+'">'+escHtml(s)+'</option>').join('');
  if((list.length?list:serials).includes(current))sel.value=current;
}
function chatMatchFilter(ev){
  const sel=document.getElementById('chat-device-select');const filterSerial=sel?sel.value:'';
  if(filterSerial){const serial=ev.label||chatIPToSerial[ev.client_ip];if(!serial||serial!==filterSerial)return false;}
  const q=document.getElementById('chat-search')?document.getElementById('chat-search').value.toLowerCase():'';
  if(q&&!(ev.sender.toLowerCase().includes(q)||ev.message.toLowerCase().includes(q)))return false;
  return true;
}
// dash チャットの自動スクロール: ユーザーが上にスクロール中は追従しない（log-area と同パターン）
(function(){
  const el=document.getElementById('dash-chat-area');
  if(el)el.addEventListener('scroll',()=>{window._dashChatUserScrolling=(el.scrollHeight-el.scrollTop-el.clientHeight)>50;});
})();
function renderDashChat(evs){
  const el=document.getElementById('dash-chat-area');if(!el)return;
  if(!evs||!evs.length){el.innerHTML='<div style="color:var(--text3);padding:8px;font-size:.82em">チャットなし</div>';return;}
	el.innerHTML=evs.map(ev=>chatMsgHtml(ev,{withActions:false})).join('');
	if(!window._dashChatUserScrolling)el.scrollTop=el.scrollHeight;
}
function renderChatCandidatePanels(source){
	const picked=getPickedChatEvents(source||chatEvents);
	const full=document.getElementById('chat-report-area');
	const dash=document.getElementById('dash-chat-report-area');
	const summary=document.getElementById('chat-report-summary');
	const rules=getChatCandidateConfig();
	const emptyHtml='<div class="chat-report-empty">候補はまだありません</div>';
	if(summary){
		summary.textContent='発見・出現・湧き・チャンネル番号を含む短文を優先して表示します。';
	}
	if(full)full.innerHTML=picked.length?picked.slice().reverse().map(ev=>chatMsgHtml(ev,{report:true})).join(''):emptyHtml;
	if(dash)dash.innerHTML=picked.length?picked.slice(-6).reverse().map(ev=>chatMsgHtml(ev,{report:true,withActions:false})).join(''):emptyHtml;
}
function isChatAtBottom(){const el=document.getElementById('chat-area');return !el||el.scrollHeight-el.scrollTop-el.clientHeight<50;}
function renderChatPanel(){
  const el=document.getElementById('chat-area');if(!el)return;
  const atBottom=isChatAtBottom();
  const prevScrollTop=el.scrollTop;
  const prevScrollHeight=el.scrollHeight;
  const filtered=chatEvents.filter(chatMatchFilter);
	const deduped=dedupeChatEvents(filtered);
	if(!deduped.length){el.innerHTML='<div style="color:var(--text3);padding:8px;font-size:.82em">チャットなし</div>';renderDashChat([]);renderChatCandidatePanels([]);return;}
  el.innerHTML=deduped.map(chatMsgHtml).join('');
  if(atBottom){el.scrollTop=el.scrollHeight;}
  else{el.scrollTop=prevScrollTop+(el.scrollHeight-prevScrollHeight);}
  renderDashChat(deduped.slice(-15));
	renderChatCandidatePanels(filtered);
}
function playNotifyBeep(){
	if(!notifySoundEnabled)return;
	try{
		const ctx=new(window.AudioContext||window.webkitAudioContext)();
		const osc=ctx.createOscillator();
		const gain=ctx.createGain();
		osc.connect(gain);gain.connect(ctx.destination);
		osc.type='sine';osc.frequency.value=880;
		gain.gain.setValueAtTime(notifySoundVolume,ctx.currentTime);
		gain.gain.exponentialRampToValueAtTime(0.001,ctx.currentTime+0.3);
		osc.start();osc.stop(ctx.currentTime+0.3);
		osc.onended=()=>ctx.close();
	}catch(_){}
}
function toggleNotifySound(){
	notifySoundEnabled=!notifySoundEnabled;
	localStorage.setItem('notifySoundEnabled',notifySoundEnabled);
	applyNotifySoundUI();
	if(notifySoundEnabled)playNotifyBeep();
}
function setNotifyVolume(v){
	notifySoundVolume=parseFloat(v);
	localStorage.setItem('notifySoundVolume',notifySoundVolume);
}
function applyNotifySoundUI(){
	const btn=document.getElementById('notify-sound-toggle');
	const vol=document.getElementById('notify-sound-volume');
	if(btn){btn.textContent=notifySoundEnabled?'🔔':'🔕';btn.classList.toggle('active',notifySoundEnabled);}
	if(vol){vol.value=notifySoundVolume;vol.disabled=!notifySoundEnabled;}
}
function appendChatToPanel(ev){
  const isDup=chatEvents.slice(-50).some(e=>e.channel===ev.channel&&e.sender===ev.sender&&e.message===ev.message);
  if(isDup)return;
  if(ev.label&&!chatKnownLabels.has(ev.label)){chatKnownLabels.add(ev.label);refreshChatDeviceDropdown();}
  chatEvents.push(ev);if(chatEvents.length>500)chatEvents=chatEvents.slice(-500);
  if(isChatCandidate(ev)){
    const facts=extractChatCandidateFacts(ev);
		fetch('/api/chat-report/notify',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:facts.channel||0,message:ev.message,location:facts.location,monster:facts.monster||'',sender:ev.sender||'',score:getChatCandidateScore(ev)})}).catch(()=>{});
  }else if(isChatExcludedPatrolHit(ev)){
    const facts=extractChatCandidateFacts(ev);
    fetch('/api/patrol/remove-ch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:facts.channel,reason:'excluded_sender',sender:ev.sender||'',message:ev.message||''})}).catch(()=>{});
  }
	renderChatPanel();
}
function clearChatPanel(){chatEvents=[];const el=document.getElementById('chat-area');if(el)el.innerHTML='';renderDashChat([]);renderChatCandidatePanels([]);}
async function initChat(){
  const dm=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
  if(dm.devices)dm.devices.forEach(e=>{if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
  refreshChatDeviceDropdown();
  const h=await fetch('/api/chat-log').then(r=>r.json()).catch(()=>[]);
  chatEvents=h||[];renderChatPanel();applyNotifySoundUI();
  const es=new EventSource('/api/chat-events');
  es.onmessage=e=>{try{appendChatToPanel(JSON.parse(e.data));}catch(_){}};
}
async function switchAll(){
  if(window._patrolRunning){toast('巡回中は実行できません。巡回を停止してください','error');return;}
  const ch=document.getElementById('allch').value;
  const bar=document.getElementById('status-bar');if(bar)bar.textContent='切替中...';
  const serials=selectedSerials();
  const r=await fetch('/api/switch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:parseInt(ch),serials})});
  const d=await r.json();
  if(d.results){const failed=d.results.filter(x=>!x.ok);if(bar)bar.textContent=failed.length===0?'✓ 完了':'✗ '+failed.length+'台失敗';}
  else{if(bar)bar.textContent=d.error?'✗ '+d.error:'✗ 失敗';}
  setTimeout(()=>{if(bar)bar.textContent='';},3000);
}
async function switchOne(serial){
  if(window._patrolRunning){toast('巡回中は実行できません。巡回を停止してください','error');return;}
  const ch=parseInt(document.getElementById('ch-'+encodeURIComponent(serial)).value);
  const bar=document.getElementById('status-bar');if(bar)bar.textContent='切替中...';
  const r=await fetch('/api/switch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:ch,serial})});
  const d=await r.json();const res=d.results&&d.results[0];
  if(bar)bar.textContent=res&&res.ok?'✓ 完了':'✗ '+(res&&res.error||d.error||'失敗');
  setTimeout(()=>{if(bar)bar.textContent='';},3000);
}
// ── Tap Loop ──
let tapLoopPollTimer=null;
async function tapLoopStart(){
  const x=parseInt(document.getElementById('tap-x').value)||0;
  const y=parseInt(document.getElementById('tap-y').value)||0;
  const interval=parseInt(document.getElementById('tap-interval').value)||1000;
  const serials=selectedSerials();
  const r=await fetch('/api/adb/tap-loop/start',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({tap_x:x,tap_y:y,interval_ms:interval,serials})});
  const d=await r.json();
  const st=document.getElementById('tap-status');
  if(d.ok){
    if(cfgData){cfgData.tap_loop_x=x;cfgData.tap_loop_y=y;cfgData.tap_loop_interval_ms=interval;saveConfig(true);}
    document.getElementById('btn-tap-start').disabled=true;
    document.getElementById('btn-tap-stop').disabled=false;
    if(st)st.textContent='実行中...';
    startTapLoopPoll();
  } else {
    if(st)st.textContent='✗ '+(d.error||'失敗');
  }
}
async function tapLoopStop(){
  await fetch('/api/adb/tap-loop/stop',{method:'POST'});
  stopTapLoopPoll();
  document.getElementById('btn-tap-start').disabled=false;
  document.getElementById('btn-tap-stop').disabled=true;
  const st=document.getElementById('tap-status');if(st)st.textContent='停止';
}
function startTapLoopPoll(){
  if(tapLoopPollTimer)return;
  tapLoopPollTimer=setInterval(async()=>{
    const d=await fetch('/api/adb/tap-loop/status').then(r=>r.json()).catch(()=>null);
    if(!d)return;
    const st=document.getElementById('tap-status');
    if(d.running){
      if(st)st.textContent='実行中 | タップ数: '+d.tick_count;
    } else {
      if(st)st.textContent='停止';
      document.getElementById('btn-tap-start').disabled=false;
      document.getElementById('btn-tap-stop').disabled=true;
      stopTapLoopPoll();
    }
  },1000);
}
function stopTapLoopPoll(){
  if(tapLoopPollTimer){clearInterval(tapLoopPollTimer);tapLoopPollTimer=null;}
}
(async function(){
  const d=await fetch('/api/adb/tap-loop/status').then(r=>r.json()).catch(()=>null);
  if(d&&d.running){
    document.getElementById('btn-tap-start').disabled=true;
    document.getElementById('btn-tap-stop').disabled=false;
    const xi=document.getElementById('tap-x'),yi=document.getElementById('tap-y'),ii=document.getElementById('tap-interval');
    if(xi)xi.value=d.tap_x;if(yi)yi.value=d.tap_y;if(ii)ii.value=d.interval_ms;
    const st=document.getElementById('tap-status');if(st)st.textContent='実行中 | タップ数: '+d.tick_count;
    startTapLoopPoll();
  }
})();
// ── Patrol ──
let patrolChannels=[],patrolReversed=localStorage.getItem('patrolReversed')==='true',patrolLoopMode=localStorage.getItem('patrolLoopMode')!=='false';
function _setBtnLabel(b,label){
  // If button uses SVG icon (v2), keep icon and update only the trailing text node.
  let textNode=null;
  for(const n of b.childNodes){if(n.nodeType===Node.TEXT_NODE && n.textContent.trim()){textNode=n;break;}}
  if(textNode){textNode.textContent=' '+label;}
  else b.textContent=label;
}
function applyReversedUI(){['btn-reversed','dash-btn-reversed'].forEach(id=>{const b=document.getElementById(id);if(!b)return;_setBtnLabel(b,patrolReversed?'逆順':'正順');b.classList.toggle('active',patrolReversed);});}
function applyLoopUI(){['btn-loop','dash-btn-loop'].forEach(id=>{const b=document.getElementById(id);if(!b)return;_setBtnLabel(b,patrolLoopMode?'ループ':'一巡');b.classList.toggle('active',!patrolLoopMode);});}
async function loadPatrolChannels(){
  const d=await fetch('/api/patrol/channels').then(r=>r.json());
  patrolChannels=d.channels||[];renderPatrolChSelector();
  const sel=document.getElementById('dash-patrol-start-ch-select');
  if(sel){const cur=sel.value;sel.innerHTML='<option value="0">(前回位置)</option>'+patrolChannels.map(ch=>'<option value="'+ch+'">'+ch+'</option>').join('');_ensureStartChOption(sel,cur||'0');}
  if(typeof renderChannelMatrix==='function')renderChannelMatrix();
}
function renderPatrolChSelector(){
  const el=document.getElementById('ch-sel-grid');if(!el)return;
  const set=new Set(patrolChannels);
  const cells=[];
  for(let ch=1;ch<=100;ch++){
    const sel=set.has(ch)?' selected':'';
    cells.push('<div class="ch-sel-cell'+sel+'" data-ch="'+ch+'" onclick="toggleChannel('+ch+')">'+ch+'</div>');
  }
  el.innerHTML=cells.join('');
  _updateChSelectedCount();
}
function _updateChSelectedCount(){
  const el=document.getElementById('ch-selected-count');
  if(el)el.textContent='('+patrolChannels.length+'/100)';
}
function toggleChannel(ch){
  const i=patrolChannels.indexOf(ch);
  if(i>=0)patrolChannels.splice(i,1);
  else{patrolChannels.push(ch);patrolChannels.sort((a,b)=>a-b);}
  const cell=document.querySelector('.ch-sel-cell[data-ch="'+ch+'"]');
  if(cell)cell.classList.toggle('selected');
  _updateChSelectedCount();
  _autoSaveChannels();
}
function selectAllChannels(){
  patrolChannels=Array.from({length:100},(_,i)=>i+1);
  renderPatrolChSelector();_autoSaveChannels();
}
function clearAllChannels(){
  patrolChannels=[];
  renderPatrolChSelector();_autoSaveChannels();
}
function applyChannelList(){
  const inp=document.getElementById('ch-list-input');
  if(!inp)return;
  const nums=[...new Set(
    inp.value.split(/[,\s]+/).map(s=>parseInt(s,10)).filter(n=>!isNaN(n)&&n>=1&&n<=100)
  )].sort((a,b)=>a-b);
  if(!nums.length)return;
  patrolChannels=nums;
  inp.value='';
  renderPatrolChSelector();_autoSaveChannels();
}
let _chSaveTimer=null;
function _autoSaveChannels(){
  const st=document.getElementById('ch-save-status');
  if(st)st.textContent='保存中...';
  if(_chSaveTimer)clearTimeout(_chSaveTimer);
  _chSaveTimer=setTimeout(async()=>{
    try{
      await fetch('/api/patrol/channels',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channels:patrolChannels})});
      const st2=document.getElementById('ch-save-status');
      if(st2){st2.textContent='✓ 保存済み';setTimeout(()=>{st2.textContent='';},2000);}
    }catch(e){const st2=document.getElementById('ch-save-status');if(st2)st2.textContent='保存失敗';}
  },300);
}
function toggleReversed(){patrolReversed=!patrolReversed;localStorage.setItem('patrolReversed',patrolReversed);applyReversedUI();}
function toggleLoop(){patrolLoopMode=!patrolLoopMode;localStorage.setItem('patrolLoopMode',patrolLoopMode);applyLoopUI();}
// select に該当 option がない値（巡回chリスト外の手動値）は「(手動: N)」option を動的追加して選択する
function _ensureStartChOption(sel,v){
  const sv=String(v);
  if(![...sel.options].some(o=>o.value===sv)){
    const o=document.createElement('option');o.value=sv;o.textContent='(手動: '+sv+')';o.dataset.manual='1';
    sel.appendChild(o);
  }
  sel.value=sv;
}
function syncPatrolStartCh(v){
  const el=document.getElementById('patrol-start-ch');
  if(el&&el.value!==String(v))el.value=v;
  const sel=document.getElementById('dash-patrol-start-ch-select');
  if(sel&&sel.value!==String(v))_ensureStartChOption(sel,v);
}
async function patrolStart(){
  const chs=patrolChannels.length>0?patrolChannels:[];
  const startChEl=document.getElementById('patrol-start-ch');
  const body={serials:selectedSerials(),reversed:patrolReversed,loop_mode:patrolLoopMode,start_channel:parseInt(startChEl?.value)||0};
  if(chs.length>0)body.channels=chs;
  const r=await fetch('/api/patrol/start',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  const d=await r.json();if(!d.ok)toast('巡回開始失敗: '+(d.error||''),'error');
}
async function patrolStop(){await fetch('/api/patrol/stop',{method:'POST'});}
async function patrolAllOnce(){
  const startCh=parseInt(document.getElementById('patrol-all-start-ch')?.value)||1;
  const endCh=parseInt(document.getElementById('patrol-all-end-ch')?.value)||100;
  if(startCh>endCh){toast('開始chが終了chより大きいです','error');return;}
  const channels=[];for(let i=startCh;i<=endCh;i++)channels.push(i);
  const body={serials:selectedSerials(),reversed:false,loop_mode:false,channels};
  const r=await fetch('/api/patrol/start',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  const d=await r.json();if(!d.ok)toast('巡回開始失敗: '+(d.error||''),'error');
}
async function clearMoveFailedChannels(){await fetch('/api/patrol/clear-move-failed',{method:'POST'});}
const patrolCycleStats={lastMoveStartAt:0,lastMoveIndex:-1,cycleMsHistory:[],avgCycleMs:0};
function formatPatrolCycleDuration(ms){
	if(!(ms>0))return '--';
	const totalSeconds=Math.max(1,Math.round(ms/1000));
	const minutes=Math.floor(totalSeconds/60);
	const seconds=totalSeconds%60;
	if(minutes<=0)return seconds+'s';
	return minutes+'m '+(seconds<10?'0':'')+seconds+'s';
}
function formatPatrolCycleRate(ms){
	if(!(ms>0))return '--';
	const perHour=3600000/ms;
	const rounded=Math.round(perHour*10)/10;
	return (Number.isInteger(rounded)?String(Math.round(rounded)):rounded.toFixed(1))+'ch/時';
}
function renderPatrolCycleStats(running){
	const pairs=[
		['ps-cycle-time','ps-cycle-rate'],
		['dash-ps-cycle-time','dash-ps-cycle-rate'],
	];
	pairs.forEach(([timeId,rateId])=>{
		const timeEl=document.getElementById(timeId);
		const rateEl=document.getElementById(rateId);
		if(!timeEl)return;
		timeEl.textContent=formatPatrolCycleDuration(patrolCycleStats.avgCycleMs);
		if(rateEl)rateEl.textContent=formatPatrolCycleRate(patrolCycleStats.avgCycleMs);
	});
	// v2: KPI cycle tile
	const kpiCycle=document.getElementById('dash-kpi-cycle');
	const kpiCycleSub=document.getElementById('dash-kpi-cycle-sub');
	const kpiCyclePer=document.getElementById('dash-kpi-cycle-per');
	if(kpiCycle){
		kpiCycle.textContent=formatPatrolCycleDuration(patrolCycleStats.avgCycleMs);
	}
	if(kpiCycleSub){
		const avg=patrolCycleStats.avgCycleMs;
		const chCount=Array.isArray(patrolChannels)?patrolChannels.length:0;
		if(avg>0 && chCount>0){
			const perCh=Math.round(avg/chCount/1000);
			if(kpiCyclePer)kpiCyclePer.textContent=perCh+'s';
		}else if(kpiCyclePer){kpiCyclePer.textContent='--';}
		kpiCycleSub.textContent='';
	}
}
function updatePatrolCycleStats(d,currentPhase){
	if(!d.running){
		patrolCycleStats.lastMoveStartAt=0;
		patrolCycleStats.lastMoveIndex=-1;
		renderPatrolCycleStats(false);
		return;
	}
	if(currentPhase==='adb_sending'){
		const startedAt=Number(d.phase_started_at_unix_ms||0);
		const idx=Number(d.current_index);
		if(startedAt>0 && idx!==patrolCycleStats.lastMoveIndex){
			if(patrolCycleStats.lastMoveStartAt>0){
				const cycleMs=startedAt-patrolCycleStats.lastMoveStartAt;
				if(cycleMs>10000 && cycleMs<86400000){
					patrolCycleStats.cycleMsHistory.push(cycleMs);
					if(patrolCycleStats.cycleMsHistory.length>10)patrolCycleStats.cycleMsHistory.shift();
					const sum=patrolCycleStats.cycleMsHistory.reduce((a,b)=>a+b,0);
					patrolCycleStats.avgCycleMs=sum/patrolCycleStats.cycleMsHistory.length;
				}
			}
			patrolCycleStats.lastMoveStartAt=startedAt;
			patrolCycleStats.lastMoveIndex=idx;
		}
	}
	renderPatrolCycleStats(true);
}
function updatePatrolUI(running){
  window._patrolRunning=running;
  ['btn-patrol-start','dash-btn-patrol-start'].forEach(id=>{const b=document.getElementById(id);if(b)b.disabled=running;});
  ['btn-patrol-stop','dash-btn-patrol-stop'].forEach(id=>{const b=document.getElementById(id);if(b)b.disabled=!running;});
  const idb=document.getElementById('btn-identify');
  if(idb){idb.disabled=running;idb.title=running?'巡回中は実行できません。巡回を停止してください':'デバイスとUID(インスタンス番号)を紐付けます。巡回停止中に実行してください';}
  const bar=document.getElementById('hdr-bar');
  if(bar){bar.className=running?'titlebar running':'titlebar';}
}
let _pollFailCount=0;
window._backendDisconnected=false;
async function pollPatrolStatus(){
  try{
    const d=await fetch('/api/patrol/status').then(r=>r.json());
    _pollFailCount=0;
    window._backendDisconnected=false;
    _uptimePatrolling=!!d.running;
    const els=(id)=>document.getElementById(id);
    if(d.running){
			const phaseMap = {
				adb_sending: {label:'ADB送信中',  start:0,  end:15},
				wait_0x2e:   {label:'シグナル待ち', start:15, end:60},
				stabilizing: {label:'安定化中',   start:60, end:90},
				dwell_wait:  {label:'滞在待機',   start:90, end:100},
				// 旧値互換（No.37以前のキャッシュ等に対応）
				move_start:  {label:'移動開始',   start:0,  end:20},
				loading:     {label:'ロード中',   start:20, end:68},
			};
      const currentPhase = d.phase && phaseMap[d.phase] ? d.phase : (d.waiting_move ? 'wait_0x2e' : 'adb_sending');
			const phaseState = phaseMap[currentPhase] || {label:'巡回中', start:0, end:0};
			const now = Date.now();
			const startedAt = Number(d.phase_started_at_unix_ms || 0);
			const totalMs = Math.max(0, Number(d.phase_total_secs || 0) * 1000);
			const elapsedMs = startedAt > 0 ? Math.max(0, now - startedAt) : 0;
			let progressPct = phaseState.end;
			if(totalMs > 0 && phaseState.end > phaseState.start){
				const ratio = Math.min(elapsedMs / totalMs, 1);
				progressPct = phaseState.start + ((phaseState.end - phaseState.start) * ratio);
			}else if(phaseState.end > phaseState.start){
				progressPct = phaseState.start + ((phaseState.end - phaseState.start) * 0.6);
			}
			const remainingSecs = totalMs > 0 ? Math.max(0, Math.ceil((totalMs - elapsedMs) / 1000)) : 0;
			let phaseLabel = phaseState.label;
			if(currentPhase === 'wait_0x2e' && d.avg_signal_latency_secs > 0 && d.switch_started_at_unix_ms > 0){
				const switchElapsedMs = now - Number(d.switch_started_at_unix_ms);
				const predictedRemainMs = d.avg_signal_latency_secs * 1000 - switchElapsedMs;
				if(predictedRemainMs > 1000){
					phaseLabel += ' ~' + Math.ceil(predictedRemainMs / 1000) + 's';
				} else if(predictedRemainMs > 0){
					phaseLabel += ' ~まもなく';
				} else {
					phaseLabel += ' (遅延)';
				}
			}
			const progressText = Math.round(progressPct) + '%';
			const showTimeout = remainingSecs > 0 && currentPhase !== 'wait_0x2e';
			const timeoutText = showTimeout ? '⏱ ' + remainingSecs + 's' : '';
			const timeoutColor = showTimeout ? (remainingSecs <= 10 ? 'var(--danger)' : remainingSecs <= 20 ? 'var(--warn)' : 'var(--accent2)') : '';
      updatePatrolCycleStats(d, currentPhase);
      ['ps-state'].forEach(id=>{const e=els(id);if(e){e.className='running';e.textContent='▶ '+phaseState.label+(d.waiting_move?' ⏳':'');}});
      { const e=els('dash-ps-state'); if(e){e.className='hero-state';e.textContent=phaseState.label+(d.waiting_move?' ⏳':'');} }
      { const h=els('dash-hero'); if(h){h.classList.remove('is-stopped','is-warn');} }
      ['ps-ch'].forEach(id=>{const e=els(id);if(e)e.textContent='Ch'+d.current_channel;});
      { const e=els('dash-ps-ch'); if(e)e.textContent='ch.'+d.current_channel; }
      ['ps-prog','dash-ps-prog'].forEach(id=>{const e=els(id);if(e)e.textContent=(d.current_index+1)+'/'+d.total_channels;});
      ['dash-patrol-label','ps-patrol-label'].forEach(id=>{const e=els(id);if(e)e.textContent=phaseLabel;});
      ['dash-patrol-percent','ps-patrol-percent'].forEach(id=>{const e=els(id);if(e)e.textContent=progressText;});
			['dash-patrol-fill','ps-progress-fill'].forEach(id=>{const e=els(id);if(e)e.style.width=Math.round(progressPct)+'%';});
			['dash-patrol-timeout','ps-patrol-timeout'].forEach(id=>{const e=els(id);if(e){e.textContent=timeoutText;e.style.color=timeoutColor;}});
      ['ps-parallel','dash-ps-parallel'].forEach(id=>{
        const par=els(id);if(par){
          const delay=d.parallel_group_delay>0?'(+'+d.parallel_group_delay+'s)':'';
          par.textContent=(d.parallel_limit===0?'並列:無制限':'並列:'+d.parallel_limit+'台'+delay)+(d.move_timeout_secs>0?' | timeout:'+d.move_timeout_secs+'s':'')+' | 滞在:'+Math.round(d.dwell_secs)+'s';
        }
      });
      updatePatrolUI(true);
    }else{
      ['ps-state'].forEach(id=>{const e=els(id);if(e){e.className='stopped';e.textContent='■ 停止中';}});
      { const e=els('dash-ps-state'); if(e){e.className='hero-state';e.textContent='停止中';} }
      { const h=els('dash-hero'); if(h){h.classList.add('is-stopped');h.classList.remove('is-warn');} }
      ['ps-ch','ps-prog','dash-ps-prog'].forEach(id=>{const e=els(id);if(e)e.textContent=id==='ps-ch'&&d.last_channel>0?'前回: Ch'+d.last_channel:'';});
      { const e=els('dash-ps-ch'); if(e)e.textContent=d.last_channel>0?'ch.'+d.last_channel:'--'; }
			['dash-patrol-label','ps-patrol-label'].forEach(id=>{const e=els(id);if(e)e.textContent='--';});
			['dash-patrol-percent','ps-patrol-percent'].forEach(id=>{const e=els(id);if(e)e.textContent='';});
			['dash-patrol-fill','ps-progress-fill'].forEach(id=>{const e=els(id);if(e)e.style.width='0%';});
			['dash-patrol-timeout','ps-patrol-timeout'].forEach(id=>{const e=els(id);if(e)e.textContent='';});
			updatePatrolCycleStats(d, '');
      ['ps-parallel','dash-ps-parallel'].forEach(id=>{const par=els(id);if(par)par.textContent='';});
      updatePatrolUI(false);
    }
    [['ps-move-failed','btn-clear-move-failed'],['dash-ps-move-failed','btn-dash-clear-move-failed']].forEach(([failId,clearId])=>{
      const failEl=els(failId),clearBtn=els(clearId);
      if(d.move_failed_channels&&d.move_failed_channels.length){
        if(failEl){
          if(failId==='dash-ps-move-failed')failEl.textContent='ch.'+d.move_failed_channels.join(', ch.');
          else failEl.textContent='🚫 移動失敗: Ch'+d.move_failed_channels.join(', Ch');
        }
        if(clearBtn)clearBtn.style.display='';
      }else{if(failEl)failEl.textContent='';if(clearBtn)clearBtn.style.display='none';}
    });
    // v2: toggle hero-bar move-failed wrapper visibility
    { const w=els('dash-ps-move-failed-wrap'); if(w)w.style.display=(d.move_failed_channels&&d.move_failed_channels.length)?'':'none'; }
    // v2: render channel matrix using current patrol status
    if(typeof renderChannelMatrix==='function')renderChannelMatrix(d);
    const crashed=d.crashed_instances&&d.crashed_instances.length?d.crashed_instances:null;
    const showWarn=!crashed&&d.running&&(d.consecutive_move_fail_count||0)>=3;
    const warnMsg=crashed?'⚠ クラッシュ判定: '+crashed.join(', ')+' (3回連続未応答)':'⚠ ゲームクライアントがch移動できない状態です（クラッシュの可能性）。ADBサーバーを再起動してください。';
    ['crash-warning','dash-crash-warning'].forEach(id=>{
      const e=els(id);if(!e)return;
      const show=crashed||showWarn;
      e.style.display=show?'':'none';if(show)e.textContent=warnMsg;
    });
    try{
      const ds=await fetch('/api/patrol/device-statuses').then(r=>r.json());
      renderDeviceStatuses(ds);
    }catch(e){}
  }catch(e){
    console.warn('patrol status error:',e);
    _pollFailCount++;
    // 3回連続失敗 = バックエンド接続断とみなし hero に表示（成功で自動復帰）
    if(_pollFailCount>=3&&!window._backendDisconnected){
      window._backendDisconnected=true;
      const e1=document.getElementById('dash-ps-state');if(e1)e1.textContent='接続断';
      const h=document.getElementById('dash-hero');if(h){h.classList.remove('is-stopped');h.classList.add('is-warn');}
      const ps=document.getElementById('ps-state');if(ps){ps.className='stopped';ps.textContent='⚠ 接続断';}
      ['dash-patrol-label','ps-patrol-label'].forEach(id=>{const el=document.getElementById(id);if(el)el.textContent='バックエンド応答なし';});
    }
  }
  finally{setTimeout(pollPatrolStatus,1000);}
}
function renderDeviceStatuses(statuses){
  const tbody=document.getElementById('device-status-tbody');if(!tbody)return;
  if(!statuses||!statuses.length){tbody.innerHTML='<tr><td colspan="6" style="color:var(--text3)">デバイスデータなし</td></tr>';return;}
  tbody.innerHTML=statuses.map(function(d){
    var badge,state;
    if(d.recovering){badge='dev-stat-recover';state='🔄 復帰中';}
    else if(d.game_crashed){badge='dev-stat-crash';state='🔴 クラッシュ';}
    else if(d.adb_failed){badge='dev-stat-adb';state='⚠ ADB失敗';}
    else if(d.timed_out_last){badge='dev-stat-timeout';state='🟡 タイムアウト';}
    else{badge='dev-stat-ok';state='🟢 正常';}
    var recoverBtn=d.game_crashed&&!d.recovering
      ?'<button class="btn" style="padding:1px 8px;font-size:.75em" onclick="recoverDevice(\''+escHtml(d.serial)+'\')">復帰</button>'
      :'';
    var consec=d.consecutive_timeout>1?' x'+d.consecutive_timeout:'';
    return '<tr>'
      +'<td style="font-family:var(--font-mono);font-size:.75em">'+escHtml(d.serial)+'</td>'
      +'<td>'+escHtml(d.label||'--')+'</td>'
      +'<td>'+(d.current_ch>0?d.current_ch:'--')+'</td>'
      +'<td>'+(d.last_load_secs>0?d.last_load_secs.toFixed(1):'--')+'</td>'
      +'<td><span class="dev-stat-badge '+badge+'">'+state+'</span>'+consec+'</td>'
      +'<td>'+recoverBtn+'</td>'
      +'</tr>';
  }).join('');
  updateDashDeviceCh(statuses);
}
function updateDashDeviceCh(statuses){
  if(!statuses||!statuses.length)return;
  let abnormal=0;
  statuses.forEach(function(ds){
    const bad=!!(ds.game_crashed||ds.adb_failed);
    if(bad)abnormal++;
    const pill=document.querySelector('.devpill[data-serial="'+CSS.escape(ds.serial)+'"]');
    if(!pill)return;
    pill.classList.toggle('offline',bad);
    const chEl=pill.querySelector('.dp-ch');if(!chEl)return;
    const ch=ds.actual_ch>0?ds.actual_ch:(ds.current_ch>0?ds.current_ch:0);
    chEl.textContent=ch?String(ch):'—';
    if(currentDeviceMap[ds.serial])currentDeviceMap[ds.serial].current_ch=ch;
  });
  const kpiState=document.getElementById('dash-kpi-dev-state');
  if(kpiState)kpiState.textContent=abnormal>0?('⚠ '+abnormal+'台異常'):'全台接続中';
}
async function recoverDevice(serial){
  const r=await fetch('/api/patrol/recover',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({serial})});
  const d=await r.json();
  if(!d.ok)toast('復帰失敗: '+(d.error||''),'error');
}
// ── Config ──
const CHAT_FILTER_FIELDS=[
	{k:'chat_report_senders',label:'候補発言者(含む)',type:'multiline-list',desc:'hidden'},
	{k:'chat_report_excluded_senders',label:'候補発言者(除外)',type:'multiline-list',desc:'hidden'},
];
const CHAT_RULE_FIELDS=[
	{k:'chat_report_location_rules',label:'地点別名ルール',type:'multiline-list',desc:'1行形式: 地点名|別名|モンスター1,モンスター2'},
	{k:'chat_report_monster_alias_rules',label:'モンスター別名ルール',type:'multiline-list',desc:'1行形式: モンスター名|別名'},
];
const CHAT_FILTER_MINIMIZED_KEY='settingsChatFilterMinimized';
let chatFilterMinimized=localStorage.getItem(CHAT_FILTER_MINIMIZED_KEY)==='true';
const CHAT_NOTIFY_SCORE_MIN=0;
const CHAT_NOTIFY_SCORE_MAX=20;
const CHAT_NOTIFY_SCORE_DEFAULT=6;
const CHAT_NOTIFY_SCORE_KEY='chatNotifyMinScore';
let chatNotifyMinScore=CHAT_NOTIFY_SCORE_DEFAULT;
try{
	const saved=parseInt(localStorage.getItem(CHAT_NOTIFY_SCORE_KEY)||'',10);
	if(Number.isFinite(saved))chatNotifyMinScore=saved;
}catch(_){ }
const CFG_FIELDS_DISCORD=[
  {k:'discord_webhook',label:'Discord Webhook URL (検知報告)',type:'text',desc:'空にするとDiscord通知無効',testBtn:true},
  {k:'discord_chat_report_webhook',label:'Discord Webhook URL (ワルチャ報告)',type:'text',desc:'チャット報告候補を別チャンネルに通知。空で無効',testBtn:true},
  {k:'chat_exclude',label:'チャット除外キーワード',type:'csv',desc:'カンマ区切り。例: いない,終わった'},
  {k:'chat_report_min_length',label:'報告候補 最小文字数',type:'number',desc:'0でデフォルト(4)。これ未満のメッセージを除外'},
  {k:'chat_report_max_length',label:'報告候補 最大文字数',type:'number',desc:'0でデフォルト(80)。これ超のメッセージを除外'},
];
const CFG_FIELDS_PATROL=[
  {k:'patrol_dwell_secs',label:'滞在時間 (秒)',type:'number',desc:'ch移動完了後〜次ch移動開始までの待機秒数'},
  {k:'patrol_move_timeout_secs',label:'初回マージ待ちタイムアウト (秒)',type:'number',desc:'1台目のマージを待つ最大秒数。0=無効 (適応型有効時は下限値として機能)'},
  {k:'patrol_merge_timeout_secs',label:'残りマージ待ちタイムアウト (秒)',type:'number',desc:'1台目受信後、残り台数を待つ最大秒数 (適応型有効時は下限値として機能)'},
  {k:'patrol_adaptive_timeout',label:'適応型タイムアウト',type:'bool',desc:'実ロード時間を学習してタイムアウトを自動延長。ロード中デバイスへの早期切替を防止'},
  {k:'patrol_adaptive_timeout_window',label:'適応型: 学習サンプル数',type:'number',desc:'参照する直近ロード回数（デフォルト: 10）'},
  {k:'patrol_load_stabilization_auto',label:'ロード安定化遅延: 自動',type:'bool',desc:'ONの場合、0x2E受信からの遅延をロード観測データから自動算出する。OFFは手動値を使用'},
  {k:'patrol_load_stabilization_secs',label:'ロード安定化遅延: 手動値 (秒)',type:'number',desc:'0x2E UUID受信から完了シグナル発火までの待機秒数。自動モードOFF時に使用（0=デフォルト6s）'},
  {k:'patrol_load_detect_mode',label:'ロード完了判定方式',type:'select',options:['time','either','screen'],desc:'time=時間のみ(従来)、screen=ADBスクショで黒画面消失検知のみ、either=両方並走で先勝ち(推奨)'},
  {k:'patrol_screen_poll_ms',label:'画面判定: ポーリング間隔 (ms)',type:'number',desc:'200-2000推奨。短すぎるとADB負荷増。デフォルト500'},
  {k:'_screen_region_picker',label:'',type:'region-picker-btn',desc:''},
  {k:'patrol_screen_region_x',label:'画面判定: 監視矩形 X (px)',type:'number',desc:'監視矩形の左上 X 座標（px）。エミュレータ 1280x720 想定でデフォルト340'},
  {k:'patrol_screen_region_y',label:'画面判定: 監視矩形 Y (px)',type:'number',desc:'監視矩形の左上 Y 座標（px）。デフォルト160（1280x720想定）'},
  {k:'patrol_screen_region_w',label:'画面判定: 監視矩形 幅 (px)',type:'number',desc:'監視矩形の幅（px）。デフォルト600（1280x720想定）。0以下で全自動リセット'},
  {k:'patrol_screen_region_h',label:'画面判定: 監視矩形 高さ (px)',type:'number',desc:'監視矩形の高さ（px）。デフォルト400（1280x720想定）。0以下で全自動リセット'},
  {k:'patrol_screen_black_luma',label:'画面判定: 黒輝度閾値 (0-255)',type:'number',desc:'これ以下を黒画素とみなす。デフォルト25'},
  {k:'patrol_screen_black_pixel_ratio',label:'画面判定: 黒画素割合下限 (0-1)',type:'number',desc:'この割合以上が黒なら「黒画面」と判定。デフォルト0.95'},
  {k:'patrol_screen_timeout_secs',label:'画面判定: タイムアウト (秒)',type:'number',desc:'判定が完了しない場合の強制進行上限。デフォルト12'},
];
const CFG_FIELDS_GAME=[
  {k:'game_package_name',label:'ゲームパッケージ名',type:'text',desc:'クラッシュ検知・ADB起動用のパッケージ名。例: com.example.game'},
  {k:'game_launch_activity',label:'起動アクティビティ',type:'text',desc:'復帰時に使用。空のmonkeyモードで起動'},
  {k:'crash_recovery_enabled',label:'クラッシュ自動復帰',type:'bool',desc:'ゲームクラッシュ時にADBで自動再起動'},
  {k:'crash_recovery_delay_secs',label:'復帰待機時間 (秒)',type:'number',desc:'起動コマンド後、当該デバイスを反応待ちする秒数'},
];
const CFG_FIELDS_ADB=[
  {k:'serial_to_label',label:'シリアル→ラベル分配 (JSON)',type:'json',desc:'ADBシリアルとInstance-Nラベルの対応。巡回中に自動判定・保存される。手動設定も可。例: {"127.0.0.1:5555":"Instance-1"}'},
  {k:'exclude_uids',label:'除外 UID (カンマ区切り)',type:'csv-num',desc:'バインド候補から除外する UID。同 PC 上の本物クライアントの UID を登録すると誤バインドを防げる。例: 430314,12345'},
  {k:'parallel_limit',label:'並列切替台数',type:'number',desc:'0=全台同時（ディレイ無効）'},
  {k:'parallel_group_delay_secs',label:'グループ間ディレイ (秒)',type:'number',desc:'並列台数>0のとき有効'},
  {k:'adb_path',label:'ADBパス',type:'text',desc:'adb.exeのフルパスまたは「adb」'},
  {k:'mumu_delay_ms',label:'ADBコマンド間隔 (ms)',type:'number',desc:'各ADBコマンド間の待機時間'},
  {k:'mumu_tap_x',label:'タップX座標',type:'number',desc:'チャンネル入力欄のタップX'},
  {k:'mumu_tap_y',label:'タップY座標',type:'number',desc:'チャンネル入力欄のタップY'},
  {k:'mumu_pre_keycode',label:'プリキーコード',type:'text',desc:'チャンネル入力欄を開くキーコード'},
];
const CFG_FIELDS_MISC=[
  // gas_enable は巡回ページ(card-patrol-control)に移設。cfg-gas_enable 要素は静的 HTML で定義済み
  {k:'gas_target_enemy',label:'GAS 対象エネミー',type:'select',options:['金ウリボ','金ナッポ'],desc:'Chrome拡張から受信するエネミー種別'},
  {k:'debug_verbose',label:'詳細デバッグログ',type:'bool',desc:'true にすると [DBG][...] プレフィックスの詳細ログを出力。不具合調査用。本番運用では false を推奨'},
  {k:'show_no_device_dialog',label:'デバイス未検出ダイアログ',type:'bool',desc:'起動時にADBデバイスが見つからない場合ダイアログを表示する'},
];
const CFG_FIELDS=[...CFG_FIELDS_DISCORD,...CFG_FIELDS_PATROL,...CFG_FIELDS_GAME,...CFG_FIELDS_ADB,...CFG_FIELDS_MISC];
let cfgData={};
function renderConfigFields(containerId, fields){
	const root=document.getElementById(containerId);if(!root)return;
	root.innerHTML=fields.map(function(f){
		var val=cfgData[f.k]!==undefined?cfgData[f.k]:'';
		var noteHtml=f.desc?('<span class="cfg-note">'+escHtml(f.desc)+'</span>'):'';
		if(f.type==='csv'&&Array.isArray(val))val=val.join(',');
		if(f.type==='csv-num'&&Array.isArray(val))val=val.join(',');
		if(f.type==='multiline-list'&&Array.isArray(val))val=val.join('\n');
		if(f.type==='multiline-list'){
			return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label><textarea id="cfg-'+f.k+'" rows="10" spellcheck="false" placeholder="'+escHtml(f.desc||'')+'">'+escHtml(String(val))+'</textarea>'+noteHtml+'</div>';
		}
		if(f.type==='json'){
			var jsonStr=typeof val==='object'?JSON.stringify(val,null,2):String(val||'{}');
			return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label><textarea id="cfg-'+f.k+'" rows="5" spellcheck="false" style="font-family:monospace;font-size:12px" placeholder="'+escHtml(f.desc||'')+'">'+escHtml(jsonStr)+'</textarea>'+noteHtml+'</div>';
		}
		if(f.type==='bool'){
			return '<div class="cfg-field cfg-field-bool"><label class="cfg-bool-label"><input type="checkbox" id="cfg-'+f.k+'"'+(val?' checked':'')+'><span>'+escHtml(f.label)+'</span></label>'+noteHtml+'</div>';
		}
		if(f.type==='select'){
			const opts=(f.options||[]).map(o=>'<option value="'+escHtml(o)+'"'+(val===o?' selected':'')+'>'+escHtml(o)+'</option>').join('');
			return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label><select id="cfg-'+f.k+'">'+opts+'</select>'+noteHtml+'</div>';
		}
		if(f.type==='region-picker-btn'){
			return '<div class="cfg-field"><button type="button" class="btn btn-region-picker" onclick="openScreenshotPicker()">📐 監視矩形をドラッグで選択...</button></div>';
		}
		var inputType=(f.type==='csv'||f.type==='csv-num')?'text':f.type;
		var testBtnHtml=f.testBtn?('<div style="margin-top:4px"><button type="button" class="btn" onclick="testWebhook(\'cfg-'+f.k+'\')">📨 テスト送信</button><span id="cfg-'+f.k+'-test-result" style="margin-left:8px;font-size:var(--fs-sm);opacity:.8"></span></div>'):'';
		return '<div class="cfg-field"><label>'+escHtml(f.label)+'</label><input type="'+inputType+'" id="cfg-'+f.k+'" value="'+escHtml(String(val))+'" placeholder="'+escHtml(f.desc||'')+'">'+noteHtml+testBtnHtml+'</div>';
	}).join('');
}
function renderChatRuleTable(headers, rows, emptyText){
	if(!rows.length)return '<div class="cfg-rule-empty">'+escHtml(emptyText)+'</div>';
	return '<div class="cfg-rule-table-wrap"><table class="cfg-rule-table"><thead><tr>'
		+headers.map(h=>'<th>'+escHtml(h)+'</th>').join('')
		+'</tr></thead><tbody>'
		+rows.join('')
		+'</tbody></table></div>';
}
function applyChatFilterMinimizeState(){
	const card=document.querySelector('.chat-filter-card');
	const btn=document.getElementById('chat-filter-toggle-btn');
	if(card)card.classList.toggle('minimized',chatFilterMinimized);
	if(btn)btn.textContent=chatFilterMinimized?'＋ 展開':'－ 最小化';
}
function toggleChatFilterMinimize(){
	chatFilterMinimized=!chatFilterMinimized;
	localStorage.setItem(CHAT_FILTER_MINIMIZED_KEY,String(chatFilterMinimized));
	applyChatFilterMinimizeState();
}
function clampChatNotifyScore(value){
	const n=parseInt(value,10);
	if(!Number.isFinite(n))return CHAT_NOTIFY_SCORE_DEFAULT;
	return Math.max(CHAT_NOTIFY_SCORE_MIN,Math.min(CHAT_NOTIFY_SCORE_MAX,n));
}
function getChatNotifyMinScore(){
	chatNotifyMinScore=clampChatNotifyScore(chatNotifyMinScore);
	return chatNotifyMinScore;
}
function syncChatNotifyScoreInputs(){
	const score=getChatNotifyMinScore();
	const slider=document.getElementById('chat-notify-score-slider');
	const number=document.getElementById('chat-notify-score-number');
	const label=document.getElementById('chat-notify-score-value');
	if(slider && String(slider.value)!==String(score))slider.value=String(score);
	if(number && String(number.value)!==String(score))number.value=String(score);
	if(label)label.textContent='score '+score+' 以上で通知';
}
function setChatNotifyMinScore(value){
	const next=clampChatNotifyScore(value);
	if(next===chatNotifyMinScore){
		syncChatNotifyScoreInputs();
		return;
	}
	chatNotifyMinScore=next;
	localStorage.setItem(CHAT_NOTIFY_SCORE_KEY,String(chatNotifyMinScore));
	syncChatNotifyScoreInputs();
	renderChatCandidatePanels();
}
function onChatNotifyScoreSliderInput(value){
	setChatNotifyMinScore(value);
}
function onChatNotifyScoreNumberChange(value){
	setChatNotifyMinScore(value);
}
function getCustomLocationRuleLines(){
	return normalizeCsvList(cfgData.chat_report_location_rules);
}
function getCustomMonsterAliasRuleLines(){
	return normalizeCsvList(cfgData.chat_report_monster_alias_rules);
}
function getChatFilterLines(key){
	return normalizeCsvList(cfgData[key]);
}
function renderSimpleFilterRows(key){
	return getChatFilterLines(key).map(value=>'<tr>'
		+'<td>'+escHtml(value)+'</td>'
		+'<td><button type="button" class="btn danger" style="padding:2px 8px" onclick="removeChatFilterValue(\''+key+'\','+escAttrJs(value)+')">削除</button></td>'
		+'</tr>');
}
function renderCustomLocationRuleRows(usePopupHandler){
	return getCustomLocationRuleLines().map(line=>{
		const parsed=parseChatLocationRuleLine(line);
		if(!parsed)return '';
		const deleteCall=(usePopupHandler?'removeRule':'removeChatRuleLine')+'(\'chat_report_location_rules\','+escAttrJs(line)+')';
		return '<tr>'
			+'<td class="cell-mono">'+escHtml(parsed.name)+'</td>'
			+'<td>'+escHtml(parsed.aliases.filter(v=>v!==parsed.name).join(', '))+'</td>'
			+'<td>'+escHtml(parsed.monsters.join(', '))+'</td>'
			+'<td><button type="button" class="btn danger" style="padding:2px 8px" onclick="'+deleteCall+'">削除</button></td>'
			+'</tr>';
	}).filter(Boolean);
}
function renderCustomMonsterAliasRuleRows(usePopupHandler){
	return getCustomMonsterAliasRuleLines().map(line=>{
		const parsed=parseChatMonsterAliasRuleLine(line);
		if(!parsed)return '';
		const deleteCall=(usePopupHandler?'removeRule':'removeChatRuleLine')+'(\'chat_report_monster_alias_rules\','+escAttrJs(line)+')';
		return '<tr>'
			+'<td class="cell-mono">'+escHtml(parsed.name)+'</td>'
			+'<td>'+escHtml(parsed.aliases.join(', '))+'</td>'
			+'<td><button type="button" class="btn danger" style="padding:2px 8px" onclick="'+deleteCall+'">削除</button></td>'
			+'</tr>';
	}).filter(Boolean);
}
let chatRuleWindowRef=null;
function buildChatRuleWindowHtml(){
	const locationRows=renderCustomLocationRuleRows(true);
	const monsterRows=renderCustomMonsterAliasRuleRows(true);
	const locationOptions=getChatLocationRules().map(rule=>'<option value="'+escHtml(rule.name)+'">'+escHtml(rule.name)+'</option>').join('');
	const monsterOptions=getChatMonsterAliases().map(rule=>'<option value="'+escHtml(rule.name)+'">'+escHtml(rule.name)+'</option>').join('');
	return '<!doctype html><html><head><meta charset="utf-8"><title>チャット候補ルール一覧</title><style>'
		+'body{margin:0;background:#0b0e15;color:#e6edf6;font-family:Segoe UI,sans-serif;padding:16px}'
		+'.cfg-rule-window-section{margin-bottom:18px}'
		+'.cfg-rule-window-title{font-size:14px;font-weight:600;color:#e6edf6;margin-bottom:10px}'
		+'.cfg-rule-window-note{font-size:12px;color:#98a3b8;margin-bottom:10px}'
		+'.cfg-rule-actions{display:flex;gap:6px;align-items:center;flex-wrap:wrap;margin-bottom:10px}'
		+'.cfg-rule-actions select,.cfg-rule-actions input{background:#121826;color:#e6edf6;border:1px solid #2b3448;border-radius:8px;padding:7px 10px;font-size:12px}'
		+'.cfg-rule-actions select{min-width:180px;flex:1}.cfg-rule-actions input{flex:1;min-width:160px}'
		+'.btn{background:#1a2233;border:1px solid #2b3448;color:#e6edf6;border-radius:8px;padding:7px 10px;font-size:12px;cursor:pointer}'
		+'.btn:hover{background:#202b42}.btn.danger{color:#ff8a8a}'
		+'.cfg-rule-table-wrap{border:1px solid #2b3448;border-radius:10px;overflow:auto;background:#121826;max-height:none}'
		+'.cfg-rule-table{width:100%;border-collapse:collapse;font-size:12px;table-layout:fixed}'
		+'.cfg-rule-table th,.cfg-rule-table td{border-bottom:1px solid #2b3448;border-right:1px solid #2b3448;padding:8px 10px;vertical-align:top;line-height:1.5;word-break:break-word}'
		+'.cfg-rule-table th:last-child,.cfg-rule-table td:last-child{border-right:none}'
		+'.cfg-rule-table th{background:#1a2233;color:#98a3b8;font-weight:600;position:sticky;top:0}'
		+'.cfg-rule-table tr:last-child td{border-bottom:none}'
		+'.cell-mono{font-family:Consolas,monospace;color:#6ea8ff}'
		+'.cfg-rule-empty{padding:12px;color:#98a3b8;font-size:12px}'
		+'</style></head><body>'
		+'<div class="cfg-rule-window-section"><div class="cfg-rule-window-title">場所別名ルール一覧</div><div class="cfg-rule-window-note">このウィンドウから追加・削除できます。</div>'
		+'<div class="cfg-rule-actions"><select id="popup-location-rule-target"><option value="">場所を選択</option>'+locationOptions+'</select><input type="text" id="popup-location-rule-alias" placeholder="別名を入力。例: tnt"><button type="button" class="btn" onclick="addLocationRule()">追加</button></div>'
		+renderChatRuleTable(['場所','追加した別名','出現候補',''],locationRows,'追加した場所別名はありません')+'</div>'
		+'<div class="cfg-rule-window-section"><div class="cfg-rule-window-title">モンスター別名ルール一覧</div>'
		+'<div class="cfg-rule-actions"><select id="popup-monster-rule-target"><option value="">モンスターを選択</option>'+monsterOptions+'</select><input type="text" id="popup-monster-rule-alias" placeholder="別名を入力。例: 金ウリ"><button type="button" class="btn" onclick="addMonsterRule()">追加</button></div>'
		+renderChatRuleTable(['モンスター','追加した別名',''],monsterRows,'追加したモンスター別名はありません')+'</div>'
		+'<script>'
		+'function addLocationRule(){var target=document.getElementById("popup-location-rule-target");var input=document.getElementById("popup-location-rule-alias");if(!window.opener||!target||!input)return;window.opener.addChatLocationRuleValue(String(target.value||""),String(input.value||""));}'
		+'function addMonsterRule(){var target=document.getElementById("popup-monster-rule-target");var input=document.getElementById("popup-monster-rule-alias");if(!window.opener||!target||!input)return;window.opener.addChatMonsterAliasRuleValue(String(target.value||""),String(input.value||""));}'
		+'function removeRule(key,line){if(window.opener)window.opener.removeChatRuleLine(key,line);}'
		+'<\/script>'
		+'</body></html>';
}
function refreshChatRuleWindow(){
	if(!chatRuleWindowRef || chatRuleWindowRef.closed)return;
	const doc=chatRuleWindowRef.document;
	doc.open();
	doc.write(buildChatRuleWindowHtml());
	doc.close();
}
function openChatRuleWindow(){
	chatRuleWindowRef=window.open('','chat-rule-window','width=1200,height=780');
	if(!chatRuleWindowRef)return;
	refreshChatRuleWindow();
	chatRuleWindowRef.focus();
}
function renderChatRuleManagers(){
	const root=document.getElementById('cfg-chat-rule-managers');if(!root)return;
	const scroller=document.getElementById('view-settings');
	const savedScroll=scroller?scroller.scrollTop:0;
	if(document.activeElement && root.contains(document.activeElement))document.activeElement.blur();
	const locationOptions=getChatLocationRules().map(rule=>'<option value="'+escHtml(rule.name)+'">'+escHtml(rule.name)+'</option>').join('');
	const monsterOptions=getChatMonsterAliases().map(rule=>'<option value="'+escHtml(rule.name)+'">'+escHtml(rule.name)+'</option>').join('');
	const excludeRows=renderSimpleFilterRows('chat_report_excluded_senders');
	const senderExcludeRules=normalizeCsvList(cfgData.chat_report_excluded_senders).join('\n');
	const locationRules=normalizeCsvList(cfgData.chat_report_location_rules).join('\n');
	const monsterRules=normalizeCsvList(cfgData.chat_report_monster_alias_rules).join('\n');
	const notifyScore=getChatNotifyMinScore();
	const locationRows=renderCustomLocationRuleRows(false);
	const monsterRows=renderCustomMonsterAliasRuleRows(false);
	root.innerHTML=''
		+'<div class="chat-score-threshold-box">'
		+'<div class="chat-score-threshold-title">通知しきい値</div>'
		+'<div class="chat-score-threshold-controls">'
		+'<input type="range" id="chat-notify-score-slider" min="'+CHAT_NOTIFY_SCORE_MIN+'" max="'+CHAT_NOTIFY_SCORE_MAX+'" step="1" value="'+notifyScore+'" oninput="onChatNotifyScoreSliderInput(this.value)">'
		+'<input type="number" id="chat-notify-score-number" min="'+CHAT_NOTIFY_SCORE_MIN+'" max="'+CHAT_NOTIFY_SCORE_MAX+'" step="1" value="'+notifyScore+'" onchange="onChatNotifyScoreNumberChange(this.value)">'
		+'<span id="chat-notify-score-value" class="chat-score-threshold-value">score '+notifyScore+' 以上で通知</span>'
		+'</div>'
		+'</div>'
		+'<div class="cfg-rule-box">'
		+'<div class="cfg-rule-box-title" style="color:var(--danger)">検知不要プレイヤー</div>'
		+'<div class="cfg-rule-actions">'
		+'<input type="text" id="cfg-sender-exclude-input" placeholder="発言者名を入力">'
		+'<button type="button" class="btn danger" onclick="addChatFilterManagerValue(\'chat_report_excluded_senders\',\'cfg-sender-exclude-input\')">+ 除外に追加</button>'
		+'</div>'
		+renderChatRuleTable(['発言者',''],excludeRows,'除外発言者なし')
		+'<textarea class="cfg-hidden-field" id="cfg-chat_report_excluded_senders">'+escHtml(senderExcludeRules)+'</textarea>'
		+'</div>'
		+'<div class="cfg-rule-box">'
		+'<div class="cfg-rule-header"><div class="cfg-rule-box-title">場所別名を追加</div><button type="button" class="btn" style="margin-left:auto" onclick="openChatRuleWindow()">⧉ 一覧を別ウィンドウ</button></div>'
		+'<div class="cfg-rule-actions">'
		+'<select id="cfg-location-rule-target"><option value="">場所を選択</option>'+locationOptions+'</select>'
		+'<input type="text" id="cfg-location-rule-alias" placeholder="別名を入力。例: tnt">'
		+'<button type="button" class="btn" onclick="addChatLocationRule()">追加</button>'
		+'</div>'
		+renderChatRuleTable(['場所','追加した別名','出現候補',''],locationRows,'追加した場所別名はありません')
		+'<textarea class="cfg-hidden-field" id="cfg-chat_report_location_rules" rows="10" spellcheck="false" placeholder="地点名|別名|モンスター1,モンスター2">'+escHtml(locationRules)+'</textarea>'
		+'<span class="cfg-note">追加内容はセル形式で表示しています。保存はこの一覧から行われます。</span>'
		+'</div>'
		+'<div class="cfg-rule-box">'
		+'<div class="cfg-rule-box-title">モンスター別名を追加</div>'
		+'<div class="cfg-rule-actions">'
		+'<select id="cfg-monster-rule-target"><option value="">モンスターを選択</option>'+monsterOptions+'</select>'
		+'<input type="text" id="cfg-monster-rule-alias" placeholder="別名を入力。例: 金ウリ"><button type="button" class="btn" onclick="addChatMonsterAliasRule()">追加</button>'
		+'</div>'
		+renderChatRuleTable(['モンスター','追加した別名',''],monsterRows,'追加したモンスター別名はありません')
		+'<textarea class="cfg-hidden-field" id="cfg-chat_report_monster_alias_rules" rows="10" spellcheck="false" placeholder="モンスター名|別名">'+escHtml(monsterRules)+'</textarea>'
		+'<span class="cfg-note">追加内容はセル形式で表示しています。保存はこの一覧から行われます。</span>'
		+'</div>';
	syncChatNotifyScoreInputs();
	if(scroller)requestAnimationFrame(()=>{ scroller.scrollTop=savedScroll; });
}
async function removeChatFilterValue(key, value){
	const current=Array.isArray(cfgData[key])?cfgData[key]:[];
	updateChatFilterTextarea(key, current.filter(v=>v!==value));
	renderChatRuleManagers();
	refreshChatRuleWindow();
	await saveConfig(true);
}
async function addChatFilterManagerValue(key, inputId){
	const input=document.getElementById(inputId);
	if(!input)return;
	const value=String(input.value||'').trim();
	if(!value)return;
	await appendChatFilterValue(key, value);
	input.value='';
	renderChatRuleManagers();
	refreshChatRuleWindow();
}
async function removeChatRuleLine(key, line){
	const current=Array.isArray(cfgData[key])?cfgData[key]:[];
	updateChatFilterTextarea(key, current.filter(v=>v!==line));
	renderChatRuleManagers();
	refreshChatRuleWindow();
	await saveConfig(true);
}
async function addChatLocationRuleValue(nameValue, aliasValue){
	const name=String(nameValue||'').trim();
	const alias=String(aliasValue||'').trim();
	if(!name||!alias)return;
	const current=Array.isArray(cfgData.chat_report_location_rules)?cfgData.chat_report_location_rules:[];
	const idx=current.findIndex(line=>{const p=parseChatLocationRuleLine(line);return p&&p.name===name;});
	if(idx>=0){
		const parsed=parseChatLocationRuleLine(current[idx]);
		const newAliases=normalizeCsvList(parsed.aliases.concat([alias]));
		const newLine=[name,newAliases.join(','),parsed.monsters.join(',')].join('|');
		const updated=current.slice();
		updated[idx]=newLine;
		updateChatFilterTextarea('chat_report_location_rules',updated);
		renderChatCandidatePanels();
		await saveConfig(true);
	}else{
		const rule=getChatLocationRules().find(v=>v.name===name);
		const monsters=rule&&Array.isArray(rule.monsters)?rule.monsters.join(','):'';
		await appendChatFilterValue('chat_report_location_rules',[name,alias,monsters].join('|'));
	}
	const input=document.getElementById('cfg-location-rule-alias');
	if(input)input.value='';
	renderChatRuleManagers();
	refreshChatRuleWindow();
}
async function addChatLocationRule(){
	const target=document.getElementById('cfg-location-rule-target');
	const input=document.getElementById('cfg-location-rule-alias');
	if(!target||!input)return;
	await addChatLocationRuleValue(target.value,input.value);
	input.value='';
}
async function addChatMonsterAliasRuleValue(nameValue, aliasValue){
	const name=String(nameValue||'').trim();
	const alias=String(aliasValue||'').trim();
	if(!name||!alias)return;
	await appendChatFilterValue('chat_report_monster_alias_rules',[name,alias].join('|'));
	const input=document.getElementById('cfg-monster-rule-alias');
	if(input)input.value='';
	renderChatRuleManagers();
	refreshChatRuleWindow();
}
async function addChatMonsterAliasRule(){
	const target=document.getElementById('cfg-monster-rule-target');
	const input=document.getElementById('cfg-monster-rule-alias');
	if(!target||!input)return;
	await addChatMonsterAliasRuleValue(target.value,input.value);
	input.value='';
}
async function loadConfig(){
  cfgData=await fetch('/api/config').then(r=>r.json());
	renderConfigFields('cfg-chat-form',CHAT_FILTER_FIELDS);
	renderChatRuleManagers();
	renderConfigFields('cfg-form-discord',CFG_FIELDS_DISCORD);
	renderConfigFields('cfg-form-patrol',CFG_FIELDS_PATROL);
	renderConfigFields('cfg-form-game',CFG_FIELDS_GAME);
	renderConfigFields('cfg-form-adb',CFG_FIELDS_ADB);
	renderConfigFields('cfg-form-misc',CFG_FIELDS_MISC);
	// gas_enable は巡回ページの静的要素を手動セット
	const gasEl=document.getElementById('cfg-gas_enable');
	if(gasEl){
		gasEl.checked=cfgData.gas_enable===true;
		gasEl.onchange=()=>saveConfig(true);
	}
	renderChatCandidatePanels();
	renderConfigForm(cfgData);
	applyChatFilterMinimizeState();
	// タップループ入力値を config から復元（ループ実行中はステータスポールが上書き）
	{const xi=document.getElementById('tap-x'),yi=document.getElementById('tap-y'),ii=document.getElementById('tap-interval');
	if(xi&&cfgData.tap_loop_x!==undefined)xi.value=cfgData.tap_loop_x;
	if(yi&&cfgData.tap_loop_y!==undefined)yi.value=cfgData.tap_loop_y;
	if(ii&&cfgData.tap_loop_interval_ms)ii.value=cfgData.tap_loop_interval_ms;}
}
async function saveConfig(silent){
	const updated={...cfgData};
		[...CHAT_FILTER_FIELDS,...CHAT_RULE_FIELDS,...CFG_FIELDS].forEach(f=>{const el=document.getElementById('cfg-'+f.k);if(!el)return;
		if(f.type==='number')updated[f.k]=parseFloat(el.value)||0;
		else if(f.type==='multiline-list')updated[f.k]=el.value.split(/\r?\n/).map(s=>s.trim()).filter(Boolean);
		else if(f.type==='csv')updated[f.k]=el.value.split(',').map(s=>s.trim()).filter(Boolean);
		else if(f.type==='csv-num')updated[f.k]=el.value.split(',').map(s=>parseInt(s.trim(),10)).filter(n=>!isNaN(n));
		else if(f.type==='bool')updated[f.k]=el.checked;
		else if(f.type==='json'){try{updated[f.k]=JSON.parse(el.value);}catch(e){updated[f.k]={};}}
		else updated[f.k]=el.value;});
	// gas_enable は巡回ページの静的要素から手動収集
	{const gasEl=document.getElementById('cfg-gas_enable');if(gasEl)updated.gas_enable=gasEl.checked;}
	// 通知エネミー（{name, enabled} 形式で全件保存）
	updated.notify_enemies=[];
	document.querySelectorAll('.cfg-enemy').forEach(chk=>{updated.notify_enemies.push({name:chk.value,enabled:chk.checked});});
	const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(updated)});
	const d=await r.json();const st=document.getElementById('cfg-status');
		if(st && !silent){
				st.textContent=d.ok?'✓ 保存・反映済':'✗ 失敗: '+(d.error||'');
				setTimeout(()=>st.textContent='',4000);
		}
	cfgData=updated;renderChatRuleManagers();renderChatCandidatePanels();
}
async function testWebhook(inputId){
	const url=(document.getElementById(inputId)||{}).value||'';
	const resultEl=document.getElementById(inputId+'-test-result');
	if(!url.trim()){if(resultEl)resultEl.textContent='URL未入力';return;}
	if(resultEl)resultEl.textContent='送信中...';
	try{
		const r=await fetch('/api/webhook/test',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({url:url.trim()})});
		const d=await r.json();
		if(resultEl)resultEl.textContent=d.ok?'✓ 送信成功':'✗ 失敗: '+(d.error||'');
	}catch(e){if(resultEl)resultEl.textContent='✗ エラー: '+e.message;}
	setTimeout(()=>{if(resultEl)resultEl.textContent='';},5000);
}
function renderConfigForm(cfg){
	// 通知エネミー チェックボックス反映（{name,enabled} 形式）
	const notifyEnemies=cfg.notify_enemies||[
		{name:"ウリボ・ゴールド",enabled:true},
		{name:"金ナッポ",enabled:true},
		{name:"銀ナッポ",enabled:true}
	];
	document.querySelectorAll('.cfg-enemy').forEach(chk=>{
		const entry=notifyEnemies.find(e=>e.name===chk.value);
		chk.checked=entry?entry.enabled:true;
	});
}
// ── Uptime counter (巡回中だけ加算・累積保持) ──
localStorage.removeItem('uptimeStart'); // 旧キーをクリーンアップ
let _uptimeAccumulated=parseInt(localStorage.getItem('uptimeAccumulated')||'0')||0;
let _uptimePatrolling=false;
let _uptimeLastTickAt=Date.now();
function resetUptime(){_uptimeAccumulated=0;localStorage.setItem('uptimeAccumulated','0');}
function _fmtUptime(s){const h=Math.floor(s/3600),m=Math.floor((s%3600)/60),ss=s%60;return (h<10?'0':'')+h+':'+(m<10?'0':'')+m+':'+(ss<10?'0':'')+ss;}
setInterval(()=>{
	const now=Date.now();
	if(_uptimePatrolling){
		_uptimeAccumulated+=Math.floor((now-_uptimeLastTickAt)/1000);
		localStorage.setItem('uptimeAccumulated',String(_uptimeAccumulated));
	}
	_uptimeLastTickAt=now;
	const txt=_fmtUptime(_uptimeAccumulated);
	const el=document.getElementById('dash-uptime');if(el)el.textContent=txt;
	const sm=document.getElementById('brand-uptime-small');if(sm)sm.textContent=txt;
},1000);
// ── Dashboard layout ──
const DASH_PANEL_IDS=['card-dash-devices','card-dash-patrol','card-dash-gold','card-dash-chat','card-dash-report'];
const DASH_SIZE_CLASSES=['panel-size-1x1','panel-size-1x2','panel-size-2x1','panel-size-2x2','panel-size-2x3','panel-size-2x4'];
const PATROL_LAYOUT_CARD_IDS=['card-patrol-control','card-patrol-channels','card-patrol-device-status','card-patrol-gold'];
// 初期の列配置は index.html の panel-col-1/panel-col-2 静的クラスで決まる
// （保存済みレイアウトがあれば loadPatrolLayout が上書き）
const DASH_GRID_ROW_UNIT=8;
const DASH_GRID_GAP=10;
const DASH_MIN_PANEL_ROWS=18;
const DASH_MAX_PANEL_ROWS=220;
let layoutEditMode=false;
function getDefaultPanelRows(size){
	switch(size){
		case '1x2':
		case '2x2': return 52;
		case '2x3': return 76;
		case '2x4': return 100;
		default: return 28;
	}
}
function getPanelRows(card){
	if(!card)return DASH_MIN_PANEL_ROWS;
	const inlineRows=parseInt(card.style.getPropertyValue('--panel-rows'),10);
	if(inlineRows>0)return inlineRows;
	const computedRows=parseInt(getComputedStyle(card).getPropertyValue('--panel-rows'),10);
	if(computedRows>0)return computedRows;
	const sc=DASH_SIZE_CLASSES.find(c=>card.classList.contains(c));
	return getDefaultPanelRows(sc?sc.replace('panel-size-',''):'1x1');
}
function getPanelWidthUnits(card){
	if(!card)return 1;
	return DASH_SIZE_CLASSES.some(c=>c.indexOf('panel-size-2x')===0&&card.classList.contains(c))?2:1;
}
function clampPanelRows(rows){
	return Math.max(DASH_MIN_PANEL_ROWS,Math.min(DASH_MAX_PANEL_ROWS,rows));
}
function pixelsToPanelRows(heightPx){
	return clampPanelRows(Math.round((heightPx + DASH_GRID_GAP) / (DASH_GRID_ROW_UNIT + DASH_GRID_GAP)));
}
function panelRowsToPixels(rows){
	const safeRows=clampPanelRows(rows);
	return safeRows * DASH_GRID_ROW_UNIT + Math.max(0,safeRows - 1) * DASH_GRID_GAP;
}
function setPanelRows(card,rows,save){
	if(!card)return;
	card.style.setProperty('--panel-rows',String(clampPanelRows(rows)));
	if(save)saveDashboardLayout();
}
function getPanelColumn(card){
	if(!card)return 1;
	if(card.classList.contains('panel-col-2'))return 2;
	return 1;
}
function setPanelColumn(card,column){
	if(!card)return;
	card.classList.remove('panel-col-1','panel-col-2');
	if(column===2)card.classList.add('panel-col-2');
	else card.classList.add('panel-col-1');
}
function setPanelWidthUnits(card,width,save,column){
	if(!card)return;
	const currentRows=getPanelRows(card);
	DASH_SIZE_CLASSES.forEach(c=>card.classList.remove(c));
	if(width===2){
		card.classList.remove('panel-col-1','panel-col-2');
		card.classList.add('panel-size-2x1');
	}else{
		card.classList.add('panel-size-1x1');
		setPanelColumn(card,column===2?2:1);
	}
	setPanelRows(card,currentRows,false);
	updateDashboardResizeHandlePositions();
	if(save)saveDashboardLayout();
}
function updateDashboardResizeHandlePositions(){
	const grid=document.getElementById('dashboard-grid');if(!grid)return;
	const gridRect=grid.getBoundingClientRect();
	const gridMidX=gridRect.left + gridRect.width/2;
	grid.querySelectorAll(':scope > .card').forEach(card=>{
		const leftHandle=card.querySelector(':scope > .panel-resize-handle-x.handle-left');
		const rightHandle=card.querySelector(':scope > .panel-resize-handle-x.handle-right');
		if(!leftHandle||!rightHandle)return;
		const rect=card.getBoundingClientRect();
		const isWide=getPanelWidthUnits(card)===2 || rect.width >= gridRect.width*0.8;
		const column=isWide?(rect.left < gridMidX?1:2):getPanelColumn(card);
		leftHandle.classList.toggle('is-hidden',!isWide && column===1);
		rightHandle.classList.toggle('is-hidden',!isWide && column===2);
	});
}
function setPatrolPanelWidthUnits(card,width,save,column){
	if(!card)return;
	const currentRows=getPanelRows(card);
	DASH_SIZE_CLASSES.forEach(c=>card.classList.remove(c));
	if(width===2){
		card.classList.remove('panel-col-1','panel-col-2');
		card.classList.add('panel-size-2x1');
	}else{
		card.classList.add('panel-size-1x1');
		setPanelColumn(card,column===2?2:1);
	}
	setPanelRows(card,currentRows,false);
	updatePatrolResizeHandlePositions();
	if(save)savePatrolLayout();
}
function updatePatrolResizeHandlePositions(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	const gridRect=grid.getBoundingClientRect();
	const gridMidX=gridRect.left + gridRect.width/2;
	grid.querySelectorAll(':scope > .card').forEach(card=>{
		const leftHandle=card.querySelector(':scope > .panel-resize-handle-x.handle-left');
		const rightHandle=card.querySelector(':scope > .panel-resize-handle-x.handle-right');
		if(!leftHandle||!rightHandle)return;
		const rect=card.getBoundingClientRect();
		const isWide=getPanelWidthUnits(card)===2 || rect.width >= gridRect.width*0.8;
		const column=isWide?(rect.left < gridMidX?1:2):getPanelColumn(card);
		leftHandle.classList.toggle('is-hidden',!isWide && column===1);
		rightHandle.classList.toggle('is-hidden',!isWide && column===2);
	});
}
function applyPanelSizeInternal(panelId,size,save){
  const card=document.getElementById(panelId);if(!card)return;
  DASH_SIZE_CLASSES.forEach(c=>card.classList.remove(c));
  card.classList.add('panel-size-'+size);
	setPanelRows(card,getDefaultPanelRows(size),false);
  if(save)saveDashboardLayout();
}
function applyPanelSize(panelId,size){applyPanelSizeInternal(panelId,size,true);}
function initDashboardResizeHandles(){
	const grid=document.getElementById('dashboard-grid');if(!grid)return;
	let activeResize=null;
	function stopResize(){
		if(!activeResize)return;
		activeResize.card.classList.remove('resizing-x','resizing-y');
		document.body.style.userSelect='';
		updateDashboardResizeHandlePositions();
		saveDashboardLayout();
		activeResize=null;
	}
	function onPointerMove(e){
		if(!activeResize)return;
		if(activeResize.type==='height'){
			const deltaY=e.clientY-activeResize.startY;
			const targetHeight=Math.max(120,activeResize.startHeight+deltaY);
			setPanelRows(activeResize.card,pixelsToPanelRows(targetHeight),false);
			return;
		}
		const deltaX=e.clientX-activeResize.startX;
		const startUnits=activeResize.startUnits;
		if(activeResize.side==='right'){
			if(startUnits===1)setPatrolPanelWidthUnits(activeResize.card,deltaX>activeResize.threshold?2:1,false,1);
			else setPatrolPanelWidthUnits(activeResize.card,deltaX<-activeResize.threshold?1:2,false,1);
			return;
		}
		if(startUnits===1)setPatrolPanelWidthUnits(activeResize.card,deltaX<-activeResize.threshold?2:1,false,2);
		else setPatrolPanelWidthUnits(activeResize.card,deltaX>activeResize.threshold?1:2,false,2);
	}
	function onPointerUp(){
		stopResize();
	}
	document.addEventListener('pointermove',onPointerMove);
	document.addEventListener('pointerup',onPointerUp);
	grid.querySelectorAll(':scope > .card').forEach(card=>{
		if(card.querySelector(':scope > .panel-resize-handle-y'))return;
		const heightHandle=document.createElement('div');
		heightHandle.className='panel-resize-handle-y';
		heightHandle.title='上下にドラッグして高さ変更';
		heightHandle.addEventListener('pointerdown',e=>{
			if(!layoutEditMode)return;
			e.preventDefault();
			e.stopPropagation();
			activeResize={type:'height',card,startY:e.clientY,startHeight:panelRowsToPixels(getPanelRows(card))};
			card.classList.add('resizing-y');
			document.body.style.userSelect='none';
		});
		function createWidthHandle(side){
			const handle=document.createElement('div');
			handle.className='panel-resize-handle-x handle-'+side;
			handle.title='左右にドラッグして横幅変更';
			handle.addEventListener('pointerdown',e=>{
				if(!layoutEditMode)return;
				e.preventDefault();
				e.stopPropagation();
				const gridRect=grid.getBoundingClientRect();
				const columnWidth=(gridRect.width-DASH_GRID_GAP)/2;
				activeResize={type:'width',side:side,card,startX:e.clientX,startUnits:getPanelWidthUnits(card),threshold:Math.max(24,columnWidth*0.22)};
				card.classList.add('resizing-x');
				document.body.style.userSelect='none';
			});
			return handle;
		}
		const leftWidthHandle=createWidthHandle('left');
		const rightWidthHandle=createWidthHandle('right');
		card.appendChild(heightHandle);
		card.appendChild(leftWidthHandle);
		card.appendChild(rightWidthHandle);
	});
	updateDashboardResizeHandlePositions();
	window.addEventListener('resize',updateDashboardResizeHandlePositions);
}
function initPatrolResizeHandles(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	let activeResize=null;
	function stopResize(){
		if(!activeResize)return;
		activeResize.card.classList.remove('resizing-x','resizing-y');
		document.body.style.userSelect='';
		updatePatrolResizeHandlePositions();
		savePatrolLayout();
		activeResize=null;
	}
	function onPointerMove(e){
		if(!activeResize)return;
		if(activeResize.type==='height'){
			const deltaY=e.clientY-activeResize.startY;
			const targetHeight=Math.max(120,activeResize.startHeight+deltaY);
			setPanelRows(activeResize.card,pixelsToPanelRows(targetHeight),false);
			return;
		}
		const deltaX=e.clientX-activeResize.startX;
		const startUnits=activeResize.startUnits;
		if(activeResize.side==='right'){
			if(startUnits===1)setPatrolPanelWidthUnits(activeResize.card,deltaX>activeResize.threshold?2:1,false,1);
			else setPatrolPanelWidthUnits(activeResize.card,deltaX<-activeResize.threshold?1:2,false,1);
			return;
		}
		if(startUnits===1)setPatrolPanelWidthUnits(activeResize.card,deltaX<-activeResize.threshold?2:1,false,2);
		else setPatrolPanelWidthUnits(activeResize.card,deltaX>activeResize.threshold?1:2,false,2);
	}
	function onPointerUp(){
		stopResize();
	}
	document.addEventListener('pointermove',onPointerMove);
	document.addEventListener('pointerup',onPointerUp);
	grid.querySelectorAll(':scope > .card').forEach(card=>{
		if(card.querySelector(':scope > .panel-resize-handle-y'))return;
		const heightHandle=document.createElement('div');
		heightHandle.className='panel-resize-handle-y';
		heightHandle.title='上下にドラッグして高さ変更';
		heightHandle.addEventListener('pointerdown',e=>{
			if(!layoutEditMode||currentViewId!=='patrol')return;
			e.preventDefault();
			e.stopPropagation();
			activeResize={type:'height',card,startY:e.clientY,startHeight:panelRowsToPixels(getPanelRows(card))};
			card.classList.add('resizing-y');
			document.body.style.userSelect='none';
		});
		function createWidthHandle(side){
			const handle=document.createElement('div');
			handle.className='panel-resize-handle-x handle-'+side;
			handle.title='左右にドラッグして横幅変更';
			handle.addEventListener('pointerdown',e=>{
				if(!layoutEditMode||currentViewId!=='patrol')return;
				e.preventDefault();
				e.stopPropagation();
				const gridRect=grid.getBoundingClientRect();
				const columnWidth=(gridRect.width-DASH_GRID_GAP)/2;
				activeResize={type:'width',side:side,card,startX:e.clientX,startUnits:getPanelWidthUnits(card),threshold:Math.max(24,columnWidth*0.22)};
				card.classList.add('resizing-x');
				document.body.style.userSelect='none';
			});
			return handle;
		}
		card.appendChild(heightHandle);
		card.appendChild(createWidthHandle('left'));
		card.appendChild(createWidthHandle('right'));
	});
	updatePatrolResizeHandlePositions();
	window.addEventListener('resize',updatePatrolResizeHandlePositions);
}
function saveChatLayout(){
	const container=document.getElementById('chat-split-container');
	const logCard=document.getElementById('card-chat-log');
	const data={};
	if(container&&logCard){data.reportOnTop=container.firstElementChild!==logCard;}
	localStorage.setItem('chatLayout',JSON.stringify(data));
}
function loadChatLayout(){
	try{
		const saved=JSON.parse(localStorage.getItem('chatLayout')||'{}');
		if(saved.reportOnTop)swapChatPanels();
	}catch(e){}
}
function swapChatPanels(){
	const container=document.getElementById('chat-split-container');
	const logCard=document.getElementById('card-chat-log');
	const reportCard=document.getElementById('card-chat-report-col');
	const handle=document.getElementById('chat-swap-handle');
	if(!container||!logCard||!reportCard||!handle)return;
	if(container.firstElementChild===logCard){
		container.appendChild(handle);
		container.appendChild(logCard);
	} else {
		container.appendChild(handle);
		container.appendChild(reportCard);
	}
	saveChatLayout();
}
function initChatLayoutEdit(){}
function swapDashColumns(){
  const gold=document.getElementById('card-dash-gold');
  const chatCol=document.getElementById('card-dash-chat-col');
  if(!gold||!chatCol)return;
  const goldIsLeft=!!(gold.compareDocumentPosition(chatCol)&Node.DOCUMENT_POSITION_FOLLOWING);
  if(goldIsLeft){
    chatCol.parentNode.appendChild(gold);
  } else {
    chatCol.parentNode.appendChild(chatCol);
  }
  saveDashboardLayout();
}
function saveDashboardLayout(){
  const gold=document.getElementById('card-dash-gold');
  const chatCol=document.getElementById('card-dash-chat-col');
  const goldOnLeft=!!(gold&&chatCol&&gold.compareDocumentPosition(chatCol)&Node.DOCUMENT_POSITION_FOLLOWING);
  localStorage.setItem('dashLayoutV2',JSON.stringify({goldOnLeft}));
}
function loadDashboardLayout(){
  try{
    const saved=JSON.parse(localStorage.getItem('dashLayoutV2')||'{}');
    if(saved.goldOnLeft===false)swapDashColumns();
  }catch(e){}
}
function savePatrolLayout(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	const order=[...grid.querySelectorAll(':scope > .card')].map(c=>c.id);
	const sizes={};
	const heights={};
	const columns={};
	PATROL_LAYOUT_CARD_IDS.forEach(id=>{
		const card=document.getElementById(id);if(!card)return;
		const sc=DASH_SIZE_CLASSES.find(c=>card.classList.contains(c));
		sizes[id]=sc?sc.replace('panel-size-',''):'1x1';
		heights[id]=getPanelRows(card);
		columns[id]=getPanelColumn(card);
	});
	localStorage.setItem('patrolLayout',JSON.stringify({order,sizes,heights,columns}));
}
function loadPatrolLayout(){
	const grid=document.getElementById('patrol-layout-root');if(!grid)return;
	try{
		const saved=JSON.parse(localStorage.getItem('patrolLayout')||'{}');
		if(saved.sizes){Object.entries(saved.sizes).forEach(([id,size])=>{const card=document.getElementById(id);if(!card)return;DASH_SIZE_CLASSES.forEach(c=>card.classList.remove(c));card.classList.add('panel-size-'+size);});}
		if(saved.columns){Object.entries(saved.columns).forEach(([id,column])=>{const card=document.getElementById(id);if(card&&getPanelWidthUnits(card)===1)setPanelColumn(card,parseInt(column,10)===2?2:1);});}
		if(saved.heights){Object.entries(saved.heights).forEach(([id,rows])=>{const card=document.getElementById(id);if(card)setPanelRows(card,parseInt(rows,10)||getPanelRows(card),false);});}
		if(saved.order&&saved.order.length){
			saved.order.forEach(id=>{const el=document.getElementById(id);if(el&&el.parentNode===grid)grid.appendChild(el);});
		}
		updatePatrolResizeHandlePositions();
	}catch(e){}
}
function toggleLayoutEdit(){
	if(!isLayoutEditableView(currentViewId))return;
  layoutEditMode=!layoutEditMode;
	syncLayoutEditState();
}
// ── Init ──
buildFilterBar();
applyReversedUI();
applyLoopUI();
loadPatrolChannels();
pollPatrolStatus();
loadConfig();
loadGoldHistory();
setInterval(loadGoldHistory,30000);
initChat();
initPanelDragAndCollapse();
initGridDragDrop();
initPatrolGridDragDrop();
initDashboardResizeHandles();
initPatrolResizeHandles();
loadDashboardLayout();
loadPatrolLayout();
initChatLayoutEdit();
loadChatLayout();
syncLayoutEditState();
// 金履歴フィルタバーの初期 active 状態を反映
['gold-filter-bar-dash','gold-filter-bar-patrol'].forEach(id=>{
  const bar=document.getElementById(id);
  if(bar) bar.querySelectorAll('.btn-gf').forEach(b=>b.classList.toggle('active',b.dataset.filter===_goldFilter));
});
(async function startupDeviceCheck(){
  async function fetchDevicesOnly(){
    const r=await fetch('/api/devices');const res=await r.json();
    const devs=Array.isArray(res)?res:(res.devices||[]);
    const mapRes=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
    const deviceMap={};if(mapRes.devices)mapRes.devices.forEach(e=>{if(e.serial)deviceMap[e.serial]=e;if(e.device_ip&&e.serial)chatIPToSerial[e.device_ip]=e.serial;});
    if(devs&&devs.length>0){chatKnownSerials=devs;refreshChatDeviceDropdown();}
    renderDashDevices(devs,deviceMap);
    currentDevices=devs||[];currentDeviceMap=deviceMap;
    renderDeviceList();
    return devs&&devs.length>0;
  }
  for(let i=1;i<=3;i++){const found=await fetchDevicesOnly();if(found)return;if(i<3)await new Promise(r=>setTimeout(r,3000));}
})();

// ── Appearance: theme + font size ──
(function(){
  function setTheme(t,el){
    document.body.setAttribute('data-theme',t);
    localStorage.setItem('uiTheme',t);
    document.querySelectorAll('.theme-swatch').forEach(s=>s.classList.toggle('active',s.dataset.t===t));
  }
  function setFontSize(fs,el){
    document.body.setAttribute('data-fs',fs);
    localStorage.setItem('uiFontSize',fs);
    document.querySelectorAll('.fs-btn').forEach(b=>b.classList.toggle('active',b.dataset.fs===fs));
  }
  window.setTheme=setTheme;
  window.setFontSize=setFontSize;
  // restore persisted prefs
  if(localStorage.getItem('uiTheme')==='grey')localStorage.setItem('uiTheme','light');
  const savedTheme=localStorage.getItem('uiTheme')||'dark-blue';

  // Migrate old font size keys
  (function(){const m={'sm':'s','md':'m','lg':'m'};const v=localStorage.getItem('uiFontSize');if(v&&m[v])localStorage.setItem('uiFontSize',m[v]);})();
  const savedFs=localStorage.getItem('uiFontSize')||'xl';
  document.body.setAttribute('data-theme',savedTheme);
  document.body.setAttribute('data-fs',savedFs);
  document.querySelectorAll('.theme-swatch').forEach(s=>s.classList.toggle('active',s.dataset.t===savedTheme));
  document.querySelectorAll('.fs-btn').forEach(b=>b.classList.toggle('active',b.dataset.fs===savedFs));

  // keyboard accessibility: nav items / theme swatches を Tab + Enter/Space で操作可能にする
  document.querySelectorAll('.nav-item,.theme-swatch').forEach(el=>{
    el.setAttribute('tabindex','0');
    el.setAttribute('role','button');
    el.addEventListener('keydown',e=>{
      if(e.key==='Enter'||e.key===' '){e.preventDefault();el.click();}
    });
  });

  // brand status sync
  function syncBrandStatus(){
    const bar=document.getElementById('hdr-bar');
    const dot=document.getElementById('brand-dot');
    const txt=document.getElementById('brand-status-text');
    const upt=document.getElementById('brand-uptime-small');
    const running=bar&&bar.classList.contains('running');
    if(dot)dot.classList.toggle('running',running);
    if(txt){
      const psState=document.getElementById('ps-state')||document.getElementById('dash-ps-state');
      if(window._backendDisconnected)txt.textContent='接続断';
      else if(running&&psState)txt.textContent='巡回中';
      else if(running)txt.textContent='稼働中';
      else txt.textContent='待機中';
    }
    const uptimeEl=document.getElementById('dash-uptime');
    if(upt&&uptimeEl)upt.textContent=uptimeEl.textContent||'';
  }
  setInterval(syncBrandStatus,1000);
})();

// ===== 画面判定: 監視矩形 ドラッグ選択モーダル =====
const _ssState={img:null,imgW:0,imgH:0,drag:null,rect:null};

async function openScreenshotPicker(){
  const overlay=document.getElementById('ss-overlay');
  if(!overlay)return;
  overlay.style.display='flex';
  _ssInitHandlers();
  // 既存値をプリセット
  _ssState.rect={
    x:parseInt((document.getElementById('cfg-patrol_screen_region_x')||{}).value||'0',10)||0,
    y:parseInt((document.getElementById('cfg-patrol_screen_region_y')||{}).value||'0',10)||0,
    w:parseInt((document.getElementById('cfg-patrol_screen_region_w')||{}).value||'0',10)||0,
    h:parseInt((document.getElementById('cfg-patrol_screen_region_h')||{}).value||'0',10)||0,
  };
  _ssUpdateCoordDisplay();
  await _ssLoadDevices();
  _ssCaptureScreenshot();
}

function closeScreenshotPicker(){
  const overlay=document.getElementById('ss-overlay');
  if(overlay)overlay.style.display='none';
  _ssState.drag=null;
}

async function _ssLoadDevices(){
  const sel=document.getElementById('ss-serial');
  if(!sel)return;
  const prev=sel.value;
  try{
    const r=await fetch('/api/devices');
    const d=await r.json();
    const list=(d&&(d.devices||d.serials))||(Array.isArray(d)?d:[]);
    if(!list.length){sel.innerHTML='<option value="">デバイスなし</option>';return;}
    sel.innerHTML=list.map(s=>'<option value="'+escHtml(s)+'"'+(s===prev?' selected':'')+'>'+escHtml(s)+'</option>').join('');
  }catch(_e){
    sel.innerHTML='<option value="">取得失敗</option>';
  }
}

async function _ssCaptureScreenshot(){
  const sel=document.getElementById('ss-serial');
  const status=document.getElementById('ss-status');
  const canvas=document.getElementById('ss-canvas');
  if(!sel||!sel.value){if(status)status.textContent='デバイス未選択';return;}
  if(!canvas)return;
  if(status)status.textContent='スクショ取得中…';
  try{
    const r=await fetch('/api/patrol/screenshot?serial='+encodeURIComponent(sel.value));
    if(!r.ok){if(status)status.textContent='失敗: HTTP '+r.status;return;}
    const blob=await r.blob();
    const url=URL.createObjectURL(blob);
    const img=new Image();
    img.onload=()=>{
      _ssState.img=img;
      _ssState.imgW=img.naturalWidth;
      _ssState.imgH=img.naturalHeight;
      canvas.width=img.naturalWidth;
      canvas.height=img.naturalHeight;
      _ssRedraw();
      URL.revokeObjectURL(url);
      if(status)status.textContent=img.naturalWidth+'×'+img.naturalHeight+' px';
    };
    img.onerror=()=>{if(status)status.textContent='画像デコード失敗';URL.revokeObjectURL(url);};
    img.src=url;
  }catch(e){
    if(status)status.textContent='失敗: '+(e&&e.message||e);
  }
}

function _ssRedraw(){
  const canvas=document.getElementById('ss-canvas');
  if(!canvas||!_ssState.img)return;
  const ctx=canvas.getContext('2d');
  ctx.drawImage(_ssState.img,0,0);
  const r=_ssState.rect;
  if(r&&r.w>0&&r.h>0){
    ctx.fillStyle='rgba(255,255,0,0.18)';
    ctx.fillRect(r.x,r.y,r.w,r.h);
    ctx.strokeStyle='#ff3';
    ctx.lineWidth=Math.max(2,Math.round(_ssState.imgW/640));
    ctx.strokeRect(r.x,r.y,r.w,r.h);
  }
}

function _ssCanvasToImage(e){
  const canvas=document.getElementById('ss-canvas');
  const rect=canvas.getBoundingClientRect();
  const sx=canvas.width/rect.width;
  const sy=canvas.height/rect.height;
  let x=Math.round((e.clientX-rect.left)*sx);
  let y=Math.round((e.clientY-rect.top)*sy);
  if(x<0)x=0;if(y<0)y=0;
  if(x>canvas.width)x=canvas.width;
  if(y>canvas.height)y=canvas.height;
  return{x:x,y:y};
}

function _ssUpdateCoordDisplay(){
  const r=_ssState.rect||{};
  ['x','y','w','h'].forEach(k=>{
    const el=document.getElementById('ss-'+k);
    if(el)el.textContent=(r[k]!==undefined&&r[k]>=0&&(k==='x'||k==='y'||r[k]>0))?r[k]:'-';
  });
  const apply=document.getElementById('ss-apply');
  if(apply)apply.disabled=!(r.w>0&&r.h>0);
}

function _ssInitHandlers(){
  const canvas=document.getElementById('ss-canvas');
  if(canvas&&!canvas.__ssInit){
    canvas.__ssInit=true;
    canvas.addEventListener('mousedown',e=>{
      e.preventDefault();
      const p=_ssCanvasToImage(e);
      _ssState.drag={sx:p.x,sy:p.y,ex:p.x,ey:p.y};
    });
    canvas.addEventListener('mousemove',e=>{
      if(!_ssState.drag)return;
      const p=_ssCanvasToImage(e);
      _ssState.drag.ex=p.x;_ssState.drag.ey=p.y;
      const sx=Math.min(_ssState.drag.sx,_ssState.drag.ex);
      const sy=Math.min(_ssState.drag.sy,_ssState.drag.ey);
      const ex=Math.max(_ssState.drag.sx,_ssState.drag.ex);
      const ey=Math.max(_ssState.drag.sy,_ssState.drag.ey);
      _ssState.rect={x:sx,y:sy,w:ex-sx,h:ey-sy};
      _ssRedraw();
      _ssUpdateCoordDisplay();
    });
    const endDrag=()=>{_ssState.drag=null;};
    canvas.addEventListener('mouseup',endDrag);
    canvas.addEventListener('mouseleave',endDrag);
  }
  const reload=document.getElementById('ss-reload');
  if(reload&&!reload.__ssInit){reload.__ssInit=true;reload.onclick=_ssCaptureScreenshot;}
  const apply=document.getElementById('ss-apply');
  if(apply&&!apply.__ssInit){apply.__ssInit=true;apply.onclick=_ssApply;}
  const sel=document.getElementById('ss-serial');
  if(sel&&!sel.__ssInit){sel.__ssInit=true;sel.onchange=_ssCaptureScreenshot;}
}

function _ssApply(){
  const r=_ssState.rect;
  if(!r||r.w<=0||r.h<=0)return;
  const setVal=(k,v)=>{
    const el=document.getElementById('cfg-patrol_screen_region_'+k);
    if(el)el.value=String(v);
  };
  setVal('x',r.x);setVal('y',r.y);setVal('w',r.w);setVal('h',r.h);
  closeScreenshotPicker();
  if(typeof saveConfig==='function')saveConfig(false);
}
