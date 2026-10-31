'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'contribute_landing.go'), 'utf8');
const generator = source.split('// MLX onboarding generator:')[1].split('// End MLX onboarding generator.')[0];
const context = {};
vm.runInNewContext(generator.slice(generator.indexOf('\n')), context);
const commands = (os = 'macos', mode = 'containerized', runtime = '', model = '/Users/example/models/coder') =>
  context.mlxCommands(os, mode, runtime, model, 'https://chosen.example', 'v5', 'brew install just gh');

test('MLX picker generates the real Pi-backed just workflow and safe host/container boundary', () => {
  const script = commands();
  for (const wanted of [
    'python3 -m venv .venv-mlx', 'pip install mlx-lm==0.32.2',
    'just contribute-mlx-serve \'/Users/example/models/coder\'',
    'just contribute-mlx-setup \'/Users/example/models/coder\' --verify-tools',
    'export AGENT_MODEL=\'mlx//Users/example/models/coder\'',
    'export HIVE_HUB=\'https://chosen.example\'',
    'export HIVE_CONTAINER_RUNTIME=docker',
    'just contribute-setup pi', 'just contribute-mlx\n',
  ]) assert.ok(script.includes(wanted), wanted);
  assert.ok(!script.includes('just contribute-setup mlx'));
  assert.ok(!script.includes('0.0.0.0'));
  assert.ok(script.includes('# HIVE_MLX_PYTHON='), 'foreground model server is a separate, explicit terminal step');
  assert.match(source, /<option value="mlx"/);
  assert.match(source, /mlx:\{name:'Apple MLX'.*peer:true/);
  assert.match(source, /mlx:'pi'/);
  assert.match(source, /window\.location\.hash==='#apple-mlx'/);
});

test('unsupported platforms, mode boundaries, Podman and unsafe model inputs do not launch anything', () => {
  for (const args of [
    ['linux'], ['windows'], ['macos', 'kubernetes'], ['macos', 'host'],
    ['macos', 'containerized', 'podman'],
    ['macos', 'containerized', '', 'model;touch /file'],
    ['macos', 'containerized', '', 'default_model'],
    ['macos', 'containerized', '', 'mlx/provider/model'],
    ['macos', 'containerized', '', ''],
  ]) {
    assert.ok(!commands(...args).includes('just contribute-mlx\n'), JSON.stringify(args));
  }
  assert.equal(context.mlxShellQuote("a'b"), "'a'\\''b'");
  assert.ok(commands('macos', 'containerized', '', 'mlx-community/coder').includes("AGENT_MODEL='mlx/mlx-community/coder'"));
});

test('MLX is additive; existing backend defaults and generic command templates remain', () => {
  assert.match(source, /var containerTpl='PREREQ\\ngit clone/);
  assert.match(source, /var SETUP_BACKEND=\{openrouter:'litellm',vllm:'litellm','llm-d':'litellm',watsonx:'litellm',mlx:'pi'\}/);
  assert.match(source, /if\(cli==='mlx'\)\{[\s\S]*?return;\n\}\nvar modelLine=''/);
  assert.match(source, /<option value="pi"/);
});
