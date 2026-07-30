/* ===== 邮箱管理 ===== */
let mbPage = 1;
const size = 20;
let mbCache = {};

const MB_STATUS = {
  unverified: '待验证',
  verifying: '验证中',
  verify_failed: '验证失败',
  verified: '已验证',
};

let mbLoading = false;
const mbSelected = new Set();

async function loadMailboxes() {
  if (mbLoading) return; // 避免请求重叠
  mbLoading = true;
  try {
    const q = document.getElementById('mb-search').value.trim();
    const status = document.getElementById('mb-filter').value;
    const params = new URLSearchParams({ page: mbPage, size });
    if (q) params.set('q', q);
    if (status) params.set('status', status);
    const r = await api('/api/mailboxes?' + params);
    const d = await r.json();
    mbCache = {};
    (d.data || []).forEach(x => (mbCache[x.id] = x));
    document.getElementById('mb-rows').innerHTML = (d.data || []).map(x => `
      <tr class="${mbSelected.has(x.id) ? 'row-sel' : ''}">
        <td class="col-check"><input type="checkbox" ${mbSelected.has(x.id) ? 'checked' : ''} onclick="toggleSelect(${x.id}, this.checked)"></td>
        <td>${esc(x.email)}${x.provider === 'varymail' ? ' <span class="badge vary">vary</span>' : ''}</td>
        <td>${Number(x.register_count || 0)} / ${Number(x.register_limit || 0)}</td>
        <td>${fmtTime(x.created_at)}</td>
        <td><span class="badge ${esc(x.status)}">${MB_STATUS[x.status] || esc(x.status)}</span></td>
        <td>
          ${x.status === 'verified' ? `<button class="icon-btn" title="取件" onclick="openMailModal(${x.id})">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3 7 9 6 9-6"/></svg>
          </button>` : ''}
          <button class="icon-btn danger" title="删除" onclick="delMailbox(${x.id})">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m2 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M10 11v6M14 11v6"/></svg>
          </button>
        </td>
      </tr>`).join('');
    const maxPage = Math.max(1, Math.ceil((d.total || 0) / size));
    renderPager('mb-pager', mbPage, maxPage, p => { mbPage = p; loadMailboxes(); });
    syncBatchBar();
  } finally {
    mbLoading = false;
  }
}

/* ===== 多选 ===== */
function toggleSelect(id, checked) {
  if (checked) mbSelected.add(id); else mbSelected.delete(id);
  syncBatchBar();
}

function toggleSelectAll(checked) {
  Object.keys(mbCache).forEach(id => {
    if (checked) mbSelected.add(Number(id)); else mbSelected.delete(Number(id));
  });
  loadMailboxes();
}

function clearSelection() {
  mbSelected.clear();
  loadMailboxes();
}

function syncBatchBar() {
  const bar = document.getElementById('mb-batch');
  bar.style.display = mbSelected.size ? 'flex' : 'none';
  document.getElementById('mb-batch-count').textContent = '已选 ' + mbSelected.size + ' 项';
  const all = document.getElementById('mb-check-all');
  const ids = Object.keys(mbCache).map(Number);
  all.checked = ids.length > 0 && ids.every(id => mbSelected.has(id));
}

async function verifySelected() {
  if (!mbSelected.size) return;
  await runVerify([...mbSelected]);
}

async function delSelected() {
  const ids = [...mbSelected];
  if (!ids.length) return;
  if (!confirm('确定删除所选 ' + ids.length + ' 个邮箱?')) return;
  for (const id of ids) {
    await api('/api/mailboxes/' + id, { method: 'DELETE' });
    mbSelected.delete(id);
  }
  toast('已删除 ' + ids.length + ' 个');
  loadMailboxes();
}

/* ===== 批量导入 ===== */
function openImportModal() {
  document.getElementById('import-text').value = '';
  document.getElementById('import-count').textContent = '已识别 0 个邮箱';
  document.getElementById('import-modal').style.display = 'flex';
}

function updateImportCount() {
  const n = parseImportLines(document.getElementById('import-text').value).length;
  document.getElementById('import-count').textContent = '已识别 ' + n + ' 个邮箱';
}

