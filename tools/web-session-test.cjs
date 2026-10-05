const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, '../web/app.js'), 'utf8');

function element() {
  return {
    value:'', textContent:'', hidden:false, checked:false, disabled:false, dataset:{}, children:[], rows:[], options:[],
    classList:{contains:()=>false, toggle(){}},
    append(...items) { this.children.push(...items); },
    replaceChildren(...items) { this.children = items; this.options = items; },
    add(item) { this.options.push(item); },
    insertRow() { const row = {insertCell:element}; this.rows.push(row); return row; },
  };
}
const response = (status, body) => ({ok:status === 200, status, json:async()=>body});
function serverReply(url) {
  if (url === '/api/v1/session') return response(200, {csrf:'restored-csrf'});
  if (url === '/api/v1/status') return response(200, {ports:[], configured_ports:[], allowed_ips:[], bpf_attached:true, version:'test'});
  if (url === '/api/v1/budgets') return response(200, {budgets:[], status:[]});
  return response(200, {});
}
async function panel(fetch = serverReply) {
  const nodes = new Map();
  const get = id => { if (!nodes.has(id)) nodes.set(id, element()); return nodes.get(id); };
  get('app').hidden = true;
  get('retrySession').hidden = true;
  const calls = [];
  const context = vm.createContext({
    console, URLSearchParams, Option:function(text, value) { this.text = text; this.value = value; },
    fetch:async(url, options)=>{ calls.push({url, options}); return fetch(url, options); },
    document:{getElementById:get, querySelectorAll:()=>[], querySelector:()=>element(), createElement:element},
  });
  await vm.runInContext(source, context);
  return {nodes, get, calls, context, run:code=>vm.runInContext(code, context)};
}

(async()=>{
  const restored = await panel(); // No sessionStorage or localStorage exists in this context.
  assert.equal(restored.calls[0].url, '/api/v1/session');
  assert.equal(restored.get('login').hidden, true);
  assert.equal(restored.get('app').hidden, false);
  assert.equal(restored.run('csrf'), 'restored-csrf');
  assert.equal(restored.run("endpoint('112.24.229.65',44791)"), '112.24.229.65:44791');
  assert.equal(restored.run("endpoint('2001:db8::2',44791)"), '[2001:db8::2]:44791');
  assert.equal(restored.run("endpoint('fe80::2%eth0',443)"), '[fe80::2%eth0]:443');

  restored.context.fetch = async()=>{throw new Error('network unavailable');};
  await restored.run('restoreLogin()');
  assert.equal(restored.get('app').hidden, false, 'network error must preserve logged in UI');
  assert.equal(restored.run('csrf'), 'restored-csrf');
  assert.match(restored.get('notice').textContent, /刷新数据/);

  const offline = await panel(async()=>{throw new Error('offline');});
  assert.equal(offline.get('loginForm').hidden, true, 'unverified cookie must not be treated as signed out');
  assert.equal(offline.get('retrySession').hidden, false);
  offline.context.fetch = async url=>serverReply(url);
  await offline.get('retrySession').onclick();
  assert.equal(offline.get('app').hidden, false, 'retry must restore the existing cookie');

  let expire = false;
  const signedIn = await panel(async url=>{
    if (url === '/api/v1/ports') return response(401, {error:'business request failed'});
    if (url === '/api/v1/session' && expire) return response(401, {error:'session expired'});
    return serverReply(url);
  });
  await assert.rejects(signedIn.run("api('/api/v1/ports')"));
  assert.equal(signedIn.get('app').hidden, false, 'business 401 with valid session must not log out');
  expire = true;
  await assert.rejects(signedIn.run("api('/api/v1/ports')"));
  assert.equal(signedIn.get('app').hidden, true);
  assert.equal(signedIn.get('loginForm').hidden, false);
  assert.equal(signedIn.run('csrf'), '');

  const loggedOut = await panel(async()=>response(401, {error:'login required'}));
  assert.equal(loggedOut.get('loginForm').hidden, false);
  assert.equal(loggedOut.get('retrySession').hidden, true);
  let loginBody;
  loggedOut.context.fetch = async(url, options)=>{
    if (url === '/api/v1/login') { loginBody = JSON.parse(options.body); return response(200, {csrf:'new-csrf'}); }
    return serverReply(url);
  };
  for (const remember of [false, true]) {
    loggedOut.get('rememberMe').checked = remember;
    loggedOut.get('password').value = 'test-password';
    await loggedOut.get('loginForm').onsubmit({preventDefault(){}, submitter:element()});
    assert.deepEqual(loginBody, {password:'test-password', remember_me:remember});
    assert.equal(loggedOut.get('password').value, '');
  }

  loggedOut.context.fetch = async()=>response(503, {error:'cannot revoke session'});
  await loggedOut.get('logout').onclick();
  assert.equal(loggedOut.get('app').hidden, false, 'logout failure must not claim success');
  assert.match(loggedOut.get('notice').textContent, /cannot revoke session/);
  let logoutOptions;
  loggedOut.context.fetch = async(url, options)=>{ logoutOptions = options; return response(200, {logged_out:true}); };
  await loggedOut.get('logout').onclick();
  assert.equal(logoutOptions.headers['X-CSRF-Token'], 'new-csrf');
  assert.equal(loggedOut.get('app').hidden, true);
  assert.equal(loggedOut.run('csrf'), '');
  console.log('web session and endpoint regression checks passed');
})().catch(error=>{console.error(error); process.exitCode = 1;});
