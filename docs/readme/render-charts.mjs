import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const script = fileURLToPath(import.meta.url);
const directory = path.dirname(script);
const data = fs.readFileSync(path.join(directory, 'chart-data.json'));
const charts = JSON.parse(data);
const WIDTH = 720;
const PERCENT = 100;
const COLORS = ['#8250df', '#bf8700', '#57606a'];
const BACKGROUND = '#161b22';
const TEXT = '#f0f6fc';
const MUTED_TEXT = '#a4adb8';
const assets = {};
const escape = value => String(value).replace(/[&<>"']/g, character => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
})[character]);
const count = value => new Intl.NumberFormat('en-US', { maximumFractionDigits: 2 }).format(value);
const svgStyle = `<style>text{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;fill:${TEXT}}.muted{fill:${MUTED_TEXT}}.track,.axis{stroke:#3d444d}</style>`;
const hash = value => createHash('sha256').update(value).digest('hex');

function frame(id, title, height, content, description) {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${WIDTH}" height="${height}" viewBox="0 0 ${WIDTH} ${height}" role="img" aria-labelledby="${id}-title ${id}-description"><title id="${id}-title">${escape(title)}</title><desc id="${id}-description">${escape(description)}</desc>${svgStyle}<rect width="${WIDTH}" height="${height}" rx="10" fill="${BACKGROUND}"/>${content}</svg>`;
}

function save(id, svg) {
  const file = `${id}.svg`;
  fs.writeFileSync(path.join(directory, file), svg);
  assets[file] = hash(svg);
}

function ring(chart) {
  assert.ok(Number.isInteger(chart.numerator) && chart.numerator >= 0);
  assert.ok(Number.isInteger(chart.denominator) && chart.denominator > 0);
  assert.ok(chart.numerator <= chart.denominator);
  const percentage = chart.numerator / chart.denominator * PERCENT;
  const formatted = percentage.toFixed(1);
  const X = 132, Y = 126, RADIUS = 88, STROKE = 24;
  const description = `${chart.label}: ${count(chart.numerator)} of ${count(chart.denominator)} (${formatted}%). ${chart.note}`;
  const lines = [];
  for (const word of chart.label.split(' ')) {
    const previous = lines.at(-1);
    if (previous && `${previous} ${word}`.length <= 34) lines[lines.length - 1] += ` ${word}`;
    else lines.push(word);
  }
  assert.ok(lines.length <= 4, 'Use a concise chart label');
  const text = lines.map((line, index) => `<text x="260" y="${91 + index * 29}" font-size="21" font-weight="600">${escape(line)}</text>`).join('');
  const content = `<circle class="track" cx="${X}" cy="${Y}" r="${RADIUS}" fill="none" stroke-width="${STROKE}"/><circle cx="${X}" cy="${Y}" r="${RADIUS}" fill="none" stroke="${COLORS[0]}" stroke-width="${STROKE}" pathLength="${PERCENT}" stroke-dasharray="${percentage} ${PERCENT - percentage}" transform="rotate(-90 ${X} ${Y})"/><text x="${X}" y="${Y + 9}" font-size="34" font-weight="700" text-anchor="middle">${formatted}%</text>${text}<text x="260" y="${112 + lines.length * 29}" class="muted" font-size="19">${count(chart.numerator)} of ${count(chart.denominator)}</text>`;
  return frame(chart.id, chart.title, 252, content, description);
}

