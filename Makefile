.PHONY: quota-report dashboard-gen dashboard-gen-check

quota-report:
	bash src/scripts/gh-quota-report.sh

# Regenerate src/pkg/dashboard/static/index.html from the per-area sources in
# src/pkg/dashboard/static-src/ (see src/docs/dashboard-source-layout.md).
dashboard-gen:
	cd src && go generate ./pkg/dashboard/webstatic/

# Fail if the committed index.html is stale relative to static-src/.
dashboard-gen-check:
	cd src && go run ./pkg/dashboard/webstatic/gen -src pkg/dashboard/static-src -out pkg/dashboard/static/index.html -check
