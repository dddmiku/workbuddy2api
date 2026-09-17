"use strict";
// ═══ 更新日志 ═══
// 2026-09-17：新增用量统计页：总量卡片 + 按密钥明细 + 单密钥模型拆分，
//             数据经本机管理通道读取网关记账（/usage）。

var US = {data: null, loading: false, error: '', open: {}};

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
  var tiles = [
    {k: '请求数', v: exactTokens(t.requests), s: '成功完成的调用'},
    {k: '合计 tokens', v: compactTokens(t.total_tokens), s: exactTokens(t.total_tokens) + ' tokens'},
    {k: '输入 tokens', v: compactTokens(t.prompt_tokens), s: exactTokens(t.prompt_tokens) + ' tokens'},
    {k: '输出 tokens', v: compactTokens(t.completion_tokens), s: exactTokens(t.completion_tokens) + ' tokens'}
  ];
  if (t.credit) tiles.push({k: '上游计费', v: String(t.credit), s: 'usage.credit 累计'});
  $('#usageTiles').innerHTML = tiles.map(function(tile){
    return '<div class="usage-tile"><div class="k">' + esc(tile.k) + '</div>' +
      '<div class="v">' + esc(tile.v) + '</div>' +
      '<div class="s">' + esc(tile.s) + '</div></div>';
  }).join('');
}

function renderUsageRows(keys, totals){
  var rows = keys || [];
  if (!rows.length){
    $('#usageRows').innerHTML = '<tr><td colspan="7"><div class="empty">还没有用量记录，客户端发一次请求后这里就有数据。</div></td></tr>';
    return;
  }
  var max = 0;
  rows.forEach(function(item){ max = Math.max(max, Number(item.totals.total_tokens) || 0); });
  $('#usageRows').innerHTML = rows.map(function(item){
    var t = item.totals || {};
    var share = max > 0 ? Math.round((Number(t.total_tokens) || 0) / max * 100) : 0;
    var open = !!US.open[item.key_id];
    var models = (item.models || []).map(function(model){
      return '<li><span>' + esc(model.model) + '</span><span>' + esc(compactTokens(model.totals.total_tokens)) + '</span></li>';
    }).join('');
    var name = item.name ? esc(item.name) : '未命名密钥';
    var mask = item.masked_key ? '<code>' + esc(item.masked_key) + '</code>' : '';
    return '<tr>' +
      '<td><div class="usage-key"><b>' + name + '</b>' + mask +
        '<div class="usage-bar"><i style="width:' + share + '%"></i></div></div></td>' +
      '<td class="num">' + esc(exactTokens(t.requests)) + '</td>' +
      '<td class="num">' + esc(exactTokens(t.prompt_tokens)) + '</td>' +
      '<td class="num">' + esc(exactTokens(t.completion_tokens)) + '</td>' +
      '<td class="num">' + esc(exactTokens(t.total_tokens)) + '</td>' +
      '<td>' + esc(usageTime(item.last_used_at)) + '</td>' +
      '<td class="r">' + (models
        ? '<button class="btn sm" data-usage-toggle="' + esc(item.key_id) + '">' + (open ? '收起' : '按模型') + '</button>'
        : '<span class="sub">—</span>') + '</td>' +
      '</tr>' + (open && models
        ? '<tr><td colspan="7"><ul class="usage-models">' + models + '</ul></td></tr>'
        : '');
  }).join('');
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
  renderUsageTiles(payload.totals);
  renderUsageRows(payload.keys, payload.totals);
  var since = $('#usageSince');
  if (since){
    since.textContent = payload.since
      ? '统计起始 ' + usageTime(payload.since) + '｜更新于 ' + usageTime(payload.updated_at)
      : '';
  }
  var foot = $('#usageFile');
  if (foot) foot.textContent = payload.file ? '账本文件：' + payload.file : '';
}

document.addEventListener('click', function(e){
  var toggle = e.target.closest('button[data-usage-toggle]');
  if (toggle){
    var id = toggle.getAttribute('data-usage-toggle');
    US.open[id] = !US.open[id];
    var payload = (US.data && US.data.ok) ? US.data : {keys: []};
    renderUsageRows(payload.keys, payload.totals);
  }
});

function usageInit(){
  var btn = $('#btnUsageReload');
  if (btn) btn.addEventListener('click', loadUsage);
}
usageInit();
