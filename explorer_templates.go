package main

import "html/template"

// Parsed once at init: template.Must fails the process at startup rather than
// letting a malformed page surface as a 500 on the first request.
var (
	explorerIndexPage = template.Must(template.New("index").Parse(explorerIndexTmpl))
	explorerBlockPage = template.Must(template.New("block").Parse(explorerBlockTmpl))
	explorerTxPage    = template.Must(template.New("tx").Parse(explorerTxTmpl))
	explorerStatsPage = template.Must(template.New("stats").Parse(explorerStatsTmpl))
)

const explorerCSS = `*{margin:0;padding:0;box-sizing:border-box}
:root{--ac:#af0;--ac-h:#cf3}
body{background:#000;color:#b0b0b0;font:15px/1.6 ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;padding:24px 32px}
a{color:var(--ac)}
a:hover{color:var(--ac-h)}
h1,h2{color:#eee;font-weight:normal}
h1{font-size:24px;margin:0}
h2{font-size:18px;margin:0 0 16px}
p{margin:16px 0}
.g{color:var(--ac)}
.d{color:#555}
.header{display:flex;justify-content:space-between;align-items:baseline;flex-wrap:wrap;gap:12px;margin-bottom:8px}
.topnav{font-size:13px;display:flex;gap:16px;margin-bottom:24px}
.search{display:flex;gap:8px;margin-bottom:32px}
.search input{flex:1;background:#000;border:1px solid #333;color:#eee;padding:12px 16px;font:inherit;min-width:0}
.search input:focus{outline:none;border-color:var(--ac)}
.search button{background:var(--ac);border:0;color:#000;padding:12px 24px;cursor:pointer;font:inherit;white-space:nowrap}
.search button:hover{background:var(--ac-h)}
.stat-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:16px;margin-bottom:32px}
.stat{padding:20px;text-align:center}
.stat-v{font-size:24px;color:#eee}
.stat-k{font-size:12px;color:#666;text-transform:uppercase;margin-top:4px}
.panels{display:flex;gap:32px;align-items:flex-start}
.panels>div{flex:1;min-width:0}
.nav{display:flex;gap:16px;margin-bottom:24px}
table{width:100%;border-collapse:collapse;margin:16px 0}
th,td{text-align:left;padding:10px 12px}
th{color:#666;font-weight:normal;font-size:13px;text-transform:uppercase}
tr:hover{background:#0a0a0a}
.hash{color:#666;font-size:13px}
.props{padding:20px 0}
.prop{display:flex;padding:8px 0}
.prop-k{width:140px;color:#666;flex-shrink:0}
.prop-v{flex:1;word-break:break-all}
.prop-v.mono{font-size:13px;color:#888}
.block-columns{display:flex;gap:32px;align-items:flex-start}
.block-columns>div{flex:1;min-width:0}
.tx-columns{display:flex;gap:32px;align-items:flex-start}
.tx-col-left{flex:1;min-width:0;order:0}
.tx-col-right{flex:1;min-width:0;order:1}
.io-card{padding:16px 0;margin-bottom:16px}
.io-card .prop{padding:6px 0}
.io-card .prop-k{width:130px}
.prove-tabs{display:flex;gap:0}
.prove-tab{background:#000;color:#666;border:0;padding:8px 20px;cursor:pointer;font:13px/1.4 monospace}
.prove-tab-active{background:var(--ac);color:#000}
.prove-box{padding:20px 0}
.prove-box input{background:#000;border:1px solid #333;color:#eee;padding:10px 12px;font:13px/1.4 monospace;width:100%;margin-bottom:8px}
.prove-box input:focus{outline:none;border-color:var(--ac)}
.prove-box button{background:var(--ac);border:0;color:#000;padding:10px 24px;cursor:pointer;font:inherit}
.prove-box button:hover{background:var(--ac-h)}
footer{margin-top:48px;padding-top:24px;border-top:1px dashed #333;color:#444;font-size:13px}
@media(max-width:900px){.panels,.block-columns,.tx-columns{flex-direction:column}body{padding:16px}h1{font-size:20px}.stat-grid{grid-template-columns:repeat(auto-fit,minmax(140px,1fr))}}
@media(max-width:500px){.stat-grid{grid-template-columns:1fr 1fr}.stat-v{font-size:18px}.search{flex-direction:column}.search button{padding:12px}.prop{flex-direction:column;gap:2px}.prop-k{width:auto}.io-card .prop-k{width:auto}}`

const explorerProxyFixScript = `<script>
(function(){
var aa=document.querySelectorAll('.topnav a,div a,h1 span');
for(var i=0;i<aa.length;i++){var t=aa[i].textContent.trim();
if(t==='mainnet'||t==='testnet'||t==='TESTNET'){aa[i].remove();}}
})();
</script>`

const explorerLiveScript = `<script>
(function(){
if(!window.EventSource)return;
var es=new EventSource('/events');
function txt(id,v){var el=document.getElementById(id);if(el)el.textContent=v;}
function shorten(h){return h.slice(0,16)+'...';}
function prependBlock(d){
var tbl=document.querySelector('table.recent-blocks');if(!tbl)return;
var head=tbl.rows[0];if(!head)return;
for(var i=1;i<tbl.rows.length;i++){
var first=tbl.rows[i].cells[0];
if(first&&first.textContent.trim()===String(d.height))return;
}
var row=tbl.insertRow(1);
row.insertCell(0).innerHTML='<a href="/block/'+d.height+'">'+d.height+'</a>';
var hc=row.insertCell(1);hc.className='hash';
hc.innerHTML='<a href="/block/'+d.hash+'">'+shorten(d.hash)+'</a>';
row.insertCell(2).textContent='0s ago';
row.insertCell(3).textContent=d.tx_count;
while(tbl.rows.length>21)tbl.deleteRow(tbl.rows.length-1);
}
function bumpConfirmations(d){
var el=document.getElementById('tx-confirmations');if(!el)return;
var at=parseInt(el.getAttribute('data-height'),10);
if(isNaN(at))return;
el.textContent=(d.height-at+1);
}
es.addEventListener('connected',function(ev){
var d;try{d=JSON.parse(ev.data);}catch(e){return;}
txt('stat-height',d.height);
txt('stat-difficulty',d.next_difficulty);
bumpConfirmations(d);
});
es.addEventListener('new_block',function(ev){
var d;try{d=JSON.parse(ev.data);}catch(e){return;}
txt('stat-height',d.height);
txt('stat-difficulty',d.next_difficulty);
prependBlock(d);
bumpConfirmations(d);
});
})();
</script>`

