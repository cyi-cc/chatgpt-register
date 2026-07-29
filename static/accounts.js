/* ===== 账户管理（ChatGPT + Codex 生产） ===== */
const ACC_STATUS = {
  pending: '待生产',
  registering: '注册中',
  registered: '已注册',
  register_failed: '注册失败',
  already_registered: '停用',
};
let page = 1;
const size = 20;
let accCache = {};
let accTotal = 0;
const accSelected = new Set();

async function load() {
  const q = document.getElementById('search').value.trim();
  const status = document.getElementById('filter-status').value;
  const params = new URLSearchParams({ page, size });
  if (q) params.set('q', q);
  if (status) params.set('status', status);
  const r = await api('/api/registrations?' + params);
  const d = await r.json();
  accCache = {};
  accTotal = d.total || 0;
  (d.data || []).forEach(x => { accCache[x.id] = x; });
  document.getElementById('rows').innerHTML = (d.data || []).map(rowHtml).join('')
    || '<tr><td colspan="6" style="text-align:center;color:var(--text-3)">暂无数据</td></tr>';
  const maxPage = Math.max(1, Math.ceil((d.total || 0) / size));
  renderPager('pager', page, maxPage, p => { page = p; load(); });
  syncBatchBar();
}

function rowHtml(x) {
  const canDownload = x.status === 'registered';
  return `
    <tr class="${accSelected.has(x.id) ? 'row-sel' : ''}">
      <td class="col-check"><input type="checkbox" ${accSelected.has(x.id) ? 'checked' : ''} onclick="toggleSelect(${x.id}, this.checked)"></td>
      <td>${esc(x.email)}</td>
      <td>${fmtTime(x.created_at)}</td>
      <td><span class="badge ${esc(x.status)}">${ACC_STATUS[x.status] || esc(x.status)}</span></td>
      <td class="ship-cell">
        <span class="badge ${x.shipped ? 'registered' : 'pending'}" title="下载后自动标记，不能手动修改">${x.shipped ? '已出库' : '未出库'}</span>
      </td>
      <td>
        <button class="icon-btn" title="日志" onclick="showLog(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h5"/></svg>
        </button>
        <button class="icon-btn" title="下载" ${canDownload ? '' : 'disabled'} onclick="downloadAcc(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/><path d="M12 15V3"/></svg>
        </button>
        <button class="icon-btn danger" title="删除" onclick="del(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m2 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M10 11v6M14 11v6"/></svg>
        </button>
      </td>
    </tr>`;
}

/* ===== 生产进度 ===== */
async function loadProduce() {
  try {
    const r = await api('/api/produce/status');
    const s = await r.json();
    document.getElementById('pd-pending').textContent = s.pending || 0;
    document.getElementById('pd-running').textContent = s.running_num || 0;
    document.getElementById('pd-registered').textContent = s.registered || 0;
    document.getElementById('pd-failed').textContent = s.failed || 0;
    document.getElementById('pd-stop').style.display = s.running ? '' : 'none';
  } catch (e) { /* ignore */ }
}

/* 浏览器就绪状态：未就绪禁用生产 */
let browserReady = true;
async function loadBrowserGate() {
  try {
    const s = await (await api('/api/browser/status')).json();
    browserReady = !!s.ready;
    const btn = document.getElementById('produce-btn');
    if (btn) {
      btn.disabled = !browserReady;
      btn.title = browserReady ? '' : (s.message || '缺少浏览器');
    }
    const msg = document.getElementById('pd-msg');
    if (!browserReady && msg) msg.textContent = '⚠ ' + (s.message || '缺少浏览器，暂不能生产');
  } catch (e) { /* ignore */ }
}

function openProduceModal() {
  if (!browserReady) return toast('缺少浏览器，正在下载或下载失败，暂不能生产', true);
  document.getElementById('produce-count').value = 10;
  document.getElementById('produce-modal').style.display = 'flex';
}

async function startProduce() {
  const count = parseInt(document.getElementById('produce-count').value, 10);
  if (!count || count < 1) return toast('请输入有效数量', true);
  const r = await api('/api/produce', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ count }),
  });
  if (!r.ok) {
    const d = await r.json().catch(() => ({}));
    return toast(d.error || '启动生产失败', true);
  }
  closeModal('produce-modal');
  toast('已开始生产 ' + count + ' 个账号');
  loadProduce();
  load();
}

async function stopProduce() {
  if (!confirm('确定停止当前生产任务?')) return;
  await api('/api/produce/stop', { method: 'POST' });
  toast('已请求停止');
  loadProduce();
}

let logTimer = null;
let logAccId = null;