function bars(chart) {
  assert.ok(chart.series.length > 0 && chart.series.length <= COLORS.length);
  for (const item of chart.series) {
    assert.ok(typeof item.value === 'number' && Number.isFinite(item.value) && item.value >= 0);
    assert.ok(item.label.length <= 60);
  }
  const maximum = Math.max(...chart.series.map(item => item.value));
  assert.ok(maximum > 0);
  const magnitude = 10 ** Math.floor(Math.log10(maximum));
  const axisMaximum = Math.ceil(maximum / magnitude) * magnitude;
  const LEFT = 8, PLOT_WIDTH = 560, TOP = 50, ROW_HEIGHT = 88, BAR_HEIGHT = 26;
  const TICK_COUNT = 4;
  const axisY = TOP + chart.series.length * ROW_HEIGHT - 25;
  let content = '';
  for (let index = 0; index <= TICK_COUNT; index++) {
    const x = LEFT + index / TICK_COUNT * PLOT_WIDTH;
    content += `<line class="axis" x1="${x}" x2="${x}" y1="${TOP - 12}" y2="${axisY}"/><text x="${x}" y="${axisY + 28}" class="muted" font-size="16" text-anchor="${index === 0 ? 'start' : 'middle'}">${count(axisMaximum * index / TICK_COUNT)}</text>`;
  }
  chart.series.forEach((item, index) => {
    const y = TOP + index * ROW_HEIGHT;
    const label = `${count(item.value)}${chart.unit ? ` ${chart.unit}` : ''}`;
    content += `<text x="${LEFT}" y="${y - 19}" font-size="19" font-weight="600">${escape(item.label)}</text><rect x="${LEFT}" y="${y}" width="${item.value / axisMaximum * PLOT_WIDTH}" height="${BAR_HEIGHT}" rx="5" fill="${COLORS[index]}"/><text x="${LEFT + item.value / axisMaximum * PLOT_WIDTH + 12}" y="${y + 20}" font-size="18" font-weight="600">${escape(label)}</text>`;
  });
  content += `<text x="${LEFT}" y="${axisY + 56}" class="muted" font-size="16">${escape(chart.axisLabel)} · zero-based scale</text>`;
  const summary = chart.series.map(item => `${item.label}: ${count(item.value)} ${chart.unit}`).join('; ');
  return frame(chart.id, chart.title, axisY + 75, content, `${summary}. ${chart.note}`);
}

assert.equal(new Set(charts.map(chart => chart.id)).size, charts.length);
for (const chart of charts) {
  assert.match(chart.id, /^[a-z][a-z0-9-]*$/);
  assert.ok(chart.title && chart.note && chart.sources.length > 0);
  for (const source of chart.sources) assert.equal(new URL(source.url).protocol, 'https:');
  assert.ok(chart.kind === 'ring' || chart.kind === 'bars');
  save(chart.id, chart.kind === 'ring' ? ring(chart) : bars(chart));
}

const panels = [
  { x: 8, title: 'People + agents', lines: ['Your team', 'Your chosen agents', 'Outside contributors'] },
  { x: 252, title: 'Hive', lines: ['Shared work queue', 'Claims + permissions', 'Review records'] },
  { x: 496, title: 'Your tools', lines: ['GitHub + Enterprise', 'GitHub Projects', 'Linear + Jira'] },
];
const PANEL_WIDTH = 208, PANEL_HEIGHT = 198;
const boxes = panels.map((panel, index) => {
  const center = panel.x + PANEL_WIDTH / 2;
  const labels = panel.lines.map((line, lineIndex) => `<text x="${center}" y="${103 + lineIndex * 31}" font-size="16" text-anchor="middle">${escape(line)}</text>`).join('');
  return `<rect x="${panel.x}" y="12" width="${PANEL_WIDTH}" height="${PANEL_HEIGHT}" rx="10" fill="none" stroke="${index === 1 ? COLORS[1] : COLORS[0]}" stroke-width="2"/><text x="${center}" y="58" font-size="21" font-weight="700" text-anchor="middle">${escape(panel.title)}</text>${labels}`;
}).join('');
const arrows = ['M216 111H242', 'M460 111H486']
  .map(d => `<path d="${d}" fill="none" stroke="#8c959f" stroke-width="2" marker-end="url(#workflow-arrow)"/>`).join('');
save('workflow', frame(
  'workflow', 'What Hive adds to your agents', 224,
  `<defs><marker id="workflow-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse"><path d="M0 0L10 5L0 10Z" fill="#8c959f"/></marker></defs>${boxes}${arrows}`,
  'People bring their own agents. Hive connects shared work, task claims, permissions and review records across GitHub, GitHub Enterprise, GitHub Projects, Linear and Jira.',
));
const source = createHash('sha256').update(fs.readFileSync(script)).update(data).digest('hex');
fs.writeFileSync(path.join(directory, 'manifest.json'), JSON.stringify({ source, assets }, null, 2) + '\n');
console.log(`Rendered ${Object.keys(assets).length} README graphics.`);