const explorerEggScript = `<script>
(function(){
var egg=document.getElementById('egg');if(!egg)return;
var ne=egg.cloneNode(true);egg.parentNode.replaceChild(ne,egg);egg=ne;
var active=false,hex='0123456789abcdef';
function px(c){c=c.replace('#','');if(c.length===3)c=c[0]+c[0]+c[1]+c[1]+c[2]+c[2];return[parseInt(c.slice(0,2),16),parseInt(c.slice(2,4),16),parseInt(c.slice(4,6),16)];}
function hx(r,g,b){return'#'+((1<<24)+(r<<16)+(g<<8)+b).toString(16).slice(1);}
function lp(a,b,t){return Math.round(a+(b-a)*t);}
function tw(from,to,t){return hx(lp(from[0],to[0],t),lp(from[1],to[1],t),lp(from[2],to[2],t));}
var tn=document.cookie.indexOf('net=testnet')!==-1;
var acO=px(tn?'#f0a':'#af0'),acT=px(tn?'#af0':'#f0a');
var ahO=px(tn?'#f5c':'#cf3'),ahT=px(tn?'#cf3':'#f5c');
egg.addEventListener('click',function(ev){
ev.preventDefault();ev.stopPropagation();
if(active)return;active=true;
document.cookie='net='+(tn?'mainnet':'testnet')+';path=/;max-age=31536000';
var els=document.querySelectorAll('h1,h2,.topnav a,.stat-k,.stat-v,.prop-k,.prop-v,td,th,footer a,button,.nav a');
var saved=[];
for(var i=0;i<els.length;i++){
saved.push(els[i].textContent);
var s='';for(var j=0;j<els[i].textContent.length;j++){var c=els[i].textContent[j];s+=/\s/.test(c)?c:hex[Math.random()*16|0];}
els[i].textContent=s;
}
var t0=Date.now(),dur=1800;
var iv=setInterval(function(){
var t=Math.min(1,(Date.now()-t0)/dur);
document.documentElement.style.setProperty('--ac',tw(acO,acT,t));
document.documentElement.style.setProperty('--ac-h',tw(ahO,ahT,t));
if(t>0.45){var dt=(t-0.45)/0.55;
for(var i=0;i<els.length;i++){var orig=saved[i],cur='';
for(var j=0;j<orig.length;j++){if(Math.random()<dt)cur+=orig[j];else cur+=/\s/.test(orig[j])?orig[j]:hex[Math.random()*16|0];}
els[i].textContent=cur;}
}else{for(var i=0;i<els.length;i++){var s='';
for(var j=0;j<els[i].textContent.length;j++){var c=els[i].textContent[j];s+=/\s/.test(c)?c:hex[Math.random()*16|0];}
els[i].textContent=s;}}
if(t>=1){clearInterval(iv);location.reload();}
},60);
});
})();
</script>`

const explorerIndexTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>blocknet explorer</title>
<meta name="description" content="Zero-knowledge money">
<meta property="og:title" content="blocknet explorer">
<meta property="og:description" content="Zero-knowledge money">
<meta property="og:image" content="https://blocknetcrypto.com/blocknet.png">
<meta property="og:image:width" content="1024">
<meta property="og:image:height" content="1024">
<meta property="og:url" content="https://explorer.blocknetcrypto.com">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="#000000">
<meta http-equiv="refresh" content="300">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<style>` + explorerCSS + `</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c}</style>{{end}}
</head>
<body>
<div class="header"><h1><span class="g" id="egg" style="-webkit-user-select:none;user-select:none">$</span> blocknet <span class="d">explorer</span></h1><div style="display:flex;gap:16px;font-size:13px"><a href="/stats">network stats</a></div></div>

<form class="search" action="/search" method="get">
<input type="text" name="q" placeholder="Search by block height, block hash, or transaction hash...">
<button type="submit">Search</button>
</form>

<div class="stat-grid">
<div class="stat"><div class="stat-v" id="stat-height">{{.Height}}</div><div class="stat-k">Block Height</div></div>
<div class="stat"><div class="stat-v">{{.Peers}}</div><div class="stat-k">Peers</div></div>
<div class="stat"><div class="stat-v" id="stat-difficulty">{{.Difficulty}}</div><div class="stat-k">Difficulty</div></div>
<div class="stat"><div class="stat-v">{{.Hashrate}} H/s</div><div class="stat-k">Network Hashrate</div></div>
</div>

<h2 style="margin-top:40px"><span class="g">#</span> supply</h2>
<div class="stat-grid">
<div class="stat"><div class="stat-v">{{.Emitted}}</div><div class="stat-k">Coins Emitted</div></div>
<div class="stat"><div class="stat-v">{{.Remaining}}</div><div class="stat-k">Remaining (pre-tail)</div></div>
<div class="stat"><div class="stat-v">{{.PctEmitted}}%</div><div class="stat-k">Emission Progress</div></div>
{{if .TailStarted}}<div class="stat"><div class="stat-v" style="color:var(--ac)">Active</div><div class="stat-k">Tail Emission</div></div>{{end}}
</div>

{{if .MempoolTxs}}
<div class="panels">
<div>
<h2><span class="g">#</span> mempool <span class="d">({{.MempoolTotal}})</span></h2>
<table>
<tr><th>Hash</th><th>Fee</th><th>Fee/byte</th><th>Size</th><th>Age</th></tr>
{{range .MempoolTxs}}
<tr>
<td class="hash"><a href="/tx/{{.Hash}}">{{slice .Hash 0 16}}...</a></td>
<td>{{printf "%.8f" .Fee}}</td>
<td>{{.FeeRate}}</td>
<td>{{.Size}} B</td>
<td>{{.Ago}}</td>
</tr>
{{end}}
</table>
</div>

<div>
<h2><span class="g">#</span> recent blocks</h2>
<table class="recent-blocks">
<tr><th>Height</th><th>Hash</th><th>Age</th><th>Txs</th></tr>
{{range .Blocks}}
<tr>
<td><a href="/block/{{.Height}}">{{.Height}}</a></td>
<td class="hash"><a href="/block/{{.Hash}}">{{slice .Hash 0 16}}...</a></td>
<td>{{.Ago}}</td>
<td>{{.TxCount}}</td>
</tr>
{{end}}
</table>
</div>
</div>
{{else}}
<h2><span class="g">#</span> recent blocks</h2>
<table class="recent-blocks">
<tr><th>Height</th><th>Hash</th><th>Age</th><th>Txs</th></tr>
{{range .Blocks}}
<tr>
<td><a href="/block/{{.Height}}">{{.Height}}</a></td>
<td class="hash"><a href="/block/{{.Hash}}">{{.Hash}}</a></td>
<td>{{.Ago}}</td>
<td>{{.TxCount}}</td>
</tr>
{{end}}
</table>
{{end}}

