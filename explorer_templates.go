package main

import "html/template"

// Parsed once at init: template.Must fails the process at startup rather than
// letting a malformed page surface as a 500 on the first request. Every page
// shares the head, top bar and footer defined in explorerCommonTmpl.
var (
	explorerIndexPage    = template.Must(template.New("index").Parse(explorerCommonTmpl + explorerIndexTmpl))
	explorerBlockPage    = template.Must(template.New("block").Parse(explorerCommonTmpl + explorerBlockTmpl))
	explorerTxPage       = template.Must(template.New("tx").Parse(explorerCommonTmpl + explorerTxTmpl))
	explorerStatsPage    = template.Must(template.New("stats").Parse(explorerCommonTmpl + explorerStatsTmpl))
	explorerNotFoundPage = template.Must(template.New("notfound").Parse(explorerCommonTmpl + explorerNotFoundTmpl))
)

const explorerCSS = `
:root{--ac:#af0;--tx:#b4b4b4;--hi:#f0f0f0;--mu:#6c6c6c;--dim:#444;--ln:#1a1a1a;--pn:#070707}
*{box-sizing:border-box;margin:0;padding:0}
html{background:#000}
body{background:#000;color:var(--tx);font:14px/1.55 ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;-webkit-text-size-adjust:100%}
a{color:var(--ac);text-decoration:none}
.wrap{max-width:1180px;margin:0 auto;padding:0 24px}
.top{display:flex;align-items:center;gap:20px;height:60px}
.brand{color:var(--hi);font-size:17px;white-space:nowrap}
.brand i{color:var(--ac);font-style:normal;animation:bl 1.1s steps(1) infinite}
@keyframes bl{50%{opacity:0}}
.nav{display:flex;gap:18px;font-size:13px}
.nav a{color:var(--mu)}
.nav a.on{color:var(--ac)}
.q{margin-left:auto;flex:0 1 320px;min-width:0}
.q input{width:100%;background:#0008;border:1px solid #222;color:var(--hi);font:inherit;font-size:12px;padding:8px 12px;border-radius:8px}
.q input:focus{outline:none;border-color:var(--ac)}
.advb{background:none;border:1px solid #222;color:var(--mu);font:inherit;font-size:12px;padding:6px 12px;border-radius:999px;cursor:pointer}
body.adv .advb{color:var(--ac);border-color:var(--ac)}
.k{font-size:11px;color:var(--mu);letter-spacing:.7px;text-transform:uppercase}
.v{font-size:22px;color:var(--hi);font-variant-numeric:tabular-nums;white-space:nowrap}
.v small{font-size:12px;color:var(--mu);margin-left:3px}
.big{font-size:84px;line-height:1;color:#fff;letter-spacing:-3px;font-variant-numeric:tabular-nums}
.live{display:inline-flex;gap:8px;align-items:center;font-size:11px;color:var(--mu);letter-spacing:1px;text-transform:uppercase}
.live:before{content:"";width:7px;height:7px;border-radius:50%;background:var(--ac);box-shadow:0 0 10px var(--ac)}
.nb{color:var(--tx)}
.nb b{color:var(--ac);font-weight:normal}
.card{background:var(--pn);border:1px solid var(--ln);border-radius:12px}
.pad{padding:18px 20px}
h3{font-size:12px;font-weight:normal;color:var(--hi);letter-spacing:.6px;text-transform:uppercase;display:flex;align-items:center;gap:10px}
h3 em{font-style:normal;color:var(--dim);text-transform:none;letter-spacing:0}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:9px 10px;white-space:nowrap;font-variant-numeric:tabular-nums}
th{font-size:11px;color:var(--mu);font-weight:normal;text-transform:uppercase;letter-spacing:.6px}
td{border-top:1px solid #0f0f0f}
td.r,th.r{text-align:right}
td.h{color:var(--ac)}
td.m{color:#777}
tbody tr:hover td{background:#0c0c0c}
tr.new td{animation:rowin 2.5s ease-out}
@keyframes rowin{from{background:#af03}to{background:transparent}}
.ad{display:none!important}
body.adv .ad{display:revert!important}
body.adv td.ad,body.adv th.ad{display:table-cell!important}
.grid{display:grid;gap:12px}
.tip{position:fixed;z-index:60;pointer-events:none;display:none;background:#050505ee;border:1px solid #2a2a2a;border-radius:9px;padding:8px 11px;white-space:nowrap;box-shadow:0 10px 30px #000;-webkit-backdrop-filter:blur(8px);backdrop-filter:blur(8px)}
.tip .a{color:var(--hi)}
.tip .a b{color:var(--ac);font-weight:normal}
.tip .b{color:var(--mu);font-size:11px}
.chips{display:flex;gap:6px;margin-left:auto}
.chips button{background:none;border:1px solid #222;color:var(--mu);font:inherit;font-size:11px;padding:3px 10px;border-radius:999px;cursor:pointer}
.chips button.on{color:#000;background:var(--ac);border-color:var(--ac)}
.legend{display:flex;align-items:center;gap:8px;font-size:11px;color:var(--mu);flex-wrap:wrap}
.legend i{display:block;width:120px;height:8px;border-radius:2px;background:linear-gradient(90deg,#121412,#1a3d00,#af0,#efffd0)}
.legend .mp{width:10px;height:10px;border:1px solid var(--ac);border-radius:2px;display:inline-block}
.legend .fd{width:10px;height:10px;border:1px solid #fff;border-radius:50%;display:inline-block;box-shadow:0 0 6px #fff8}
.foot{color:var(--dim);font-size:12px;padding:40px 0}
.hero{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.1fr);gap:28px;align-items:end;padding:36px 0 26px}
.st{grid-template-columns:repeat(3,minmax(0,1fr))}
.st .card{padding:14px 16px}
#heat{display:block;width:100%;height:250px;margin-top:14px;cursor:crosshair;touch-action:none}
.two{display:grid;grid-template-columns:minmax(0,1.2fr) minmax(0,1fr);gap:12px;margin-top:12px}
.far{display:flex;align-items:center;gap:10px;font-size:12px;color:var(--mu)}
.far b{color:var(--hi);font-weight:normal}
#blk{display:grid;grid-template-rows:0fr;transition:grid-template-rows .35s cubic-bezier(.2,.8,.2,1),margin .35s;margin-top:0}
#blk.on{grid-template-rows:1fr;margin-top:16px}
#blk>div{overflow:hidden}
.bp{border-top:1px solid var(--ln);padding-top:16px;display:grid;grid-template-columns:minmax(0,.9fr) minmax(0,1.4fr);gap:24px}
.bp .bh{font-size:40px;color:#fff;letter-spacing:-1px;line-height:1}
.bp .bh span{color:var(--ac)}
.bp .row{display:flex;gap:8px;flex-wrap:wrap;margin-top:10px}
.pill{font-size:11px;border:1px solid #2a2a2a;border-radius:999px;padding:2px 9px;color:var(--tx)}
.pill.g{border-color:#3c5a0a;color:var(--ac)}
.tbar{height:6px;background:#141414;border-radius:3px;position:relative;margin-top:16px;overflow:visible}
.tbar i{position:absolute;left:0;top:0;bottom:0;border-radius:3px;background:var(--ac)}
.tbar b{position:absolute;top:-4px;bottom:-4px;width:1px;background:#888}
.tlab{display:flex;justify-content:space-between;font-size:11px;color:var(--mu);margin-top:6px}
.bnav{display:flex;gap:8px;margin-top:16px}
.bnav button{background:none;border:1px solid #2a2a2a;color:var(--tx);font:inherit;font-size:12px;padding:5px 12px;border-radius:7px;cursor:pointer}
.bnav button:hover{border-color:var(--ac);color:var(--ac)}
.bnav button:disabled{opacity:.3;pointer-events:none}
.dots{display:inline-flex;gap:2px;vertical-align:middle;margin-left:6px}
.dots i{width:5px;height:5px;border-radius:50%;background:#555}
.dots i.o{background:var(--ac)}
.kv2{display:grid;grid-template-columns:auto 1fr;gap:0 14px;font-size:12px;margin-top:4px}
.kv2 div{padding:5px 0;border-top:1px solid #0f0f0f}
.kv2 .kk{color:var(--mu)}
.kv2 .vv{color:var(--hi);text-align:right;word-break:break-all}
@media(max-width:860px){.bp{grid-template-columns:minmax(0,1fr)}}
.hrow{display:flex;align-items:center;gap:18px;margin:10px 0}
#gy{width:92px;height:92px;flex-shrink:0}
#gyl{position:absolute;right:0;top:18px;width:56px;height:56px;pointer-events:none}
@media(max-width:860px){#gy{width:64px;height:64px}}
@media(max-width:860px){.wrap{padding:0 16px}.q{display:none}.big{font-size:56px;letter-spacing:-2px}.hero,.two{grid-template-columns:minmax(0,1fr)}#heat{height:220px}.hide-s{display:none}.chips{margin-left:0}}
@media(prefers-reduced-motion:reduce){*{animation:none!important;transition:none!important}}


.nav a{cursor:pointer}
.crumb{font-size:12px;color:var(--dim);margin:26px 0 8px}
.crumb a{color:var(--mu)}
.vh{display:flex;align-items:flex-end;justify-content:space-between;gap:20px;flex-wrap:wrap;margin-bottom:22px}
.vh .ttl{display:flex;align-items:center;gap:16px}
.vh .ttl .big{font-size:64px;letter-spacing:-2px}
.vh .ttl .big span{color:var(--ac)}
.vh .ttl .mid{font-size:34px;color:#fff;letter-spacing:-1px;line-height:1.1;word-break:break-all}
.vh .ttl .mid span{color:var(--ac)}
.slot{width:72px;height:72px;flex-shrink:0}
.slot canvas{width:100%;height:100%;display:block}
.pills{display:flex;gap:8px;flex-wrap:wrap;margin-top:12px}
.navbtn{display:flex;gap:8px}
.navbtn a,.btn{display:inline-flex;align-items:center;background:none;border:1px solid #2a2a2a;color:var(--tx);font:inherit;font-size:12px;padding:6px 12px;border-radius:7px;cursor:pointer}
.navbtn a:hover,.btn:hover{border-color:var(--ac);color:var(--ac)}
.navbtn a.off{opacity:.3;pointer-events:none}


.kv3{display:grid;grid-template-columns:150px minmax(0,1fr);font-size:13px}
.kv3>div{padding:10px 0;border-top:1px solid #0f0f0f}
.kv3>div:nth-child(-n+2){border-top:0}
.kv3 .kk{color:var(--mu);font-size:11px;text-transform:uppercase;letter-spacing:.6px;padding-top:12px}
.kv3 .vv{color:var(--hi);word-break:break-all;display:flex;gap:10px;align-items:flex-start}
.kv3 .vv.m{color:#9a9a9a;font-size:12px}
.kv3 .vv>span{min-width:0}
.cp{flex-shrink:0;background:none;border:1px solid #2a2a2a;color:var(--mu);font:inherit;font-size:11px;padding:2px 7px;border-radius:5px;cursor:pointer}
.cp:hover{color:var(--ac);border-color:var(--ac)}
.cp.done{background:var(--ac);color:#000;border-color:var(--ac)}
.vgrid{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.15fr);gap:12px;align-items:start}



.ioc{border-top:1px solid #0f0f0f;padding:14px 0}
.ioc:first-of-type{border-top:0}
.ioc .hd{display:flex;justify-content:space-between;align-items:center;color:var(--hi);margin-bottom:6px}
.note{font-size:12px;color:var(--mu)}
.tabs{display:flex;gap:4px;background:#0a0a0a;border:1px solid var(--ln);border-radius:8px;padding:3px;width:max-content;margin:12px 0}
.tabs button{background:none;border:0;color:var(--mu);font:inherit;font-size:12px;padding:5px 12px;border-radius:6px;cursor:pointer}
.tabs button.on{background:var(--ac);color:#000}
.prove input{display:block;width:100%;background:#000;border:1px solid #222;color:var(--hi);font:inherit;font-size:13px;padding:10px 12px;margin-bottom:8px;border-radius:7px}
.prove input:focus{outline:none;border-color:var(--ac)}
.prove .go{background:var(--ac);border:0;color:#000;font:inherit;font-size:13px;padding:9px 20px;border-radius:7px;cursor:pointer}
.warnl{font-size:12px;color:#fa0;margin:0 0 10px}


.cax{display:flex;justify-content:space-between;font-size:11px;color:var(--dim);margin-top:6px}
.cnow{font-size:12px;color:var(--mu)}
.cnow b{color:var(--hi);font-weight:normal}
.nf{text-align:center;padding:60px 0 20px}
.nf .slot{width:200px;height:200px;margin:0 auto}
.nf h1{font-weight:normal;color:#fff;font-size:28px;margin-top:20px}
.nf p{color:var(--mu);margin-top:8px}
.nf .q2{max-width:520px;margin:24px auto 0;display:flex}
.nf .q2 input{flex:1;background:#000;border:1px solid #2a2a2a;border-right:0;color:var(--hi);font:inherit;padding:12px 14px;border-radius:8px 0 0 8px}
.nf .q2 input:focus{outline:none;border-color:var(--ac)}
.nf .q2 button{background:var(--ac);border:0;color:#000;font:inherit;padding:0 18px;border-radius:0 8px 8px 0;cursor:pointer}
.tbl-click tbody tr,tr[data-go]{cursor:pointer}
@media(max-width:860px){.vgrid{grid-template-columns:minmax(0,1fr)}.vh .ttl .big{font-size:42px}.vh .ttl .mid{font-size:22px}.slot{width:52px;height:52px}.kv3{grid-template-columns:minmax(0,1fr)}.kv3 .kk{padding:10px 0 0;border-top:1px solid #0f0f0f}.kv3 .vv{border-top:0!important;padding-top:2px}}
.sboard .card{padding:18px 20px}
.sboard .g{display:grid;gap:12px}
.sboard .g4{grid-template-columns:repeat(4,minmax(0,1fr))}
.sboard .mt{margin-top:12px}
.sboard .plot{position:relative;width:100%;margin-top:12px}
.sboard .plot canvas{position:absolute;inset:0;width:100%;height:100%;display:block}
.sboard .plot canvas+canvas{cursor:crosshair}
.sboard .note{font-size:12px;color:var(--mu)}
.sboard .big{color:#fff;letter-spacing:-1px;line-height:1;font-variant-numeric:tabular-nums}
.sboard .v small{font-size:12px;color:var(--mu);margin-left:3px}
@media(max-width:860px){.sboard .g4{grid-template-columns:repeat(2,minmax(0,1fr))}}
.notice{border-bottom:1px solid #2a1d00;background:#0d0800;color:#c80;font-size:12px;line-height:1.5}
.notice .wrap{padding-top:8px;padding-bottom:8px;display:flex;gap:10px;align-items:baseline}
.notice b{color:#fa0;font-weight:normal;white-space:nowrap}
.notice a{color:#fa0;white-space:nowrap}
.notice .ns{display:none}
footer{margin-top:64px;border-top:1px solid var(--ln);color:var(--dim);font-size:12px}
footer .wrap{display:flex;gap:18px;flex-wrap:wrap;padding-top:22px;padding-bottom:32px}
footer a{color:var(--mu)}
footer a:hover{color:var(--ac)}
.brand i{cursor:pointer;-webkit-user-select:none;user-select:none}
.err{color:#f44}
#prove-result{margin-top:12px;font-size:13px}
.mt{margin-top:12px}
@media(max-width:700px){.notice .nl{display:none}.notice .ns{display:inline}}
`

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
{{if .IsTestnet}}<style>:root{--ac:#f0a}.live:before{background:#f0a;box-shadow:0 0 10px #f0a}</style>{{end}}{{end}}
{{define "top"}}<div class="wrap"><div class="top"><a class="brand" href="/">blocknet<i id="egg">_</i></a><nav class="nav"><a href="/" data-nav="blocks">blocks</a><a href="/stats" data-nav="stats">stats</a><a class="hide-s" href="https://blocknetcrypto.com/#faraday">faraday</a></nav><form class="q" action="/search" method="get"><input name="q" placeholder="search height, block or tx hash" spellcheck="false" autocomplete="off" aria-label="Search"></form><button class="advb" id="advb" type="button">advanced</button></div></div>
` + explorerUpgradeBanner + `
{{end}}
{{define "foot"}}<footer><div class="wrap"><a href="https://blocknetcrypto.com">blocknetcrypto.com</a><a href="https://blocknetcrypto.com/#faraday">faraday upgrade</a><a href="https://blocknetcrypto.com/api.html">api</a><a href="https://github.com/blocknetprivacy/core">github</a>{{if .IsTestnet}}<span style="color:var(--ac)">testnet</span>{{end}}</div></footer>
` + explorerShaderScript + explorerCommonScript + explorerEggScript + explorerLiveScript + `{{end}}`

// explorerShaderScript carries the two fragment shaders as JS strings: the
// line-art gyroid used in page headers and the index chain map.
const explorerShaderScript = `<script>window.BN_GFS="#version 300 es\nprecision highp float;\nuniform vec2 uR;uniform float uRot,uDraw,uFlash;\nout vec4 o;\nconst vec3 AC=vec3(.667,1.,0.);\nmat2 r2(float a){float c=cos(a),s=sin(a);return mat2(c,-s,s,c);}\nvec3 rot(vec3 p){p.xz*=r2(uRot);p.yz*=r2(.55);return p;}\nfloat sd(vec3 p){vec3 q=rot(p);float k=4.2;float g=sin(q.x*k)*cos(q.y*k)+sin(q.y*k)*cos(q.z*k)+sin(q.z*k)*cos(q.x*k);return max(length(p)-1.,abs(g)/k*.6-.035);}\nvec3 nrm(vec3 p){vec2 e=vec2(.002,0);return normalize(vec3(sd(p+e.xyy)-sd(p-e.xyy),sd(p+e.yxy)-sd(p-e.yxy),sd(p+e.yyx)-sd(p-e.yyx)));}\nvoid main(){vec2 uv=(gl_FragCoord.xy-.5*uR)/min(uR.x,uR.y)*2.3;\nvec3 ro=vec3(0,0,3.2),rd=normalize(vec3(uv,-3.2));float b=dot(ro,rd),c=dot(ro,ro)-1.02,h=b*b-c;\nif(h<0.){o=vec4(0);return;}\nfloat t=-b-sqrt(h),te=-b+sqrt(h);bool hit=false;for(int i=0;i<56;i++){float d=sd(ro+rd*t);if(d<.0015){hit=true;break;}t+=d*.9;if(t>te)break;}\nif(!hit){o=vec4(0);return;}\nvec3 p=ro+rd*t,n=nrm(p);float nv=abs(dot(n,-rd));\nfloat rim=1.-smoothstep(.22,.42,nv);\nfloat yy=rot(p).y;float sl=yy*7.;float fw=max(fwidth(sl),1e-4);float stripe=1.-clamp(abs(fract(sl+.5)-.5)/(fw*1.1),0.,1.);\nfloat line=max(rim,stripe*.55*smoothstep(.1,.5,nv));\nfloat y=yy*.5+.5;float dr=uDraw*1.1;\nfloat shown=step(y,dr);float lead=(1.-smoothstep(0.,.03,abs(y-dr)))*step(uDraw,.999);\nvec3 col=AC*line*(.9+.5*uFlash)+vec3(.02);\nfloat a=shown*max(line,.0)+lead*rim;\ncol=col*shown+AC*lead*rim;\nfloat al=clamp(max(a,shown*.85),0.,1.);\no=vec4(col*al,al);}";window.BN_FSMAP="#version 300 es\nprecision highp float;\nuniform vec2 uR;uniform float uT,uDN,uRows,uCols,uShift,uDrop,uM0,uM1,uMix,uProg,uHov,uDpr,uSel,uClick;uniform sampler2D uD;\nout vec4 o;\nconst vec3 AC=vec3(.667,1.,0.);\nvec3 ramp(float v){vec3 c=mix(vec3(.075,.08,.075),vec3(.09,.22,0.),smoothstep(0.,.3,v));c=mix(c,AC,smoothstep(.3,.75,v));return mix(c,vec3(.93,1.,.78),smoothstep(.8,1.,v));}\nfloat pick(vec4 d,float m){return m<.5?d.r:m<1.5?d.g:d.b;}\nfloat val(float i){if(i<0.||i>=uDN)return 0.;vec4 d=texelFetch(uD,ivec2(int(i),0),0);return mix(pick(d,uM0),pick(d,uM1),uMix);}\nfloat back(float t){t=clamp(t,0.,1.);float c=1.70158;t-=1.;return 1.+(c+1.)*t*t*t+c*t*t;}\nvoid main(){vec2 fc=gl_FragCoord.xy;vec2 uv=fc/uR;uv.y=1.-uv.y;float rows=uRows,cols=uCols,tot=rows*cols;\nvec2 g=uv*vec2(cols,rows);float L=floor(g.x)*rows+g.y;float sh=1.-uShift;float Ls=L<uDN+1.?L-sh:L;\nfloat idx=floor(Ls);float fy=fract(Ls),fx=fract(g.x);\nvec2 cs=uR/vec2(cols,rows);vec2 q=(vec2(fx,fy)-.5)*cs;vec2 hs=cs*.5-1.2*uDpr;\nif(idx==uDN-1.&&uDrop<1.)hs*=max(back(uDrop),0.);\nvec2 bd=abs(q)-hs;float sd=length(max(bd,0.))+min(max(bd.x,bd.y),0.);float a=clamp(.5-sd,0.,1.);\nvec3 col=vec3(0.);\nif(idx<uDN&&idx>=0.){float v=val(idx);vec3 c=ramp(v);if(idx==uHov)c=mix(c,vec3(1.),.35);\nfloat nw=step(uDN-1.,idx);c+=AC*nw*max(0.,1.-uDrop*1.2)*1.4+AC*nw*(.1+.08*sin(uT*3.));\nif(uSel>=0.){vec2 sc=vec2(floor(uSel/rows),mod(uSel,rows));vec2 cc=vec2(floor(idx/rows),mod(idx,rows));float d=length(cc-sc);float w=exp(-pow(d-uClick*30.,2.)*.35)*max(0.,1.-uClick*.8);c+=AC*w*.7;\nif(idx!=uSel)c*=mix(1.,.55,smoothstep(0.,.4,uClick));}\ncol+=c*a;\nif(idx==uSel){float ring=clamp(1.6*uDpr-abs(sd+1.2*uDpr),0.,1.);col=mix(col,vec3(1.),ring*(.75+.25*sin(uT*4.)));}}\nelse if(idx==uDN){float pulse=.55+.45*sin(uT*3.2);float ring=clamp(1.5*uDpr-abs(sd+1.5*uDpr),0.,1.);float fill=step(fy,uProg)*clamp(-sd-2.5*uDpr,0.,1.);\ncol+=AC*ring*pulse+AC*.35*fill*(.6+.4*pulse);if(idx==uHov)col+=AC*.15*a;}\nelse if(idx==tot-1.){float r=length(q);float ring=clamp(1.4*uDpr-abs(r-min(cs.x,cs.y)*.36),0.,1.);col+=vec3(1.)*ring+vec3(1.,1.,.9)*exp(-r/(min(cs.x,cs.y)*.18))*.6*(.75+.25*sin(uT*1.6))+vec3(.6,.8,.3)*exp(-r/(min(cs.x,cs.y)*.6))*.2;}\nelse if(idx>uDN&&idx<tot-1.){float r=length(q);col+=vec3(.16)*clamp(1.2*uDpr-r,0.,1.);}\no=vec4(max(col,0.),1.);}";</script>
`

// explorerCommonScript runs on every page: helpers, tooltips, copy buttons,
// the advanced toggle, the decode transition and the gyroid renderer.
const explorerCommonScript = `<script>
(function(){
var BN=window.BN={};
var $=BN.$=function(s){return document.querySelector(s);};
var $$=BN.$$=function(s){return Array.prototype.slice.call(document.querySelectorAll(s));};
BN.now=function(){return Date.now()/1000;};
var rm=BN.rm=!!(window.matchMedia&&matchMedia('(prefers-reduced-motion: reduce)').matches);
BN.ago=function(s){s=Math.max(0,Math.floor(s));if(s<60)return s+'s';if(s<3600)return Math.floor(s/60)+'m '+(s%60)+'s';if(s<86400)return Math.floor(s/3600)+'h '+Math.floor(s%3600/60)+'m';return Math.floor(s/86400)+'d '+Math.floor(s%86400/3600)+'h';};
BN.dur=function(s){return s<60?Math.round(s)+' s':s<3600?Math.floor(s/60)+'m '+Math.round(s%60)+'s':Math.floor(s/3600)+'h '+Math.round(s%3600/60)+'m';};
BN.short=function(h){return h.slice(0,8)+'…'+h.slice(-4);};
BN.bnt=function(v,d){return (v/1e8).toLocaleString('en-US',{maximumFractionDigits:d===undefined?4:d});};
BN.fmt=function(n){var a=Math.abs(n);return a>=1e9?(n/1e9).toFixed(2)+'G':a>=1e6?(n/1e6).toFixed(2)+'M':a>=1e3?(n/1e3).toFixed(1)+'k':String(Math.round(n*10)/10);};
BN.dt=function(t){return new Date(t*1000).toISOString().slice(0,16).replace('T',' ');};
BN.esc=function(s){return String(s).replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});};

var tip=null;
BN.tip=function(e,a,b){if(!tip){tip=document.createElement('div');tip.className='tip';document.body.appendChild(tip);}tip.innerHTML='<div class="a">'+a+'</div><div class="b">'+(b||'')+'</div>';tip.style.display='block';tip.style.left=Math.min(e.clientX+18,innerWidth-tip.offsetWidth-10)+'px';tip.style.top=Math.max(8,e.clientY-62)+'px';};
BN.untip=function(){if(tip)tip.style.display='none';};

function tick(){var t=BN.now();$$('[data-ts]').forEach(function(e){var s=BN.ago(t-(+e.getAttribute('data-ts')));if(e.getAttribute('data-ago')!==null)s+=' ago';if(e.textContent!==s)e.textContent=s;});}
BN.tick=tick;setInterval(tick,1000);

document.addEventListener('click',function(e){
var b=e.target.closest&&e.target.closest('.cp');
if(b){var v=b.getAttribute('data-cp');var ok=function(){b.textContent='copied';b.classList.add('done');setTimeout(function(){b.textContent='copy';b.classList.remove('done');},1100);};
if(navigator.clipboard&&navigator.clipboard.writeText)navigator.clipboard.writeText(v).then(ok,function(){});return;}
if(e.target.closest&&e.target.closest('a,button,input'))return;
var tr=e.target.closest&&e.target.closest('tr[data-go]');if(tr)location.href=tr.getAttribute('data-go');});
document.addEventListener('keydown',function(e){if(e.key!=='/'||e.metaKey||e.ctrlKey)return;var a=document.activeElement;if(a&&(a.tagName==='INPUT'||a.tagName==='TEXTAREA'))return;var q=$('.q input');if(q&&q.offsetParent){e.preventDefault();q.focus();}});

function setAdv(on){document.body.classList.toggle('adv',on);try{localStorage.setItem('bnadv',on?'1':'0');}catch(x){}}
try{if(localStorage.getItem('bnadv')==='1')document.body.classList.add('adv');}catch(x){}
document.addEventListener('click',function(e){if(e.target.id==='advb')setAdv(!document.body.classList.contains('adv'));});
document.addEventListener('keydown',function(e){var a=document.activeElement;if(e.key==='a'&&!e.metaKey&&!e.ctrlKey&&!(a&&(a.tagName==='INPUT'||a.tagName==='TEXTAREA')))setAdv(!document.body.classList.contains('adv'));});

var HEXC='0123456789abcdef';
BN.decode=function(root){if(!root||rm)return;var L=[];var w=document.createTreeWalker(root,NodeFilter.SHOW_TEXT,{acceptNode:function(n){return n.nodeValue.trim()&&!(n.parentNode.closest&&n.parentNode.closest('button,.navbtn,.tabs,script,style,input'))?1:3;}});var n;while((n=w.nextNode()))L.push([n,n.nodeValue]);
var t0=performance.now(),dur=520,id=root._dec=(root._dec||0)+1;
function f(){if(root._dec!==id)return;var k=Math.min(1,(performance.now()-t0)/dur);L.forEach(function(p,li){var s=p[1],o='';for(var j=0;j<s.length;j++){var c=s[j];var lock=(j/s.length)*.7+Math.min(li,20)*.012;o+=(/\s/.test(c)||k>=lock+.3)?c:HEXC[Math.random()*16|0];}p[0].nodeValue=o;});
if(k<1)requestAnimationFrame(f);else L.forEach(function(p){p[0].nodeValue=p[1];});}f();};

BN.lastInput=performance.now();addEventListener('pointermove',function(){BN.lastInput=performance.now();},{passive:true});
BN.VS='#version 300 es\nin vec2 a;void main(){gl_Position=vec4(a,0,1);}';
var GFS=window.BN_GFS;
var gys=[];
BN.gyroid=function(cv,drawDur){if(!GFS)return null;var g=cv.getContext('webgl2',{antialias:false,alpha:true,premultipliedAlpha:true,powerPreference:'low-power'});if(!g)return null;
function mk(t,src){var x=g.createShader(t);g.shaderSource(x,src);g.compileShader(x);return x;}
var p=g.createProgram();g.attachShader(p,mk(g.VERTEX_SHADER,BN.VS));g.attachShader(p,mk(g.FRAGMENT_SHADER,GFS));g.bindAttribLocation(p,0,'a');g.linkProgram(p);if(!g.getProgramParameter(p,g.LINK_STATUS))return null;g.useProgram(p);
var b=g.createBuffer();g.bindBuffer(g.ARRAY_BUFFER,b);g.bufferData(g.ARRAY_BUFFER,new Float32Array([-1,-1,3,-1,-1,3]),g.STATIC_DRAW);g.enableVertexAttribArray(0);g.vertexAttribPointer(0,2,g.FLOAT,false,0,0);
g.enable(g.BLEND);g.blendFunc(g.ONE,g.ONE_MINUS_SRC_ALPHA);
var U={R:g.getUniformLocation(p,'uR'),rot:g.getUniformLocation(p,'uRot'),draw:g.getUniformLocation(p,'uDraw'),fl:g.getUniformLocation(p,'uFlash')};
var st={drawAt:performance.now(),drawDur:drawDur||1100,spinAt:-1e9,last:0,vis:true,W:0,H:0};
if(window.IntersectionObserver)new IntersectionObserver(function(es){st.vis=es[0].isIntersecting;}).observe(cv);
st.redraw=function(){st.drawAt=performance.now();};st.spin=function(){st.spinAt=performance.now();};
st.frame=function(ts){if(!st.vis||document.hidden||cv.offsetParent===null)return;var drawing=ts-st.drawAt<st.drawDur+50,spinning=ts-st.spinAt<1400;var fps=drawing||spinning?60:(ts-BN.lastInput>6000?8:24);
if(ts-st.last<1000/fps-2)return;st.last=ts;var r=cv.getBoundingClientRect(),w=Math.round(r.width*2),h=Math.round(r.height*2);if(w!==st.W||h!==st.H){st.W=cv.width=w;st.H=cv.height=h;g.viewport(0,0,w,h);}
var sp=Math.min(1,(ts-st.spinAt)/1400);var spin=(1-Math.pow(1-sp,3))*Math.PI*2;
g.clearColor(0,0,0,0);g.clear(g.COLOR_BUFFER_BIT);g.uniform2f(U.R,w,h);g.uniform1f(U.rot,(rm?0:ts/1000*.22)+spin);g.uniform1f(U.draw,Math.min(1,(ts-st.drawAt)/st.drawDur));g.uniform1f(U.fl,sp<1?1-sp:0);g.drawArrays(g.TRIANGLES,0,3);};
gys.push(st);return st;};
function loop(ts){for(var i=0;i<gys.length;i++)gys[i].frame(ts);requestAnimationFrame(loop);}requestAnimationFrame(loop);
BN.spinAll=function(){gys.forEach(function(g){g.spin();});};
$$('canvas[data-gyroid]').forEach(function(c){var st=BN.gyroid(c,+c.getAttribute('data-gyroid')||1100);if(c.id)BN[c.id]=st;});
var sh=document.getElementById('stat-height');if(sh&&/^\d+$/.test(sh.textContent.trim()))sh.textContent=Number(sh.textContent).toLocaleString('en-US');
var sd=document.getElementById('stat-difficulty');if(sd&&/^\d+$/.test(sd.textContent.trim()))sd.textContent=BN.fmt(Number(sd.textContent));
$$('[data-decode]').forEach(function(e){BN.decode(e);});
tick();
})();
</script>
`

// explorerLiveScript applies the /events stream to the open page: tip height,
// next difficulty, the block list, confirmations and the chain map.
const explorerLiveScript = `<script>
(function(){
if(!window.EventSource)return;
var es=new EventSource('/events');
var BN=window.BN||{};
function txt(id,v){var el=document.getElementById(id);if(el)el.textContent=v;}
function confs(h){var els=document.querySelectorAll('[data-conf-height]');for(var i=0;i<els.length;i++){var at=parseInt(els[i].getAttribute('data-conf-height'),10);if(!isNaN(at))els[i].textContent=(h-at+1);}}
function addRow(d){var tb=document.querySelector('table.recent-blocks tbody');if(!tb)return;
for(var i=0;i<tb.rows.length;i++){if(tb.rows[i].getAttribute('data-h')===String(d.height))return;}
var r=tb.insertRow(0);r.className='new';r.setAttribute('data-h',d.height);r.setAttribute('data-go','/block/'+d.height);
r.innerHTML='<td class="h"><a href="/block/'+d.height+'">'+d.height+'</a></td><td><span data-ts="'+d.timestamp+'"></span></td><td class="r">'+d.tx_count+'</td><td class="ad m">'+String(d.hash).slice(0,8)+'…'+String(d.hash).slice(-4)+'</td>';
while(tb.rows.length>10)tb.deleteRow(tb.rows.length-1);}
es.addEventListener('connected',function(ev){var d;try{d=JSON.parse(ev.data);}catch(e){return;}
txt('stat-height',Number(d.height).toLocaleString('en-US'));txt('stat-difficulty',BN.fmt?BN.fmt(d.next_difficulty):d.next_difficulty);confs(d.height);});
es.addEventListener('new_block',function(ev){var d;try{d=JSON.parse(ev.data);}catch(e){return;}
txt('stat-height',Number(d.height).toLocaleString('en-US'));txt('stat-difficulty',BN.fmt?BN.fmt(d.next_difficulty):d.next_difficulty);
var tip=document.getElementById('tip-ts');if(tip)tip.setAttribute('data-ts',d.timestamp);
addRow(d);confs(d.height);if(BN.tick)BN.tick();if(BN.spinAll)BN.spinAll();if(BN.onBlock)BN.onBlock(d);});
})();
</script>
`

// explorerMapScript draws the index chain map: one flat cell per block for the
// last 1,000 blocks, the next block cell and the faraday marker. It redraws
// only on hover, clicks, metric changes and new blocks.
const explorerMapScript = `<script>
(function(){
var BN=window.BN,$=BN.$,$$=BN.$$,cfg=window.BN_MAP;if(!cfg||!cfg.rows||!cfg.rows.length)return;
var B=cfg.rows.map(function(r){return{h:r[0],t:r[1],tx:r[2],d:r[3],fee:r[4]};});
B.forEach(function(b,i){b.bt=i?b.t-B[i-1].t:300;});
var last=function(){return B[B.length-1];};
var NORM;
function norms(){var mxTx=1,dmin=Infinity,dmax=0;B.forEach(function(b){if(b.tx>mxTx)mxTx=b.tx;if(b.d<dmin)dmin=b.d;if(b.d>dmax)dmax=b.d;});
NORM={f:[function(b){return Math.sqrt(Math.max(0,b.tx-1)/Math.max(1,mxTx-1));},function(b){return Math.min(1,Math.max(0,.5+Math.log2(Math.max(1,b.bt)/300)/6));},function(b){return Math.log(b.d/dmin)/Math.log(Math.max(dmax/dmin,1.0001));}],
lo:['1 tx','fast',BN.fmt(dmin)],hi:[mxTx+' tx','slow · 5 min in the middle',BN.fmt(dmax)]};}
norms();
var cv=$('#heat');var gl=cv.getContext('webgl2',{antialias:false,alpha:false,powerPreference:'low-power'});if(!gl){cv.style.display='none';return;}
function sh(t,s){var x=gl.createShader(t);gl.shaderSource(x,s);gl.compileShader(x);return x;}
var P=gl.createProgram();gl.attachShader(P,sh(gl.VERTEX_SHADER,BN.VS));gl.attachShader(P,sh(gl.FRAGMENT_SHADER,window.BN_FSMAP));gl.bindAttribLocation(P,0,'a');gl.linkProgram(P);if(!gl.getProgramParameter(P,gl.LINK_STATUS)){cv.style.display='none';return;}gl.useProgram(P);
var vb=gl.createBuffer();gl.bindBuffer(gl.ARRAY_BUFFER,vb);gl.bufferData(gl.ARRAY_BUFFER,new Float32Array([-1,-1,3,-1,-1,3]),gl.STATIC_DRAW);gl.enableVertexAttribArray(0);gl.vertexAttribPointer(0,2,gl.FLOAT,false,0,0);
var U={};['uR','uT','uDN','uRows','uCols','uShift','uDrop','uM0','uM1','uMix','uProg','uHov','uDpr','uSel','uClick','uD'].forEach(function(n){U[n]=gl.getUniformLocation(P,n);});
var tex=gl.createTexture();var dirty=true;
function upload(){var a=new Uint8Array(B.length*4);B.forEach(function(b,i){for(var k=0;k<3;k++)a[i*4+k]=Math.round(255*Math.min(1,Math.max(0,NORM.f[k](b))));a[i*4+3]=255;});
gl.bindTexture(gl.TEXTURE_2D,tex);gl.texImage2D(gl.TEXTURE_2D,0,gl.RGBA8,B.length,1,0,gl.RGBA,gl.UNSIGNED_BYTE,a);gl.texParameteri(gl.TEXTURE_2D,gl.TEXTURE_MIN_FILTER,gl.NEAREST);gl.texParameteri(gl.TEXTURE_2D,gl.TEXTURE_MAG_FILTER,gl.NEAREST);dirty=true;}
var W=0,H=0,dpr=1,rows=10,cols=100,lastDraw=0,hov=-1,selH=null,clickAt=-1e9,m0=0,m1=0,mixStart=-1e9,shiftStart=-1e9,dropStart=-1e9,animating=false,vis=true;
function layout(){var r=cv.getBoundingClientRect();dpr=Math.min(devicePixelRatio||1,2);var w=Math.round(r.width*dpr),h=Math.round(r.height*dpr);if(w!==W||h!==H){W=cv.width=w;H=cv.height=h;gl.viewport(0,0,W,H);dirty=true;}
var slots=B.length+2;rows=Math.max(6,Math.floor(Math.sqrt(slots*r.height/r.width)+.5));cols=Math.ceil(slots/rows);if(cols*rows-1<B.length+1)cols++;}
function slotAt(x,y){var r=cv.getBoundingClientRect();var u=x/r.width,v=y/r.height;if(u<0||u>=1||v<0||v>=1)return -1;return Math.floor(u*cols)*rows+Math.floor(v*rows);}
function ease(t){return 1-Math.pow(1-Math.min(1,Math.max(0,t)),3);}
function draw(ts){var T=BN.rm?10:ts/1000;var shift=Math.min(1,(ts-shiftStart)/650),drop=Math.min(1,(ts-dropStart)/900),mixT=Math.min(1,(ts-mixStart)/500);if(mixT>=1)m0=m1;
var prog=Math.min(1,Math.max(0,(BN.now()-last().t)/300));var clk=(ts-clickAt)/1000;var si=-1;if(selH!==null)for(var i=B.length-1;i>=0;i--)if(B[i].h===selH){si=i;break;}
gl.useProgram(P);gl.activeTexture(gl.TEXTURE0);gl.bindTexture(gl.TEXTURE_2D,tex);gl.uniform1i(U.uD,0);
gl.uniform2f(U.uR,W,H);gl.uniform1f(U.uT,T);gl.uniform1f(U.uDN,B.length);gl.uniform1f(U.uRows,rows);gl.uniform1f(U.uCols,cols);gl.uniform1f(U.uShift,ease(shift));gl.uniform1f(U.uDrop,drop);
gl.uniform1f(U.uM0,m0);gl.uniform1f(U.uM1,m1);gl.uniform1f(U.uMix,ease(mixT));gl.uniform1f(U.uProg,prog);gl.uniform1f(U.uHov,hov);gl.uniform1f(U.uDpr,dpr);gl.uniform1f(U.uSel,si);gl.uniform1f(U.uClick,Math.min(clk,9));
gl.drawArrays(gl.TRIANGLES,0,3);return shift<1||drop<1||mixT<1||clk<1.3;}
if(window.IntersectionObserver)new IntersectionObserver(function(es){vis=es[0].isIntersecting;dirty=true;}).observe(cv);
function loop(ts){if(vis&&!document.hidden){layout();var idle=ts-BN.lastInput>6000;var every=animating?0:(idle?250:66);if(dirty||ts-lastDraw>=every){lastDraw=ts;dirty=false;animating=draw(ts);}}requestAnimationFrame(loop);}

cv.addEventListener('pointermove',function(e){var r=cv.getBoundingClientRect();var s=slotAt(e.clientX-r.left,e.clientY-r.top);hov=s;dirty=true;
if(s<0){BN.untip();return;}
if(s<B.length){var b=B[s];var val=[b.tx+' tx',BN.dur(b.bt)+' block time','difficulty '+BN.fmt(b.d)][m1];BN.tip(e,'#'+b.h.toLocaleString('en-US')+' · <b>'+val+'</b>',BN.dt(b.t)+' UTC · '+BN.ago(BN.now()-b.t)+' ago · click for details');}
else if(s===B.length){var since=BN.now()-last().t;BN.tip(e,'next block · <b>#'+(last().h+1).toLocaleString('en-US')+'</b>',since<300?'expected in ~'+BN.dur(300-since):'due any moment');}
else if(s===cols*rows-1){BN.tip(e,'faraday upgrade · <b>block '+cfg.faraday.toLocaleString('en-US')+'</b>',Math.max(0,cfg.faraday-last().h).toLocaleString('en-US')+' blocks to go · planned ~'+cfg.upgradeDate);}
else BN.tip(e,'future · <b>#'+(last().h+s-B.length).toLocaleString('en-US')+'</b>','on the way to faraday');});
cv.addEventListener('pointerleave',function(){hov=-1;dirty=true;BN.untip();});
var downAt=null;
cv.addEventListener('pointerdown',function(e){downAt=[e.clientX,e.clientY];});
cv.addEventListener('pointerup',function(e){if(!downAt)return;var moved=Math.hypot(e.clientX-downAt[0],e.clientY-downAt[1])>6;downAt=null;if(moved)return;var r=cv.getBoundingClientRect();var s=slotAt(e.clientX-r.left,e.clientY-r.top);
if(s>=0&&s<B.length){var h=B[s].h;if(selH===h){close();}else select(h);}});

$('#metric').addEventListener('click',function(e){var m=e.target.getAttribute('data-m');if(m===null)return;m0=m1;m1=+m;mixStart=performance.now();dirty=true;
$$('#metric button').forEach(function(b){b.classList.toggle('on',b===e.target);});$('#lo').textContent=NORM.lo[m1];$('#hi').textContent=NORM.hi[m1];});
$('#lo').textContent=NORM.lo[0];$('#hi').textContent=NORM.hi[0];

function dots(n,cls){var s='';for(var i=0;i<Math.min(n,13);i++)s+='<i'+(cls?' class="'+cls+'"':'')+'></i>';return '<span class="dots">'+s+'</span>';}
var reqId=0;
function select(h){selH=h;clickAt=performance.now();dirty=true;var id=++reqId;
fetch('/blockdata/'+h).then(function(r){return r.json();}).then(function(d){if(id!==reqId||selH!==h)return;panel(d);}).catch(function(){});}
function close(){selH=null;dirty=true;$('#blk').classList.remove('on');}
function panel(d){var conf=d.confirmations;var bt=d.block_time||0;var pct=Math.min(100,bt/900*100);
var rows=d.txs.map(function(t){return '<tr data-go="/tx/'+t[0]+'"><td class="m"><a href="/tx/'+t[0]+'" style="color:inherit">'+BN.short(t[0])+'</a></td><td>'+(t[4]?'<span style="color:var(--ac)">coinbase</span>':'transfer')+'</td><td class="r">'+t[1]+' '+dots(t[1])+'</td><td class="r">'+t[2]+' '+dots(t[2],'o')+'</td><td class="r ad m">'+(t[4]?'':BN.bnt(t[3]))+'</td></tr>';}).join('');
$('#bp').innerHTML='<div class="bp"><div><div class="k">block</div><div class="bh"><span>#</span>'+d.height.toLocaleString('en-US')+'</div>'+
'<div class="row"><span class="pill g"><span data-conf-height="'+d.height+'">'+conf+'</span> confirmations</span><span class="pill">'+(conf>100?'final':'final after 100')+'</span><span class="pill">'+d.txs.length+' tx</span></div>'+
'<div class="tbar"><i style="width:'+pct+'%"></i><b style="left:33.3%"></b></div><div class="tlab"><span>block time '+BN.dur(bt)+'</span><span>target 5m</span><span>15m</span></div>'+
'<div class="kv2" style="margin-top:14px"><div class="kk">time</div><div class="vv">'+BN.dt(d.timestamp)+' UTC</div><div class="kk">age</div><div class="vv" data-ts="'+d.timestamp+'">'+BN.ago(BN.now()-d.timestamp)+'</div><div class="kk">difficulty</div><div class="vv">'+Number(d.difficulty).toLocaleString('en-US')+'</div><div class="kk">reward</div><div class="vv">'+BN.bnt(d.reward,4)+' BNT</div><div class="kk">fees</div><div class="vv">'+BN.bnt(d.fees)+' BNT</div><div class="kk ad">hash</div><div class="vv ad" style="color:#888">'+d.hash+'</div></div>'+
'<div class="bnav"><button id="bpv"'+(d.height>0?'':' disabled')+'>← '+(d.height-1)+'</button><button id="bnx"'+(d.height<last().h?'':' disabled')+'>'+(d.height+1)+' →</button><button id="bcl">close</button><a class="btn" href="/block/'+d.height+'" style="margin-left:auto">open block page →</a></div></div>'+
'<div><div class="k" style="margin-bottom:6px">transactions · amounts hidden</div><table class="tbl-click"><thead><tr><th>tx</th><th>type</th><th class="r">inputs</th><th class="r">outputs</th><th class="r ad">fee</th></tr></thead><tbody>'+rows+'</tbody></table></div></div>';
$('#blk').classList.add('on');$('#bpv').onclick=function(){select(d.height-1);};$('#bnx').onclick=function(){select(d.height+1);};$('#bcl').onclick=close;
BN.decode($('#bp'));if(BN.gyl)BN.gyl.redraw();}
addEventListener('keydown',function(e){if(selH===null)return;var a=document.activeElement;if(a&&a.tagName==='INPUT')return;if(e.key==='Escape')close();else if(e.key==='ArrowLeft'&&selH>0){e.preventDefault();select(selH-1);}else if(e.key==='ArrowRight'&&selH<last().h){e.preventDefault();select(selH+1);}});

function nextLine(){var b=last(),since=BN.now()-b.t,left=Math.ceil(300-since);var el=$('#nextb');if(el)el.innerHTML=since<300?'next block in <b>~'+(left>=60?Math.floor(left/60)+'m '+(left%60)+'s':left+'s')+'</b>':'next block <b>any moment</b>';
var f=$('#far-left');if(f)f.textContent=Math.max(0,cfg.faraday-b.h).toLocaleString('en-US');}
setInterval(nextLine,1000);nextLine();

BN.onBlock=function(d){if(B.length&&d.height<=last().h)return;var t=d.timestamp;B.push({h:d.height,t:t,tx:d.tx_count,d:d.block_difficulty||last().d,fee:d.fees||0,bt:t-last().t});if(B.length>cfg.rows.length)B.shift();
norms();upload();var tn=performance.now();shiftStart=tn;dropStart=tn;nextLine();};
upload();requestAnimationFrame(loop);
})();
</script>
`

// explorerBoardScript draws the stats page chart cards from the window points.
const explorerBoardScript = `<script>
(function(){
var BN=window.BN,cfg=window.BN_STATS,root=document.getElementById('cards');if(!cfg||!root)return;
var D=cfg.points||[];if(D.length<2){root.innerHTML='<div class="note">Not enough data yet.</div>';return;}
var AC='#aaff00';
function el(h){var d=document.createElement('div');d.innerHTML=h;return d.firstElementChild;}
function bytes(v){return v>=1024?(v/1024).toFixed(1)+' KB':Math.round(v)+' B';}
function sum(a){var s=0;for(var i=0;i<a.length;i++)s+=a[i];return s;}
function hdate(h){return new Date((cfg.gen+h*300)*1000).toISOString().slice(0,10);}
function mk(name,pts,f,o){var x=pts.map(function(p){return p[0];}),y=pts.map(function(p){return p[1];});var s={name:name,x:x,y:y,f:f,now:y[y.length-1],min:Math.min.apply(null,y),max:Math.max.apply(null,y),avg:sum(y)/y.length};for(var k in o||{})s[k]=o[k];return s;}
var dd=[],hr=[];
for(var i=1;i<D.length;i++){if(D[i-1].d>0&&D[i].h>100)dd.push([D[i].h,(D[i].d-D[i-1].d)/D[i-1].d*100]);
if(D[i].bt>0){var sd=0,st=0;for(var j=Math.max(1,i-19);j<=i;j++)if(D[j].bt>0){sd+=D[j].d;st+=D[j].bt;}if(st>0)hr.push([D[i].h,sd/st]);}}
var SER=[
mk('difficulty',D.map(function(p){return[p.h,p.d];}),BN.fmt),
mk('difficulty delta',dd,function(v){return(v>=0?'+':'')+v.toFixed(1)+'%';},{signed:true}),
mk('estimated hashrate',hr,function(v){return BN.fmt(v)+'H/s';}),
mk('block time',D.filter(function(p){return p.h>1&&p.bt>0;}).map(function(p){return[p.h,p.bt];}),BN.dur,{ref:300}),
mk('transactions per block',D.map(function(p){return[p.h,p.tx];}),function(v){return Math.round(v*10)/10+' tx';}),
mk('block size',D.map(function(p){return[p.h,p.s];}),bytes)];
var ln=D.map(function(p){return Math.log10(p.n+1);}),nMin=Math.min.apply(null,ln),nMax=Math.max.apply(null,ln),NB=48,NH=[];for(i=0;i<NB;i++)NH.push(0);ln.forEach(function(v){NH[Math.min(NB-1,Math.floor((v-nMin)/((nMax-nMin)||1)*NB))]++;});
var edge=function(i){return Math.pow(10,nMin+(nMax-nMin)*i/NB);},peak=NH.indexOf(Math.max.apply(null,NH));
var IR=72325093035,TE=2e8,BPM=8640,MTT=48,DR=.75;function rew(m){return m>=MTT?TE:(IR-TE)*Math.exp(-DR*m/12)+TE;}
var months=[];for(i=0;i<52;i++)months.push(rew(i)/1e8);var mNow=cfg.height/BPM;var emf=function(v){return v.toFixed(2)+' BNT';};
var REG=[];addEventListener('resize',function(){REG.forEach(function(f){f();});});
function fit(c){var r=c.getBoundingClientRect(),d=Math.min(devicePixelRatio||1,2);c.width=Math.max(1,Math.round(r.width*d));c.height=Math.max(1,Math.round(r.height*d));var x=c.getContext('2d');x.setTransform(d,0,0,d,0,0);x.clearRect(0,0,r.width,r.height);return{x:x,W:r.width,H:r.height};}
function spark(parent,ys,h,o){var w=el('<div class="plot" style="height:'+h+'px"><canvas></canvas><canvas></canvas></div>');parent.appendChild(w);var cs=w.querySelectorAll('canvas'),a=cs[0],b=cs[1];
function d(){var F=fit(a),x=F.x,W=F.W,H=F.H;var lo=o.lo!=null?o.lo:Math.min.apply(null,ys),hi=Math.max.apply(null,ys);var X=function(i){return i/(ys.length-1)*W;},Y=function(v){return H-2-(v-lo)/((hi-lo)||1)*(H-4);};
if(o.bars){var bw=W/ys.length;ys.forEach(function(v,i){x.fillStyle=o.hl===i?'#fff':'rgba(170,255,0,.7)';var yy=Y(v);x.fillRect(i*bw+.5,yy,Math.max(1,bw-1),H-yy);});return;}
if(o.zero!=null){x.strokeStyle='#333';x.beginPath();x.moveTo(0,Y(o.zero)+.5);x.lineTo(W,Y(o.zero)+.5);x.stroke();}
if(o.ref!=null){x.strokeStyle='#666';x.setLineDash([3,3]);x.beginPath();x.moveTo(0,Y(o.ref)+.5);x.lineTo(W,Y(o.ref)+.5);x.stroke();x.setLineDash([]);}
x.beginPath();ys.forEach(function(v,i){if(i)x.lineTo(X(i),Y(v));else x.moveTo(X(i),Y(v));});x.strokeStyle=AC;x.lineWidth=1.2;x.stroke();x.lineTo(W,H);x.lineTo(0,H);x.closePath();x.fillStyle='rgba(170,255,0,.06)';x.fill();
x.fillStyle='#fff';x.fillRect(X(ys.length-1)-2,Y(ys[ys.length-1])-2,4,4);if(o.mark!=null){x.strokeStyle='#fff';x.beginPath();x.moveTo(o.mark*W+.5,0);x.lineTo(o.mark*W+.5,H);x.stroke();}}
d();REG.push(d);b.style.cursor='crosshair';
b.onpointermove=function(e){var r=b.getBoundingClientRect();o.tip(Math.min(ys.length-1,Math.max(0,Math.round((e.clientX-r.left)/r.width*(ys.length-1)))),e);};b.onpointerleave=BN.untip;}
function foot(c,a,b){c.appendChild(el('<div class="note" style="display:flex;justify-content:space-between;margin-top:6px"><span>'+a+'</span><span>'+b+'</span></div>'));}
SER.forEach(function(s){if(s.y.length<2){root.appendChild(el('<div class="card"><div class="k">'+s.name+'</div><div class="note" style="margin-top:10px">Not enough data yet.</div></div>'));return;}var c=el('<div class="card"><div class="k">'+s.name+'</div><div class="v" style="margin:6px 0 2px">'+s.f(s.now)+'</div><div class="note">now · avg '+s.f(s.avg)+'</div></div>');root.appendChild(c);
spark(c,s.y,70,{lo:s.signed?s.min:0,zero:s.signed?0:null,ref:s.ref,tip:function(i,e){BN.tip(e,s.name+' · <b>'+s.f(s.y[i])+'</b>','block #'+s.x[i].toLocaleString('en-US')+' · ~'+hdate(s.x[i]));}});foot(c,'min '+s.f(s.min),'max '+s.f(s.max));});
var nc=el('<div class="card"><div class="k">nonce distribution</div><div class="v" style="margin:6px 0 2px">'+BN.fmt(edge(peak))+'</div><div class="note">most common winning nonce · log scale</div></div>');root.appendChild(nc);
spark(nc,NH,70,{bars:true,lo:0,hl:peak,tip:function(i,e){BN.tip(e,'nonce '+BN.fmt(edge(i))+' to '+BN.fmt(edge(i+1))+' · <b>'+NH[i]+' blocks</b>','log scale');}});foot(nc,BN.fmt(edge(0)),BN.fmt(edge(NB)));
var ec=el('<div class="card"><div class="k">emission schedule</div><div class="v" style="margin:6px 0 2px">'+emf(rew(Math.floor(mNow))/1e8)+'</div><div class="note">per block now · month '+(Math.floor(mNow)+1)+' of 48</div></div>');root.appendChild(ec);
spark(ec,months,70,{lo:0,mark:mNow/(months.length-1),tip:function(i,e){BN.tip(e,'month '+(i+1)+' · <b>'+emf(months[i])+' per block</b>','~'+hdate(i*BPM).slice(0,7)+(i===Math.floor(mNow)?' · now':''));}});foot(ec,emf(months[0]),'tail 2 BNT');
})();
</script>
`

const explorerProxyFixScript = `<script>
(function(){
var aa=document.querySelectorAll('.topnav a,div a,h1 span');
for(var i=0;i<aa.length;i++){var t=aa[i].textContent.trim();
if(t==='mainnet'||t==='testnet'||t==='TESTNET'){aa[i].remove();}}
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
var els=document.querySelectorAll('.nav a,.k,.v,.big,.kk,.vv>span,td,th,footer a,h3,.btn,.pill');
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
<title>blocknet explorer</title>
<meta name="description" content="Live blocks, transactions and network stats for blocknet, zero-knowledge money.">
<meta property="og:title" content="blocknet explorer">
<meta property="og:description" content="Live blocks, transactions and network stats for blocknet.">
<meta property="og:url" content="https://explorer.blocknetcrypto.com">
{{template "head" .}}
</head>
<body>
{{template "top" .}}
<main class="wrap">
<div class="hero">
<div><div class="live">{{if .IsTestnet}}testnet{{else}}mainnet{{end}} · live</div><div class="hrow"><div class="big" id="stat-height">{{.Height}}</div><canvas id="gy" data-gyroid="1400" aria-hidden="true"></canvas></div><div class="nb" id="nextb">next block</div></div>
<div class="grid st">
<div class="card"><div class="k">hashrate</div><div class="v">{{.Hashrate}}<small>H/s</small></div></div>
<div class="card"><div class="k">avg block</div><div class="v">{{.AvgBlockTime}}<small>s</small></div></div>
<div class="card"><div class="k">emitted</div><div class="v">{{.PctEmitted}}<small>%</small></div></div>
<div class="card ad"><div class="k">next difficulty</div><div class="v" id="stat-difficulty">{{.Difficulty}}</div></div>
<div class="card ad"><div class="k">reward</div><div class="v">{{.Reward}}<small>BNT</small></div></div>
<div class="card ad"><div class="k">peers</div><div class="v">{{.Peers}}</div></div>
</div>
</div>
<div class="card pad">
<h3 style="flex-wrap:wrap">chain map <em>last 1,000 blocks · one cell per block · click a cell for the block</em><div class="chips" id="metric"><button class="on" data-m="0">transactions</button><button data-m="1">block time</button><button data-m="2">difficulty</button></div></h3>
<canvas id="heat"></canvas>
<div style="display:flex;justify-content:space-between;margin-top:12px;gap:14px;flex-wrap:wrap">
<div class="legend"><span id="lo"></span><i></i><span id="hi"></span><span style="margin-left:10px" class="mp"></span>next block<span style="margin-left:10px" class="fd"></span>faraday</div>
<div class="far">faraday upgrade in <b id="far-left">{{.FaradayLeft}}</b> blocks · planned ~{{.UpgradeDate}}</div>
</div>
<div id="blk"><div style="position:relative"><canvas id="gyl" data-gyroid="650" aria-hidden="true"></canvas><div id="bp"></div></div></div>
</div>
{{if .MempoolTxs}}<div class="card pad mt"><h3>mempool <em>{{.MempoolTotal}} waiting</em></h3><table class="tbl-click"><thead><tr><th>tx</th><th class="r">fee</th><th class="r ad">fee/B</th><th class="r ad">size</th><th class="r">age</th></tr></thead><tbody>{{range .MempoolTxs}}<tr data-go="/tx/{{.Hash}}"><td class="m"><a href="/tx/{{.Hash}}" style="color:inherit">{{slice .Hash 0 8}}…{{slice .Hash 60}}</a></td><td class="r">{{printf "%.8f" .Fee}}</td><td class="r ad">{{.FeeRate}}</td><td class="r ad">{{.Size}} B</td><td class="r"><span data-ts="{{.Ts}}">{{.Ago}}</span></td></tr>{{end}}</tbody></table></div>{{end}}
<div class="two">
<div class="card pad"><h3>latest blocks</h3><table class="recent-blocks"><thead><tr><th>height</th><th>age</th><th class="r">txs</th><th class="ad">hash</th></tr></thead><tbody>{{range .Blocks}}<tr data-h="{{.Height}}" data-go="/block/{{.Height}}"><td class="h"><a href="/block/{{.Height}}">{{.Height}}</a></td><td><span data-ts="{{.Ts}}">{{.Ago}}</span></td><td class="r">{{.TxCount}}</td><td class="ad m">{{slice .Hash 0 8}}…{{slice .Hash 60}}</td></tr>{{end}}</tbody></table></div>
<div class="card pad"><h3>latest transactions <em>amounts hidden</em></h3><table class="tbl-click"><thead><tr><th>tx</th><th>block</th><th class="r">in / out</th><th class="r ad">fee</th></tr></thead><tbody>{{range .LatestTxs}}<tr data-go="/tx/{{.Hash}}"><td class="m"><a href="/tx/{{.Hash}}" style="color:inherit">{{slice .Hash 0 8}}…{{slice .Hash 60}}</a></td><td class="h">{{.BlockHeight}}</td><td class="r">{{.Inputs}} / {{.Outputs}}</td><td class="r ad m">{{printf "%.4f" .Fee}}</td></tr>{{else}}<tr><td class="m" colspan="4">No transfers in recent blocks.</td></tr>{{end}}</tbody></table></div>
</div>
</main>
<script>window.BN_MAP={rows:{{.MapJSON}},faraday:{{.FaradayHeight}},upgradeDate:{{.UpgradeDate}}};</script>
{{template "foot" .}}
` + explorerMapScript + `
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
<main class="wrap" data-decode>
<div class="crumb"><a href="/">blocks</a> / block</div>
<div class="vh"><div><div class="ttl"><div class="big"><span>#</span>{{.Height}}</div><div class="slot"><canvas data-gyroid="900" aria-hidden="true"></canvas></div></div>
<div class="pills"><span class="pill g"><span id="tx-confirmations" data-conf-height="{{.Height}}">{{.Confirmations}}</span> confirmations</span><span class="pill">{{if .Final}}final{{else}}final after 100{{end}}</span><span class="pill">{{.TxCount}} tx</span><span class="pill"><span data-ts="{{.Ts}}" data-ago></span></span></div></div>
<div class="navbtn"><a href="/block/{{.PrevHeight}}" class="{{if not .HasPrev}}off{{end}}">← {{.PrevHeight}}</a><a href="/block/{{.NextHeight}}" class="{{if not .HasNext}}off{{end}}">{{.NextHeight}} →</a></div></div>
<div class="vgrid">
<div class="card pad"><div class="kv3">
<div class="kk">hash</div><div class="vv m"><span{{if .IsGenesis}} title="{{.GenesisMsg}}" style="cursor:help"{{end}}>{{.Hash}}</span><button class="cp" data-cp="{{.Hash}}" type="button">copy</button></div>
{{if .HasPrev}}<div class="kk">previous</div><div class="vv m"><span><a href="/block/{{.PrevHash}}">{{.PrevHash}}</a></span></div>{{end}}
<div class="kk">merkle root</div><div class="vv m"><span>{{.MerkleRoot}}</span></div>
<div class="kk">time</div><div class="vv"><span>{{.Time}}</span></div>
{{if .HasPrev}}<div class="kk">block time</div><div class="vv" style="display:block"><span data-dur="{{.BlockTime}}">{{.BlockTime}} s</span><div class="tbar" style="margin-top:8px"><i style="width:{{.BlockTimePct}}%"></i><b style="left:33.3%"></b></div><div class="tlab"><span></span><span>target 5m</span><span>15m</span></div></div>{{end}}
<div class="kk">difficulty</div><div class="vv"><span>{{.Difficulty}}</span></div>
<div class="kk">nonce</div><div class="vv"><span>{{.Nonce}}</span></div>
<div class="kk">reward</div><div class="vv"><span>{{printf "%.8f" .Reward}} <span style="color:var(--mu)">BNT</span></span></div>
<div class="kk">fees</div><div class="vv"><span>{{printf "%.8f" .Fees}} <span style="color:var(--mu)">BNT</span></span></div>
<div class="kk">inputs / outputs</div><div class="vv"><span>{{.InputsTotal}} / {{.OutputsTotal}}</span></div>
</div></div>
<div class="card pad"><h3>transactions <em>{{.TxCount}} · amounts hidden</em></h3><div style="margin-top:8px;overflow-x:auto"><table class="tbl-click"><thead><tr><th>tx</th><th>type</th><th class="r">inputs</th><th class="r">outputs</th><th class="r ad">fee</th></tr></thead><tbody>{{range .Txs}}<tr data-go="/tx/{{.Hash}}"><td class="m"><a href="/tx/{{.Hash}}" style="color:inherit">{{slice .Hash 0 10}}…{{slice .Hash 58}}</a></td><td>{{if .IsCoinbase}}<span style="color:var(--ac)">coinbase</span>{{else}}transfer{{end}}</td><td class="r">{{.Inputs}}</td><td class="r">{{.Outputs}}</td><td class="r ad m">{{if not .IsCoinbase}}{{printf "%.8f" .Fee}}{{end}}</td></tr>{{end}}</tbody></table></div></div>
</div>
</main>
{{template "foot" .}}
<script>(function(){var s=document.querySelector('[data-dur]');if(s&&window.BN)s.textContent=BN.dur(+s.getAttribute('data-dur'));})();</script>
</body>
</html>`

const explorerTxTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Transaction {{slice .Hash 0 12}} · blocknet explorer</title>
<meta name="description" content="A private blocknet transaction.">
<meta property="og:title" content="Transaction · blocknet">
<meta property="og:description" content="A private blocknet transaction. Amounts and senders are hidden.">
{{template "head" .}}
</head>
<body>
{{template "top" .}}
<main class="wrap" data-decode>
<div class="crumb"><a href="/">blocks</a> / {{if .InMempool}}mempool{{else}}<a href="/block/{{.BlockHeight}}">block {{.BlockHeight}}</a>{{end}} / transaction</div>
<div class="vh"><div><div class="ttl"><div class="mid"><span>#</span>{{slice .Hash 0 12}}…{{slice .Hash 58}}</div><div class="slot"><canvas data-gyroid="900" aria-hidden="true"></canvas></div></div>
<div class="pills">{{if .InMempool}}<span class="pill" style="border-color:#3a2a00;color:#fa0">Pending (in mempool)</span>{{else}}<span class="pill g"><span id="tx-confirmations" data-conf-height="{{.BlockHeight}}">{{.Confirmations}}</span> confirmations</span>{{end}}<span class="pill{{if .IsCoinbase}} g{{end}}">{{if .IsCoinbase}}coinbase{{else}}transfer{{end}}</span><span class="pill">amounts hidden</span></div></div>
{{if not .InMempool}}<div class="navbtn"><a href="/block/{{.BlockHeight}}">block {{.BlockHeight}} →</a></div>{{end}}</div>
<div class="vgrid">
<div>
<div class="card pad"><div class="kv3">
<div class="kk">hash</div><div class="vv m"><span>{{.Hash}}</span><button class="cp" data-cp="{{.Hash}}" type="button">copy</button></div>
<div class="kk">tx public key</div><div class="vv m"><span>{{.TxPubKey}}</span><button class="cp" data-cp="{{.TxPubKey}}" type="button">copy</button></div>
<div class="kk">block</div><div class="vv"><span>{{if .InMempool}}<span style="color:var(--mu)">pending</span>{{else}}<a href="/block/{{.BlockHeight}}">{{.BlockHeight}}</a>{{end}}</span></div>
{{if not .IsCoinbase}}<div class="kk">fee</div><div class="vv"><span>{{printf "%.8f" .Fee}} <span style="color:var(--mu)">BNT</span></span></div>{{end}}
<div class="kk">inputs</div><div class="vv"><span>{{.InputCount}}</span></div>
<div class="kk">outputs</div><div class="vv"><span>{{.OutputCount}}</span></div>
</div></div>
<div class="card pad prove mt"><h3>tx proof</h3>
<div class="tabs"><button class="on" data-t="r" type="button">received</button><button data-t="s" type="button">sent</button></div>
<div data-p="r"><p class="note" style="margin-bottom:8px">Check if an output in this transaction is yours.</p><p class="warnl">Your view private key is sent to this server.</p>
<input id="recv-spend" placeholder="spend public key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off"><input id="recv-view" placeholder="view private key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off"><button class="go" id="recv-btn" type="button">check</button></div>
<div data-p="s" style="display:none"><p class="note" style="margin-bottom:8px">Prove you sent this transaction. Give the view private key and the recipient's public keys. Anyone can check this proof.</p>
<input id="send-txpriv" placeholder="view private key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off"><input id="send-spend" placeholder="recipient spend public key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off"><input id="send-view" placeholder="recipient view public key (64 hex)" maxlength="64" spellcheck="false" autocomplete="off"><button class="go" id="send-btn" type="button">prove</button></div>
<div id="prove-result" style="display:none"></div></div>
</div>
<div>
{{if .Inputs}}<div class="card pad"><h3>inputs <em>{{.InputCount}}</em></h3>{{range .Inputs}}<div class="ioc"><div class="hd"><span>input {{.Index}}</span><span class="pill">ring size {{.RingSize}}</span></div><div class="kv3"><div class="kk">key image</div><div class="vv m"><span>{{.KeyImage}}</span></div><div class="kk">pseudo output</div><div class="vv m"><span>{{.PseudoOutput}}</span></div></div></div>{{end}}</div>{{end}}
<div class="card pad{{if .Inputs}} mt{{end}}"><h3>outputs <em>stealth addresses · amounts hidden</em></h3>{{range $i, $o := .Outputs}}<div class="ioc"{{if ge $i 4}} data-more style="display:none"{{end}}><div class="hd"><span>output {{$o.Index}}</span><span class="pill">{{$o.RangeProof}} byte range proof</span></div><div class="kv3"><div class="kk">stealth address</div><div class="vv m"><span>{{$o.PublicKey}}</span></div><div class="kk">commitment</div><div class="vv m"><span>{{$o.Commitment}}</span></div>{{if $o.EncMemo}}<div class="kk ad">memo</div><div class="vv m ad"><span><span style="color:var(--ac)">encrypted</span> {{$o.EncMemo}}</span></div>{{end}}</div></div>{{end}}
{{if gt (len .Outputs) 4}}<button class="btn" id="showall" type="button">show all {{len .Outputs}} outputs</button>{{end}}</div>
</div>
</div>
</main>
{{template "foot" .}}
<script>
(function(){
var TXH={{.Hash}};
var sa=document.getElementById('showall');if(sa)sa.onclick=function(){document.querySelectorAll('[data-more]').forEach(function(x){x.style.display='';});sa.remove();};
var res=document.getElementById('prove-result');
document.querySelectorAll('.tabs button').forEach(function(bt){bt.onclick=function(){document.querySelectorAll('.tabs button').forEach(function(x){x.classList.toggle('on',x===bt);});document.querySelectorAll('[data-p]').forEach(function(p){p.style.display=p.getAttribute('data-p')===bt.getAttribute('data-t')?'':'none';});res.style.display='none';};});
function esc(s){return String(s).replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}
function show(h){res.style.display='block';res.innerHTML=h;}
function render(data){if(data.error){show('<span class="err">'+esc(data.error)+'</span>');return;}
var m=data.filter(function(o){return o.match;});if(!m.length){show('<span class="note">No outputs in this transaction match these keys.</span>');return;}
var h='<div style="color:var(--ac);margin-bottom:8px">'+m.length+' output'+(m.length>1?'s':'')+' matched</div><div class="kv3">';
m.forEach(function(o){h+='<div class="kk">output '+o.index+'</div><div class="vv"><span style="color:var(--ac);font-size:18px">'+o.amount.toFixed(8)+' BNT</span></div>';if(o.memo)h+='<div class="kk">memo</div><div class="vv"><span>'+esc(o.memo)+'</span></div>';});show(h+'</div>');}
function req(url,body,btn,label){btn.disabled=true;btn.textContent='checking…';
fetch(url,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:body}).then(function(r){return r.json();}).then(function(d){btn.disabled=false;btn.textContent=label;render(d);}).catch(function(){btn.disabled=false;btn.textContent=label;show('<span class="err">Request failed</span>');});}
function v(id){return document.getElementById(id).value.trim();}
function hex(s){return /^[0-9a-fA-F]{64}$/.test(s);}
document.getElementById('recv-btn').onclick=function(){var sp=v('recv-spend'),vp=v('recv-view');if(!hex(sp)||!hex(vp)){show('<span class="err">Both keys must be 64 hex characters.</span>');return;}req('/prove','tx='+TXH+'&spend_pub='+sp+'&view_priv='+vp,this,'check');};
document.getElementById('send-btn').onclick=function(){var tp=v('send-txpriv'),sp=v('send-spend'),vp=v('send-view');if(!hex(tp)||!hex(sp)||!hex(vp)){show('<span class="err">All keys must be 64 hex characters.</span>');return;}req('/prove-send','tx='+TXH+'&tx_priv='+tp+'&spend_pub='+sp+'&view_pub='+vp,this,'prove');};
})();
</script>
</body>
</html>`

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
</head>
<body>
{{template "top" .}}
<main class="wrap sboard">
<div class="crumb"><a href="/">blocks</a> / stats</div>
<div class="vh"><div class="ttl"><div class="big" style="font-size:56px">network</div><div class="slot"><canvas data-gyroid="900" aria-hidden="true"></canvas></div></div><div class="cnow">hover a chart for values</div></div>
<div class="g g4">
<div class="card"><div class="k">height</div><div class="big" style="font-size:34px;margin-top:6px" id="stat-height">{{.Height}}</div></div>
<div class="card"><div class="k">hashrate</div><div class="big" style="font-size:34px;margin-top:6px">{{.Hashrate}}<small style="font-size:14px;color:var(--mu)"> H/s</small></div></div>
<div class="card"><div class="k">difficulty</div><div class="big" style="font-size:34px;margin-top:6px" id="stat-difficulty">{{.Difficulty}}</div></div>
<div class="card"><div class="k">avg block time</div><div class="big" style="font-size:34px;margin-top:6px">{{.AvgBlockTime}}<small style="font-size:14px;color:var(--mu)"> s</small></div></div>
<div class="card"><div class="k">window transactions</div><div class="big" style="font-size:34px;margin-top:6px">{{.TotalTx}}</div></div>
<div class="card"><div class="k">coins emitted</div><div class="big" style="font-size:34px;margin-top:6px">{{.Emitted}}</div></div>
<div class="card"><div class="k">emission progress</div><div class="big" style="font-size:34px;margin-top:6px">{{.PctEmitted}}<small style="font-size:14px;color:var(--mu)"> %</small></div></div>
<div class="card"><div class="k">peers</div><div class="big" style="font-size:34px;margin-top:6px">{{.Peers}}</div></div>
</div>
<div class="g g4 mt" id="cards"></div>
</main>
<script>window.BN_STATS={points:{{.DataJSON}},gen:{{.GenesisTs}},height:{{.Height}}};</script>
{{template "foot" .}}
` + explorerBoardScript + `
</body>
</html>`

const explorerNotFoundTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Not found · blocknet explorer</title>
<meta name="robots" content="noindex">
{{template "head" .}}
</head>
<body>
{{template "top" .}}
<main class="wrap">
<div class="nf"><div class="slot"><canvas data-gyroid="1100" aria-hidden="true"></canvas></div><h1>nothing found for {{.What}}</h1><p>{{.Why}}</p>
<form class="q2" action="/search" method="get"><input name="q" placeholder="height, block hash or tx hash" spellcheck="false" autocomplete="off"><button type="submit">search</button></form>
<p style="margin-top:18px"><a href="/">back to blocks</a> · <a href="/stats">stats</a></p></div>
</main>
{{template "foot" .}}
</body>
</html>`
