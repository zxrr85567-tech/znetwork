// Headless check that the explorer's own JavaScript works against a live node.
// It runs the page's script in a DOM stub, routes every fetch through the real
// node RPC and fails if the UI uses a non-relative URL or renders nothing.
//
//   node web/explorer/live-ui-check.js [rpc-url]
//
// The harness file is generated from web/explorer/index.html by the same script,
// so there is no second copy of the UI to drift out of date.
const { execFileSync } = require('child_process');
const vm=require('vm'), fs=require('fs'), os=require('os'), path=require('path');
const RPC=process.argv[2]||'http://127.0.0.1:26657/';
let nonRelative=0;

const sandbox={console,Date,Math,Number,JSON,String,Error,Promise,setTimeout};
sandbox.globalThis=sandbox;
sandbox.__fetch=async(u,o)=>{ if(u!==''){nonRelative++;throw new Error('non-relative: '+u);} return fetch(RPC,o); };
vm.createContext(sandbox);
const src=fs.readFileSync(path.join(os.tmpdir(),"explorer_harness.js"),"utf8");
vm.runInContext(src,sandbox);
const api=sandbox.__api, dom=sandbox.__dom;
if(!api){console.error("no api");process.exit(1);}
(async()=>{
  await sandbox.__ready;
  const out=[];
  // head + blocks
  out.push(['head cards',(dom['head'].innerHTML.match(/class="card"/g)||[]).length]);
  out.push(['block rows',(dom['blocks'].innerHTML.match(/<tr>/g)||[]).length]);
  // block detail for the head height
  const headTxt=dom['head'].innerHTML; const m=headTxt.match(/Height<\/div><div class="v">(\d+)/);
  if(m){ await api.showBlock(m[1]); if(dom['qmsg']&&dom['qmsg'].textContent) console.log('BLOCK ERR:',dom['qmsg'].textContent); out.push(['block detail fields',(dom['detail'].innerHTML.match(/<dt>/g)||[]).length]); }
  // account view
  await api.showAccount('z1xfv23vqzvsh34vmxdyn0ry3645e54eaf38s98y');
  out.push(['account view has balance',/Balance/.test(dom['detail'].innerHTML)]);
  // tx view using a known committed deploy tx
  await api.showTx('728a4ca8f74ca32665e5f5f96f6926c5b4deaf3ccd0b842fbb9af04dc6396653');
  out.push(['tx view has execution',/Execution/.test(dom['detail'].innerHTML)]);
  out.push(['tx view has events',/Events/.test(dom['detail'].innerHTML)]);
  for(const [k,v] of out) console.log(k+':',v);
  const ok = out[0][1]===4 && out[1][1]===10 && nonRelative===0 && out.every(([k,v])=>v===true||typeof v==='number');
  if(!ok){console.log('DETAIL:',dom['detail'].innerHTML.slice(0,600));process.exit(1);}
  console.log('EXPLORER_LIVE_OK non_relative_fetches='+nonRelative);
})().catch(e=>{console.error('FAIL',e);process.exit(1);});
