import { strict as assert } from 'assert';
import { readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import path from 'path';

// Regression guard for the /api-docs Redoc supply-chain hardening: the CDN
// script must be pinned to a specific version and carry a Subresource Integrity
// hash, never the mutable 'latest' tag without SRI.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const src = readFileSync(path.join(__dirname, 'server.js'), 'utf8');

assert.ok(
  !/cdn\.redoc\.ly\/redoc\/latest\//.test(src),
  "server.js must not load Redoc from the mutable 'latest' tag",
);
const script = src.match(/<script[^>]*cdn\.redoc\.ly\/redoc\/[^>]*>/);
assert.ok(script, 'expected a Redoc <script> tag referencing cdn.redoc.ly');
assert.ok(
  /cdn\.redoc\.ly\/redoc\/v\d+\.\d+\.\d+\//.test(script[0]),
  'Redoc script must be pinned to a specific vMAJOR.MINOR.PATCH version',
);
assert.ok(
  /integrity="sha(256|384|512)-[A-Za-z0-9+/=]+"/.test(script[0]),
  'Redoc script must carry a Subresource Integrity hash',
);
assert.ok(
  /crossorigin=/.test(script[0]),
  'Redoc script must set crossorigin for SRI to be enforced',
);
console.log('redoc_pin.test.js: OK');
