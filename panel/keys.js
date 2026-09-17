"use strict";
// ═══ 更新日志 ═══
// 2026-09-16：实现密钥创建、编辑、启停、删除与一次性显示，沿用控制台交互与主题。
// 2026-09-16：确认关闭时同步清空完整密钥，避免等待异步 close 事件才清除。
// 2026-09-17：密钥支持模型绑定：表单可填写或从模型列表挑选，列表展示绑定范围。

var KS = {keys:null, loading:false, error:'', query:'', models:null};
var KD = {id:null, secret:'', busy:false, copied:false, closeConfirmed:false};

function keyModels(value){
  return String(value || '').split(',').map(function(item){return item.trim();}).filter(function(item,index,all){
    return item && all.indexOf(item) === index;
  });
}

async function loadKeyModels(){
  if (KS.models || KS.modelsLoading) return KS.models || [];
  KS.modelsLoading = true;
  try{
    var result = await api('api/models');
    KS.models = (result && Array.isArray(result.models)) ? result.models : [];
  }catch(error){ KS.models = []; }
  finally{
    KS.modelsLoading = false;
    var pick = $('#keyModelPick');
    pick.innerHTML = '<option value="">从模型列表添加…</option>' + KS.models.map(function(id){
      return '<option value="' + esc(id) + '">' + esc(id) + '</option>';
    }).join('');
  }
  return KS.models;
}

function fillModelInput(models){
  $('#keyModels').value = models.join(', ');
}

async function loadKeys(){
  if (KS.loading) return;
  KS.loading = true;
  $('#btnKeysReload').disabled = true;
  try{
    var result = await api('api/keys');
    if (!result || !Array.isArray(result.keys)) throw new Error('服务返回的密钥列表不完整');
    KS.keys = result.keys; KS.error = '';
  }catch(error){ KS.error = error.message || '加载失败，请稍后重试'; }
  finally{ KS.loading = false; $('#btnKeysReload').disabled = false; renderKeys(); }
}

function renderKeys(){
  var keys = KS.keys || [];
  $('#keyError').textContent = KS.error;
  $('#keyError').classList.toggle('hide', !KS.error);
  $('#tabKeys').textContent = KS.keys ? keys.length : '—';
  $('#keyEnabledCount').textContent = KS.keys ? keys.filter(function(k){return k.enabled;}).length : '—';
  var query = KS.query.toLowerCase();
  var filtered = keys.filter(function(k){return (k.name + ' ' + (k.note || '')).toLowerCase().indexOf(query) >= 0;});
  if (!filtered.length){
    var message = !KS.keys ? (KS.error ? '暂时无法加载密钥' : '正在加载密钥…') : (query ? '没有匹配的密钥' : '还没有密钥');
    $('#keyRows').innerHTML = '<tr><td colspan="6">' + emptyBox(IC.box, message, !query && KS.keys ? '创建一把密钥，用于连接你的客户端。' : '') + '</td></tr>';
    return;
  }
  $('#keyRows').innerHTML = filtered.map(function(key){
    var models = keyModels((key.models || []).join(','));
    return '<tr><td data-l="名称"><div class="key-name">' + esc(key.name) + (key.legacy ? '<span class="key-legacy">原有</span>' : '') + '</div>' +
      '<div class="sub key-note">' + esc(key.note || '未填写备注') + '</div></td>' +
      '<td data-l="密钥"><code class="key-mask">' + esc(key.masked_key) + '</code></td>' +
      '<td data-l="模型绑定">' + (models.length
        ? '<div class="key-model-tags">' + models.map(function(name){return '<span class="key-model-tag">' + esc(name) + '</span>';}).join('') + '</div>'
        : '<span class="sub">不限制</span>') + '</td>' +
      '<td data-l="状态"><span class="bdg ' + (key.enabled ? 'ok' : 'off') + '"><i></i>' + (key.enabled ? '启用' : '停用') + '</span></td>' +
      '<td data-l="创建时间" class="mono key-date">' + esc(fmtTime(key.created_at)) + '</td>' +
      '<td data-l="操作"><div class="key-actions"><button class="btn sm" data-key-action="edit" data-id="' + esc(key.id) + '">编辑</button>' +
      '<button class="btn sm" data-key-action="toggle" data-id="' + esc(key.id) + '">' + (key.enabled ? '停用' : '启用') + '</button>' +
      '<button class="btn sm danger" data-key-action="delete" data-id="' + esc(key.id) + '">删除</button></div></td></tr>';
  }).join('');
}

function keyFormError(message){
  $('#keyFormError').textContent = message;
  $('#keyFormError').classList.toggle('hide', !message);
}

function openKeyEditor(key){
  KD = {id:key ? key.id : null, secret:'', busy:false, copied:false, closeConfirmed:false};
  $('#keyDialogTitle').textContent = key ? '编辑密钥' : '创建密钥';
  $('#keyName').value = key ? key.name : '';
  $('#keyNote').value = key ? (key.note || '') : '';
  fillModelInput(key ? keyModels((key.models || []).join(',')) : []);
  $('#keyModelPick').value = '';
  $('#keySecret').value = '';
  $('#keyFields').classList.remove('hide'); $('#keyCreated').classList.add('hide');
  $('#keyDialogSave').classList.remove('hide'); $('#keyDialogSave').textContent = key ? '保存修改' : '创建密钥';
  $('#keyDialogSave').disabled = false; $('#keyDialogCancel').textContent = '取消';
  $('#btnCopyKey').textContent = '复制密钥';
  keyFormError('');
  $('#keyDialog').showModal(); $('#keyName').focus();
  loadKeyModels();
}

