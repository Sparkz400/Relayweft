// Controller tests without a browser. Layout/rendering still needs visual QA.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const test = require('node:test');
const source = fs.readFileSync('static/app.js', 'utf8');
class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.attrs = {}; this.events = {}; this.disabled = false; }
  append(...nodes) { this.children.push(...nodes); }
  setAttribute(k, v) { this.attrs[k] = v; }
  addEventListener(k, v) { this.events[k] = v; }
  get textContent() { return this.children.map(c => c instanceof Element ? c.textContent : c).join(''); }
  set textContent(v) { this.children = v ? [v] : []; }
  get value() { return this._value === undefined ? (this.tag === 'textarea' ? this.textContent : '') : this._value; }
  set value(v) { this._value = v; }
  async click() { if (!this.disabled) await this.events.click(); }
}
const find = (root, predicate) => [root, ...root.children.flatMap(c => c instanceof Element ? find(c, predicate) : [])].filter(predicate);
function harness(api, extra = {}) {
  const ctx = { Node: Element, document: { createElement: t => new Element(t), createTextNode: t => t }, api,
    S: {snap:{running:false}}, toast: () => {}, confirm: () => true, closeDrawer: () => {}, ...extra };
  vm.createContext(ctx);
  vm.runInContext(source.slice(source.indexOf('function h('), source.indexOf('// Static, trusted SVG')) + '\n' +
    source.slice(source.indexOf('async function drawMemory('), source.indexOf('async function drawHistory(')), ctx);
  return ctx;
}
test('memory shows existing text, edits by revision, adds and removes notes', async () => {
  const calls = [];
  let notes = [{id:'note-1', text:'<script>literal context</script>', reason:'included'}];
  let revision = 1;
  const ctx = harness(async (method, path, body) => {
    assert.equal(path, '/api/memory');
    if (method === 'POST') {
      calls.push(body); assert.equal(body.revision, String(revision));
      if (body.delete) notes = notes.filter(n => n.id !== body.id);
      else if (body.id) notes[0].text = body.text;
      else notes.push({id:'new', text:body.text, reason:'included'});
      revision++;
    }
    return {entries:notes, revision:String(revision), prompt:notes.map(n=>n.text).join('\n')};
  });
  const body = new Element('div');
  await ctx.drawMemory(body);
  let editor = find(body, e => e.attrs['aria-label'] === 'Project note')[0];
  assert.equal(editor.value, '<script>literal context</script>');
  assert.equal(find(body,e=>e.tag==='script').length,0);
  editor.value = 'Revised decision';
  await find(body,e=>e.tag==='button' && e.textContent==='Save note')[0].click();
  assert.equal(calls[0].text,'Revised decision');
  assert.equal(calls[0].id,'note-1');
  find(body,e=>e.attrs['aria-label']==='New project note')[0].value='New decision';
  await find(body,e=>e.tag==='button' && e.textContent==='Add note')[0].click();
  assert.equal(notes.length,2);
  // Remove's event intentionally does not await the refresh; drain its promises.
  await find(body,e=>e.tag==='button' && e.textContent==='Remove')[0].click();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(notes.length,1);
  assert.equal(notes[0].text,'New decision');
});
test('memory pins, sets sources, re-confirms changed notes and previews a task', async () => {
  const calls = [], gets = [];
  const note = {id:'n1', text:'Use the retry helper', reason:'one of the newest notes', included:true, pinned:false,
    sources:[{path:'net/retry.go', state:'changed'}]};
  const ctx = harness(async (method, path, body) => {
    if (method === 'POST') calls.push(body); else gets.push(path);
    return {entries:[note], revision:'1', prompt:''};
  });
  const body = new Element('div');
  await ctx.drawMemory(body);
  const button = (t) => find(body, e => e.tag === 'button' && e.textContent === t)[0];
  await button('Pin as convention').click();
  assert.deepEqual(JSON.parse(JSON.stringify(calls.pop())), {revision:'1', id:'n1', pin:true});
  await button('Re-confirm').click();
  assert.equal(calls.pop().refresh, true);
  await button('Save note').click();
  assert.equal(calls.pop().refs, undefined, 'unchanged sources are not re-recorded');
  find(body, e => e.attrs['aria-label'] === 'Source files')[0].value = 'net/retry.go, net/backoff.go';
  await button('Save note').click();
  assert.deepEqual(Array.from(calls.pop().refs), ['net/retry.go', 'net/backoff.go']);
  find(body, e => e.attrs['aria-label'] === 'New project note')[0].value = 'Wrap errors with %w';
  await button('Add pinned convention').click();
  const added = calls.pop();
  assert.equal(added.pin, true); assert.equal(added.id, undefined); assert.equal(added.refs, undefined);
  find(body, e => e.attrs['aria-label'] === 'Preview for a task')[0].value = 'fix retry & backoff';
  await button('Preview').click();
  assert.equal(gets.pop(), '/api/memory?task=fix%20retry%20%26%20backoff');
});
test('explain shows the shape, each run\'s reason, estimates and escalations', async () => {
  const gets = [], opened = [];
  const fixture = {ID:'t-1', Task:'Fix the parser', Status:'done', Duration:65e9, Mode:'routed', Shape:'planned', ShapeWhy:'broad task; the planner made 2 steps',
    headline:'several agents (2 steps)', agents:'planner 1, worker 2',
    Reviews:[{What:'review-final', Outcome:'runs', Why:'the change touches auth'}],
    Runs:[{Step:'greet', Attempt:2, Role:'worker_high', Provider:'claude', Rule:'error-repeats', Reason:'same error twice: worker -> worker_high', Ran:true, OK:true, Duration:3e9,
      Tokens:{input:5000, output:700}, Estimate:{tokens:{low:2000, mid:4000, high:8000}, source:'this repo', samples:4}},
      {Step:'notes', Attempt:1, Role:'worker', Provider:'codex', Rule:'limit-fallback', Fallback:true, From:'claude', Ran:false, Estimate:{tokens:{mid:0}, source:'no history'}}],
    run_labels:['claude:opus@high', 'codex:gpt-5'], run_deltas:['+43%', ''],
    Providers:[{Provider:'claude', Runs:1, Tokens:5700, Rules:['error-repeats ×1']}],
    EstTokens:{low:2000, mid:4000, high:8000}, EstUSD:{mid:0}, Tokens:{input:5000, output:700}, NoHistory:1,
    Escalations:[{Step:'greet', Attempt:2, What:'worker → worker_high', Cause:'the same error twice: undefined: fmt'}, {Step:'fix', Attempt:1, What:'fix round 1', Cause:'checks fail'}],
    escalate_at:['greet attempt 2', ''], Notes:[]};
  const ctx = harness(async (method, path) => { gets.push(path); return fixture; }, {
    human: n => String(n), dur: ms => Math.round(ms / 1000) + 's', fresh: t => t ? (t.input || 0) - (t.cached || 0) + (t.output || 0) : 0,
    oneLine: s => s, openDrawer: name => opened.push(name)});
  ctx.S.explainId = 't-1'; ctx.S.explainFrom = 'history';
  const body = new Element('div');
  await ctx.drawExplain(body);
  assert.equal(gets[0], '/api/explain/t-1');
  const text = body.textContent;
  for (const want of ['Why several agents (2 steps)', 'planned: broad task; the planner made 2 steps', 'greet (attempt 2)', 'claude:opus@high',
    'why: error-repeats — same error twice: worker -> worker_high', 'est 4000 (2000–8000, this repo, 4 runs) +43%', '(moved from claude)',
    'no result logged', 'Final review runs: the change touches auth', 'greet attempt 2: worker → worker_high — the same error twice',
    'fix round 1 — checks fail', '1 of the estimates had no history']) {
    assert.ok(text.includes(want), want + ' missing in: ' + text);
  }
  await find(body, e => e.tag === 'button' && e.textContent === 'Back to history')[0].click();
  assert.deepEqual(opened, ['history']);
});
test('recovery requires undo preview/confirmation and blocks actions while busy', async () => {
  const calls=[];
  let accepted=false;
  const item={id:'fixture',task:'Repair parser',status:'interrupted',done:1,steps:2,can_resume:true,can_undo:true,branches:['rw/saved']};
  const ctx=harness(async(method,path,body)=>{
    calls.push({method,path,body});
    if(path==='/api/recovery') return [item];
    return {plan:{Changes:['M parser.go'],Others:[]}};
  },{confirm:()=>accepted,act:async()=>{throw new Error('busy action must not run')}});
  const body=new Element('div');await ctx.drawRecovery(body);
  assert.ok(body.textContent.includes('rw/saved'));
  await find(body,e=>e.tag==='button' && e.textContent==='Preview undo')[0].click();
  assert.equal(calls.filter(c=>c.body?.apply).length,0);
  accepted=true;
  await find(body,e=>e.tag==='button' && e.textContent==='Preview undo')[0].click();
  assert.equal(calls.filter(c=>c.body?.apply).length,1);
  ctx.S.snap.running=true;await ctx.drawRecovery(body);
  for(const b of find(body,e=>e.tag==='button' && ['Resume','Preview undo'].includes(e.textContent))) {
    assert.equal(b.disabled,true);await b.click();
  }
});