<footer><a href="https://blocknetcrypto.com">← blocknetcrypto.com</a>{{if .IsTestnet}} <span class="d">·</span> <span class="d">testnet</span>{{end}}</footer>
` + explorerEggScript + explorerLiveScript + `
</body>
</html>`

const explorerBlockTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Block {{.Height}} - blocknet explorer</title>
<meta name="description" content="Zero-knowledge money">
<meta property="og:title" content="Block {{.Height}} - blocknet">
<meta property="og:description" content="Zero-knowledge money">
<meta property="og:image" content="https://blocknetcrypto.com/blocknet.png">
<meta property="og:image:width" content="1024">
<meta property="og:image:height" content="1024">
<meta property="og:url" content="https://explorer.blocknetcrypto.com/block/{{.Height}}">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="#000000">
<meta http-equiv="refresh" content="300">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<style>` + explorerCSS + `</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c}</style>{{end}}
</head>
<body>
<div class="header"><h1><a href="/" style="text-decoration:none;color:#eee"><span class="g" id="egg" style="-webkit-user-select:none;user-select:none">$</span> blocknet <span class="d">explorer</span></a></h1></div>
<div class="topnav"><a href="/">blocks</a> <a href="/stats">stats</a></div>

<div class="nav">
{{if .HasPrev}}<a href="/block/{{.PrevHeight}}">← Block {{.PrevHeight}}</a>{{end}}
{{if .HasNext}}<a href="/block/{{.NextHeight}}">Block {{.NextHeight}} →</a>{{end}}
</div>

<div class="block-columns">
<div>
<h2><span class="g">#</span> block {{.Height}}</h2>
<div class="props">
<div class="prop"><div class="prop-k">Hash</div><div class="prop-v mono">{{if .IsGenesis}}<span title="{{.GenesisMsg}}" style="cursor:help">{{.Hash}}</span>{{else}}{{.Hash}}{{end}}</div></div>
<div class="prop"><div class="prop-k">Previous</div><div class="prop-v mono"><a href="/block/{{.PrevHash}}">{{.PrevHash}}</a></div></div>
<div class="prop"><div class="prop-k">Merkle Root</div><div class="prop-v mono">{{.MerkleRoot}}</div></div>
<div class="prop"><div class="prop-k">Time</div><div class="prop-v">{{.Time}}</div></div>
<div class="prop"><div class="prop-k">Difficulty</div><div class="prop-v">{{.Difficulty}}</div></div>
<div class="prop"><div class="prop-k">Nonce</div><div class="prop-v">{{.Nonce}}</div></div>
<div class="prop"><div class="prop-k">Block Reward</div><div class="prop-v">{{printf "%.8f" .Reward}} BNT</div></div>
<div class="prop"><div class="prop-k">Transactions</div><div class="prop-v">{{.TxCount}}</div></div>
</div>
</div>

<div>
<h2><span class="g">#</span> transactions</h2>
<table>
<tr><th>Hash</th><th>Type</th><th>Inputs</th><th>Outputs</th></tr>
{{range .Txs}}
<tr>
<td class="hash"><a href="/tx/{{.Hash}}">{{slice .Hash 0 24}}...</a></td>
<td>{{if .IsCoinbase}}<span class="g">coinbase</span>{{else}}transfer{{end}}</td>
<td>{{.Inputs}}</td>
<td>{{.Outputs}}</td>
</tr>
{{end}}
</table>
</div>
</div>

<footer><a href="/">← explorer</a>   <a href="https://blocknetcrypto.com">blocknetcrypto.com</a></footer>
` + explorerEggScript + explorerLiveScript + `
</body>
</html>`

const explorerTxTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Transaction - blocknet explorer</title>
<meta name="description" content="Zero-knowledge money">
<meta property="og:title" content="Transaction - blocknet">
<meta property="og:description" content="Zero-knowledge money">
<meta property="og:image" content="https://blocknetcrypto.com/blocknet.png">
<meta property="og:image:width" content="1024">
<meta property="og:image:height" content="1024">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="#000000">
<meta http-equiv="refresh" content="300">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<style>` + explorerCSS + `</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c}</style>{{end}}
</head>
<body>
<div class="header"><h1><a href="/" style="text-decoration:none;color:#eee"><span class="g" id="egg" style="-webkit-user-select:none;user-select:none">$</span> blocknet <span class="d">explorer</span></a></h1></div>
<div class="topnav"><a href="/">blocks</a> <a href="/stats">stats</a></div>

<div class="tx-columns">
<div class="tx-col-left">
<h2><span class="g">#</span> transaction</h2>
<div class="props">
<div class="prop"><div class="prop-k">Hash</div><div class="prop-v mono">{{.Hash}}</div></div>
<div class="prop"><div class="prop-k">Tx Public Key</div><div class="prop-v mono">{{.TxPubKey}}</div></div>
<div class="prop"><div class="prop-k">Block</div><div class="prop-v">{{if .InMempool}}<span class="d">Pending (in mempool)</span>{{else}}<a href="/block/{{.BlockHeight}}">{{.BlockHeight}}</a> (<span id="tx-confirmations" data-height="{{.BlockHeight}}">{{.Confirmations}}</span> confirmations){{end}}</div></div>
<div class="prop"><div class="prop-k">Type</div><div class="prop-v">{{if .IsCoinbase}}<span class="g">coinbase</span>{{else}}transfer{{end}}</div></div>
{{if not .IsCoinbase}}<div class="prop"><div class="prop-k">Fee</div><div class="prop-v">{{printf "%.8f" .Fee}} BNT</div></div>{{end}}
<div class="prop"><div class="prop-k">Inputs</div><div class="prop-v">{{.InputCount}}</div></div>
<div class="prop"><div class="prop-k">Outputs</div><div class="prop-v">{{.OutputCount}}</div></div>
</div>

<h2><span class="g">#</span> TX Proof</h2>
<div class="prove-tabs">
<button id="tab-recv" class="prove-tab prove-tab-active" onclick="switchProveTab('recv')">TX Received</button>
<button id="tab-send" class="prove-tab" onclick="switchProveTab('send')">TX Sent</button>
</div>
<div class="prove-box">
<div id="prove-recv">
<p class="d" style="font-size:13px;margin-bottom:12px">Check if an output belongs to you. Your view privkey is sent to this server.</p>
<input type="text" id="recv-spend" placeholder="Spend pubkey (64 hex)" maxlength="64" spellcheck="false">
<input type="text" id="recv-view" placeholder="View privkey (64 hex)" maxlength="64" spellcheck="false">
<button id="recv-btn" onclick="doRecvProve()">Check</button>
</div>
<div id="prove-send" style="display:none">
<p class="d" style="font-size:13px;margin-bottom:12px">Prove participation in a TX. Provide a view privkey and the recipient's public address. Anyone can independently verify this proof.</p>
<input type="text" id="send-txpriv" placeholder="View privkey (64 hex)" maxlength="64" spellcheck="false">
<input type="text" id="send-spend" placeholder="Spend pubkey (64 hex)" maxlength="64" spellcheck="false">
<input type="text" id="send-view" placeholder="View pubkey (64 hex)" maxlength="64" spellcheck="false">
<button id="send-btn" onclick="doSendProve()">Prove</button>
</div>
<div id="prove-result" style="margin-top:16px;display:none"></div>
</div>
</div>

