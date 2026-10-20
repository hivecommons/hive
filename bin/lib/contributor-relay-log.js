'use strict';

const fs = require('fs');
const path = require('path');
const CAP = 10 * 1024 * 1024;

// A live-tail buffer, not history. Synchronous writes preserve stdout/stderr
// ordering without a tee child changing the relay PID or signal semantics.
function createRelayLog(file, cap = CAP) {
  if (!Number.isSafeInteger(cap) || cap <= 0) throw new Error('relay log cap must be a positive integer');
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  let fd;
  let size = 0;
  function open() {
    fd = fs.openSync(file, fs.constants.O_WRONLY | fs.constants.O_CREAT |
      fs.constants.O_TRUNC | fs.constants.O_NOFOLLOW, 0o600);
    fs.fchmodSync(fd, 0o600);
    size = 0;
  }
  try { fs.unlinkSync(file + '.1'); } catch (e) { if (e.code !== 'ENOENT') throw e; }
  open();
  return {
    write(chunk) {
      let data = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      while (data.length) {
        if (size === cap) {
          fs.closeSync(fd);
          fs.renameSync(file, file + '.1');
          open();
        }
        const part = data.subarray(0, cap - size);
        let written = 0;
        while (written < part.length) written += fs.writeSync(fd, part, written, part.length - written);
        size += written;
        data = data.subarray(written);
      }
    },
    close() { fs.closeSync(fd); },
  };
}

function mirrorRelayOutput(file) {
  const log = createRelayLog(file);
  const warn = process.stderr.write.bind(process.stderr);
  let enabled = true;
  for (const stream of [process.stdout, process.stderr]) {
    const original = stream.write.bind(stream);
    stream.write = function(chunk, encoding, callback) {
      if (enabled) {
        try {
          log.write(typeof chunk === 'string' ? Buffer.from(chunk, typeof encoding === 'string' ? encoding : 'utf8') : chunk);
        } catch (err) {
          enabled = false;
          try { log.close(); } catch (_) {}
          warn(`WARNING: relay watch log disabled: ${err.message}\n`);
        }
      }
      return original(chunk, encoding, callback);
    };
  }
}

module.exports = { createRelayLog, mirrorRelayOutput, CAP };