test('undo preview distinguishes changes from files that will be kept', async () => {
  let confirmation = '';
  const ctx = harness(async(method, path) => {
    if (path === '/api/recovery') return [{id:'fixture',task:'Repair parser',status:'failed',done:1,steps:2,can_undo:true}];
    return {plan:{Changes:['M parser.go','M personal notes.txt'],Unreported:['personal notes.txt'],Edited:['parser.go'],Skipped:['vendor'],Missing:['offline'],Others:[{Repo:'docs',Changes:['M README.md','M draft.md'],Unreported:['draft.md']}]}};
  }, {confirm: text => { confirmation = text; return false; }});
  const body = new Element('div');
  await ctx.drawRecovery(body);
  await find(body,e=>e.tag==='button' && e.textContent==='Preview undo')[0].click();
  assert.ok(confirmation.includes('M parser.go'));
  assert.ok(confirmation.includes('M README.md'));
  assert.ok(!confirmation.includes('M personal notes.txt'));
  assert.ok(!confirmation.includes('M draft.md'));
  assert.ok(confirmation.includes('Kept (unreported): personal notes.txt'));
  assert.ok(confirmation.includes('Kept (unreported): draft.md'));
  assert.ok(confirmation.includes('Missing repositories: offline'));
  assert.ok(confirmation.includes('Submodules kept: vendor'));
});