<div class="tx-col-right">
{{if .Inputs}}
<h2><span class="g">#</span> inputs (ring signatures)</h2>
<p class="d" style="font-size:13px;margin-bottom:16px">Each input uses a ring of {{(index .Inputs 0).RingSize}} public keys. The real spender is hidden among decoys.</p>
{{range .Inputs}}
<div class="io-card">
<div class="prop"><div class="prop-k">Input #{{.Index}}</div><div class="prop-v"></div></div>
<div class="prop"><div class="prop-k">Key Image</div><div class="prop-v mono">{{.KeyImage}}</div></div>
<div class="prop"><div class="prop-k">Ring Size</div><div class="prop-v">{{.RingSize}}</div></div>
<div class="prop"><div class="prop-k">Pseudo Output</div><div class="prop-v mono">{{.PseudoOutput}}</div></div>
</div>
{{end}}
{{end}}

<h2 style="margin-top:24px"><span class="g">#</span> outputs (stealth addresses)</h2>
<p class="d" style="font-size:13px;margin-bottom:16px">Amounts are hidden by Pedersen commitments. Range proofs ensure validity without revealing values.</p>
{{range .Outputs}}
<div class="io-card">
<div class="prop"><div class="prop-k">Output #{{.Index}}</div><div class="prop-v"></div></div>
<div class="prop"><div class="prop-k">Stealth Address</div><div class="prop-v mono">{{.PublicKey}}</div></div>
<div class="prop"><div class="prop-k">Commitment</div><div class="prop-v mono">{{.Commitment}}</div></div>
<div class="prop"><div class="prop-k">Range Proof</div><div class="prop-v">{{.RangeProof}} bytes (Bulletproof)</div></div>
{{if .EncMemo}}<div class="prop"><div class="prop-k">Memo</div><div class="prop-v mono"><span class="g">{{.EncMemo}}</span> <span class="d">(encrypted)</span></div></div>{{end}}
</div>
{{end}}
</div>
</div>

<script>
function switchProveTab(tab){
var rt=document.getElementById('tab-recv'),st=document.getElementById('tab-send');
var rp=document.getElementById('prove-recv'),sp=document.getElementById('prove-send');
document.getElementById('prove-result').style.display='none';
if(tab==='recv'){
rt.className='prove-tab prove-tab-active';st.className='prove-tab';
rp.style.display='';sp.style.display='none';
}else{
st.className='prove-tab prove-tab-active';rt.className='prove-tab';
sp.style.display='';rp.style.display='none';
}
}
function renderProveResult(data){
var res=document.getElementById('prove-result');
res.style.display='block';
if(data.error){res.innerHTML='<span style="color:#f44">'+data.error+'</span>';return;}
var matched=data.filter(function(o){return o.match;});
if(matched.length===0){res.innerHTML='<span class="d">No outputs in this transaction match the provided keys.</span>';return;}
var html='<div style="color:var(--ac);margin-bottom:12px">'+matched.length+' output'+(matched.length>1?'s':'')+' matched</div>';
for(var i=0;i<matched.length;i++){
var o=matched[i];
html+='<div style="padding:12px 0;margin-bottom:8px">';
html+='<div class="prop"><div class="prop-k">Output #'+o.index+'</div><div class="prop-v" style="color:var(--ac);font-size:18px">'+o.amount.toFixed(8)+' BNT</div></div>';
if(o.memo){html+='<div class="prop"><div class="prop-k">Memo</div><div class="prop-v">'+o.memo.replace(/</g,'&lt;')+'</div></div>';}
html+='</div>';
}
res.innerHTML=html;
}
function proveRequest(url,body,btn){
btn.disabled=true;btn.textContent='Checking...';
fetch(url,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:body})
.then(function(r){return r.json();})
.then(function(data){btn.disabled=false;btn.textContent=btn.dataset.label;renderProveResult(data);})
.catch(function(){btn.disabled=false;btn.textContent=btn.dataset.label;var r=document.getElementById('prove-result');r.style.display='block';r.innerHTML='<span style="color:#f44">Request failed</span>';});
}
document.getElementById('recv-btn').dataset.label='Check';
document.getElementById('send-btn').dataset.label='Prove';
function doRecvProve(){
var sp=document.getElementById('recv-spend').value.trim();
var vp=document.getElementById('recv-view').value.trim();
var res=document.getElementById('prove-result');
if(sp.length!==64||vp.length!==64){res.style.display='block';res.innerHTML='<span style="color:#f44">Both keys must be 64 hex characters</span>';return;}
proveRequest('/prove','tx={{.Hash}}&spend_pub='+sp+'&view_priv='+vp,document.getElementById('recv-btn'));
}
function doSendProve(){
var tp=document.getElementById('send-txpriv').value.trim();
var sp=document.getElementById('send-spend').value.trim();
var vp=document.getElementById('send-view').value.trim();
var res=document.getElementById('prove-result');
if(tp.length!==64||sp.length!==64||vp.length!==64){res.style.display='block';res.innerHTML='<span style="color:#f44">All keys must be 64 hex characters</span>';return;}
proveRequest('/prove-send','tx={{.Hash}}&tx_priv='+tp+'&spend_pub='+sp+'&view_pub='+vp,document.getElementById('send-btn'));
}
</script>

