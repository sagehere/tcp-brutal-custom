const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const nodes = new Map();
const node = () => ({value:'86400',textContent:'',hidden:false,dataset:{},classList:{contains:()=>false,toggle(){}},setAttribute(){},append(){},replaceChildren(){}});
const context = vm.createContext({
  sessionStorage:{getItem:()=>null}, console, URLSearchParams,
  document:{getElementById:id=>{if(!nodes.has(id))nodes.set(id,node());return nodes.get(id);},querySelectorAll:()=>[],querySelector:()=>node(),createElement:()=>node(),createElementNS:()=>node()},
});
vm.runInContext(fs.readFileSync(path.join(__dirname,'../web/app.js'),'utf8'),context);
const segments = vm.runInContext("seriesSegments([{time:0,expected:1},{time:10,expected:NaN},{time:20,expected:2}], 'expected', 2, 0, 20)",context);
assert.equal(segments.length,2,'missing samples must break chart lines');
assert.equal(vm.runInContext("seriesSegments([{time:0,expected:1},{time:30,expected:2}], 'expected', 2, 0, 30, 10).length",context),2,'absent buckets must break chart lines');
nodes.get('metricKind').value='send';
context.data={samples:[{time:10,timing_valid:true,acked_mbps:20,sent_mbps:22},{time:10,timing_valid:true,acked_mbps:30,sent_mbps:33}]};
vm.runInContext('api=async()=>({json:async()=>data})',context);
(async()=>{
  await vm.runInContext('draw()',context);
  assert.match(nodes.get('chartNote').textContent,/55.00 Mbps/,'sum group rates, not durations');
  context.data.samples[1].timing_valid=false;
  context.data.samples[1].acked_mbps=null;
  context.data.samples[1].sent_mbps=null;
  await vm.runInContext('draw()',context);
  assert.match(nodes.get('chartNote').textContent,/暂无有效样本/,'unknown group timing must not be displayed as a zero peak');
  console.log('web rate and gap regression checks passed');
})().catch(error=>{console.error(error);process.exitCode=1;});
