// output_log.js keeps everything the dashboard auth proxy prints as the
// "proxy" log source the hive's admin MCP log_read tool reads
// (hivecommons/hive#11944). It must stay in step with the Go writer in
// src/pkg/logread/output.go:
//
//   - one JSON record per line: {"time":"<RFC3339 UTC>","stream":"stdout|stderr","text":"..."}
//   - current file <dir>/proxy.log, rotated to proxy-2006-01-02T15-04-05.000.log (UTC)
//   - at most 5 files of 10 MiB each, nothing older than 7 days
//   - files 0600, directory 0700
//
// The proxy's output still goes to the container's stdout/stderr exactly as
// before: the original write still runs with the same arguments and its
// result is returned. Keeping a copy never throws; when the directory cannot be used the copy is
// silently skipped.
import fs from 'fs';
import path from 'path';

export const OUTPUT_DIR_ENV = 'HIVE_OUTPUT_LOG_DIR';
export const DEFAULT_OUTPUT_DIR = '/data/.hive/logs';
export const SOURCE_PROXY = 'proxy';
export const MAX_FILE_BYTES = 10 * 1024 * 1024;
export const MAX_FILES = 5;
export const MAX_AGE_MS = 7 * 24 * 60 * 60 * 1000;
export const LINE_MAX_BYTES = 64 * 1024;
const PRUNE_EVERY_MS = 60 * 60 * 1000;
const BACKUP_RE = /^(\d{4}-\d{2}-\d{2})T(\d{2})-(\d{2})-(\d{2})\.(\d{3})\.log(?:\.gz)?$/;

// outputDir mirrors logread.OutputDir.
export function outputDir(env) {
  const dir = String((env && env[OUTPUT_DIR_ENV]) || '').trim();
  return dir || DEFAULT_OUTPUT_DIR;
}

// backupName mirrors OutputLog.backupPath: <stem>-<UTC ms stamp>.log.
export function backupName(stem, ms) {
  return `${stem}-${new Date(ms).toISOString().replace(/:/g, '-').replace(/Z$/, '')}.log`;
}

// backupStamp returns the epoch ms a rotated file name carries, or NaN.
export function backupStamp(stem, name) {
  if (!name.startsWith(`${stem}-`)) return NaN;
  const m = BACKUP_RE.exec(name.slice(stem.length + 1));
  if (!m) return NaN;
  return Date.parse(`${m[1]}T${m[2]}:${m[3]}:${m[4]}.${m[5]}Z`);
}

export class OutputLog {
  constructor(dir, stem, opts = {}) {
    if (!String(dir || '').trim() || !String(stem || '').trim()) {
      throw new Error('output log needs a directory and a name');
    }
    this.dir = dir;
    this.stem = stem;
    this.maxBytes = opts.maxBytes || MAX_FILE_BYTES;
    this.maxFiles = opts.maxFiles || MAX_FILES;
    this.maxAgeMs = opts.maxAgeMs || MAX_AGE_MS;
    this.now = opts.now || Date.now;
    this.fd = null;
    this.size = 0;
    this.pruned = 0;
    this.pending = new Map();
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    try { fs.chmodSync(dir, 0o700); } catch { /* not the owner: keep going */ }
    this.openCurrent();
    this.prune();
  }

  currentPath() { return path.join(this.dir, `${this.stem}.log`); }

  openCurrent() {
    const fd = fs.openSync(this.currentPath(), 'a', 0o600);
    try { fs.fchmodSync(fd, 0o600); } catch { /* keep going */ }
    this.fd = fd;
    this.size = fs.fstatSync(fd).size;
  }

  // writeRecord appends one already-terminated record, rotating first when it
  // would push the current file past its bound.
  writeRecord(rec) {
    if (this.fd === null) return;
    const buf = Buffer.from(rec, 'utf8');
    if (this.size > 0 && this.size + buf.length > this.maxBytes) {
      this.rotate();
      if (this.fd === null) return;
    } else if (this.now() - this.pruned >= PRUNE_EVERY_MS) {
      this.prune();
    }
    fs.writeSync(this.fd, buf);
    this.size += buf.length;
  }

