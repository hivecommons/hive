#!/usr/bin/env bash
# Live check of crux/forgejwt against Forgejo 17 in podman.
#
# Publishes a key list made by `hive forge key-list` from nginx over https
# with a private CA, starts Forgejo trusting only that host, creates a
# read-only authorized integration, and runs the forgejo_jwt_live test.
# Run from anywhere: bash src/crux/forgejwt/testdata/forgejo-live.sh
# Every container and the network carry the fjjwt-live- prefix and are
# removed on exit.
set -euo pipefail

SRC=$(cd "$(dirname "$0")/../../.." && pwd)
FORGEJO_IMAGE=${FORGEJO_IMAGE:-codeberg.org/forgejo/forgejo:17.0.2}
GO_IMAGE=${GO_IMAGE:-docker.io/library/golang:1.27.4}
NGINX_IMAGE=${NGINX_IMAGE:-docker.io/library/nginx:1.29-alpine}
NET=fjjwt-live-net FJ=fjjwt-live-forgejo KEYS=fjjwt-live-keys
ISSUER=https://$KEYS
STATE=$(mktemp -d)

cleanup() {
	podman rm -f "$FJ" "$KEYS" >/dev/null 2>&1 || true
	podman network rm "$NET" >/dev/null 2>&1 || true
	podman unshare rm -rf "$STATE"
}
trap cleanup EXIT

gorun() { # args: shell command run from src/ on $NET
	podman run --rm --network "$NET" -v "$SRC/..:/w:z" -v "$STATE:/state:z" \
		-v hive-gocache:/go/pkg/mod -v hive-gobuild:/root/.cache/go-build \
		-w /w/src "$@"
}

mkdir -p "$STATE/pki" "$STATE/www" "$STATE/data"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$STATE/pki/ca.key" -out "$STATE/pki/ca.crt" \
	-days 1 -subj /CN=fjjwt-live-ca 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$STATE/pki/srv.key" -out "$STATE/pki/srv.csr" \
	-subj "/CN=$KEYS" 2>/dev/null
openssl x509 -req -in "$STATE/pki/srv.csr" -CA "$STATE/pki/ca.crt" -CAkey "$STATE/pki/ca.key" \
	-CAcreateserial -out "$STATE/pki/srv.crt" -days 1 \
	-extfile <(printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth' "$KEYS") 2>/dev/null
podman run --rm --entrypoint cat "$FORGEJO_IMAGE" /etc/ssl/certs/ca-certificates.crt >"$STATE/pki/bundle.pem"
cat "$STATE/pki/ca.crt" >>"$STATE/pki/bundle.pem"
chmod 644 "$STATE/pki/"*
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$STATE/jwt-signing.pem" 2>/dev/null
chmod 600 "$STATE/jwt-signing.pem"

podman network create "$NET" >/dev/null
gorun "$GO_IMAGE" go run ./cmd/hive forge key-list --issuer "$ISSUER" --key /state/jwt-signing.pem --out /state/www
cat >"$STATE/nginx.conf" <<'EOF'
server {
  listen 443 ssl;
  ssl_certificate /pki/srv.crt;
  ssl_certificate_key /pki/srv.key;
  root /srv;
  location / { default_type application/json; }
}
EOF
podman run -d --name "$KEYS" --network "$NET" -v "$STATE/www:/srv:ro,z" -v "$STATE/pki:/pki:ro,z" \
	-v "$STATE/nginx.conf:/etc/nginx/conf.d/default.conf:ro,z" "$NGINX_IMAGE" >/dev/null

podman run -d --name "$FJ" --network "$NET" -v "$STATE/data:/data:Z" -v "$STATE/pki:/pki:ro,z" \
	-e FORGEJO__database__DB_TYPE=sqlite3 -e FORGEJO__security__INSTALL_LOCK=true \
	-e FORGEJO__server__ROOT_URL="http://$FJ:3000/" -e FORGEJO__server__DISABLE_SSH=true \
	-e FORGEJO__repository__DEFAULT_BRANCH=main -e SSL_CERT_FILE=/pki/bundle.pem \
	-e FORGEJO__authorized_integration__ALLOWED_DOMAINS="$KEYS" \
	-e FORGEJO__authorized_integration__ALLOW_LOCALNETWORKS=false \
	"$FORGEJO_IMAGE" >/dev/null
fj() { podman exec -u git "$FJ" forgejo "$@"; }
for _ in $(seq 60); do
	podman exec "$FJ" curl -sf "http://localhost:3000/api/healthz" >/dev/null 2>&1 && break
	sleep 1
done
podman exec "$FJ" curl -sf "http://localhost:3000/api/healthz" >/dev/null
echo "forgejo: $(fj --version)"

fj admin user create --admin --username fjadmin --password 'Adm1n-pass-xyz' \
	--email fjadmin@example.invalid --must-change-password=false >/dev/null
fj admin user create --username bot --password 'B0t-pass-xyz' \
	--email bot@example.invalid --must-change-password=false >/dev/null
ADMIN=$(fj admin user generate-access-token --username fjadmin --token-name seed --scopes all --raw)
podman exec "$FJ" curl -sf -o /dev/null -H "Authorization: token $ADMIN" -H 'Content-Type: application/json' \
	-X POST "http://localhost:3000/api/v1/admin/users/bot/repos" -d '{"name":"probe","private":true,"auto_init":true}'

SUBJECT=hive:advisor
set -x
fj admin user create-authorized-integration --username bot --name hive-advisor --issuer "$ISSUER" \
	--scope read:repository --repo bot/probe --claim-eq "sub=$SUBJECT" >"$STATE/advisor.json"
fj admin user create-authorized-integration --username bot --name hive-bare --issuer "$ISSUER" \
	--scope read:repository --repo bot/probe >"$STATE/bare.json"
set +x
cat "$STATE/advisor.json"
audience() { sed -n 's/^ *"audience": *"\([^"]*\)".*/\1/p' "$1"; }
AUDIENCE=$(audience "$STATE/advisor.json") BARE=$(audience "$STATE/bare.json")
[ -n "$AUDIENCE" ] && [ -n "$BARE" ] || { echo "no audience in the integration output" >&2; exit 1; }

gorun -e FORGEJO_URL="http://$FJ:3000" -e FORGEJO_REPO=bot/probe -e FORGEJO_JWT_ISSUER="$ISSUER" \
	-e FORGEJO_JWT_KEY=/state/jwt-signing.pem -e FORGEJO_JWT_AUDIENCE="$AUDIENCE" -e FORGEJO_JWT_SUBJECT="$SUBJECT" \
	-e FORGEJO_JWT_BARE_AUDIENCE="$BARE" \
	"$GO_IMAGE" go test -tags forgejo_jwt_live -run '^TestForgejoAcceptsMintedTokens$' -count=1 -v ./crux/forgejwt
