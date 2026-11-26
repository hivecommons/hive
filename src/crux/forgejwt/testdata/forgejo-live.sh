#!/usr/bin/env bash
# Live check of crux/forgejwt against Forgejo 17 in podman or docker.
#
# Publishes a key list made by `hive forge key-list` from nginx over https
# with a private CA, starts Forgejo trusting only that host, creates a
# read-only, a write, a claim-free and a REST (sub=hive:rest) authorized
# integration, and runs the forgejo_jwt_live test.
# Run from anywhere: bash src/crux/forgejwt/testdata/forgejo-live.sh
#
#   RUNTIME=podman|docker  container runtime (default podman)
#   HOST_GO=1|0            run go on the host against the published Forgejo
#                          port (default 1 when go is on PATH); 0 runs it in a
#                          golang container on the network (podman only)
#
# Every container, volume and the network carry the fjjwt-live- prefix and are
# removed on exit. On failure the Forgejo and key server logs are printed.
set -euo pipefail

RUNTIME=${RUNTIME:-podman}
SRC=$(cd "$(dirname "$0")/../../.." && pwd)
FORGEJO_IMAGE=${FORGEJO_IMAGE:-codeberg.org/forgejo/forgejo:17.0.2}
GO_IMAGE=${GO_IMAGE:-docker.io/library/golang:1.27.4}
NGINX_IMAGE=${NGINX_IMAGE:-docker.io/library/nginx:1.29-alpine}
NET=fjjwt-live-net FJ=fjjwt-live-forgejo KEYS=fjjwt-live-keys DATA=fjjwt-live-data
ISSUER=https://$KEYS
STATE=$(mktemp -d)
HOST_GO=${HOST_GO:-$(command -v go >/dev/null && echo 1 || echo 0)}

# SELinux relabeling of bind mounts is for podman only.
ZS=
if [ "$RUNTIME" = podman ]; then ZS=,z; fi
if [ "$RUNTIME" != podman ] && [ "$HOST_GO" != 1 ]; then
	echo "RUNTIME=$RUNTIME needs go on PATH (HOST_GO=1)" >&2
	exit 1
fi
rt() { "$RUNTIME" "$@"; }

cleanup() {
	rc=$?
	if [ "$rc" -ne 0 ]; then
		for c in "$FJ" "$KEYS"; do
			echo "--- $c logs" >&2
			rt logs "$c" 2>&1 | tail -80 >&2 || true
		done
	fi
	rt rm -f "$FJ" "$KEYS" >/dev/null 2>&1 || true
	rt network rm "$NET" >/dev/null 2>&1 || true
	rt volume rm -f "$DATA" >/dev/null 2>&1 || true
	if [ "$RUNTIME" = podman ]; then podman unshare rm -rf "$STATE"; else rm -rf "$STATE"; fi
}
trap cleanup EXIT

gorun() { # args: KEY=VALUE env pairs, then --, then the command; runs in src/
	local envs=() flags=()
	while [ "$1" != -- ]; do envs+=("$1"); flags+=(-e "$1"); shift; done
	shift
	if [ "$HOST_GO" = 1 ]; then
		(cd "$SRC" && env ${envs[@]+"${envs[@]}"} "$@")
	else
		rt run --rm --network "$NET" -v "$SRC/..:/w:rw$ZS" -v "$STATE:/state:rw$ZS" \
			-v hive-gocache:/go/pkg/mod -v hive-gobuild:/root/.cache/go-build \
			-w /w/src ${flags[@]+"${flags[@]}"} "$GO_IMAGE" "$@"
	fi
}
SD=/state
if [ "$HOST_GO" = 1 ]; then SD=$STATE; fi

mkdir -p "$STATE/pki" "$STATE/www"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$STATE/pki/ca.key" -out "$STATE/pki/ca.crt" \
	-days 1 -subj /CN=fjjwt-live-ca 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$STATE/pki/srv.key" -out "$STATE/pki/srv.csr" \
	-subj "/CN=$KEYS" 2>/dev/null
