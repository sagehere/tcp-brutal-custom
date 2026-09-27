let csrf = sessionStorage.getItem('csrf') || '';
const el = id => document.getElementById(id);
const say = text => { el('notice').textContent = text; };
async function api(path, method='GET', body) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const r = await fetch(path, {method, credentials:'same-origin', headers, body:body===undefined?undefined:JSON.stringify(body)});
  if (!r.ok) { let e; try { e=(await r.json()).error; } catch {} throw new Error(e || `HTTP ${r.status}`); }
  return r;
}
function textCell(row, value) { const c=row.insertCell();c.textContent=String(value);return c; }
function stat(label, value) { const d=document.createElement('div');d.className='stat';const a=document.createElement('span');a.textContent=label;const b=document.createElement('strong');b.textContent=value;d.append(a,b);el('stats').append(d); }
const mb = bytes => (bytes/1e6).toFixed(1) + ' MB';
async function refresh() {
  const data=await (await api('/api/v1/status')).json();
  el('login').style.display='none';el('app').style.display='block';
  el('stats').replaceChildren();el('ports').replaceChildren();
  stat('模块版本',data.version);stat('eBPF',data.bpf_attached?'已附着':'故障');stat('规则数',data.configured_ports.length);
  let sent=0,failed=0;
  const byPort=new Map(data.configured_ports.map(p=>[p.port,p]));
  for (const p of data.ports) { sent+=p.sent;if(p.active)failed+=p.selector.failure; }
  stat('累计发送',mb(sent));stat('接管失败',failed);
  for (const cfg of data.configured_ports) {
    const p=data.ports.find(x=>x.port===cfg.port&&x.active);
    const row=el('ports').insertRow();
    textCell(row,cfg.port);textCell(row,p?'生效':cfg.enabled?'未生效':'停用');textCell(row,cfg.rate_mbps);textCell(row,(cfg.gain/10).toFixed(1));textCell(row,p?.members??0);textCell(row,mb(p?.sent??0));textCell(row,p&&p.sent?(100*p.retrans/p.sent).toFixed(2)+'%':'无样本');textCell(row,p?.rtt_samples?((p.rtt_sum_us/p.rtt_samples)/1000).toFixed(1)+' ms':'无样本');textCell(row,p?.selector.failure??0);
    const cell=row.insertCell();
    const edit=document.createElement('button');edit.textContent='编辑';edit.onclick=()=>{el('port').value=cfg.port;el('rate').value=cfg.rate_mbps;el('gain').value=cfg.gain;el('enabled').checked=cfg.enabled;window.scrollTo({top:el('portForm').offsetTop,behavior:'smooth'});};
    const del=document.createElement('button');del.textContent='删除';del.className='danger';del.onclick=async()=>{if(!confirm(`删除端口 ${cfg.port}？旧连接会持续到自然结束。`))return;try{await api('/api/v1/ports/'+cfg.port,'DELETE');say('已删除');await refresh();}catch(e){say(e.message);}};
    cell.append(edit,del);
  }
  el('metricPort').replaceChildren(new Option('全部','0'));
  for (const p of data.configured_ports) el('metricPort').add(new Option(String(p.port),String(p.port)));
  el('host').value=data.web_host;el('webPort').value=data.web_port;el('allowIPs').value=(data.allowed_ips||[]).join(',');
  el('job').textContent=data.update?.state?`${data.update.state}: ${data.update.detail||''}`:'';
  await draw();
}
function metricParams() {
  const seconds=Number(el('range').value),to=Math.floor(Date.now()/1000),tier=seconds>90*86400?'hour':seconds>7*86400?'minute':'raw';
  return `tier=${tier}&from=${to-seconds}&to=${to}&port=${el('metricPort').value}`;
}
async function draw() {
  const data=await(await api('/api/v1/metrics?'+metricParams())).json();
  const bins=new Map();for(const row of data.samples){const x=bins.get(row.time)||{sent:0,retrans:0,rtt:0,count:0};x.sent+=row.sent;x.retrans+=row.retrans;x.rtt+=row.rtt_sum_us;x.count+=row.rtt_samples;bins.set(row.time,x);}
  const kind=el('metricKind').value;
  const range=Number(el('range').value),interval=range>90*86400?3600:range>7*86400?60:10;
  const points=[...bins].sort((a,b)=>a[0]-b[0]).filter(([,x])=>kind==='sent'||(kind==='retrans'?x.sent>0:x.count>0)).map(([t,x])=>[t,kind==='sent'?x.sent*8/interval/1e6:kind==='retrans'?100*x.retrans/x.sent:x.rtt/x.count/1000]);
  if(!points.length){el('line').setAttribute('points','');el('chartNote').textContent='暂无历史数据';return;}
  const peak=Math.max(...points.map(p=>p[1])),scale=Math.max(1,peak),minTime=points[0][0],span=Math.max(1,points.at(-1)[0]-minTime);
  el('line').setAttribute('points',points.map(([t,v])=>`${20+760*(t-minTime)/span},${200-180*v/scale}`).join(' '));
  el('chartNote').textContent=kind==='sent'?`发送速率；峰值 ${peak.toFixed(2)} Mbps`:kind==='retrans'?`重传字节÷发送字节；峰值 ${peak.toFixed(2)}%`:`加权平均 RTT；峰值 ${peak.toFixed(1)} ms`;
}
el('loginForm').onsubmit=async e=>{e.preventDefault();try{const r=await api('/api/v1/login','POST',{password:el('password').value});csrf=(await r.json()).csrf;sessionStorage.setItem('csrf',csrf);el('password').value='';say('已登录');await refresh();}catch(x){say(x.message);}};
el('portForm').onsubmit=async e=>{e.preventDefault();try{await api('/api/v1/ports','POST',{port:Number(el('port').value),rate_mbps:Number(el('rate').value),gain:Number(el('gain').value),enabled:el('enabled').checked});say('规则已保存');await refresh();}catch(x){say(x.message);}};
el('settingsForm').onsubmit=async e=>{e.preventDefault();try{await api('/api/v1/settings','PUT',{host:el('host').value,port:Number(el('webPort').value),allowed_ips:el('allowIPs').value.split(',').map(x=>x.trim()).filter(Boolean)});say('已保存；请重启 Web 服务生效');}catch(x){say(x.message);}};
el('changePassword').onsubmit=async e=>{e.preventDefault();try{await api('/api/v1/password','PUT',{password:el('newPassword').value});csrf='';sessionStorage.removeItem('csrf');el('app').style.display='none';el('login').style.display='block';say('密码已更新，请重新登录');}catch(x){say(x.message);}};
el('refresh').onclick=()=>refresh().catch(x=>say(x.message));
el('range').onchange=el('metricPort').onchange=el('metricKind').onchange=()=>draw().catch(x=>say(x.message));
async function download(format){try{const r=await api('/api/v1/metrics?'+metricParams()+'&format='+format);const blob=await r.blob(),a=document.createElement('a');a.href=URL.createObjectURL(blob);a.download='brutal-history.'+format;a.click();setTimeout(()=>URL.revokeObjectURL(a.href),1000);}catch(x){say(x.message);}}
el('exportCSV').onclick=()=>download('csv');el('exportJSON').onclick=()=>download('json');
el('checkUpdate').onclick=async()=>{try{const x=await(await api('/api/v1/update/check')).json();el('releaseNotes').textContent=`当前 ${x.installed}；最新 ${x.latest}\n${x.notes}`;}catch(x){say(x.message);}};
el('update').onclick=async()=>{if(!confirm('维护升级会中断受管 TCP 连接。现在开始？'))return;try{const r=await api('/api/v1/update','POST',{});const x=await r.json();say('升级任务 '+x.job_id+' 已启动');}catch(x){say(x.message);}};
if(csrf)refresh().catch(()=>{csrf='';sessionStorage.removeItem('csrf');});