function inspectionFixture() {
  return {here:true, recovery:{id:'inspect-1', status:'failed', can_resume:true, can_undo:true, branches:['rw/preserved']}, report:{
    ID:'inspect-1', Task:'Repair <script>alert(1)</script> parser', Status:'failed', Dir:'/project', Duration:65e9,
    Summary:'Parser updated; documentation still missing.', CostLine:'$0.12 API-equivalent', Tokens:{input:4000,output:200,cost_usd:0.12,incomplete:true},
    Acceptance:{agents:{status:'pass'},checks:{status:'pass'},requirements:{status:'fail'},explicit:true,criteria:[
      {id:'R1',text:'Keep empty fields',status:'evidence',test:'parser_test.go: TestEmpty',evidence:'parser.go:12'},
      {id:'R2',text:'Document strict mode',status:'unmet',note:'README has no flag example'}]},
    Steps:[{ID:'parser',Title:'Fix parser',Result:'ok',Final:'Implemented'}],
    Checks:[{Kind:'verify',Command:'go test ./...',OK:false,Duration:1e9},{Kind:'verify',Command:'go test ./...',OK:true,Duration:2e9,Scope:'full',Why:'Final full suite'},
      {Kind:'hook',Command:'lint',OK:false,Duration:1e9}],
    Routes:[{Step:'parser',Provider:'claude',Model:'sonnet',Role:'worker',Rule:'limit-fallback',Reason:'codex at usage limit',Fallback:true,From:'codex',Ran:true,OK:true}],
    Why:{Shape:'one agent',ShapeWhy:'small task',EstTokens:{mid:5000,low:3000,high:8000},NoHistory:1,Escalations:[{What:'fallback',Cause:'usage limit'}]},
    Diff:{Files:[{Path:'parser.go',Status:'M',Add:1,Del:1,Lines:[{Kind:'del',Tokens:[{Text:'-old'}]},{Kind:'add',Tokens:[{Text:'+<script>literal</script>'}]}]}],Add:1,Del:1},Notes:[]}};
}
function inspectionHarness(api, extra={}) {
  const ctx = harness(api, {human:String,dur:ms=>Math.round(ms/1000)+'s',fresh:t=>(t?.input||0)+(t?.output||0), ...extra});
  ctx.S.drawer='inspection';return ctx;
}
test('inspection combines evidence and separates resolved checks from remaining failures', async () => {
  const v=inspectionFixture(),calls=[],ctx=inspectionHarness(async(method,path,data)=>{calls.push({method,path,data});return v;});
  const body=new Element('div');await ctx.drawInspection(body,'last');assert.equal(ctx.S.inspectionId,'inspect-1');
  for (const title of ['Requirements','Changed files','Checks','Remaining failures','Routing decisions','Cost','Recovery actions']) assert.equal(find(body,e=>e.attrs['aria-label']===title).length,1,title);
  const remaining=find(body,e=>e.attrs['aria-label']==='Remaining failures')[0];
  assert.ok(remaining.textContent.includes('Document strict mode'));assert.ok(remaining.textContent.includes('lint'));
  assert.ok(!remaining.textContent.includes('go test ./...'),'resolved failure still listed');
  for(const text of ['earlier attempt','parser_test.go: TestEmpty','Fallback from codex','lower bound','rw/preserved']) assert.ok(body.textContent.includes(text),text);
  const detail=find(body,e=>e.className==='inspection-file')[0];detail.open=true;detail.events.toggle();detail.events.toggle();
  assert.equal(find(detail,e=>e.tag==='pre').length,1);assert.ok(detail.textContent.includes('+<script>literal</script>'));
  assert.ok(!detail.textContent.includes('++<script>'));assert.equal(find(body,e=>e.tag==='script').length,0);
  await find(body,e=>e.tag==='button' && e.textContent==='Refresh result')[0].click();assert.equal(calls.at(-1).path,'/api/results/inspect-1');
});
test('inspection handles missing evidence and cross-project recovery', async()=>{
  const v={here:false,recovery:{id:'old',status:'done'},report:{ID:'old',Status:'done',Task:'Older task',Notes:['no session log found']}};
  const ctx=inspectionHarness(async()=>v),body=new Element('div');await ctx.drawInspection(body,'old');
  for(const want of ['Requirement verification was not recorded','Snapshot diff unavailable','No check results recorded','No routing decisions recorded','No task cost summary recorded','Open this task’s project','Missing or unchecked evidence is not a pass']) assert.ok(body.textContent.includes(want),want);
  assert.equal(find(body,e=>e.tag==='button' && ['Retry unfinished','Preview undo'].includes(e.textContent)).length,0);
});
test('inspection ignores stale replies and errors after switching tasks or drawers',async()=>{
  const pending=[],ctx=inspectionHarness(()=>new Promise((resolve,reject)=>pending.push({resolve,reject}))),body=new Element('div');
  const first=ctx.drawInspection(body,'old'),second=ctx.drawInspection(body,'new');pending[1].resolve(inspectionFixture());await second;
  pending[0].resolve({report:{ID:'wrong'}});await first;assert.equal(ctx.S.inspectionId,'inspect-1');
  const third=ctx.drawInspection(body,'missing');ctx.S.drawer='history';body.textContent='History';pending[2].reject(new Error('missing'));
  await third;assert.equal(body.textContent,'History');
});
test('inspection recovery previews undo, refreshes the selected task and retries its id',async()=>{
  const calls=[],v=inspectionFixture();let confirmed=false,closed=false;
  const ctx=inspectionHarness(async(method,path,data)=>{calls.push({method,path,data});return path.startsWith('/api/results/') ? v : {plan:{Changes:['M parser.go']}};},
    {confirm:()=>confirmed,closeDrawer:()=>{closed=true;},act:async(method,path,data)=>{calls.push({method,path,data});return {};}});
  const body=new Element('div');await ctx.drawInspection(body,'inspect-1');const button=label=>find(body,e=>e.tag==='button' && e.textContent===label)[0];
  await button('Preview undo').click();assert.equal(calls.filter(c=>c.data?.apply).length,0);
  confirmed=true;await button('Preview undo').click();assert.equal(calls.filter(c=>c.data?.apply).length,1);assert.equal(calls.at(-1).path,'/api/results/inspect-1');
  await button('Retry unfinished').click();assert.equal(calls.at(-1).path,'/api/resume');assert.equal(calls.at(-1).data.id,'inspect-1');assert.equal(closed,true);
  ctx.S.snap.running=true;await ctx.drawInspection(body,'inspect-1');assert.equal(button('Preview undo').disabled,true);assert.equal(button('Retry unfinished').disabled,true);
});

