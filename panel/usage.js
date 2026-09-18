"use strict";
// ═══ 更新日志 ═══
// 2026-09-17：新增用量统计页：总量卡片 + 按密钥明细 + 单密钥模型拆分，
//             数据经本机管理通道读取网关记账（/usage）。
// 2026-09-18：输入卡片补「其中缓存命中」说明并新增缓存命中卡片：思考模式每轮重发整段
//             上下文，输入里大头是缓存命中；不单列会让人误以为"明明用得多却只记了这么点"。
// 2026-09-18：卡片加指标图标、页头加日期筛选（今天 / 近 7 天 / 近 30 天 / 全部 + 具体某天），
//             数据源是账本新增的按天分桶（/usage 的 days 与每个密钥的 days）。

// US.from / US.to 是筛选区间的起止日（YYYY-MM-DD，含当日）；两者都空表示「全部」。
// 单日筛选就是 from == to。账本按天分桶，区间求和即可。
var US = {data: null, loading: false, error: '', open: {}, from: '', to: '', label: '全部'};

function compactTokens(value){
  var n = Number(value) || 0;
  if (n >= 1000000000) return (n / 1000000000).toFixed(2) + 'B';
  if (n >= 1000000) return (n / 1000000).toFixed(2) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
  return String(n);
}

function exactTokens(value){
  var n = Number(value) || 0;
  return n.toLocaleString('zh-CN');
}

function usageTime(value){
  if (!value) return '—';
  var d = new Date(value);
  if (isNaN(d.getTime())) return '—';
  function pad(x){ return x < 10 ? '0' + x : '' + x; }
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
    ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}

function renderUsageTiles(totals){
  var t = totals || {};
  var cached = Number(t.cached_tokens) || 0;
  var prompt = Number(t.prompt_tokens) || 0;
  var promptNote = exactTokens(t.prompt_tokens) + ' tokens';
  if (cached > 0 && prompt > 0){
    promptNote += '（缓存命中 ' + Math.round(cached / prompt * 100) + '%）';
  }
  var tiles = [
    {k: '请求数', v: exactTokens(t.requests), s: '成功完成的调用', i: 'pulse'},
    {k: '合计 tokens', v: compactTokens(t.total_tokens), s: exactTokens(t.total_tokens) + ' tokens', i: 'sum'},
    {k: '输入 tokens', v: compactTokens(t.prompt_tokens), s: promptNote, i: 'in'},
    {k: '输出 tokens', v: compactTokens(t.completion_tokens), s: exactTokens(t.completion_tokens) + ' tokens（含思考）', i: 'out'}
  ];
  if (cached > 0) tiles.push({k: '缓存命中输入', v: compactTokens(cached), s: exactTokens(cached) + ' tokens', i: 'zap'});
  if (t.credit) tiles.push({k: '上游计费', v: Number(t.credit).toFixed(2), s: 'usage.credit 累计', i: 'coins'});
  $('#usageTiles').innerHTML = tiles.map(function(tile){
    return '<div class="usage-tile">' +
      '<div class="k"><span class="uz-ico">' + usageIcon(tile.i) + '</span>' + esc(tile.k) + '</div>' +
      '<div class="v">' + esc(tile.v) + '</div>' +
      '<div class="s">' + esc(tile.s) + '</div></div>';
  }).join('');
}

// usageIcon：卡片与筛选器用的线条图标（与侧栏同一套描边风格）。
function usageIcon(name){
  var paths = {
    pulse: '<path d="M3 12h3.6l2.4-6 3.9 12 2.6-6H21"/>',
    sum:   '<path d="M5 5.5h14M5 12h14M5 18.5h9"/>',
    in:    '<path d="M12 3.5v11"/><path d="m7.5 10.5 4.5 4.5 4.5-4.5"/><path d="M4.5 20.5h15"/>',
    out:   '<path d="M12 20.5v-11"/><path d="m7.5 13.5 4.5-4.5 4.5 4.5"/><path d="M4.5 3.5h15"/>',
    zap:   '<path d="M13.5 3 5.5 13.5h5L9.5 21l8.5-11h-5.2L13.5 3Z"/>',
    coins: '<circle cx="12" cy="12" r="8.4"/><path d="M12 7.6v8.8M9.6 9.6h4.8M9.6 14.4h4.8"/>',
    clock: '<circle cx="12" cy="12" r="8.4"/><path d="M12 7.6V12l3 1.8"/>'
  };
  return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" ' +
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + (paths[name] || paths.sum) + '</svg>';
}

