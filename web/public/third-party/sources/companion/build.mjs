// Standalone companion preview. No package installs or gateway required.
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const root = dirname(fileURLToPath(import.meta.url));
const read = name => readFileSync(join(root, name), 'utf8');
const write = (name, text) => writeFileSync(join(root, name), text, 'utf8');

// These reference SVGs contain raster-traced horizontal vector runs. Keep their
// actual contours; remove the surrounding paper/wordmark and unify the ink.
// This edits SVG geometry, rather than tracing a new mascot by eye.
function referenceMask(filename, bounds) {
  const [left, top, right, bottom] = bounds;
  const width = right - left, height = bottom - top;
  const mask = new Uint8Array(width * height);
  for (const tag of read(`assets/${filename}`).matchAll(/<path\b[^>]*>/g)) {
    const fill = tag[0].match(/fill="(#[0-9a-f]{6})"/i)?.[1];
    const data = tag[0].match(/\bd="([^"]+)"/)?.[1];
    if (!fill || !data) continue;
    const [r, g, b] = fill.slice(1).match(/../g).map(v => parseInt(v, 16));
    if (!(b - r > 35 && b - g > 25 && b > 120)) continue;
    for (const segment of data.matchAll(/M(\d+) (\d+)h(\d+)v1H\d+z/g)) {
      const x = +segment[1], y = +segment[2], length = +segment[3];
      if (y < top || y >= bottom) continue;
      for (let i = Math.max(x, left); i < Math.min(x + length, right); i++) {
        mask[(y - top) * width + i - left] = 1;
      }
    }
  }
  if (mask.reduce((a, b) => a + b, 0) < 20) throw new Error(`Empty reference: ${filename}`);
  // Keep the whale/laptop component only: the crop can intersect a droplet or
  // the corner of a neighbouring object in the supplied traced reference.
  const seen = new Uint8Array(mask.length);
  let largest = [];
  for (let start = 0; start < mask.length; start++) {
    if (!mask[start] || seen[start]) continue;
    const group = [start]; seen[start] = 1;
    for (let q = 0; q < group.length; q++) {
      const j = group[q], x = j % width, y = Math.floor(j / width);
      for (const [nx,ny] of [[x-1,y],[x+1,y],[x,y-1],[x,y+1]]) {
        if (nx < 0 || nx >= width || ny < 0 || ny >= height) continue;
        const k = ny * width + nx;
        if (mask[k] && !seen[k]) { seen[k] = 1; group.push(k); }
      }
    }
    if (group.length > largest.length) largest = group;
  }
  mask.fill(0); largest.forEach(i => { mask[i] = 1; });
  return { mask, width, height };
}

function runs({ width, height }, predicate) {
  let d = '';
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      if (!predicate(x, y)) continue;
      const start = x;
      while (x + 1 < width && predicate(x + 1, y)) x++;
      const n = x - start + 1;
      d += `M${start} ${y}h${n}v1h-${n}z`;
    }
  }
  return d;
}

function holes({ mask, width, height }) {
  const visited = new Uint8Array(mask.length), result = [];
  for (let i = 0; i < mask.length; i++) {
    if (mask[i] || visited[i]) continue;
    const pixels = [i]; visited[i] = 1;
    let exterior = false;
    for (let q = 0; q < pixels.length; q++) {
      const j = pixels[q], x = j % width, y = Math.floor(j / width);
      if (x === 0 || y === 0 || x === width - 1 || y === height - 1) exterior = true;
      for (const [nx, ny] of [[x-1,y],[x+1,y],[x,y-1],[x,y+1]]) {
        if (nx < 0 || nx >= width || ny < 0 || ny >= height) continue;
        const k = ny * width + nx;
        if (!mask[k] && !visited[k]) { visited[k] = 1; pixels.push(k); }
      }
    }
    if (!exterior && pixels.length > 8) result.push(new Set(pixels));
  }
  return result.sort((a, b) => b.size - a.size);
}

