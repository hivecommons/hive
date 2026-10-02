const AGENTS = {
  scanner:    { emoji: '\u{1F50D}', label: 'scanner',    color: 0x3498db },
  'ci-maintainer':   { emoji: '✅',    label: 'ci-maintainer',   color: 0x2ecc71 },
  architect:  { emoji: '\u{1F3D7}', label: 'architect',  color: 0x9b59b6 },
  outreach:   { emoji: '\u{1F310}', label: 'outreach',   color: 0xe67e22 },
  supervisor: { emoji: '\u{1F451}', label: 'supervisor', color: 0xe74c3c },
  governor:   { emoji: '\u{1F6A6}', label: 'governor',   color: 0xf1c40f },
  pipeline:   { emoji: '⚙️', label: 'pipeline',   color: 0x95a5a6 },
};

function agentPrefix(name) {
  const a = AGENTS[name] || AGENTS.pipeline;
  return `${a.emoji} **[${a.label}]**`;
}

function agentColor(name) {
  return (AGENTS[name] || AGENTS.pipeline).color;
}

function agentNames() {
  return Object.keys(AGENTS).filter(n => n !== 'pipeline' && n !== 'governor');
}

module.exports = { AGENTS, agentPrefix, agentColor, agentNames };