function closeKeyEditor(){
  if (KD.busy) return;
  if (KD.secret && !KD.copied && !KD.closeConfirmed){
    KD.closeConfirmed = true;
    keyFormError('请确认已保存密钥。继续关闭后，无法再次查看完整密钥。');
    $('#keyDialogCancel').textContent = '已保存，关闭';
    return;
  }
  KD.secret = ''; $('#keySecret').value = '';
  $('#keyDialog').close();
}

$('#keyDialog').addEventListener('close', function(){ KD.secret = ''; $('#keySecret').value = ''; $('#keyName').value = ''; $('#keyNote').value = ''; $('#keyModels').value = ''; });
$('#keyDialog').addEventListener('cancel', function(event){ event.preventDefault(); closeKeyEditor(); });
$('#keyDialogClose').addEventListener('click', closeKeyEditor);
$('#keyDialogCancel').addEventListener('click', closeKeyEditor);
$('#btnCreateKey').addEventListener('click', function(){openKeyEditor(null);});
$('#btnKeysReload').addEventListener('click', loadKeys);
$('#keySearch').addEventListener('input', function(){KS.query=this.value;renderKeys();});
$('#keyModelPick').addEventListener('change', function(){
  var value = this.value; this.value = ''; if (!value) return;
  var models = keyModels($('#keyModels').value);
  if (models.indexOf(value) < 0) models.push(value);
  fillModelInput(models);
});
$('#btnClearModels').addEventListener('click', function(){ fillModelInput([]); });

$('#keyForm').addEventListener('submit', async function(event){
  event.preventDefault(); if (KD.busy || KD.secret) return;
  var body = {name:$('#keyName').value.trim(), note:$('#keyNote').value.trim(), models:keyModels($('#keyModels').value)};
  if (!body.name){keyFormError('请填写密钥名称');$('#keyName').focus();return;}
  if (body.models.length > 64){keyFormError('模型绑定最多 64 项');$('#keyModels').focus();return;}
  if (body.models.some(function(item){return item.length > 64;})){keyFormError('单个模型名不能超过 64 个字符');$('#keyModels').focus();return;}
  if (KD.id) body.id = KD.id;
  KD.busy = true; $('#keyDialogSave').disabled = true; keyFormError('');
  try{
    var result = await api(KD.id ? 'api/keys/update' : 'api/keys', body);
    if (KD.id){ KD.busy = false; closeKeyEditor(); toast('密钥信息已保存','ok'); }
    else{
      if (!result.key) throw new Error('未收到完整密钥，请刷新列表后重试');
      KD.secret = result.key;
      $('#keySecret').value = KD.secret;
      $('#keyFields').classList.add('hide'); $('#keyCreated').classList.remove('hide');
      $('#keyDialogSave').classList.add('hide'); $('#keyDialogCancel').textContent = '完成';
      $('#keyDialogTitle').textContent = '保存你的密钥'; $('#btnCopyKey').focus();
    }
    await loadKeys();
  }catch(error){keyFormError(error.message || '保存失败，请稍后重试');}
  finally{KD.busy = false;$('#keyDialogSave').disabled = false;}
});

$('#btnCopyKey').addEventListener('click', async function(){
  if (!KD.secret) return;
  try{
    if (navigator.clipboard && window.isSecureContext) await navigator.clipboard.writeText(KD.secret);
    else {var field=$('#keySecret');field.focus();field.select();if (!document.execCommand('copy')) throw new Error('copy unavailable');}
    KD.copied = true; keyFormError(''); $('#btnCopyKey').textContent = '已复制'; toast('密钥已复制','ok');
  }catch(error){keyFormError('无法自动复制，请选中上方密钥手动复制并保存。');$('#keySecret').focus();$('#keySecret').select();}
});

$('#keyRows').addEventListener('click', function(event){
  var button = event.target.closest('button[data-key-action]'); if (!button) return;
  var key = (KS.keys || []).find(function(k){return k.id === button.getAttribute('data-id');}); if (!key) return;
  var action = button.getAttribute('data-key-action');
  if (action === 'edit'){openKeyEditor(key);return;}
  var removing = action === 'delete';
  var label = removing ? '删除' : (key.enabled ? '停用' : '启用');
  var message = removing ? '删除后无法恢复。使用这把密钥的客户端将无法继续发起请求。' : (key.enabled ? '使用这把密钥的客户端将无法发起新请求，之后可以重新启用。' : '启用后，这把密钥可以重新用于调用网关。');
  if (key.enabled && (removing || action === 'toggle') && KS.keys.filter(function(k){return k.enabled;}).length === 1) message += ' 这是最后一把启用的密钥；你仍可从管理面板创建新密钥。';
  ask(label + '「' + key.name + '」',message,label,removing || key.enabled,async function(){
    button.disabled = true;
    try{
      await api(removing ? 'api/keys/delete' : 'api/keys/update',removing ? {id:key.id} : {id:key.id,enabled:!key.enabled});
      toast('密钥已' + label,'ok'); await loadKeys();
    }catch(error){toast(error.message || '操作失败','err');}
    finally{button.disabled = false;}
  });
});

// 注意：这里不要调用 init()。keys.js 与 usage.js 会被拼进同一个脚本，本段执行时
// usage.js 的顶层状态（US）还没赋值，从 #usage 进入就会抛
// "Cannot read properties of undefined (reading 'loading')" 并卡在加载态。
// 启动统一由 app.js 末尾负责，且延迟到整个脚本执行完之后。
