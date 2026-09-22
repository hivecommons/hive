// Package audit contains the report-only audit campaign pilot fixture. It is
// deliberately not wired into any hive binary: this slice proves pinned-scope
// inspection, finding dedupe, receipts, and shadow journaling under tests while
// publication remains off.
package audit