  rotate() {
    try { fs.closeSync(this.fd); } catch { /* already gone */ }
    this.fd = null;
    let stamp = this.now();
    for (let i = 0; i < 1000 && fs.existsSync(path.join(this.dir, backupName(this.stem, stamp))); i++) stamp += 1;
    try { fs.renameSync(this.currentPath(), path.join(this.dir, backupName(this.stem, stamp))); } catch { /* reopen below regardless */ }
    this.openCurrent();
    this.prune();
  }

  prune() {
    const now = this.now();
    this.pruned = now;
    if (this.fd !== null && this.size > 0) {
      try {
        if (now - fs.fstatSync(this.fd).mtimeMs > this.maxAgeMs) {
          fs.ftruncateSync(this.fd, 0);
          this.size = 0;
        }
      } catch { /* keep the file */ }
    }
    let names;
    try { names = fs.readdirSync(this.dir); } catch { return; }
    const backups = names
      .map((name) => ({ name, stamp: backupStamp(this.stem, name) }))
      .filter((b) => !Number.isNaN(b.stamp))
      .sort((a, b) => b.stamp - a.stamp);
    let kept = 1;
    for (const b of backups) {
      if (kept >= this.maxFiles || now - b.stamp > this.maxAgeMs) {
        try { fs.unlinkSync(path.join(this.dir, b.name)); } catch { /* already gone */ }
        continue;
      }
      kept++;
    }
  }

  // writeStream stamps each complete line of chunk with the time and stream.
  // A line longer than LINE_MAX_BYTES is split; a trailing partial line waits
  // for the rest.
  writeStream(stream, chunk) {
    let buf = this.pending.get(stream) || '';
    buf += Buffer.isBuffer(chunk) ? chunk.toString('utf8') : String(chunk);
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      this.emit(stream, buf.slice(0, i));
      buf = buf.slice(i + 1);
    }
    while (Buffer.byteLength(buf, 'utf8') >= LINE_MAX_BYTES) {
      const head = Buffer.from(buf, 'utf8').subarray(0, LINE_MAX_BYTES).toString('utf8');
      this.emit(stream, head);
      buf = buf.slice(head.length);
    }
    this.pending.set(stream, buf);
  }

  emit(stream, line) {
    const text = line.endsWith('\r') ? line.slice(0, -1) : line;
    this.writeRecord(`${JSON.stringify({ time: new Date(this.now()).toISOString(), stream, text })}\n`);
  }

  // flush writes any partial lines, as the Go writer does on Close.
  flush() {
    for (const [stream, buf] of this.pending) {
      if (buf) this.emit(stream, buf);
    }
    this.pending.clear();
  }

  close() {
    this.flush();
    if (this.fd !== null) {
      try { fs.closeSync(this.fd); } catch { /* already gone */ }
      this.fd = null;
    }
  }
}

// installOutputLog tees process.stdout and process.stderr into the proxy log
// source. The test harness (NODE_ENV=test) leaves it off unless it names a
// directory. Returns the OutputLog, or null when nothing is kept.
export function installOutputLog(proc = process) {
  const env = proc.env || {};
  if (env.NODE_ENV === 'test' && !String(env[OUTPUT_DIR_ENV] || '').trim()) return null;
  let log;
  try {
    log = new OutputLog(outputDir(env), SOURCE_PROXY);
  } catch {
    return null;
  }
  for (const stream of ['stdout', 'stderr']) {
    const target = proc[stream];
    const original = target.write;
    target.write = function writeAndKeep(chunk, ...rest) {
      // The copy is kept first so a line printed just before the process
      // dies (the [SECURITY] refusal, a crash) is never lost from it.
      try {
        if (typeof chunk === 'string' || Buffer.isBuffer(chunk) || chunk instanceof Uint8Array) {
          log.writeStream(stream, Buffer.isBuffer(chunk) || typeof chunk === 'string' ? chunk : Buffer.from(chunk));
        }
      } catch { /* keeping a copy must never break the proxy */ }
      return original.call(this, chunk, ...rest);
    };
  }
  proc.on('exit', () => {
    try { log.close(); } catch { /* exiting anyway */ }
  });
  return log;
}