<footer><a href="/">← explorer</a>   <a href="https://blocknetcrypto.com">blocknetcrypto.com</a></footer>
` + explorerEggScript + explorerLiveScript + `
</body>
</html>`

const explorerStatsTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Network Stats - blocknet explorer</title>
<meta name="description" content="blocknet network statistics">
<meta property="og:title" content="Network Stats - blocknet">
<meta property="og:description" content="Historical blockchain data">
<meta property="og:image" content="https://blocknetcrypto.com/blocknet.png">
<meta property="og:image:width" content="1024">
<meta property="og:image:height" content="1024">
<meta property="og:url" content="https://explorer.blocknetcrypto.com/stats">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="#000">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<style>
*{margin:0;padding:0;box-sizing:border-box}
:root{--ac:#af0;--ac-h:#cf3}
body{background:#000;color:#b0b0b0;font:15px/1.6 ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;padding:24px 32px}
a{color:var(--ac)}
a:hover{color:var(--ac-h)}
h1,h2{color:#eee;font-weight:normal}
h1{font-size:24px;margin:0}
h2{font-size:18px;margin:40px 0 16px}
.g{color:var(--ac)}
.d{color:#555}
.header{display:flex;justify-content:space-between;align-items:baseline;flex-wrap:wrap;gap:12px;margin-bottom:8px}
.topnav{font-size:13px;display:flex;gap:16px;margin-bottom:24px}
.stat-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:16px;margin-bottom:24px}
.stat{padding:20px;text-align:center}
.stat-v{font-size:24px;color:#eee}
.stat-k{font-size:12px;color:#666;text-transform:uppercase;margin-top:4px}
.chart-box{background:#000;padding:12px 12px 8px}
canvas{width:100%;height:280px;display:block}
footer{margin-top:48px;padding-top:24px;border-top:1px dashed #333;color:#444;font-size:13px}
@media(max-width:1000px){body{padding:16px}h1{font-size:20px}.stat-grid{grid-template-columns:repeat(auto-fit,minmax(140px,1fr))}}
@media(max-width:500px){.stat-grid{grid-template-columns:1fr 1fr}.stat-v{font-size:18px}}
</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c}</style>{{end}}
</head>
<body>
<div class="header"><h1><a href="/" style="text-decoration:none;color:#eee"><span class="g" id="egg" style="-webkit-user-select:none;user-select:none">$</span> blocknet <span class="d">explorer</span></a></h1></div>
<div class="topnav"><a href="/">← blocks</a></div>

<h2><span class="g">#</span> network overview</h2>
<div class="stat-grid">
<div class="stat"><div class="stat-v" id="stat-height">{{.Height}}</div><div class="stat-k">Block Height</div></div>
<div class="stat"><div class="stat-v">{{.Hashrate}} H/s</div><div class="stat-k">Hashrate</div></div>
<div class="stat"><div class="stat-v" id="stat-difficulty">{{.Difficulty}}</div><div class="stat-k">Difficulty</div></div>
<div class="stat"><div class="stat-v">{{.AvgBlockTime}}s</div><div class="stat-k">Avg Block Time</div></div>
</div>
<div class="stat-grid">
<div class="stat"><div class="stat-v">{{.TotalTx}}</div><div class="stat-k">Window Transactions</div></div>
<div class="stat"><div class="stat-v">{{.Emitted}}</div><div class="stat-k">Coins Emitted</div></div>
<div class="stat"><div class="stat-v">{{.PctEmitted}}%</div><div class="stat-k">Emission Progress</div></div>
<div class="stat"><div class="stat-v">{{.Peers}}</div><div class="stat-k">Peers</div></div>
</div>

<h2><span class="g">#</span> difficulty</h2>
<div class="chart-box"><canvas id="c-diff"></canvas></div>

<h2 style="color:#a0f"><span style="color:#a0f">#</span> difficulty delta</h2>
<div class="chart-box"><canvas id="c-ddelta"></canvas></div>

<h2 style="color:#fa0"><span style="color:#fa0">#</span> estimated hashrate</h2>
<div class="chart-box"><canvas id="c-hash"></canvas></div>

<h2 style="color:#f0a"><span style="color:#f0a">#</span> block time</h2>
<div class="chart-box"><canvas id="c-bt"></canvas></div>

<h2 style="color:#a0f"><span style="color:#a0f">#</span> nonce distribution</h2>
<div class="chart-box"><canvas id="c-nonce"></canvas></div>

<h2 style="color:#0fa"><span style="color:#0fa">#</span> transactions per block</h2>
<div class="chart-box"><canvas id="c-tx"></canvas></div>

<h2 style="color:#0af"><span style="color:#0af">#</span> block size</h2>
<div class="chart-box"><canvas id="c-size"></canvas></div>

<h2><span class="g">#</span> emission schedule</h2>
<div class="chart-box"><canvas id="c-emission"></canvas></div>

<footer><a href="/">← explorer</a>   <a href="https://blocknetcrypto.com">blocknetcrypto.com</a></footer>

<script>
var D={{.DataJSON}};
var AC=getComputedStyle(document.documentElement).getPropertyValue('--ac').trim()||'#af0';
(function(){
if(!D||D.length<2)return;

var tip=document.createElement('div');
tip.style.cssText='position:fixed;background:#111;border:1px solid #333;color:#eee;padding:8px 12px;font:12px/1.5 monospace;pointer-events:none;display:none;z-index:10;white-space:nowrap';
document.body.appendChild(tip);

function showTip(e,html){
tip.innerHTML=html;tip.style.display='block';
var tx=e.clientX+14,ty=e.clientY-44;
if(tx+tip.offsetWidth>window.innerWidth-8)tx=e.clientX-tip.offsetWidth-14;
if(ty<8)ty=e.clientY+20;
tip.style.left=tx+'px';tip.style.top=ty+'px';
}
function hideTip(){tip.style.display='none';}

function fmt(n){
if(Math.abs(n)>=1e12)return(n/1e12).toFixed(1)+'T';
if(Math.abs(n)>=1e9)return(n/1e9).toFixed(1)+'G';
if(Math.abs(n)>=1e6)return(n/1e6).toFixed(1)+'M';
if(Math.abs(n)>=1e3)return(n/1e3).toFixed(1)+'K';
return n%1?n.toFixed(2):n.toFixed(0);
}

function xhex(c){if(c.length===4)return'#'+c[1]+c[1]+c[2]+c[2]+c[3]+c[3];return c;}

function draw(id,getY,color,opts){
opts=opts||{};
color=xhex(color);
var c=document.getElementById(id);
if(!c)return;
var dpr=window.devicePixelRatio||1;
var rect=c.getBoundingClientRect();
c.width=rect.width*dpr;
c.height=rect.height*dpr;
var ctx=c.getContext('2d');
ctx.scale(dpr,dpr);
var W=rect.width,H=rect.height;
var pad={t:24,r:16,b:32,l:60};
var pw=W-pad.l-pad.r,ph=H-pad.t-pad.b;

var src=opts.data||D;
var pts=[];
for(var i=0;i<src.length;i++){
var y=getY(src[i],i);
if(y!==null&&y!==undefined&&isFinite(y))pts.push({x:src[i].h,y:y});
}
if(pts.length<2){
ctx.fillStyle='#333';ctx.font='13px monospace';ctx.textAlign='center';
ctx.fillText('Not enough data',W/2,H/2);return;
}

var xMin=pts[0].x,xMax=pts[pts.length-1].x;
var yMin=Infinity,yMax=-Infinity;
for(var i=0;i<pts.length;i++){
if(pts[i].y<yMin)yMin=pts[i].y;
if(pts[i].y>yMax)yMax=pts[i].y;
}
if(opts.refLine!==undefined){yMin=Math.min(yMin,opts.refLine);yMax=Math.max(yMax,opts.refLine);}
if(opts.refLines&&opts.refLines.length){
for(var i=0;i<opts.refLines.length;i++){
var rl=opts.refLines[i];
if(rl&&rl.value!==undefined){yMin=Math.min(yMin,rl.value);yMax=Math.max(yMax,rl.value);}
}
}
if(opts.yMin!==undefined)yMin=Math.min(yMin,opts.yMin);
var yP=(yMax-yMin)*0.08||1;
yMin-=yP;yMax+=yP;
if(opts.yMin!==undefined)yMin=Math.max(yMin,opts.yMin);
if(xMin===xMax)xMax=xMin+1;

// measure widest y-axis label and adjust left padding
ctx.font='11px monospace';
var maxLW=0;
for(var i=0;i<=4;i++){
var val=yMax-(yMax-yMin)*i/4;
var tw=ctx.measureText(opts.fmtY?opts.fmtY(val):fmt(val)).width;
if(tw>maxLW)maxLW=tw;
}
pad.l=Math.max(60,Math.ceil(maxLW+16));
pw=W-pad.l-pad.r;

function sx(x){return pad.l+(x-xMin)/(xMax-xMin)*pw;}
function sy(y){return pad.t+ph-(y-yMin)/(yMax-yMin)*ph;}

// grid
ctx.strokeStyle='#1a1a1a';ctx.lineWidth=1;
for(var i=0;i<=4;i++){
var y=pad.t+ph*i/4;
ctx.beginPath();ctx.moveTo(pad.l,y);ctx.lineTo(W-pad.r,y);ctx.stroke();
}

// y-axis legend
if(opts.yLabel){
ctx.fillStyle=color;ctx.font='11px monospace';ctx.textAlign='left';
ctx.fillText(opts.yLabel,pad.l,pad.t-10);
}

// y tick labels
ctx.fillStyle='#555';ctx.font='11px monospace';ctx.textAlign='right';
for(var i=0;i<=4;i++){
var val=yMax-(yMax-yMin)*i/4;
ctx.fillText(opts.fmtY?opts.fmtY(val):fmt(val),pad.l-8,pad.t+ph*i/4+4);
}

// x tick labels
ctx.textAlign='center';ctx.fillStyle='#444';
for(var i=0;i<=4;i++){
var val=xMin+(xMax-xMin)*i/4;
ctx.fillText(Math.round(val).toString(),pad.l+pw*i/4,H-6);
}

// reference line
if(opts.refLine!==undefined){
ctx.save();ctx.strokeStyle=opts.refColor||'#444';ctx.setLineDash([4,4]);ctx.lineWidth=1;
var ry=sy(opts.refLine);
ctx.beginPath();ctx.moveTo(pad.l,ry);ctx.lineTo(W-pad.r,ry);ctx.stroke();
ctx.restore();
if(opts.refLabel){ctx.fillStyle=opts.refLabelColor||'#666';ctx.textAlign='left';ctx.fillText(opts.refLabel,pad.l+4,ry-6);}
}
if(opts.refLines&&opts.refLines.length){
for(var i=0;i<opts.refLines.length;i++){
var rl=opts.refLines[i];
if(!rl||rl.value===undefined)continue;
ctx.save();
ctx.strokeStyle=rl.color||'#444';
ctx.setLineDash(rl.dash||[4,4]);
ctx.lineWidth=1;
var y=sy(rl.value);
ctx.beginPath();ctx.moveTo(pad.l,y);ctx.lineTo(W-pad.r,y);ctx.stroke();
ctx.restore();
if(rl.label){ctx.fillStyle=rl.labelColor||'#666';ctx.textAlign='left';ctx.fillText(rl.label,pad.l+4,y-6);}
}
}

// line + fill (supports splitAt to change color mid-line)
var spIdx=-1,sc=color;
if(opts.splitAt!==undefined){
sc=opts.splitColor?xhex(opts.splitColor):'#555';
for(var i=pts.length-1;i>=0;i--){if(pts[i].x<=opts.splitAt){spIdx=i;break;}}
}
if(spIdx>=0&&spIdx<pts.length-1){
ctx.strokeStyle=color;ctx.lineWidth=1.5;ctx.beginPath();
for(var i=0;i<=spIdx;i++){var x=sx(pts[i].x),y=sy(pts[i].y);i===0?ctx.moveTo(x,y):ctx.lineTo(x,y);}
ctx.stroke();
ctx.beginPath();ctx.moveTo(sx(pts[0].x),sy(pts[0].y));
for(var i=1;i<=spIdx;i++)ctx.lineTo(sx(pts[i].x),sy(pts[i].y));
ctx.lineTo(sx(pts[spIdx].x),pad.t+ph);ctx.lineTo(sx(pts[0].x),pad.t+ph);ctx.closePath();
var g1=ctx.createLinearGradient(0,pad.t,0,pad.t+ph);
g1.addColorStop(0,color+'18');g1.addColorStop(1,color+'03');ctx.fillStyle=g1;ctx.fill();
ctx.strokeStyle=sc;ctx.lineWidth=1.5;ctx.beginPath();
ctx.moveTo(sx(pts[spIdx].x),sy(pts[spIdx].y));
for(var i=spIdx+1;i<pts.length;i++)ctx.lineTo(sx(pts[i].x),sy(pts[i].y));
ctx.stroke();
ctx.beginPath();ctx.moveTo(sx(pts[spIdx].x),sy(pts[spIdx].y));
for(var i=spIdx+1;i<pts.length;i++)ctx.lineTo(sx(pts[i].x),sy(pts[i].y));
ctx.lineTo(sx(pts[pts.length-1].x),pad.t+ph);ctx.lineTo(sx(pts[spIdx].x),pad.t+ph);ctx.closePath();
var g2=ctx.createLinearGradient(0,pad.t,0,pad.t+ph);
g2.addColorStop(0,sc+'10');g2.addColorStop(1,sc+'03');ctx.fillStyle=g2;ctx.fill();
ctx.save();ctx.strokeStyle=color;ctx.setLineDash([4,4]);ctx.lineWidth=1;
ctx.beginPath();ctx.moveTo(sx(pts[spIdx].x),pad.t);ctx.lineTo(sx(pts[spIdx].x),pad.t+ph);ctx.stroke();ctx.restore();
}else{
ctx.strokeStyle=color;ctx.lineWidth=1.5;ctx.beginPath();
for(var i=0;i<pts.length;i++){var x=sx(pts[i].x),y=sy(pts[i].y);i===0?ctx.moveTo(x,y):ctx.lineTo(x,y);}
ctx.stroke();
var grad=ctx.createLinearGradient(0,pad.t,0,pad.t+ph);
grad.addColorStop(0,color+'18');grad.addColorStop(1,color+'03');
ctx.fillStyle=grad;
ctx.lineTo(sx(pts[pts.length-1].x),pad.t+ph);ctx.lineTo(sx(pts[0].x),pad.t+ph);ctx.closePath();ctx.fill();
}

// trend line
if(opts.trendLine&&pts.length>=2){
var tn=pts.length,tsX=0,tsY=0,tsXY=0,tsX2=0;
for(var i=0;i<tn;i++){tsX+=pts[i].x;tsY+=pts[i].y;tsXY+=pts[i].x*pts[i].y;tsX2+=pts[i].x*pts[i].x;}
var tDen=tn*tsX2-tsX*tsX;
if(tDen!==0){
var tM=(tn*tsXY-tsX*tsY)/tDen,tB=(tsY-tM*tsX)/tn;
ctx.save();
ctx.beginPath();
ctx.moveTo(sx(xMin),sy(tB+tM*xMin));
ctx.lineTo(sx(xMax),sy(tB+tM*xMax));
ctx.strokeStyle='#0AF';ctx.globalAlpha=0.25;ctx.lineWidth=2;ctx.setLineDash([8,6]);
ctx.stroke();
ctx.restore();
}}

// snapshot for hover redraw
var base=ctx.getImageData(0,0,c.width,c.height);

c.addEventListener('mousemove',function(e){
var r=c.getBoundingClientRect();
var mx=e.clientX-r.left;
var best=null,bd=Infinity;
for(var i=0;i<pts.length;i++){
var dx=Math.abs(sx(pts[i].x)-mx);
if(dx<bd){bd=dx;best=pts[i];}
}
if(!best||bd>Math.max(20,pw/pts.length*2)){
ctx.putImageData(base,0,0);hideTip();return;
}
ctx.putImageData(base,0,0);
ctx.save();ctx.setTransform(dpr,0,0,dpr,0,0);
var px=sx(best.x),py=sy(best.y);
// crosshair
ctx.strokeStyle='#333';ctx.setLineDash([2,2]);ctx.lineWidth=1;
ctx.beginPath();ctx.moveTo(px,pad.t);ctx.lineTo(px,pad.t+ph);ctx.stroke();
ctx.beginPath();ctx.moveTo(pad.l,py);ctx.lineTo(pad.l+pw,py);ctx.stroke();
ctx.setLineDash([]);
// dot
ctx.beginPath();ctx.arc(px,py,5,0,Math.PI*2);
ctx.fillStyle='#000';ctx.fill();
ctx.beginPath();ctx.arc(px,py,4,0,Math.PI*2);
var hc=(spIdx>=0&&spIdx<pts.length-1&&best.x>pts[spIdx].x)?sc:color;
ctx.fillStyle=hc;ctx.fill();
ctx.restore();
var fv=opts.fmtY?opts.fmtY(best.y):fmt(best.y);
var extra=opts.tipExtra?opts.tipExtra(best):'';
showTip(e,'<span style="color:'+hc+'">\u25CF '+(opts.yLabel||'value')+'</span> <b style="color:#eee">'+fv+'</b><br><span style="color:#555">block '+Math.round(best.x)+'</span>'+extra);
});
c.addEventListener('mouseleave',function(){ctx.putImageData(base,0,0);hideTip();});
}

// line charts
draw('c-diff',function(d){return d.d;},AC,{yLabel:'difficulty',trendLine:true});

// difficulty delta bar chart
(function(){
var c=document.getElementById('c-ddelta');
if(!c||D.length<2)return;
var dpr=window.devicePixelRatio||1;
var rect=c.getBoundingClientRect();
c.width=rect.width*dpr;c.height=rect.height*dpr;
var ctx=c.getContext('2d');ctx.scale(dpr,dpr);
var W=rect.width,H=rect.height;
var pad={t:24,r:16,b:32,l:60};
var dd=[];
for(var i=1;i<D.length;i++){
if(D[i-1].d>0&&D[i].h>100)dd.push({h:D[i].h,v:(D[i].d-D[i-1].d)/D[i-1].d*100});
}
if(dd.length<2){ctx.fillStyle='#333';ctx.font='13px monospace';ctx.textAlign='center';ctx.fillText('Not enough data',W/2,H/2);return;}
var yMin=0,yMax=0;
for(var i=0;i<dd.length;i++){if(dd[i].v<yMin)yMin=dd[i].v;if(dd[i].v>yMax)yMax=dd[i].v;}
var yP=(yMax-yMin)*0.08||1;yMin-=yP;yMax+=yP;
ctx.font='11px monospace';
var maxLW=0;
for(var i=0;i<=4;i++){var val=yMax-(yMax-yMin)*i/4;var tw=ctx.measureText(val.toFixed(1)+'%').width;if(tw>maxLW)maxLW=tw;}
pad.l=Math.max(60,Math.ceil(maxLW+16));
var pw=W-pad.l-pad.r,ph=H-pad.t-pad.b;
var xMin=dd[0].h,xMax=dd[dd.length-1].h;
if(xMin===xMax)xMax=xMin+1;
function dsx(x){return pad.l+(x-xMin)/(xMax-xMin)*pw;}
function dsy(y){return pad.t+ph-(y-yMin)/(yMax-yMin)*ph;}
ctx.strokeStyle='#1a1a1a';ctx.lineWidth=1;
for(var i=0;i<=4;i++){var y=pad.t+ph*i/4;ctx.beginPath();ctx.moveTo(pad.l,y);ctx.lineTo(W-pad.r,y);ctx.stroke();}
var zy=dsy(0);
ctx.strokeStyle='#333';ctx.lineWidth=1;ctx.beginPath();ctx.moveTo(pad.l,zy);ctx.lineTo(W-pad.r,zy);ctx.stroke();
ctx.fillStyle='#a0f';ctx.font='11px monospace';ctx.textAlign='left';
ctx.fillText('% change',pad.l,pad.t-10);
ctx.fillStyle='#555';ctx.font='11px monospace';ctx.textAlign='right';
for(var i=0;i<=4;i++){var val=yMax-(yMax-yMin)*i/4;ctx.fillText(val.toFixed(1)+'%',pad.l-8,pad.t+ph*i/4+4);}
ctx.textAlign='center';ctx.fillStyle='#444';
for(var i=0;i<=4;i++){var val=xMin+(xMax-xMin)*i/4;ctx.fillText(Math.round(val).toString(),pad.l+pw*i/4,H-6);}
var bw=Math.max(1,pw/dd.length);
for(var i=0;i<dd.length;i++){
var x=dsx(dd[i].h)-bw/2,pos=dd[i].v>=0;
var top,bh;
if(pos){top=dsy(dd[i].v);bh=zy-top;}else{top=zy;bh=dsy(dd[i].v)-zy;}
ctx.fillStyle=pos?'#aa00ff50':'#ff00aa50';
ctx.fillRect(x,top,Math.max(1,bw-1),Math.max(1,bh));
ctx.fillStyle=pos?'#a0f':'#f0a';
ctx.fillRect(x,pos?top:top+bh-Math.min(2,bh),Math.max(1,bw-1),Math.min(2,bh));
}
var base=ctx.getImageData(0,0,c.width,c.height);
c.addEventListener('mousemove',function(e){
var r=c.getBoundingClientRect();var mx=e.clientX-r.left;
var best=null,bd=Infinity;
for(var i=0;i<dd.length;i++){var dx=Math.abs(dsx(dd[i].h)-mx);if(dx<bd){bd=dx;best=dd[i];}}
if(!best||bd>Math.max(20,pw/dd.length*2)){ctx.putImageData(base,0,0);hideTip();return;}
ctx.putImageData(base,0,0);
ctx.save();ctx.setTransform(dpr,0,0,dpr,0,0);
var px=dsx(best.h),py=dsy(best.v);
ctx.strokeStyle='#333';ctx.setLineDash([2,2]);ctx.lineWidth=1;
ctx.beginPath();ctx.moveTo(px,pad.t);ctx.lineTo(px,pad.t+ph);ctx.stroke();
ctx.beginPath();ctx.moveTo(pad.l,py);ctx.lineTo(pad.l+pw,py);ctx.stroke();
ctx.setLineDash([]);
ctx.beginPath();ctx.arc(px,py,5,0,Math.PI*2);ctx.fillStyle='#000';ctx.fill();
ctx.beginPath();ctx.arc(px,py,4,0,Math.PI*2);
var hc=best.v>=0?'#a0f':'#f0a';
ctx.fillStyle=hc;ctx.fill();ctx.restore();
var sign=best.v>=0?'+':'';
showTip(e,'<span style="color:'+hc+'">\u25CF difficulty delta</span> <b style="color:#eee">'+sign+best.v.toFixed(2)+'%</b><br><span style="color:#555">block '+Math.round(best.h)+'</span>');
});
c.addEventListener('mouseleave',function(){ctx.putImageData(base,0,0);hideTip();});
})();

var hrD=[],hrW=20;
for(var i=1;i<D.length;i++){
if(D[i].bt<=0)continue;
var sd=0,st=0;
for(var j=Math.max(1,i-hrW+1);j<=i;j++){if(D[j].bt>0){sd+=D[j].d;st+=D[j].bt;}}
if(st>0)hrD.push({h:D[i].h,hr:sd/st});
}
draw('c-hash',function(d){return d.hr;},'#fa0',{data:hrD,yLabel:'H/s ('+hrW+'-block avg)',yMin:0,trendLine:true});
var btSum=0,btCnt=0;
for(var i=0;i<D.length;i++){if(D[i].h>100&&D[i].bt>0){btSum+=D[i].bt;btCnt++;}}
var btAvg=btCnt>0?btSum/btCnt:0;
draw('c-bt',function(d,i){return d.h>1&&d.bt>0?d.bt:null;},'#f0a',{yLabel:'seconds',refLine:300,refLabel:'5m target',refColor:'#fa0',refLabelColor:'#fa0',refLines:[{value:btAvg,label:'avg '+Math.round(btAvg)+'s',color:'#0af',labelColor:'#0af',dash:[2,3]}],yMin:0});

// nonce histogram
(function(){
var c=document.getElementById('c-nonce');
if(!c||D.length<2)return;
var dpr=window.devicePixelRatio||1;
var rect=c.getBoundingClientRect();
c.width=rect.width*dpr;c.height=rect.height*dpr;
var ctx=c.getContext('2d');ctx.scale(dpr,dpr);
var W=rect.width,H=rect.height;
var pad={t:24,r:16,b:32,l:60};
var pw=W-pad.l-pad.r,ph=H-pad.t-pad.b;
var lnonces=[];for(var i=0;i<D.length;i++)lnonces.push(Math.log10(D[i].n+1));
var nMin=Infinity,nMax=-Infinity;
for(var i=0;i<lnonces.length;i++){if(lnonces[i]<nMin)nMin=lnonces[i];if(lnonces[i]>nMax)nMax=lnonces[i];}
if(nMin===nMax)nMax=nMin+1;
var bins=Math.min(64,lnonces.length);
var counts=[];for(var i=0;i<bins;i++)counts[i]=0;
var range=nMax-nMin;
for(var i=0;i<lnonces.length;i++){var b=Math.floor((lnonces[i]-nMin)/range*bins);if(b>=bins)b=bins-1;counts[b]++;}
var maxC=0;for(var i=0;i<bins;i++){if(counts[i]>maxC)maxC=counts[i];}
if(maxC===0)return;

// grid
ctx.strokeStyle='#1a1a1a';ctx.lineWidth=1;
for(var i=0;i<=4;i++){var y=pad.t+ph*i/4;ctx.beginPath();ctx.moveTo(pad.l,y);ctx.lineTo(W-pad.r,y);ctx.stroke();}

// y legend
ctx.fillStyle='#aa00ff';ctx.font='11px monospace';ctx.textAlign='left';
ctx.fillText('block count',pad.l,pad.t-10);

// y ticks
ctx.fillStyle='#555';ctx.font='11px monospace';ctx.textAlign='right';
for(var i=0;i<=4;i++){var val=maxC-maxC*i/4;ctx.fillText(Math.round(val).toString(),pad.l-8,pad.t+ph*i/4+4);}

// x ticks
ctx.textAlign='center';ctx.fillStyle='#444';
for(var i=0;i<=4;i++){var lv=nMin+(nMax-nMin)*i/4;var rv=Math.pow(10,lv);ctx.fillText(fmt(rv),pad.l+pw*i/4,H-6);}

// x legend
ctx.fillStyle='#555';ctx.textAlign='right';ctx.fillText('nonce (log)',W-pad.r,pad.t-10);

// bars
var bw=pw/bins;
for(var i=0;i<bins;i++){
var bh=counts[i]/maxC*ph;var x=pad.l+i*bw;var y=pad.t+ph-bh;
ctx.fillStyle='#aa00ff50';ctx.fillRect(x+1,y,bw-2,bh);
ctx.fillStyle='#aa00ff';ctx.fillRect(x+1,y,bw-2,Math.min(2,bh));
}

// snapshot for hover
var base=ctx.getImageData(0,0,c.width,c.height);

c.addEventListener('mousemove',function(e){
var r=c.getBoundingClientRect();
var mx=e.clientX-r.left;
var bi=Math.floor((mx-pad.l)/bw);
if(bi<0||bi>=bins){ctx.putImageData(base,0,0);hideTip();return;}
ctx.putImageData(base,0,0);
ctx.save();ctx.setTransform(dpr,0,0,dpr,0,0);
var bh=counts[bi]/maxC*ph;var bx=pad.l+bi*bw;var by=pad.t+ph-bh;
ctx.fillStyle='#aa00ff90';ctx.fillRect(bx+1,by,bw-2,bh);
ctx.restore();
var loN=Math.pow(10,nMin+range*bi/bins);
var hiN=Math.pow(10,nMin+range*(bi+1)/bins);
showTip(e,'<span style="color:#aa00ff">\u25CF block count</span> <b style="color:#eee">'+counts[bi]+'</b><br><span style="color:#555">nonce '+fmt(loN)+' \u2013 '+fmt(hiN)+'</span>');
});
c.addEventListener('mouseleave',function(){ctx.putImageData(base,0,0);hideTip();});
})();

// remaining line charts
draw('c-tx',function(d){return d.tx;},'#0fa',{yLabel:'transactions',yMin:0,fmtY:function(v){return Math.round(v).toString();}});
draw('c-size',function(d){return d.s;},'#0af',{yLabel:'bytes',yMin:0,fmtY:function(v){return v>=1024?(v/1024).toFixed(1)+'KB':v.toFixed(0)+'B';}});

// emission
var IR=72325093035,TE=200000000,BPM=8640,MTT=48,DR=0.75;
var emD=[];
function emR(mo){if(mo>=MTT)return TE;var yr=mo/12;return(IR-TE)*Math.exp(-DR*yr)+TE;}
for(var mo=0;mo<MTT;mo++){
var r=emR(mo)/100000000;
emD.push({h:mo*BPM,r:r});
emD.push({h:(mo+1)*BPM,r:r});
}
emD.push({h:MTT*BPM,r:TE/100000000});
emD.push({h:(MTT+1)*BPM,r:TE/100000000});
var genTs={{.GenesisTs}};
draw('c-emission',function(d){return d.r;},'#f0a',{data:emD,yLabel:'BNT/block',splitAt:{{.Height}},splitColor:AC,fmtY:function(v){return v.toFixed(2)+' BNT';},tipExtra:function(pt){var d=new Date((genTs+pt.x*300)*1000);return '<br><span style="color:#555">~'+d.toISOString().slice(0,10)+'</span>';}});

})();
</script>
` + explorerEggScript + explorerLiveScript + `
</body>
</html>`