function renderUsageRows(keys, totals){
  var rows = keys || [];
  if (!rows.length){
    $('#usageRows').innerHTML = '<tr><td colspan="8" data-l=""><div class="empty">' +
      (filtered() ? '这个时间范围没有用量记录，换个日期或选「全部」看看。' : '还没有用量记录，客户端发一次请求后这里就有数据。') +
      '</div></td></tr>';
    return;
  }
  var max = 0;
  rows.forEach(function(item){ max = Math.max(max, Number(item.totals.total_tokens) || 0); });
  $('#usageRows').innerHTML = rows.map(function(item){
    var t = usageTotalsFor(item);
    var share = max > 0 ? Math.round((Number(t.total_tokens) || 0) / max * 100) : 0;
    var open = !!US.open[item.key_id];
    var models = usageModelsFor(item).map(function(model){
      return '<li><span>' + esc(model.model) + '</span><span>' + esc(compactTokens(model.totals.total_tokens)) + '</span></li>';
    }).join('');
    var name = item.name ? esc(item.name) : '未命名密钥';
    var mask = item.masked_key ? '<code>' + esc(item.masked_key) + '</code>' : '';
    return '<tr>' +
      '<td data-l="密钥"><div class="usage-key"><b>' + name + '</b>' + mask +
        '<div class="usage-bar"><i style="width:' + share + '%"></i></div></div></td>' +
        '<td class="num" data-l="请求">' + esc(exactTokens(t.requests)) + '</td>' +
        '<td class="num" data-l="输入">' + esc(exactTokens(t.prompt_tokens)) + '</td>' +
        '<td class="num" data-l="缓存命中">' + esc(exactTokens(t.cached_tokens)) + '</td>' +
        '<td class="num" data-l="输出">' + esc(exactTokens(t.completion_tokens)) + '</td>' +
      '<td class="num" data-l="合计">' + esc(exactTokens(t.total_tokens)) + '</td>' +
      '<td data-l="最近使用">' + esc(usageTime(usageLastUsedFor(item))) + '</td>' +
      '<td class="r" data-l="">' + (models
        ? '<button class="btn sm" data-usage-toggle="' + esc(item.key_id) + '">' + (open ? '收起' : '按模型') + '</button>'
        : '<span class="sub">—</span>') + '</td>' +
      '</tr>' + (open && models
        ? '<tr><td colspan="8"><ul class="usage-models">' + models + '</ul></td></tr>'
        : '');
  }).join('');
}

// selectedDays 当前筛选区间命中的天桶；区间两端都空表示「全部」。
function selectedDays(days){
  var list = days || [];
  if (!US.from && !US.to) return list;
  return list.filter(function(day){
    if (US.from && day.day < US.from) return false;
    if (US.to && day.day > US.to) return false;
    return true;
  });
}

// sumTotals 把若干天桶相加（字段与网关 Totals 一一对应）。
function sumTotals(days){
  var out = {requests:0, prompt_tokens:0, cached_tokens:0, completion_tokens:0, total_tokens:0, credit:0};
  (days || []).forEach(function(day){
    var t = (day && day.totals) || {};
    out.requests += Number(t.requests) || 0;
    out.prompt_tokens += Number(t.prompt_tokens) || 0;
    out.cached_tokens += Number(t.cached_tokens) || 0;
    out.completion_tokens += Number(t.completion_tokens) || 0;
    out.total_tokens += Number(t.total_tokens) || 0;
    out.credit += Number(t.credit) || 0;
  });
  return out;
}

// filtered 当前筛选是否有生效的日期区间。
function filtered(){
  return !!(US.from || US.to);
}

// usageTotalsFor 单密钥在筛选范围内的累计量。
// 账本没有天桶（升级前的旧数据）时退回总量，避免页面突然清零。
function usageTotalsFor(item){
  if (!filtered()) return item.totals || {};
  var days = item.days || [];
  if (!days.length) return {requests:0, prompt_tokens:0, cached_tokens:0, completion_tokens:0, total_tokens:0, credit:0};
  return sumTotals(selectedDays(days));
}

// usageLastUsedFor 筛选后仍显示该密钥的最后使用时间：范围包含今天时它就是真实值。
function usageLastUsedFor(item){
  if (!filtered()) return item.last_used_at;
  var totals = usageTotalsFor(item);
  return (Number(totals.requests) || 0) > 0 ? item.last_used_at : '';
}

