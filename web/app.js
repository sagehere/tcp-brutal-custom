let csrf = sessionStorage.getItem('csrf') || '';
let current = null;
let settingsDirty = false;
let currentBudgets = [];
const el = id => document.getElementById(id);
const mb = n => (Number(n || 0) / 1e6).toFixed(1) + ' MB';
const message = (value, error = false) => { el('notice').textContent = value; el('notice').classList.toggle('error', error); };

async function api(path, method = 'GET', body) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const r = await fetch(path, {method, credentials:'same-origin', headers, body:body === undefined ? undefined : JSON.stringify(body)});
  if (!r.ok) {
    if (r.status === 401) logout('会话已失效，请重新登录');
    let detail;
    try { detail = (await r.json()).error; } catch {}
    throw new Error(detail || `HTTP ${r.status}`);
  }
  return r;
}
function logout(text) {
  csrf = '';
  sessionStorage.removeItem('csrf');
  el('app').hidden = true;
  el('login').hidden = false;
  el('clientDrawer').hidden = true;
  el('loginNotice').textContent = text;
}
async function busy(button, work) {
  button.disabled = true;
  try { await work(); } catch (e) { message(e.message, true); }
  finally { button.disabled = false; }
}
function view(id) {
  document.querySelectorAll('.section').forEach(x => x.classList.toggle('active', x.id === id));
  document.querySelectorAll('[data-view]').forEach(x => x.classList.toggle('active', x.dataset.view === id));
  const title = document.querySelector(`[data-view="${id}"]`).textContent;
  el('crumb').textContent = el('pageTitle').textContent = title;
  if (id === 'historyView') draw().catch(e => message(e.message, true));
  if (id === 'settingsView') loadAutostart().catch(e => message(e.message, true));
}
document.querySelectorAll('[data-view]').forEach(x => x.onclick = () => view(x.dataset.view));
document.querySelectorAll('[data-jump]').forEach(x => x.onclick = () => view(x.dataset.jump));

function cell(row, text) { const td = row.insertCell(); td.textContent = String(text); return td; }
function button(label, className, action) { const b = document.createElement('button'); b.textContent = label; b.className = className; b.onclick = action; return b; }
function metric(label, value, detail) {
  const d = document.createElement('div'); d.className = 'card metric';
  const a = document.createElement('span'), b = document.createElement('strong'), c = document.createElement('small');
  a.textContent = label; b.textContent = value; c.textContent = detail;
  d.append(a,b,c); el('stats').append(d);
}
function editPort(p) {
  el('portDialogTitle').textContent = p ? `编辑端口 ${p.port}` : '新增端口';
  el('portForm').reset();
  el('port').value = p?.port ?? '';
  el('rate').value = p?.rate_mbps ?? '';
  el('gain').value = p?.gain ?? 20;
  el('compensationCap').value = p?.compensation_cap_percent ?? 125;
  el('portBudget').value = p?.budget ?? '';
  el('enabled').checked = p?.enabled ?? true;
  el('portDialog').showModal();
}
el('addPort').onclick = () => editPort(null);
el('closePort').onclick = () => el('portDialog').close();
el('closeDrawer').onclick = () => { el('clientDrawer').hidden = true; };
document.onkeydown = e => { if (e.key === 'Escape') el('clientDrawer').hidden = true; };

async function showClients(port) {
  el('clientDrawer').hidden = false;
  el('drawerTitle').textContent = `端口 ${port} · 当前连接`;
  el('clientNotice').textContent = '正在查询…';
  el('clients').replaceChildren();
  try {
    const data = await (await api(`/api/v1/connections?port=${port}`)).json();
    if (el('clientDrawer').hidden || el('drawerTitle').textContent !== `端口 ${port} · 当前连接`) return;
    el('clientNotice').textContent = data.connections.length ? `当前 ${data.connections.length} 条连接` : '当前没有连接';
    for (const c of data.connections) {
      const card = document.createElement('div'); card.className = 'connection';
      const ip = document.createElement('strong'); ip.textContent = `${c.client_ip}:${c.client_port}`;
      const detail = document.createElement('small'); detail.textContent = `${c.state} · 本地 ${c.local_ip}:${port}`;
      const tag = document.createElement('div'); tag.className = c.managed ? 'tag' : 'tag off';
      tag.textContent = c.managed ? 'Brutal 已接管' : `未接管 · ${c.algorithm}`;
      card.append(ip,detail,tag); el('clients').append(card);
    }
  } catch(e) { el('clientNotice').textContent = `查询失败：${e.message}`; }
}