(function () {
  const ta = document.getElementById('import-text');
  if (!ta) return;
  ta.addEventListener('input', updateImportCount);
  ta.addEventListener('dragover', e => { e.preventDefault(); ta.classList.add('drag'); });
  ta.addEventListener('dragleave', () => ta.classList.remove('drag'));
  ta.addEventListener('drop', e => {
    e.preventDefault();
    ta.classList.remove('drag');
    const f = e.dataTransfer.files && e.dataTransfer.files[0];
    if (!f) return;
    const reader = new FileReader();
    reader.onload = () => { ta.value = reader.result; updateImportCount(); };
    reader.readAsText(f);
  });
})();

function parseImportLines(text) {
  const items = [];
  text.split(/\r?\n/).forEach(line => {
    line = line.trim();
    if (!line) return;
    const parts = line.split('----').map(p => p.trim());
    if (parts.length !== 4 || !parts[0].includes('@')) return;
    items.push({
      email: parts[0],
      password: parts[1],
      client_id: parts[2],
      refresh_token: parts[3],
    });
  });
  return items;
}

async function doImport() {
  const text = document.getElementById('import-text').value;
  const items = parseImportLines(text);
  if (!items.length) return toast('没有可导入的有效行', true);
  const r = await api('/api/mailboxes/import', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ items }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast('导入失败: ' + (d.error || r.status), true);
  closeModal('import-modal');
  toast(`识别 ${items.length} 个：新增 ${d.added}，跳过 ${d.skipped}`);
  mbPage = 1;
  loadMailboxes();
  if (d.added > 0) verifyAll(); // 导入后自动验证
}

/* ===== 批量验证：10 并发，10 个一批 ===== */
let verifying = false;

async function verifyAll() {
  if (verifying) return;
  // 拉取所有待验证 / 验证失败的邮箱 id
  const ids = [];
  let page = 1;
  for (;;) {
    const r = await api('/api/mailboxes?' + new URLSearchParams({ page, size: 100 }));
    const d = await r.json().catch(() => ({}));
    const list = d.data || [];
    list.forEach(x => {
      if (x.status === 'unverified' || x.status === 'verify_failed' || x.status === 'verifying') ids.push(x.id);
    });
    if (page * 100 >= (d.total || 0) || !list.length) break;
    page++;
  }
  if (!ids.length) return toast('没有需要验证的邮箱');
  await runVerify(ids);
}

// 10 并发验证给定的邮箱 id 列表
async function runVerify(ids) {
  if (verifying || !ids.length) return;
  verifying = true;
  let ok = 0, fail = 0;
  const CONCURRENCY = 10;
  let idx = 0;

  async function worker() {
    while (idx < ids.length) {
      const id = ids[idx++];
      try {
        const r = await api('/api/mailboxes/' + id + '/verify', { method: 'POST' });
        const d = await r.json().catch(() => ({}));
        if (d.status === 'verified') ok++; else fail++;
      } catch (e) {
        fail++;
      }
      loadMailboxes();
    }
  }

  const workers = [];
  for (let i = 0; i < CONCURRENCY; i++) workers.push(worker());
  await Promise.all(workers);

  verifying = false;
  toast(`验证完成：成功 ${ok}，失败 ${fail}`);
  loadMailboxes();
}

function openMailboxModal(data) {
  document.getElementById('mb-modal-title').textContent = data ? '编辑邮箱 #' + data.id : '新增邮箱';
  document.getElementById('mb-id').value = data ? data.id : '';
  document.getElementById('mb-email').value = data ? data.email : '';
  document.getElementById('mb-password').value = data ? data.password : '';
  document.getElementById('mb-provider').value = data ? data.provider : '';
  document.getElementById('mb-client-id').value = data ? data.client_id : '';
  document.getElementById('mb-refresh-token').value = data ? data.refresh_token : '';
  document.getElementById('mb-status').value = data ? data.status : 'unverified';
  syncSelect('mb-status');
  document.getElementById('mb-note').value = data ? data.note : '';
  document.getElementById('mb-modal').style.display = 'flex';
}

function editMailbox(id) {
  if (mbCache[id]) openMailboxModal(mbCache[id]);
}

async function saveMailbox() {
  const id = document.getElementById('mb-id').value;
  const body = {
    email: document.getElementById('mb-email').value.trim(),
    password: document.getElementById('mb-password').value,
    provider: document.getElementById('mb-provider').value.trim(),
    client_id: document.getElementById('mb-client-id').value.trim(),
    refresh_token: document.getElementById('mb-refresh-token').value.trim(),
    status: document.getElementById('mb-status').value,
    note: document.getElementById('mb-note').value,
  };
  if (!body.email) return toast('email 必填', true);
  const r = await api('/api/mailboxes' + (id ? '/' + id : ''), {
    method: id ? 'PUT' : 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({}));
    return toast('保存失败: ' + (e.error || r.status), true);
  }
  closeModal('mb-modal');
  toast(id ? '已更新' : '已新增');
  loadMailboxes();
}