openssl x509 -req -in "$STATE/pki/srv.csr" -CA "$STATE/pki/ca.crt" -CAkey "$STATE/pki/ca.key" \
	-CAcreateserial -out "$STATE/pki/srv.crt" -days 1 \
	-extfile <(printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth' "$KEYS") 2>/dev/null
rt run --rm --entrypoint cat "$FORGEJO_IMAGE" /etc/ssl/certs/ca-certificates.crt >"$STATE/pki/bundle.pem"
cat "$STATE/pki/ca.crt" >>"$STATE/pki/bundle.pem"
chmod 644 "$STATE/pki/"*
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$STATE/jwt-signing.pem" 2>/dev/null
chmod 600 "$STATE/jwt-signing.pem"

rt network create "$NET" >/dev/null
gorun -- go run ./cmd/hive forge key-list --issuer "$ISSUER" --key "$SD/jwt-signing.pem" --out "$SD/www"
cat >"$STATE/nginx.conf" <<'EOF'
server {
  listen 443 ssl;
  ssl_certificate /pki/srv.crt;
  ssl_certificate_key /pki/srv.key;
  root /srv;
  location / { default_type application/json; }
}
EOF
rt run -d --name "$KEYS" --network "$NET" -v "$STATE/www:/srv:ro$ZS" -v "$STATE/pki:/pki:ro$ZS" \
	-v "$STATE/nginx.conf:/etc/nginx/conf.d/default.conf:ro$ZS" "$NGINX_IMAGE" >/dev/null

rt run -d --name "$FJ" --network "$NET" -p 127.0.0.1::3000 -v "$DATA:/data" -v "$STATE/pki:/pki:ro$ZS" \
	-e FORGEJO__database__DB_TYPE=sqlite3 -e FORGEJO__security__INSTALL_LOCK=true \
	-e FORGEJO__server__ROOT_URL="http://$FJ:3000/" -e FORGEJO__server__DISABLE_SSH=true \
	-e FORGEJO__repository__DEFAULT_BRANCH=main -e SSL_CERT_FILE=/pki/bundle.pem \
	-e FORGEJO__authorized_integration__ALLOWED_DOMAINS="$KEYS" \
	-e FORGEJO__authorized_integration__ALLOW_LOCALNETWORKS=false \
	"$FORGEJO_IMAGE" >/dev/null
fj() { rt exec -u git "$FJ" forgejo "$@"; }
for _ in $(seq 60); do
	rt exec "$FJ" curl -sf "http://localhost:3000/api/healthz" >/dev/null 2>&1 && break
	sleep 1
done
rt exec "$FJ" curl -sf "http://localhost:3000/api/healthz" >/dev/null
echo "forgejo: $(fj --version)"

fj admin user create --admin --username fjadmin --password 'Adm1n-pass-xyz' \
	--email fjadmin@example.invalid --must-change-password=false >/dev/null
fj admin user create --username bot --password 'B0t-pass-xyz' \
	--email bot@example.invalid --must-change-password=false >/dev/null
ADMIN=$(fj admin user generate-access-token --username fjadmin --token-name seed --scopes all --raw)
rt exec "$FJ" curl -sf -o /dev/null -H "Authorization: token $ADMIN" -H 'Content-Type: application/json' \
	-X POST "http://localhost:3000/api/v1/admin/users/bot/repos" -d '{"name":"probe","private":true,"auto_init":true}'
# For the REST endpoints the sweeps read (#19920): a write collaborator whose
# permission the hive's user asks about, and an issue that was closed and
# reopened so its timeline has a reopen event.
fj admin user create --username writer --password 'Wr1ter-pass-xyz' \
	--email writer@example.invalid --must-change-password=false >/dev/null
api() { # method path [json]: an admin API call, prints the response body
	rt exec "$FJ" curl -sf -H "Authorization: token $ADMIN" -H 'Content-Type: application/json' \
		-X "$1" "http://localhost:3000/api/v1$2" ${3:+-d "$3"}
}
api PUT /repos/bot/probe/collaborators/writer '{"permission":"write"}' >/dev/null
ISSUE=$(api POST /repos/bot/probe/issues '{"title":"jwt live"}' | sed -n 's/.*"number":\([0-9]*\).*/\1/p')
[ -n "$ISSUE" ] || { echo "no issue number from the issue create" >&2; exit 1; }
api PATCH "/repos/bot/probe/issues/$ISSUE" '{"state":"closed"}' >/dev/null
api PATCH "/repos/bot/probe/issues/$ISSUE" '{"state":"open"}' >/dev/null

SUBJECT=hive:advisor
set -x
fj admin user create-authorized-integration --username bot --name hive-advisor --issuer "$ISSUER" \
	--scope read:repository --repo bot/probe --claim-eq "sub=$SUBJECT" >"$STATE/advisor.json"
fj admin user create-authorized-integration --username bot --name hive-bare --issuer "$ISSUER" \
	--scope read:repository --repo bot/probe >"$STATE/bare.json"
fj admin user create-authorized-integration --username bot --name hive-contributor --issuer "$ISSUER" \
	--scope write:repository --repo bot/probe >"$STATE/write.json"
# The hive's own REST integration as forge-app-setup.md documents it. If
# Forgejo refuses read:user and read:organization on a repo-limited
# integration, retry without them; the test logs which scopes it got.
REST_SCOPES=write:repository,write:issue,read:user,read:organization
if ! fj admin user create-authorized-integration --username bot --name hive-rest --issuer "$ISSUER" \
	--scope "$REST_SCOPES" --repo bot/probe --claim-eq "sub=hive:rest" >"$STATE/rest.json"; then
	echo "create-authorized-integration refused $REST_SCOPES; retrying without the user and organization scopes" >&2
	REST_SCOPES=write:repository,write:issue
	fj admin user create-authorized-integration --username bot --name hive-rest --issuer "$ISSUER" \
		--scope "$REST_SCOPES" --repo bot/probe --claim-eq "sub=hive:rest" >"$STATE/rest.json"
fi
set +x
cat "$STATE/advisor.json"
audience() { sed -n 's/^ *"audience": *"\([^"]*\)".*/\1/p' "$1"; }
AUDIENCE=$(audience "$STATE/advisor.json") BARE=$(audience "$STATE/bare.json") WRITE=$(audience "$STATE/write.json")
REST=$(audience "$STATE/rest.json")
[ -n "$AUDIENCE" ] && [ -n "$BARE" ] && [ -n "$WRITE" ] && [ -n "$REST" ] || { echo "no audience in the integration output" >&2; exit 1; }

if [ "$HOST_GO" = 1 ]; then
	PORT=$(rt port "$FJ" 3000/tcp | head -1)
	URL=http://127.0.0.1:${PORT##*:}
else
	URL=http://$FJ:3000
fi
gorun FORGEJO_URL="$URL" FORGEJO_REPO=bot/probe FORGEJO_JWT_ISSUER="$ISSUER" \
	FORGEJO_JWT_KEY="$SD/jwt-signing.pem" FORGEJO_JWT_AUDIENCE="$AUDIENCE" FORGEJO_JWT_SUBJECT="$SUBJECT" \
	FORGEJO_JWT_BARE_AUDIENCE="$BARE" FORGEJO_JWT_WRITE_AUDIENCE="$WRITE" \
	FORGEJO_JWT_REST_AUDIENCE="$REST" FORGEJO_JWT_REST_SCOPES="$REST_SCOPES" \
	FORGEJO_ISSUE="$ISSUE" FORGEJO_COLLABORATOR=writer FORGEJO_OWNER=bot \
	-- go test -tags forgejo_jwt_live -run '^TestForgejoAcceptsMintedTokens$' -count=1 -v ./crux/forgejwt
