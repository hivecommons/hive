# acme-dns: delegated RFC 2136 zone for wildcard DNS-01

hivecommons.dev DNS lives at DNSimple (CNCF/LFX), which cannot issue a
single-domain token. cert-manager instead writes challenges, with TSIG-signed
RFC 2136 updates, into the subzone `acme.hive.hivecommons.dev` served here.
LFX delegates once with fixed records:

| Type  | Name                                   | Value                              |
|-------|----------------------------------------|------------------------------------|
| A     | `ns-acme.hive.hivecommons.dev`         | `129.80.107.104`                   |
| NS    | `acme.hive.hivecommons.dev`            | `ns-acme.hive.hivecommons.dev`     |
| CNAME | `_acme-challenge.hive.hivecommons.dev` | `hive.acme.hive.hivecommons.dev`   |
| CNAME | `_acme-challenge.lke.hive.hivecommons.dev` | `lke-hive.acme.hive.hivecommons.dev` |
| CNAME | `_acme-challenge.dibs.hivecommons.dev` | `dibs.acme.hive.hivecommons.dev`   |

Knot only accepts TXT updates for `hive`, `lke-hive` and `dibs` under the zone,
signed with the `acme-dns-tsig` key, and does not recurse.

## TSIG key: two copies that must match

The TSIG secret exists in two places: the `acme-dns` namespace Secret that Knot
reads (`tsig-key.conf`), and the `cert-manager` namespace Secret that the
ClusterIssuer references (`tsig-secret`). Both must hold the SAME key name
(`acme-dns-tsig`), algorithm (`hmac-sha256`, `HMACSHA256` in the issuer) and
secret. Rotating means updating both, then `kubectl -n acme-dns rollout restart
deploy/acme-dns`. One command sequence creates both from a single generated key;
the key lives only in a shell variable (never echoed) and is unset afterwards:

```sh
KEY=$(openssl rand -base64 32)
kubectl -n acme-dns create secret generic acme-dns-tsig \
  --from-literal=tsig-key.conf="key:
  - id: acme-dns-tsig
    algorithm: hmac-sha256
    secret: $KEY"
kubectl -n cert-manager create secret generic acme-dns-tsig \
  --from-literal=tsig-secret="$KEY"
unset KEY
```

Run it in a shell with history disabled (`unset HISTFILE`, or a leading space
with `HISTCONTROL=ignorespace`). To rotate, use `kubectl ... create secret ...
--dry-run=client -o yaml | kubectl apply -f -` for both.

## Rollout

0. Create the namespace first so the Secrets have somewhere to go:
   `kubectl apply -f namespace.yaml`.
1. Create both Secrets with the sequence in the previous section, then
   `kubectl apply -k .` (re-applying the namespace is harmless). Confirm the
   Service holds `129.80.107.104`. Verify the pod is Ready and the log shows
   no module or bind errors:
   `kubectl -n acme-dns logs deploy/acme-dns` (expect no "module" error for
   `mod-rrl`, and the server listening on `0.0.0.0@53`). Also check the
   non-root bind: `kubectl -n acme-dns exec deploy/acme-dns -- id`. The pod sets
   the `net.ipv4.ip_unprivileged_port_start=0` sysctl so a non-root knotd can
   bind port 53; if the pod is rejected over the sysctl, the cluster forbids it.
2. Ask LFX to add the records above. Check:
   `dig @129.80.107.104 SOA acme.hive.hivecommons.dev` and
   `dig NS acme.hive.hivecommons.dev`.
3. `kubectl apply -f clusterissuer-rfc2136.yaml` (the Cloudflare issuer stays).
4. Staging test: a throwaway Certificate for `acme-test.hive.hivecommons.dev`
   against a copy of the issuer using the Let's Encrypt staging URL. It must
   reach `Ready`.
5. Switch `hive-hub/hive-wildcard-tls` to `letsencrypt-dns01-rfc2136` and
   force-renew well before 2027-01-08 (`cmctl renew -n hive-hub hive-wildcard-tls`).
   Confirm the mail receiver's synced copy picked up the new certificate.
6. Remove the Cloudflare solver and its token Secret.

Response rate limiting (`mod-rrl`, 20 UDP responses/s per source, slip 2) is on
globally so the open authoritative server cannot be used for reflection or
amplification.

The zonefile is seeded into an `emptyDir`: challenge records are short-lived,
so a pod restart loses nothing that cert-manager does not re-create. With
`journal-content: none` and `zonefile-sync: -1`, dynamic updates are held in
memory only and are lost when Knot restarts; in-flight challenges at that moment
fail and cert-manager retries them.