// usageModelsFor 单密钥的模型拆分；筛选后按区间占比折算（天桶不记模型维度）。
function usageModelsFor(item){
  var models = item.models || [];
  if (!filtered() || !models.length) return models;
  var scopeTotals = usageTotalsFor(item);
  var allTotals = item.totals || {};
  var scale = (Number(allTotals.total_tokens) || 0) > 0
    ? (Number(scopeTotals.total_tokens) || 0) / Number(allTotals.total_tokens)
    : 0;
  return models.map(function(model){
    var totals = model.totals || {};
    return {model: model.model, totals: {
      requests: Math.round((Number(totals.requests) || 0) * scale),
      prompt_tokens: Math.round((Number(totals.prompt_tokens) || 0) * scale),
      cached_tokens: Math.round((Number(totals.cached_tokens) || 0) * scale),
      completion_tokens: Math.round((Number(totals.completion_tokens) || 0) * scale),
      total_tokens: Math.round((Number(totals.total_tokens) || 0) * scale),
      credit: (Number(totals.credit) || 0) * scale
    }};
  });
}

async function loadUsage(){
  if (US.loading) return;
  US.loading = true;
  var btn = $('#btnUsageReload');
  if (btn) btn.disabled = true;
  try{
    var result = await api('api/usage');
    US.data = result || {};
    US.error = '';
  }catch(error){
    US.error = error && error.message ? error.message : '加载失败';
  }finally{
    US.loading = false;
    if (btn) btn.disabled = false;
  }
  var note = $('#usageError');
  if (US.error){
    note.textContent = US.error;
    note.classList.remove('hide');
  } else if (!US.data || !US.data.ok){
    note.textContent = (US.data && US.data.message) || '用量账本未启用：在网关 config.json 里设置 usage_file 后重启容器。';
    note.classList.remove('hide');
  } else {
    note.classList.add('hide');
  }
  var payload = (US.data && US.data.ok) ? US.data : {totals: {}, keys: []};
  renderUsageFilter(payload.days);
  renderUsageTiles(usageTotalsForScope(payload));
  renderUsageRows(payload.keys, payload.totals);
  var since = $('#usageSince');
  if (since){
    var scope = filtered() ? '范围 ' + usageScopeLabel() : '全部时间';
    since.textContent = (payload.since
      ? '统计起始 ' + usageTime(payload.since) + '｜' + scope + '｜更新于 ' + usageTime(payload.updated_at)
      : '');
  }
  var foot = $('#usageFile');
  if (foot) foot.textContent = payload.file ? '账本文件：' + payload.file : '';
}

// usageTotalsForScope 顶部卡片的口径：选了区间就只统计范围内，否则用账本总量。
// 账本没有天桶（升级前的旧数据）时退回总量，不会突然显示 0。
function usageTotalsForScope(payload){
  if (!filtered()) return payload.totals || {};
  var days = payload.days || [];
  if (!days.length) return payload.totals || {};
  return sumTotals(selectedDays(days));
}

// usageScopeLabel 当前区间的可读描述（今天 / 近 7 天 / 某个具体日期 / 自定义范围）。
function usageScopeLabel(){
  if (!filtered()) return '全部时间';
  var today = localDayKey(new Date());
  if (US.from === today && US.to === today) return '今天';
  if (US.from === cutoffDayKey(6) && US.to === today) return '近 7 天';
  if (US.from === cutoffDayKey(29) && US.to === today) return '近 30 天';
  if (!US.from) return '截至 ' + US.to;
  if (!US.to) return US.from + ' 起';
  if (US.from === US.to) return US.from;
  return US.from + ' 至 ' + US.to;
}