// Trace the supplied vector cells into short contours. The subpixel tolerance
// removes scan-line stair steps without inventing a different silhouette.
function contours(grid, predicate) {
  const {width,height} = grid, edges = new Map();
  const has = (x,y) => x >= 0 && y >= 0 && x < width && y < height && predicate(x,y);
  const add = (x,y,nx,ny) => { const key=`${x},${y}`; if(!edges.has(key)) edges.set(key,[]); edges.get(key).push([nx,ny]); };
  for(let y=0;y<height;y++) for(let x=0;x<width;x++) {
    if(!has(x,y)) continue;
    if(!has(x,y-1)) add(x,y,x+1,y);
    if(!has(x+1,y)) add(x+1,y,x+1,y+1);
    if(!has(x,y+1)) add(x+1,y+1,x,y+1);
    if(!has(x-1,y)) add(x,y+1,x,y);
  }
  function simplify(points) {
    if(points.length < 3) return points;
    const a=points[0], b=points.at(-1), dx=b[0]-a[0], dy=b[1]-a[1], denominator=dx*dx+dy*dy;
    let farthest=0, index=0;
    for(let i=1;i<points.length-1;i++) {
      const p=points[i], t=denominator ? Math.max(0,Math.min(1,((p[0]-a[0])*dx+(p[1]-a[1])*dy)/denominator)) : 0;
      const distance=Math.hypot(p[0]-a[0]-t*dx,p[1]-a[1]-t*dy);
      if(distance > farthest) { farthest=distance; index=i; }
    }
    return farthest > .65 ? [...simplify(points.slice(0,index+1)).slice(0,-1), ...simplify(points.slice(index))] : [a,b];
  }
  const loops=[];
  while(edges.size) {
    const first=edges.keys().next().value.split(',').map(Number), loop=[first];
    let current=first, guard=0;
    do {
      const key=current.join(','), destinations=edges.get(key);
      if(!destinations?.length) throw new Error('Unclosed reference contour');
      const next=destinations.pop(); if(!destinations.length) edges.delete(key);
      current=next;
      if(++guard > width*height*4) throw new Error('Reference contour overflow');
      loop.push(current);
    } while(current[0]!==first[0] || current[1]!==first[1]);
    if(loop.length > 8) {
      const half=Math.floor(loop.length/2);
      loops.push([...simplify(loop.slice(0,half+1)).slice(0,-1),...simplify(loop.slice(half)).slice(0,-1)]);
    }
  }
  return loops;
}

function contourPath(loops) {
  const num = n => Number(n.toFixed(2));
  return loops.map(points => {
    let d='';
    for(let i=0;i<points.length;i++) {
      const p=points[i], prev=points[(i+points.length-1)%points.length], next=points[(i+1)%points.length];
      const a=Math.min(.25,.7/Math.hypot(p[0]-prev[0],p[1]-prev[1]));
      const b=Math.min(.25,.7/Math.hypot(next[0]-p[0],next[1]-p[1]));
      const before=[p[0]+(prev[0]-p[0])*a,p[1]+(prev[1]-p[1])*a], after=[p[0]+(next[0]-p[0])*b,p[1]+(next[1]-p[1])*b];
      d+=`${i?'L':'M'}${num(before[0])} ${num(before[1])}Q${p[0]} ${p[1]} ${num(after[0])} ${num(after[1])}`;
    }
    return d+'Z';
  }).join('');
}

function whale(filename, bounds, prefix, tailPivot, eyeOverride = null) {
  const grid = referenceMask(filename, bounds);
  const blue = (x, y) => grid.mask[y * grid.width + x] === 1;
  const whites = holes(grid);
  if (whites.length < 2) throw new Error(`${filename}: expected belly and eye; found ${whites.length}`);
  if (eyeOverride !== null) whites.slice(1).forEach(pixels => pixels.forEach(i => { grid.mask[i] = 1; }));
  const outline=contours(grid,blue);
  const whitePaths = whites.filter((_,i) => i === 0 || eyeOverride === null).map((pixels, i) => `<path class="${i === 0 ? 'whale-belly' : 'whale-eye'}" d="${contourPath(contours(grid, (x,y) => pixels.has(y * grid.width + x)))}" fill="#f8fbff"/>`).join('') + (eyeOverride || '');
  const content = `<g class="whale-body ${prefix}"><path class="whale-outline" data-points='${JSON.stringify(outline)}' data-pivot="${tailPivot}" d="${contourPath(outline)}" fill-rule="evenodd" fill="var(--whale-blue, #526bf4)"/>${whitePaths}</g>`;
  write(`assets/${prefix}.svg`, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${grid.width} ${grid.height}">${content}</svg>\n`);
  console.log(`${prefix}: ${grid.width} x ${grid.height}, white regions ${whites.map(s=>s.size).join(', ')}`);
  return content;
}

function clawd(filename, prefix) {
  // The building source has a comment before its <svg>. Strip comments before
  // unwrapping, otherwise it embeds a second 500px viewport inside our stage.
  const source = read(`assets/${filename}`).replace(/<!--[\s\S]*?-->/g, '').trim();
  if (/<script|<foreignObject|\son\w+=|https?:\/\//i.test(source.replace('http://www.w3.org/2000/svg', ''))) {
    throw new Error(`Unexpected executable or external content in ${filename}`);
  }
  const tokens = new Set();
  for (const m of source.matchAll(/\b(?:class|id)="([^"]+)"/g)) m[1].split(/\s+/).forEach(t=>tokens.add(t));
  for (const m of source.matchAll(/@keyframes\s+([\w-]+)/g)) tokens.add(m[1]);
  const expression = new RegExp(`(?<![\\w-])(${[...tokens].sort((a,b)=>b.length-a.length).join('|')})(?![\\w-])`, 'g');
  const scoped = source.replace(expression, token => `${prefix}-${token}`);
  if (!/^<svg\b[^>]*>/.test(scoped) || !/<\/svg>$/.test(scoped)) throw new Error(`Invalid SVG root in ${filename}`);
  const rendering = source.match(/shape-rendering="([^"]+)"/)?.[1];
  return `<g class="clawd ${prefix}-root"${rendering ? ` shape-rendering="${rendering}"` : ''}>${scoped.replace(/^<svg\b[^>]*>/, '').replace(/<\/svg>\s*$/, '')}</g>`;
}