async function delMailbox(id) {
  if (!confirm('确定删除邮箱 #' + id + ' ?')) return;
  const r = await api('/api/mailboxes/' + id, { method: 'DELETE' });
  if (!r.ok) return toast('删除失败', true);
  toast('已删除');
  loadMailboxes();
}

/* ===== 取件弹窗：打开开始轮询（3 秒），关闭立即停止 ===== */
let mailTimer = null;      // 轮询定时器
let mailMailboxId = null;  // 当前取件的邮箱 id
let mailMsgs = [];         // 最新一次拉到的邮件
let mailSelected = 0;      // 当前选中邮件下标
let mailFetching = false;  // 避免请求重叠
let mailListSig = '';      // 列表渲染签名，用于跳过无变化的重绘

function openMailModal(id) {
  const mb = mbCache[id];
  if (!mb) return;
  mailMailboxId = id;
  mailMsgs = [];
  mailSelected = 0;
  mailListSig = '';
  document.getElementById('mail-title').textContent = mb.email;
  document.getElementById('mail-sub').textContent = '收件箱 · 3 秒自动刷新';
  document.getElementById('mail-list').innerHTML = '<div class="mail-empty">加载中...</div>';
  document.getElementById('mail-meta').innerHTML = '';
  document.getElementById('mail-frame').srcdoc = '';
  document.getElementById('mail-modal').style.display = 'flex';
  document.body.classList.add('modal-open');
  fetchMail();
  mailTimer = setInterval(fetchMail, 3000);
}

function closeMailModal() {
  if (mailTimer) { clearInterval(mailTimer); mailTimer = null; }
  mailMailboxId = null;
  mailMsgs = [];
  document.getElementById('mail-modal').style.display = 'none';
  document.body.classList.remove('modal-open');
}

async function fetchMail() {
  if (mailFetching || mailMailboxId === null) return;
  mailFetching = true;
  const id = mailMailboxId;
  try {
    const r = await api('/api/mailboxes/' + id + '/messages');
    if (id !== mailMailboxId) return; // 请求返回时弹窗已关/已切换
    const d = await r.json().catch(() => ({}));
    if (!r.ok) {
      document.getElementById('mail-list').innerHTML =
        `<div class="mail-empty err">${esc(d.error || '取件失败')}</div>`;
      return;
    }
    // 合并而非整体替换：新邮件插到最前，已有邮件保留。
    // 这样即使某次轮询返回不全（临时少几封），列表也不会先塌陷再跳回，避免闪烁。
    const prevId = msgKey(mailMsgs[mailSelected]);
    mailMsgs = mergeMsgs(mailMsgs, d.items || []);
    const i = prevId ? mailMsgs.findIndex(m => msgKey(m) === prevId) : -1;
    mailSelected = i >= 0 ? i : 0;
    renderMailList();
    renderMailDetail();
  } finally {
    mailFetching = false;
  }
}

// 邮件唯一键：优先用消息 id，退回 主题+时间。
function msgKey(m) {
  if (!m) return '';
  return m.id || (m.subject + '|' + m.received_at);
}

// 合并已有与新拉取的邮件：按 key 去重，新邮件覆盖旧记录，按接收时间倒序（最新在前）。
function mergeMsgs(existing, incoming) {
  const map = new Map();
  (existing || []).forEach(m => map.set(msgKey(m), m));
  (incoming || []).forEach(m => map.set(msgKey(m), m));
  return [...map.values()].sort((a, b) =>
    new Date(b.received_at || 0) - new Date(a.received_at || 0));
}