async function showLog(id) {
  logAccId = id;
  // 清空旧日志，避免打开新账号时残留上一次的内容
  document.getElementById('log-title').textContent = '执行日志';
  document.getElementById('log-body').textContent = '加载中...';
  document.getElementById('log-shot-btn').style.display = 'none';
  document.getElementById('log-modal').style.display = 'flex';
  document.body.style.overflow = 'hidden';
  await refreshLog(false);
  clearInterval(logTimer);
  logTimer = setInterval(() => refreshLog(true), 2000);
}

async function refreshLog(silent) {
  if (logAccId == null) return;
  const r = await api('/api/registrations/' + logAccId + '/logs');
  if (!r.ok) { if (!silent) toast('读取日志失败', true); return; }
  const d = await r.json();
  document.getElementById('log-title').textContent = '执行日志 · ' + d.email;
  document.getElementById('log-shot-btn').style.display = d.has_shot ? '' : 'none';
  const parts = [];
  if (d.note) parts.push('备注: ' + d.note);
  if (parts.length) parts.push('');
  parts.push(d.log || '（无执行日志）');
  document.getElementById('log-body').textContent = parts.join('\n');
}

function closeLog() {
  clearInterval(logTimer);
  logTimer = null;
  logAccId = null;
  document.getElementById('log-modal').style.display = 'none';
  document.body.style.overflow = '';
  document.getElementById('log-body').textContent = '';
}

/* ===== 异常截图 ===== */
async function viewShot() {
  if (logAccId == null) return;
  const r = await api('/api/registrations/' + logAccId + '/shot');
  if (!r.ok) return toast('暂无异常截图', true);
  const blob = await r.blob();
  const img = document.getElementById('shot-img');
  if (img.dataset.url) URL.revokeObjectURL(img.dataset.url);
  img.src = img.dataset.url = URL.createObjectURL(blob);
  document.getElementById('shot-modal').style.display = 'flex';
}
function closeShot() {
  document.getElementById('shot-modal').style.display = 'none';
}

/* ===== 多选 ===== */
function toggleSelect(id, checked) {
  if (checked) accSelected.add(id); else accSelected.delete(id);
  syncBatchBar();
}
function toggleSelectAll(checked) {
  Object.keys(accCache).forEach(id => {
    if (checked) accSelected.add(Number(id)); else accSelected.delete(Number(id));
  });
  load();
}
function clearSelection() { accSelected.clear(); load(); }
function syncBatchBar() {
  const bar = document.getElementById('acc-batch');
  bar.style.display = accSelected.size ? 'flex' : 'none';
  document.getElementById('acc-batch-count').textContent = '已选 ' + accSelected.size + ' 项';
  const all = document.getElementById('acc-check-all');
  const ids = Object.keys(accCache).map(Number);
  all.checked = ids.length > 0 && ids.every(id => accSelected.has(id));
}

/* ===== 下载（access_token 纯文本，一行一个；下载即出库） ===== */
async function downloadAcc(id) {
  await downloadByIds([id], 'access_token_' + id + '.txt');
}
async function downloadSelected() {
  const ids = [...accSelected];
  if (!ids.length) return;
  await downloadByIds(ids, 'access_tokens_' + ids.length + '.txt');
}
async function downloadByIds(ids, filename) {
  const r = await api('/api/download', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  });
  if (!r.ok) {
    const d = await r.json().catch(() => ({}));
    return toast(d.error || '下载失败', true);
  }
  const blob = await r.blob();
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  a.click();
  URL.revokeObjectURL(a.href);
  load(); // 刷新出库状态
}

/* ===== 删除 ===== */
async function delSelected() {
  const ids = [...accSelected];
  if (!ids.length) return;
  if (!confirm('确定删除所选 ' + ids.length + ' 个账户?')) return;
  for (const id of ids) {
    await api('/api/registrations/' + id, { method: 'DELETE' });
    accSelected.delete(id);
  }
  toast('已删除 ' + ids.length + ' 个');
  load();
}
async function del(id) {
  if (!confirm('确定删除账户 #' + id + ' ?')) return;
  const r = await api('/api/registrations/' + id, { method: 'DELETE' });
  if (!r.ok) return toast('删除失败', true);
  accSelected.delete(id);
  toast('已删除');
  load();
}

document.getElementById('search').addEventListener('keydown', e => {
  if (e.key === 'Enter') { page = 1; load(); }
});
document.getElementById('filter-status').addEventListener('change', () => { page = 1; load(); });

load();
loadProduce();
loadBrowserGate();
setInterval(load, 3000);
setInterval(loadProduce, 2000);
setInterval(loadBrowserGate, 2500);
