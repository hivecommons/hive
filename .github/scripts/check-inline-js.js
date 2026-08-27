#!/usr/bin/env node
'use strict';

// Syntax-checks the inline <script> blocks of the HTML files whose paths are
// given as command line arguments. Paths are never interpolated into a shell
// or JavaScript string, so any filename (quotes, spaces, metacharacters) is
// handled safely.

const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');

const SCRIPT_BLOCK_RE = /<script[^>]*>([\s\S]*?)<\/script>/gi;

function checkFile(htmlFile, tmpDir) {
  const html = fs.readFileSync(htmlFile, 'utf8');
  let match;
  let idx = 0;
  const blocks = [];

  SCRIPT_BLOCK_RE.lastIndex = 0;
  while ((match = SCRIPT_BLOCK_RE.exec(html)) !== null) {
    const js = match[1].trim();
    if (!js) continue;
    const outFile = path.join(tmpDir, `block_${idx}.js`);
    fs.writeFileSync(outFile, js);
    blocks.push(outFile);
    idx++;
  }
  console.log(`Extracted ${idx} script block(s)`);

  let failed = false;
  for (const block of blocks) {
    console.log(`  Parsing ${path.basename(block)}...`);
    try {
      execFileSync(process.execPath, ['--check', block], { stdio: 'inherit' });
    } catch (err) {
      failed = true;
    }
    fs.rmSync(block, { force: true });
  }
  return !failed;
}

function main() {
  const files = process.argv.slice(2);
  if (files.length === 0) {
    console.log('No HTML files to check.');
    return 0;
  }

  const tmpRoot = process.env.RUNNER_TEMP || os.tmpdir();
  const tmpDir = fs.mkdtempSync(path.join(tmpRoot, 'inline-js-'));
  let ok = true;
  try {
    for (const htmlFile of files) {
      console.log(`Checking ${htmlFile}...`);
      if (!checkFile(htmlFile, tmpDir)) ok = false;
    }
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }

  if (!ok) {
    console.error('Syntax errors found in inline script block(s).');
    return 1;
  }
  console.log('All script blocks passed syntax check.');
  return 0;
}

process.exit(main());
