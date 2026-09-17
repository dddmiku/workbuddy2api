"use strict";
// ═══ 更新日志 ═══
// 2026-09-16：接入密钥管理导航与本页刷新，侧栏不再下发或复制完整配置密钥。

var $  = function(s, r){ return (r || document).querySelector(s); };
var $$ = function(s, r){ return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
function esc(t){ return String(t == null ? '' : t).replace(/[&<>"']/g, function(c){
  return ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'})[c]; }); }
function pad(n){ return n < 10 ? '0' + n : '' + n; }
function num(n){ return (n == null || isNaN(n)) ? '—' : Number(n).toLocaleString('en-US'); }
function shortUid(u){ return (u && u.length > 18) ? u.slice(0, 8) + '…' + u.slice(-6) : (u || '—'); }
function fmtTime(iso){
  if (!iso) return '—';
  var s = String(iso);
  if (s.indexOf('0001-01-01') === 0) return '—';
  var d = new Date(s);
  if (isNaN(d.getTime())) return '—';
  return pad(d.getMonth()+1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}
function relTime(iso){
  if (!iso) return '从未';
  var s = String(iso);
  if (s.indexOf('0001-01-01') === 0) return '从未';
  var d = new Date(s);
  if (isNaN(d.getTime())) return '从未';
  var diff = Math.floor((Date.now() - d.getTime()) / 1000);
  if (diff < 0) return '即将';
  if (diff < 60) return '刚刚';
  if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
  if (diff < 2592000) return Math.floor(diff / 86400) + ' 天前';
  return fmtTime(iso);
}
function daysLeft(sec){
  if (!sec) return null;
  return Math.round((sec * 1000 - Date.now()) / 86400000);
}
function svg(inner, size){
  return '<svg width="' + (size||16) + '" height="' + (size||16) + '" viewBox="0 0 24 24" fill="none" ' +
    'stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">' + inner + '</svg>';
}
var IC = {
  users: '<path d="M15 20.5v-1.6a3.4 3.4 0 0 0-3.4-3.4H6.4A3.4 3.4 0 0 0 3 18.9v1.6"/><circle cx="9" cy="7.6" r="3.6"/><path d="M21 20.5v-1.6a3.4 3.4 0 0 0-2.6-3.3"/>',
  check: '<circle cx="12" cy="12" r="8.6"/><path d="m8.4 12.2 2.5 2.5 4.7-5.2"/>',
  clock: '<circle cx="12" cy="12" r="8.6"/><path d="M12 7.4V12l3 1.8"/>',
  warn:  '<path d="M12 9.5v4M12 17h.01"/><path d="M10.3 4.4 2.9 17.2A1.9 1.9 0 0 0 4.6 20h14.8a1.9 1.9 0 0 0 1.7-2.8L13.7 4.4a1.9 1.9 0 0 0-3.4 0Z"/>',
  box:   '<path d="M20.5 7.5 12 3 3.5 7.5v9L12 21l8.5-4.5v-9Z"/><path d="m3.5 7.5 8.5 4.5 8.5-4.5"/>',
  coins: '<circle cx="12" cy="12" r="8.6"/><path d="M12 7.4v9.2M9.4 10h5.2M9.4 14h5.2"/>',
  eye:   '<path d="M2.2 12S5.8 5.4 12 5.4 21.8 12 21.8 12 18.2 18.6 12 18.6 2.2 12 2.2 12Z"/><circle cx="12" cy="12" r="2.7"/>',
  play:  '<path d="M8 5.4v13.2L19 12 8 5.4Z"/>',
  trash: '<path d="M4 7h16M9.6 7V4.8h4.8V7M6.6 7l.9 13.2h9l.9-13.2"/>',
  pulse: '<path d="M3 12h3.6l2.4-6 3.9 12 2.6-6H21"/>',
  moon:  '<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z"/>',
  sun:   '<circle cx="12" cy="12" r="4"/><path d="M12 2.6v2.2M12 19.2v2.2M2.6 12h2.2M19.2 12h2.2M5.3 5.3l1.6 1.6M17.1 17.1l1.6 1.6M18.7 5.3l-1.6 1.6M6.9 17.1l-1.6 1.6"/>'
};

/* ── 状态 ─────────────────────────────────────────── */
var S = { data:null, tasks:null, filter:'all', q:'', logLines:120 };
var DT = { realm:'cn', url:'', timer:null, tries:0 };
var modalCb = null;
var logKey = null, logName = '';

/* ── 主题 ─────────────────────────────────────────── */
function setTheme(t){
  document.documentElement.setAttribute('data-theme', t);
  $('#icTheme').innerHTML = (t === 'dark') ? IC.moon : IC.sun;
  $('#themeLabel').textContent = (t === 'dark') ? '暗色' : '亮色';
  try { localStorage.setItem('wb2a-theme', t); } catch(e){}
}
$('#btnTheme').addEventListener('click', function(){
  setTheme(document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark');
});
(function(){
  var t = 'dark';
  try { var v = localStorage.getItem('wb2a-theme'); if (v) t = v; } catch(e){}
  setTheme(t);
})();

/* ── 通讯 ─────────────────────────────────────────── */
async function api(path, body){
  var opt = { headers:{ 'Content-Type':'application/json' }, cache:'no-store' };
  // 写操作要带管理标记：面板自身的同源校验据此拒绝第三方页面代发请求。
  if (path.indexOf('api/keys') === 0 || path.indexOf('api/update/') === 0){
    opt.headers['X-Admin-Request'] = '1';
  }
  if (body !== undefined){ opt.method = 'POST'; opt.body = JSON.stringify(body || {}); }
  var r = await fetch(path, opt);
  if (r.status === 401){ location.replace('login'); throw new Error('登录已失效'); }
  var data = null;
  try{ data = await r.json(); }catch(e){ data = null; }
  if (!r.ok){
    var msg = (data && (data.message || (data.error && data.error.message))) ||
      ('HTTP ' + r.status);
    var err = new Error(msg);
    err.status = r.status;
    throw err;
  }
  return data;
}
function toast(msg, kind){
  var d = document.createElement('div');
  d.className = 'toast ' + (kind || '');
  var icon = kind === 'ok' ? IC.check : (kind === 'err' ? IC.warn : IC.clock);
  d.innerHTML = svg(icon, 15) + '<div>' + esc(msg) + '</div>';
  $('#toasts').appendChild(d);
  setTimeout(function(){ d.style.transition = 'opacity .25s'; d.style.opacity = '0';
    setTimeout(function(){ d.remove(); }, 260); }, 4200);
}

/* ── 视图 ─────────────────────────────────────────── */
var PAGE = {
  overview:{ t:'概览', d:'账号池与运行状态' },
  accounts:{ t:'账号', d:'凭证、积分与启停' },
  keys:    { t:'密钥管理', d:'创建与管理客户端的访问密钥' },
  usage:   { t:'用量统计', d:'按 API key 累计的 token 用量' },
  tasks:   { t:'排程', d:'定时任务开关与手动触发' },
  logs:    { t:'请求日志', d:'每次请求一行，含调用密钥、账号与耗时' },
  system:  { t:'系统', d:'服务状态、登录账号与运行日志' }
};
var sysLoaded = false;
function go(v){
  if (!PAGE[v]) v = 'overview';
  $$('.view[data-view]').forEach(function(p){
    p.classList.toggle('hide', p.getAttribute('data-view') !== v);
  });
  $$('#tabs button, #tabs2 button').forEach(function(b){
    b.classList.toggle('on', b.getAttribute('data-view') === v);
  });
  $('#pgTitle').textContent = PAGE[v].t;
  $('#pgDesc').textContent = PAGE[v].d;
  document.title = PAGE[v].t + ' · workbuddy2api';
  try { localStorage.setItem('wb2a-view', v); } catch(e){}
  history.replaceState(null, '', '#' + v);
  closeNav();
  if (v === 'keys' && typeof loadKeys === 'function') loadKeys();
  if (v === 'usage' && typeof loadUsage === 'function') loadUsage();
  // 更新卡片不依赖 /api/state，先拉它：即使系统页的数据还没到也不会漏掉加载。
  if (v === 'system' && typeof loadUpdate === 'function') loadUpdate();
  if (v === 'system' && !sysLoaded){ sysLoaded = true; renderSystem(); }
  if (v !== 'system' && typeof stopUpdatePoll === 'function') stopUpdatePoll();
  // 日志页：进入即拉一次，并按开关状态维持自动刷新；离开即停，避免后台空转。
  if (v === 'logs'){ loadLogs(); startLogAuto(); } else { stopLogAuto(); }
}
document.addEventListener('click', function(e){
  var b = e.target.closest('button[data-view]');
  if (b) go(b.getAttribute('data-view'));
});
$('#btnHome').addEventListener('click', function(){ go('overview'); });
$('#btnGoTasks').addEventListener('click', function(){ go('tasks'); });

/* ── 窄屏导航 ─────────────────────────────────────── */
function openNav(){ $('#side').classList.add('open'); $('#scrimNav').classList.add('on'); }
function closeNav(){ $('#side').classList.remove('open'); $('#scrimNav').classList.remove('on'); }
$('#btnMenu').addEventListener('click', openNav);
$('#scrimNav').addEventListener('click', closeNav);

/* ── 忙碌 ─────────────────────────────────────────── */
var busyDepth = 0;
function busy(on){
  busyDepth = Math.max(0, busyDepth + (on ? 1 : -1));
  var b = busyDepth > 0;
  ['#btnRefresh','#btnRestart','#btnAdd','#btnTaskReload','#btnLogs'].forEach(function(s){
    var el = $(s); if (el) el.disabled = b;
  });
}

/* ── 加载 ─────────────────────────────────────────── */
async function loadAll(forceCredit){
  busy(true);
  try{
    var res = await Promise.all([
      api('api/state' + (forceCredit ? '?refresh_credit=1' : '')),
      api('api/tasks').catch(function(){ return null; })
    ]);
    S.data = res[0];
    S.tasks = res[1];
    render();
    svc(true);
  }catch(err){
    svc(false, String(err && err.message || err));
    toast('加载失败：' + (err && err.message || err), 'err');
  }finally{ busy(false); }
}
function svc(up){
  var b = $('#svcBadge'), t = $('#svcText');
  var running = S.data && S.data.containerRunning;
  if (!up){ b.className = 'chip e'; t.textContent = '面板不可达'; return; }
  if (running){ b.className = 'chip ok'; t.textContent = '网关运行中'; }
  else { b.className = 'chip e'; t.textContent = '容器未运行'; }
}
function taskList(){
  return (S.tasks && S.tasks.available && Array.isArray(S.tasks.tasks)) ? S.tasks.tasks : [];
}
function badge(st){ return '<span class="bdg ' + st.cls + '"><i></i>' + st.text + '</span>'; }
function mrow(k, v, cls){
  return '<div class="mrow"><span class="k">' + esc(k) + '</span>' +
    '<span class="v ' + (cls || '') + '">' + esc(v) + '</span></div>';
}
function emptyBox(icon, title, sub){
  return '<div class="empty">' + svg(icon, 30) + '<div class="empty-t">' + esc(title) + '</div>' +
    (sub ? '<div class="empty-s">' + esc(sub) + '</div>' : '') + '</div>';
}

/* ── 渲染 ─────────────────────────────────────────── */
function render(){
  var d = S.data; if (!d) return;
  if (d.service) $('#brandVer').textContent = d.service + ' · v1';
  var a = d.accounts || [], tasks = taskList();
  $('#tabAcct').textContent = a.length;
  $('#tabTask').textContent = tasks.length ? tasks.length : '—';
  renderNotes();
  renderStrip();
  renderMetrics();
  renderPool();
  renderCredit();
  renderOvTasks();
  renderAccounts();
  renderTasks();
  renderSystem();
}

function metric(k, value, unit, sub, cls, pct, icon){
  return '<div class="metric">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + svg(icon, 15) + '<b>' + value + '</b>' +
    (unit ? '<small>' + esc(unit) + '</small>' : '') + '</div>' +
    (typeof pct === 'number'
      ? '<div class="bar"><i class="' + (cls || '') + '" style="width:' +
        Math.max(0, Math.min(100, pct)) + '%"></i></div>'
      : '') +
    (sub ? '<div class="s ' + (cls || '') + '">' + esc(sub) + '</div>' : '') +
    '</div>';
}

function renderMetrics(){
  var d = S.data, a = d.accounts || [], p = d.pool || {};
  var on = a.filter(function(x){ return !x.disabled; }).length;
  var total = Number(p.total) || 0, healthy = Number(p.healthy) || 0;
  var cooling = Number(p.cooling) || 0, dis = Number(p.disabled) || 0;
  var runs = taskList().filter(function(t){ return t.running; }).length;
  var have = a.filter(function(x){ return x.credits && typeof x.credits.remain === 'number'; });
  var remain = have.reduce(function(s, x){ return s + x.credits.remain; }, 0);
  var size = have.reduce(function(s, x){
    return s + (typeof x.credits.size === 'number' ? x.credits.size : x.credits.remain); }, 0);
  var pct = size ? Math.round(remain / size * 100) : 0;
  var hpct = total ? Math.round(healthy / total * 100) : 0;

  $('#metrics').innerHTML = [
    metric('账号', a.length, on + ' 启用', (a.length - on) + ' 个停用',
      (a.length - on) ? 'w' : 'ok', null, IC.users),
    metric('账号池', healthy + ' / ' + total, '健康',
      cooling ? (cooling + ' 个冷却中') : (dis ? (dis + ' 个停用') : '全部可用'),
      cooling ? 'w' : (dis ? 'e' : 'ok'), hpct, IC.pulse),
    metric('可用积分', num(remain), '剩余',
      have.length + ' / ' + a.length + ' 个账号取到',
      pct < 20 ? 'e' : (pct < 50 ? 'w' : 'ok'), pct, IC.coins),
    metric('排程', taskList().length ? String(taskList().length) : '—', '个任务',
      runs ? (runs + ' 个执行中') : '全部空闲', runs ? 'w' : '', null, IC.clock)
  ].join('');
}

function renderNotes(){
  var d = S.data, out = [];
  var off = (d.accounts || []).filter(function(a){ return a.disabled; });
  if (d.pendingRestart){
    out.push(note('w', '账号变更待生效',
      '有 ' + off.length + ' 个账号已停用但仍在池中，重启网关后生效。',
      { label:'立即重启', act:'restart' }));
  }
  if (!d.containerRunning){
    out.push(note('e', '网关容器未运行', '服务当前不可用，账号操作与积分查询都会失败。', null));
  }
  if (d.creditError){
    out.push(note('w', '积分数据不可用', String(d.creditError), null));
  }
  if (S.tasks && S.tasks.available === false){
    out.push(note('a', '排程未接入', '当前网关进程没有加载排程器。', null));
  }
  $('#notes').innerHTML = out.join('');
}
function note(kind, title, body, btn){
  var cls = kind === 'e' ? 'e' : (kind === 'w' ? 'w' : 'a');
  var icon = kind === 'a' ? IC.clock : IC.warn;
  return '<div class="note ' + cls + '">' + svg(icon, 16) +
    '<div class="g"><b>' + esc(title) + '</b> ' + esc(body) + '</div>' +
    (btn ? '<button class="btn sm" data-note-act="' + esc(btn.act) + '">' + esc(btn.label) + '</button>' : '') +
    '</div>';
}
$('#notes').addEventListener('click', function(e){
  var b = e.target.closest('[data-note-act]');
  if (b && b.getAttribute('data-note-act') === 'restart') actRestart();
});

function setCell(sel, text, copy){
  var el = $(sel), code = el.querySelector('code'), btn = el.querySelector('.mini');
  code.textContent = text;
  code.title = text;
  if (copy){ btn.disabled = false; btn.setAttribute('data-copy', copy); }
  else { btn.disabled = true; btn.removeAttribute('data-copy'); }
}
function renderStrip(){
  var base = location.origin + '/v1';
  var health = location.origin + '/healthz';
  setCell('#cellBase', base, base);
  setCell('#cellKey', '在密钥页管理', '');
  $('#cellKey .mini').disabled = false;
  setCell('#cellHealth', health, health);
}

function renderPool(){
  var p = S.data.pool || {};
  var total = p.total || 0, healthy = p.healthy || 0, cooling = p.cooling || 0, dis = p.disabled || 0;
  var off = Math.max(0, total - healthy - cooling - dis);
  function w(n){ return total ? (n / total * 100) : 0; }
  var html = '<div class="big"><span class="v">' + healthy + '</span><span class="u">/ ' + total + ' 健康</span></div>' +
    '<div class="comp">' +
      '<i class="a" style="width:' + w(healthy) + '%"></i>' +
      '<i class="b" style="width:' + w(cooling) + '%"></i>' +
      '<i class="c" style="width:' + w(dis + off) + '%"></i>' +
    '</div>' +
    mrow('池内总数', String(total)) +
    mrow('冷却中', String(cooling), cooling ? 'w' : '') +
    mrow('已停用', String(dis), dis ? 'w' : '') +
    mrow('并发占满', String(p.inFlightFull || 0)) +
    mrow('粘性会话', String(p.stickySessions || 0));
  var rt = p.realmTotals || {};
  Object.keys(rt).forEach(function(k){
    var v = rt[k], label = (k === 'cn' ? '国内' : (k === 'global' ? '国际' : k));
    if (v && typeof v === 'object'){
      var tot = Number(v.total) || 0, ok = Number(v.healthy) || 0;
      html += mrow(label + '可用', ok + ' / ' + tot, tot && ok === tot ? 'ok' : (ok ? '' : 'e'));
    } else {
      html += mrow(label + '可用', String(v));
    }
  });
  $('#poolPanel').innerHTML = html;
}

function renderCredit(){
  var d = S.data, a = d.accounts || [];
  var have = a.filter(function(x){ return x.credits && typeof x.credits.remain === 'number'; });
  var remain = have.reduce(function(s, x){ return s + x.credits.remain; }, 0);
  var size = have.reduce(function(s, x){
    return s + (typeof x.credits.size === 'number' ? x.credits.size : x.credits.remain); }, 0);
  var used = Math.max(0, size - remain);
  var pct = size ? Math.round(remain / size * 100) : 0;
  var html = '<div class="big"><span class="v">' + num(remain) + '</span><span class="u">剩余</span></div>' +
    '<div class="comp">' +
      '<i class="a" style="width:' + pct + '%"></i>' +
      '<i class="c" style="width:' + Math.max(0, 100 - pct) + '%"></i>' +
    '</div>' +
    mrow('已用', num(used)) +
    mrow('总额度', num(size)) +
    mrow('取到积分', have.length + ' / ' + a.length);
  $('#creditPanel').innerHTML = html;
}

function taskState(t){
  if (!t) return { cls:'n', text:'未知' };
  if (!t.enabled) return { cls:'n', text:'已停用' };
  if (t.running) return { cls:'a', text:'执行中' };
  return { cls:'ok', text:'已启用' };
}
function sortTasks(list){
  return list.slice().sort(function(a, b){
    var ha = (a.hours && a.hours[0]) || 99, hb = (b.hours && b.hours[0]) || 99;
    return ha - hb;
  });
}
function renderOvTasks(){
  var list = taskList();
  if (!list.length){
    $('#ovTasks').innerHTML = emptyBox(IC.clock, '暂无排程任务', '网关进程没有加载排程器');
    return;
  }
  $('#ovTasks').innerHTML = sortTasks(list).map(function(t){
    var st = taskState(t);
    var time = (t.hours && t.hours.length) ? (pad(t.hours[0]) + ':00') : '—';
    return '<div class="tl-row">' +
      '<div class="tl-time">' + time + '</div>' +
      '<div class="tl-main"><div class="tl-nm">' + esc(t.name) + '</div>' +
      '<div class="tl-meta">下次 ' + esc(fmtTime(t.next_run)) + ' · 上次 ' + esc(relTime(t.last_run)) + '</div></div>' +
      '<div class="tl-acts">' + badge(st) +
      '<button class="btn sm" data-run="' + esc(t.key) + '">' + svg(IC.play, 11) + '跑一次</button>' +
      '<button class="btn sm ico ghost" data-log="' + esc(t.key) + '" data-name="' + esc(t.name) + '" title="日志">' +
      svg(IC.eye, 13) + '</button></div></div>';
  }).join('');
}

function acctState(a){
  if (a.disabled) return { cls:'n', text:'已停用' };
  var p = a.pool || {};
  if (!p.inPool) return { cls:'w', text:'不在池中' };
  if (p.cooling) return { cls:'w', text:'冷却中' };
  if (!p.healthy) return { cls:'e', text:'异常' };
  return { cls:'ok', text:'正常' };
}
function renderAccounts(){
  var all = S.data.accounts || [];
  var list = all.slice();
  var q = S.q.trim().toLowerCase();
  if (S.filter === 'on') list = list.filter(function(a){ return !a.disabled; });
  if (S.filter === 'off') list = list.filter(function(a){ return a.disabled; });
  if (q) list = list.filter(function(a){
    return (a.nickname || '').toLowerCase().indexOf(q) >= 0 ||
           (a.uid || '').toLowerCase().indexOf(q) >= 0;
  });
  if (!list.length){
    $('#acctRows').innerHTML = '<tr><td colspan="5" data-l="">' +
      emptyBox(IC.users, '没有匹配的账号', all.length ? '换个关键词或筛选条件' : '用右上角添加账号') +
      '</td></tr>';
    return;
  }
  $('#acctRows').innerHTML = list.map(function(a){
    var st = acctState(a), c = a.credits || {}, p = a.pool || {};
    var left = daysLeft(a.expiresAt);
    var creditCell = (typeof c.remain === 'number')
      ? '<div class="cred"><span class="v">' + num(c.remain) + '</span>' +
        '<span class="m">已用 ' + num(c.used) + ' / ' + num(c.size) + '</span>' +
        (typeof c.size === 'number' && c.size > 0
          ? '<span class="bar"><i style="width:' + Math.min(100, Math.round(c.remain / c.size * 100)) + '%"></i></span>'
          : '') + '</div>'
      : '<span style="color:var(--text3)">—</span>';
    var lastTxt = p.lastSuccess ? relTime(p.lastSuccess) : '无记录';
    var lastSub = (p.lastErr && String(p.lastErr).indexOf('0001') !== 0) ? ('错误 ' + relTime(p.lastErr))
      : (p.breakerFails ? ('连错 ' + p.breakerFails + ' 次') : (p.inFlight ? ('并发 ' + p.inFlight) : ''));
    return '<tr>' +
      '<td data-l="账号"><div class="who">' + esc(a.nickname || '') + '</div>' +
      '<div class="sub mono">' + esc(shortUid(a.uid)) + ' · ' + esc(String(a.realm || '').toUpperCase()) + '</div></td>' +
      '<td data-l="状态"><span class="cell-in">' + badge(st) +
      (left != null ? '<span class="sub">' + (left > 0 ? ('剩 ' + left + ' 天') : '已过期') + '</span>' : '') +
      '</span></td>' +
      '<td class="r" data-l="积分"><span class="cred">' + creditCell + '</span></td>' +
      '<td data-l="最近活动"><span class="cell-in">' + esc(lastTxt) +
      (lastSub ? '<span class="sub">' + esc(lastSub) + '</span>' : '') + '</span></td>' +
      '<td class="acts">' +
      '<button class="btn sm" data-toggle="' + esc(a.uid) + '" data-disabled="' + (a.disabled ? '1' : '0') + '">' +
      (a.disabled ? '启用' : '停用') + '</button>' +
      '<button class="btn sm ico ghost dgr" data-del="' + esc(a.uid) + '" data-name="' + esc(a.nickname || '') +
      '" title="删除">' + svg(IC.trash, 13) + '</button></td></tr>';
  }).join('');
}
$('#acctSearch').addEventListener('input', function(e){ S.q = e.target.value; renderAccounts(); });
$('#acctFilter').addEventListener('click', function(e){
  var b = e.target.closest('button[data-f]'); if (!b) return;
  S.filter = b.getAttribute('data-f');
  $$('#acctFilter button').forEach(function(x){
    x.setAttribute('aria-pressed', String(x === b));
  });
  renderAccounts();
});
$('#acctRows').addEventListener('click', function(e){
  var t = e.target.closest('[data-toggle]');
  if (t) return actToggle(t.getAttribute('data-toggle'), t.getAttribute('data-disabled') === '0');
  var d = e.target.closest('[data-del]');
  if (d) return actDelete(d.getAttribute('data-del'), d.getAttribute('data-name'));
});

function renderTasks(){
  var list = taskList();
  if (!list.length){
    $('#taskList').innerHTML = emptyBox(IC.clock, '暂无排程任务',
      (S.tasks && S.tasks.error) || '网关进程没有加载排程器');
    return;
  }
  $('#taskList').innerHTML = sortTasks(list).map(function(t){
    var st = taskState(t);
    var hours = (t.hours || []).map(function(h){
      return '<span class="hour">' + pad(h) + ':00</span>'; }).join('');
    return '<div class="tile">' +
      '<div class="tile-t">' + badge(st) + '<span class="sp"></span>' +
      '<button class="sw" role="switch" data-task="' + esc(t.key) + '" aria-checked="' +
      (t.enabled ? 'true' : 'false') + '"' + (t.running ? ' disabled' : '') +
      ' aria-label="' + esc(t.name) + ' 开关"></button></div>' +
      '<div><div class="tl-nm">' + esc(t.name) + '</div>' +
      '<div class="detail">' + esc(t.detail || '') + '</div></div>' +
      '<div class="hours">' + hours + '</div>' +
      '<div class="tile-f">' +
      '<button class="btn sm" data-run="' + esc(t.key) + '">' + svg(IC.play, 11) + '跑一次</button>' +
      '<button class="btn sm ico ghost" data-log="' + esc(t.key) + '" data-name="' + esc(t.name) +
      '" title="日志">' + svg(IC.eye, 13) + '</button>' +
      '<span class="sp"></span><span class="meta">' + esc(fmtTime(t.next_run)) + '</span>' +
      '</div></div>';
  }).join('');
}

function renderSystem(){
  var d = S.data; if (!d) return;   // 直接打开 /#system（刷新或深链）时数据还没到，等 render() 再补
  var accounts = d.accounts || [];
  var cool = accounts.filter(function(a){ return a.pool && a.pool.cooling; }).length;
  var bad = accounts.filter(function(a){ return a.pool && a.pool.inPool && !a.pool.healthy; }).length;
  $('#sysCards').innerHTML =
    mrow('容器状态', d.containerRunning ? '运行中' : '已停止', d.containerRunning ? 'ok' : 'e') +
    mrow('容器名', 'workbuddy2api') +
    mrow('冷却账号', String(cool), cool ? 'w' : '') +
    mrow('异常账号', String(bad), bad ? 'e' : 'ok') +
    mrow('账号总数', String(accounts.length)) +
    mrow('排程任务', taskList().length ? String(taskList().length) : '—');
}

/* ── 交互委托 ─────────────────────────────────────── */
document.addEventListener('click', function(e){
  var r = e.target.closest('[data-run]');
  if (r){ actTaskRun(r.getAttribute('data-run')); return; }
  var g = e.target.closest('[data-log]');
  if (g){ openTaskLog(g.getAttribute('data-log'), g.getAttribute('data-name')); return; }
  var s = e.target.closest('[data-task]');
  if (s){ actTaskToggle(s.getAttribute('data-task'), s.getAttribute('aria-checked') !== 'true'); return; }
  var c = e.target.closest('[data-copy]');
  if (c) copyText(c.getAttribute('data-copy'));
});

function copyText(t){
  if (navigator.clipboard && navigator.clipboard.writeText){
    navigator.clipboard.writeText(t).then(function(){ toast('已复制', 'ok'); },
      function(){ toast('复制失败', 'err'); });
  } else {
    var ta = document.createElement('textarea');
    ta.value = t; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); toast('已复制', 'ok'); }
    catch(err){ toast('复制失败', 'err'); }
    ta.remove();
  }
}

/* ── 确认弹窗 ─────────────────────────────────────── */
function ask(title, text, okLabel, danger, cb){
  $('#mdTitle').textContent = title;
  $('#mdText').textContent = text;
  $('#mdIcon').className = 'mic ' + (danger ? 'e' : 'w');
  var yes = $('#mdYes');
  yes.textContent = okLabel;
  modalCb = cb;
  $('#scrimM').classList.add('on');
  $('#modal').classList.add('on');
  setTimeout(function(){ yes.focus(); }, 60);
}
function closeModal(){
  $('#scrimM').classList.remove('on');
  $('#modal').classList.remove('on');
  modalCb = null;
}
$('#mdNo').addEventListener('click', closeModal);
$('#scrimM').addEventListener('click', closeModal);
$('#mdYes').addEventListener('click', function(){
  var cb = modalCb; closeModal(); if (cb) cb();
});
document.addEventListener('keydown', function(e){
  if (e.key !== 'Escape') return;
  if ($('#modal').classList.contains('on')) closeModal();
  else if ($('#logModal').classList.contains('on')) closeLogModal();
  else if ($('#drawer').classList.contains('on')) closeDrawer();
});

/* ── 操作 ─────────────────────────────────────────── */
async function actToggle(uid, nextDisabled){
  busy(true);
  try{
    var r = await api('api/account/toggle', { uid:uid, disabled:nextDisabled });
    toast(r.message, r.ok ? 'ok' : 'err');
    if (r.ok) await loadAll();
  }catch(err){ toast('操作失败：' + (err && err.message || err), 'err'); }
  finally{ busy(false); }
}
function actDelete(uid, name){
  ask('删除账号', '将把「' + (name || shortUid(uid)) + '」移入 auths-trash 并重启网关，可手动从回收目录恢复。',
    '删除', true, async function(){
      busy(true);
      try{
        var r = await api('api/account/delete', { uid:uid });
        toast(r.message, r.ok ? 'ok' : 'err');
        if (r.ok) await loadAll();
      }catch(err){ toast('删除失败：' + (err && err.message || err), 'err'); }
      finally{ busy(false); }
    });
}
function actRestart(){
  ask('重启网关', '重启期间服务中断约十秒，账号与配置改动会在此后生效。', '重启', false, async function(){
    busy(true);
    try{
      var r = await api('api/service/restart', {});
      toast(r.message, r.ok ? 'ok' : 'err');
      await loadAll(true);
    }catch(err){ toast('重启失败：' + (err && err.message || err), 'err'); }
    finally{ busy(false); }
  });
}
async function actTaskRun(key){
  busy(true);
  try{
    var r = await api('api/task/run', { key:key });
    toast(r.message, r.ok ? 'ok' : 'err');
    if (r.ok) setTimeout(function(){ loadAll(); }, 1200);
  }catch(err){ toast('触发失败：' + (err && err.message || err), 'err'); }
  finally{ busy(false); }
}
function actTaskToggle(key, nextEnabled){
  var t = taskList().filter(function(x){ return x.key === key; })[0] || {};
  ask((nextEnabled ? '启用' : '停用') + '任务',
    '「' + (t.name || key) + '」' + (nextEnabled ? '将恢复按点执行' : '将不再自动执行') + '，保存后重启网关生效。',
    nextEnabled ? '启用' : '停用', !nextEnabled, async function(){
      busy(true);
      try{
        var r = await api('api/task/toggle', { key:key, enabled:nextEnabled });
        toast(r.message, r.ok ? 'ok' : 'err');
        if (r.ok) await loadAll();
      }catch(err){ toast('操作失败：' + (err && err.message || err), 'err'); }
      finally{ busy(false); }
    });
}

/* ── 任务日志 ─────────────────────────────────────── */
function openLogModal(title){
  $('#lgTitle').textContent = title;
  $('#scrimL').classList.add('on');
  $('#logModal').classList.add('on');
}
function closeLogModal(){
  $('#scrimL').classList.remove('on');
  $('#logModal').classList.remove('on');
  logKey = null;
}
$('#lgClose').addEventListener('click', closeLogModal);
$('#scrimL').addEventListener('click', closeLogModal);
$('#lgRefresh').addEventListener('click', function(){ if (logKey) loadTaskLog(); });
async function openTaskLog(key, name){
  logKey = key; logName = name || key;
  openLogModal('任务日志 · ' + logName);
  $('#lgBody').textContent = '加载中…';
  await loadTaskLog();
}
async function loadTaskLog(){
  try{
    var r = await api('api/task/log?key=' + encodeURIComponent(logKey));
    var lines = r.lines || [];
    $('#lgBody').textContent = lines.length ? lines.join('\n') : (r.message || '这次运行还没有输出。');
  }catch(err){
    $('#lgBody').textContent = '拉取失败：' + (err && err.message || err);
  }
}

/* ── 请求日志（独立页，表格 + 自动刷新） ───────────── */
var LOG = { loading:false, timer:null, auto:false };

function logAutoText(){
  var b = $('#btnLogAuto');
  if (!b) return;
  b.textContent = '自动刷新：' + (LOG.auto ? '开' : '关');
  b.setAttribute('aria-pressed', String(LOG.auto));
}

function startLogAuto(){
  if (!LOG.auto || LOG.timer) return;
  LOG.timer = setInterval(function(){
    if (document.visibilityState === 'visible') loadLogs();
  }, 5000);
}

function stopLogAuto(){
  if (LOG.timer){ clearInterval(LOG.timer); LOG.timer = null; }
}

function statusClass(code){
  var n = parseInt(code, 10);
  if (!n || n >= 500) return 'st-err';
  if (n >= 400) return 'st-warn';
  return 'st-ok';
}

async function loadLogs(){
  if (LOG.loading) return;
  LOG.loading = true;
  var btn = $('#btnLogs');
  if (btn) btn.disabled = true;
  try{
    var r = await api('api/logs?lines=' + S.logLines);
    var rows = r.rows || [];
    if (!rows.length){
      $('#logRows').innerHTML = '<tr><td colspan="11"><div class="empty">还没有请求记录。</div></td></tr>';
    } else {
      $('#logRows').innerHTML = rows.slice().reverse().map(function(it){
        var cls = statusClass(it.status);
        return '<tr>' +
          '<td class="mono">#' + esc(it.seq) + '</td>' +
          '<td class="mono">' + esc(it.time) + '</td>' +
          '<td class="mono">' + esc(it.model) + '</td>' +
          '<td>' + esc(it.mode === 'stream' ? '流式' : '非流式') + '</td>' +
          '<td class="' + cls + '">' + esc(it.status) + '</td>' +
          '<td>' + esc(it.key) + '</td>' +
          '<td class="mono">' + esc(it.uid) + '</td>' +
          '<td class="mono num">' + esc(it.ttfb) + '</td>' +
          '<td class="mono num">' + esc(it.tok) + '</td>' +
          '<td class="mono num">' + esc(it.rate) + '</td>' +
          '<td class="mono num">' + esc(it.total) + '</td>' +
          '</tr>';
      }).join('');
    }
    var count = $('#logCount'); if (count) count.textContent = String(rows.length);
    var updated = $('#logUpdated');
    if (updated){
      var now = new Date();
      function p(x){ return x < 10 ? '0' + x : '' + x; }
      updated.textContent = '更新于 ' + p(now.getHours()) + ':' + p(now.getMinutes()) + ':' + p(now.getSeconds());
    }
    $('#logBox').textContent = (r.other && r.other.length) ? r.other.join('\n') : '（无警告与错误）';
    var note = $('#logError');
    if (note){
      if (r.ok){ note.classList.add('hide'); }
      else { note.textContent = '读取容器日志失败，请检查面板是否有 docker 权限。'; note.classList.remove('hide'); }
    }
  }catch(err){
    var note = $('#logError');
    if (note){ note.textContent = '拉取失败：' + (err && err.message || err); note.classList.remove('hide'); }
  }finally{
    LOG.loading = false;
    if (btn) btn.disabled = false;
  }
}

$('#logLines').addEventListener('click', function(e){
  var b = e.target.closest('button[data-n]'); if (!b) return;
  S.logLines = parseInt(b.getAttribute('data-n'), 10) || 120;
  $$('#logLines button').forEach(function(x){
    x.setAttribute('aria-pressed', String(x === b));
  });
  loadLogs();
});
$('#btnLogAuto').addEventListener('click', function(){
  LOG.auto = !LOG.auto;
  logAutoText();
  if (LOG.auto){ startLogAuto(); loadLogs(); } else { stopLogAuto(); }
});
logAutoText();

/* ── 添加账号 ─────────────────────────────────────── */
function openDrawer(){
  $('#scrimD').classList.add('on');
  $('#drawer').classList.add('on');
  renderPick();
}
function closeDrawer(){
  stopPoll();
  $('#scrimD').classList.remove('on');
  $('#drawer').classList.remove('on');
  DT.url = '';
}
$('#drClose').addEventListener('click', closeDrawer);
$('#scrimD').addEventListener('click', closeDrawer);
$('#btnAdd').addEventListener('click', openDrawer);

function renderPick(){
  DT.url = '';
  $('#drTitle').textContent = '添加账号';
  $('#drBody').innerHTML =
    '<div class="mtext">扫码完成授权后，凭证会写入 auths 并重启网关。</div>' +
    '<div style="margin-top:16px"><div class="sub" style="margin-bottom:7px">区域</div>' +
    '<div class="seg grow" id="segRealm">' +
    '<button data-r="cn" aria-pressed="' + (DT.realm === 'cn' ? 'true' : 'false') + '">国内版 CN</button>' +
    '<button data-r="global" aria-pressed="' + (DT.realm === 'global' ? 'true' : 'false') + '">国际版 Global</button>' +
    '</div></div>';
  $('#drFoot').innerHTML = '<button class="btn pri" id="drGo">生成授权链接</button>';
  $('#segRealm').addEventListener('click', function(e){
    var b = e.target.closest('button[data-r]'); if (!b) return;
    DT.realm = b.getAttribute('data-r');
    $$('#segRealm button').forEach(function(x){
      x.setAttribute('aria-pressed', String(x === b));
    });
  });
  $('#drGo').addEventListener('click', startLogin);
}

async function startLogin(){
  var go = $('#drGo'); go.disabled = true; go.textContent = '正在获取…';
  try{
    var r = await api('api/login/start', { realm:DT.realm });
    if (!r.ok){ toast(r.message, 'err'); go.disabled = false; go.textContent = '重新生成'; return; }
    DT.url = r.url;
    renderWait();
    startPoll();
  }catch(err){
    toast('获取链接失败：' + (err && err.message || err), 'err');
    go.disabled = false; go.textContent = '重新生成';
  }
}

function renderWait(){
  $('#drTitle').textContent = '扫码登录';
  $('#drBody').innerHTML =
    '<div class="qr-box"><canvas id="qr"></canvas></div>' +
    '<div class="url-box" id="urlText">' + esc(DT.url) + '</div>' +
    '<div class="wait"><span class="spin"></span><span id="waitMsg">等待扫码…</span></div>';
  $('#drFoot').innerHTML = '<button class="btn" id="drCancel">重新选择区域</button>' +
    '<button class="btn" id="drCopy">复制链接</button>';
  $('#drCancel').addEventListener('click', renderPick);
  $('#drCopy').addEventListener('click', function(){ copyText(DT.url); });
  drawQR(DT.url);
}

function drawQR(text){
  var cv = $('#qr'); if (!cv) return;
  try{
    var q = qrcode(0, 'M');
    q.addData(text); q.make();
    var n = q.getModuleCount(), pad2 = 2;
    var cell = Math.max(2, Math.floor(176 / (n + pad2 * 2)));
    var side = cell * (n + pad2 * 2);
    var dpr = window.devicePixelRatio || 1;
    cv.width = side * dpr; cv.height = side * dpr;
    cv.style.width = side + 'px'; cv.style.height = side + 'px';
    var ctx = cv.getContext('2d');
    ctx.scale(dpr, dpr);
    ctx.fillStyle = '#fff'; ctx.fillRect(0, 0, side, side);
    ctx.fillStyle = '#111214';
    for (var r = 0; r < n; r++)
      for (var c = 0; c < n; c++)
        if (q.isDark(r, c)) ctx.fillRect((c + pad2) * cell, (r + pad2) * cell, cell, cell);
  }catch(err){
    cv.parentNode.innerHTML = '<div class="sub" style="padding:56px 0">二维码生成失败，请用下方链接打开</div>';
  }
}
function setWait(msg){ var w = $('#waitMsg'); if (w) w.textContent = msg; }
function stopPoll(){ if (DT.timer){ clearInterval(DT.timer); DT.timer = null; } }
function startPoll(){
  stopPoll(); DT.tries = 0;
  DT.timer = setInterval(async function(){
    if (DT.tries++ > 100){ stopPoll(); setWait('等待超时，请重新生成链接。'); return; }
    try{
      var r = await api('api/login/poll', { realm:DT.realm });
      if (r.ok){ stopPoll(); renderDone(r); return; }
      if (r.pending && DT.tries % 3 === 0) setWait('等待扫码…');
    }catch(err){ /* 单次失败继续等 */ }
  }, 3000);
}
function renderDone(r){
  var a = r.account || {};
  $('#drTitle').textContent = '添加成功';
  $('#drBody').innerHTML =
    '<div class="note ok">' + svg(IC.check, 16) +
    '<div class="g"><b>' + esc(a.nickname || '账号') + '</b> 已写入并加载</div></div>' +
    '<div style="margin-top:14px">' +
    mrow('UID', a.uid || '—') +
    mrow('区域', String(a.realm || '').toUpperCase()) +
    mrow('有效期', a.expiresInDays ? (a.expiresInDays + ' 天') : '—') +
    '</div>';
  $('#drFoot').innerHTML = '<button class="btn" id="drMore">再加一个</button>' +
    '<button class="btn pri" id="drDone">完成</button>';
  $('#drDone').addEventListener('click', async function(){ closeDrawer(); await loadAll(true); });
  $('#drMore').addEventListener('click', renderPick);
  loadAll(true);
}

/* ── 顶栏动作 ─────────────────────────────────────── */
$('#btnRefresh').addEventListener('click', function(){ loadAll(true); if (location.hash === '#keys') loadKeys(); });
$('#btnRestart').addEventListener('click', actRestart);
$('#btnTaskReload').addEventListener('click', async function(){
  await loadAll(); toast('已刷新', 'ok');
});
$('#btnLogs').addEventListener('click', loadLogs);
$('#btnLogout').addEventListener('click', function(){
  ask('退出登录', '将结束本机的登录会话，需要重新输入用户名和密码。', '退出', false, async function(){
    try{ await api('api/auth/logout', {}); }catch(e){}
    location.replace('login');
  });
});

/* ── 登录账号 ─────────────────────────────────────── */
async function loadIdentity(){
  try{
    var r = await api('api/session');
    $('#whoami').textContent = r.username || '';
    $('#pwUser').value = r.username || '';
    $('#pwHint').textContent = r.inherited
      ? '沿用 nginx 时期的密码，保存一次即切换成本面板自管'
      : '保存后所有设备都需要重新登录';
  }catch(err){ /* 401 已由 api() 跳转登录页 */ }
}
$('#btnPwSave').addEventListener('click', async function(){
  var cur = $('#pwCur').value, user = $('#pwUser').value.trim();
  var pw = $('#pwNew').value, cfm = $('#pwCfm').value;
  if (!cur){ toast('请输入当前密码', 'err'); $('#pwCur').focus(); return; }
  if (!user){ toast('请输入新用户名', 'err'); $('#pwUser').focus(); return; }
  if (pw.length < 8){ toast('新密码至少 8 位', 'err'); $('#pwNew').focus(); return; }
  if (pw !== cfm){ toast('两次输入的新密码不一致', 'err'); $('#pwCfm').focus(); return; }
  var btn = $('#btnPwSave');
  btn.disabled = true;
  try{
    var r = await api('api/auth/password',
      { current:cur, username:user, password:pw, confirm:cfm });
    toast(r.message || '已保存', 'ok');
    $('#pwCur').value = ''; $('#pwNew').value = ''; $('#pwCfm').value = '';
    $('#whoami').textContent = r.username || user;
  }catch(err){
    toast('保存失败：' + (err && err.message || err), 'err');
  }finally{ btn.disabled = false; }
});

/* ── 启动 ─────────────────────────────────────────── */
function init(){
  var v = 'overview';
  try { var s = localStorage.getItem('wb2a-view'); if (s && PAGE[s]) v = s; } catch(e){}
  var hashView = location.hash.slice(1); if (PAGE[hashView]) v = hashView;
  go(v);
  loadAll();
  loadIdentity();
  setInterval(function(){
    if (!document.hidden && !DT.timer) loadAll();
  }, 30000);
}

// app.js 与 keys.js / usage.js 拼在同一个脚本里，后者的顶层状态（KS / US / LOG）
// 要等各自那一段执行完才赋值。init() 里的 go() 会直接调用这些页面的加载函数，
// 所以必须等整个脚本跑完再启动——否则从 #usage 进入时 US 还是 undefined，
// 页面会永远停在「正在加载用量…」。
if (document.readyState === 'loading'){
  document.addEventListener('DOMContentLoaded', init);
} else {
  init();
}