async function refresh() {
  el('health').textContent = '正在刷新';
  const data = await (await api('/api/v1/status')).json();
  await loadBudgets();
  current = data;
  el('login').hidden = true; el('app').hidden = false;
  el('health').textContent = data.bpf_attached ? `运行中 · v${data.version}` : `eBPF 异常 · v${data.version}`;
  el('health').className = data.bpf_attached ? 'pill' : 'pill bad';
  el('stats').replaceChildren(); el('ports').replaceChildren(); el('overviewPorts').replaceChildren();
  const live = data.ports.filter(p => p.active);
  const total = live.reduce((a,p) => ({expected:a.expected+p.expected_bytes,actual:a.actual+p.actual_bytes,failed:a.failed+(p.selector?.failure||0)}), {expected:0,actual:0,failed:0});
  metric('活跃端口', String(live.length), `已配置 ${data.configured_ports.length} 条规则`);
  metric('当前连接', String(live.reduce((n,p) => n+p.members,0)), '已接管 TCP 连接');
  metric('累计净发送', mb(total.expected), '发送减重传，不代表已确认');
  metric('累计发送', mb(total.actual), '包含重传');
  metric('接管失败', String(total.failed), '累计选择失败');
  for (const b of currentBudgetStatus) if (b.warning) metric('出口预算告警', b.name, `预计超出 ${b.excess_mbps.toFixed(2)} Mbps`);
  for (const cfg of data.configured_ports) {
    const p = live.find(x => x.port === cfg.port);
    const row = el('ports').insertRow();
    cell(row, `${cfg.port} · ${p ? '生效' : cfg.enabled ? '未生效' : '停用'}`);
    cell(row, `${cfg.rate_mbps} · 补偿 ${cfg.compensation_cap_percent || 125}% · ${cfg.budget || '未归属预算'}`); cell(row, p?.members ?? 0);
    cell(row, mb(p?.expected_bytes)); cell(row, mb(p?.actual_bytes));
    cell(row, p?.sent ? (100*p.retrans/p.sent).toFixed(2)+'%' : '无样本');
    cell(row, p?.rtt_samples ? `采样 SRTT ${(p.rtt_sum_us/p.rtt_samples/1000).toFixed(1)} ms · 生命周期最大 ${(p.rtt_max_us/1000).toFixed(1)} ms` : '无样本');
    const actions = row.insertCell();
    actions.append(button('客户端', 'ghost', () => showClients(cfg.port)), button('编辑', 'secondary', () => editPort(cfg)), button('删除', 'danger', () => removePort(cfg.port)));
    const summary = document.createElement('div'); summary.className = 'row spread';
    const a = document.createElement('strong'), b = document.createElement('span');
    a.textContent = `${cfg.port} · ${p ? '生效' : cfg.enabled ? '未生效' : '停用'}`;
    b.className = 'muted'; b.textContent = `连接 ${p?.members ?? 0}  ·  实发送 ${mb(p?.actual_bytes)}`;
    summary.append(a,b); el('overviewPorts').append(summary);
  }
  for (const p of data.ports.filter(x => !x.active && x.members > 0)) {
    const row = el('ports').insertRow();
    cell(row, `${p.port} · 旧连接组（预算未覆盖）`); cell(row, '—'); cell(row, p.members);
    cell(row, mb(p.expected_bytes)); cell(row, mb(p.actual_bytes));
    cell(row, p.sent ? (100*p.retrans/p.sent).toFixed(2)+'%' : '无样本');
    cell(row, p.rtt_samples ? `采样 SRTT ${(p.rtt_sum_us/p.rtt_samples/1000).toFixed(1)} ms · 生命周期最大 ${(p.rtt_max_us/1000).toFixed(1)} ms` : '无样本');
    row.insertCell().append(button('客户端', 'ghost', () => showClients(p.port)));
  }
  if (!el('ports').rows.length) {
    const row = el('ports').insertRow(); const c = row.insertCell(); c.colSpan = 8; c.className = 'empty'; c.textContent = '尚无接管端口';
    el('overviewPorts').textContent = '尚无接管端口';
  }
  const selected = el('metricPort').value;
  el('metricPort').replaceChildren(new Option('全部','0'));
  for (const p of data.configured_ports) el('metricPort').add(new Option(String(p.port),String(p.port)));
  if ([...el('metricPort').options].some(x => x.value === selected)) el('metricPort').value = selected;
  if (!settingsDirty) {
    el('host').value = data.web_host; el('webPort').value = data.web_port;
    el('allowIPs').value = (data.allowed_ips || []).join(',');
  }
  el('job').textContent = data.update?.state ? `${data.update.state}: ${data.update.detail || ''}` : '';
  if (el('historyView').classList.contains('active')) await draw();
  if (el('settingsView').classList.contains('active')) await loadAutostart();
}
async function removePort(port) {
  if (!confirm(`删除端口 ${port}？旧连接会持续到自然结束。`)) return;
  await busy(el('refresh'), async () => { await api(`/api/v1/ports/${port}`, 'DELETE'); message('端口已删除'); await refresh(); });
}

