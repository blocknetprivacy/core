package main

import "html/template"

// Parsed once at init: template.Must fails the process at startup rather than
// letting a malformed page surface as a 500 on the first request. Every page
// shares the head, top bar and footer defined in explorerCommonTmpl.
var (
	explorerIndexPage = template.Must(template.New("index").Parse(explorerCommonTmpl + explorerIndexTmpl))
	explorerBlockPage = template.Must(template.New("block").Parse(explorerCommonTmpl + explorerBlockTmpl))
	explorerTxPage    = template.Must(template.New("tx").Parse(explorerCommonTmpl + explorerTxTmpl))
	explorerStatsPage = template.Must(template.New("stats").Parse(explorerCommonTmpl + explorerStatsTmpl))
)

const explorerCSS = `*{margin:0;padding:0;box-sizing:border-box}
:root{--ac:#af0;--ac-h:#cf3;--ac-a:rgba(170,255,0,.12);--ln:#161616;--ln2:#262626;--tx:#b0b0b0;--hi:#eee;--mu:#666;--dim:#444;--warn:#fa0;--pn:#060606}
html{background:#000}
body{background:#000;color:var(--tx);font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;-webkit-text-size-adjust:100%}
a{color:var(--ac);text-decoration:none}
a:hover{color:var(--ac-h)}
h1,h2,h3{color:var(--hi);font-weight:normal}
h2{font-size:15px;margin:0 0 14px;display:flex;align-items:baseline;gap:10px}
h2 .n{color:var(--dim);font-size:12px}
.g{color:var(--ac)}
.d{color:var(--dim)}
.wrap{max-width:1180px;margin:0 auto;padding:0 24px}
.top{position:sticky;top:0;z-index:20;background:rgba(0,0,0,.82);-webkit-backdrop-filter:blur(10px);backdrop-filter:blur(10px);border-bottom:1px solid var(--ln)}
.top .wrap{display:flex;align-items:center;gap:22px;height:56px}
.brand{color:var(--hi);font-size:17px;white-space:nowrap;letter-spacing:-.2px}
.brand:hover{color:var(--hi)}
.brand .cur{color:var(--ac);animation:blink 1.1s steps(1) infinite;-webkit-user-select:none;user-select:none;cursor:pointer}
.brand .sub{color:var(--dim);margin-left:6px}
@keyframes blink{50%{opacity:0}}
.nav{display:flex;gap:18px;font-size:13px}
.nav a{color:var(--mu)}
.nav a:hover,.nav a.on{color:var(--ac)}
.qs{margin-left:auto;display:flex;flex:0 1 340px;min-width:0}
.qs input{width:100%;background:#050505;border:1px solid var(--ln2);color:var(--hi);padding:7px 12px;font-size:12px;border-radius:6px}
.qs input:focus{outline:none;border-color:var(--ac)}
.notice{border-bottom:1px solid #2a1d00;background:#0d0800;color:#c80;font-size:12px;line-height:1.5}
.notice .wrap{padding-top:8px;padding-bottom:8px;display:flex;gap:10px;align-items:baseline}
.notice b{color:var(--warn);font-weight:normal;white-space:nowrap}
.notice a{color:var(--warn);white-space:nowrap}
.notice .ns{display:none}
main.wrap{padding-top:28px}
.card{background:var(--pn);border:1px solid var(--ln);border-radius:10px;padding:18px 20px}
.k{font-size:11px;color:var(--mu);text-transform:uppercase;letter-spacing:.6px}
.v{font-size:22px;color:var(--hi);margin-top:4px;font-variant-numeric:tabular-nums;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.v small{font-size:13px;color:var(--mu)}
.s{font-size:12px;color:var(--dim);margin-top:2px}
.grid{display:grid;gap:14px}
.g4{grid-template-columns:repeat(4,minmax(0,1fr))}
.g2{grid-template-columns:repeat(2,minmax(0,1fr))}
.g21{grid-template-columns:minmax(0,2fr) minmax(0,1fr)}
.sec{margin-top:36px}
.pbar{height:4px;background:#111;border-radius:2px;overflow:hidden;margin-top:10px}
.pbar i{display:block;height:100%;background:var(--ac);border-radius:2px;box-shadow:0 0 10px var(--ac)}
.dot{display:inline-block;width:7px;height:7px;border-radius:50%;background:var(--ac);box-shadow:0 0 0 0 var(--ac);animation:pulse 2s infinite;vertical-align:1px}
@keyframes pulse{0%{box-shadow:0 0 0 0 rgba(170,255,0,.6)}70%{box-shadow:0 0 0 8px rgba(170,255,0,0)}100%{box-shadow:0 0 0 0 rgba(170,255,0,0)}}
.badge{display:inline-flex;align-items:center;gap:6px;font-size:12px;border:1px solid var(--ln2);color:var(--tx);padding:2px 10px;border-radius:999px;white-space:nowrap}
.badge.ok{border-color:rgba(170,255,0,.35);color:var(--ac);background:var(--ac-a)}
.badge.wait{border-color:#3a2a00;color:var(--warn);background:#0d0800}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:9px 12px;white-space:nowrap}
th{color:var(--mu);font-weight:normal;font-size:11px;text-transform:uppercase;letter-spacing:.6px;border-bottom:1px solid var(--ln)}
td{border-bottom:1px solid #0d0d0d;font-variant-numeric:tabular-nums}
tr:last-child td{border-bottom:0}
tbody tr:hover{background:#0a0a0a}
td.r,th.r{text-align:right}
.hash{color:var(--mu)}
.hash a{color:var(--tx)}
.hash a:hover{color:var(--ac)}
.tbl{padding:6px 8px;overflow-x:auto}
tr.new td{animation:rowin 2.4s ease-out}
@keyframes rowin{0%{background:rgba(170,255,0,.16)}100%{background:transparent}}
.kv{display:grid;grid-template-columns:150px minmax(0,1fr);gap:0}
.kv>div{padding:11px 0;border-bottom:1px solid #0e0e0e}
.kv>div:nth-last-child(-n+2){border-bottom:0}
.kv .kk{color:var(--mu);font-size:12px;text-transform:uppercase;letter-spacing:.5px;padding-top:13px}
.kv .vv{color:var(--hi);word-break:break-all;display:flex;gap:10px;align-items:flex-start}
.kv .vv.m{font-size:13px;color:#999}
.kv .vv>span{min-width:0}
.cp{flex-shrink:0;background:transparent;border:1px solid var(--ln2);color:var(--mu);font-size:11px;line-height:1;padding:4px 7px;border-radius:5px;cursor:pointer}
.cp:hover{color:var(--ac);border-color:var(--ac)}
.cp.done{color:#000;background:var(--ac);border-color:var(--ac)}
.btn{display:inline-flex;align-items:center;gap:8px;background:#0a0a0a;border:1px solid var(--ln2);color:var(--tx);padding:7px 14px;border-radius:7px;font-size:13px}
.btn:hover{border-color:var(--ac);color:var(--ac)}
.btn.off{opacity:.3;pointer-events:none}
.pager{display:flex;gap:10px;align-items:center}
.pager .hint{color:var(--dim);font-size:11px;margin-left:6px}
.ph{display:flex;align-items:center;justify-content:space-between;gap:16px;flex-wrap:wrap;margin-bottom:22px}
.ph h1{font-size:26px;letter-spacing:-.4px}
.ph h1 .d{font-size:14px;letter-spacing:0}
.crumb{font-size:12px;color:var(--dim);margin-bottom:6px}
.crumb a{color:var(--mu)}
.chip{display:inline-block;font-size:11px;padding:1px 8px;border-radius:4px;border:1px solid var(--ln2);color:var(--mu)}
.chip.cb{color:var(--ac);border-color:rgba(170,255,0,.3)}
.note{font-size:12px;color:var(--mu);margin:-6px 0 14px}
footer{margin-top:64px;border-top:1px solid var(--ln);color:var(--dim);font-size:12px}
footer .wrap{display:flex;gap:18px;flex-wrap:wrap;padding-top:22px;padding-bottom:32px}
footer a{color:var(--mu)}
footer a:hover{color:var(--ac)}
input,button{font-family:inherit}
@media(max-width:980px){.g4{grid-template-columns:repeat(2,minmax(0,1fr))}.g2,.g21{grid-template-columns:1fr}}
@media(max-width:700px){.wrap{padding:0 16px}.qs{display:none}.nav{gap:14px}.brand .sub{display:none}.kv{grid-template-columns:1fr}.kv .kk{padding:10px 0 0;border-bottom:0!important}.kv>div.vv{padding-top:2px}.ph h1{font-size:22px}.v{font-size:18px}.hide-s{display:none}.pager .hint{display:none}.notice .nl{display:none}.notice .ns{display:inline}}
@media(prefers-reduced-motion:reduce){*{animation:none!important;transition:none!important}}`

