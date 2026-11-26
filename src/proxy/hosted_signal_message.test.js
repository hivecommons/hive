import { strict as assert } from 'assert';
import { readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import path from 'path';

// The boot refusal for a self-hosted proxy with no HIVE_DASHBOARD_TOKEN must
// name every variable that makes the proxy count as hub-hosted (IS_HOSTED),
// so an operator reading it is not told a hosted spoke needs the retired
// HIVE_SESSION_KEY (hivecommons/hive#19995). IS_HOSTED is SESSION_KEY
// (HIVE_SESSION_KEY, else derived from HIVE_HUB_SECRET) or SESSION_PUBLIC_KEY
// (HIVE_SESSION_PUBLIC_KEY).
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const src = readFileSync(path.join(__dirname, 'server.js'), 'utf8');

assert.ok(
  /const IS_HOSTED = SESSION_KEY !== '' \|\| SESSION_PUBLIC_KEY !== '';/.test(src),
  'IS_HOSTED must stay keyed off SESSION_KEY and SESSION_PUBLIC_KEY; update this test and the refusal message together',
);

const refusal = src.split('\n').find((line) => line.includes('HIVE_DASHBOARD_TOKEN is not set'));
assert.ok(refusal, 'expected the HIVE_DASHBOARD_TOKEN boot refusal in server.js');
for (const name of ['HIVE_SESSION_PUBLIC_KEY', 'HIVE_SESSION_KEY', 'HIVE_HUB_SECRET']) {
  assert.ok(
    new RegExp(`\\b${name}\\b`).test(refusal),
    `the boot refusal must name ${name}, a hub-hosted signal`,
  );
}
console.log('hosted_signal_message.test.js: OK');