function metricParams() {
  const seconds=Number(el('range').value), to=Math.floor(Date.now()/1000), tier=seconds>90*86400?'hour':seconds>7*86400?'minute':'raw';
  return `tier=${tier}&from=${to-seconds}&to=${to}&port=${el('metricPort').value}`;
}
function seriesSegments(points, key, scale, minTime, span, bucketSeconds = Infinity) {
  let segment = [];
  const segments = [];
  let previousTime;
  for (const p of points) {
    if ((p.gap || p.time - previousTime > bucketSeconds) && segment.length) { segments.push(segment); segment = []; }
    previousTime = p.time;
    if (!Number.isFinite(p[key])) { if (segment.length) segments.push(segment); segment=[]; continue; }
    segment.push(`${30+750*(p.time-minTime)/span},${220-185*p[key]/scale}`);
  }
  if (segment.length) segments.push(segment);
  return segments;
}
async function draw() {
  const data=await (await api('/api/v1/metrics?'+metricParams())).json();
  const bins = new Map();
  for (const p of data.samples) {
    const x = bins.get(p.time) || {time:p.time,expected:0,actual:0,retrans:0,rtt:0,count:0,gap:false,ackRate:0,sendRate:0,valid:true};
    x.expected += p.expected_bytes; x.actual += p.actual_bytes; x.retrans += p.retrans;
    x.rtt += p.rtt_sum_us; x.count += p.rtt_samples; x.gap ||= p.gap;
    x.valid &&= p.timing_valid && Number.isFinite(p.acked_mbps) && Number.isFinite(p.sent_mbps);
    x.ackRate += p.acked_mbps || 0; x.sendRate += p.sent_mbps || 0;
    bins.set(p.time,x);
  }
  const kind=el('metricKind').value;
  const points=[...bins.values()].sort((a,b)=>a.time-b.time).map(x => ({...x,expected:kind==='send'?(x.valid?x.ackRate:NaN):kind==='retrans'?(x.actual?100*x.retrans/x.actual:NaN):(x.count?x.rtt/x.count/1000:NaN),actual:kind==='send'&&x.valid?x.sendRate:NaN}));
  el('expectedLine').setAttribute('points',''); el('actualLine').setAttribute('points','');
  document.querySelectorAll('#chart .series').forEach(x=>x.remove());
  el('legend').hidden = kind !== 'send';
  if (!points.length) { el('chartNote').textContent = '暂无历史数据'; return; }
  const values=points.flatMap(p=>[p.expected,p.actual].filter(Number.isFinite));
  if (!values.length) { el('chartNote').textContent = '暂无有效样本；缺采、旧口径及组覆盖不一致时速率为不可用，不是零。'; return; }
  const peak=Math.max(0,...values);
  const scale=Math.max(1,peak);
  const minTime=points[0].time, span=Math.max(1,points.at(-1).time-minTime);
  for (const [key,className] of [['expected','expected'],['actual','actual']]) {
    const tier = new URLSearchParams(metricParams()).get('tier');
    const bucketSeconds = {raw:10,minute:60,hour:3600}[tier];
    for (const part of seriesSegments(points,key,scale,minTime,span,bucketSeconds)) {
      const poly=document.createElementNS('http://www.w3.org/2000/svg','polyline');
      poly.setAttribute('class',`series ${className}`);
      poly.setAttribute('points', part.length===1 ? `${part[0]} ${part[0]}` : part.join(' '));
      el('chart').append(poly);
    }
  }
  el('chartNote').textContent = kind==='send' ? `峰值 ${peak.toFixed(2)} Mbps；缺采、旧口径或组覆盖不一致区间不显示速率。ACK 吞吐不等于接收端应用 goodput。` : kind==='retrans' ? `重传率峰值 ${peak.toFixed(2)}%` : `采样 SRTT 均值峰值 ${peak.toFixed(1)} ms`;
}