function renderMailList() {
  const list = document.getElementById('mail-list');
  if (!mailMsgs.length) {
    mailListSig = '';
    list.innerHTML = '<div class="mail-empty">暂无邮件，等待新邮件...</div>';
    return;
  }
  // 列表内容与选中项都没变时不重绘，避免每 3 秒轮询造成闪烁。
  const sig = mailSelected + '#' + mailMsgs.map(msgKey).join(',');
  if (sig === mailListSig) return;
  mailListSig = sig;
  list.innerHTML = mailMsgs.map((m, i) => `
    <div class="mail-item${i === mailSelected ? ' active' : ''}" onclick="selectMail(${i})">
      <div class="mail-item-from">${esc(m.from_name || m.from)}</div>
      <div class="mail-item-subject">${esc(m.subject)}</div>
      <div class="mail-item-time">${fmtTime(m.received_at)}</div>
    </div>`).join('');
}

function selectMail(i) {
  mailSelected = i;
  renderMailList();
  renderMailDetail();
}

const mailBodyCache = {};  // 按消息 id 缓存正文，避免每次轮询/切换重复拉取

function renderMailDetail() {
  const m = mailMsgs[mailSelected];
  const meta = document.getElementById('mail-meta');
  const frame = document.getElementById('mail-frame');
  if (!m) {
    meta.innerHTML = '';
    setFrameHTML(frame, '');
    frame.dataset.cur = '';
    return;
  }
  meta.innerHTML = `
    <div class="mail-subject">${esc(m.subject)}</div>
    <div class="mail-from">${esc(m.from_name || '')} &lt;${esc(m.from)}&gt;</div>
    <div class="mail-time">${fmtTime(m.received_at)}</div>`;
  const body = mailBodyCache[m.id];
  if (!body) {
    if (frame.dataset.cur !== 'loading:' + m.id) {
      frame.dataset.cur = 'loading:' + m.id;
      setFrameHTML(frame, '<!doctype html><meta charset="utf-8"><style>html,body{height:100%;margin:0}.wrap{height:100%;display:flex;align-items:center;justify-content:center}.spin{width:32px;height:32px;border:3px solid #e4e9f2;border-top-color:#3b82f6;border-radius:50%;animation:r .8s linear infinite}@keyframes r{to{transform:rotate(360deg)}}</style><div class="wrap"><div class="spin"></div></div>');
    }
    loadMailBody(m.id);
    return;
  }
  // 只展示 HTML 模式；iframe 隔离邮件内容
  const html = body.html || `<pre style="white-space:pre-wrap;font-family:inherit">${esc(body.text)}</pre>`;
  if (frame.dataset.cur !== 'body:' + m.id) {
    frame.dataset.cur = 'body:' + m.id;
    setFrameHTML(frame, html);
  }
}

// 直接写入 iframe 文档（sandbox 保留 allow-same-origin 但不含 allow-scripts，
// 因此父页可访问文档、邮件内脚本不会执行）。document.write 比 srcdoc/blob 在各浏览器里最稳。
const FRAME_BASE_CSS = '<style>::-webkit-scrollbar{width:0;height:0}html{scrollbar-width:none}</style>';

function setFrameHTML(frame, html) {
  const content = FRAME_BASE_CSS + (html || '<!doctype html><meta charset="utf-8">');
  try {
    const doc = frame.contentDocument || (frame.contentWindow && frame.contentWindow.document);
    if (doc) {
      doc.open();
      doc.write(content);
      doc.close();
      return;
    }
  } catch (e) { /* 退回 srcdoc */ }
  frame.srcdoc = content;
}

async function loadMailBody(msgId) {
  if (mailBodyCache[msgId] || mailMailboxId === null) return;
  const boxId = mailMailboxId;
  const r = await api('/api/mailboxes/' + boxId + '/message?mid=' + encodeURIComponent(msgId));
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return;
  mailBodyCache[msgId] = { html: d.html || '', text: d.text || '' };
  // 若用户仍停留在这封邮件，立即渲染
  const cur = mailMsgs[mailSelected];
  if (boxId === mailMailboxId && cur && cur.id === msgId) renderMailDetail();
}

/* ===== 表格 3 秒自动刷新（页面隐藏时暂停，避免浪费） ===== */
let mbTimer = setInterval(() => {
  if (!document.hidden) loadMailboxes();
}, 3000);

document.getElementById('mb-search').addEventListener('keydown', e => {
  if (e.key === 'Enter') { mbPage = 1; loadMailboxes(); }
});
document.getElementById('mb-filter').addEventListener('change', () => { mbPage = 1; loadMailboxes(); });

/* 点遮罩关闭取件弹窗时也要停轮询 */
document.getElementById('mail-modal').addEventListener('click', e => {
  if (e.target === e.currentTarget) closeMailModal();
});

loadMailboxes();