// renderUsageFilter 日期筛选：快捷区间 + 具体日期选择 + 自定义起止。
//
// 只列「账本里真的有数据」的日期，避免选到空白天导致整页看着像坏了；
// 每个日期后面标出当天的请求数，方便直接判断哪天的量值得看。
function renderUsageFilter(days){
  var box = $('#usageFilter');
  if (!box) return;
  var list = days || [];
  var totalRequests = 0;
  list.forEach(function(day){ totalRequests += Number((day.totals || {}).requests) || 0; });

  var today = localDayKey(new Date());
  var week = cutoffDayKey(6);
  var month = cutoffDayKey(29);

  function sumSince(cutoff){
    var sum = 0;
    list.forEach(function(day){
      if (day.day >= cutoff) sum += Number((day.totals || {}).requests) || 0;
    });
    return sum;
  }

  // 快捷区间：按下时同时设置 from/to，语义与「选日期」共用一套状态。
  function rangeButton(from, to, label, count){
    var on = (US.from === from && US.to === to);
    return '<button type="button" data-usage-range="' + esc(from) + '|' + esc(to) + '"' +
      (on ? ' aria-pressed="true"' : '') + '>' +
      esc(label) + (count ? '<i>' + esc(exactTokens(count)) + ' 次</i>' : '') + '</button>';
  }

  var html = '';
  html += rangeButton(today, today, '今天', sumSince(today));
  html += rangeButton(week, today, '近 7 天', sumSince(week));
  html += rangeButton(month, today, '近 30 天', sumSince(month));
  html += rangeButton('', '', '全部', totalRequests);
  box.innerHTML = html;

  var picker = $('#usageDayPick');
  if (picker){
    var options = ['<option value="">选择某一天…</option>'];
    list.forEach(function(day){
      var requests = Number((day.totals || {}).requests) || 0;
      var on = (US.from === day.day && US.to === day.day);
      options.push('<option value="' + esc(day.day) + '"' + (on ? ' selected' : '') + '>' +
        esc(day.day) + '（' + esc(exactTokens(requests)) + ' 次）</option>');
    });
    picker.innerHTML = options.join('');
    picker.disabled = !list.length;
  }

  var fromInput = $('#usageFrom');
  var toInput = $('#usageTo');
  if (fromInput) fromInput.value = US.from || '';
  if (toInput) toInput.value = US.to || '';
}

// localDayKey 本机时区的日历日（与网关天桶同口径）。
function localDayKey(date){
  function p(x){ return x < 10 ? '0' + x : '' + x; }
  return date.getFullYear() + '-' + p(date.getMonth() + 1) + '-' + p(date.getDate());
}

// cutoffDayKey 从今天往前推 daysBack 天的日期串（近 N 天用）。
function cutoffDayKey(daysBack){
  var date = new Date();
  date.setDate(date.getDate() - daysBack);
  return localDayKey(date);
}

document.addEventListener('click', function(e){
  var toggle = e.target.closest('button[data-usage-toggle]');
  if (toggle){
    var id = toggle.getAttribute('data-usage-toggle');
    US.open[id] = !US.open[id];
    var payload = (US.data && US.data.ok) ? US.data : {keys: []};
    renderUsageRows(payload.keys, payload.totals);
    return;
  }
  // 快捷区间切换（今天 / 近 7 天 / 近 30 天 / 全部）：只重绘，不重新拉账本。
  var range = e.target.closest('button[data-usage-range]');
  if (range){
    var parts = String(range.getAttribute('data-usage-range') || '').split('|');
    US.from = parts[0] || '';
    US.to = parts[1] || '';
    applyUsageFilter();
  }
});

// applyUsageFilter 用当前区间重绘卡片与表格（数据已在内存里，无需再请求）。
function applyUsageFilter(){
  var payload = (US.data && US.data.ok) ? US.data : {totals: {}, keys: [], days: []};
  renderUsageFilter(payload.days);
  renderUsageTiles(usageTotalsForScope(payload));
  renderUsageRows(payload.keys, payload.totals);
  var since = $('#usageSince');
  if (since){
    var scope = filtered() ? '范围 ' + usageScopeLabel() : '全部时间';
    since.textContent = payload.since
      ? '统计起始 ' + usageTime(payload.since) + '｜' + scope + '｜更新于 ' + usageTime(payload.updated_at)
      : '';
  }
}

function usageInit(){
  var btn = $('#btnUsageReload');
  if (btn) btn.addEventListener('click', loadUsage);
  var picker = $('#usageDayPick');
  if (picker){
    picker.addEventListener('change', function(){
      US.from = picker.value || '';
      US.to = picker.value || '';
      applyUsageFilter();
    });
  }
  // 自定义起止：input[type=date] 改动即生效，两个都空等于「全部」。
  function bindRange(input, key){
    if (!input) return;
    input.addEventListener('change', function(){
      US[key] = input.value || '';
      // 起点晚于终点时把终点对齐过来，避免出现空区间。
      if (US.from && US.to && US.from > US.to){
        if (key === 'from') US.to = US.from;
        else US.from = US.to;
      }
      applyUsageFilter();
    });
  }
  bindRange($('#usageFrom'), 'from');
  bindRange($('#usageTo'), 'to');
}
usageInit();