async function loadAutostart() {
  el('autostartStatus').textContent = '正在读取状态…';
  try {
    const a=await (await api('/api/v1/autostart')).json();
    const labels={on:'已开启',off:'已关闭',partial:'部分开启',unknown:'状态未知'};
    el('autostartStatus').textContent = `${labels[a.state] || a.state} · 管理服务 ${a.manager?'开':'关'} / Web 服务 ${a.web?'开':'关'}`;
  } catch(e) { el('autostartStatus').textContent = `读取失败：${e.message}`; }
}
async function changeAutostart(state, button) {
  await busy(button, async () => {
    try { await api('/api/v1/autostart','PUT',{state}); message('开机启动设置已更新'); }
    finally { await loadAutostart(); }
  });
}
el('autostartOn').onclick=()=>changeAutostart('on',el('autostartOn'));
el('autostartOff').onclick=()=>changeAutostart('off',el('autostartOff'));
el('loginForm').onsubmit=async e=>{e.preventDefault(); const b=e.submitter; b.disabled=true; el('loginNotice').textContent='正在登录…'; try { const r=await api('/api/v1/login','POST',{password:el('password').value}); csrf=(await r.json()).csrf; sessionStorage.setItem('csrf',csrf); el('password').value=''; el('loginNotice').textContent=''; await refresh(); message('已登录'); } catch(x) { el('loginNotice').textContent=x.message; } finally { b.disabled=false; }};
el('portForm').onsubmit=async e=>{e.preventDefault(); await busy(e.submitter,async()=>{await api('/api/v1/ports','POST',{port:Number(el('port').value),rate_mbps:Number(el('rate').value),gain:Number(el('gain').value),enabled:el('enabled').checked,compensation_cap_percent:Number(el('compensationCap').value),budget:el('portBudget').value}); el('portDialog').close(); message('规则已保存'); await refresh();});};
el('settingsForm').oninput=()=>{settingsDirty=true;};
el('settingsForm').onsubmit=async e=>{e.preventDefault(); await busy(e.submitter,async()=>{await api('/api/v1/settings','PUT',{host:el('host').value,port:Number(el('webPort').value),allowed_ips:el('allowIPs').value.split(',').map(x=>x.trim()).filter(Boolean)}); settingsDirty=false; message('设置已保存；请重启 Web 服务生效');});};
el('changePassword').onsubmit=async e=>{e.preventDefault(); if ([...el('newPassword').value].length<8){message('新密码至少 8 个字符',true);return;} await busy(e.submitter,async()=>{await api('/api/v1/password','PUT',{password:el('newPassword').value}); el('newPassword').value=''; logout('密码已更新，请重新登录');});};
el('resetPassword').onclick=()=>busy(el('resetPassword'),async()=>{if(!confirm('确认重置面板登录密码？重置后所有已登录会话都会立即失效。'))return;const r=await api('/api/v1/password/reset','POST',{});const x=await r.json();el('resetPasswordValue').value=x.password;el('resetPasswordResult').hidden=false;message('密码已重置，请立即保存新密码');});
el('copyResetPassword').onclick=async()=>{const value=el('resetPasswordValue').value;if(!value)return;try{await navigator.clipboard.writeText(value);message('新密码已复制');}catch{el('resetPasswordValue').select();document.execCommand('copy');message('新密码已复制');}};
el('refresh').onclick=()=>busy(el('refresh'),async()=>{await refresh(); message('数据已刷新');});
el('range').onchange=el('metricPort').onchange=el('metricKind').onchange=()=>draw().catch(e=>message(e.message,true));
async function download(format,button){await busy(button,async()=>{const r=await api('/api/v1/metrics?'+metricParams()+'&format='+format);const blob=await r.blob(),a=document.createElement('a');a.href=URL.createObjectURL(blob);a.download='brutal-history.'+format;a.click();setTimeout(()=>URL.revokeObjectURL(a.href),1000);});}
el('exportCSV').onclick=()=>download('csv',el('exportCSV'));
el('exportJSON').onclick=()=>download('json',el('exportJSON'));
el('checkUpdate').onclick=()=>busy(el('checkUpdate'),async()=>{const x=await(await api('/api/v1/update/check')).json();el('releaseNotes').textContent=`当前 ${x.installed}；最新 ${x.latest}\n${x.notes}`;});
let currentBudgetStatus = [];
const budgetNav = button('出口预算与诊断','',()=>view('budgetsView'));
budgetNav.dataset.view='budgetsView'; document.querySelector('.nav').append(budgetNav);
async function loadBudgets() {
  const data=await(await api('/api/v1/budgets')).json();
  currentBudgets=data.budgets||[]; currentBudgetStatus=data.status||[];
  const selected=el('portBudget').value;
  el('portBudget').replaceChildren(new Option('不归属预算',''));
  el('budgetStatuses').replaceChildren();
  for (const b of currentBudgetStatus) {
    el('portBudget').add(new Option(b.name,b.name));
    const row=document.createElement('div'); row.className='row spread';
    const text=document.createElement('span');
    text.textContent=`${b.name}：可用 ${b.available_mbps.toFixed(2)} Mbps，补偿后预计 ${b.requested_mbps.toFixed(2)} Mbps${b.warning?'，超出 '+b.excess_mbps.toFixed(2)+' Mbps；建议合计目标至少下调 '+b.reduce_targets_percent.toFixed(1)+'% 或增加实测容量':''}`;
    row.append(text,button('编辑','ghost',()=>{el('budgetName').value=b.name;el('budgetCapacity').value=b.capacity_mbps;el('budgetReserve').value=b.reserve_percent;}),button('删除','danger',()=>busy(el('refresh'),async()=>{await api('/api/v1/budgets','PUT',currentBudgets.filter(x=>x.name!==b.name));await refresh();})));
    el('budgetStatuses').append(row);
  }
  if ([...el('portBudget').options].some(x=>x.value===selected)) el('portBudget').value=selected;
}
el('budgetForm').onsubmit=async e=>{e.preventDefault();await busy(e.submitter,async()=>{
  const b={name:el('budgetName').value,capacity_mbps:Number(el('budgetCapacity').value),reserve_percent:Number(el('budgetReserve').value)};
  await api('/api/v1/budgets','PUT',[...currentBudgets.filter(x=>x.name!==b.name),b]); await refresh(); message('预算已保存，仅告警，不改变出口限速');
});};
el('runDiagnose').onclick=()=>busy(el('runDiagnose'),async()=>{const d=await(await api('/api/v1/diagnose')).json();el('diagnoseOutput').textContent=JSON.stringify(d,null,2);});
if (csrf) refresh().catch(()=>logout('会话已失效，请重新登录'));