test('inspection never promotes skipped or narrowed checks to full verification',async()=>{
  const v=inspectionFixture();v.report.Acceptance.checks.status='fail';
  v.report.Checks=[{Kind:'verify',Command:'suite',Scope:'full',OK:false},{Kind:'verify',Command:'suite',Scope:'affected',OK:true},
    {Kind:'verify',Command:'suite',Scope:'full',OK:true,Why:'nothing to run: no affected packages'}];
  const ctx=inspectionHarness(async()=>v),body=new Element('div');await ctx.drawInspection(body);
  assert.ok(find(body,e=>e.attrs['aria-label']==='Remaining failures')[0].textContent.includes('verify: suite'));
  assert.ok(find(body,e=>e.attrs['aria-label']==='Checks')[0].textContent.includes('skipped'));
  // A saved final acceptance pass supersedes earlier failed verify commands.
  v.report.Acceptance.checks.status='pass';v.report.Status='done';
  v.report.Reviews=[{Checkpoint:'final',Approve:false,Advice:'Old review before fixes'}];
  await ctx.drawInspection(body);
  const remaining=find(body,e=>e.attrs['aria-label']==='Remaining failures')[0].textContent;
  assert.ok(!remaining.includes('verify: suite'));assert.ok(!remaining.includes('Old review before fixes'));
});