// explorerCommonTmpl holds the parts every page shares. Pages call
// {{template "head" .}} inside <head>, then "top" and "foot" in <body>.
const explorerCommonTmpl = `{{define "head"}}<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="theme-color" content="#000000">
<meta property="og:image" content="https://blocknetcrypto.com/og.png">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:image" content="https://blocknetcrypto.com/og.png">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<link rel="apple-touch-icon" href="https://blocknetcrypto.com/apple-touch-icon.png">
<style>` + explorerCSS + `</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c;--ac-a:rgba(255,0,170,.12)}</style>{{end}}{{end}}
{{define "top"}}<header class="top"><div class="wrap">
<a class="brand" href="/">blocknet<span class="cur" id="egg">_</span><span class="sub">explorer</span></a>
<nav class="nav"><a href="/" data-nav="/">blocks</a><a href="/stats" data-nav="/stats">stats</a><a href="https://blocknetcrypto.com/#faraday" class="hide-s">faraday</a></nav>
<form class="qs" action="/search" method="get"><input type="text" name="q" placeholder="search height, block or tx hash   /" aria-label="Search" spellcheck="false" autocomplete="off"></form>
</div></header>
` + explorerUpgradeBanner + `
{{end}}
{{define "foot"}}<footer><div class="wrap">
<a href="https://blocknetcrypto.com">blocknetcrypto.com</a>
<a href="https://blocknetcrypto.com/#faraday">faraday upgrade</a>
<a href="https://blocknetcrypto.com/api.html">api</a>
<a href="https://github.com/blocknetprivacy/blocknet">github</a>
{{if .IsTestnet}}<span class="g">testnet</span>{{end}}
</div></footer>
` + explorerCommonScript + explorerEggScript + explorerLiveScript + `{{end}}`

// explorerCommonScript runs on every page: live "ago" labels, copy buttons,
// the "/" search shortcut and the active nav link.
const explorerCommonScript = `<script>
(function(){
function ago(s){if(s<0)s=0;if(s<60)return s+'s ago';if(s<3600)return Math.floor(s/60)+'m '+(s%60)+'s ago';if(s<86400)return Math.floor(s/3600)+'h '+Math.floor(s%3600/60)+'m ago';return Math.floor(s/86400)+'d ago';}
window.bnAgo=ago;
function tick(){var now=Date.now()/1000,els=document.querySelectorAll('[data-ts]');for(var i=0;i<els.length;i++){var t=+els[i].getAttribute('data-ts');if(t)els[i].textContent=ago(Math.floor(now-t));}}
window.bnTick=tick;tick();setInterval(tick,1000);
document.addEventListener('click',function(e){var b=e.target.closest&&e.target.closest('.cp');if(!b)return;
var v=b.getAttribute('data-cp');function ok(){b.textContent='copied';b.classList.add('done');setTimeout(function(){b.textContent='copy';b.classList.remove('done');},1200);}
if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(v).then(ok,function(){});}else{var t=document.createElement('textarea');t.value=v;document.body.appendChild(t);t.select();try{document.execCommand('copy');ok();}catch(x){}t.remove();}});
document.addEventListener('keydown',function(e){if(e.key!=='/'||e.metaKey||e.ctrlKey)return;var a=document.activeElement;if(a&&(a.tagName==='INPUT'||a.tagName==='TEXTAREA'))return;
var q=document.querySelector('.hero input[name=q]')||document.querySelector('.qs input');if(q&&q.offsetParent){e.preventDefault();q.focus();}});
var p=location.pathname,n=document.querySelectorAll('[data-nav]');for(var i=0;i<n.length;i++){var h=n[i].getAttribute('data-nav');if(h===p||(h==='/'&&(p.indexOf('/block')===0||p.indexOf('/tx')===0)))n[i].classList.add('on');}
})();
</script>`

