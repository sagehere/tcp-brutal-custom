let csrf = sessionStorage.getItem('csrf') || '';
let current = null;
let settingsDirty = false;
let abState = {experiments:[], ports:[], summary:null, series:null, statistics:null, rollout:null, selectedEpoch:0};
let abRolloutDeadlineTimer = null;
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
  if (id === 'abView') refreshAB().catch(e => message(e.message, true));
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
  current = data;
  el('login').hidden = true; el('app').hidden = false;
  el('health').textContent = data.bpf_attached ? `运行中 · v${data.version}` : `eBPF 异常 · v${data.version}`;
  el('health').className = data.bpf_attached ? 'pill' : 'pill bad';
  el('stats').replaceChildren(); el('ports').replaceChildren(); el('overviewPorts').replaceChildren();
  const live = data.ports.filter(p => p.active);
  const total = live.reduce((a,p) => ({expected:a.expected+p.expected_bytes,actual:a.actual+p.actual_bytes,failed:a.failed+(p.selector?.failure||0)}), {expected:0,actual:0,failed:0});
  metric('活跃端口', String(live.length), `已配置 ${data.configured_ports.length} 条规则`);
  metric('当前连接', String(live.reduce((n,p) => n+p.members,0)), '已接管 TCP 连接');
  metric('应发送', mb(total.expected), '不重复数据量');
  metric('实发送', mb(total.actual), '包含重传');
  metric('接管失败', String(total.failed), '累计选择失败');
  for (const cfg of data.configured_ports) {
    const p = live.find(x => x.port === cfg.port);
    const row = el('ports').insertRow();
    cell(row, `${cfg.port} · ${p ? '生效' : cfg.enabled ? '未生效' : '停用'}`);
    cell(row, cfg.rate_mbps); cell(row, p?.members ?? 0);
    cell(row, mb(p?.expected_bytes)); cell(row, mb(p?.actual_bytes));
    cell(row, p?.sent ? (100*p.retrans/p.sent).toFixed(2)+'%' : '无样本');
    cell(row, p?.rtt_samples ? (p.rtt_sum_us/p.rtt_samples/1000).toFixed(1)+' ms' : '无样本');
    const actions = row.insertCell();
    actions.append(button('客户端', 'ghost', () => showClients(cfg.port)), button('编辑', 'secondary', () => editPort(cfg)), button('删除', 'danger', () => removePort(cfg.port)));
    const summary = document.createElement('div'); summary.className = 'row spread';
    const a = document.createElement('strong'), b = document.createElement('span');
    a.textContent = `${cfg.port} · ${p ? '生效' : cfg.enabled ? '未生效' : '停用'}`;
    b.className = 'muted'; b.textContent = `连接 ${p?.members ?? 0}  ·  实发送 ${mb(p?.actual_bytes)}`;
    summary.append(a,b); el('overviewPorts').append(summary);
  }
  for (const p of data.ports.filter(x => !x.active && x.members > 0 && !data.configured_ports.some(cfg => cfg.port === x.port))) {
    const row = el('ports').insertRow();
    cell(row, `${p.port} · 旧连接`); cell(row, '—'); cell(row, p.members);
    cell(row, mb(p.expected_bytes)); cell(row, mb(p.actual_bytes));
    cell(row, p.sent ? (100*p.retrans/p.sent).toFixed(2)+'%' : '无样本');
    cell(row, p.rtt_samples ? (p.rtt_sum_us/p.rtt_samples/1000).toFixed(1)+' ms' : '无样本');
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
function seriesSegments(points, key, scale, minTime, span) {
  let segment = [];
  const segments = [];
  for (const p of points) {
    if (p.gap && segment.length) { segments.push(segment); segment = []; }
    if (Number.isFinite(p[key])) segment.push(`${30+750*(p.time-minTime)/span},${220-185*p[key]/scale}`);
  }
  if (segment.length) segments.push(segment);
  return segments;
}
async function draw() {
  const data=await (await api('/api/v1/metrics?'+metricParams())).json();
  const bins = new Map();
  for (const p of data.samples) {
    const x = bins.get(p.time) || {time:p.time,expected:0,actual:0,retrans:0,rtt:0,count:0,gap:false};
    x.expected += p.expected_bytes; x.actual += p.actual_bytes; x.retrans += p.retrans;
    x.rtt += p.rtt_sum_us; x.count += p.rtt_samples; x.gap ||= p.gap;
    bins.set(p.time,x);
  }
  const range=Number(el('range').value), interval=range>90*86400?3600:range>7*86400?60:10, kind=el('metricKind').value;
  const points=[...bins.values()].sort((a,b)=>a.time-b.time).map(x => ({...x,expected:kind==='send'?x.expected*8/interval/1e6:kind==='retrans'?(x.actual?100*x.retrans/x.actual:NaN):(x.count?x.rtt/x.count/1000:NaN),actual:kind==='send'?x.actual*8/interval/1e6:NaN}));
  el('expectedLine').setAttribute('points',''); el('actualLine').setAttribute('points','');
  document.querySelectorAll('#chart .series').forEach(x=>x.remove());
  el('legend').hidden = kind !== 'send';
  if (!points.length) { el('chartNote').textContent = '暂无历史数据'; return; }
  const peak=Math.max(0,...points.flatMap(p=>[p.expected,p.actual].filter(Number.isFinite)));
  const scale=Math.max(1,peak);
  const minTime=points[0].time, span=Math.max(1,points.at(-1).time-minTime);
  for (const [key,className] of [['expected','expected'],['actual','actual']]) {
    for (const part of seriesSegments(points,key,scale,minTime,span)) {
      const poly=document.createElementNS('http://www.w3.org/2000/svg','polyline');
      poly.setAttribute('class',`series ${className}`);
      poly.setAttribute('points', part.length===1 ? `${part[0]} ${part[0]}` : part.join(' '));
      el('chart').append(poly);
    }
  }
  el('chartNote').textContent = kind==='send' ? `发送速率峰值 ${peak.toFixed(2)} Mbps` : kind==='retrans' ? `重传率峰值 ${peak.toFixed(2)}%` : `RTT 峰值 ${peak.toFixed(1)} ms`;
}


const abNumber = (value, digits = 2) => Number(value || 0).toFixed(digits);
const abDuration = seconds => {
  seconds = Number(seconds || 0);
  if (seconds >= 86400) return (seconds / 86400).toFixed(seconds % 86400 ? 1 : 0) + ' 天';
  if (seconds >= 3600) return (seconds / 3600).toFixed(seconds % 3600 ? 1 : 0) + ' 小时';
  if (seconds >= 60) return Math.floor(seconds / 60) + ' 分钟';
  return seconds + ' 秒';
};
const abTime = value => value ? new Date(Number(value) * 1000).toLocaleString() : '进行中';
function abMetricCard(label, value, detail) {
  const d = document.createElement('div'); d.className = 'card metric';
  const a = document.createElement('span'), b = document.createElement('strong'), c = document.createElement('small');
  a.textContent = label; b.textContent = value; c.textContent = detail;
  d.append(a,b,c); return d;
}
function abWindow() {
  const seconds = Number(el('abRange').value || 86400);
  const to = Math.floor(Date.now()/1000), from = to - seconds;
  const tier = seconds <= 21600 ? 'raw' : seconds <= 7*86400 ? 'minute' : 'hour';
  return {seconds, from, to, tier};
}
function abReasonZH(reason) {
  if (reason === 'epoch is not a two-cohort comparison') return '该 epoch 为 0% 或 100%，不是双组同时对照';
  if (reason === 'selector failure rate exceeds policy') return 'Selector 失败率超过预设门槛';
  if (reason === 'no assigned connections') return '尚无已分配的新连接';
  if (reason === 'gap samples present') return '存在数据缺口样本';
  if (reason === 'analysis plan was not predeclared for this epoch') return '该历史 epoch 没有在开始前固化统计计划，只能回看，不能进入阶段复核';
  if (reason === 'network readiness requirements are not yet met') return '网络数据完整性门槛尚未满足';
  if (reason.startsWith('paired minute samples are insufficient')) return '用于 block bootstrap 的成对 minute 样本还不够';
  if (reason === 'one or more network confidence intervals cross a predeclared guardrail') return '至少一项网络指标置信区间跨过预设 Guardrail，需要人工复核';
  if (reason === 'application metrics are not available') return '没有应用层指标，只能进行网络层复核';
  if (reason === 'application readiness or precomputed sample target is not yet met') return '应用层完整性或预计算样本量目标尚未满足';
  if (reason === 'application success non-inferiority confidence interval crosses the predeclared margin') return '应用成功率非劣区间跨过预设界限，需要人工复核';
  if (reason === 'predeclared data-quality, sample-size, and guardrail checks are satisfied') return '数据质量、样本量和预设 Guardrail 均满足，可人工复核下一档';
  let m = reason.match(/^duration (\d+)s < (\d+)s$/);
  if (m) return '运行时间 ' + abDuration(m[1]) + '，低于门槛 ' + abDuration(m[2]);
  m = reason.match(/^baseline connections (\d+) < (\d+)$/);
  if (m) return 'Baseline 新连接 ' + m[1] + '，低于门槛 ' + m[2];
  m = reason.match(/^canary connections (\d+) < (\d+)$/);
  if (m) return 'Canary 新连接 ' + m[1] + '，低于门槛 ' + m[2];
  m = reason.match(/^allocation \|z\| ([\d.]+) > ([\d.]+)$/);
  if (m) return '实际分流偏差 |z|=' + m[1] + '，超过门槛 ' + m[2];
  m = reason.match(/^baseline app requests (\d+) < (\d+)$/);
  if (m) return 'Baseline 应用请求 ' + m[1] + '，低于门槛 ' + m[2];
  m = reason.match(/^canary app requests (\d+) < (\d+)$/);
  if (m) return 'Canary 应用请求 ' + m[1] + '，低于门槛 ' + m[2];
  return reason;
}
function abMetaLine(label, value) {
  const row = document.createElement('div'); row.className = 'row spread';
  const a = document.createElement('span'), b = document.createElement('strong');
  a.className = 'muted'; a.textContent = label; b.textContent = value;
  row.append(a,b); return row;
}
function renderABExperiments() {
  const box = el('abExperiments'); box.replaceChildren();
  if (!abState.experiments.length) {
    const p = document.createElement('div'); p.className = 'ab-empty'; p.textContent = '当前没有运行中的 A/B 实验；历史数据仍可从上方端口选择查看。';
    box.append(p); return;
  }
  for (const x of abState.experiments) {
    const card = document.createElement('div'); card.className = 'ab-exp';
    const head = document.createElement('div'); head.className = 'row spread';
    const title = document.createElement('strong'); title.textContent = '端口 ' + x.port;
    const target = document.createElement('span'); target.className = 'pill'; target.textContent = 'Canary ' + x.canary_percent + '%';
    head.append(title,target);
    const total = Number(x.selector?.baseline || 0) + Number(x.selector?.canary || 0);
    const actual = total ? 100 * Number(x.selector.canary || 0) / total : 0;
    const meta = document.createElement('div'); meta.className = 'ab-meta';
    const values = [
      ['目标速率', Number(x.rate_mbps).toFixed(1) + ' Mbps'],
      ['实际分流', total ? actual.toFixed(1) + '%' : '无样本'],
      ['Baseline 新连接', String(x.selector?.baseline || 0)],
      ['Canary 新连接', String(x.selector?.canary || 0)],
      ['Selector 失败', String(x.selector?.failure || 0)],
      ['CWND gain', String(x.gain)],
      ['App 非劣界限', x.analysis_plan ? '-'+Number(x.analysis_plan.app_success_ni_margin_pp).toFixed(2)+' pp' : '默认'],
      ['Bootstrap', x.analysis_plan ? x.analysis_plan.bootstrap_block_minutes+' 分钟' : '默认'],
      ['阶段编排', x.rollout_plan ? x.rollout_plan.stages.join('→')+'%' : '手动'],
      ['观察窗口', x.rollout_plan ? abDuration(x.rollout_plan.observation_window_seconds) : '—']
    ];
    for (const pair of values) {
      const span=document.createElement('span'), b=document.createElement('b');
      span.textContent=pair[0]+' '; b.textContent=pair[1]; span.append(b); meta.append(span);
    }
    const actions=document.createElement('div'); actions.className='ab-actions';
    const del=button('删除实验','danger',()=>removeABExperiment(x.port,del));
    if (x.rollout_plan) {
      const managed=document.createElement('span'); managed.className='ab-state info'; managed.textContent='受阶段编排管理';
      const focus=button('查看编排','secondary',()=>{el('abPort').value=String(x.port);refreshAB().catch(e=>message(e.message,true));});
      actions.append(managed,focus,del);
    } else {
      const label=document.createElement('label'); label.className='field'; label.textContent='调整 Canary %';
      const select=document.createElement('select');
      const choices=[0,5,10,25,50,100,Number(x.canary_percent)].filter((v,i,a)=>a.indexOf(v)===i).sort((a,b)=>a-b);
      for (const pct of choices) select.add(new Option(pct+'%',String(pct)));
      select.value=String(x.canary_percent); label.append(select);
      const apply=button('应用比例','secondary',()=>changeABPercent(x.port,select,apply));
      actions.append(label,apply,del);
    }
    card.append(head,meta,actions); box.append(card);
  }
}
async function changeABPercent(port, select, control) {
  await busy(control, async () => {
    await api('/api/v1/ab/' + port, 'PUT', {canary_percent:Number(select.value)});
    message('端口 ' + port + ' 的 Canary 比例已更新；只影响新连接');
    await refreshAB();
  });
}
async function removeABExperiment(port, control) {
  if (!confirm('删除 A/B 实验端口 ' + port + '？已有连接会自然结束。')) return;
  await busy(control, async () => {
    await api('/api/v1/ab/' + port, 'DELETE');
    message('A/B 实验已删除，历史 epoch 数据仍保留');
    await refreshAB();
  });
}


function scheduleABRolloutDeadlineRefresh() {
  if (abRolloutDeadlineTimer) {
    clearTimeout(abRolloutDeadlineTimer);
    abRolloutDeadlineTimer=null;
  }
  const v=abState.rollout, s=v?.current_stage;
  if (!v || v.window_state!=='observing' || !s) return;
  const delay=Math.max(1000,(Number(s.observation_ends)-Date.now()/1000+1)*1000);
  if (delay >= 2147480000) return;
  abRolloutDeadlineTimer=setTimeout(()=>{
    abRolloutDeadlineTimer=null;
    refreshAB().catch(e=>message(e.message,true));
  },delay);
}

function abRolloutStateZH(state) {
  return {
    observing:'固定窗口观察中',
    eligible_review:'窗口完成 · 可人工批准下一档',
    guardrail_review:'窗口完成 · Guardrail 人工复核',
    network_review_only:'窗口完成 · 仅网络层可复核',
    insufficient_review:'窗口完成 · 样本不足',
    completion_review:'最终阶段窗口完成 · 待人工完成',
    paused:'已暂停 · 当前为 0% Canary',
    rolled_back:'已回退 baseline 并结束',
    completed:'编排已完成',
    stopped:'实验已删除 / 编排停止',
    superseded:'已被新的编排替代',
    desynced:'编排与当前配置不一致'
  }[state] || state || '未启用阶段编排';
}
function abRolloutActionZH(action) {
  return {pause:'暂停',resume:'恢复本档',rollback:'回退 baseline',retry:'重开本档窗口',advance:'批准下一档',complete:'完成编排'}[action] || action;
}
function abRolloutActionClass(action) {
  if (action === 'rollback') return 'danger';
  if (action === 'advance' || action === 'complete') return 'primary';
  if (action === 'pause' || action === 'resume') return 'secondary';
  return 'ghost';
}
async function runABRolloutAction(port, action, control) {
  const prompts={
    pause:'暂停会立即把新连接切回 0% Canary，并结束当前观察窗口。恢复时会重新开启完整的新窗口。确认暂停？',
    resume:'恢复会回到当前阶段比例，并从现在开始一个全新的固定观察窗口。确认恢复？',
    rollback:'回退会立即把新连接切回 0% Canary，并结束本次阶段编排。之后如需继续，应重新创建实验。确认回退？',
    retry:'重试会结束当前窗口，并以相同 Canary 比例开启一个全新的固定观察窗口。确认重试？',
    advance:'批准下一档会改变之后建立的新连接比例，并创建新的 epoch 与固定观察窗口。确认进入下一档？',
    complete:'完成只会结束编排状态，不会自动改变当前 Canary 比例。确认标记为完成？'
  };
  if (!confirm(prompts[action] || ('确认执行 '+action+'？'))) return;
  await busy(control, async()=>{
    await api('/api/v1/ab/rollout/'+port+'/'+action,'POST',{});
    abState.selectedEpoch=0;
    message('阶段编排动作已执行：'+abRolloutActionZH(action));
    await refreshAB();
  });
}
function renderABRollout() {
  const box=el('abRollout'), stagesBody=el('abRolloutStages'), eventsBody=el('abRolloutEvents');
  box.replaceChildren(); stagesBody.replaceChildren(); eventsBody.replaceChildren();
  const v=abState.rollout;
  scheduleABRolloutDeadlineRefresh();
  if (!v) {
    const p=document.createElement('p'); p.className='muted'; p.textContent='这个端口没有阶段编排记录；可继续使用手动 A/B 与 Step 5 统计分析。';
    box.append(p);
    let row=stagesBody.insertRow(),td=row.insertCell();td.colSpan=8;td.className='empty';td.textContent='无编排记录';
    row=eventsBody.insertRow();td=row.insertCell();td.colSpan=5;td.className='empty';td.textContent='无编排记录';
    return;
  }
  const r=v.rollout||{}, s=v.current_stage;
  const top=document.createElement('div'); top.className='row spread';
  const badge=document.createElement('span');
  const bad=['guardrail_review','desynced','rolled_back'].includes(v.window_state);
  const info=['paused','completed','stopped','superseded','completion_review','network_review_only'].includes(v.window_state);
  badge.className='ab-state '+(bad?'bad':info?'info':v.window_state==='observing'||v.window_state==='insufficient_review'?'wait':'');
  badge.textContent=abRolloutStateZH(v.window_state);
  const steps=document.createElement('span'); steps.className='hint';
  steps.textContent='计划 '+(r.stages||[]).map(x=>x+'%').join(' → ')+' · 每档 '+abDuration(r.observation_window_seconds||0);
  top.append(badge,steps); box.append(top);

  const grid=document.createElement('div'); grid.className='decision-grid';
  const remaining=s ? Math.max(0,Number(s.observation_ends||0)-Math.floor(Date.now()/1000)) : 0;
  grid.append(
    abDecisionBox('当前阶段',s ? '#'+(Number(s.stage_index)+1)+' · '+s.canary_percent+'%' : '—','Rollout #'+(r.id||'—')),
    abDecisionBox('固定窗口截止',s ? abTime(s.observation_ends) : '—',remaining>0?'剩余 '+abDuration(remaining):'窗口已到期'),
    abDecisionBox('当前 Epoch',s ? '#'+s.epoch_id : '—',s?'开始 '+abTime(s.started):''),
    abDecisionBox('编排状态',r.status||'—',r.ended?'结束 '+abTime(r.ended):'进行中')
  );
  box.append(grid);
  const actions=document.createElement('div'); actions.className='row'; actions.style.marginTop='14px';
  for (const action of v.allowed_actions||[]) {
    const b=button(abRolloutActionZH(action),abRolloutActionClass(action),()=>runABRolloutAction(r.port,action,b));
    actions.append(b);
  }
  if ((v.allowed_actions||[]).length) box.append(actions);
  const note=document.createElement('p'); note.className='hint';
  note.textContent='扩流不会自动发生；“批准下一档”只有在固定窗口结束且服务器复核状态允许时才会出现。';
  box.append(note);

  const history=v.stage_history||[];
  if (!history.length) {
    const row=stagesBody.insertRow(),td=row.insertCell();td.colSpan=8;td.className='empty';td.textContent='暂无阶段记录';
  } else {
    for (const x of history) {
      const row=stagesBody.insertRow();
      cell(row,'#'+x.id); cell(row,'#'+(Number(x.stage_index)+1)); cell(row,x.canary_percent+'%'); cell(row,'#'+x.epoch_id);
      cell(row,abTime(x.started)); cell(row,abTime(x.observation_ends)); cell(row,x.ended?abTime(x.ended):'进行中'); cell(row,x.final_state||x.end_reason||'—');
    }
  }
  const events=v.events||[];
  if (!events.length) {
    const row=eventsBody.insertRow(),td=row.insertCell();td.colSpan=5;td.className='empty';td.textContent='暂无审计事件';
  } else {
    for (const x of events) {
      const row=eventsBody.insertRow();
      cell(row,abTime(x.time)); cell(row,abRolloutActionZH(x.action)); cell(row,x.from_percent+'% → '+x.to_percent+'%'); cell(row,x.epoch_id?'#'+x.epoch_id:'—'); cell(row,x.detail||'—');
    }
  }
}
async function optionalJSON(path) {
  const r=await fetch(path,{credentials:'same-origin'});
  if (r.status===404) return null;
  if (!r.ok) {
    if (r.status===401) logout('会话已失效，请重新登录');
    let detail; try { detail=(await r.json()).error; } catch {}
    throw new Error(detail||('HTTP '+r.status));
  }
  return r.json();
}

function renderABReadiness(c, policy) {
  const box=el('abReadiness'); box.replaceChildren();
  if (!c) { box.textContent='当前 epoch 尚无可汇总的 cohort 样本'; return; }
  const states=document.createElement('div'); states.className='row';
  const network=document.createElement('span'); network.className='ab-state' + (c.network_ready?'':' wait');
  network.textContent=c.network_ready?'网络数据可分析':'网络数据继续采集';
  const app=document.createElement('span'); app.className='ab-state' + (c.application_ready?'':' wait');
  app.textContent=c.application_ready?'应用数据可分析':'应用数据继续采集';
  states.append(network,app); box.append(states);
  const threshold=document.createElement('p'); threshold.className='hint';
  threshold.textContent='门槛：≥'+abDuration(policy?.min_duration_seconds||1800)+'、每组 ≥'+(policy?.min_connections_per_cohort||200)+' 新连接、Selector 失败 ≤'+(policy?.max_selector_failure_percent||0.1)+'%、|z| ≤'+(policy?.max_allocation_abs_z||4)+'；应用层每组 ≥'+(policy?.min_app_requests_per_cohort||1000)+' 请求。';
  box.append(threshold);
  if (c.reasons?.length) {
    const ul=document.createElement('ul'); ul.className='reason-list';
    for (const reason of c.reasons) { const li=document.createElement('li'); li.textContent=abReasonZH(reason); ul.append(li); }
    box.append(ul);
  } else {
    const p=document.createElement('p'); p.className='ab-note'; p.textContent='当前数据通过预设完整性门槛；这仍不是性能优劣结论。';
    box.append(p);
  }
}
function abCIText(ci, digits = 2, suffix = '') {
  if (!ci?.available) return '样本不足';
  return Number(ci.estimate).toFixed(digits)+' ['+Number(ci.lower).toFixed(digits)+', '+Number(ci.upper).toFixed(digits)+']'+suffix;
}
function abDecisionBox(label, value, detail='') {
  const d=document.createElement('div'); d.className='decision-box';
  const a=document.createElement('span'), b=document.createElement('strong');
  a.textContent=label; b.textContent=value; d.append(a,b);
  if (detail) { const c=document.createElement('div'); c.className='hint'; c.textContent=detail; d.append(c); }
  return d;
}
function abDecisionState(state) {
  return {
    collecting:['继续采集','wait'],
    guardrail_review:['Guardrail 人工复核','bad'],
    eligible_review:['满足预设条件 · 可人工复核下一档',''],
    network_review_only:['仅网络层可复核','info'],
    not_comparable:['非双组对照','info']
  }[state] || [state || '等待分析','wait'];
}
async function advanceABStage(port, percent, control) {
  if (!confirm('将端口 '+port+' 的 Canary 比例调整到 '+percent+'%？只影响之后建立的新连接，不会自动断开现有连接。')) return;
  await busy(control, async()=>{
    await api('/api/v1/ab/'+port,'PUT',{canary_percent:Number(percent)});
    message('已人工确认进入 '+percent+'% 阶段；新的 epoch 已创建');
    await refreshAB();
  });
}
function renderABDecision() {
  const box=el('abDecision'); box.replaceChildren();
  const analyses=abState.statistics?.analyses||[];
  const a=analyses.find(x=>Number(x.epoch_id)===Number(abState.selectedEpoch));
  if (!a) {
    const p=document.createElement('p'); p.className='muted'; p.textContent='当前 epoch 尚无统计分析结果';
    box.append(p); return;
  }
  const state=abDecisionState(a.state);
  const top=document.createElement('div'); top.className='row spread';
  const badge=document.createElement('span'); badge.className='ab-state '+state[1]; badge.textContent=state[0];
  const planFlag=document.createElement('span'); planFlag.className='hint';
  planFlag.textContent=a.plan_predeclared?'统计计划已在 epoch 开始前固化':'历史 epoch：统计计划未预先固化';
  top.append(badge,planFlag); box.append(top);

  const c=(abState.summary?.comparisons||[]).find(x=>Number(x.epoch_id)===Number(a.epoch_id));
  const grid=document.createElement('div'); grid.className='decision-grid';
  const app=a.application||{}, net=a.network||{}, plan=a.plan||{};
  grid.append(
    abDecisionBox('应用成功率差 · 95% CI',abCIText(app.difference_pp,3,' pp'),'非劣界限 ≥ -'+Number(plan.app_success_ni_margin_pp||0).toFixed(2)+' pp'),
    abDecisionBox('重传差 · Block Bootstrap CI',abCIText(net.retrans_delta_pp,3,' pp'),'上界需 ≤ +'+Number(plan.max_retrans_delta_pp||0).toFixed(2)+' pp'),
    abDecisionBox('RTT 相对变化 · Bootstrap CI',abCIText(net.mean_rtt_delta_percent,2,'%'),'上界需 ≤ +'+Number(plan.max_mean_rtt_delta_percent||0).toFixed(1)+'%'),
    abDecisionBox('Goodput 相对变化 · Bootstrap CI',abCIText(net.goodput_per_member_delta_percent,2,'%'),'下界需 ≥ '+Number(plan.min_goodput_delta_percent||0).toFixed(1)+'%'),
    abDecisionBox('应用样本量目标',app.sample_target?.available?(app.sample_target.baseline+' / '+app.sample_target.canary):'不适用','Baseline / Canary；当前 '+(c?.baseline_app_requests||0)+' / '+(c?.canary_app_requests||0)),
    abDecisionBox('成对 Minute 样本',String(net.paired_minutes||0),'Block '+(net.block_minutes||plan.bootstrap_block_minutes||0)+' 分钟 · '+(net.replicates||0)+' 次重采样')
  );
  box.append(grid);

  const planNote=document.createElement('p'); planNote.className='hint';
  planNote.textContent='预设：alpha='+Number(plan.alpha||0).toFixed(3)+'，power='+Number(plan.power||0).toFixed(2)+'，预计基线成功率='+Number(plan.expected_app_success_percent||0).toFixed(2)+'%。统计区间用于阶段复核，不进行连续 p-value 自动扩流。';
  box.append(planNote);

  if (a.reasons?.length) {
    const ul=document.createElement('ul'); ul.className='reason-list';
    for (const reason of a.reasons) { const li=document.createElement('li'); li.textContent=abReasonZH(reason); ul.append(li); }
    box.append(ul);
  }
  const epoch=(abState.summary?.epochs||[]).find(x=>Number(x.id)===Number(a.epoch_id));
  const exp=abState.experiments.find(x=>Number(x.port)===Number(a.port));
  if (a.state==='eligible_review' && a.next_stage_percent && epoch && !epoch.ended && exp && !exp.rollout_plan && Number(exp.canary_percent)===Number(a.canary_percent)) {
    const row=document.createElement('div'); row.className='row'; row.style.marginTop='14px';
    const b=button('人工确认进入 '+a.next_stage_percent+'%','primary',()=>advanceABStage(a.port,a.next_stage_percent,b));
    const note=document.createElement('span'); note.className='hint'; note.textContent='点击后才会调整；系统不会自动扩流。';
    row.append(b,note); box.append(row);
  }
}

function abAddCompareRow(label, base, canary, delta) {
  const row=el('abComparison').insertRow();
  cell(row,label); cell(row,base); cell(row,canary);
  const d=cell(row,delta); d.className='delta';
}
function renderABComparison(c) {
  el('abComparison').replaceChildren();
  if (!c) {
    const row=el('abComparison').insertRow(), td=row.insertCell(); td.colSpan=4; td.className='empty'; td.textContent='该 epoch 尚无成对样本';
    return;
  }
  const signed=(n,d=2,suffix='') => (Number(n)>=0?'+':'')+Number(n||0).toFixed(d)+suffix;
  abAddCompareRow('重传率',abNumber(c.baseline_retrans_percent,3)+'%',abNumber(c.canary_retrans_percent,3)+'%',signed(c.retrans_delta_pp,3,' pp'));
  abAddCompareRow('平均 RTT',abNumber(c.baseline_mean_rtt_ms,3)+' ms',abNumber(c.canary_mean_rtt_ms,3)+' ms',signed(c.mean_rtt_delta_percent,2,'%'));
  abAddCompareRow('归一有效吞吐',abNumber(c.baseline_goodput_per_member_mbps,3)+' Mbps',abNumber(c.canary_goodput_per_member_mbps,3)+' Mbps',signed(c.goodput_per_member_delta_percent,2,'%'));
  const haveApp=Number(c.baseline_app_requests||0)+Number(c.canary_app_requests||0)>0;
  abAddCompareRow('应用成功率',haveApp?abNumber(c.baseline_app_success_percent,3)+'%':'—',haveApp?abNumber(c.canary_app_success_percent,3)+'%':'—',haveApp?signed(c.app_success_delta_pp,3,' pp'):'—');
  const haveLatency=Number(c.baseline_app_mean_latency_ms||0)>0 || Number(c.canary_app_mean_latency_ms||0)>0;
  abAddCompareRow('应用平均延迟',haveLatency?abNumber(c.baseline_app_mean_latency_ms,3)+' ms':'—',haveLatency?abNumber(c.canary_app_mean_latency_ms,3)+' ms':'—',haveLatency?signed(c.app_mean_latency_delta_percent,2,'%'):'—');
}
function renderABEpochs() {
  const tbody=el('abEpochs'); tbody.replaceChildren();
  const epochs=[...(abState.summary?.epochs||[])].reverse();
  const comps=abState.summary?.comparisons||[];
  if (!epochs.length) {
    const row=tbody.insertRow(), td=row.insertCell(); td.colSpan=6; td.className='empty'; td.textContent='所选时间范围内没有 epoch';
    return;
  }
  for (const epoch of epochs) {
    const c=comps.find(x=>Number(x.epoch_id)===Number(epoch.id));
    const row=tbody.insertRow(); row.className='epoch-row'+(Number(epoch.id)===Number(abState.selectedEpoch)?' selected':'');
    cell(row,'#'+epoch.id); cell(row,epoch.canary_percent+'%'); cell(row,abTime(epoch.started)); cell(row,epoch.ended?abTime(epoch.ended):'进行中'); cell(row,epoch.code_version);
    cell(row,c?(c.network_ready?'数据可分析':(epoch.canary_percent===0||epoch.canary_percent===100?'非双组对照':'采集中')):'无汇总样本');
    row.onclick=()=>{abState.selectedEpoch=epoch.id; renderABAnalysis();};
  }
}
function renderABChart() {
  document.querySelectorAll('#abChart .series').forEach(x=>x.remove());
  const kind=el('abMetric').value, epoch=Number(abState.selectedEpoch);
  const data=abState.series;
  if (!data || !epoch) { el('abChartNote').textContent='暂无趋势数据'; return; }
  const bins=new Map();
  if (kind==='appSuccess' || kind==='appLatency') {
    for (const x of data.app_samples||[]) {
      if (Number(x.epoch_id)!==epoch) continue;
      const key=x.time+'|'+x.cohort, b=bins.get(key)||{time:x.time,cohort:x.cohort,requests:0,success:0,latency:0,latencySamples:0,gap:false};
      b.requests+=Number(x.requests||0); b.success+=Number(x.success||0); b.latency+=Number(x.latency_sum_us||0); b.latencySamples+=Number(x.latency_samples||0); bins.set(key,b);
    }
  } else {
    for (const x of data.samples||[]) {
      if (Number(x.epoch_id)!==epoch) continue;
      const key=x.time+'|'+x.cohort, b=bins.get(key)||{time:x.time,cohort:x.cohort,sent:0,acked:0,retrans:0,rtt:0,rttSamples:0,memberSeconds:0,gap:false};
      b.sent+=Number(x.sent||0); b.acked+=Number(x.acked||0); b.retrans+=Number(x.retrans||0); b.rtt+=Number(x.rtt_sum_us||0); b.rttSamples+=Number(x.rtt_samples||0); b.memberSeconds+=Number(x.member_seconds||0); b.gap ||= !!x.gap; bins.set(key,b);
    }
  }
  const label={retrans:['重传率','%'],rtt:['平均 RTT',' ms'],goodput:['归一有效吞吐',' Mbps'],appSuccess:['应用成功率','%'],appLatency:['应用平均延迟',' ms']}[kind];
  const values=[...bins.values()].map(x=>{
    let value=NaN;
    if (kind==='retrans') value=x.sent?100*x.retrans/x.sent:NaN;
    if (kind==='rtt') value=x.rttSamples?x.rtt/x.rttSamples/1000:NaN;
    if (kind==='goodput') value=x.memberSeconds?x.acked*8/x.memberSeconds/1e6:NaN;
    if (kind==='appSuccess') value=x.requests?100*x.success/x.requests:NaN;
    if (kind==='appLatency') value=x.latencySamples?x.latency/x.latencySamples/1000:NaN;
    return {...x,value};
  }).filter(x=>Number.isFinite(x.value)).sort((a,b)=>a.time-b.time);
  const baseline=values.filter(x=>x.cohort==='baseline'), canary=values.filter(x=>x.cohort==='canary');
  el('abChartTitle').textContent=label[0]+' · epoch #'+epoch+' · '+data.tier+' 粒度';
  if (!values.length) { el('abChartNote').textContent='该 epoch 暂无 '+label[0]+' 时序样本'; return; }
  const peak=Math.max(0,...values.map(x=>x.value)), scale=Math.max(1,peak);
  const minTime=Math.min(...values.map(x=>x.time)), maxTime=Math.max(...values.map(x=>x.time)), span=Math.max(1,maxTime-minTime);
  for (const [points,className] of [[baseline,'baseline'],[canary,'canary']]) {
    for (const part of seriesSegments(points,'value',scale,minTime,span)) {
      const poly=document.createElementNS('http://www.w3.org/2000/svg','polyline');
      poly.setAttribute('class','series '+className);
      poly.setAttribute('points',part.length===1?part[0]+' '+part[0]:part.join(' '));
      el('abChart').append(poly);
    }
  }
  el('abChartNote').textContent='峰值 '+peak.toFixed(kind==='rtt'||kind==='appLatency'?2:3)+label[1]+' · Baseline '+baseline.length+' 点 / Canary '+canary.length+' 点；gap 区间不会被折线跨接。';
}
function renderABAnalysis() {
  const summary=abState.summary, epochs=summary?.epochs||[], comps=summary?.comparisons||[];
  if (!epochs.length) {
    el('abAnalysis').hidden=true; el('abNoData').hidden=false; return;
  }
  if (!epochs.some(x=>Number(x.id)===Number(abState.selectedEpoch))) abState.selectedEpoch=epochs.at(-1).id;
  const epoch=epochs.find(x=>Number(x.id)===Number(abState.selectedEpoch));
  const c=comps.find(x=>Number(x.epoch_id)===Number(abState.selectedEpoch));
  el('abNoData').hidden=true; el('abAnalysis').hidden=false;
  const headline=el('abHeadline'); headline.replaceChildren();
  if (c) {
    headline.append(
      abMetricCard('目标 Canary',c.canary_percent+'%','epoch #'+c.epoch_id),
      abMetricCard('实际 Canary',abNumber(c.actual_canary_percent,1)+'%',signedAllocation(c.allocation_error_pp)),
      abMetricCard('新连接分配',c.baseline_connections+' / '+c.canary_connections,'Baseline / Canary'),
      abMetricCard('Selector 失败',String(c.selector_failures),'当前 epoch 汇总'),
      abMetricCard('采样时长',abDuration(c.duration_seconds),c.network_ready?'达到网络门槛':'仍在采集')
    );
  } else {
    headline.append(abMetricCard('Epoch','#'+epoch.id,epoch.canary_percent+'% Canary'),abMetricCard('汇总状态','等待样本','采集到 minute 数据后生成对比'));
  }
  renderABReadiness(c,summary?.analysis_policy||{});
  renderABRollout();
  renderABDecision();
  const meta=el('abEpochMeta'); meta.replaceChildren();
  meta.append(abMetaLine('Epoch','#'+epoch.id),abMetaLine('Canary 目标',epoch.canary_percent+'%'),abMetaLine('开始',abTime(epoch.started)),abMetaLine('结束',epoch.ended?abTime(epoch.ended):'进行中'),abMetaLine('目标速率',Number(epoch.rate_mbps).toFixed(1)+' Mbps'),abMetaLine('CWND gain',String(epoch.gain)),abMetaLine('代码版本',epoch.code_version),abMetaLine('边界原因',epoch.reason));
  renderABComparison(c); renderABEpochs(); renderABChart();
}
function signedAllocation(value) {
  const n=Number(value||0);
  return '目标偏差 '+(n>=0?'+':'')+n.toFixed(2)+' pp';
}
async function refreshAB() {
  const selected=el('abPort').value;
  const expResp=await api('/api/v1/ab'), portResp=await api('/api/v1/ab/ports');
  abState.experiments=await expResp.json();
  const historyPorts=(await portResp.json()).ports||[];
  abState.ports=[...new Set([...historyPorts,...abState.experiments.map(x=>x.port)])].sort((a,b)=>a-b);
  renderABExperiments();
  el('abPort').replaceChildren();
  for (const port of abState.ports) el('abPort').add(new Option(String(port),String(port)));
  if (selected && abState.ports.map(String).includes(selected)) el('abPort').value=selected;
  if (!abState.ports.length) {
    abState.summary=null; abState.series=null; abState.statistics=null; abState.rollout=null; el('abAnalysis').hidden=true; el('abNoData').hidden=false; return;
  }
  const port=Number(el('abPort').value), w=abWindow();
  const query='port='+port+'&from='+w.from+'&to='+w.to;
  const summaryResp=await api('/api/v1/ab/summary?'+query);
  const seriesResp=await api('/api/v1/ab/series?'+query+'&tier='+w.tier);
  const analysisResp=await api('/api/v1/ab/analysis?'+query);
  const rollout=await optionalJSON('/api/v1/ab/rollout?port='+port);
  abState.summary=await summaryResp.json(); abState.series=await seriesResp.json(); abState.statistics=await analysisResp.json(); abState.rollout=rollout;
  renderABAnalysis();
}
async function downloadABReport(control) {
  if (!el('abPort').value) { message('没有可导出的实验端口',true); return; }
  await busy(control,async()=>{
    const port=Number(el('abPort').value), w=abWindow();
    const r=await api('/api/v1/ab/report?port='+port+'&from='+w.from+'&to='+w.to+'&tier='+w.tier);
    const blob=await r.blob(),a=document.createElement('a'); a.href=URL.createObjectURL(blob); a.download='brutal-ab-port-'+port+'.zip'; a.click(); setTimeout(()=>URL.revokeObjectURL(a.href),1000);
  });
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
el('portForm').onsubmit=async e=>{e.preventDefault(); await busy(e.submitter,async()=>{await api('/api/v1/ports','POST',{port:Number(el('port').value),rate_mbps:Number(el('rate').value),gain:Number(el('gain').value),enabled:el('enabled').checked}); el('portDialog').close(); message('规则已保存'); await refresh();});};
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

el('addAB').onclick=()=>{el('abForm').reset();el('abNewGain').value='20';el('abNewPercent').value='5';el('abOrchestrate').checked=true;el('abRolloutStagesInput').value='5,10,25,50,100';el('abRolloutWindow').value='60';el('abDialog').showModal();};
el('closeAB').onclick=()=>el('abDialog').close();
el('abNewPercent').onchange=()=>{const p=Number(el('abNewPercent').value);if(p===0){el('abOrchestrate').checked=false;el('abRolloutStagesInput').value='';return;}const standard=[5,10,25,50,100];el('abRolloutStagesInput').value=[p,...standard.filter(x=>x>p)].filter((v,i,a)=>a.indexOf(v)===i).join(',');};
el('abForm').onsubmit=async e=>{e.preventDefault();await busy(e.submitter,async()=>{const stages=el('abRolloutStagesInput').value.split(',').map(x=>Number(x.trim())).filter(Number.isFinite);const body={port:Number(el('abNewPort').value),rate_mbps:Number(el('abNewRate').value),gain:Number(el('abNewGain').value),canary_percent:Number(el('abNewPercent').value),analysis_plan:{alpha:Number(el('abPlanAlpha').value),power:Number(el('abPlanPower').value),expected_app_success_percent:Number(el('abPlanSuccess').value),app_success_ni_margin_pp:Number(el('abPlanNI').value),max_retrans_delta_pp:Number(el('abPlanRetrans').value),max_mean_rtt_delta_percent:Number(el('abPlanRTT').value),min_goodput_delta_percent:Number(el('abPlanGoodput').value),bootstrap_block_minutes:Number(el('abPlanBlock').value)},rollout_plan:el('abOrchestrate').checked?{stages,observation_window_seconds:Number(el('abRolloutWindow').value)*60}:undefined};await api('/api/v1/ab','POST',body);el('abDialog').close();message(el('abOrchestrate').checked?'A/B 实验已创建，统计计划与阶段窗口已固化':'A/B 实验已创建，统计计划已固化');await refreshAB();});};
el('refreshAB').onclick=()=>busy(el('refreshAB'),async()=>{await refreshAB();message('A/B 实验数据已刷新');});
el('abPort').onchange=()=>refreshAB().catch(e=>message(e.message,true));
el('abRange').onchange=()=>refreshAB().catch(e=>message(e.message,true));
el('abMetric').onchange=()=>renderABChart();
el('exportABReport').onclick=()=>downloadABReport(el('exportABReport'));

if (csrf) refresh().catch(()=>logout('会话已失效，请重新登录'));