// Retain the original startled eye's curves from deepwake, scaled onto the
// already accepted body so the character does not change between scenes.
function wakeEye() {
  const paths = [...read('assets/deepwake.svg').matchAll(/<path\b[^>]*\/>/g)]
    .map(m=>m[0]).filter(tag => /transform="translate\((561\.430419921875,406\.62158203125|538,431)\)"/.test(tag));
  if (paths.length !== 2) throw new Error('deepwake eye source changed; review selected contours');
  const content = paths.map(tag=>tag.replace(/fill="[^"]+"/, 'fill="#f8fbff"')).join('');
  return `<g transform="translate(64 39) scale(.27)"><g class="awake-eye"><g transform="translate(-530 -403)">${content}</g></g></g>`;
}

const sleep = whale('deepsleep.svg', [66, 66, 200, 165], 'whale-sleep', [106, 46]);
const work = whale('deepwork.svg', [94, 36, 237, 144], 'whale-work', [109, 52]);
const wake = whale('deepsleep.svg', [66, 66, 200, 165], 'whale-wake', [106, 46], wakeEye());
const laptopGrid = referenceMask('deepwork.svg', [27, 98, 107, 144]);
const laptop = `<path fill="#526bf4" d="${runs(laptopGrid, (x,y)=>laptopGrid.mask[y*laptopGrid.width+x] === 1)}"/>`;

const inserts = {
  CLAWD_LICENSE: read('assets/CLAWD-LICENSE.txt'),
  WHALE_SLEEP: sleep,
  WHALE_WORK: work,
  WHALE_WAKE: wake,
  LAPTOP: laptop,
  CLAWD_SLEEP: clawd('clawd-sleeping.svg', 'cs'),
  CLAWD_TYPE: clawd('clawd-working-typing.svg', 'ct'),
  CLAWD_JUGGLE: clawd('clawd-working-juggling.svg', 'cj'),
  CLAWD_BUILD: clawd('clawd-working-building.svg', 'cb'),
  CLAWD_DEBUG: clawd('clawd-working-debugger.svg', 'cd'),
  CLAWD_COMPLETE: clawd('clawd-happy.svg', 'cc'),
  CLAWD_HAPPY_SLEEP: clawd('clawd-happy.svg', 'hs'),
  CLAWD_HAPPY_WORK: clawd('clawd-happy.svg', 'hw'),
  CLAWD_HAPPY_SURF: clawd('clawd-happy.svg', 'hj'),
};
const output = read('index.template.html').replace(/\{\{([A-Z_]+)\}\}/g, (_, name) => {
  if (!(name in inserts)) throw new Error(`Unknown insert ${name}`);
  return inserts[name];
});
write('index.html', output);
console.log(`Built standalone companion: ${Buffer.byteLength(output)} bytes`);

// The production stage uses exactly this approved composition, without the
// preview's simulated telemetry, controls or document-wide runtime script.
// Run this same command after changing approved source artwork/template CSS.
function scopedCss(css) {
  const clean = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const names = new Set([...clean.matchAll(/@keyframes\s+([\w-]+)/g)].map(m => m[1]));
  const renamed = clean
    .replace(/(@keyframes\s+)([\w-]+)/g, (_, head, name) => `${head}companion-stage-${name}`)
    .replace(/(animation(?:-name)?\s*:)([^;}]+)/g, (_, head, value) =>
      head + value.replace(/[\w-]+/g, name => names.has(name) ? `companion-stage-${name}` : name));
  return renamed.replace(/([^{}]+)\{/g, (_, head) => {
    const selector = head.trim();
    if (selector.startsWith('@') || /^(?:\d|from\b|to\b)/.test(selector)) return `${head}{`;
    return `${selector.split(',').map(part => {
      const local = part.trim().replace(/#world\b/g, '.companion-world');
      return local.startsWith(':root') ? local.replace(':root', '.companion-stage') : `.companion-stage ${local}`;
    }).join(', ')} {`;
  });
}

const templateStyle = output.match(/<style>([\s\S]*?)<\/style>/)?.[1];
let world = output.match(/<svg id="world"[\s\S]*?<\/svg>/)?.[0];
if (!templateStyle || !world) throw new Error('Approved stage/template style not found');
const innerStyles = [...world.matchAll(/<style>([\s\S]*?)<\/style>/g)].map(m => m[1]);
const palette = [...templateStyle.slice(0, templateStyle.indexOf('* {')).matchAll(/:root[^{}]*\{[^{}]*\}/g)]
  .map(m => m[0]).filter(rule => !rule.includes('data-size')).join('\n');
const stageRules = templateStyle.slice(templateStyle.indexOf('.stage-button {'), templateStyle.indexOf('.telemetry {'));
if (!stageRules) throw new Error('Approved stage styles not found');
const stageCss = [palette, stageRules, ...innerStyles].map(scopedCss).join('\n') + `
.companion-stage { width:100%; color:var(--ink); }
.companion-stage .stage-button { font:inherit; color:inherit; -webkit-tap-highlight-color:transparent; }
.companion-stage .stage-button:focus-visible { outline:2px solid var(--blue); outline-offset:-5px; }
.companion-stage[data-paused="true"] .scene.is-active .hello-clawd * { animation-play-state:paused !important; }
.companion-stage .companion-stage-live { position:absolute; width:1px; height:1px; padding:0; margin:-1px; overflow:hidden; clip:rect(0,0,0,0); white-space:nowrap; border:0; }
@media (prefers-reduced-motion:reduce) {
  .companion-stage *, .companion-stage *::before, .companion-stage *::after { transition:none !important; animation-play-state:paused !important; }
}
`;
world = world.replace(/<style>[\s\S]*?<\/style>/g, '').replace('id="world"', 'class="companion-world"');
// React replaces this marker with the instance's stable unique prefix. SVG
// references must remain local even when two stages share the same document.
world = world.replace(/\bid="([^"]+)"/g, 'id="__COMPANION_INSTANCE__-$1"')
  .replace(/url\(#([^)]+)\)/g, 'url(#__COMPANION_INSTANCE__-$1)')
  .replace(/\bhref="#([^"]+)"/g, 'href="#__COMPANION_INSTANCE__-$1"');
if (/<script|<foreignObject|\son\w+=|\bsrc=|\bhref="(?!#__COMPANION_INSTANCE__-)/i.test(world)) throw new Error('Production SVG must be static and self-contained');
const projectRoot = join(root, '../../');
const componentDir = join(projectRoot, 'web/src/companion');
const publicDir = join(projectRoot, 'web/public/companion');
mkdirSync(componentDir, { recursive: true });
mkdirSync(publicDir, { recursive: true });
writeFileSync(join(componentDir, 'mascot.generated.ts'),
  `// Generated by scripts/companion/build.mjs. Trusted local SVG only.\n// Clawd: GNU AGPL v3; see web/public/companion/CLAWD-LICENSE.txt.\nexport const companionWorld = ${JSON.stringify(world)};\n`, 'utf8');
writeFileSync(join(componentDir, 'stage.css'), `/* Generated by scripts/companion/build.mjs; edit the approved template. */\n${stageCss}`, 'utf8');
writeFileSync(join(publicDir, 'CLAWD-LICENSE.txt'), read('assets/CLAWD-LICENSE.txt'), 'utf8');
const sources = JSON.parse(read('assets/SOURCES.json'));
writeFileSync(join(publicDir, 'SOURCES.json'), JSON.stringify({
  ...sources,
  license: 'CLAWD-LICENSE.txt (upstream GNU AGPL v3)',
  adaptations: 'Scoped SVG identifiers/CSS, scene composition and timing overrides; geometry preserved from approved companion prototype.',
  reproducible_source: 'scripts/companion/index.template.html, assets/ and build.mjs in this repository',
  whale: { source: 'User-supplied deepsleep.svg, deepwork.svg and deepwake.svg in scripts/companion/assets/; user identifies these as online circulating derivative artwork based on the official DeepSeek logo.', authorization: 'Derivative author, original publication URL and redistribution permission remain unknown; no license or official endorsement is claimed.', adaptations: 'Cropped original contours, simplified scanline corners and continuous tail deformation.' },
}, null, 2) + '\n', 'utf8');
console.log('Exported production stage SVG, scoped CSS and source/license metadata');