const explorerProxyFixScript = `<script>
(function(){
var aa=document.querySelectorAll('.topnav a,div a,h1 span');
for(var i=0;i<aa.length;i++){var t=aa[i].textContent.trim();
if(t==='mainnet'||t==='testnet'||t==='TESTNET'){aa[i].remove();}}
})();
</script>`

// explorerLiveScript applies the /events stream to whatever page is open: tip
// height, next difficulty, the block list, the chain strip and confirmations.
const explorerLiveScript = `<script>
(function(){
if(!window.EventSource)return;
var es=new EventSource('/events');
function txt(id,v){var el=document.getElementById(id);if(el)el.textContent=v;}
function short(h){return h.slice(0,12)+'…'+h.slice(-6);}
function esc(s){return String(s).replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}
function addRow(d){
var tb=document.querySelector('table.recent-blocks tbody');if(!tb)return;
for(var i=0;i<tb.rows.length;i++){if(tb.rows[i].getAttribute('data-h')===String(d.height))return;}
var r=tb.insertRow(0);r.className='new';r.setAttribute('data-h',d.height);
r.innerHTML='<td><a href="/block/'+d.height+'">'+d.height+'</a></td><td class="hash"><a href="/block/'+esc(d.hash)+'">'+short(esc(d.hash))+'</a></td><td class="r"><span data-ts="'+d.timestamp+'"></span></td><td class="r">'+d.tx_count+'</td>';
while(tb.rows.length>20)tb.deleteRow(tb.rows.length-1);
}
function addTile(d){
var s=document.getElementById('strip');if(!s)return;
if(s.querySelector('[data-h="'+d.height+'"]'))return;
var a=document.createElement('a');a.className='blk new';a.href='/block/'+d.height;a.setAttribute('data-h',d.height);
a.innerHTML='<b>'+d.height+'</b><span>'+d.tx_count+' tx</span><span data-ts="'+d.timestamp+'"></span>';
s.insertBefore(a,s.firstChild);
while(s.children.length>14)s.removeChild(s.lastChild);
}
function confs(d){
var els=document.querySelectorAll('[data-conf-height]');
for(var i=0;i<els.length;i++){var at=parseInt(els[i].getAttribute('data-conf-height'),10);if(!isNaN(at))els[i].textContent=(d.height-at+1);}
var nx=document.getElementById('next-link');if(nx&&nx.classList.contains('off')&&d.height>=+nx.getAttribute('data-next')){nx.classList.remove('off');}
}
es.addEventListener('connected',function(ev){
var d;try{d=JSON.parse(ev.data);}catch(e){return;}
txt('stat-height',d.height);txt('stat-difficulty',d.next_difficulty);confs(d);
});
es.addEventListener('new_block',function(ev){
var d;try{d=JSON.parse(ev.data);}catch(e){return;}
txt('stat-height',d.height);txt('stat-difficulty',d.next_difficulty);
var tip=document.getElementById('tip-ago');if(tip)tip.setAttribute('data-ts',d.timestamp);
window.bnTipTs=d.timestamp;
addRow(d);addTile(d);confs(d);
if(window.bnTick)window.bnTick();
if(window.bnPulse)window.bnPulse();
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
var els=document.querySelectorAll('h1,h2,.nav a,.k,.v,.kk,.vv>span,td,th,footer a,button,.btn,.topnav a,.stat-k,.stat-v,.prop-k,.prop-v');
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

// explorerGyroidScript draws the small graphite gyroid in the index hero. It
// pauses off screen, lights up green toward the pointer, and flashes when a
// new block arrives (window.bnPulse, called by the live script).
const explorerGyroidScript = `<script>
(function(){
var c=document.getElementById('gy');if(!c)return;
var gl=c.getContext('webgl',{antialias:false,alpha:true,premultipliedAlpha:false});if(!gl){c.style.display='none';return;}
var ac=getComputedStyle(document.documentElement).getPropertyValue('--ac').trim()||'#af0';
function rgb(h){h=h.replace('#','');if(h.length===3)h=h[0]+h[0]+h[1]+h[1]+h[2]+h[2];return[parseInt(h.slice(0,2),16)/255,parseInt(h.slice(2,4),16)/255,parseInt(h.slice(4,6),16)/255];}
var AC=rgb(ac);
var vs='attribute vec2 p;void main(){gl_Position=vec4(p,0.,1.);}';
var fs='precision highp float;uniform vec2 R;uniform float T,P;uniform vec3 M,A;'+
'mat2 r2(float a){float c=cos(a),s=sin(a);return mat2(c,-s,s,c);}'+
'vec3 rot(vec3 p){p.xz*=r2(T*.23);p.yz*=r2(.45+sin(T*.11)*.2);return p;}'+
'float sd(vec3 p){p=rot(p);float k=4.2;float g=sin(p.x*k)*cos(p.y*k)+sin(p.y*k)*cos(p.z*k)+sin(p.z*k)*cos(p.x*k);float sh=abs(g)/k*.62-.035;return max(length(p)-1.,sh);}'+
'vec3 nr(vec3 p){vec2 e=vec2(.0015,0.);return normalize(vec3(sd(p+e.xyy)-sd(p-e.xyy),sd(p+e.yxy)-sd(p-e.yxy),sd(p+e.yyx)-sd(p-e.yyx)));}'+
'void main(){vec2 uv=(gl_FragCoord.xy*2.-R)/min(R.x,R.y);vec3 ro=vec3(0.,0.,3.1),rd=normalize(vec3(uv*.82,-2.));'+
'float b=dot(ro,rd),cc=dot(ro,ro)-1.03,h=b*b-cc;if(h<0.){gl_FragColor=vec4(0.);return;}'+
'float t=-b-sqrt(h),tm=-b+sqrt(h),d;bool hit=false;for(int i=0;i<64;i++){d=sd(ro+rd*t);if(d<.0012){hit=true;break;}t+=d*.9;if(t>tm)break;}'+
'if(!hit){gl_FragColor=vec4(0.);return;}'+
'vec3 p=ro+rd*t,n=nr(p),l=normalize(vec3(-.5,.8,.6));float df=max(dot(n,l),0.),ao=clamp(sd(p+n*.06)/.06,0.,1.);'+
'float fr=pow(1.-max(dot(n,-rd),0.),3.);vec3 col=vec3(.07,.075,.08)+vec3(.30,.31,.33)*df*(.4+.6*ao)+vec3(.12)*fr;'+
'col+=vec3(.5)*pow(max(dot(reflect(rd,n),l),0.),24.)*ao;'+
'vec3 lm=normalize(M-p);float g1=max(dot(n,lm),0.);float near=exp(-2.4*length(p.xy-M.xy));'+
'col+=A*(pow(g1,6.)*.9*near+fr*.25*near)*(.5+.5*ao);'+
'float rr=length(rot(p));col+=A*P*smoothstep(.25,0.,abs(rr-(1.-P)*1.1))*1.4;'+
'col=col/(1.+max(max(col.r,col.g),col.b)*.35);gl_FragColor=vec4(col,1.);}';
function sh(t,s){var o=gl.createShader(t);gl.shaderSource(o,s);gl.compileShader(o);return o;}
var pr=gl.createProgram();gl.attachShader(pr,sh(gl.VERTEX_SHADER,vs));gl.attachShader(pr,sh(gl.FRAGMENT_SHADER,fs));gl.linkProgram(pr);
if(!gl.getProgramParameter(pr,gl.LINK_STATUS)){c.style.display='none';return;}
gl.useProgram(pr);var bf=gl.createBuffer();gl.bindBuffer(gl.ARRAY_BUFFER,bf);gl.bufferData(gl.ARRAY_BUFFER,new Float32Array([-1,-1,3,-1,-1,3]),gl.STATIC_DRAW);
var lp=gl.getAttribLocation(pr,'p');gl.enableVertexAttribArray(lp);gl.vertexAttribPointer(lp,2,gl.FLOAT,false,0,0);
var uR=gl.getUniformLocation(pr,'R'),uT=gl.getUniformLocation(pr,'T'),uP=gl.getUniformLocation(pr,'P'),uM=gl.getUniformLocation(pr,'M'),uA=gl.getUniformLocation(pr,'A');
gl.uniform3f(uA,AC[0],AC[1],AC[2]);
var rm=window.matchMedia&&matchMedia('(prefers-reduced-motion: reduce)').matches;
var mx=.6,my=.5,tx=.6,ty=.5,idle=1,lastMove=0,pulse=0,vis=true,t0=performance.now(),W=0,H=0;
function size(){var dpr=Math.min(window.devicePixelRatio||1,1.5),w=Math.round(c.clientWidth*dpr),h=Math.round(c.clientHeight*dpr);if(w!==W||h!==H){W=c.width=w;H=c.height=h;gl.viewport(0,0,w,h);}}
window.addEventListener('pointermove',function(e){var r=c.getBoundingClientRect(),m=Math.min(r.width,r.height);tx=((e.clientX-r.left)*2-r.width)/m;ty=-((e.clientY-r.top)*2-r.height)/m;lastMove=performance.now();});
window.bnPulse=function(){pulse=1;if(rm)draw(performance.now());};
if('IntersectionObserver' in window){new IntersectionObserver(function(es){vis=es[0].isIntersecting;if(vis&&!rm)requestAnimationFrame(loop);}).observe(c);}
function draw(now){size();var t=rm?8:(now-t0)/1000;
var id=now-lastMove>2500;idle+=((id?1:0)-idle)*.03;
var ix=Math.cos(t*.5)*.9,iy=Math.sin(t*.37)*.6;
mx+=((tx*(1-idle)+ix*idle)-mx)*.12;my+=((ty*(1-idle)+iy*idle)-my)*.12;
gl.uniform2f(uR,W,H);gl.uniform1f(uT,t);gl.uniform1f(uP,pulse);gl.uniform3f(uM,mx,my,1.6);
gl.drawArrays(gl.TRIANGLES,0,3);pulse=Math.max(0,pulse-.012);}
function loop(now){if(!vis||document.hidden)return;draw(now);requestAnimationFrame(loop);}
document.addEventListener('visibilitychange',function(){if(!document.hidden&&vis&&!rm)requestAnimationFrame(loop);});
if(rm)draw(performance.now());else requestAnimationFrame(loop);
})();
</script>`

const explorerIndexCSS = `<style>
.hero{position:relative;border:1px solid var(--ln);border-radius:14px;background:radial-gradient(120% 140% at 85% 40%,#0b0f05 0%,#030303 55%,#000 100%);overflow:hidden;display:grid;grid-template-columns:minmax(0,1.15fr) minmax(0,.85fr);min-height:330px}
.hero-l{position:relative;z-index:2;padding:30px 32px;display:flex;flex-direction:column;gap:6px}
.live{font-size:12px;color:var(--mu);display:flex;gap:8px;align-items:center;text-transform:uppercase;letter-spacing:.8px}
.tip{font-size:64px;line-height:1.05;color:#fff;letter-spacing:-2px;font-variant-numeric:tabular-nums;margin-top:10px}
.tip-s{font-size:13px;color:var(--mu)}
.tip-s b{color:var(--tx);font-weight:normal}
.nxt{margin-top:14px;max-width:420px}
.nxt .row{display:flex;justify-content:space-between;font-size:12px;color:var(--mu)}
.hero form{display:flex;margin-top:auto;padding-top:22px;max-width:560px}
.hero input{flex:1;min-width:0;background:rgba(0,0,0,.7);border:1px solid var(--ln2);border-right:0;color:var(--hi);padding:13px 16px;font-size:14px;border-radius:8px 0 0 8px}
.hero input:focus{outline:none;border-color:var(--ac)}
.hero button{background:var(--ac);border:0;color:#000;padding:0 22px;font-size:14px;border-radius:0 8px 8px 0;cursor:pointer}
.hero button:hover{background:var(--ac-h)}
#gy{width:100%;height:100%;min-height:300px;display:block}
.gy-w{position:relative}
.strip{display:flex;gap:10px;overflow-x:auto;padding:2px 2px 10px;scrollbar-width:thin;scrollbar-color:#222 transparent}
.blk{flex:0 0 112px;display:flex;flex-direction:column;gap:1px;padding:12px 12px 10px;border:1px solid var(--ln);border-radius:9px;background:var(--pn);color:var(--mu);font-size:11px;position:relative}
.blk::before{content:"";position:absolute;left:-11px;top:50%;width:10px;height:1px;background:var(--ln2)}
.blk:first-child::before{display:none}
.blk b{color:var(--hi);font-weight:normal;font-size:14px}
.blk:hover{border-color:var(--ac);color:var(--tx)}
.blk.new{animation:blkin 2.6s ease-out}
@keyframes blkin{0%{border-color:var(--ac);box-shadow:0 0 22px rgba(170,255,0,.35);transform:translateY(-4px)}100%{border-color:var(--ln);box-shadow:none;transform:none}}
.far{display:flex;flex-direction:column;justify-content:space-between;background:linear-gradient(160deg,#0a0d04,#060606 60%);border-color:#1d2410}
.far .v{color:var(--ac)}
.far a{font-size:12px}
@media(max-width:860px){.hero{grid-template-columns:1fr}.gy-w{position:absolute;right:-70px;top:-10px;width:300px;height:300px;opacity:.55;z-index:1}.tip{font-size:46px}.hero-l{padding:24px 20px}}
@media(max-width:700px){.hero form{flex-direction:column;gap:8px}.hero input{border-right:1px solid var(--ln2);border-radius:8px}.hero button{padding:12px;border-radius:8px}}
</style>`

const explorerIndexTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>blocknet explorer</title>
<meta name="description" content="Live blocks, transactions and network stats for blocknet, zero-knowledge money.">
<meta property="og:title" content="blocknet explorer">
<meta property="og:description" content="Live blocks, transactions and network stats for blocknet.">
<meta property="og:url" content="https://explorer.blocknetcrypto.com">
<meta http-equiv="refresh" content="900">
{{template "head" .}}
` + explorerIndexCSS + `
</head>
<body>
{{template "top" .}}
<main class="wrap">

<section class="hero">
<div class="hero-l">
<div class="live"><span class="dot"></span>{{if .IsTestnet}}testnet{{else}}mainnet{{end}} · live</div>
<div class="tip" id="stat-height">{{.Height}}</div>
<div class="tip-s">block height · last block <b id="tip-ago" data-ts="{{.TipTs}}"></b></div>
<div class="nxt">
<div class="pbar"><i id="nb-bar" style="width:0"></i></div>
<div class="row" style="margin-top:6px"><span id="nb-txt">next block</span><span>target 5m</span></div>
</div>
<form action="/search" method="get">
<input type="text" name="q" placeholder="block height, block hash or tx hash" aria-label="Search" spellcheck="false" autocomplete="off">
<button type="submit">search</button>
</form>
</div>
<div class="gy-w"><canvas id="gy" aria-hidden="true"></canvas></div>
</section>

<section class="sec grid g4">
<div class="card"><div class="k">hashrate</div><div class="v">{{.Hashrate}} <small>H/s</small></div><div class="s">recent blocks</div></div>
<div class="card"><div class="k">next difficulty</div><div class="v" id="stat-difficulty">{{.Difficulty}}</div><div class="s">retargets every block</div></div>
<div class="card"><div class="k">avg block time</div><div class="v">{{.AvgBlockTime}}<small>s</small></div><div class="s">target 300s</div></div>
<div class="card"><div class="k">peers</div><div class="v">{{.Peers}}</div><div class="s">connected to this node</div></div>
</section>

<section class="sec grid g21">
<div class="card"><div class="k">supply emitted</div><div class="v">{{.Emitted}} <small>BNT</small></div>
<div class="pbar"><i style="width:{{.PctEmitted}}%"></i></div>
<div class="s" style="margin-top:8px">{{.PctEmitted}}% of pre-tail supply · {{.Remaining}} BNT left{{if .TailStarted}} · <span class="g">tail emission active</span>{{end}}</div></div>
<a class="card far" href="https://blocknetcrypto.com/#faraday"><div class="k">faraday upgrade</div><div class="v" id="far-days">~` + networkUpgradeDate + `</div><div class="s"><span id="far-s">planned ` + networkUpgradeDate + `</span> · details ↗</div></a>
</section>

<section class="sec">
<h2><span class="g">#</span> chain <span class="n">newest first</span></h2>
<div class="strip" id="strip">{{range $i, $b := .Blocks}}{{if lt $i 14}}<a class="blk" href="/block/{{$b.Height}}" data-h="{{$b.Height}}"><b>{{$b.Height}}</b><span>{{$b.TxCount}} tx</span><span data-ts="{{$b.Ts}}">{{$b.Ago}}</span></a>{{end}}{{end}}</div>
</section>

<section class="sec {{if .MempoolTxs}}grid g2{{end}}" style="align-items:start">
<div>
<h2><span class="g">#</span> recent blocks</h2>
<div class="card tbl"><table class="recent-blocks">
<thead><tr><th>height</th><th>hash</th><th class="r">age</th><th class="r">txs</th></tr></thead>
<tbody>{{range .Blocks}}<tr data-h="{{.Height}}"><td><a href="/block/{{.Height}}">{{.Height}}</a></td><td class="hash"><a href="/block/{{.Hash}}">{{slice .Hash 0 12}}…{{slice .Hash 58}}</a></td><td class="r"><span data-ts="{{.Ts}}">{{.Ago}}</span></td><td class="r">{{.TxCount}}</td></tr>{{end}}</tbody>
</table></div>
</div>
{{if .MempoolTxs}}<div>
<h2><span class="g">#</span> mempool <span class="n">{{.MempoolTotal}} waiting</span></h2>
<div class="card tbl"><table>
<thead><tr><th>hash</th><th class="r">fee</th><th class="r hide-s">fee/B</th><th class="r hide-s">size</th><th class="r">age</th></tr></thead>
<tbody>{{range .MempoolTxs}}<tr><td class="hash"><a href="/tx/{{.Hash}}">{{slice .Hash 0 12}}…</a></td><td class="r">{{printf "%.8f" .Fee}}</td><td class="r hide-s">{{.FeeRate}}</td><td class="r hide-s">{{.Size}} B</td><td class="r"><span data-ts="{{.Ts}}">{{.Ago}}</span></td></tr>{{end}}</tbody>
</table></div>
</div>{{end}}
</section>

</main>
<script>
(function(){
window.bnTipTs={{.TipTs}};
var bar=document.getElementById('nb-bar'),tx=document.getElementById('nb-txt');
function nb(){var s=Date.now()/1000-window.bnTipTs,f=Math.min(1,s/300);bar.style.width=(f*100)+'%';
if(s<300){var r=Math.ceil(300-s);tx.textContent='next block in ~'+(r>=60?Math.floor(r/60)+'m '+(r%60)+'s':r+'s');}else tx.textContent='next block any moment';}
nb();setInterval(nb,1000);
var up=Date.parse('` + networkUpgradeDate + `T00:00:00Z'),days=Math.ceil((up-Date.now())/864e5);
if(days>0){document.getElementById('far-days').textContent='~'+days+' days';}
})();
</script>
` + explorerGyroidScript + `
{{template "foot" .}}
</body>
</html>`

const explorerBlockTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Block {{.Height}} · blocknet explorer</title>
<meta name="description" content="blocknet block {{.Height}}">
<meta property="og:title" content="Block {{.Height}} · blocknet">
<meta property="og:description" content="blocknet block {{.Height}}: {{.TxCount}} transactions.">
<meta property="og:url" content="https://explorer.blocknetcrypto.com/block/{{.Height}}">
{{template "head" .}}
</head>
<body>
{{template "top" .}}
<main class="wrap">

<div class="crumb"><a href="/">blocks</a> / block</div>
<div class="ph">
<h1><span class="g">#</span>{{.Height}} <span class="d">block</span></h1>
<div class="pager">
<a class="btn{{if not .HasPrev}} off{{end}}" href="/block/{{.PrevHeight}}" id="prev-link">← {{.PrevHeight}}</a>
<a class="btn{{if not .HasNext}} off{{end}}" href="/block/{{.NextHeight}}" id="next-link" data-next="{{.NextHeight}}">{{.NextHeight}} →</a>
<span class="hint">← → keys</span>
</div>
</div>

<div style="display:flex;gap:8px;flex-wrap:wrap;margin:-8px 0 22px">
<span class="badge ok"><span data-conf-height="{{.Height}}">{{.Confirmations}}</span> confirmations</span>
{{if .Final}}<span class="badge">final</span>{{else}}<span class="badge wait">final after 100 confirmations</span>{{end}}
<span class="badge"><span data-ts="{{.Ts}}"></span></span>
</div>

<div class="grid g2" style="align-items:start">
<div class="card">
<div class="kv">
<div class="kk">hash</div><div class="vv m"><span{{if .IsGenesis}} title="{{.GenesisMsg}}" style="cursor:help"{{end}}>{{.Hash}}</span><button class="cp" data-cp="{{.Hash}}">copy</button></div>
<div class="kk">previous</div><div class="vv m"><span>{{if .HasPrev}}<a href="/block/{{.PrevHash}}">{{.PrevHash}}</a>{{else}}{{.PrevHash}}{{end}}</span></div>
<div class="kk">merkle root</div><div class="vv m"><span>{{.MerkleRoot}}</span></div>
<div class="kk">time</div><div class="vv"><span>{{.Time}}</span></div>
<div class="kk">difficulty</div><div class="vv"><span>{{.Difficulty}}</span></div>
<div class="kk">nonce</div><div class="vv"><span>{{.Nonce}}</span></div>
<div class="kk">reward</div><div class="vv"><span>{{printf "%.8f" .Reward}} <span class="d">BNT</span></span></div>
</div>
</div>

<div>
<h2><span class="g">#</span> transactions <span class="n">{{.TxCount}}</span></h2>
<div class="card tbl"><table>
<thead><tr><th>hash</th><th>type</th><th class="r">in</th><th class="r">out</th></tr></thead>
<tbody>{{range .Txs}}<tr><td class="hash"><a href="/tx/{{.Hash}}">{{slice .Hash 0 16}}…{{slice .Hash 58}}</a></td><td>{{if .IsCoinbase}}<span class="chip cb">coinbase</span>{{else}}<span class="chip">transfer</span>{{end}}</td><td class="r">{{.Inputs}}</td><td class="r">{{.Outputs}}</td></tr>{{end}}</tbody>
</table></div>
</div>
</div>

</main>
<script>
document.addEventListener('keydown',function(e){if(e.metaKey||e.ctrlKey||e.altKey)return;var a=document.activeElement;if(a&&a.tagName==='INPUT')return;
var l=e.key==='ArrowLeft'?document.getElementById('prev-link'):e.key==='ArrowRight'?document.getElementById('next-link'):null;
if(l&&!l.classList.contains('off'))location.href=l.href;});
</script>
{{template "foot" .}}
</body>
</html>`

const explorerTxCSS = `<style>
.io{margin-bottom:12px}
.io-h{display:flex;justify-content:space-between;align-items:center;margin-bottom:6px}
.io-h b{color:var(--hi);font-weight:normal}
.ringw{display:flex;gap:16px;align-items:center;margin:4px 0 6px}
.ring{width:84px;height:84px;flex-shrink:0}
.ring-t{font-size:12px;color:var(--mu);line-height:1.5}
.ring-t b{color:var(--tx);font-weight:normal}
.lock{color:var(--ac)}
.tabs{display:flex;gap:4px;margin-bottom:14px;background:#0a0a0a;border:1px solid var(--ln);border-radius:8px;padding:3px;width:max-content}
.tab{background:transparent;color:var(--mu);border:0;padding:6px 14px;cursor:pointer;font-size:12px;border-radius:6px}
.tab.on{background:var(--ac);color:#000}
.prove input{display:block;width:100%;background:#000;border:1px solid var(--ln2);color:var(--hi);padding:10px 12px;font-size:13px;margin-bottom:8px;border-radius:7px}
.prove input:focus{outline:none;border-color:var(--ac)}
.prove button.go{background:var(--ac);border:0;color:#000;padding:10px 22px;cursor:pointer;font-size:13px;border-radius:7px}
.prove button.go:hover{background:var(--ac-h)}
.prove button.go:disabled{opacity:.5}
.warnl{font-size:12px;color:var(--warn);margin:0 0 12px}
#prove-result{margin-top:14px}
.io.more{display:none}
.all-out .io.more{display:block}
#more-out{cursor:pointer;font-family:inherit}
.vv .trunc{white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
</style>`

const explorerTxTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Transaction {{slice .Hash 0 12}} · blocknet explorer</title>
<meta name="description" content="blocknet transaction">
<meta property="og:title" content="Transaction · blocknet">
<meta property="og:description" content="A private blocknet transaction. Amounts and senders are hidden.">
{{template "head" .}}
` + explorerTxCSS + `
</head>
<body>
{{template "top" .}}
<main class="wrap">

<div class="crumb"><a href="/">blocks</a> / {{if .InMempool}}mempool{{else}}<a href="/block/{{.BlockHeight}}">block {{.BlockHeight}}</a>{{end}} / transaction</div>
<div class="ph">
<h1><span class="g">#</span>{{slice .Hash 0 12}}… <span class="d">transaction</span></h1>
<div style="display:flex;gap:8px;flex-wrap:wrap">
{{if .InMempool}}<span class="badge wait"><span class="dot" style="background:var(--warn);animation:none"></span>Pending (in mempool)</span>{{else}}<span class="badge ok"><span id="tx-confirmations" data-conf-height="{{.BlockHeight}}">{{.Confirmations}}</span> confirmations</span>{{end}}
{{if .IsCoinbase}}<span class="badge ok">coinbase</span>{{else}}<span class="badge">transfer</span>{{end}}
</div>
</div>

<div class="grid g2" style="align-items:start">
<div>
<div class="card">
<div class="kv">
<div class="kk">hash</div><div class="vv m"><span>{{.Hash}}</span><button class="cp" data-cp="{{.Hash}}">copy</button></div>
<div class="kk">tx public key</div><div class="vv m"><span>{{.TxPubKey}}</span><button class="cp" data-cp="{{.TxPubKey}}">copy</button></div>
<div class="kk">block</div><div class="vv"><span>{{if .InMempool}}<span class="d">pending</span>{{else}}<a href="/block/{{.BlockHeight}}">{{.BlockHeight}}</a>{{end}}</span></div>
{{if not .IsCoinbase}}<div class="kk">fee</div><div class="vv"><span>{{printf "%.8f" .Fee}} <span class="d">BNT</span></span></div>{{end}}
<div class="kk">inputs</div><div class="vv"><span>{{.InputCount}}</span></div>
<div class="kk">outputs</div><div class="vv"><span>{{.OutputCount}}</span></div>
<div class="kk">amounts</div><div class="vv"><span><span class="lock">●</span> hidden <span class="d">· Pedersen commitments</span></span></div>
</div>
</div>

<div class="card prove sec">
<h2 style="margin-bottom:12px"><span class="g">#</span> tx proof</h2>
<div class="tabs"><button id="tab-recv" class="tab on" onclick="switchProveTab('recv')">received</button><button id="tab-send" class="tab" onclick="switchProveTab('send')">sent</button></div>
<div id="prove-recv">
<p class="note" style="margin:0 0 10px">Check if an output in this transaction is yours.</p>
<p class="warnl">Your view private key is sent to this server.</p>
<input type="text" id="recv-spend" placeholder="spend public key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off">
<input type="text" id="recv-view" placeholder="view private key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off">
<button class="go" id="recv-btn" onclick="doRecvProve()">check</button>
</div>
<div id="prove-send" style="display:none">
<p class="note" style="margin:0 0 10px">Prove you sent this transaction. Give the view private key and the recipient's public keys. Anyone can check this proof.</p>
<input type="text" id="send-txpriv" placeholder="view private key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off">
<input type="text" id="send-spend" placeholder="recipient spend public key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off">
<input type="text" id="send-view" placeholder="recipient view public key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off">
<button class="go" id="send-btn" onclick="doSendProve()">prove</button>
</div>
<div id="prove-result" style="display:none"></div>
</div>
</div>

<div>
{{if .Inputs}}
<h2><span class="g">#</span> inputs <span class="n">ring signatures</span></h2>
{{range .Inputs}}
<div class="card io">
<div class="io-h"><b>input {{.Index}}</b><span class="chip">ring of {{.RingSize}}</span></div>
<div class="ringw"><svg class="ring" data-n="{{.RingSize}}" viewBox="0 0 84 84" aria-hidden="true"></svg>
<div class="ring-t"><b>1 real input, {{.RingSize}} keys in all.</b><br>The others are decoys. Nobody can tell which one is real.</div></div>
<div class="kv">
<div class="kk">key image</div><div class="vv m"><span>{{.KeyImage}}</span></div>
<div class="kk">pseudo output</div><div class="vv m"><span>{{.PseudoOutput}}</span></div>
</div>
</div>
{{end}}
{{end}}

<h2 class="{{if .Inputs}}sec{{end}}"><span class="g">#</span> outputs <span class="n">stealth addresses</span></h2>
{{range $i, $o := .Outputs}}
<div class="card io{{if ge $i 4}} more{{end}}">
<div class="io-h"><b>output {{.Index}}</b><span class="chip"><span class="lock">●</span> amount hidden</span></div>
<div class="kv">
<div class="kk">stealth address</div><div class="vv m"><span>{{.PublicKey}}</span></div>
<div class="kk">commitment</div><div class="vv m"><span>{{.Commitment}}</span></div>
<div class="kk">range proof</div><div class="vv"><span>{{.RangeProof}} bytes <span class="d">· Bulletproof</span></span></div>
{{if .EncMemo}}<div class="kk">memo</div><div class="vv m"><span class="trunc"><span class="g">encrypted</span> {{.EncMemo}}</span></div>{{end}}
</div>
</div>
{{end}}
{{if gt (len .Outputs) 4}}<button class="btn" id="more-out" onclick="document.body.classList.add('all-out');this.remove()">show all {{len .Outputs}} outputs</button>{{end}}
</div>
</div>

</main>
<script>
(function(){
var rings=document.querySelectorAll('svg.ring'),NS='http://www.w3.org/2000/svg',rm=window.matchMedia&&matchMedia('(prefers-reduced-motion: reduce)').matches;
for(var k=0;k<rings.length;k++){var s=rings[k],n=+s.getAttribute('data-n')||1,dots=[];
var c=document.createElementNS(NS,'circle');c.setAttribute('cx',42);c.setAttribute('cy',42);c.setAttribute('r',30);c.setAttribute('fill','none');c.setAttribute('stroke','#1c1c1c');s.appendChild(c);
for(var i=0;i<n;i++){var a=i/n*Math.PI*2-Math.PI/2,d=document.createElementNS(NS,'circle');d.setAttribute('cx',(42+30*Math.cos(a)).toFixed(2));d.setAttribute('cy',(42+30*Math.sin(a)).toFixed(2));d.setAttribute('r',3.4);d.setAttribute('fill','#3a3a3a');s.appendChild(d);dots.push(d);}
var q=document.createElementNS(NS,'text');q.setAttribute('x',42);q.setAttribute('y',47);q.setAttribute('text-anchor','middle');q.setAttribute('fill','#666');q.setAttribute('font-size','14');q.setAttribute('font-family','ui-monospace,monospace');q.textContent='?';s.appendChild(q);
if(!rm)(function(dots){var cur=-1;setInterval(function(){if(cur>=0)dots[cur].setAttribute('fill','#3a3a3a');var nx;do{nx=Math.random()*dots.length|0;}while(nx===cur&&dots.length>1);cur=nx;dots[cur].setAttribute('fill',getComputedStyle(document.documentElement).getPropertyValue('--ac').trim()||'#af0');},420);})(dots);}
})();
function switchProveTab(tab){
document.getElementById('prove-result').style.display='none';
document.getElementById('tab-recv').className='tab'+(tab==='recv'?' on':'');
document.getElementById('tab-send').className='tab'+(tab==='send'?' on':'');
document.getElementById('prove-recv').style.display=tab==='recv'?'':'none';
document.getElementById('prove-send').style.display=tab==='send'?'':'none';
}
function esc(s){return String(s).replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}
function renderProveResult(data){
var res=document.getElementById('prove-result');
res.style.display='block';
if(data.error){res.innerHTML='<span style="color:#f44">'+esc(data.error)+'</span>';return;}
var matched=data.filter(function(o){return o.match;});
if(matched.length===0){res.innerHTML='<span class="d">No outputs in this transaction match these keys.</span>';return;}
var html='<div class="g" style="margin-bottom:8px">'+matched.length+' output'+(matched.length>1?'s':'')+' matched</div>';
for(var i=0;i<matched.length;i++){
var o=matched[i];
html+='<div class="kv" style="border-top:1px solid #111;padding-top:4px"><div class="kk">output '+o.index+'</div><div class="vv"><span class="g" style="font-size:18px">'+o.amount.toFixed(8)+' BNT</span></div>';
if(o.memo){html+='<div class="kk">memo</div><div class="vv"><span>'+esc(o.memo)+'</span></div>';}
html+='</div>';
}
res.innerHTML=html;
}
function proveRequest(url,body,btn){
btn.disabled=true;btn.textContent='checking…';
fetch(url,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:body})
.then(function(r){return r.json();})
.then(function(data){btn.disabled=false;btn.textContent=btn.dataset.label;renderProveResult(data);})
.catch(function(){btn.disabled=false;btn.textContent=btn.dataset.label;var r=document.getElementById('prove-result');r.style.display='block';r.innerHTML='<span style="color:#f44">Request failed</span>';});
}
document.getElementById('recv-btn').dataset.label='check';
document.getElementById('send-btn').dataset.label='prove';
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
{{template "foot" .}}
</body>
</html>`

const explorerStatsCSS = `<style>
.charts{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}
.chart{padding:16px 16px 8px}
.chart h2{font-size:13px;margin-bottom:6px}
.chart.wide{grid-column:1/-1}
canvas{width:100%;height:260px;display:block}
@media(max-width:980px){.charts{grid-template-columns:1fr}}
</style>`

const explorerStatsTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Network stats · blocknet explorer</title>
<meta name="description" content="blocknet network statistics">
<meta property="og:title" content="Network stats · blocknet">
<meta property="og:description" content="Difficulty, hashrate, block time and emission for blocknet.">
<meta property="og:url" content="https://explorer.blocknetcrypto.com/stats">
{{template "head" .}}
` + explorerStatsCSS + `
</head>
<body>
{{template "top" .}}
<main class="wrap">

<div class="ph"><h1><span class="g">#</span>network <span class="d">stats</span></h1><span class="badge"><span class="dot"></span>live</span></div>

<section class="grid g4">
<div class="card"><div class="k">block height</div><div class="v" id="stat-height">{{.Height}}</div></div>
<div class="card"><div class="k">hashrate</div><div class="v">{{.Hashrate}} <small>H/s</small></div></div>
<div class="card"><div class="k">next difficulty</div><div class="v" id="stat-difficulty">{{.Difficulty}}</div></div>
<div class="card"><div class="k">avg block time</div><div class="v">{{.AvgBlockTime}}<small>s</small></div></div>
<div class="card"><div class="k">window transactions</div><div class="v">{{.TotalTx}}</div></div>
<div class="card"><div class="k">coins emitted</div><div class="v">{{.Emitted}}</div></div>
<div class="card"><div class="k">emission progress</div><div class="v">{{.PctEmitted}}<small>%</small></div><div class="pbar"><i style="width:{{.PctEmitted}}%"></i></div></div>
<div class="card"><div class="k">peers</div><div class="v">{{.Peers}}</div></div>
</section>

<section class="sec charts">
<div class="card chart wide"><h2><span class="g">#</span> difficulty</h2><canvas id="c-diff"></canvas></div>
<div class="card chart"><h2 style="color:#a0f"><span>#</span> difficulty delta</h2><canvas id="c-ddelta"></canvas></div>
<div class="card chart"><h2 style="color:#fa0"><span>#</span> estimated hashrate</h2><canvas id="c-hash"></canvas></div>
<div class="card chart"><h2 style="color:#f0a"><span>#</span> block time</h2><canvas id="c-bt"></canvas></div>
<div class="card chart"><h2 style="color:#a0f"><span>#</span> nonce distribution</h2><canvas id="c-nonce"></canvas></div>
<div class="card chart"><h2 style="color:#0fa"><span>#</span> transactions per block</h2><canvas id="c-tx"></canvas></div>
<div class="card chart"><h2 style="color:#0af"><span>#</span> block size</h2><canvas id="c-size"></canvas></div>
<div class="card chart wide"><h2><span class="g">#</span> emission schedule</h2><canvas id="c-emission"></canvas></div>
</section>

</main>

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
var pad={t:24,r:30,b:32,l:60};
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
var pad={t:24,r:30,b:32,l:60};
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
var pad={t:24,r:30,b:32,l:60};
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
{{template "foot" .}}
</body>
</html>`
